package systemd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/coreos/go-systemd/v22/sdjournal"
	"github.com/hashicorp/nomad/client/lib/cpustats"
	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx/semconv"
)

// Manager controls systemd units over a single DBus connection.
//
// All methods are safe for concurrent use by multiple goroutines. The zero value
// is not usable: create a Manager with [NewManager], call [Manager.Start] to run
// its background loops, and [Manager.Stop] to release its connection and
// goroutines. A Manager whose connection drops re-establishes it on its own, so
// callers should retry a failed operation rather than discard the Manager;
// [Manager.Healthy] reports whether a connection is currently available.
type Manager struct {
	logger logx.Logger

	// conn is replaced by reconnectLoop whenever the connection drops, so it
	// must only be reached through getConn/setConn.
	conn     dbusConn
	connLock sync.RWMutex

	// units holds everything tracked per managed unit. One map under one lock:
	// every piece of per-unit state is keyed by the same unit name and mutated
	// on the same paths, so splitting it up bought nothing but lock-ordering
	// questions. No lock is ever held across a DBus call, a file read or a
	// channel send.
	units     map[string]*unitState
	unitsLock sync.RWMutex

	// stopping is set by Stop, under unitsLock, to refuse new journal readers.
	stopping bool

	// journalWg counts running journal readers. Readers are registered by
	// startJournalReader, which is what keeps registration and shutdown
	// ordered.
	journalWg sync.WaitGroup

	// propUpdateCh/propErrCh back conn.SetPropertiesSubscriber. They belong to
	// the Manager, not to a connection, so they survive a reconnect.
	propUpdateCh chan *dbus.PropertiesUpdate
	propErrCh    chan error

	compute cpustats.Compute

	// cgroupRoot is where ResourceStats reads accounting files from, normally
	// cgroupV2Root.
	cgroupRoot string

	//nolint:containedctx // @kirychukyurii: bounds the Manager's own lifetime, not a per-call deadline; Stop cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

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

const (
	// jobTimeout is the ceiling on a start or stop job. It exceeds systemd's
	// own default TimeoutStopSec of 90s, so reaching it means the job is stuck.
	jobTimeout = 2 * time.Minute

	// propertyTimeout is the ceiling on a single DBus round-trip that does not
	// wait on a systemd job.
	propertyTimeout = 10 * time.Second

	// cgroupRetryInterval is how long a cached empty ControlGroup is kept
	// before being re-read.
	cgroupRetryInterval = 5 * time.Second

	// reconnectInterval is the connection health-check period, and the first
	// reconnect delay.
	reconnectInterval = 10 * time.Second

	// reconnectBackoffMax caps the reconnect delay, which doubles on each
	// consecutive failure.
	reconnectBackoffMax = 5 * time.Minute

	// journalWaitTimeout is how long a journal reader blocks waiting for new
	// entries before re-checking for cancellation. New entries wake it
	// immediately; this is not a polling interval.
	journalWaitTimeout = 1 * time.Second

	// journalErrorBackoffMin/Max bound the retry delay after a journal read
	// error, doubling on each consecutive failure and resetting on success.
	journalErrorBackoffMin = 500 * time.Millisecond
	journalErrorBackoffMax = 5 * time.Second

	// cgroupV2Root is the unified cgroup hierarchy, the default for
	// Manager.cgroupRoot.
	cgroupV2Root = "/sys/fs/cgroup"

	// propUpdateBufferSize/propErrBufferSize size the channels backing
	// conn.SetPropertiesSubscriber, whose writes are non-blocking: a full
	// buffer drops updates rather than blocking.
	propUpdateBufferSize = 64
	propErrBufferSize    = 8
)

// ErrUnitNotRegistered is returned, wrapped, by operations that need a unit to be
// under management: see [Manager.ResourceStats] and [Manager.StreamLogs].
//
// It is part of this package's contract because it is the one error whose right
// handling differs from the rest. It means the unit is gone for good - normally
// because it was unregistered while the caller was still working with it - so a
// caller should give up on the unit rather than retry, where any other error may
// well be transient.
var ErrUnitNotRegistered = errors.New("unit is not registered")

// CgroupV2Available reports whether the host uses the unified (v2) cgroup
// hierarchy that [Manager.ResourceStats] requires.
//
// When it reports false, units still start and stop normally but resource
// statistics read as zeros. Callers should surface that up front rather than
// leave it to be discovered as unexplained empty statistics.
func (sm *Manager) CgroupV2Available() bool {
	_, err := os.Stat(filepath.Join(sm.cgroupRoot, "cgroup.controllers"))

	return err == nil
}

// NewManager connects to the systemd system bus and returns a Manager ready for
// [Manager.Start].
//
// It returns an error if the system bus is unreachable, which on a typical host
// means systemd is not running or the caller lacks permission to talk to it. The
// returned Manager holds the connection until [Manager.Stop] is called; ctx
// bounds the Manager's whole lifetime, so canceling it stops all background
// work and fails all in-flight operations.
//
// compute describes the host's CPU capacity and is used to turn cgroup CPU time
// into the percentages and ticks Nomad reports.
func NewManager(ctx context.Context, compute cpustats.Compute, logger logx.Logger) (*Manager, error) {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to systemd: %w", err)
	}

	managerCtx, cancel := context.WithCancel(ctx)
	sm := &Manager{
		conn:         conn,
		logger:       logger.Named("systemd_manager"),
		units:        make(map[string]*unitState),
		propUpdateCh: make(chan *dbus.PropertiesUpdate, propUpdateBufferSize),
		propErrCh:    make(chan error, propErrBufferSize),
		compute:      compute,
		cgroupRoot:   cgroupV2Root,
		ctx:          managerCtx,
		cancel:       cancel,
	}

	sm.subscribeToPropertyChanges(conn)

	return sm, nil
}

