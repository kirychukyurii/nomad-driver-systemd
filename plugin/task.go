// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx/semconv"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/systemd"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/task"
)

// destroyStopGracePeriod is how long DestroyTask waits for a unit to stop cleanly
// before force-killing it.
//
// It applies only to a forced destroy of a task that has not finished; Nomad
// supplies its own grace period for an ordinary stop.
const destroyStopGracePeriod = 5 * time.Second

// RecoverTask detects running tasks when nomad client or task driver is restarted.
// When a driver is restarted it is not expected to persist any internal state to disk.
// To support this, Nomad will attempt to recover a task that was previously started
// if the driver does not recognize the task ID. During task recovery,
// Nomad calls RecoverTask passing the TaskHandle that was returned by the StartTask function.
func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	if _, ok := d.tasks.Get(handle.Config.ID); ok {
		d.logger.Info("nothing to recover; task already exists", semconv.TaskID(handle.Config.ID))

		return nil
	}

	var taskConfig TaskConfig
	if err := handle.GetDriverState(&taskConfig); err != nil {
		return fmt.Errorf("decode task config: %w", err)
	}

	mgr, err := d.getManager()
	if err != nil {
		return err
	}

	if err := d.claimUnit(taskConfig.Unit, handle.Config.ID); err != nil {
		return err
	}

	logger := d.logger.With(semconv.TaskID(handle.Config.ID), semconv.Unit(taskConfig.Unit))
	logger.Info("recovering task")

	activeState, err := mgr.UnitState(d.ctx, taskConfig.Unit)
	if err != nil {
		d.releaseUnit(taskConfig.Unit)

		return fmt.Errorf("get unit status: %w", err)
	}

	taskState := systemd.ToTaskState(activeState)
	logger.Info("recovered task state", semconv.UnitState(activeState), semconv.TaskState(taskState))

	if taskState == drivers.TaskStateUnknown {
		logger.Warn("recovered task in unknown/transitioning state, treating as running", semconv.UnitState(activeState))

		taskState = drivers.TaskStateRunning
	}

	mgr.RegisterUnit(d.ctx, taskConfig.Unit)
	taskHandler := task.NewHandler(handle.Config.ID, taskConfig.Unit, handle, mgr, mgr.WakeChannel(taskConfig.Unit), taskState, d.logger)

	startedAt, err := mgr.UnitStartTime(d.ctx, taskConfig.Unit)
	if err == nil {
		taskHandler.SetStartedAt(startedAt)
	} else {
		logger.Debug("can't determine unit start time; defaulting to recovery time", logx.Err(err))
	}

	if taskState == drivers.TaskStateExited {
		var exitResult *drivers.ExitResult

		status, err := mgr.UnitExitStatus(d.ctx, taskConfig.Unit)
		if err == nil {
			exitResult = systemd.ExitResultFromStatus(status)
		}

		if exitResult == nil {
			if activeState.IsFailed() {
				exitResult = &drivers.ExitResult{ExitCode: 1, Err: errors.New("systemd unit failed")}
			} else {
				exitResult = &drivers.ExitResult{ExitCode: 0}
			}
		}

		taskHandler.SetExitResult(exitResult, time.Now())
	}

	taskHandler.Start()
	d.tasks.Set(handle.Config.ID, taskHandler)

	return nil
}

// StartTask starts a new task by starting the specified systemd unit
func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if _, ok := d.tasks.Get(cfg.ID); ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}

	var taskConfig TaskConfig
	if err := cfg.DecodeDriverConfig(&taskConfig); err != nil {
		return nil, nil, fmt.Errorf("decode task config: %w", err)
	}

	if err := taskConfig.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid task config: %w", err)
	}

	// New tasks only, not recovered ones: tightening the policy must not orphan a
	// unit already under management.
	if policy := d.unitPolicy.Load(); policy != nil {
		if err := policy.check(taskConfig.Unit); err != nil {
			return nil, nil, fmt.Errorf("unit not permitted: %w", err)
		}
	}

	mgr, err := d.getManager()
	if err != nil {
		return nil, nil, err
	}

	if err := d.claimUnit(taskConfig.Unit, cfg.ID); err != nil {
		return nil, nil, err
	}

	handle := drivers.NewTaskHandle(taskHandleVersion)

	handle.Config = cfg
	if err := handle.SetDriverState(&taskConfig); err != nil {
		d.releaseUnit(taskConfig.Unit)

		return nil, nil, fmt.Errorf("encode task state: %w", err)
	}

	d.logger.Info("starting task", semconv.TaskID(cfg.ID), semconv.Unit(taskConfig.Unit))

	activeState, err := mgr.UnitState(d.ctx, taskConfig.Unit)
	if err != nil {
		d.releaseUnit(taskConfig.Unit)

		return nil, nil, fmt.Errorf("get unit status: %w", err)
	}

	if systemd.ToTaskState(activeState) != drivers.TaskStateRunning {
		if err := mgr.StartUnit(d.ctx, taskConfig.Unit); err != nil {
			d.rollBackFailedStart(mgr, taskConfig.Unit)
			d.releaseUnit(taskConfig.Unit)

			return nil, nil, fmt.Errorf("start unit: %w", err)
		}

		activeState = systemd.UnitStateActive
	}

	mgr.RegisterUnit(d.ctx, taskConfig.Unit)
	taskHandler := task.NewHandler(cfg.ID, taskConfig.Unit, handle, mgr, mgr.WakeChannel(taskConfig.Unit), drivers.TaskStateRunning, d.logger)

	taskHandler.Start()
	d.tasks.Set(cfg.ID, taskHandler)

	if err := d.eventer.EmitEvent(&drivers.TaskEvent{
		TaskID:    cfg.ID,
		AllocID:   cfg.AllocID,
		TaskName:  cfg.Name,
		Timestamp: time.Now(),
		Message:   fmt.Sprintf("Task started: systemd unit %s", taskConfig.Unit),
		Annotations: map[string]string{
			"unit":  taskConfig.Unit,
			"state": activeState.String(),
		},
	}); err != nil {
		d.logger.Warn("can't emit task started event", semconv.TaskID(cfg.ID), logx.Err(err))
	}

	return handle, nil, nil
}

