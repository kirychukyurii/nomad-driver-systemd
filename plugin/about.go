package plugin

import (
	"fmt"
	"time"

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

	// fingerprintPeriod is the interval at which the driver will send fingerprint responses
	fingerprintPeriod = 30 * time.Second
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
	// configSpec is the HCL specification for the driver configuration
	// Currently empty as this driver doesn't require driver-level configuration
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{})

	// taskConfigSpec is the HCL specification for per-task configuration
	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"unit": hclspec.NewAttr("unit", "string", true),
	})
)

type (
	// Config is the driver configuration set by the SetConfig RPC call
	// For the systemd driver, we don't need any driver-level configuration
	Config struct{}

	// TaskConfig is the per-task configuration for the systemd driver
	// It specifies which systemd unit should be managed
	TaskConfig struct {
		// Unit is the name of the systemd unit to manage
		// This is a required field and must be a valid systemd unit name
		// Examples: "nginx.service", "redis.service", "myapp.service"
		Unit string `hcl:"unit,optional" codec:"unit"`
	}
)

// Validate validates the task configuration
// It ensures that the required Unit field is provided
func (tc *TaskConfig) Validate() error {
	if tc.Unit == "" {
		return fmt.Errorf("unit name is required")
	}
	return nil
}
