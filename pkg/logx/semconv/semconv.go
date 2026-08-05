package semconv

import (
	"fmt"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
)

// TaskID returns the Nomad task's ID.
func TaskID(id string) logx.Attr {
	return logx.String("nomad.task.id", id)
}

// TaskState returns the task's state as Nomad spells it.
func TaskState(state drivers.TaskState) logx.Attr {
	return logx.String("nomad.task.state", string(state))
}

// TaskStateChange returns both sides of a task state transition.
func TaskStateChange(from, to drivers.TaskState) logx.Attr {
	return logx.Multi(
		logx.String("nomad.task.state.from", string(from)),
		logx.String("nomad.task.state.to", string(to)),
	)
}

// DestroyForce returns whether Nomad asked for a task to be destroyed while
// still running.
func DestroyForce(force bool) logx.Attr {
	return logx.Bool("nomad.task.destroy_force", force)
}

// Unit returns the systemd unit's name, including its suffix.
func Unit(name string) logx.Attr {
	return logx.String("systemd.unit.name", name)
}

// UnitState returns the unit's ActiveState. It takes a [fmt.Stringer] so that
// this package need not import the one defining the state type.
func UnitState(state fmt.Stringer) logx.Attr {
	return logx.Stringer("systemd.unit.state", state)
}

// UnitCgroup returns the unit's control group path as systemd reports it in the
// ControlGroup property.
func UnitCgroup(path string) logx.Attr {
	return logx.String("systemd.unit.cgroup", path)
}

// UnitProperty returns the name of a systemd unit property being read.
func UnitProperty(name string) logx.Attr {
	return logx.String("systemd.unit.property", name)
}

// Timeout returns the deadline an operation was given.
func Timeout(timeout time.Duration) logx.Attr {
	return logx.Duration("timeout", timeout)
}

// RetryAttempt returns which attempt of a retried operation this is, counting
// from one.
func RetryAttempt(attempt int) logx.Attr {
	return logx.Int("retry.attempt", attempt)
}

// RetryDelay returns how long a retried operation waits before trying again.
func RetryDelay(delay time.Duration) logx.Attr {
	return logx.Duration("retry.delay", delay)
}

// FilePath returns the path of a file being read or written.
func FilePath(path string) logx.Attr {
	return logx.String("file.path", path)
}