// subscribeToPropertyChanges registers the Manager's channels for this
// connection's PropertiesChanged signals. Failure is non-fatal: wake channels
// then never fire, and callers fall back to their own re-checks.
func (sm *Manager) subscribeToPropertyChanges(conn dbusConn) {
	if err := conn.Subscribe(); err != nil {
		sm.logger.Warn("can't subscribe to systemd unit signals; unit changes will only be noticed by periodic re-checks", logx.Err(err))

		return
	}

	conn.SetPropertiesSubscriber(sm.propUpdateCh, sm.propErrCh)
}

// Start launches the Manager's background work: watching the DBus connection's
// health and re-establishing it when it drops, and dispatching unit change
// notifications to the channels handed out by [Manager.WakeChannel].
//
// Start must be called at most once, and unit operations work without it - they
// are ordinary method calls, not queued work - but without it a dropped
// connection is never repaired and wake channels never fire. Every goroutine it
// starts is joined by [Manager.Stop].
func (sm *Manager) Start() {
	sm.wg.Add(2)

	go sm.reconnectLoop()
	go sm.propertiesDispatchLoop()
}

// opContext returns the context for a single DBus operation. It ends at the
// earliest of the caller's context, the Manager's shutdown, and limit.
func (sm *Manager) opContext(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	opCtx, cancel := context.WithTimeout(ctx, limit)
	stop := context.AfterFunc(sm.ctx, cancel)

	return opCtx, func() {
		stop()
		cancel()
	}
}

// Stop shuts the Manager down and closes its DBus connection.
//
// It blocks until every goroutine the Manager owns has finished, including
// journal readers started by [Manager.StreamLogs], so callers may afterwards
// tear down resources those goroutines used, such as the channels passed to
// StreamLogs. In-flight operations fail rather than complete. Once Stop returns,
// the Manager is unusable: StreamLogs refuses new work and every other operation
// fails. Stop must be called at most once.
func (sm *Manager) Stop() {
	sm.logger.Info("stopping systemd manager")
	sm.cancel()

	sm.wg.Wait()

	// Refuse further journal readers, then stop and wait out the running ones.
	// startJournalReader is the only place that registers one, and it refuses
	// once stopping is set, so no reader can appear after this point.
	sm.unitsLock.Lock()
	sm.stopping = true

	cancels := make([]context.CancelFunc, 0, len(sm.units))
	for _, st := range sm.units {
		if st.cancelJournal != nil {
			cancels = append(cancels, st.cancelJournal)
		}
	}

	sm.units = make(map[string]*unitState)
	sm.unitsLock.Unlock()

	for _, cancel := range cancels {
		cancel()
	}

	sm.journalWg.Wait()

	sm.connLock.Lock()
	if sm.conn != nil {
		sm.conn.Close()
	}
	sm.connLock.Unlock()

	sm.logger.Info("systemd manager stopped")
}

// Healthy reports whether a DBus connection is currently available.
//
// It is a point-in-time observation, not a reservation: a connection may drop
// immediately after it reports true. Its intended use is health reporting rather
// than guarding an operation, since every operation checks the connection itself.
func (sm *Manager) Healthy() bool {
	sm.connLock.RLock()
	defer sm.connLock.RUnlock()

	return sm.conn != nil && sm.conn.Connected()
}

