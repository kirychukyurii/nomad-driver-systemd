// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package plugin implements a Nomad task driver that runs tasks as systemd units.
//
// [New] returns the driver Nomad loads over its plugin protocol. Each task names
// an existing unit on the host, which the driver starts, monitors and stops on
// the task's behalf. Because units live outside any allocation, the driver
// enforces two rules of its own: a unit may be managed by at most one task at a
// time, and which units are eligible at all can be restricted by the
// allowed_units and denied_units regex lists in the driver's configuration.
// Without those lists any job submitter can take over arbitrary host units, as
// this driver provides no isolation of its own.
//
// The driver provides no filesystem or network isolation, cannot send signals to
// tasks, and cannot exec into them. Resource statistics require a host on the
// unified (v2) cgroup hierarchy; the driver's fingerprint reports whether that
// is the case.
package plugin
