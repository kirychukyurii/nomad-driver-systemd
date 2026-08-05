package plugin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/systemd"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/task"
)

func newTestDriver() *Driver {
	ctx, cancel := context.WithCancel(context.Background())

	return &Driver{
		tasks:          task.NewStore(),
		unitOwners:     make(map[string]string),
		ctx:            ctx,
		signalShutdown: cancel,
		logger:         logx.New(hclog.NewNullLogger()),
	}
}

// fakeUnits stands in for the systemd.Manager behavior a task.Handler needs,
// recording which operations the driver drove it through.
type fakeUnits struct {
	stopErr error
	killErr error

	mu    sync.Mutex
	calls []string
}

func (f *fakeUnits) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, name)
}

func (f *fakeUnits) called(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, c := range f.calls {
		if c == name {
			return true
		}
	}

	return false
}

func (f *fakeUnits) StopUnit(context.Context, string) error {
	f.record("stop")

	return f.stopErr
}

func (f *fakeUnits) KillUnit(context.Context, string) error {
	f.record("kill")

	return f.killErr
}

func (f *fakeUnits) UnitState(context.Context, string) (systemd.UnitState, error) {
	f.record("state")

	return systemd.UnitStateActive, nil
}

func (f *fakeUnits) UnitExitStatus(context.Context, string) (systemd.ExitStatus, error) {
	f.record("exit_status")

	return systemd.ExitStatus{}, errors.New("not available")
}

func (f *fakeUnits) ResourceStats(context.Context, string) (*systemd.ResourceStats, error) {
	f.record("stats")

	return systemd.EmptyResourceStats(), nil
}

func (f *fakeUnits) StreamLogs(string, chan<- *systemd.LogEntry) error {
	f.record("stream_logs")

	return nil
}

// newTask registers a task in state backed by units and returns it.
func newTask(t *testing.T, d *Driver, units *fakeUnits, state drivers.TaskState) *task.Handler {
	t.Helper()

	h := task.NewHandler("task-1", "app.service", &drivers.TaskHandle{}, units, nil, state, logx.New(hclog.NewNullLogger()))
	d.tasks.Set("task-1", h)
	t.Cleanup(h.Stop)

	return h
}

// newRunningTask registers a running task backed by units and returns both.
func newRunningTask(t *testing.T, d *Driver, units *fakeUnits) *task.Handler {
	t.Helper()

	return newTask(t, d, units, drivers.TaskStateRunning)
}

func TestUnitOwnership(t *testing.T) {
	cases := []struct {
		name string
		// ops are applied in order; each is either a claim or a release
		ops []struct {
			release bool
			unit    string
			taskID  string
			wantErr bool
		}
		wantOwner map[string]string
	}{
		{
			name: "claiming a free unit succeeds",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{unit: "app.service", taskID: "task-a"},
			},
			wantOwner: map[string]string{"app.service": "task-a"},
		},
		{
			name: "conflicting owner is rejected without mutating state",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{unit: "app.service", taskID: "task-a"},
				{unit: "app.service", taskID: "task-b", wantErr: true},
			},
			wantOwner: map[string]string{"app.service": "task-a"},
		},
		{
			name: "reclaiming by the same owner is idempotent",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{unit: "app.service", taskID: "task-a"},
				{unit: "app.service", taskID: "task-a"},
			},
			wantOwner: map[string]string{"app.service": "task-a"},
		},
		{
			name: "release allows another task to claim",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{unit: "app.service", taskID: "task-a"},
				{release: true, unit: "app.service"},
				{unit: "app.service", taskID: "task-b"},
			},
			wantOwner: map[string]string{"app.service": "task-b"},
		},
		{
			name: "releasing an unknown unit is a no-op",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{release: true, unit: "nope.service"},
			},
			wantOwner: map[string]string{},
		},
		{
			name: "different units are independent",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{unit: "a.service", taskID: "task-a"},
				{unit: "b.service", taskID: "task-b"},
			},
			wantOwner: map[string]string{"a.service": "task-a", "b.service": "task-b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver()

			for i, op := range tc.ops {
				if op.release {
					d.releaseUnit(op.unit)

					continue
				}

				err := d.claimUnit(op.unit, op.taskID)
				if (err != nil) != op.wantErr {
					t.Fatalf("op %d: claimUnit(%q, %q) error = %v, wantErr %v", i, op.unit, op.taskID, err, op.wantErr)
				}
			}

			if len(d.unitOwners) != len(tc.wantOwner) {
				t.Fatalf("owners = %v, want %v", d.unitOwners, tc.wantOwner)
			}

			for unit, want := range tc.wantOwner {
				if got := d.unitOwners[unit]; got != want {
					t.Errorf("owner of %q = %q, want %q", unit, got, want)
				}
			}
		})
	}
}

