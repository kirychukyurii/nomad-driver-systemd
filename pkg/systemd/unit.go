package systemd

import (
	"fmt"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
)

// UnitState is a systemd unit's ActiveState, as reported over DBus.
//
// The zero value is the empty string, which matches no known state: every
// predicate below reports false for it, and [ToTaskState] maps it to
// [drivers.TaskStateUnknown]. Values outside the constants below are preserved
// as-is rather than rejected, since systemd may report states this package does
// not know about.
type UnitState string

// The unit states systemd defines for ActiveState.
//
// See https://www.freedesktop.org/software/systemd/man/systemd.html
const (
	// UnitStateActivating means the unit is starting up.
	UnitStateActivating UnitState = "activating"

	// UnitStateDeactivating means the unit is shutting down.
	UnitStateDeactivating UnitState = "deactivating"

	// UnitStateActive means the unit is running.
	UnitStateActive UnitState = "active"

	// UnitStateInactive means the unit is stopped, having either exited cleanly
	// or never been started.
	UnitStateInactive UnitState = "inactive"

	// UnitStateFailed means the unit stopped abnormally: a non-zero exit, a
	// fatal signal, or a timeout.
	UnitStateFailed UnitState = "failed"

	// UnitStateReloading means the unit is reloading its configuration and is
	// expected to return to active.
	UnitStateReloading UnitState = "reloading"

	// UnitStateMaintenance means the unit is undergoing maintenance and its
	// eventual state is not yet determined.
	UnitStateMaintenance UnitState = "maintenance"
)

// IsTransitioning reports whether the unit is moving between states and so has
// no settled state yet.
func (s UnitState) IsTransitioning() bool {
	return s == UnitStateActivating || s == UnitStateDeactivating || s == UnitStateReloading
}

// IsActive reports whether the unit is running.
func (s UnitState) IsActive() bool {
	return s == UnitStateActive
}

// IsInactive reports whether the unit is stopped.
func (s UnitState) IsInactive() bool {
	return s == UnitStateInactive
}

// IsFailed reports whether the unit stopped abnormally.
func (s UnitState) IsFailed() bool {
	return s == UnitStateFailed
}

// IsTerminal reports whether the unit has stopped and will not resume on its
// own, whether it stopped cleanly or not.
func (s UnitState) IsTerminal() bool {
	return s == UnitStateInactive || s == UnitStateFailed
}

// String returns the state as systemd spells it.
func (s UnitState) String() string {
	return string(s)
}

// ParseUnitState converts systemd's ActiveState string into a [UnitState].
//
// Unrecognized values are preserved rather than rejected, so callers must not
// assume the result equals one of the constants above.
func ParseUnitState(state string) UnitState {
	return UnitState(state)
}

// ToTaskState maps a systemd unit state onto the Nomad task state it
// corresponds to.
//
// Transitional and unrecognized states both map to [drivers.TaskStateUnknown]:
// such a task has neither started nor finished, so callers deciding whether a
// task may be torn down must test for [drivers.TaskStateExited] rather than for
// "not running".
func ToTaskState(state UnitState) drivers.TaskState {
	switch state {
	case UnitStateActivating, UnitStateDeactivating, UnitStateReloading:
		return drivers.TaskStateUnknown
	case UnitStateActive:
		return drivers.TaskStateRunning
	case UnitStateFailed, UnitStateInactive:
		return drivers.TaskStateExited
	case UnitStateMaintenance:
		return drivers.TaskStateUnknown
	default:
		return drivers.TaskStateUnknown
	}
}

// ExitStatus reports how a service unit's main process last exited, as systemd
// records it in the ExecMainCode and ExecMainStatus properties. It is the same
// information `systemctl status` renders as "code=exited, status=1/FAILURE" or
// "code=killed, status=9/KILL".
//
// The zero value means the main process has not exited, and possibly never ran,
// because no waitid si_code is zero.
type ExitStatus struct {
	// Code is the waitid si_code. [CLDExited], [CLDKilled] and [CLDDumped]
	// describe a process that terminated; any other value means it did not.
	Code int32

	// Status is the process's exit code when Code is [CLDExited], or the
	// terminating signal number when Code is [CLDKilled] or [CLDDumped]. Its
	// meaning is undefined for any other Code.
	Status int32
}

// The waitid si_code values that describe a terminated process. See wait(2).
const (
	// CLDExited means the process exited of its own accord, so Status holds its
	// exit code.
	CLDExited = 1

	// CLDKilled means a signal killed the process, so Status holds the signal
	// number.
	CLDKilled = 2

	// CLDDumped means a signal killed the process and it dumped core, so Status
	// holds the signal number.
	CLDDumped = 3
)

// ExitResultFromStatus translates a systemd [ExitStatus] into the exit result
// Nomad expects.
//
// It returns nil when status.Code does not describe a terminated process, for
// example because the unit has no main process or never started one. A nil
// result does not mean success: callers must fall back to another signal of
// success or failure, usually the unit's [UnitState].
func ExitResultFromStatus(status ExitStatus) *drivers.ExitResult {
	switch status.Code {
	case CLDExited:
		result := &drivers.ExitResult{ExitCode: int(status.Status)}
		if status.Status != 0 {
			result.Err = fmt.Errorf("systemd unit exited with code %d", status.Status)
		}

		return result

	case CLDKilled, CLDDumped:
		return &drivers.ExitResult{
			ExitCode: -1,
			Signal:   int(status.Status),
			Err:      fmt.Errorf("systemd unit terminated by signal %d", status.Status),
		}

	default:
		return nil
	}
}

// ResourceStats holds one sample of a unit's CPU and memory usage.
//
// Both fields are non-nil in every value this package produces, so callers need
// no nil checks. Each carries a Measured list naming the metrics actually
// obtained; a metric absent from that list is zero because it could not be read,
// not because its value is zero.
type ResourceStats struct {
	// CPUStats holds CPU usage, including a percentage derived from the change
	// since the previous sample taken for the same unit.
	CPUStats *drivers.CpuStats

	// MemoryStats holds memory usage: Usage is total charged memory, RSS the
	// anonymous portion, and Cache the page-cache portion.
	MemoryStats *drivers.MemoryStats
}

// EmptyResourceStats returns a sample with non-nil, zeroed CPU and memory stats
// and an empty Measured list on each.
//
// It lets callers that cannot obtain real numbers - no cgroup path yet, DBus
// unavailable, a cgroup v1 host - return a usable value instead of nil.
func EmptyResourceStats() *ResourceStats {
	return &ResourceStats{
		CPUStats:    &drivers.CpuStats{},
		MemoryStats: &drivers.MemoryStats{},
	}
}

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
