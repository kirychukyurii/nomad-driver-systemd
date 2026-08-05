# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

log_level = "TRACE"

plugin "nomad-driver-systemd" {
  config {
    # No driver-level configuration is required.

    # Serve net/http/pprof on this address. Empty or unset disables it.
    # The profiles expose the memory of a process that talks to systemd over
    # DBus, so keep the address on loopback.
    # pprof_addr = "127.0.0.1:6061"
  }
}
