// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins"

	"github.com/kirychuk/nomad-systemd-driver-plugin/plugin"
)

func main() {
	// The plugin protocol has no shutdown RPC, so the driver is torn down here
	// instead: Serve returns once Nomad closes the plugin connection, which it does
	// a couple of seconds before it force-kills the process. Nothing may depend on
	// this running - a killed process never reaches it.
	var driver *plugin.Driver

	plugins.Serve(func(log hclog.Logger) any {
		driver = plugin.New(log)

		return driver
	})

	if driver != nil {
		driver.Shutdown()
	}
}
