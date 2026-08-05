// Package logx wraps [hclog.Logger] in an API that takes typed attributes
// instead of hclog's alternating key/value pairs.
//
// Records are written through a [Logger], and every attribute on them is built
// by one of the constructors in attr.go. Callers name attributes through the
// semconv subpackage rather than writing key strings at the call site.
package logx
