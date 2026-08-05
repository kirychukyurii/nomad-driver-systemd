package systemd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/hashicorp/nomad/client/lib/cpustats"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
)

const (
	// reconnectInterval is the connection health-check period, and the first
	// reconnect delay.
	reconnectInterval = 10 * time.Second

	// reconnectBackoffMax caps the reconnect delay, which doubles on each
	// consecutive failure.
	reconnectBackoffMax = 5 * time.Minute

	// propUpdateBufferSize/propErrBufferSize size the channels backing
	// conn.SetPropertiesSubscriber, whose writes are non-blocking: a full
	// buffer drops updates rather than blocking.
	propUpdateBufferSize = 64
	propErrBufferSize    = 8
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

// ErrUnitNotRegistered is returned, wrapped, by operations that need a unit to be
// under management: see [Manager.ResourceStats] and [Manager.StreamLogs].
//
// It is part of this package's contract because it is the one error whose right
// handling differs from the rest. It means the unit is gone for good - normally
// because it was unregistered while the caller was still working with it - so a
// caller should give up on the unit rather than retry, where any other error may
// well be transient.
var ErrUnitNotRegistered = errors.New("unit is not registered")

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
