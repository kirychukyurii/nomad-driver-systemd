// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/cpustats"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/hashicorp/nomad/plugins/shared/structs"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/systemd"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/task"
)

// Driver is the systemd driver plugin
type Driver struct {
	// This is not supported for systemd units
	drivers.DriverSignalTaskNotSupported
	drivers.DriverExecTaskNotSupported

	// eventer is used to send events to Nomad
	eventer *eventer.Eventer

	// config is the driver configuration
	config *Config

	// nomadConfig is the Nomad client configuration
	nomadConfig *base.ClientDriverConfig

	// compute contains information about the available cpu compute
	compute cpustats.Compute

	// tasks is the map of active task handlers
	tasks *task.Store

	// systemdMgr handles all systemd interactions
	systemdMgr *systemd.Manager

	// unitRefs tracks how many tasks are using each unit (for shared units)
	unitRefs     map[string]int
	unitRefsLock sync.Mutex

	// ctx is the context for the driver. It is passed to other subsystems to
	// coordinate shutdown
	ctx context.Context

	// signalShutdown is called when the driver is shutting down and cancels the
	// ctx passed to any subsystems
	signalShutdown context.CancelFunc

	// logger is the driver logger
	logger hclog.Logger
}

// New creates a new systemd driver
func New(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	return &Driver{
		eventer:        eventer.NewEventer(ctx, logger),
		tasks:          task.NewStore(),
		unitRefs:       make(map[string]int),
		ctx:            ctx,
		signalShutdown: cancel,
		logger:         logger.Named(pluginName),
	}
}

// PluginInfo returns information describing the plugin
func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

// ConfigSchema returns the schema for parsing the driver configuration
func (d *Driver) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

// SetConfig function is called when starting the plugin for the first time.
// The Config given has two different configuration fields. The first PluginConfig,
// is an encoded configuration from the plugin block of the client config.
// The second, AgentConfig, is the Nomad agent's configuration which is given to all plugins.
func (d *Driver) SetConfig(cfg *base.Config) error {
	var config Config
	if len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return fmt.Errorf("decode driver config: %w", err)
		}
	}

	d.config = &config
	if cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
	}

	d.compute = cfg.AgentConfig.Compute()
	systemdMgr, err := systemd.NewManager(d.ctx, d.compute, d.logger)
	if err != nil {
		return fmt.Errorf("create systemd manager: %w", err)
	}

	d.systemdMgr = systemdMgr
	d.systemdMgr.Start()

	return nil
}

// TaskConfigSchema returns the schema for parsing the task configuration
func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

// Capabilities returns the capabilities of the driver
func (d *Driver) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

// Fingerprint is called by the client when the plugin is started.
// It allows the driver to indicate its health to the client.
// The channel returned should immediately send an initial Fingerprint,
// then send periodic updates at an interval that is appropriate for the driver
// until the context is canceled.
func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)

	return ch, nil
}

func (d *Driver) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)
	ticker := time.NewTimer(0)

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(fingerprintPeriod)
			ch <- d.buildFingerprint()
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	fp := &drivers.Fingerprint{
		Attributes:        map[string]*structs.Attribute{},
		Health:            drivers.HealthStateHealthy,
		HealthDescription: drivers.DriverHealthy,
	}

	// Check if systemd manager has been initialized (SetConfig called)
	if d.systemdMgr == nil {
		// Plugin is still initializing, report as undetected
		fp.Health = drivers.HealthStateUndetected
		fp.HealthDescription = "waiting for driver initialization"
		return fp
	}

	// Check if systemd connection is valid
	if d.systemdMgr.Conn == nil || !d.systemdMgr.Conn.Connected() {
		fp.Health = drivers.HealthStateUnhealthy
		fp.HealthDescription = "systemd is not available"
		return fp
	}

	fp.Attributes["driver.systemd"] = structs.NewBoolAttribute(true)
	fp.Attributes["driver.systemd.version"] = structs.NewStringAttribute(pluginVersion)
	fp.Attributes["driver.systemd.logs"] = structs.NewBoolAttribute(true)
	fp.Attributes["driver.systemd.signals"] = structs.NewBoolAttribute(false)

	return fp
}

