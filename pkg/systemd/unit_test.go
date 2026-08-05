package systemd

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

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

func TestParseTimestampUsec(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		wantOK  bool
		wantVal uint64
	}{
		{"uint64", uint64(1234567890), true, 1234567890},
		{"positive int64", int64(42), true, 42},
		{"negative int64 rejected", int64(-1), false, 0},
		{"zero uint64", uint64(0), true, 0},
		{"unsupported type", "not a number", false, 0},
		{"nil", nil, false, 0},

		// USEC_INFINITY is systemd's "never". Accepting it would overflow the
		// int64 time.UnixMicro takes and silently report December 1969 as the
		// unit's start time.
		{"USEC_INFINITY rejected", usecInfinity, false, 0},
		{"anything past MaxInt64 rejected", uint64(math.MaxInt64) + 1, false, 0},
		{"MaxInt64 itself is still accepted", uint64(math.MaxInt64), true, uint64(math.MaxInt64)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseTimestampUsec(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}

			if ok && got != tc.wantVal {
				t.Fatalf("value = %v, want %v", got, tc.wantVal)
			}
		})
	}
}
