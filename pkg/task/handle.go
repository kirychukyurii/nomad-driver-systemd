package task

import (
	"context"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/systemd"
)

// Handler manages the lifecycle of a single task
// It communicates with systemd.Manager via channels
type Handler struct {
	// Task identification
	taskID string
	Unit   string

	// Channels for communication
	cmdCh   chan<- systemd.Command // Send commands to SystemdManager
	eventCh <-chan systemd.Message // Receive events from SystemdManager
	LogCh   chan *systemd.LogEntry // Receive log entries

	// Task state
	handle      *drivers.TaskHandle
	state       drivers.TaskState
	startedAt   time.Time
	completedAt time.Time
	exitResult  *drivers.ExitResult

	// Resource stats (cached from SystemdManager push events)
	latestStats *systemd.ResourceStats
	statsLock   sync.RWMutex

	// Synchronization
	stateLock sync.RWMutex
	waitCh    chan struct{}

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	logger hclog.Logger
}

// NewHandler creates a new task handler
func NewHandler(taskID string, unit string, handle *drivers.TaskHandle, cmdCh chan<- systemd.Command, eventCh <-chan systemd.Message, initialState drivers.TaskState, logger hclog.Logger) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	th := &Handler{
		taskID:    taskID,
		Unit:      unit,
		handle:    handle,
		cmdCh:     cmdCh,
		eventCh:   eventCh,
		LogCh:     make(chan *systemd.LogEntry, 100),
		state:     initialState,
		startedAt: time.Now(),
		waitCh:    make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
		logger:    logger.With("task", taskID, "unit", unit),
	}

	// If task is already exited, close waitCh immediately
	if initialState == drivers.TaskStateExited {
		close(th.waitCh)
	}

	return th
}

// Start begins monitoring the task
func (th *Handler) Start() {
	th.logger.Debug("starting task handler")

	go th.eventLoop()
	go th.streamLogs()
	go th.pollTaskState()
}

// Stop stops the task handler
func (th *Handler) Stop() {
	th.logger.Debug("stopping task handler")
	th.cancel()
}

// SetExitResult sets the exit result for an already-exited task (used during recovery)
func (th *Handler) SetExitResult(exitResult *drivers.ExitResult, completedAt time.Time) {
	th.stateLock.Lock()
	defer th.stateLock.Unlock()

	th.exitResult = exitResult
	th.completedAt = completedAt
}

// pollTaskState periodically checks unit state to detect when task exits
func (th *Handler) pollTaskState() {
	th.logger.Debug("starting state polling")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-th.ctx.Done():
			th.logger.Debug("state polling stopped")
			return

		case <-ticker.C:
			// Check if task already exited
			th.stateLock.RLock()
			if th.state == drivers.TaskStateExited {
				th.stateLock.RUnlock()
				return
			}
			th.stateLock.RUnlock()

			// Poll current state
			respCh := make(chan systemd.Message, 1)
			cmd := systemd.Command{
				Type:   systemd.CmdGetStatus,
				Unit:   th.Unit,
				RespCh: respCh,
			}

			select {
			case th.cmdCh <- cmd:
			case <-th.ctx.Done():
				return
			case <-time.After(2 * time.Second):
				th.logger.Warn("timeout sending status command")
				continue
			}

			select {
			case resp := <-respCh:
				if resp.Error != nil {
					th.logger.Warn("failed to get unit state", "error", resp.Error)
					continue
				}

				state, ok := resp.Data.(systemd.UnitState)
				if !ok {
					th.logger.Error("invalid status response", "expected", "SystemdUnitState", "got", fmt.Sprintf("%T", resp.Data))
					continue
				}

				// Simulate state change event
				th.handleStateChange(state)

			case <-th.ctx.Done():
				return
			case <-time.After(2 * time.Second):
				th.logger.Warn("timeout waiting for status response")
			}
		}
	}
}

