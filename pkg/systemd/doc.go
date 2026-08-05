// Package systemd controls systemd units over DBus on behalf of a Nomad task
// driver.
//
// A [Manager] owns one connection to the system bus and exposes the operations a
// driver needs for a unit it manages: starting and stopping it, reading its
// state and exit status, sampling its resource usage, and streaming its journal.
// Typical use is to create a Manager with [NewManager], call
// [Manager.Start] to run its background loops, then [Manager.RegisterUnit] for
// each unit before operating on it.
//
// Every operation takes a context and is bounded by the earliest of that
// context, the Manager's own shutdown, and a budget this package assigns to the
// operation - property lookups are held to seconds, while start and stop jobs
// are allowed the minutes a unit's own startup or TimeoutStopSec may take.
// Callers therefore need no timeouts of their own, though a caller that passes a
// shorter deadline still gets it honored.
//
// Two host requirements are worth noting. Resource statistics are read from the
// unified (v2) cgroup hierarchy only; on a cgroup v1 or hybrid host units still
// start and stop normally but statistics read as zeros, which
// [Manager.CgroupV2Available] detects up front. Journal streaming links against
// libsystemd, so this package requires cgo.
package systemd