func TestClaimUnit_ErrorNamesCurrentOwner(t *testing.T) {
	d := newTestDriver()

	if err := d.claimUnit("app.service", "task-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err := d.claimUnit("app.service", "task-b")
	if err == nil {
		t.Fatalf("expected a conflict error")
	}

	if !strings.Contains(err.Error(), "task-a") {
		t.Fatalf("error should name the current owner, got: %v", err)
	}
}

func TestStopTask(t *testing.T) {
	cases := []struct {
		name string
		// timeout passed to StopTask (Nomad's kill_timeout)
		timeout  time.Duration
		stopErr  error
		killErr  error
		wantErr  bool
		wantKill bool
	}{
		{
			name:    "graceful stop completes",
			timeout: 30 * time.Second,
		},
		{
			name:     "zero timeout escalates immediately",
			timeout:  0,
			wantKill: true,
		},
		{
			name:     "negative timeout escalates immediately",
			timeout:  -time.Second,
			wantKill: true,
		},
		{
			name:     "stop failure escalates to kill",
			timeout:  30 * time.Second,
			stopErr:  errors.New("stop job failed"),
			wantKill: true,
		},
		{
			// forceKillTask can't confirm a terminal state without a manager,
			// so a failing kill must surface as an error.
			name:     "kill failure surfaces when state unconfirmable",
			timeout:  0,
			killErr:  errors.New("no such process"),
			wantKill: true,
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver()
			units := &fakeUnits{stopErr: tc.stopErr, killErr: tc.killErr}
			newRunningTask(t, d, units)

			err := d.StopTask("task-1", tc.timeout, "")
			if (err != nil) != tc.wantErr {
				t.Fatalf("StopTask error = %v, wantErr %v", err, tc.wantErr)
			}

			// The graceful stop is dispatched concurrently, so give it a
			// moment to be recorded before asserting.
			deadline := time.Now().Add(2 * time.Second)
			for !units.called("stop") && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}

			if !units.called("stop") {
				t.Errorf("expected a graceful stop to be attempted")
			}

			if got := units.called("kill"); got != tc.wantKill {
				t.Errorf("kill attempted = %v, want %v", got, tc.wantKill)
			}
		})
	}
}

func TestStopTask_UnknownTask(t *testing.T) {
	d := newTestDriver()

	if err := d.StopTask("nope", time.Second, ""); !errors.Is(err, drivers.ErrTaskNotFound) {
		t.Fatalf("error = %v, want ErrTaskNotFound", err)
	}
}

// TestDestroyTask covers which task states may be destroyed, and whether the
// unit gets stopped on the way out.
//
// The TaskStateUnknown rows are the interesting ones: a unit in a transitional
// systemd state (activating/deactivating/reloading) maps there, and it is
// neither Running nor Exited. DestroyTask used to key off IsRunning, so such a
// task fell through both the guard and the stop - it was silently dropped while
// its unit kept running, owned by nobody.
func TestDestroyTask(t *testing.T) {
	cases := []struct {
		name      string
		state     drivers.TaskState
		force     bool
		wantErr   bool
		wantStop  bool
		wantOwned bool // ownership still held after the call
	}{
		{
			name:  "exited task is destroyed without stopping anything",
			state: drivers.TaskStateExited,
		},
		{
			name:      "running task without force is rejected",
			state:     drivers.TaskStateRunning,
			wantErr:   true,
			wantOwned: true,
		},
		{
			name:     "running task with force is stopped first",
			state:    drivers.TaskStateRunning,
			force:    true,
			wantStop: true,
		},
		{
			name:      "unknown state without force is rejected",
			state:     drivers.TaskStateUnknown,
			wantErr:   true,
			wantOwned: true,
		},
		{
			name:     "unknown state with force is stopped first",
			state:    drivers.TaskStateUnknown,
			force:    true,
			wantStop: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver()
			units := &fakeUnits{}
			newTask(t, d, units, tc.state)

			if err := d.claimUnit("app.service", "task-1"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			err := d.DestroyTask("task-1", tc.force)
			if (err != nil) != tc.wantErr {
				t.Fatalf("DestroyTask error = %v, wantErr %v", err, tc.wantErr)
			}

			if got := units.called("stop"); got != tc.wantStop {
				t.Errorf("stop attempted = %v, want %v", got, tc.wantStop)
			}

			// A rejected destroy must be a complete no-op, so Nomad can safely
			// retry it (e.g. with force=true) without us having already
			// released the unit out from under a task that is still alive.
			_, tracked := d.tasks.Get("task-1")
			if tracked != tc.wantErr {
				t.Errorf("task still tracked = %v, want %v", tracked, tc.wantErr)
			}

			if owned := d.unitOwners["app.service"] == "task-1"; owned != tc.wantOwned {
				t.Errorf("unit still owned = %v, want %v", owned, tc.wantOwned)
			}
		})
	}
}

func TestDestroyTask_UnknownTask(t *testing.T) {
	d := newTestDriver()

	if err := d.DestroyTask("nope", false); !errors.Is(err, drivers.ErrTaskNotFound) {
		t.Fatalf("error = %v, want ErrTaskNotFound", err)
	}
}

func TestInspectTask_UnknownTask(t *testing.T) {
	d := newTestDriver()

	if _, err := d.InspectTask("nope"); !errors.Is(err, drivers.ErrTaskNotFound) {
		t.Fatalf("error = %v, want ErrTaskNotFound", err)
	}
}

func TestWaitTask_UnknownTask(t *testing.T) {
	d := newTestDriver()

	if _, err := d.WaitTask(context.Background(), "nope"); !errors.Is(err, drivers.ErrTaskNotFound) {
		t.Fatalf("error = %v, want ErrTaskNotFound", err)
	}
}
