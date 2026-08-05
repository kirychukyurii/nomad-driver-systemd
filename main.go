// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins"

	"github.com/kirychuk/nomad-systemd-driver-plugin/plugin"
)

func main() {
	plugins.Serve(factory)
}

// factory builds the driver instance Nomad's plugin server serves.
func factory(log hclog.Logger) any {
	return plugin.New(log)
}
