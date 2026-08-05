// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins"

	"github.com/kirychuk/nomad-systemd-driver-plugin/plugin"
)

func main() {
	plugins.Serve(func(log hclog.Logger) any {
		return plugin.New(log)
	})
}
