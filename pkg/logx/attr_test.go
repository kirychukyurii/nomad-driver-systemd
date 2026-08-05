package logx

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

type stringerFunc string

func (s stringerFunc) String() string { return string(s) }

func TestConstructors(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 30, 0, 0, time.UTC)

	cases := []struct {
		name string
		attr Attr
		want map[string]any
	}{
		{
			name: "string",
			attr: String("k", "v"),
			want: map[string]any{"k": "v"},
		},
		{
			name: "empty string keeps the key",
			attr: String("k", ""),
			want: map[string]any{"k": ""},
		},
		{
			name: "strings",
			attr: Strings("k", []string{"a", "b"}),
			want: map[string]any{"k": []any{"a", "b"}},
		},
		{
			name: "int",
			attr: Int("k", -7),
			want: map[string]any{"k": json.Number("-7")},
		},
		{
			name: "int64",
			attr: Int64("k", 1<<40),
			want: map[string]any{"k": json.Number("1099511627776")},
		},
		{
			name: "uint64 keeps full precision",
			attr: Uint64("k", 18446744073709551615),
			want: map[string]any{"k": json.Number("18446744073709551615")},
		},
		{
			name: "float64",
			attr: Float64("k", 1.5),
			want: map[string]any{"k": json.Number("1.5")},
		},
		{
			name: "bool",
			attr: Bool("k", true),
			want: map[string]any{"k": true},
		},
		{
			// The point of normalizing at construction: hclog would otherwise
			// encode a time.Duration as its nanosecond count.
			name: "duration is formatted, not counted",
			attr: Duration("k", 5*time.Second),
			want: map[string]any{"k": "5s"},
		},
		{
			name: "sub-second duration",
			attr: Duration("k", 1500*time.Millisecond),
			want: map[string]any{"k": "1.5s"},
		},
		{
			name: "time",
			attr: Time("k", now),
			want: map[string]any{"k": "2026-08-05T12:30:00Z"},
		},
		{
			name: "stringer",
			attr: Stringer("k", stringerFunc("active")),
			want: map[string]any{"k": "active"},
		},
		{
			name: "nil stringer",
			attr: Stringer("k", nil),
			want: map[string]any{"k": ""},
		},
		{
			name: "any renders a nested object",
			attr: Any("k", map[string]int{"n": 1}),
			want: map[string]any{"k": map[string]any{"n": json.Number("1")}},
		},
		{
			name: "zero attr is dropped",
			attr: Attr{},
			want: map[string]any{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, buf := newTestLogger(hclog.Debug)
			l.Info("msg", tc.attr)

			if got := record(t, buf); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("attributes = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestErr(t *testing.T) {
	l, buf := newTestLogger(hclog.Debug)
	l.Error("failed", Err(fmt.Errorf("wrapped: %w", errors.New("root"))))

	want := map[string]any{
		"error.message": "wrapped: root",
		"error.type":    "*fmt.wrapError",
	}

	if got := record(t, buf); !reflect.DeepEqual(got, want) {
		t.Fatalf("attributes = %#v, want %#v", got, want)
	}
}

func TestErr_nilIsDropped(t *testing.T) {
	l, buf := newTestLogger(hclog.Debug)
	l.Info("fine", String("k", "v"), Err(nil))

	want := map[string]any{"k": "v"}

	if got := record(t, buf); !reflect.DeepEqual(got, want) {
		t.Fatalf("attributes = %#v, want %#v", got, want)
	}
}

func TestNamedErr(t *testing.T) {
	l, buf := newTestLogger(hclog.Debug)
	l.Error("both failed", Err(errors.New("stop")), NamedErr("kill_error", errors.New("kill")))

	want := map[string]any{
		"error.message":      "stop",
		"error.type":         "*errors.errorString",
		"kill_error.message": "kill",
		"kill_error.type":    "*errors.errorString",
	}

	if got := record(t, buf); !reflect.DeepEqual(got, want) {
		t.Fatalf("attributes = %#v, want %#v", got, want)
	}
}

func TestMulti_flattensAndSkipsZeroAttrs(t *testing.T) {
	l, buf := newTestLogger(hclog.Debug)
	l.Info("msg", Multi(
		String("a", "1"),
		Multi(String("b", "2"), Attr{}),
		Err(nil),
	))

	want := map[string]any{"a": "1", "b": "2"}

	if got := record(t, buf); !reflect.DeepEqual(got, want) {
		t.Fatalf("attributes = %#v, want %#v", got, want)
	}
}

func TestMulti_emptyWritesNothing(t *testing.T) {
	l, buf := newTestLogger(hclog.Debug)
	l.Info("msg", Multi())

	if got := record(t, buf); len(got) != 0 {
		t.Fatalf("attributes = %#v, want none", got)
	}
}
