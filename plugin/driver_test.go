package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"

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