// WaitTask function is expected to return a channel that will send an *ExitResult when the task
// exits or close the channel when the context is canceled. It is also expected that calling
// WaitTask on an exited task will immediately send an *ExitResult on the returned channel.
// A call to WaitTask after StopTask is valid and should be handled.
// If WaitTask is called after DestroyTask, it should return drivers.ErrTaskNotFound as no task state should exist after DestroyTask is called.
func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	// Buffered so the goroutine below can always deliver without blocking on a
	// reader that may have stopped listening.
	ch := make(chan *drivers.ExitResult, 1)

	taskHandler, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	go func() {
		defer close(ch)

		select {
		case <-ctx.Done():
			return

		case <-taskHandler.WaitCh():
			status := taskHandler.TaskStatus()
			if status.ExitResult != nil {
				ch <- status.ExitResult

				return
			}

			ch <- &drivers.ExitResult{
				ExitCode: 0,
				Signal:   0,
			}
		}
	}()

	return ch, nil
}

// StopTask function is expected to stop a running task by sending the given signal to it.
// If the task does not stop during the given timeout, the driver must forcefully kill the task.
// StopTask does not clean up resources of the task or remove it from the driver's internal state.
//
//nolint:revive // @kirychukyurii: signal is unused - systemd decides how to stop a unit via its own KillSignal/KillMode; the name is kept to match Nomad's DriverPlugin signature.
func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	taskHandler, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	logger := d.logger.With(semconv.TaskID(taskID), semconv.Unit(taskHandler.Unit))
	logger.Info("stopping task", semconv.Timeout(timeout))

	// Concurrent so that Nomad's kill_timeout keeps ticking independently: a stop
	// job may take as long as the unit's TimeoutStopSec.
	stopDone := make(chan error, 1)

	go func() { stopDone <- taskHandler.StopUnit() }()

	// No grace period at all: escalate immediately.
	if timeout <= 0 {
		return d.forceKillTask(taskHandler)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-taskHandler.WaitCh():
		return nil

	case err := <-stopDone:
		if err == nil {
			// The unit is down; the handler will observe it shortly.
			return nil
		}

		logger.Warn("graceful stop, escalating to kill", logx.Err(err))

		return d.forceKillTask(taskHandler)

	case <-timer.C:
		logger.Warn("graceful stop timed out, escalating to kill", semconv.Timeout(timeout))

		return d.forceKillTask(taskHandler)
	}
}

// DestroyTask function cleans up and removes a task that has terminated.
// If force is set to true, the driver must destroy the task even if it is still running.
func (d *Driver) DestroyTask(taskID string, force bool) error {
	taskHandler, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	unit := taskHandler.Unit
	logger := d.logger.With(semconv.TaskID(taskID), semconv.Unit(unit))
	logger.Info("destroying task", semconv.DestroyForce(force))

	// Both checks below ask whether the task has terminated, not whether it is
	// running: a unit in a transitional state is neither (see Handler.HasExited).
	//
	// The guard must precede any state mutation, so that a rejected destroy
	// leaves the task exactly as it was and Nomad can retry it with force.
	if !taskHandler.HasExited() && !force {
		return fmt.Errorf("cannot destroy task in state %q", taskHandler.TaskStatus().State)
	}

	if !taskHandler.HasExited() {
		logger.Info("stopping unit")

		// Concurrent for the same reason as in StopTask.
		stopDone := make(chan error, 1)

		go func() { stopDone <- taskHandler.StopUnit() }()

		select {
		case <-taskHandler.WaitCh():
			logger.Debug("task stopped")

		case err := <-stopDone:
			if err != nil {
				logger.Warn("stop unit during destroy, escalating to kill", logx.Err(err))

				if killErr := d.forceKillTask(taskHandler); killErr != nil {
					logger.Error("kill unit during destroy", logx.Err(killErr))
				}
			}

		case <-time.After(destroyStopGracePeriod):
			logger.Warn("task cleanup timed out, escalating to kill")

			if killErr := d.forceKillTask(taskHandler); killErr != nil {
				logger.Error("kill unit during destroy", logx.Err(killErr))
			}
		}
	}

	taskHandler.Stop()

	if mgr, err := d.getManager(); err != nil {
		// Unreachable: a tracked task implies a manager, which is never unset.
		logger.Error("unregister unit: systemd manager unavailable", logx.Err(err))
	} else {
		mgr.UnregisterUnit(unit)
	}

	d.tasks.Delete(taskID)
	d.releaseUnit(unit)

	logger.Info("task destroyed")

	return nil
}

