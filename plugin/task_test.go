package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
)

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
