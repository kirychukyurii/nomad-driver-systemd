// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"log"
	"net/http"
	_ "net/http/pprof"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins"

	"github.com/kirychuk/nomad-systemd-driver-plugin/plugin"
)

func init() {
	go func() {
		log.Println("pprof listening on :6061")

		if err := http.ListenAndServe("127.0.0.1:6061", nil); err != nil {
			log.Println("pprof error", "error", err)

			return
		}
	}()
}

func main() {
	// Serve the plugin
	plugins.Serve(factory)
}

// factory returns a new instance of the systemd driver plugin
func factory(log hclog.Logger) interface{} {
	return plugin.New(log)
}
