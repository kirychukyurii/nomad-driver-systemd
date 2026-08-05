package systemd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
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

// jobFunc builds a Start/StopUnitContext stub that reports jobResult (or
// fails to enqueue when enqueueErr is set), and records the args it saw.
func jobFunc(t *testing.T, jobResult string, enqueueErr error, gotName, gotMode *string) func(context.Context, string, string, chan<- string) (int, error) {
	t.Helper()

	return func(_ context.Context, name, mode string, ch chan<- string) (int, error) {
		*gotName, *gotMode = name, mode

		if enqueueErr != nil {
			return 0, enqueueErr
		}

		ch <- jobResult

		return 1, nil
	}
}

func TestManager_UnitJobs(t *testing.T) {
	cases := []struct {
		name       string
		connected  bool
		jobResult  string
		enqueueErr error
		wantErr    bool
	}{
		{name: "job done", connected: true, jobResult: "done"},
		{name: "job failed", connected: true, jobResult: "failed", wantErr: true},
		{name: "job canceled", connected: true, jobResult: "canceled", wantErr: true},
		{name: "job timeout", connected: true, jobResult: "timeout", wantErr: true},
		{name: "job dependency", connected: true, jobResult: "dependency", wantErr: true},
		{name: "enqueue error", connected: true, enqueueErr: errDbus, wantErr: true},
		{name: "no connection", connected: false, jobResult: "done", wantErr: true},
	}

	// Start and Stop share their entire result-handling path, so both are
	// driven through the same table.
	ops := []struct {
		name string
		run  func(sm *Manager, unit string) error
		wire func(t *testing.T, conn *fakeDbusConn, fn func(context.Context, string, string, chan<- string) (int, error))
	}{
		{
			name: "StartUnit",
			run:  func(sm *Manager, unit string) error { return sm.StartUnit(context.Background(), unit) },
			wire: func(_ *testing.T, conn *fakeDbusConn, fn func(context.Context, string, string, chan<- string) (int, error)) {
				conn.startFunc = fn
			},
		},
		{
			name: "StopUnit",
			run:  func(sm *Manager, unit string) error { return sm.StopUnit(context.Background(), unit) },
			wire: func(_ *testing.T, conn *fakeDbusConn, fn func(context.Context, string, string, chan<- string) (int, error)) {
				conn.stopFunc = fn
			},
		},
	}

	for _, op := range ops {
		for _, tc := range cases {
			t.Run(op.name+"/"+tc.name, func(t *testing.T) {
				var gotName, gotMode string

				conn := &fakeDbusConn{connected: tc.connected}
				op.wire(t, conn, jobFunc(t, tc.jobResult, tc.enqueueErr, &gotName, &gotMode))

				sm := newTestManager(t, conn)

				err := op.run(sm, "app.service")
				if (err != nil) != tc.wantErr {
					t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
				}

				if tc.connected {
					if gotName != "app.service" {
						t.Errorf("unit name = %q, want app.service", gotName)
					}

					if gotMode != "replace" {
						t.Errorf("mode = %q, want replace", gotMode)
					}
				}
			})
		}
	}
}

// TestManager_JobWaitIsBoundedByCallerContext covers the wait path for a
// systemd job that never posts a result.
//
// Callers now pass a plain context and rely on this package for the ceiling, so
// the bound has to come from opContext - waitJobResult no longer runs a timer of
// its own. Asserting against the real jobTimeout would mean a two-minute test,
// so this drives the same select through a caller deadline instead; that the
// library's own limit reaches the context at all is covered by
// TestOpContext_BoundedByLimit.
func TestManager_JobWaitIsBoundedByCallerContext(t *testing.T) {
	ops := []struct {
		name string
		wire func(conn *fakeDbusConn, f func(context.Context, string, string, chan<- string) (int, error))
		run  func(sm *Manager, ctx context.Context, unit string) error
	}{
		{
			name: "start",
			wire: func(conn *fakeDbusConn, f func(context.Context, string, string, chan<- string) (int, error)) {
				conn.startFunc = f
			},
			run: func(sm *Manager, ctx context.Context, unit string) error { return sm.StartUnit(ctx, unit) },
		},
		{
			name: "stop",
			wire: func(conn *fakeDbusConn, f func(context.Context, string, string, chan<- string) (int, error)) {
				conn.stopFunc = f
			},
			run: func(sm *Manager, ctx context.Context, unit string) error { return sm.StopUnit(ctx, unit) },
		},
	}

	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			conn := &fakeDbusConn{connected: true}

			// Enqueues the job and then never posts a result - a systemd job
			// stuck in the queue.
			op.wire(conn, func(context.Context, string, string, chan<- string) (int, error) {
				return 1, nil
			})

			sm := newTestManager(t, conn)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()

			done := make(chan error, 1)

			go func() { done <- op.run(sm, ctx, "app.service") }()

			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("error = %v, want it to wrap DeadlineExceeded", err)
				}

				if !strings.Contains(err.Error(), "app.service") {
					t.Errorf("error should name the unit, got: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s unit blocked forever on a job that never resolved", op.name)
			}
		})
	}
}

