package logx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

// newTestLogger returns a Logger writing JSON records into the returned buffer,
// which is the format go-plugin gives a plugin in production.
func newTestLogger(level hclog.Level) (Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := hclog.New(&hclog.LoggerOptions{
		Output:      buf,
		Level:       level,
		JSONFormat:  true,
		DisableTime: true,
	})

	return New(h), buf
}

// record decodes the single JSON record written to buf, with numbers left as
// [json.Number] so the test compares what hclog encoded rather than what a
// float64 round trip produces. The level and message keys are removed, leaving
// only the record's attributes.
func record(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	dec.UseNumber()

	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode record %q: %v", buf.String(), err)
	}

	if err := dec.Decode(new(map[string]any)); !errors.Is(err, io.EOF) {
		t.Fatalf("want exactly one record, got %q", buf.String())
	}

	delete(got, "@level")
	delete(got, "@message")

	return got
}

func TestWith(t *testing.T) {
	l, buf := newTestLogger(hclog.Debug)
	l.With(String("bound", "yes")).Info("msg", String("call", "site"))

	want := map[string]any{"bound": "yes", "call": "site"}

	if got := record(t, buf); !reflect.DeepEqual(got, want) {
		t.Fatalf("attributes = %#v, want %#v", got, want)
	}
}

func TestWith_noAttrsReturnsSameLogger(t *testing.T) {
	l, _ := newTestLogger(hclog.Debug)

	if got := l.With(); got.Raw() != l.Raw() {
		t.Fatal("With() with no attributes built a new logger")
	}
}

func TestWith_accumulates(t *testing.T) {
	l, buf := newTestLogger(hclog.Debug)
	l.With(String("a", "1")).With(String("b", "2")).Info("msg")

	want := map[string]any{"a": "1", "b": "2"}

	if got := record(t, buf); !reflect.DeepEqual(got, want) {
		t.Fatalf("attributes = %#v, want %#v", got, want)
	}
}

func TestNamed(t *testing.T) {
	l, buf := newTestLogger(hclog.Debug)
	l.Named("child").Info("msg")

	if got := record(t, buf)["@module"]; got != "child" {
		t.Fatalf("@module = %v, want %q", got, "child")
	}
}

func TestLevels(t *testing.T) {
	cases := []struct {
		name  string
		write func(Logger)
		want  string
	}{
		{name: "trace", write: func(l Logger) { l.Trace("m") }, want: "trace"},
		{name: "debug", write: func(l Logger) { l.Debug("m") }, want: "debug"},
		{name: "info", write: func(l Logger) { l.Info("m") }, want: "info"},
		{name: "warn", write: func(l Logger) { l.Warn("m") }, want: "warn"},
		{name: "error", write: func(l Logger) { l.Error("m") }, want: "error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, buf := newTestLogger(hclog.Trace)
			tc.write(l)

			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("decode record %q: %v", buf.String(), err)
			}

			if got["@level"] != tc.want {
				t.Fatalf("@level = %v, want %q", got["@level"], tc.want)
			}
		})
	}
}

func TestLevelFiltering(t *testing.T) {
	l, buf := newTestLogger(hclog.Warn)

	l.Debug("dropped")
	l.Info("dropped")

	if buf.Len() != 0 {
		t.Fatalf("records below the level were written: %q", buf.String())
	}

	if l.IsDebug() || l.IsTrace() {
		t.Fatal("IsDebug/IsTrace reported true at warn level")
	}

	l.Warn("kept")

	if buf.Len() == 0 {
		t.Fatal("record at the level was dropped")
	}
}

// TestTextOutput covers the format used when the plugin binary is run outside
// go-plugin, where hclog formats values through fmt rather than encoding/json.
func TestTextOutput(t *testing.T) {
	buf := &bytes.Buffer{}
	l := New(hclog.New(&hclog.LoggerOptions{
		Output:      buf,
		Level:       hclog.Debug,
		DisableTime: true,
	}))

	l.Info("msg", Duration("timeout", 5*time.Second), Err(errors.New("boom")))

	// hclog quotes a value holding any rune outside "-" through "~", which the
	// leading "*" of a pointer type name is.
	got := buf.String()
	for _, want := range []string{"timeout=5s", "error.message=boom", `error.type="*errors.errorString"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("text output %q is missing %q", got, want)
		}
	}
}