// RecoverTask detects running tasks when nomad client or task driver is restarted.
// When a driver is restarted it is not expected to persist any internal state to disk.
// To support this, Nomad will attempt to recover a task that was previously started
// if the driver does not recognize the task ID. During task recovery,
// Nomad calls RecoverTask passing the TaskHandle that was returned by the StartTask function.
func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	if _, ok := d.tasks.Get(handle.Config.ID); ok {
		d.logger.Info("nothing to recover; task already exists", "task", handle.Config.ID)

		return nil
	}

	var taskConfig TaskConfig
	if err := handle.GetDriverState(&taskConfig); err != nil {
		return fmt.Errorf("decode task config: %w", err)
	}

	d.logger.Info("recovering task", "task", handle.Config.ID, "unit", taskConfig.Unit)
	respCh := make(chan systemd.Message, 1)
	d.systemdMgr.CmdCh <- systemd.Command{
		Type:   systemd.CmdGetStatus,
		Unit:   taskConfig.Unit,
		RespCh: respCh,
	}

	resp := <-respCh
	if resp.Error != nil {
		return fmt.Errorf("failed to get unit status: %w", resp.Error)
	}

	activeState, ok := resp.Data.(systemd.UnitState)
	if !ok {
		return fmt.Errorf("invalid status response: expected SystemdUnitState, got %T", resp.Data)
	}

	taskState := systemd.ToTaskState(activeState)
	d.logger.Info("recovered task state", "task", handle.Config.ID, "unit", taskConfig.Unit, "systemd_state", activeState.String(), "task_state", taskState)

	if taskState == drivers.TaskStateUnknown {
		d.logger.Warn("recovered task in unknown/transitioning state, treating as running", "task", handle.Config.ID, "systemd_state", activeState.String())
		taskState = drivers.TaskStateRunning
	}

	eventCh := d.systemdMgr.RegisterUnit(taskConfig.Unit)
	taskHandler := task.NewHandler(handle.Config.ID, taskConfig.Unit, handle, d.systemdMgr.CommandChannel(), eventCh, taskState, d.logger)
	if taskState == drivers.TaskStateExited {
		var exitResult *drivers.ExitResult
		if activeState.IsFailed() {
			exitResult = &drivers.ExitResult{
				ExitCode: 1,
				Signal:   0,
				Err:      fmt.Errorf("systemd unit failed"),
			}
		} else {
			exitResult = &drivers.ExitResult{
				ExitCode: 0,
				Signal:   0,
			}
		}

		taskHandler.SetExitResult(exitResult, time.Now())
	}

	go d.streamLogsForTask(taskHandler)
	taskHandler.Start()
	d.tasks.Set(handle.Config.ID, taskHandler)

	// Increment unit reference count
	d.unitRefsLock.Lock()
	d.unitRefs[taskConfig.Unit]++
	refCount := d.unitRefs[taskConfig.Unit]
	d.unitRefsLock.Unlock()

	d.logger.Debug("incremented unit ref count", "unit", taskConfig.Unit, "count", refCount)

	return nil
}

// StartTask starts a new task by starting the specified systemd unit
func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if _, ok := d.tasks.Get(cfg.ID); ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}

	var taskConfig TaskConfig
	if err := cfg.DecodeDriverConfig(&taskConfig); err != nil {
		return nil, nil, fmt.Errorf("decode driver config: %w", err)
	}

	if err := taskConfig.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid task config: %w", err)
	}

	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg
	if err := handle.SetDriverState(&taskConfig); err != nil {
		d.logger.Error("encode task state", "error", err)
	}

	d.logger.Info("starting task", "task", cfg.ID, "unit", taskConfig.Unit)
	statusRespCh := make(chan systemd.Message, 1)
	d.systemdMgr.CmdCh <- systemd.Command{
		Type:   systemd.CmdGetStatus,
		Unit:   taskConfig.Unit,
		RespCh: statusRespCh,
	}

	statusResp := <-statusRespCh
	if statusResp.Error != nil {
		return nil, nil, fmt.Errorf("get unit status: %w", statusResp.Error)
	}

	activeState, ok := statusResp.Data.(systemd.UnitState)
	if !ok {
		return nil, nil, fmt.Errorf("invalid status response: expected SystemdUnitState, got %T", statusResp.Data)
	}

	if systemd.ToTaskState(activeState) != drivers.TaskStateRunning {
		startRespCh := make(chan systemd.Message, 1)
		d.systemdMgr.CmdCh <- systemd.Command{
			Type:   systemd.CmdStart,
			Unit:   taskConfig.Unit,
			RespCh: startRespCh,
		}

		startResp := <-startRespCh
		if startResp.Error != nil {
			return nil, nil, fmt.Errorf("start unit: %w", startResp.Error)
		}

		activeState = systemd.UnitStateActive
	}

	eventCh := d.systemdMgr.RegisterUnit(taskConfig.Unit)
	taskHandler := task.NewHandler(cfg.ID, taskConfig.Unit, handle, d.systemdMgr.CommandChannel(), eventCh, drivers.TaskStateRunning, d.logger)

	go d.streamLogsForTask(taskHandler)
	taskHandler.Start()
	d.tasks.Set(cfg.ID, taskHandler)

	// Increment unit reference count
	d.unitRefsLock.Lock()
	d.unitRefs[taskConfig.Unit]++
	refCount := d.unitRefs[taskConfig.Unit]
	d.unitRefsLock.Unlock()

	d.logger.Debug("incremented unit ref count", "unit", taskConfig.Unit, "count", refCount)

	d.eventer.EmitEvent(&drivers.TaskEvent{
		TaskID:    cfg.ID,
		AllocID:   cfg.AllocID,
		TaskName:  cfg.Name,
		Timestamp: time.Now(),
		Message:   fmt.Sprintf("Task started: systemd unit %s", taskConfig.Unit),
		Annotations: map[string]string{
			"unit":  taskConfig.Unit,
			"state": activeState.String(),
		},
	})

	return handle, nil, nil
}