// TestManager_StopAndKillWakeUnit pins the guarantee that a state-changing
// operation nudges the unit's handler itself, rather than depending on the
// (documented-lossy) DBus push signal to eventually deliver.
func TestManager_StopAndKillWakeUnit(t *testing.T) {
	cases := []struct {
		name    string
		wire    func(conn *fakeDbusConn)
		run     func(sm *Manager, unit string) error
		wantErr bool
	}{
		{
			name: "successful stop wakes",
			wire: func(conn *fakeDbusConn) {
				conn.stopFunc = func(_ context.Context, _, _ string, ch chan<- string) (int, error) {
					ch <- "done"

					return 1, nil
				}
			},
			run: func(sm *Manager, unit string) error { return sm.StopUnit(context.Background(), unit) },
		},
		{
			name: "failed stop still wakes",
			wire: func(conn *fakeDbusConn) {
				conn.stopFunc = func(_ context.Context, _, _ string, ch chan<- string) (int, error) {
					ch <- "failed"

					return 1, nil
				}
			},
			run:     func(sm *Manager, unit string) error { return sm.StopUnit(context.Background(), unit) },
			wantErr: true,
		},
		{
			name: "kill wakes",
			wire: func(conn *fakeDbusConn) {
				conn.killFunc = func(context.Context, string, dbus.Who, int32) error { return nil }
			},
			run: func(sm *Manager, unit string) error { return sm.KillUnit(context.Background(), unit) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeDbusConn{connected: true}
			tc.wire(conn)

			sm := newTestManager(t, conn)
			register(sm, "app.service")

			if err := tc.run(sm, "app.service"); (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}

			select {
			case <-sm.WakeChannel("app.service"):
			default:
				t.Fatalf("expected the unit's handler to be woken")
			}
		})
	}
}

