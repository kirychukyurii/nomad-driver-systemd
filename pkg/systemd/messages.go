package systemd

import (
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
)

// UnitState represents the state of a systemd unit
type UnitState string

// Systemd unit states as defined by systemd
// See: https://www.freedesktop.org/software/systemd/man/systemd.html
const (
	// UnitStateActivating Unit is currently starting
	UnitStateActivating UnitState = "activating"

	// UnitStateDeactivating Unit is currently stopping
	UnitStateDeactivating UnitState = "deactivating"

	// UnitStateActive Unit is active and running
	UnitStateActive UnitState = "active"

	// UnitStateInactive Unit is inactive (stopped)
	UnitStateInactive UnitState = "inactive"

	// UnitStateFailed Unit failed to start or crashed
	UnitStateFailed UnitState = "failed"

	// UnitStateReloading Unit is being reloaded
	UnitStateReloading UnitState = "reloading"

	// UnitStateMaintenance Unit maintenance state
	UnitStateMaintenance UnitState = "maintenance"
)

// IsTransitioning returns true if the unit is in a transitional state
func (s UnitState) IsTransitioning() bool {
	return s == UnitStateActivating || s == UnitStateDeactivating || s == UnitStateReloading
}

// IsActive returns true if the unit is active
func (s UnitState) IsActive() bool {
	return s == UnitStateActive
}

// IsInactive returns true if the unit is inactive (stopped)
func (s UnitState) IsInactive() bool {
	return s == UnitStateInactive
}

// IsFailed returns true if the unit is in a failed state
func (s UnitState) IsFailed() bool {
	return s == UnitStateFailed
}

// IsTerminal returns true if the unit is in a terminal state (not running)
func (s UnitState) IsTerminal() bool {
	return s == UnitStateInactive || s == UnitStateFailed
}

// String returns the string representation of the state
func (s UnitState) String() string {
	return string(s)
}

// ParseSystemdUnitState parses a string into a SystemdUnitState
func ParseSystemdUnitState(state string) UnitState {
	return UnitState(state)
}

// ToTaskState converts a systemd unit state to Nomad task state
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

// CommandType represents the type of command to send to SystemdManager
type CommandType int

const (
	CmdStart CommandType = iota
	CmdStop
	CmdGetStatus
	CmdGetProperties
)

// EventType represents the type of event from SystemdManager
type EventType int

const (
	EventStateChanged EventType = iota
	EventStatsUpdate
	EventLogEntry
	EventError
)

// Command is sent from Driver to SystemdManager to perform operations
type Command struct {
	Type   CommandType
	Unit   string
	RespCh chan Message
}

// Message is a unified message type for both commands/responses and events
// Used for command responses (synchronous) and event notifications (asynchronous)
type Message struct {
	Unit      string
	Type      EventType // Type of event (for async events) or zero for command responses
	Data      any       // Payload: SystemdUnitState, *ResourceStats, *LogEntry, map[string]interface{}, etc.
	Error     error
	Timestamp time.Time
}

// ResourceStats contains CPU and memory statistics
type ResourceStats struct {
	CPUStats    *drivers.CpuStats
	MemoryStats *drivers.MemoryStats
}

// LogEntry represents a single log line from journald
type LogEntry struct {
	Message          string
	Priority         string
	SyslogIdentifier string
	Timestamp        time.Time
}

// TaskMessage is sent from TaskHandler to Driver
type TaskMessage struct {
	TaskID     string
	State      drivers.TaskState
	ExitResult *drivers.ExitResult
	Error      error
	Timestamp  time.Time
}
