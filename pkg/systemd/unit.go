package systemd

import (
	"context"
	"fmt"
	"math"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/hashicorp/nomad/client/lib/cpustats"
	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx/semconv"
)

const (
	// jobTimeout is the ceiling on a start or stop job. It exceeds systemd's
	// own default TimeoutStopSec of 90s, so reaching it means the job is stuck.
	jobTimeout = 2 * time.Minute

	// propertyTimeout is the ceiling on a single DBus round-trip that does not
	// wait on a systemd job.
	propertyTimeout = 10 * time.Second

	// usecInfinity is systemd's USEC_INFINITY, the "never" sentinel for
	// timestamp properties. It is distinct from 0, which carries the same
	// meaning elsewhere.
	usecInfinity = ^uint64(0)
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

// unitState is everything a Manager tracks for one managed unit. Every field is
// guarded by Manager.unitsLock.
type unitState struct {
	// wake is the channel handed out by WakeChannel. Buffered by one, so wakes
	// coalesce while one is still pending.
	wake chan struct{}

	// cancelJournal stops this unit's journal reader, or is nil if it has none.
	cancelJournal context.CancelFunc

	// controlGroup is the unit's cgroup path, empty if it could not be read.
	// cachedAt dates the reading, so an empty value can be retried after
	// cgroupRetryInterval.
	controlGroup string
	cachedAt     time.Time

	// cpu accumulates CPU samples, needed to turn cumulative cgroup CPU time
	// into a percentage. Nil until the first sample.
	cpu *cpustats.Tracker
}

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

// RegisterUnit brings a unit under management.
//
// It must be called before [Manager.WakeChannel], [Manager.ResourceStats] or
// [Manager.StreamLogs] for that unit, and matched by [Manager.UnregisterUnit]
// when the unit is no longer managed. Registering an already-registered unit
// re-reads its cached properties and replaces its wake channel, abandoning any
// channel a previous caller still holds.
//
// Registration performs a DBus round-trip and reports failures to the logger
// rather than to the caller: a unit whose properties cannot be read yet is still
// registered, and the read is retried on demand.
func (sm *Manager) RegisterUnit(ctx context.Context, unit string) {
	sm.unitsLock.Lock()
	previous := sm.units[unit]
	sm.units[unit] = &unitState{wake: make(chan struct{}, 1)}
	sm.unitsLock.Unlock()

	// Outside the lock: canceling runs arbitrary code, and the DBus round-trip
	// below must not be made under a lock either.
	if previous != nil && previous.cancelJournal != nil {
		previous.cancelJournal()
	}

	sm.cacheUnitProperties(ctx, unit)

	sm.logger.Debug("registered unit", semconv.Unit(unit))
}

// UnregisterUnit releases everything the Manager holds for a unit: its cached
// properties, its CPU sampling history, its wake channel, and its journal
// reader, which is stopped.
//
// It does not stop the unit itself. Unregistering an unknown unit does nothing.
// The wake channel returned earlier by [Manager.WakeChannel] stops firing, and
// callers holding it should discard it.
func (sm *Manager) UnregisterUnit(unit string) {
	sm.unitsLock.Lock()
	st := sm.unitLocked(unit)
	delete(sm.units, unit)
	sm.unitsLock.Unlock()

	// Outside the lock: canceling runs arbitrary code.
	if st != nil && st.cancelJournal != nil {
		st.cancelJournal()
	}

	sm.logger.Debug("unregistered unit", semconv.Unit(unit))
}

// WakeChannel returns a channel that receives a value whenever systemd reports a
// property change for unit, or nil if the unit is not registered.
//
// It is a hint to re-read the unit's state, never a source of truth: it carries
// no payload, coalesces bursts into a single pending value, and may miss changes
// entirely, because DBus delivers these notifications on a best-effort basis and
// the subscription may have been refused outright. Callers must therefore keep an
// upper bound of their own, such as a periodic re-check. A nil channel is safe in
// a select - it simply never fires - so an unregistered unit degrades to that
// fallback rather than needing a special case.
func (sm *Manager) WakeChannel(unit string) <-chan struct{} {
	sm.unitsLock.RLock()
	defer sm.unitsLock.RUnlock()

	if st := sm.unitLocked(unit); st != nil {
		return st.wake
	}

	return nil
}

// StartUnit starts a unit and waits for systemd to finish the job.
//
// A nil error means the unit actually came up. An error means the job was
// rejected, failed, or did not finish in time - and in the latter two cases the
// job may still be queued with systemd, so the unit can come up after StartUnit
// has already returned. Callers that treat the error as "the unit is not running"
// should compensate, for example by stopping the unit.
func (sm *Manager) StartUnit(ctx context.Context, unit string) error {
	sm.logger.Info("starting unit", semconv.Unit(unit))

	// StartUnitContext only enqueues the job; the result channel is what tells
	// us whether ExecStart succeeded.
	conn, err := sm.getConn()
	if err != nil {
		return err
	}

	opCtx, cancel := sm.opContext(ctx, jobTimeout)
	defer cancel()

	resultCh := make(chan string, 1)
	if _, err := conn.StartUnitContext(opCtx, unit, "replace", resultCh); err != nil {
		return fmt.Errorf("enqueue start job: %w", err)
	}

	return sm.waitJobResult(opCtx, unit, "start", resultCh)
}

// StopUnit stops a unit and waits for systemd to finish the job.
//
// A nil error means the unit is down. An error means the job was rejected,
// failed, or did not finish in time; the unit may or may not have stopped, so
// callers needing certainty should check its state or escalate to
// [Manager.KillUnit].
//
// A stop legitimately takes as long as the unit's own TimeoutStopSec, 90 seconds
// by default, after which systemd escalates to SIGKILL by itself. Either way the
// unit's wake channel fires before StopUnit returns, so a watcher re-reads the
// unit's state promptly.
func (sm *Manager) StopUnit(ctx context.Context, unit string) error {
	sm.logger.Info("stopping unit", semconv.Unit(unit))

	// The unit's state is about to change; wake its watcher directly rather
	// than depend on the DBus signal arriving.
	defer sm.wakeUnit(unit)

	conn, err := sm.getConn()
	if err != nil {
		return err
	}

	opCtx, cancel := sm.opContext(ctx, jobTimeout)
	defer cancel()

	resultCh := make(chan string, 1)
	if _, err := conn.StopUnitContext(opCtx, unit, "replace", resultCh); err != nil {
		return fmt.Errorf("enqueue stop job: %w", err)
	}

	return sm.waitJobResult(opCtx, unit, "stop", resultCh)
}

// KillUnit sends SIGKILL to every process of a unit.
//
// Unlike [Manager.StopUnit] it does not wait for the unit to settle: a nil error
// means the signal was delivered, not that the unit has stopped. It also fails
// when there is nothing left to kill, so callers escalating from a timed-out
// stop should treat an error as inconclusive and confirm against the unit's
// state. The unit's wake channel fires before KillUnit returns.
func (sm *Manager) KillUnit(ctx context.Context, unit string) error {
	sm.logger.Info("killing unit", semconv.Unit(unit))

	defer sm.wakeUnit(unit)

	conn, err := sm.getConn()
	if err != nil {
		return err
	}

	opCtx, cancel := sm.opContext(ctx, propertyTimeout)
	defer cancel()

	return conn.KillUnitWithTarget(opCtx, unit, dbus.All, int32(syscall.SIGKILL))
}

// UnitState returns the unit's current ActiveState.
//
// It returns an error if no DBus connection is available, if the unit does not
// exist, or if the lookup does not complete in time. States this package does not
// know about are returned unchanged rather than rejected, so callers should use
// the [UnitState] predicates instead of comparing against constants.
func (sm *Manager) UnitState(ctx context.Context, unit string) (UnitState, error) {
	conn, err := sm.getConn()
	if err != nil {
		return "", err
	}

	opCtx, cancel := sm.opContext(ctx, propertyTimeout)
	defer cancel()

	activeStateProp, err := conn.GetUnitPropertyContext(opCtx, unit, "ActiveState")
	if err != nil {
		return "", fmt.Errorf("get ActiveState property: %w", err)
	}

	activeStateStr, ok := activeStateProp.Value.Value().(string)
	if !ok {
		return "", fmt.Errorf("ActiveState property has unexpected type: %T", activeStateProp.Value.Value())
	}

	return ParseUnitState(activeStateStr), nil
}

// UnitStartTime returns when the unit last became active.
//
// It returns an error if the unit has never been active, as well as on the usual
// lookup failures. The value survives restarts of this process, so it is the way
// to learn a recovered unit's true start time instead of assuming it started
// just now.
func (sm *Manager) UnitStartTime(ctx context.Context, unit string) (time.Time, error) {
	conn, err := sm.getConn()
	if err != nil {
		return time.Time{}, err
	}

	opCtx, cancel := sm.opContext(ctx, propertyTimeout)
	defer cancel()

	prop, err := conn.GetUnitPropertyContext(opCtx, unit, "ActiveEnterTimestamp")
	if err != nil {
		return time.Time{}, fmt.Errorf("get ActiveEnterTimestamp property: %w", err)
	}

	usec, ok := parseTimestampUsec(prop.Value.Value())
	if !ok {
		return time.Time{}, fmt.Errorf("ActiveEnterTimestamp property has unexpected type: %T", prop.Value.Value())
	}

	if usec == 0 {
		return time.Time{}, fmt.Errorf("start time of %s: unit has never been active", unit)
	}

	return time.UnixMicro(int64(usec)), nil
}

// UnitExitStatus returns how the unit's main process last exited.
//
// It applies to service units; for any other unit type, and on the usual lookup
// failures, it returns an error. A successful call may still describe a process
// that never exited - see [ExitStatus] and [ExitResultFromStatus] - so callers
// need a fallback for that case rather than assuming the status is meaningful.
func (sm *Manager) UnitExitStatus(ctx context.Context, unit string) (ExitStatus, error) {
	conn, err := sm.getConn()
	if err != nil {
		return ExitStatus{}, err
	}

	opCtx, cancel := sm.opContext(ctx, propertyTimeout)
	defer cancel()

	props, err := conn.GetUnitTypePropertiesContext(opCtx, unit, "Service")
	if err != nil {
		return ExitStatus{}, fmt.Errorf("get service properties: %w", err)
	}

	code, ok := props["ExecMainCode"].(int32)
	if !ok {
		return ExitStatus{}, fmt.Errorf("ExecMainCode property has unexpected type: %T", props["ExecMainCode"])
	}

	status, ok := props["ExecMainStatus"].(int32)
	if !ok {
		return ExitStatus{}, fmt.Errorf("ExecMainStatus property has unexpected type: %T", props["ExecMainStatus"])
	}

	return ExitStatus{Code: code, Status: status}, nil
}

// unitLocked returns unit's state, or nil if it is not registered. The caller
// must hold unitsLock.
func (sm *Manager) unitLocked(unit string) *unitState {
	return sm.units[unit]
}

// registered reports whether unit is currently under management.
func (sm *Manager) registered(unit string) bool {
	sm.unitsLock.RLock()
	defer sm.unitsLock.RUnlock()

	return sm.unitLocked(unit) != nil
}

// propertiesDispatchLoop wakes a unit's channel for each PropertiesChanged
// notification DBus delivers. It interprets no payload: a woken caller re-reads
// the unit's state itself.
func (sm *Manager) propertiesDispatchLoop() {
	defer sm.wg.Done()

	for {
		select {
		case <-sm.ctx.Done():
			return

		case upd := <-sm.propUpdateCh:
			sm.wakeUnit(upd.UnitName)

		case err := <-sm.propErrCh:
			sm.logger.Debug("systemd properties subscription", logx.Err(err))
		}
	}
}

// wakeUnit signals unit's wake channel if it has one. The send is
// non-blocking, so wakes coalesce while one is still pending.
func (sm *Manager) wakeUnit(unit string) {
	sm.unitsLock.RLock()
	st := sm.unitLocked(unit)
	sm.unitsLock.RUnlock()

	if st == nil {
		return
	}

	// Outside the lock: never send on a channel while holding one.
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// waitJobResult blocks until the systemd job posts a terminal result or ctx
// ends. ctx already carries jobTimeout, so there is no timer here.
func (sm *Manager) waitJobResult(ctx context.Context, unit, action string, resultCh chan string) error {
	select {
	case result := <-resultCh:
		if result != "done" {
			return fmt.Errorf("%s unit %s: job result %q", action, unit, result)
		}

		return nil

	case <-ctx.Done():
		return fmt.Errorf("%s unit %s: waiting for job result: %w", action, unit, ctx.Err())
	}
}

// cacheUnitProperties reads the unit's properties and caches its cgroup path.
// Failures are logged, not returned: the entry is cached either way and re-read
// on demand.
func (sm *Manager) cacheUnitProperties(ctx context.Context, unit string) {
	logger := sm.logger.With(semconv.Unit(unit))
	logger.Debug("caching unit properties")

	conn, err := sm.getConn()
	if err != nil {
		logger.Warn("cache unit properties", logx.Err(err))

		return
	}

	opCtx, cancel := sm.opContext(ctx, propertyTimeout)
	defer cancel()

	properties, err := conn.GetUnitPropertiesContext(opCtx, unit)
	if err != nil {
		logger.Warn("cache unit properties", logx.Err(err))

		return
	}

	controlGroup, ok := properties["ControlGroup"].(string)
	if !ok || controlGroup == "" {
		logger.Warn("unit property missing", semconv.UnitProperty("ControlGroup"))

		controlGroup = "" // cached anyway; ResourceStats retries after cgroupRetryInterval
	}

	sm.unitsLock.Lock()
	if st := sm.unitLocked(unit); st != nil {
		st.controlGroup = controlGroup
		st.cachedAt = time.Now()
	}
	sm.unitsLock.Unlock()

	logger.Info("cached unit properties", semconv.UnitCgroup(controlGroup))
}

// parseTimestampUsec decodes a DBus property holding a systemd usec timestamp.
// It reports false for anything that cannot be a real point in time, so callers
// need not guard the int64 conversion time.UnixMicro requires.
func parseTimestampUsec(v any) (uint64, bool) {
	switch t := v.(type) {
	case uint64:
		if t == usecInfinity || t > uint64(math.MaxInt64) {
			return 0, false
		}

		return t, true
	case int64:
		if t < 0 {
			return 0, false
		}

		return uint64(t), true
	default:
		return 0, false
	}
}
