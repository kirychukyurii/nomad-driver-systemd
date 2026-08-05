package plugin

import (
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/drivers/fsisolation"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
)

const (
	// pluginName is the name of the plugin as it will be known in Nomad
	pluginName = "systemd"

	// pluginVersion is the current version of the plugin
	pluginVersion = "v0.1.0"

	// taskHandleVersion is the version of the task handle encoding
	taskHandleVersion = 1
)

var (
	// pluginInfo is the response returned for the PluginInfo RPC
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     pluginVersion,
		Name:              pluginName,
	}

	// capabilities is returned by the Capabilities RPC and indicates what
	// optional features this driver supports
	capabilities = &drivers.Capabilities{
		SendSignals: false,
		Exec:        false,
		FSIsolation: fsisolation.None,
		NetIsolationModes: []drivers.NetIsolationMode{
			drivers.NetIsolationModeHost,
		},
		MustInitiateNetwork: false,
		MountConfigs:        drivers.MountConfigSupportNone,
	}
)

var (
	// configSpec is the HCL specification for the driver configuration.
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"allowed_units": hclspec.NewAttr("allowed_units", "list(string)", false),
		"denied_units":  hclspec.NewAttr("denied_units", "list(string)", false),
		"pprof_addr":    hclspec.NewAttr("pprof_addr", "string", false),
	})

	// taskConfigSpec is the HCL specification for per-task configuration
	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"unit": hclspec.NewAttr("unit", "string", true),
	})
)