// getConn returns the current DBus connection, or an error if none is
// available. Every DBus caller goes through it so a concurrent reconnect cannot
// leave them on a stale connection.
func (sm *Manager) getConn() (dbusConn, error) {
	sm.connLock.RLock()
	defer sm.connLock.RUnlock()

	if sm.conn == nil || !sm.conn.Connected() {
		return nil, errors.New("no systemd dbus connection available")
	}

	return sm.conn, nil
}

// setConn replaces the DBus connection and closes the one it replaced.
func (sm *Manager) setConn(conn dbusConn) {
	sm.connLock.Lock()
	old := sm.conn
	sm.conn = conn
	sm.connLock.Unlock()

	if old != nil {
		old.Close()
	}
}

// reconnectLoop re-establishes the DBus connection whenever it drops, retrying
// with exponential backoff up to reconnectBackoffMax.
func (sm *Manager) reconnectLoop() {
	defer sm.wg.Done()

	backoff := reconnectInterval

	timer := time.NewTimer(backoff)
	defer timer.Stop()

	for {
		select {
		case <-sm.ctx.Done():
			return
		case <-timer.C:
			switch {
			case sm.Healthy(), sm.tryReconnect():
				backoff = reconnectInterval
			default:
				backoff = min(backoff*2, reconnectBackoffMax)
			}

			timer.Reset(backoff)
		}
	}
}

