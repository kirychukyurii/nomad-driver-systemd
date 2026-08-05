package task

import "testing"

func TestMapPriorityToLevel(t *testing.T) {
	cases := []struct {
		priority string
		want     string
	}{
		{"0", "EMERG"},
		{"1", "ALERT"},
		{"2", "CRIT"},
		{"3", "ERR"},
		{"4", "WARN"},
		{"5", "NOTICE"},
		{"6", "INFO"},
		{"7", "DEBUG"},
		{"", "UNKNOWN"},
		{"9", "UNKNOWN"},
		{"garbage", "UNKNOWN"},
	}

	for _, tc := range cases {
		t.Run("priority "+tc.priority, func(t *testing.T) {
			if got := mapPriorityToLevel(tc.priority); got != tc.want {
				t.Fatalf("mapPriorityToLevel(%q) = %q, want %q", tc.priority, got, tc.want)
			}
		})
	}
}
