package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// blockingUnits holds a state read open until its gate is closed, so that a test
// can observe what happens while a handler is mid-call.
type blockingUnits struct {
	*fakeUnits

	gate chan struct{}
}

func (b *blockingUnits) UnitState(ctx context.Context, unit string) (systemd.UnitState, error) {
	<-b.gate

	return b.fakeUnits.UnitState(ctx, unit)
}

// newStartedTaskHandle returns a handle whose log paths are ordinary files, so
// that a started Handler streams logs instead of retrying absent FIFOs.
func newStartedTaskHandle(t *testing.T) *drivers.TaskHandle {
	t.Helper()

	dir := t.TempDir()
	paths := make([]string, 0, 2)

	for _, name := range []string{"stdout", "stderr"} {
		path := filepath.Join(dir, name)

		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}

		_ = f.Close()

		paths = append(paths, path)
	}

	return &drivers.TaskHandle{
		Config: &drivers.TaskConfig{StdoutPath: paths[0], StderrPath: paths[1]},
	}
}

func TestDriver_Shutdown_WaitsForTaskHandlers(t *testing.T) {
	d := newTestDriver()

	units := &blockingUnits{fakeUnits: &fakeUnits{}, gate: make(chan struct{})}
	h := task.NewHandler("task-1", "app.service", newStartedTaskHandle(t), units, nil, drivers.TaskStateRunning, logx.New(hclog.NewNullLogger()))
	d.tasks.Set("task-1", h)
	h.Start()

	returned := make(chan struct{})

	go func() {
		defer close(returned)

		d.Shutdown()
	}()

	select {
	case <-returned:
		t.Fatal("Shutdown returned while a task handler was still mid-call")

	case <-time.After(50 * time.Millisecond):
	}

	close(units.gate)

	select {
	case <-returned:
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("Shutdown did not return after the task handler was released")
	}

	if !units.called("state") {
		t.Fatal("expected the handler to have read the unit state before stopping")
	}

	if d.ctx.Err() == nil {
		t.Fatal("Shutdown left the driver context live")
	}
}

func TestDriver_Shutdown_StopsPprof(t *testing.T) {
	d := newTestDriver()

	addr := freeLoopbackAddr(t)

	if err := d.configurePprof(addr); err != nil {
		t.Fatalf("configurePprof = %v, want nil", err)
	}

	d.Shutdown()

	if d.pprof != nil {
		t.Fatal("Shutdown left a pprof server behind")
	}

	resp, err := get(t, "http://"+addr+"/debug/pprof/cmdline")
	if err == nil {
		_ = resp.Body.Close()

		t.Fatal("pprof server is still serving after Shutdown")
	}
}

func TestDriver_Shutdown_WithoutManagerOrTasks(t *testing.T) {
	d := newTestDriver()

	d.Shutdown()

	if d.ctx.Err() == nil {
		t.Fatal("Shutdown left the driver context live")
	}
}
