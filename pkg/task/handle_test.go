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

func TestHandleStateChange(t *testing.T) {
	cases := []struct {
		name string
		// unit state reported by systemd
		unitState systemd.UnitState
		// exit status the unit reports, or nil to make the lookup fail
		// (exercising the ActiveState-only fallback)
		exitStatus *systemd.ExitStatus
		wantState  drivers.TaskState
		wantClosed bool
		// only checked when wantClosed
		wantExitCode int
		wantSignal   int
		wantExitErr  bool
	}{
		{
			name:      "active keeps running",
			unitState: systemd.UnitStateActive,
			wantState: drivers.TaskStateRunning,
		},
		{
			name:      "activating maps to unknown",
			unitState: systemd.UnitStateActivating,
			wantState: drivers.TaskStateUnknown,
		},
		{
			name:      "reloading maps to unknown",
			unitState: systemd.UnitStateReloading,
			wantState: drivers.TaskStateUnknown,
		},
		{
			name:         "inactive exits cleanly via fallback",
			unitState:    systemd.UnitStateInactive,
			wantState:    drivers.TaskStateExited,
			wantClosed:   true,
			wantExitCode: 0,
		},
		{
			name:         "failed exits non-zero via fallback",
			unitState:    systemd.UnitStateFailed,
			wantState:    drivers.TaskStateExited,
			wantClosed:   true,
			wantExitCode: 1,
			wantExitErr:  true,
		},
		{
			name:         "real exit code preferred over fallback",
			unitState:    systemd.UnitStateFailed,
			exitStatus:   &systemd.ExitStatus{Code: systemd.CLDExited, Status: 42},
			wantState:    drivers.TaskStateExited,
			wantClosed:   true,
			wantExitCode: 42,
			wantExitErr:  true,
		},
		{
			name:         "clean real exit code has no error",
			unitState:    systemd.UnitStateInactive,
			exitStatus:   &systemd.ExitStatus{Code: systemd.CLDExited, Status: 0},
			wantState:    drivers.TaskStateExited,
			wantClosed:   true,
			wantExitCode: 0,
		},
		{
			name:         "signal death reports signal",
			unitState:    systemd.UnitStateFailed,
			exitStatus:   &systemd.ExitStatus{Code: systemd.CLDKilled, Status: 9},
			wantState:    drivers.TaskStateExited,
			wantClosed:   true,
			wantExitCode: -1,
			wantSignal:   9,
			wantExitErr:  true,
		},
		{
			name:      "non-terminal exit code falls back to ActiveState",
			unitState: systemd.UnitStateFailed,
			// code 0 is not one of CLDExited/CLDKilled/CLDDumped, so
			// ExitResultFromStatus returns nil and the heuristic applies
			exitStatus:   &systemd.ExitStatus{Code: 0, Status: 0},
			wantState:    drivers.TaskStateExited,
			wantClosed:   true,
			wantExitCode: 1,
			wantExitErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			units := &fakeUnits{}

			if tc.exitStatus != nil {
				status := *tc.exitStatus
				units.exitStatusFunc = func(context.Context, string) (systemd.ExitStatus, error) {
					return status, nil
				}
			}

			h := newTestHandler(t, units, drivers.TaskStateRunning)
			h.handleStateChange(tc.unitState)

			status := h.TaskStatus()
			if status.State != tc.wantState {
				t.Fatalf("state = %v, want %v", status.State, tc.wantState)
			}

			closed := false

			select {
			case <-h.WaitCh():
				closed = true
			default:
			}

			if closed != tc.wantClosed {
				t.Fatalf("waitCh closed = %v, want %v", closed, tc.wantClosed)
			}

			if !tc.wantClosed {
				return
			}

			if status.ExitResult == nil {
				t.Fatalf("expected an ExitResult once exited")
			}

			if status.ExitResult.ExitCode != tc.wantExitCode {
				t.Errorf("exit code = %d, want %d", status.ExitResult.ExitCode, tc.wantExitCode)
			}

			if status.ExitResult.Signal != tc.wantSignal {
				t.Errorf("signal = %d, want %d", status.ExitResult.Signal, tc.wantSignal)
			}

			if (status.ExitResult.Err != nil) != tc.wantExitErr {
				t.Errorf("exit err = %v, wantErr %v", status.ExitResult.Err, tc.wantExitErr)
			}

			if status.CompletedAt.IsZero() {
				t.Errorf("expected completedAt to be set")
			}
		})
	}
}

// TestHandleStateChange_ExitedIsSticky guards against a regression where a
// unit that transitions Exited -> Running -> Exited (e.g. restarted
// out-of-band) would close waitCh twice and panic.
func TestHandleStateChange_ExitedIsSticky(t *testing.T) {
	h := newTestHandler(t, &fakeUnits{}, drivers.TaskStateRunning)

	h.handleStateChange(systemd.UnitStateFailed)
	firstExit := h.TaskStatus().ExitResult

	// Unit somehow reports active again; the task must not "revive".
	h.handleStateChange(systemd.UnitStateActive)
	// And exiting again must not attempt to close waitCh a second time.
	h.handleStateChange(systemd.UnitStateInactive)

	status := h.TaskStatus()
	if status.State != drivers.TaskStateExited {
		t.Fatalf("state = %v, want it to remain Exited", status.State)
	}

	if status.ExitResult != firstExit {
		t.Fatalf("expected the first recorded exit result to stand, got %+v", status.ExitResult)
	}
}