// eventLoop processes events from SystemdManager
func (th *Handler) eventLoop() {
	th.logger.Debug("starting event loop")

	for {
		select {
		case <-th.ctx.Done():
			th.logger.Debug("event loop context cancelled")
			return

		case event, ok := <-th.eventCh:
			if !ok {
				th.logger.Debug("event channel closed")
				return
			}

			th.handleEvent(event)
		}
	}
}

// handleEvent processes a single event
func (th *Handler) handleEvent(event systemd.Message) {
	th.logger.Debug("received event", "type", event.Type)

	switch event.Type {
	case systemd.EventStateChanged:
		state, ok := event.Data.(systemd.UnitState)
		if !ok {
			th.logger.Error("invalid state event data", "expected", "SystemdUnitState", "got", fmt.Sprintf("%T", event.Data))
			return
		}

		th.handleStateChange(state)

	case systemd.EventError:
		th.logger.Error("systemd error", "error", event.Error)
		th.handleError(event.Error)

	case systemd.EventStatsUpdate:
		stats, ok := event.Data.(*systemd.ResourceStats)
		if !ok {
			th.logger.Error("invalid stats event data", "expected", "*ResourceStats", "got", fmt.Sprintf("%T", event.Data))
			return
		}
		th.handleStatsUpdate(stats, event.Error)

	case systemd.EventLogEntry:
		logEntry, ok := event.Data.(*systemd.LogEntry)
		if !ok {
			th.logger.Error("invalid log event data", "expected", "*LogEntry", "got", fmt.Sprintf("%T", event.Data))
			return
		}

		select {
		case th.LogCh <- logEntry:
		default:
			th.logger.Warn("log channel full, dropping log entry")
		}
	}
}

// handleStateChange updates task state based on systemd Unit state
func (th *Handler) handleStateChange(activeState systemd.UnitState) {
	th.stateLock.Lock()
	defer th.stateLock.Unlock()

	var (
		ost = th.state
		cst = systemd.ToTaskState(activeState)
	)

	if ost == cst {
		th.state = cst
		th.logger.Info("task state changed", "old_state", ost, "new_state", th.state, "systemd_state", activeState.String())
	}

	// If task exited, close wait channel and record completion
	if th.state == drivers.TaskStateExited && ost != drivers.TaskStateExited {
		th.completedAt = time.Now()
		th.exitResult = &drivers.ExitResult{
			ExitCode: 0,
			Signal:   0,
		}

		if activeState.IsFailed() {
			th.exitResult = &drivers.ExitResult{
				ExitCode: 1,
				Signal:   0,
				Err:      fmt.Errorf("systemd unit failed"),
			}
		}

		// Close wait channel to signal task completion
		close(th.waitCh)
	}
}

// handleError handles errors from SystemdManager
func (th *Handler) handleError(err error) {
	th.stateLock.Lock()
	defer th.stateLock.Unlock()

	if th.state != drivers.TaskStateExited {
		th.state = drivers.TaskStateExited
		th.completedAt = time.Now()
		th.exitResult = &drivers.ExitResult{
			ExitCode: 1,
			Signal:   0,
			Err:      err,
		}

		close(th.waitCh)
	}
}

// handleStatsUpdate caches the latest stats from SystemdManager
func (th *Handler) handleStatsUpdate(stats *systemd.ResourceStats, err error) {
	th.statsLock.Lock()
	defer th.statsLock.Unlock()

	if err != nil {
		th.logger.Debug("received stats update with error", "error", err)
		// Keep previous stats on error
		return
	}

	th.latestStats = stats
	th.logger.Debug("cached stats update", "cpu_percent", stats.CPUStats.Percent, "memory_rss", stats.MemoryStats.RSS)
}

