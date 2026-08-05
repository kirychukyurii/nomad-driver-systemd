// Package task tracks the lifecycle of a single Nomad task backed by a systemd
// unit.
//
// A [Handler] watches one unit, translates its systemd state into the task state
// Nomad expects, resolves the task's exit result once the unit stops, and copies
// the unit's journal into the task's stdout and stderr. A [Store] holds the
// handlers a driver currently manages, keyed by task ID.
package task
