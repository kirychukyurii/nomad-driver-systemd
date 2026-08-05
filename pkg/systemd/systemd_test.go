package systemd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/hashicorp/go-hclog"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
)

// newTestManager builds a Manager wired to conn instead of a real DBus
// connection, so Manager's own logic (job-result waiting, property parsing,
// caching) can be tested without a systemd host.
func newTestManager(t *testing.T, conn dbusConn) *Manager {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return &Manager{
		conn:         conn,
		logger:       logx.New(hclog.NewNullLogger()),
		units:        make(map[string]*unitState),
		propUpdateCh: make(chan *dbus.PropertiesUpdate, propUpdateBufferSize),
		propErrCh:    make(chan error, propErrBufferSize),
		cgroupRoot:   cgroupV2Root,
		ctx:          ctx,
		cancel:       cancel,
	}
}

// register brings unit under management without a DBus round-trip, which is what
// RegisterUnit would otherwise do to read its cgroup path.
func register(sm *Manager, unit string) {
	sm.unitsLock.Lock()
	defer sm.unitsLock.Unlock()

	sm.units[unit] = &unitState{wake: make(chan struct{}, 1)}
}

var errDbus = errors.New("dbus failure")

func TestHealthy(t *testing.T) {
	cases := []struct {
		name      string
		connected bool
		want      bool
	}{
		{name: "connected", connected: true, want: true},
		{name: "disconnected", connected: false, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := newTestManager(t, &fakeDbusConn{connected: tc.connected})
			if got := sm.Healthy(); got != tc.want {
				t.Fatalf("Healthy() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSubscribeToPropertyChanges(t *testing.T) {
	cases := []struct {
		name           string
		subscribeErr   error
		wantSubscriber bool
	}{
		{name: "success registers subscriber", wantSubscriber: true},
		{name: "subscribe failure is non-fatal", subscribeErr: errDbus, wantSubscriber: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeDbusConn{
				connected:     true,
				subscribeFunc: func() error { return tc.subscribeErr },
			}
			sm := newTestManager(t, conn)

			sm.subscribeToPropertyChanges(conn) // must never panic

			if !conn.subscribed {
				t.Fatalf("expected Subscribe() to be attempted")
			}

			if got := conn.propUpdateCh != nil; got != tc.wantSubscriber {
				t.Fatalf("subscriber registered = %v, want %v", got, tc.wantSubscriber)
			}
		})
	}
}

// TestOpContext_CancelledByManagerShutdown pins the core of the fix that
// removed the command channel: an operation context must die when the manager
// shuts down, so a hung DBus call can no longer outlive its caller.
func TestOpContext_CancelledByManagerShutdown(t *testing.T) {
	sm := newTestManager(t, &fakeDbusConn{connected: true})

	opCtx, cancel := sm.opContext(context.Background(), time.Hour)
	defer cancel()

	select {
	case <-opCtx.Done():
		t.Fatalf("operation context must start out live")
	default:
	}

	sm.cancel() // simulate manager shutdown

	select {
	case <-opCtx.Done():
	case <-time.After(time.Second):
		t.Fatalf("expected manager shutdown to cancel the operation context")
	}
}

func TestOpContext_CancelledByCallerDeadline(t *testing.T) {
	sm := newTestManager(t, &fakeDbusConn{connected: true})

	callerCtx, cancelCaller := context.WithCancel(context.Background())

	opCtx, cancel := sm.opContext(callerCtx, time.Hour)
	defer cancel()

	cancelCaller()

	select {
	case <-opCtx.Done():
	case <-time.After(time.Second):
		t.Fatalf("expected caller cancellation to cancel the operation context")
	}
}

// TestOpContext_BoundedByLimit covers the third bound: the budget this package
// assigns to the operation. Without it, a caller passing a plain context (which
// every caller now does, since the timeout moved in here) would get an unbounded
// DBus call.
func TestOpContext_BoundedByLimit(t *testing.T) {
	sm := newTestManager(t, &fakeDbusConn{connected: true})

	opCtx, cancel := sm.opContext(context.Background(), time.Millisecond)
	defer cancel()

	select {
	case <-opCtx.Done():
		if !errors.Is(opCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("err = %v, want DeadlineExceeded", opCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatalf("expected the manager's own limit to bound the operation")
	}
}

// TestOpContext_LimitIsACeilingNotAnOverride is the property that makes moving
// the timeout into this package safe: a caller that wants to be stricter than
// the Manager still wins. Only lengthening is impossible.
func TestOpContext_LimitIsACeilingNotAnOverride(t *testing.T) {
	sm := newTestManager(t, &fakeDbusConn{connected: true})

	callerCtx, cancelCaller := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelCaller()

	// A generous library budget must not extend the caller's own deadline.
	opCtx, cancel := sm.opContext(callerCtx, time.Hour)
	defer cancel()

	deadline, ok := opCtx.Deadline()
	if !ok {
		t.Fatalf("expected the operation context to carry a deadline")
	}

	if callerDeadline, _ := callerCtx.Deadline(); deadline.After(callerDeadline) {
		t.Errorf("operation deadline %v is later than the caller's %v", deadline, callerDeadline)
	}

	select {
	case <-opCtx.Done():
	case <-time.After(time.Second):
		t.Fatalf("expected the caller's shorter deadline to bound the operation")
	}
}

// TestUnregisteredUnitErrors pins the error contract of the operations that need
// a registered unit: each names the operation and the unit, and each wraps the
// same cause so the three sites cannot drift apart.
func TestUnregisteredUnitErrors(t *testing.T) {
	cases := []struct {
		name string
		run  func(sm *Manager) error
	}{
		{
			name: "ResourceStats",
			run: func(sm *Manager) error {
				_, err := sm.ResourceStats(context.Background(), "app.service")

				return err
			},
		},
		{
			name: "StreamLogs",
			run: func(sm *Manager) error {
				return sm.StreamLogs("app.service", make(chan *LogEntry))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := newTestManager(t, &fakeDbusConn{connected: true})

			err := tc.run(sm)
			if err == nil {
				t.Fatalf("expected an error for an unregistered unit")
			}

			if !errors.Is(err, ErrUnitNotRegistered) {
				t.Errorf("error should wrap ErrUnitNotRegistered, got: %v", err)
			}

			if !strings.Contains(err.Error(), "app.service") {
				t.Errorf("error should name the unit, got: %v", err)
			}

			if strings.Contains(err.Error(), "failed to") {
				t.Errorf("error should not pile up 'failed to', got: %v", err)
			}
		})
	}
}
