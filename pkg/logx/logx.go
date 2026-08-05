package logx

import (
	"github.com/hashicorp/go-hclog"
)

// Logger writes structured records to an underlying [hclog.Logger].
//
// The zero Logger is not usable; obtain one from [New]. A Logger may be copied,
// and copies write to the same underlying hclog.Logger.
type Logger struct {
	h hclog.Logger
}

// New returns a Logger writing to h.
func New(h hclog.Logger) Logger {
	return Logger{h: h}
}

// Raw returns the underlying [hclog.Logger], for the Nomad and go-plugin APIs
// that accept only that type.
func (l Logger) Raw() hclog.Logger {
	return l.h
}

// Named returns a Logger writing to a sublogger of l's, with name appended to
// l's name.
func (l Logger) Named(name string) Logger {
	return Logger{h: l.h.Named(name)}
}

// With returns a Logger that adds attrs to every record it writes.
func (l Logger) With(attrs ...Attr) Logger {
	if len(attrs) == 0 {
		return l
	}

	return Logger{h: l.h.With(flatten(attrs)...)}
}

// Trace writes a record at trace level.
func (l Logger) Trace(msg string, attrs ...Attr) {
	l.h.Trace(msg, flatten(attrs)...)
}

// Debug writes a record at debug level.
func (l Logger) Debug(msg string, attrs ...Attr) {
	l.h.Debug(msg, flatten(attrs)...)
}

// Info writes a record at info level.
func (l Logger) Info(msg string, attrs ...Attr) {
	l.h.Info(msg, flatten(attrs)...)
}

// Warn writes a record at warn level.
func (l Logger) Warn(msg string, attrs ...Attr) {
	l.h.Warn(msg, flatten(attrs)...)
}

// Error writes a record at error level.
func (l Logger) Error(msg string, attrs ...Attr) {
	l.h.Error(msg, flatten(attrs)...)
}

// IsTrace reports whether trace records are written. Guard the construction of
// attributes that are expensive to build with it.
func (l Logger) IsTrace() bool {
	return l.h.IsTrace()
}

// IsDebug reports whether debug records are written. Guard the construction of
// attributes that are expensive to build with it.
func (l Logger) IsDebug() bool {
	return l.h.IsDebug()
}
