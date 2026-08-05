// Package semconv names every log attribute this plugin emits.
//
// Each function returns one [logx.Attr] under a fixed key, so renaming an
// attribute is a change here and nowhere else. Keys are flat, dot-separated
// namespaces in the manner of OpenTelemetry's semantic conventions; the dots are
// naming, not nesting.
//
// This package must not import the plugin's own packages, which log through it
// and would form an import cycle. Attributes carrying a plugin type therefore
// take an interface or a plain string.
package semconv
