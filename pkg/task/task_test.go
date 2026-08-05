package task

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/systemd"
)

var errNotConfigured = errors.New("not configured in this test")

// fakeUnits is a unitController test double. Unset function fields report a
// clear "not configured" error rather than panicking, so each test wires up
// only the calls its scenario exercises.
type fakeUnits struct {
	stopFunc       func(ctx context.Context, unit string) error
	killFunc       func(ctx context.Context, unit string) error
	stateFunc      func(ctx context.Context, unit string) (systemd.UnitState, error)
	exitStatusFunc func(ctx context.Context, unit string) (systemd.ExitStatus, error)
	statsFunc      func(ctx context.Context, unit string) (*systemd.ResourceStats, error)
	streamFunc     func(unit string, logCh chan<- *systemd.LogEntry) error

	mu    sync.Mutex
	calls []string
}

func (f *fakeUnits) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, name)
}

func (f *fakeUnits) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0

	for _, c := range f.calls {
		if c == name {
			n++
		}
	}

	return n
}

func (f *fakeUnits) StopUnit(ctx context.Context, unit string) error {
	f.record("stop")

	if f.stopFunc == nil {
		return errNotConfigured
	}

	return f.stopFunc(ctx, unit)
}

func (f *fakeUnits) KillUnit(ctx context.Context, unit string) error {
	f.record("kill")

	if f.killFunc == nil {
		return errNotConfigured
	}

	return f.killFunc(ctx, unit)
}

func (f *fakeUnits) UnitState(ctx context.Context, unit string) (systemd.UnitState, error) {
	f.record("state")

	if f.stateFunc == nil {
		return "", errNotConfigured
	}

	return f.stateFunc(ctx, unit)
}

func (f *fakeUnits) UnitExitStatus(ctx context.Context, unit string) (systemd.ExitStatus, error) {
	f.record("exit_status")

	if f.exitStatusFunc == nil {
		return systemd.ExitStatus{}, errNotConfigured
	}

	return f.exitStatusFunc(ctx, unit)
}

func (f *fakeUnits) ResourceStats(ctx context.Context, unit string) (*systemd.ResourceStats, error) {
	f.record("stats")

	if f.statsFunc == nil {
		return nil, errNotConfigured
	}

	return f.statsFunc(ctx, unit)
}

func (f *fakeUnits) StreamLogs(unit string, logCh chan<- *systemd.LogEntry) error {
	f.record("stream_logs")

	if f.streamFunc == nil {
		return nil
	}

	return f.streamFunc(unit, logCh)
}

var _ unitController = (*fakeUnits)(nil)

// newTestHandler builds a Handler over units for exercising its state machine.
func newTestHandler(t *testing.T, units unitController, initialState drivers.TaskState) *Handler {
	t.Helper()

	return NewHandler("task-1", "example.service", &drivers.TaskHandle{}, units, nil, initialState, logx.New(hclog.NewNullLogger()))
}

func TestNewHandler_WaitChReflectsInitialState(t *testing.T) {
	cases := []struct {
		name         string
		initialState drivers.TaskState
		wantClosed   bool
	}{
		{name: "already exited closes waitCh", initialState: drivers.TaskStateExited, wantClosed: true},
		{name: "running leaves waitCh open", initialState: drivers.TaskStateRunning, wantClosed: false},
		{name: "unknown leaves waitCh open", initialState: drivers.TaskStateUnknown, wantClosed: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t, &fakeUnits{}, tc.initialState)

			closed := false

			select {
			case <-h.WaitCh():
				closed = true
			default:
			}

			if closed != tc.wantClosed {
				t.Fatalf("waitCh closed = %v, want %v", closed, tc.wantClosed)
			}
		})
	}
}

// TestIsRunningAndHasExited pins the distinction the two predicates exist to
// make. A transitional systemd state maps to TaskStateUnknown, where BOTH are
// false - so IsRunning is not the negation of HasExited, and teardown paths that
// treat it as such drop the task while its unit is still alive.
func TestIsRunningAndHasExited(t *testing.T) {
	cases := []struct {
		name          string
		unitState     systemd.UnitState
		wantRunning   bool
		wantHasExited bool
	}{
		{name: "active", unitState: systemd.UnitStateActive, wantRunning: true},
		{name: "inactive", unitState: systemd.UnitStateInactive, wantHasExited: true},
		{name: "failed", unitState: systemd.UnitStateFailed, wantHasExited: true},

		// Neither running nor exited - the case that motivated HasExited.
		{name: "activating", unitState: systemd.UnitStateActivating},
		{name: "deactivating", unitState: systemd.UnitStateDeactivating},
		{name: "reloading", unitState: systemd.UnitStateReloading},
		{name: "maintenance", unitState: systemd.UnitStateMaintenance},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t, &fakeUnits{}, drivers.TaskStateRunning)
			h.handleStateChange(tc.unitState)

			if got := h.IsRunning(); got != tc.wantRunning {
				t.Errorf("IsRunning() = %v, want %v", got, tc.wantRunning)
			}

			if got := h.HasExited(); got != tc.wantHasExited {
				t.Errorf("HasExited() = %v, want %v", got, tc.wantHasExited)
			}
		})
	}
}

func TestSetExitResult(t *testing.T) {
	h := newTestHandler(t, &fakeUnits{}, drivers.TaskStateExited)
	completedAt := time.Now()
	exitResult := &drivers.ExitResult{ExitCode: 3}

	h.SetExitResult(exitResult, completedAt)

	status := h.TaskStatus()
	if status.ExitResult != exitResult {
		t.Fatalf("expected exit result to be set")
	}

	if !status.CompletedAt.Equal(completedAt) {
		t.Fatalf("expected completedAt to be set")
	}
}

func TestSetStartedAt(t *testing.T) {
	h := newTestHandler(t, &fakeUnits{}, drivers.TaskStateRunning)
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	h.SetStartedAt(startedAt)

	if got := h.TaskStatus().StartedAt; !got.Equal(startedAt) {
		t.Fatalf("startedAt = %v, want %v", got, startedAt)
	}
}

func TestStopUnit_And_KillUnit_DelegateToController(t *testing.T) {
	cases := []struct {
		name    string
		wire    func(units *fakeUnits)
		run     func(h *Handler) error
		call    string
		wantErr bool
	}{
		{
			name: "stop success",
			wire: func(u *fakeUnits) {
				u.stopFunc = func(context.Context, string) error { return nil }
			},
			run:  (*Handler).StopUnit,
			call: "stop",
		},
		{
			name: "stop error propagates",
			wire: func(u *fakeUnits) {
				u.stopFunc = func(context.Context, string) error { return errNotConfigured }
			},
			run:     (*Handler).StopUnit,
			call:    "stop",
			wantErr: true,
		},
		{
			name: "kill success",
			wire: func(u *fakeUnits) {
				u.killFunc = func(context.Context, string) error { return nil }
			},
			run:  (*Handler).KillUnit,
			call: "kill",
		},
		{
			name: "kill error propagates",
			wire: func(u *fakeUnits) {
				u.killFunc = func(context.Context, string) error { return errNotConfigured }
			},
			run:     (*Handler).KillUnit,
			call:    "kill",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			units := &fakeUnits{}
			tc.wire(units)

			h := newTestHandler(t, units, drivers.TaskStateRunning)

			if err := tc.run(h); (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}

			if got := units.callCount(tc.call); got != 1 {
				t.Fatalf("%s called %d times, want 1", tc.call, got)
			}
		})
	}
}
