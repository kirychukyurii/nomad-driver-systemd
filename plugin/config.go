package plugin

import (
	"errors"
	"fmt"
	"regexp"
)

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

// unitNamePattern matches a syntactically valid systemd unit name: the character
// set systemd allows (see systemd.unit(5)), ending in a known unit type suffix.
//
// It applies regardless of allowed_units and denied_units, rejecting embedded
// whitespace, path traversal and control characters before they reach DBus.
var unitNamePattern = regexp.MustCompile(`^[a-zA-Z0-9:_.\\@-]+\.(service|socket|timer|mount|automount|swap|target|path|slice|scope|device)$`)

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
