package systemd

import (
	"context"
	"fmt"
	"time"

	"github.com/coreos/go-systemd/v22/sdjournal"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx/semconv"
)

const (
	// journalWaitTimeout is how long a journal reader blocks waiting for new
	// entries before re-checking for cancellation. New entries wake it
	// immediately; this is not a polling interval.
	journalWaitTimeout = 1 * time.Second

	// journalErrorBackoffMin/Max bound the retry delay after a journal read
	// error, doubling on each consecutive failure and resetting on success.
	journalErrorBackoffMin = 500 * time.Millisecond
	journalErrorBackoffMax = 5 * time.Second
)

// LogEntry is a single journald record emitted by a unit.
type LogEntry struct {
	// Message is the record's MESSAGE field, with no trailing newline.
	Message string

	// Priority is the record's syslog priority as an unparsed decimal string,
	// "0" through "7", or empty if the record carried none.
	Priority string

	// SyslogIdentifier names the program that emitted the record, or is empty
	// if the record carried no identifier.
	SyslogIdentifier string

	// Timestamp is when journald received the record, not when the program
	// emitted it.
	Timestamp time.Time
}

// StreamLogs begins delivering the unit's journal records to logCh and returns
// as soon as the reader is running.
//
// Delivery continues until the unit is unregistered or the Manager stops.
// StreamLogs returns an error wrapping [ErrUnitNotRegistered] if the unit is not
// registered, and a plain error if the Manager has stopped, if the journal cannot
// be opened, or if a match for the unit cannot be installed. Calling it again for the same unit replaces the existing reader.
//
// Only records written from this point on are delivered: anything the unit logged
// earlier, including while this process was down, is not replayed. Delivery
// blocks when logCh is full rather than dropping records, so a caller that stops
// reading logCh stalls journal consumption for that unit until it is
// unregistered. logCh is never closed by the Manager.
func (sm *Manager) StreamLogs(unit string, logCh chan<- *LogEntry) error {
	logger := sm.logger.With(semconv.Unit(unit))
	logger.Debug("starting log streamer")

	// Cheap pre-check, re-done by startJournalReader below: avoid opening a
	// journal that would immediately be thrown away.
	if !sm.registered(unit) {
		return fmt.Errorf("stream logs for %s: %w", unit, ErrUnitNotRegistered)
	}

	journal, err := sdjournal.NewJournal()
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}

	if err := journal.AddMatch("_SYSTEMD_UNIT=" + unit); err != nil {
		journal.Close()

		return fmt.Errorf("add journal match: %w", err)
	}

	// Start from now; earlier records are not replayed.
	if err := journal.SeekRealtimeUsec(uint64(time.Now().UnixMicro())); err != nil {
		logger.Warn("can't seek journal; starting from the tail", logx.Err(err))

		if err := journal.SeekTail(); err != nil {
			logger.Warn("can't seek journal tail; starting wherever the cursor is", logx.Err(err))
		}
	}

	unitCtx, unitCancel := context.WithCancel(sm.ctx)

	if !sm.startJournalReader(unit, unitCancel) {
		unitCancel()
		journal.Close()

		return fmt.Errorf("stream logs for %s: unit was unregistered or the manager stopped", unit)
	}

	go func() {
		defer sm.journalWg.Done()
		defer journal.Close()

		errBackoff := journalErrorBackoffMin

		for {
			select {
			case <-unitCtx.Done():
				logger.Debug("journal streaming stopped")

				return
			default:
				n, err := journal.Next()
				if err != nil {
					logger.Warn("advance journal", logx.Err(err), semconv.RetryDelay(errBackoff))

					select {
					case <-time.After(errBackoff):
					case <-unitCtx.Done():
						logger.Debug("journal streaming stopped")

						return
					}

					errBackoff = min(errBackoff*2, journalErrorBackoffMax)

					continue
				}

				errBackoff = journalErrorBackoffMin

				if n == 0 {
					journal.Wait(journalWaitTimeout)

					continue
				}

				entry, err := journal.GetEntry()
				if err != nil {
					logger.Error("get journal entry", logx.Err(err))

					continue
				}

				message, ok := entry.Fields["MESSAGE"]
				if !ok {
					continue
				}

				logEntry := &LogEntry{
					Message:          message,
					Priority:         entry.Fields["PRIORITY"],
					SyslogIdentifier: entry.Fields["SYSLOG_IDENTIFIER"],
					Timestamp:        time.Unix(0, int64(entry.RealtimeTimestamp)*1000),
				}

				select {
				case logCh <- logEntry:
				case <-unitCtx.Done():
					logger.Debug("journal streaming stopped")

					return
				}
			}
		}
	}()

	return nil
}

// startJournalReader registers cancel as unit's journal reader and counts the
// reader on journalWg.
//
// It reports whether the reader was registered, returning false if the Manager
// is stopping or the unit is not registered; the caller must then abandon the
// reader. On true the caller must call journalWg.Done when the reader exits.
//
// Registration, the stopping check and the journalWg.Add all happen in one
// critical section, which is what keeps a reader from being registered after
// Stop has begun waiting for readers to finish.
func (sm *Manager) startJournalReader(unit string, cancel context.CancelFunc) bool {
	sm.unitsLock.Lock()

	st := sm.unitLocked(unit)
	if sm.stopping || st == nil {
		sm.unitsLock.Unlock()

		return false
	}

	previous := st.cancelJournal
	st.cancelJournal = cancel

	sm.journalWg.Add(1)
	sm.unitsLock.Unlock()

	if previous != nil {
		previous()
	}

	return true
}