// TestHandleStateChange_ConsistentSnapshotDuringExitFetch pins the invariant
// that the published task status is never half-transitioned: while the exit
// status lookup is in flight, the task still reads as Running with an open
// WaitCh; once published, State/ExitResult/WaitCh flip together.
func TestHandleStateChange_ConsistentSnapshotDuringExitFetch(t *testing.T) {
	fetchStarted := make(chan struct{})
	release := make(chan struct{})

	units := &fakeUnits{
		exitStatusFunc: func(context.Context, string) (systemd.ExitStatus, error) {
			close(fetchStarted)
			<-release

			return systemd.ExitStatus{Code: systemd.CLDExited, Status: 7}, nil
		},
	}
	h := newTestHandler(t, units, drivers.TaskStateRunning)

	done := make(chan struct{})

	go func() {
		defer close(done)

		h.handleStateChange(systemd.UnitStateFailed)
	}()

	<-fetchStarted

	// Mid-fetch: the transition must not be visible yet.
	status := h.TaskStatus()
	if status.State != drivers.TaskStateRunning {
		t.Errorf("mid-fetch state = %v, want Running", status.State)
	}

	if status.ExitResult != nil {
		t.Errorf("mid-fetch ExitResult = %+v, want nil", status.ExitResult)
	}

	select {
	case <-h.WaitCh():
		t.Errorf("waitCh must not be closed mid-fetch")
	default:
	}

	close(release)
	<-done

	status = h.TaskStatus()
	if status.State != drivers.TaskStateExited {
		t.Fatalf("state = %v, want Exited", status.State)
	}

	if status.ExitResult == nil || status.ExitResult.ExitCode != 7 {
		t.Fatalf("ExitResult = %+v, want exit code 7", status.ExitResult)
	}

	if status.CompletedAt.IsZero() {
		t.Fatalf("expected completedAt to be set together with the transition")
	}

	select {
	case <-h.WaitCh():
	default:
		t.Fatalf("expected waitCh closed together with the transition")
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

// TestPollTaskState_ImmediateInitialCheck verifies that a state change which
// happened BEFORE the poll loop started (so no wake signal will ever arrive
// for it) is detected right away instead of waiting for the 30s safety net.
func TestPollTaskState_ImmediateInitialCheck(t *testing.T) {
	units := &fakeUnits{
		stateFunc: func(context.Context, string) (systemd.UnitState, error) {
			return systemd.UnitStateInactive, nil
		},
	}
	h := newTestHandler(t, units, drivers.TaskStateRunning)
	t.Cleanup(h.Stop)

	h.wg.Add(1)

	go func() {
		defer h.wg.Done()

		h.pollTaskState()
	}()

	select {
	case <-h.WaitCh():
	case <-time.After(2 * time.Second):
		t.Fatalf("expected the pre-existing exit to be detected immediately, not after the safety net")
	}
}

// TestPollTaskState_WakeSignalTriggersImmediateRecheck covers the integration
// point between the Manager's push mechanism and the Handler: a wake signal
// must trigger a state lookup without waiting for safetyNetInterval (30s).
func TestPollTaskState_WakeSignalTriggersImmediateRecheck(t *testing.T) {
	stateChecked := make(chan struct{}, 4)
	units := &fakeUnits{
		stateFunc: func(context.Context, string) (systemd.UnitState, error) {
			select {
			case stateChecked <- struct{}{}:
			default:
			}

			return systemd.UnitStateActive, nil
		},
	}

	wakeCh := make(chan struct{}, 1)
	h := NewHandler("task-1", "example.service", &drivers.TaskHandle{}, units, wakeCh, drivers.TaskStateRunning, logx.New(hclog.NewNullLogger()))
	t.Cleanup(h.Stop)

	h.wg.Add(1)

	go func() {
		defer h.wg.Done()

		h.pollTaskState()
	}()

	// First lookup comes from the immediate initial check.
	select {
	case <-stateChecked:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected an immediate initial state check on poll start")
	}

	// A wake signal must trigger another one, well before the safety net.
	wakeCh <- struct{}{}

	select {
	case <-stateChecked:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected wake signal to trigger a state check well before the 30s safety net")
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

func TestMapPriorityToLevel(t *testing.T) {
	cases := []struct {
		priority string
		want     string
	}{
		{"0", "EMERG"},
		{"1", "ALERT"},
		{"2", "CRIT"},
		{"3", "ERR"},
		{"4", "WARN"},
		{"5", "NOTICE"},
		{"6", "INFO"},
		{"7", "DEBUG"},
		{"", "UNKNOWN"},
		{"9", "UNKNOWN"},
		{"garbage", "UNKNOWN"},
	}

	for _, tc := range cases {
		t.Run("priority "+tc.priority, func(t *testing.T) {
			if got := mapPriorityToLevel(tc.priority); got != tc.want {
				t.Fatalf("mapPriorityToLevel(%q) = %q, want %q", tc.priority, got, tc.want)
			}
		})
	}
}
