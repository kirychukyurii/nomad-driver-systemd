package semconv

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
)

type unitState string

func (s unitState) String() string { return string(s) }

// TestAttributes pins the key each function emits. A failure here means an
// attribute was renamed, which changes what operators query on.
func TestAttributes(t *testing.T) {
	cases := []struct {
		name string
		attr logx.Attr
		want map[string]any
	}{
		{
			name: "task id",
			attr: TaskID("a1b2"),
			want: map[string]any{"nomad.task.id": "a1b2"},
		},
		{
			name: "task state",
			attr: TaskState(drivers.TaskStateRunning),
			want: map[string]any{"nomad.task.state": "running"},
		},
		{
			name: "task state change",
			attr: TaskStateChange(drivers.TaskStateRunning, drivers.TaskStateExited),
			want: map[string]any{
				"nomad.task.state.from": "running",
				"nomad.task.state.to":   "exited",
			},
		},
		{
			name: "destroy force",
			attr: DestroyForce(true),
			want: map[string]any{"nomad.task.destroy_force": true},
		},
		{
			name: "unit",
			attr: Unit("web.service"),
			want: map[string]any{"systemd.unit.name": "web.service"},
		},
		{
			name: "unit state",
			attr: UnitState(unitState("active")),
			want: map[string]any{"systemd.unit.state": "active"},
		},
		{
			name: "unit cgroup",
			attr: UnitCgroup("/system.slice/web.service"),
			want: map[string]any{"systemd.unit.cgroup": "/system.slice/web.service"},
		},
		{
			name: "unit property",
			attr: UnitProperty("ControlGroup"),
			want: map[string]any{"systemd.unit.property": "ControlGroup"},
		},
		{
			name: "timeout",
			attr: Timeout(30 * time.Second),
			want: map[string]any{"timeout": "30s"},
		},
		{
			name: "retry attempt",
			attr: RetryAttempt(2),
			want: map[string]any{"retry.attempt": json.Number("2")},
		},
		{
			name: "retry delay",
			attr: RetryDelay(2 * time.Second),
			want: map[string]any{"retry.delay": "2s"},
		},
		{
			name: "file path",
			attr: FilePath("/sys/fs/cgroup/memory.current"),
			want: map[string]any{"file.path": "/sys/fs/cgroup/memory.current"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			logx.New(hclog.New(&hclog.LoggerOptions{
				Output:      buf,
				Level:       hclog.Debug,
				JSONFormat:  true,
				DisableTime: true,
			})).Info("msg", tc.attr)

			dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
			dec.UseNumber()

			var got map[string]any
			if err := dec.Decode(&got); err != nil {
				t.Fatalf("decode record %q: %v", buf.String(), err)
			}

			delete(got, "@level")
			delete(got, "@message")

			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("attributes = %#v, want %#v", got, tc.want)
			}
		})
	}
}
