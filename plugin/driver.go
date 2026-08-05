// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/cpustats"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/systemd"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/task"
)

// Driver is the systemd task driver.
//
// The zero value is not usable: create one with [New]. Nomad calls SetConfig
// before any task operation, and every method may be called concurrently from
// Nomad's plugin server, including concurrently with SetConfig.
//
// Contexts passed to systemd operations carry no deadlines of their own: the
// systemd package bounds each call according to what the operation is.
type Driver struct {
	// This is not supported for systemd units
	drivers.DriverSignalTaskNotSupported
	drivers.DriverExecTaskNotSupported

	// eventer is used to send events to Nomad
	eventer *eventer.Eventer

	// compute contains information about the available cpu compute
	compute cpustats.Compute

	// tasks is the map of active task handlers
	tasks *task.Store

	// systemdMgr performs all systemd interactions. Atomic because SetConfig
	// writes it while task RPCs and the fingerprint loop read it, with no
	// ordering guaranteed between them.
	systemdMgr atomic.Pointer[systemd.Manager]

	// unitPolicy is the compiled allowed_units/denied_units policy. Atomic for
	// the same reason as systemdMgr.
	unitPolicy atomic.Pointer[unitPolicy]

	// unitOwners maps a unit name to the ID of the task managing it. At most one
	// task may manage a unit at a time.
	unitOwners     map[string]string
	unitOwnersLock sync.Mutex

	// pprof is the running pprof server, or nil if pprof is disabled. Guarded by
	// pprofLock, which also serializes the compare-and-restart that a config
	// reload performs.
	pprof     *pprofServer
	pprofLock sync.Mutex

	// ctx bounds the driver's lifetime and everything it starts.
	//nolint:containedctx // @kirychukyurii: Nomad's DriverPlugin methods take no ctx, so the driver has to hold its own.
	ctx context.Context

	// signalShutdown cancels ctx.
	signalShutdown context.CancelFunc

	// logger is the driver logger
	logger logx.Logger
}

// New returns a systemd task driver ready to be served to Nomad.
//
// The driver cannot run tasks until Nomad calls SetConfig, which establishes the
// connection to systemd; until then it fingerprints itself as undetected.
func New(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())

	return &Driver{
		eventer:        eventer.NewEventer(ctx, logger),
		tasks:          task.NewStore(),
		unitOwners:     make(map[string]string),
		ctx:            ctx,
		signalShutdown: cancel,
		logger:         logx.New(logger).Named(pluginName),
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

	if cfg.AgentConfig == nil {
		return errors.New("nomad agent config is required")
	}

	d.compute = cfg.AgentConfig.Compute()

	policy, err := compileUnitPolicy(config.AllowedUnits, config.DeniedUnits)
	if err != nil {
		return fmt.Errorf("invalid unit policy configuration: %w", err)
	}

	d.unitPolicy.Store(policy)

	if err := d.configurePprof(config.PprofAddr); err != nil {
		return fmt.Errorf("configure pprof: %w", err)
	}

	systemdMgr, err := systemd.NewManager(d.ctx, d.compute, d.logger)
	if err != nil {
		return fmt.Errorf("create systemd manager: %w", err)
	}

	systemdMgr.Start()

	// SetConfig may be called again on a config reload; the previous manager must
	// be stopped or its goroutines and connection outlive it.
	if old := d.systemdMgr.Swap(systemdMgr); old != nil {
		old.Stop()
	}

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

// getManager returns the systemd manager, or an error if SetConfig has not
// established one yet.
//
// Only the operations that are about the manager itself go through it - creating,
// registering and unregistering units, and reporting health. Everything about an
// already-managed unit goes through its [task.Handler], which cannot exist
// without a manager and so needs no such check.
func (d *Driver) getManager() (*systemd.Manager, error) {
	mgr := d.systemdMgr.Load()
	if mgr == nil {
		return nil, errors.New("systemd manager is not initialized; SetConfig has not run")
	}

	return mgr, nil
}