// streamLogs streams logs to stdout/stderr FIFOs
func (th *Handler) streamLogs() {
	th.logger.Debug("starting log streamer")

	// During recovery, FIFOs might not exist yet. Retry with backoff.
	var stdout, stderr *os.File
	var err error

	maxRetries := 5
	for i := 0; i < maxRetries; i++ {
		stdout, err = os.OpenFile(th.handle.Config.StdoutPath, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
		if err == nil {
			break
		}

		if i < maxRetries-1 {
			th.logger.Debug("failed to open stdout, retrying", "error", err, "attempt", i+1)
			time.Sleep(time.Duration(i+1) * 100 * time.Millisecond)
		}
	}

	if err != nil {
		th.logger.Warn("failed to open stdout after retries, log streaming disabled", "error", err)
		return
	}

	defer stdout.Close()

	stderr, err = os.OpenFile(th.handle.Config.StderrPath, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
	if err != nil {
		th.logger.Warn("failed to open stderr, logs will only go to stdout", "error", err)
		stderr = stdout // Fallback to stdout
	} else {
		defer stderr.Close()
	}

	for {
		select {
		case <-th.ctx.Done():
			return

		case logEntry, ok := <-th.LogCh:
			if !ok {
				return
			}

			th.writeLogEntry(logEntry, stdout, stderr)
		}
	}
}

// writeLogEntry writes a log entry to the appropriate output stream
func (th *Handler) writeLogEntry(entry *systemd.LogEntry, stdout, stderr *os.File) {
	writer := stdout
	if entry.Priority == "0" || entry.Priority == "1" || entry.Priority == "2" || entry.Priority == "3" {
		writer = stderr
	}

	priorityLevel := mapPriorityToLevel(entry.Priority)
	var logLine string
	if entry.SyslogIdentifier != "" {
		logLine = fmt.Sprintf("[%s] [%s] %s\n", priorityLevel, entry.SyslogIdentifier, entry.Message)
	} else {
		logLine = fmt.Sprintf("[%s] %s\n", priorityLevel, entry.Message)
	}

	if _, err := fmt.Fprint(writer, logLine); err != nil {
		th.logger.Warn("failed to write log", "error", err)
	}
}

// TaskStatus returns the current task status
func (th *Handler) TaskStatus() *drivers.TaskStatus {
	th.stateLock.RLock()
	defer th.stateLock.RUnlock()

	return &drivers.TaskStatus{
		ID:          th.taskID,
		Name:        th.Unit,
		State:       th.state,
		StartedAt:   th.startedAt,
		CompletedAt: th.completedAt,
		ExitResult:  th.exitResult,
		DriverAttributes: map[string]string{
			"unit": th.Unit,
		},
	}
}

// IsRunning returns true if the task is running
func (th *Handler) IsRunning() bool {
	th.stateLock.RLock()
	defer th.stateLock.RUnlock()

	return th.state == drivers.TaskStateRunning
}

// WaitCh returns a channel that is closed when the task exits
func (th *Handler) WaitCh() <-chan struct{} {
	return th.waitCh
}

// GetResourceStats returns the cached resource statistics
// Stats are pushed by SystemdManager immediately on registration and then periodically
func (th *Handler) GetResourceStats() (*systemd.ResourceStats, error) {
	th.statsLock.RLock()
	defer th.statsLock.RUnlock()

	if th.latestStats == nil {
		th.logger.Debug("GetResourceStats called but no stats cached yet")
		return &systemd.ResourceStats{
			CPUStats:    &drivers.CpuStats{},
			MemoryStats: &drivers.MemoryStats{},
		}, nil
	}

	return th.latestStats, nil
}

// StopUnit sends a stop command for the unit
func (th *Handler) StopUnit() error {
	th.logger.Info("stopping unit")
	respCh := make(chan systemd.Message, 1)
	cmd := systemd.Command{
		Type:   systemd.CmdStop,
		Unit:   th.Unit,
		RespCh: respCh,
	}

	select {
	case th.cmdCh <- cmd:
	case <-th.ctx.Done():
		return fmt.Errorf("task handler stopped")
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timeout sending stop command")
	}

	select {
	case resp := <-respCh:
		return resp.Error
	case <-th.ctx.Done():
		return fmt.Errorf("task handler stopped")
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timeout waiting for stop response")
	}
}

// mapPriorityToLevel maps syslog priority numbers to human-readable levels
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
