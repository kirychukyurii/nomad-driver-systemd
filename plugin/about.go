package plugin

import (
	"errors"
	"fmt"
	"regexp"
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

// unitNamePattern matches a syntactically valid systemd unit name: the character
// set systemd allows (see systemd.unit(5)), ending in a known unit type suffix.
//
// It applies regardless of allowed_units and denied_units, rejecting embedded
// whitespace, path traversal and control characters before they reach DBus.
var unitNamePattern = regexp.MustCompile(`^[a-zA-Z0-9:_.\\@-]+\.(service|socket|timer|mount|automount|swap|target|path|slice|scope|device)$`)

type (
	// Config is the driver configuration set by the SetConfig RPC call
	Config struct {
		// AllowedUnits, if non-empty, is a list of regexes; a task's unit
		// must match at least one to be permitted (allowlist mode).
		AllowedUnits []string `codec:"allowed_units"`

		// DeniedUnits is a list of regexes; a task's unit matching any of
		// them is always rejected, even if it also matches AllowedUnits.
		DeniedUnits []string `codec:"denied_units"`

		// PprofAddr, if non-empty, is the address the pprof HTTP server listens
		// on, for example "127.0.0.1:6061". Empty disables pprof entirely.
		//
		// The profiles it serves expose the memory of a process that talks to
		// systemd over DBus, so the address should stay on loopback.
		PprofAddr string `codec:"pprof_addr"`
	}

	// TaskConfig is a task's driver configuration: which systemd unit the task
	// manages.
	TaskConfig struct {
		// Unit is the name of the systemd unit to manage, for example
		// "nginx.service". It is required and must be a syntactically valid
		// unit name; see [TaskConfig.Validate].
		//
		// The hcl tag carries no option suffix: a bare tag already means a
		// required attribute, and gohcl panics on an option kind it does not
		// know - only attr, block, label, optional and remain are valid.
		Unit string `hcl:"unit" codec:"unit"`
	}
)

// Validate reports whether the task configuration is usable, returning an error
// describing the first problem found.
//
// It checks only that Unit is a syntactically valid systemd unit name of at most
// 255 characters. Whether the unit is one this driver is permitted to manage is a
// separate question, decided by the driver's allowed_units and denied_units
// configuration when the task starts.
func (tc *TaskConfig) Validate() error {
	if tc.Unit == "" {
		return errors.New("unit name is required")
	}

	if len(tc.Unit) > 255 {
		return fmt.Errorf("unit name %q exceeds 255 characters", tc.Unit)
	}

	if !unitNamePattern.MatchString(tc.Unit) {
		return fmt.Errorf("unit name %q is not a valid systemd unit name (expected a name ending in .service, .socket, .timer, .mount, .automount, .swap, .target, .path, .slice, .scope, or .device)", tc.Unit)
	}

	return nil
}
