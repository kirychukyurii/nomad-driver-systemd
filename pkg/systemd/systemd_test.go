package systemd

import (
	"math"
	"testing"
)

func TestParseTimestampUsec(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		wantOK  bool
		wantVal uint64
	}{
		{"uint64", uint64(1234567890), true, 1234567890},
		{"positive int64", int64(42), true, 42},
		{"negative int64 rejected", int64(-1), false, 0},
		{"zero uint64", uint64(0), true, 0},
		{"unsupported type", "not a number", false, 0},
		{"nil", nil, false, 0},

		// USEC_INFINITY is systemd's "never". Accepting it would overflow the
		// int64 time.UnixMicro takes and silently report December 1969 as the
		// unit's start time.
		{"USEC_INFINITY rejected", usecInfinity, false, 0},
		{"anything past MaxInt64 rejected", uint64(math.MaxInt64) + 1, false, 0},
		{"MaxInt64 itself is still accepted", uint64(math.MaxInt64), true, uint64(math.MaxInt64)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseTimestampUsec(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}

			if ok && got != tc.wantVal {
				t.Fatalf("value = %v, want %v", got, tc.wantVal)
			}
		})
	}
}
