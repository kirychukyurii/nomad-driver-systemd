package logx

import (
	"fmt"
	"time"
)

// Attr is one attribute of a log record: a key together with a value already
// converted to a form hclog renders the same way in both its text and its JSON
// output.
//
// The zero Attr carries nothing and is dropped when the record is written, so a
// constructor with nothing to report - [Err] given a nil error - returns it.
type Attr struct {
	key   string
	value any

	// group holds the attributes of a [Multi]. When it is non-empty, key and
	// value are ignored.
	group []Attr
}

// String returns key with a string value.
func String(key, val string) Attr {
	return Attr{key: key, value: val}
}

// Strings returns key with a list of strings, which hclog renders as a JSON
// array or as a bracketed list depending on its output format.
func Strings(key string, val []string) Attr {
	return Attr{key: key, value: val}
}

// Int returns key with an int value.
func Int(key string, val int) Attr {
	return Attr{key: key, value: val}
}

// Int64 returns key with an int64 value.
func Int64(key string, val int64) Attr {
	return Attr{key: key, value: val}
}

// Uint64 returns key with a uint64 value.
func Uint64(key string, val uint64) Attr {
	return Attr{key: key, value: val}
}

// Float64 returns key with a float64 value.
func Float64(key string, val float64) Attr {
	return Attr{key: key, value: val}
}

// Bool returns key with a bool value.
func Bool(key string, val bool) Attr {
	return Attr{key: key, value: val}
}

// Duration returns key with val formatted the way [time.Duration.String] does,
// rather than as a count of nanoseconds.
func Duration(key string, val time.Duration) Attr {
	return Attr{key: key, value: val.String()}
}

// Time returns key with val formatted as RFC 3339.
func Time(key string, val time.Time) Attr {
	return Attr{key: key, value: val.Format(time.RFC3339)}
}

// Stringer returns key with the result of val.String(), or an empty string if
// val is nil.
//
// A non-nil interface holding a nil pointer is not nil, so String is called on
// it as usual.
func Stringer(key string, val fmt.Stringer) Attr {
	if val == nil {
		return Attr{key: key, value: ""}
	}

	return Attr{key: key, value: val.String()}
}

// Any returns key with val passed to hclog unconverted.
//
// hclog writes such a value as a nested object in its JSON output and through
// fmt's %v in its text output. A val that encoding/json cannot marshal costs
// the whole record: hclog drops the record and writes a warning in its place.
func Any(key string, val any) Attr {
	return Attr{key: key, value: val}
}

// Multi returns the attributes as a single Attr, for constructors that describe
// one thing with more than one key. Nesting is flattened when the record is
// written, and zero attrs are dropped.
func Multi(attrs ...Attr) Attr {
	return Attr{group: attrs}
}

// Err returns the attributes describing err: its message under "error.message"
// and its dynamic type under "error.type". A nil err yields the zero [Attr].
func Err(err error) Attr {
	return NamedErr("error", err)
}

// NamedErr is [Err] with key in place of the "error" prefix, for records that
// report more than one error.
func NamedErr(key string, err error) Attr {
	if err == nil {
		return Attr{}
	}

	return Multi(
		String(key+".message", err.Error()),
		String(key+".type", fmt.Sprintf("%T", err)),
	)
}

// flatten converts attrs into the alternating key/value slice hclog takes,
// returning nil rather than an empty slice when there is nothing to write.
func flatten(attrs []Attr) []any {
	if len(attrs) == 0 {
		return nil
	}

	return appendAttrs(make([]any, 0, len(attrs)*2), attrs)
}

// appendAttrs appends each attr's key and value to out, descending into groups
// and skipping zero attrs.
func appendAttrs(out []any, attrs []Attr) []any {
	for _, attr := range attrs {
		switch {
		case len(attr.group) > 0:
			out = appendAttrs(out, attr.group)
		case attr.key != "":
			out = append(out, attr.key, attr.value)
		}
	}

	return out
}