// tryReconnect establishes a fresh DBus connection and refreshes the cached
// properties of every registered unit. It reports whether it succeeded.
func (sm *Manager) tryReconnect() bool {
	sm.logger.Debug("systemd dbus connection unhealthy, attempting reconnect")

	conn, err := dbus.NewSystemConnectionContext(sm.ctx)
	if err != nil {
		sm.logger.Warn("reconnect to systemd dbus", logx.Err(err))

		return false
	}

	// Each connection carries its own signal dispatch, so the subscription does
	// not carry over and must be re-established before the swap.
	sm.subscribeToPropertyChanges(conn)
	sm.setConn(conn)
	sm.logger.Info("reconnected to systemd dbus")

	sm.unitsLock.RLock()

	units := make([]string, 0, len(sm.units))
	for unit := range sm.units {
		units = append(units, unit)
	}

	sm.unitsLock.RUnlock()

	for _, unit := range units {
		sm.cacheUnitProperties(sm.ctx, unit)
	}

	return true
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

// usecInfinity is systemd's USEC_INFINITY, the "never" sentinel for timestamp
// properties. It is distinct from 0, which carries the same meaning elsewhere.
const usecInfinity = ^uint64(0)

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

// ResourceStats samples the unit's current CPU and memory usage.
//
// It returns an error wrapping [ErrUnitNotRegistered], and only that error, if
// the unit is not registered. A sample that could not
// be read - the unit has no cgroup yet, or the host is not on cgroup v2 - comes
// back zeroed with empty Measured lists rather than as an error, and both the
// result and its metrics are non-nil in that case. Consult those lists to tell
// "zero" from "not measured".
//
// CPU percentage is derived from the change since the previous call for the same
// unit, so the first sample after registration reports no percentage.
func (sm *Manager) ResourceStats(ctx context.Context, unit string) (*ResourceStats, error) {
	controlGroup, cachedAt, registered := sm.cachedControlGroup(unit)
	if !registered {
		return nil, fmt.Errorf("resource stats for %s: %w", unit, ErrUnitNotRegistered)
	}

	logger := sm.logger.With(semconv.Unit(unit))

	// Re-read if never read, or if an empty reading has gone stale.
	if cachedAt.IsZero() || (controlGroup == "" && time.Since(cachedAt) > cgroupRetryInterval) {
		logger.Debug("refreshing cached unit properties")
		sm.cacheUnitProperties(ctx, unit)

		controlGroup, _, registered = sm.cachedControlGroup(unit)
		if !registered {
			return nil, fmt.Errorf("resource stats for %s: %w", unit, ErrUnitNotRegistered)
		}
	}

	if controlGroup == "" {
		logger.Warn("cgroup path is empty for unit")

		return EmptyResourceStats(), nil
	}

	stats := sm.getCgroupV2Stats(unit, controlGroup)
	if stats == nil {
		logger.Warn("read cgroup v2 stats", semconv.UnitCgroup(controlGroup))

		return EmptyResourceStats(), nil
	}

	return stats, nil
}

// cachedControlGroup returns unit's cached cgroup path and when it was read. The
// final result reports whether the unit is registered at all; a zero cachedAt
// means it is registered but nothing has been read yet.
func (sm *Manager) cachedControlGroup(unit string) (controlGroup string, cachedAt time.Time, registered bool) {
	sm.unitsLock.RLock()
	defer sm.unitsLock.RUnlock()

	st := sm.unitLocked(unit)
	if st == nil {
		return "", time.Time{}, false
	}

	return st.controlGroup, st.cachedAt, true
}

// getCgroupV2Stats reads a unit's cgroup accounting files. It returns nil if
// nothing at all could be measured; a partial read is still reported, with
// Measured naming what was obtained.
func (sm *Manager) getCgroupV2Stats(unit, cgroupPath string) *ResourceStats {
	logger := sm.logger.With(semconv.Unit(unit))
	fullPath := filepath.Join(sm.cgroupRoot, cgroupPath)

	var (
		cpuStats    drivers.CpuStats
		memoryStats drivers.MemoryStats
	)

	memoryStats.Measured = make([]string, 0, 3)

	// memory.current is total charged memory, including page cache and kernel
	// memory, so it is Usage. RSS and Cache come from memory.stat below.
	memCurrentPath := filepath.Join(fullPath, "memory.current")
	if memCurrent, err := readCgroupV2File(memCurrentPath); err == nil {
		memoryStats.Usage = memCurrent
		memoryStats.Measured = append(memoryStats.Measured, "Usage")
	} else {
		logger.Warn("read memory.current", semconv.FilePath(memCurrentPath), logx.Err(err))
	}

	memStatPath := filepath.Join(fullPath, "memory.stat")
	if data, err := os.ReadFile(memStatPath); err == nil {
		var haveAnon, haveFile bool

		scanCgroupKeyValue(data, func(key string, value uint64) bool {
			switch key {
			case "anon":
				memoryStats.RSS = value
				memoryStats.Measured = append(memoryStats.Measured, "RSS")
				haveAnon = true
			case "file":
				memoryStats.Cache = value
				memoryStats.Measured = append(memoryStats.Measured, "Cache")
				haveFile = true
			}

			// No further key is consumed, so stop once both are in hand.
			// Tracked with flags, not values, since 0 is a real reading.
			return !haveAnon || !haveFile
		})
	} else {
		logger.Warn("read memory.stat", semconv.FilePath(memStatPath), logx.Err(err))
	}

	cpuStatPath := filepath.Join(fullPath, "cpu.stat")
	if data, err := os.ReadFile(cpuStatPath); err == nil {
		scanCgroupKeyValue(data, func(key string, value uint64) bool {
			if key != "usage_usec" {
				return true
			}

			sm.calculateCPUPercent(unit, value*1000, &cpuStats)

			return false
		})
	} else {
		logger.Warn("read cpu.stat", semconv.FilePath(cpuStatPath), logx.Err(err))
	}

	if len(memoryStats.Measured) > 0 || len(cpuStats.Measured) > 0 {
		return &ResourceStats{
			CPUStats:    &cpuStats,
			MemoryStats: &memoryStats,
		}
	}

	logger.Warn("no stats measured from cgroup v2")

	return nil
}

// scanCgroupKeyValue walks the "key value" lines of a cgroup stat file, calling
// fn for each line that parses and stopping early when fn returns false.
//
// Malformed lines are skipped rather than fatal: these files gain keys across
// kernel versions, and one odd line must not cost the remaining metrics.
func scanCgroupKeyValue(data []byte, fn func(key string, value uint64) bool) {
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil
		}

		key, rest, ok := bytes.Cut(line, []byte{' '})
		if !ok {
			continue
		}

		value, err := strconv.ParseUint(string(bytes.TrimSpace(rest)), 10, 64)
		if err != nil {
			continue
		}

		if !fn(string(key), value) {
			return
		}
	}
}

// calculateCPUPercent turns cumulative CPU nanoseconds into the percentage and
// tick count Nomad reports, using the unit's previous sample.
func (sm *Manager) calculateCPUPercent(unit string, cpuUsageNsec uint64, cpuStats *drivers.CpuStats) {
	// The write lock, not the read lock: the tracker itself is mutated here.
	sm.unitsLock.Lock()
	defer sm.unitsLock.Unlock()

	st := sm.unitLocked(unit)
	if st == nil {
		return
	}

	if st.cpu == nil {
		st.cpu = cpustats.New(sm.compute)
	}

	percent := st.cpu.Percent(float64(cpuUsageNsec))
	ticks := st.cpu.TicksConsumed(percent)

	cpuStats.Percent = percent
	cpuStats.TotalTicks = ticks
	cpuStats.Measured = []string{"Percent", "Total Ticks"}
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

// readCgroupV2File reads a cgroup file holding one unsigned decimal value. It
// returns an error for anything else, including the "max" sentinel.
func readCgroupV2File(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}

	return value, nil
}
