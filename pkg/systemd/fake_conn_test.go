package systemd

import (
	"context"
	"errors"

	"github.com/coreos/go-systemd/v22/dbus"
)

// fakeDbusConn is a test double for dbusConn. Each method delegates to an
// optional function field, so a test only wires up the calls its scenario
// actually exercises; anything left nil returns a clear "not configured"
// error instead of a nil-pointer panic.
type fakeDbusConn struct {
	startFunc             func(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	stopFunc              func(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	killFunc              func(ctx context.Context, name string, target dbus.Who, signal int32) error
	getPropertyFunc       func(ctx context.Context, unit, propertyName string) (*dbus.Property, error)
	getPropertiesFunc     func(ctx context.Context, unit string) (map[string]any, error)
	getTypePropertiesFunc func(ctx context.Context, unit, unitType string) (map[string]any, error)
	subscribeFunc         func() error

	connected bool
	closed    bool

	// subscribed/propUpdateCh/propErrCh record what SetPropertiesSubscriber
	// was called with, so tests can drive fake PropertiesChanged signals or
	// verify the subscriber was (re-)registered, e.g. after a reconnect.
	subscribed   bool
	propUpdateCh chan<- *dbus.PropertiesUpdate
	propErrCh    chan<- error
}

func (f *fakeDbusConn) StartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error) {
	if f.startFunc == nil {
		return 0, errors.New("fakeDbusConn: StartUnitContext not configured")
	}

	return f.startFunc(ctx, name, mode, ch)
}

func (f *fakeDbusConn) StopUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error) {
	if f.stopFunc == nil {
		return 0, errors.New("fakeDbusConn: StopUnitContext not configured")
	}

	return f.stopFunc(ctx, name, mode, ch)
}

func (f *fakeDbusConn) KillUnitWithTarget(ctx context.Context, name string, target dbus.Who, signal int32) error {
	if f.killFunc == nil {
		return errors.New("fakeDbusConn: KillUnitWithTarget not configured")
	}

	return f.killFunc(ctx, name, target, signal)
}

func (f *fakeDbusConn) GetUnitPropertyContext(ctx context.Context, unit, propertyName string) (*dbus.Property, error) {
	if f.getPropertyFunc == nil {
		return nil, errors.New("fakeDbusConn: GetUnitPropertyContext not configured")
	}

	return f.getPropertyFunc(ctx, unit, propertyName)
}

func (f *fakeDbusConn) GetUnitPropertiesContext(ctx context.Context, unit string) (map[string]any, error) {
	if f.getPropertiesFunc == nil {
		return nil, errors.New("fakeDbusConn: GetUnitPropertiesContext not configured")
	}

	return f.getPropertiesFunc(ctx, unit)
}

func (f *fakeDbusConn) GetUnitTypePropertiesContext(ctx context.Context, unit, unitType string) (map[string]any, error) {
	if f.getTypePropertiesFunc == nil {
		return nil, errors.New("fakeDbusConn: GetUnitTypePropertiesContext not configured")
	}

	return f.getTypePropertiesFunc(ctx, unit, unitType)
}

func (f *fakeDbusConn) Subscribe() error {
	f.subscribed = true

	if f.subscribeFunc == nil {
		return nil
	}

	return f.subscribeFunc()
}

func (f *fakeDbusConn) SetPropertiesSubscriber(updateCh chan<- *dbus.PropertiesUpdate, errCh chan<- error) {
	f.propUpdateCh = updateCh
	f.propErrCh = errCh
}

func (f *fakeDbusConn) Connected() bool { return f.connected }
func (f *fakeDbusConn) Close()          { f.closed = true }

var _ dbusConn = (*fakeDbusConn)(nil)
