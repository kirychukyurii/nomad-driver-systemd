package systemd

import (
	"context"

	"github.com/coreos/go-systemd/v22/dbus"
)

// dbusConn is the subset of *dbus.Conn a Manager depends on. It is an interface
// so a Manager can be driven against a fake connection.
type dbusConn interface {
	StartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	StopUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	KillUnitWithTarget(ctx context.Context, name string, target dbus.Who, signal int32) error
	GetUnitPropertyContext(ctx context.Context, unit, propertyName string) (*dbus.Property, error)
	GetUnitPropertiesContext(ctx context.Context, unit string) (map[string]any, error)
	GetUnitTypePropertiesContext(ctx context.Context, unit, unitType string) (map[string]any, error)

	// Subscribe tells systemd to start emitting unit change signals to this
	// connection. It is required once per connection, and so again after every
	// reconnect, before SetPropertiesSubscriber receives anything.
	Subscribe() error

	// SetPropertiesSubscriber registers a channel receiving every unit's
	// property changes. Delivery is a non-blocking send, so updates are dropped
	// under backpressure and must never be treated as the sole source of truth.
	SetPropertiesSubscriber(updateCh chan<- *dbus.PropertiesUpdate, errCh chan<- error)

	Connected() bool
	Close()
}

// Compile-time assertion that the real client satisfies dbusConn.
var _ dbusConn = (*dbus.Conn)(nil)