// InspectTask function returns detailed status information for the referenced taskID.
func (d *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	taskHandler, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return taskHandler.TaskStatus(), nil
}

// TaskStats function returns a channel which the driver should send stats to at the given interval.
// The driver must send stats at the given interval until the given context is canceled or the task terminates.
// Retrieves CPU and memory usage from systemd cgroup accounting.
func (d *Driver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	taskHandler, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	d.logger.Info("create channel for task stats",
		semconv.TaskID(taskID), semconv.Unit(taskHandler.Unit), logx.Duration("interval", interval))

	ch := make(chan *drivers.TaskResourceUsage)

	go func() {
		defer close(ch)

		timer := time.NewTimer(0)
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-taskHandler.WaitCh():
				return
			case <-timer.C:
				timer.Reset(interval)
			}

			if !d.sendTaskStats(ctx, ch, taskHandler, taskID) {
				return
			}
		}
	}()

	return ch, nil
}

// TaskEvents function allows the driver to publish driver specific events about tasks and
// the Nomad client publishes events associated with an allocation.
func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

// rollBackFailedStart makes a best-effort attempt to stop a unit whose start
// this driver gave up waiting for, and logs at error level if it cannot.
//
// A failed start does not mean the unit stays down: systemd keeps the enqueued
// job, so the unit can come up minutes after StartTask reported failure and
// released its ownership, leaving it running with nothing managing it. This
// closes that window where it can, and makes the orphan discoverable where it
// cannot.
func (d *Driver) rollBackFailedStart(mgr *systemd.Manager, unit string) {
	if err := mgr.StopUnit(d.ctx, unit); err != nil {
		d.logger.Error("can't roll back unit start; the unit may come up with no task managing it",
			semconv.Unit(unit), logx.Err(err))
	}
}

// forceKillTask force-kills the task's unit, reporting success if the unit has
// already reached a terminal state.
//
// By the time a kill escalation fires the unit may have finished stopping on its
// own, which must not surface to Nomad as a stop failure.
func (d *Driver) forceKillTask(taskHandler *task.Handler) error {
	killErr := taskHandler.KillUnit()
	if killErr == nil {
		return nil
	}

	// A kill can fail simply because there is nothing left to kill.
	if state, err := taskHandler.UnitState(); err == nil && state.IsTerminal() {
		return nil
	}

	return fmt.Errorf("kill unit: %w", killErr)
}

// sendTaskStats samples the task's resource usage and sends it on ch, giving up
// on both if ctx ends first. It reports whether the stats stream should continue.
//
// A sample that could not be read is sent as zeros rather than skipped, so the
// stream Nomad consumes stays regular. The one error it does not degrade past is
// the unit no longer being managed: there is nothing left to measure, so the
// stream ends instead of reporting zeros indefinitely.
func (d *Driver) sendTaskStats(ctx context.Context, ch chan *drivers.TaskResourceUsage, taskHandler *task.Handler, taskID string) bool {
	// The caller's ctx, not d.ctx: this read must also end when Nomad stops
	// consuming the stats stream.
	stats, err := taskHandler.ResourceStats(ctx)
	if err != nil {
		if errors.Is(err, systemd.ErrUnitNotRegistered) {
			d.logger.Debug("ending stats stream, unit is no longer managed",
				semconv.TaskID(taskID), semconv.Unit(taskHandler.Unit))

			return false
		}

		d.logger.Debug("get resource stats", semconv.TaskID(taskID), logx.Err(err))

		stats = systemd.EmptyResourceStats()
	}

	usage := &drivers.TaskResourceUsage{
		Timestamp: time.Now().UTC().UnixNano(),
		ResourceUsage: &drivers.ResourceUsage{
			CpuStats:    stats.CPUStats,
			MemoryStats: stats.MemoryStats,
		},
	}

	// Guarded: Nomad may have stopped consuming by the time the sample is ready.
	select {
	case ch <- usage:
	case <-ctx.Done():
	}

	return true
}