func TestKillUnit_PassesSigkillToAllProcesses(t *testing.T) {
	var (
		gotTarget dbus.Who
		gotSignal int32
	)

	conn := &fakeDbusConn{
		connected: true,
		killFunc: func(_ context.Context, name string, target dbus.Who, signal int32) error {
			if name != "app.service" {
				t.Errorf("unexpected unit: %q", name)
			}

			gotTarget, gotSignal = target, signal

			return nil
		},
	}
	sm := newTestManager(t, conn)

	if err := sm.KillUnit(context.Background(), "app.service"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotTarget != dbus.All {
		t.Errorf("target = %v, want dbus.All", gotTarget)
	}

	if gotSignal != 9 { // SIGKILL
		t.Errorf("signal = %d, want 9 (SIGKILL)", gotSignal)
	}
}

func TestManager_UnitState(t *testing.T) {
	cases := []struct {
		name      string
		connected bool
		value     any
		propErr   error
		want      UnitState
		wantErr   bool
	}{
		{name: "active", connected: true, value: "active", want: UnitStateActive},
		{name: "failed", connected: true, value: "failed", want: UnitStateFailed},
		{name: "unknown string passes through", connected: true, value: "weird", want: UnitState("weird")},
		{name: "non-string value", connected: true, value: int32(1), wantErr: true},
		{name: "property error", connected: true, value: "active", propErr: errDbus, wantErr: true},
		{name: "no connection", connected: false, value: "active", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeDbusConn{
				connected: tc.connected,
				getPropertyFunc: func(_ context.Context, unit, propertyName string) (*dbus.Property, error) {
					if propertyName != "ActiveState" {
						t.Errorf("property = %q, want ActiveState", propertyName)
					}

					if tc.propErr != nil {
						return nil, tc.propErr
					}

					return &dbus.Property{Name: propertyName, Value: godbus.MakeVariant(tc.value)}, nil
				},
			}
			sm := newTestManager(t, conn)

			got, err := sm.UnitState(context.Background(), "app.service")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}

			if err == nil && got != tc.want {
				t.Fatalf("state = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManager_UnitStartTime(t *testing.T) {
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	cases := []struct {
		name      string
		connected bool
		value     any
		propErr   error
		want      time.Time
		wantErr   bool
	}{
		{name: "uint64 timestamp", connected: true, value: uint64(want.UnixMicro()), want: want}, //nolint:gosec
		{name: "int64 timestamp", connected: true, value: want.UnixMicro(), want: want},
		{name: "zero means never active", connected: true, value: uint64(0), wantErr: true},
		{name: "unsupported type", connected: true, value: "nope", wantErr: true},
		{name: "property error", connected: true, value: uint64(1), propErr: errDbus, wantErr: true},
		{name: "no connection", connected: false, value: uint64(1), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeDbusConn{
				connected: tc.connected,
				getPropertyFunc: func(_ context.Context, unit, propertyName string) (*dbus.Property, error) {
					if propertyName != "ActiveEnterTimestamp" {
						t.Errorf("property = %q, want ActiveEnterTimestamp", propertyName)
					}

					if tc.propErr != nil {
						return nil, tc.propErr
					}

					return &dbus.Property{Value: godbus.MakeVariant(tc.value)}, nil
				},
			}
			sm := newTestManager(t, conn)

			got, err := sm.UnitStartTime(context.Background(), "app.service")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}

			if err == nil && !got.Equal(tc.want) {
				t.Fatalf("time = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManager_UnitExitStatus(t *testing.T) {
	cases := []struct {
		name       string
		connected  bool
		props      map[string]any
		propsErr   error
		wantCode   int32
		wantStatus int32
		wantErr    bool
	}{
		{
			name:      "exited with code",
			connected: true,
			props:     map[string]any{"ExecMainCode": int32(CLDExited), "ExecMainStatus": int32(42)},
			wantCode:  CLDExited, wantStatus: 42,
		},
		{
			name:      "killed by signal",
			connected: true,
			props:     map[string]any{"ExecMainCode": int32(CLDKilled), "ExecMainStatus": int32(9)},
			wantCode:  CLDKilled, wantStatus: 9,
		},
		{
			name:      "missing ExecMainCode",
			connected: true,
			props:     map[string]any{"ExecMainStatus": int32(0)},
			wantErr:   true,
		},
		{
			name:      "wrong ExecMainStatus type",
			connected: true,
			props:     map[string]any{"ExecMainCode": int32(CLDExited), "ExecMainStatus": uint32(1)},
			wantErr:   true,
		},
		{name: "properties error", connected: true, propsErr: errDbus, wantErr: true},
		{name: "no connection", connected: false, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeDbusConn{
				connected: tc.connected,
				getTypePropertiesFunc: func(_ context.Context, _, unitType string) (map[string]any, error) {
					if unitType != "Service" {
						t.Errorf("unit type = %q, want Service", unitType)
					}

					return tc.props, tc.propsErr
				},
			}
			sm := newTestManager(t, conn)

			got, err := sm.UnitExitStatus(context.Background(), "app.service")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}

			if err == nil && (got.Code != tc.wantCode || got.Status != tc.wantStatus) {
				t.Fatalf("status = %+v, want {Code:%d Status:%d}", got, tc.wantCode, tc.wantStatus)
			}
		})
	}
}

func TestCacheUnitProperties_ExtractsControlGroup(t *testing.T) {
	cases := []struct {
		name     string
		props    map[string]any
		propsErr error
		want     string
		wantMiss bool
	}{
		{
			name:  "cgroup present",
			props: map[string]any{"ControlGroup": "/system.slice/app.service"},
			want:  "/system.slice/app.service",
		},
		{
			name:  "cgroup missing is cached empty",
			props: map[string]any{"Id": "app.service"},
			want:  "",
		},
		{
			name:  "cgroup wrong type is cached empty",
			props: map[string]any{"ControlGroup": 42},
			want:  "",
		},
		{
			name:     "dbus error caches nothing",
			propsErr: errDbus,
			wantMiss: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeDbusConn{
				connected: true,
				getPropertiesFunc: func(context.Context, string) (map[string]any, error) {
					return tc.props, tc.propsErr
				},
			}
			sm := newTestManager(t, conn)
			register(sm, "app.service")

			sm.cacheUnitProperties(context.Background(), "app.service")

			sm.unitsLock.RLock()
			st := sm.units["app.service"]
			cached, controlGroup := !st.cachedAt.IsZero(), st.controlGroup

			sm.unitsLock.RUnlock()

			if tc.wantMiss {
				if cached {
					t.Fatalf("expected nothing cached, got %q", controlGroup)
				}

				return
			}

			if !cached {
				t.Fatalf("expected app.service to be cached")
			}

			if controlGroup != tc.want {
				t.Fatalf("controlGroup = %q, want %q", controlGroup, tc.want)
			}
		})
	}
}

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

func TestRegisterUnit_CreatesWakeChannel(t *testing.T) {
	conn := &fakeDbusConn{
		connected:         true,
		getPropertiesFunc: func(context.Context, string) (map[string]any, error) { return map[string]any{}, nil },
	}
	sm := newTestManager(t, conn)

	if ch := sm.WakeChannel("app.service"); ch != nil {
		t.Fatalf("expected no wake channel before registration")
	}

	sm.RegisterUnit(context.Background(), "app.service")

	if ch := sm.WakeChannel("app.service"); ch == nil {
		t.Fatalf("expected a wake channel after RegisterUnit")
	}
}

func TestUnregisterUnit_RemovesWakeChannel(t *testing.T) {
	conn := &fakeDbusConn{
		connected:         true,
		getPropertiesFunc: func(context.Context, string) (map[string]any, error) { return map[string]any{}, nil },
	}
	sm := newTestManager(t, conn)

	sm.RegisterUnit(context.Background(), "app.service")
	sm.UnregisterUnit("app.service")

	if ch := sm.WakeChannel("app.service"); ch != nil {
		t.Fatalf("expected wake channel to be gone after UnregisterUnit")
	}
}

func TestWakeUnit(t *testing.T) {
	t.Run("signals registered unit", func(t *testing.T) {
		sm := newTestManager(t, &fakeDbusConn{connected: true})
		register(sm, "app.service")

		sm.wakeUnit("app.service")

		select {
		case <-sm.WakeChannel("app.service"):
		default:
			t.Fatalf("expected wake channel to receive a signal")
		}
	})

	t.Run("non-blocking when already pending", func(t *testing.T) {
		sm := newTestManager(t, &fakeDbusConn{connected: true})
		register(sm, "app.service")

		// Two wakes in a row must not block even though the channel holds
		// only one pending signal - a redundant wake is a no-op, not a stall.
		done := make(chan struct{})

		go func() {
			sm.wakeUnit("app.service")
			sm.wakeUnit("app.service")
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("wakeUnit blocked instead of dropping the redundant signal")
		}
	})

	t.Run("no-op for unknown unit", func(t *testing.T) {
		sm := newTestManager(t, &fakeDbusConn{connected: true})
		sm.wakeUnit("does-not-exist.service") // must not panic
	})
}

func TestPropertiesDispatchLoop_WakesTheReportedUnit(t *testing.T) {
	sm := newTestManager(t, &fakeDbusConn{connected: true})

	register(sm, "app.service")

	sm.wg.Add(1)

	go sm.propertiesDispatchLoop()

	sm.propUpdateCh <- &dbus.PropertiesUpdate{UnitName: "app.service"}

	select {
	case <-sm.WakeChannel("app.service"):
	case <-time.After(time.Second):
		t.Fatalf("expected propertiesDispatchLoop to wake app.service")
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

func TestStreamLogs_RefusedAfterStop(t *testing.T) {
	sm := newTestManager(t, &fakeDbusConn{connected: true})

	register(sm, "app.service")

	sm.unitsLock.Lock()
	sm.stopping = true
	sm.unitsLock.Unlock()

	if err := sm.StreamLogs("app.service", make(chan *LogEntry)); err == nil {
		t.Fatalf("expected StreamLogs to refuse work after manager stop")
	}
}

// TestStreamLogs_RefusedForUnregisteredUnit covers the ordering that used to
// leak a journal reader for the rest of the process's life: callers start
// StreamLogs from a detached goroutine, so a task destroyed right after being
// started can have its UnregisterUnit run first. Registering a cancel func at
// that point would leave nobody to call it, and the reader would block forever
// writing to the dead handler's LogCh.
//
// This exercises the pre-open check; the identical re-check after the journal
// is opened is what actually closes the race, but it can only be reached on a
// host with a real journal, which the test host is not.
func TestStreamLogs_RefusedForUnregisteredUnit(t *testing.T) {
	sm := newTestManager(t, &fakeDbusConn{connected: true})

	// Deliberately not registered: no RegisterUnit call, matching the state
	// left behind by UnregisterUnit.
	err := sm.StreamLogs("app.service", make(chan *LogEntry))
	if err == nil {
		t.Fatalf("expected StreamLogs to refuse an unregistered unit")
	}

	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should say the unit is unregistered, got: %v", err)
	}

	sm.unitsLock.RLock()
	_, stored := sm.units["app.service"]
	sm.unitsLock.RUnlock()

	if stored {
		t.Errorf("a refused StreamLogs must not leave a cancel func behind")
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