// WaitTask function is expected to return a channel that will send an *ExitResult when the task
// exits or close the channel when the context is canceled. It is also expected that calling
// WaitTask on an exited task will immediately send an *ExitResult on the returned channel.
// A call to WaitTask after StopTask is valid and should be handled.
// If WaitTask is called after DestroyTask, it should return drivers.ErrTaskNotFound as no task state should exist after DestroyTask is called.
func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	ch := make(chan *drivers.ExitResult)
	taskHandler, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	go func() {
		defer close(ch)

		select {
		case <-ctx.Done():
			ch <- &drivers.ExitResult{Err: ctx.Err()}
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
func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	taskHandler, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	d.logger.Info("stopping task", "task", taskID, "unit", taskHandler.Unit, "timeout", timeout)
	if err := taskHandler.StopUnit(); err != nil {
		return fmt.Errorf("stop unit: %w", err)
	}

	return nil
}

// DestroyTask function cleans up and removes a task that has terminated.
// If force is set to true, the driver must destroy the task even if it is still running.
func (d *Driver) DestroyTask(taskID string, force bool) error {
	taskHandler, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	unit := taskHandler.Unit
	d.logger.Info("destroying task", "task", taskID, "unit", unit, "force", force)

	// Decrement unit reference count
	d.unitRefsLock.Lock()
	if d.unitRefs[unit] > 0 {
		d.unitRefs[unit]--
	}

	refCount := d.unitRefs[unit]
	if refCount == 0 {
		delete(d.unitRefs, unit)
	}

	d.unitRefsLock.Unlock()
	d.logger.Debug("decremented unit ref count", "unit", unit, "count", refCount)
	if taskHandler.IsRunning() && !force {
		return fmt.Errorf("cannot destroy running task")
	}

	// Only stop the systemd unit if no other tasks are using it
	if taskHandler.IsRunning() && refCount == 0 {
		d.logger.Info("stopping unit (last reference)", "unit", unit)

		if err := taskHandler.StopUnit(); err != nil {
			d.logger.Error("stop unit during destroy", "error", err)
		}

		select {
		case <-taskHandler.WaitCh():
			d.logger.Debug("task stopped", "task", taskID)
		case <-time.After(2 * time.Second):
			d.logger.Warn("timeout waiting for task cleanup", "task", taskID)
		}
	}

	if taskHandler.IsRunning() && refCount > 0 {
		d.logger.Info("not stopping unit, still in use by other tasks", "unit", unit, "ref_count", refCount)
	}

	taskHandler.Stop()
	d.systemdMgr.UnregisterUnit(unit)
	d.tasks.Delete(taskID)

	d.logger.Info("task destroyed", "task", taskID)

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

	d.logger.Info("create channel for task stats", "task", taskID, "unit", taskHandler.Unit, "interval", interval.String())
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

			d.sendTaskStats(ch, taskHandler, taskID)
		}
	}()

	return ch, nil
}

func (d *Driver) sendTaskStats(ch chan *drivers.TaskResourceUsage, taskHandler *task.Handler, taskID string) {
	stats, err := taskHandler.GetResourceStats()
	if err != nil {
		d.logger.Debug("get resource stats", "task_id", taskID, "error", err)
		stats = &systemd.ResourceStats{
			CPUStats:    &drivers.CpuStats{},
			MemoryStats: &drivers.MemoryStats{},
		}
	}

	ch <- &drivers.TaskResourceUsage{
		Timestamp: time.Now().UTC().UnixNano(),
		ResourceUsage: &drivers.ResourceUsage{
			CpuStats:    stats.CPUStats,
			MemoryStats: stats.MemoryStats,
		},
	}
}

// TaskEvents function allows the driver to publish driver specific events about tasks and
// the Nomad client publishes events associated with an allocation.
func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

// streamLogsForTask initiates log streaming from journald to the task handler
func (d *Driver) streamLogsForTask(taskHandler *task.Handler) {
	if err := d.systemdMgr.StreamLogs(taskHandler.Unit, taskHandler.LogCh); err != nil {
		d.logger.Error("start log streaming", "unit", taskHandler.Unit, "error", err)
	}
}
