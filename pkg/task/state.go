package task

import (
	"errors"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx/semconv"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/systemd"
)

// safetyNetInterval is how long pollTaskState waits for a wake signal before
// re-reading the unit's state anyway. A wake signal may be dropped or never
// arrive, so it cannot be the only trigger.
const safetyNetInterval = 30 * time.Second

// pollTaskState watches the unit until the task exits, woken by wakeCh and, as
// an upper bound, by a safetyNetInterval ticker.
func (th *Handler) pollTaskState() {
	th.logger.Debug("starting state polling")

	ticker := time.NewTicker(safetyNetInterval)
	defer ticker.Stop()

	// A change that happened before this handler existed produces no future wake
	// signal, so check once up front.
	if th.checkTaskState() {
		return
	}

	for {
		select {
		case <-th.ctx.Done():
			th.logger.Debug("state polling stopped")

			return

		case <-th.wakeCh:
		case <-ticker.C:
		}

		if th.checkTaskState() {
			return
		}
	}
}

// checkTaskState reads the unit's state once and feeds it into the task state
// machine. It reports whether the task has exited, and so whether polling is
// finished.
func (th *Handler) checkTaskState() bool {
	th.stateLock.RLock()
	exited := th.state == drivers.TaskStateExited
	th.stateLock.RUnlock()

	if exited {
		return true
	}

	state, err := th.units.UnitState(th.ctx, th.Unit)
	if err != nil {
		th.logger.Warn("get unit state", logx.Err(err))

		return false
	}

	th.handleStateChange(state)

	th.stateLock.RLock()
	exited = th.state == drivers.TaskStateExited
	th.stateLock.RUnlock()

	return exited
}

// handleStateChange advances the task state machine to reflect activeState,
// publishing the exit transition and closing waitCh when the unit has stopped.
func (th *Handler) handleStateChange(activeState systemd.UnitState) {
	cst := systemd.ToTaskState(activeState)

	th.stateLock.Lock()

	// Exited is terminal: a Nomad task cannot revive. This also guarantees
	// waitCh is closed exactly once below.
	if th.state == drivers.TaskStateExited {
		th.stateLock.Unlock()

		return
	}

	ost := th.state

	if cst != drivers.TaskStateExited {
		if ost != cst {
			th.state = cst
			th.logger.Info("task state changed", semconv.TaskStateChange(ost, cst), semconv.UnitState(activeState))
		}
		th.stateLock.Unlock()

		return
	}

	th.stateLock.Unlock()

	// Resolve the exit result before publishing the transition: this is a DBus
	// round-trip, and flipping the state first would expose State=Exited with a
	// nil ExitResult for its whole duration.
	exitResult := th.buildExitResult(activeState)

	th.stateLock.Lock()

	// Re-checked because the lock was released for the round-trip above: the
	// first result to be published wins, rather than being overwritten.
	if th.state == drivers.TaskStateExited {
		th.stateLock.Unlock()

		return
	}

	th.state = drivers.TaskStateExited
	th.completedAt = time.Now()
	th.exitResult = exitResult
	th.closeWaitCh()
	th.stateLock.Unlock()

	th.logger.Info("task state changed", semconv.TaskStateChange(ost, drivers.TaskStateExited), semconv.UnitState(activeState))
}

// buildExitResult determines the exit result of a unit that has just stopped.
//
// It reports the process's real exit code or terminating signal where systemd
// knows it, and otherwise falls back to activeState alone: 0 for a clean stop, 1
// for a failed one.
func (th *Handler) buildExitResult(activeState systemd.UnitState) *drivers.ExitResult {
	if status, err := th.units.UnitExitStatus(th.ctx, th.Unit); err == nil {
		if result := systemd.ExitResultFromStatus(status); result != nil {
			return result
		}
	} else {
		th.logger.Debug("can't get unit exit status; falling back to the unit state", logx.Err(err))
	}

	if activeState.IsFailed() {
		return &drivers.ExitResult{ExitCode: 1, Err: errors.New("systemd unit failed")}
	}

	return &drivers.ExitResult{ExitCode: 0}
}
