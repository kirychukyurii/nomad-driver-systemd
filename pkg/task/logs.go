package task

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx/semconv"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/systemd"
)

// logChannelBufferSize is how many log entries may queue between the journal
// reader and the FIFO writer. A full buffer blocks the reader rather than
// dropping entries.
const logChannelBufferSize = 100

// maxStdoutOpenRetries and stdoutRetryBackoffUnit bound how long streamLogs
// waits for the task's stdout FIFO to appear, which Nomad may not have created
// yet during recovery. Backoff is (attempt+1) * stdoutRetryBackoffUnit.
const (
	maxStdoutOpenRetries   = 5
	stdoutRetryBackoffUnit = 100 * time.Millisecond
)

// streamLogs copies journal entries from logCh to the task's stdout and stderr
// FIFOs until the Handler stops.
//
// It gives up quietly if stdout cannot be opened, and falls back to stdout if
// only stderr cannot be: losing logs must not fail the task.
func (th *Handler) streamLogs() {
	th.logger.Debug("starting log streamer")

	// During recovery the FIFOs may not exist yet.
	var (
		stdout, stderr *os.File
		err            error
	)

	for i := range maxStdoutOpenRetries {
		stdout, err = os.OpenFile(th.handle.Config.StdoutPath, os.O_WRONLY|syscall.O_NONBLOCK, 0o600)
		if err == nil {
			break
		}

		if i < maxStdoutOpenRetries-1 {
			th.logger.Debug("can't open stdout, retrying", logx.Err(err), semconv.RetryAttempt(i+1))
			time.Sleep(time.Duration(i+1) * stdoutRetryBackoffUnit)
		}
	}

	if err != nil {
		th.logger.Warn("can't open stdout after retries; log streaming disabled", logx.Err(err))

		return
	}

	defer stdout.Close()

	stderr, err = os.OpenFile(th.handle.Config.StderrPath, os.O_WRONLY|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		th.logger.Warn("can't open stderr; sending all logs to stdout", logx.Err(err))

		stderr = stdout
	} else {
		defer stderr.Close()
	}

	for {
		select {
		case <-th.ctx.Done():
			return

		case logEntry, ok := <-th.logCh:
			if !ok {
				return
			}

			th.writeLogEntry(logEntry, stdout, stderr)
		}
	}
}

// writeLogEntry writes one entry to stderr if its priority is error or worse,
// and to stdout otherwise.
func (th *Handler) writeLogEntry(entry *systemd.LogEntry, stdout, stderr *os.File) {
	writer := stdout
	if entry.Priority == "0" || entry.Priority == "1" || entry.Priority == "2" || entry.Priority == "3" {
		writer = stderr
	}

	priorityLevel := mapPriorityToLevel(entry.Priority)

	var err error
	if entry.SyslogIdentifier != "" {
		_, err = fmt.Fprintf(writer, "[%s] [%s] %s\n", priorityLevel, entry.SyslogIdentifier, entry.Message)
	} else {
		_, err = fmt.Fprintf(writer, "[%s] %s\n", priorityLevel, entry.Message)
	}

	if err != nil {
		th.logger.Warn("write log entry", logx.Err(err))
	}
}

// mapPriorityToLevel renders a syslog priority string as its conventional level
// name, or "UNKNOWN" for anything that is not a priority digit.
func mapPriorityToLevel(priority string) string {
	switch priority {
	case "0":
		return "EMERG"
	case "1":
		return "ALERT"
	case "2":
		return "CRIT"
	case "3":
		return "ERR"
	case "4":
		return "WARN"
	case "5":
		return "NOTICE"
	case "6":
		return "INFO"
	case "7":
		return "DEBUG"
	default:
		return "UNKNOWN"
	}
}
