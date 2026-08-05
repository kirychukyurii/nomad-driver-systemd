package plugin

import (
	"strconv"
	"strings"
	"testing"
)

func TestUnitPolicy_Check(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		denied  []string
		unit    string
		wantErr bool
	}{
		{
			name: "empty policy permits everything",
			unit: "sshd.service",
		},
		{
			name:    "denied rejects even without an allowlist",
			denied:  []string{`^sshd\.service$`, `^nomad\.service$`},
			unit:    "sshd.service",
			wantErr: true,
		},
		{
			name:   "unlisted unit permitted when only denials configured",
			denied: []string{`^sshd\.service$`},
			unit:   "myapp.service",
		},
		{
			name:    "allowlist mode permits a match",
			allowed: []string{`^myapp-.*\.service$`},
			unit:    "myapp-web.service",
		},
		{
			name:    "allowlist mode rejects a non-match",
			allowed: []string{`^myapp-.*\.service$`},
			unit:    "sshd.service",
			wantErr: true,
		},
		{
			name:    "denied wins over a matching allow rule",
			allowed: []string{`^myapp-.*\.service$`},
			denied:  []string{`^myapp-secret\.service$`},
			unit:    "myapp-secret.service",
			wantErr: true,
		},
		{
			name:    "non-denied sibling still permitted",
			allowed: []string{`^myapp-.*\.service$`},
			denied:  []string{`^myapp-secret\.service$`},
			unit:    "myapp-web.service",
		},
		{
			name:    "any of several allow rules is enough",
			allowed: []string{`^web-.*\.service$`, `^worker-.*\.service$`},
			unit:    "worker-1.service",
		},
		{
			name:    "unanchored pattern matches substrings",
			allowed: []string{`myapp`},
			unit:    "prefix-myapp-suffix.service",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := compileUnitPolicy(tc.allowed, tc.denied)
			if err != nil {
				t.Fatalf("compileUnitPolicy: %v", err)
			}

			err = policy.check(tc.unit)
			if (err != nil) != tc.wantErr {
				t.Fatalf("check(%q) error = %v, wantErr %v", tc.unit, err, tc.wantErr)
			}
		})
	}
}

// TestUnitPolicy_ErrorNamesTheOffendingRule keeps the rejection message
// actionable: an operator needs to know which rule blocked the unit.
func TestUnitPolicy_ErrorNamesTheOffendingRule(t *testing.T) {
	const pattern = `^sshd\.service$`

	policy, err := compileUnitPolicy(nil, []string{pattern})
	if err != nil {
		t.Fatalf("compileUnitPolicy: %v", err)
	}

	err = policy.check("sshd.service")
	if err == nil {
		t.Fatalf("expected sshd.service to be denied")
	}

	// The message renders the pattern with %q, so compare against the same
	// quoted form rather than the raw regex.
	if want := strconv.Quote(pattern); !strings.Contains(err.Error(), want) {
		t.Fatalf("error should name the matching pattern %s, got: %v", want, err)
	}

	if !strings.Contains(err.Error(), "denied_units") {
		t.Fatalf("error should say which list rejected the unit, got: %v", err)
	}
}

func TestCompileUnitPolicy_InvalidRegex(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		denied  []string
	}{
		{name: "invalid allowed pattern", allowed: []string{"["}},
		{name: "invalid denied pattern", denied: []string{"["}},
		{name: "invalid among valid", allowed: []string{`^ok\.service$`, "(unclosed"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := compileUnitPolicy(tc.allowed, tc.denied); err == nil {
				t.Fatalf("expected compilation to fail")
			}
		})
	}
}

func TestUnitOwnership(t *testing.T) {
	cases := []struct {
		name string
		// ops are applied in order; each is either a claim or a release
		ops []struct {
			release bool
			unit    string
			taskID  string
			wantErr bool
		}
		wantOwner map[string]string
	}{
		{
			name: "claiming a free unit succeeds",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{unit: "app.service", taskID: "task-a"},
			},
			wantOwner: map[string]string{"app.service": "task-a"},
		},
		{
			name: "conflicting owner is rejected without mutating state",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{unit: "app.service", taskID: "task-a"},
				{unit: "app.service", taskID: "task-b", wantErr: true},
			},
			wantOwner: map[string]string{"app.service": "task-a"},
		},
		{
			name: "reclaiming by the same owner is idempotent",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{unit: "app.service", taskID: "task-a"},
				{unit: "app.service", taskID: "task-a"},
			},
			wantOwner: map[string]string{"app.service": "task-a"},
		},
		{
			name: "release allows another task to claim",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{unit: "app.service", taskID: "task-a"},
				{release: true, unit: "app.service"},
				{unit: "app.service", taskID: "task-b"},
			},
			wantOwner: map[string]string{"app.service": "task-b"},
		},
		{
			name: "releasing an unknown unit is a no-op",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{release: true, unit: "nope.service"},
			},
			wantOwner: map[string]string{},
		},
		{
			name: "different units are independent",
			ops: []struct {
				release bool
				unit    string
				taskID  string
				wantErr bool
			}{
				{unit: "a.service", taskID: "task-a"},
				{unit: "b.service", taskID: "task-b"},
			},
			wantOwner: map[string]string{"a.service": "task-a", "b.service": "task-b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver()

			for i, op := range tc.ops {
				if op.release {
					d.releaseUnit(op.unit)

					continue
				}

				err := d.claimUnit(op.unit, op.taskID)
				if (err != nil) != op.wantErr {
					t.Fatalf("op %d: claimUnit(%q, %q) error = %v, wantErr %v", i, op.unit, op.taskID, err, op.wantErr)
				}
			}

			if len(d.unitOwners) != len(tc.wantOwner) {
				t.Fatalf("owners = %v, want %v", d.unitOwners, tc.wantOwner)
			}

			for unit, want := range tc.wantOwner {
				if got := d.unitOwners[unit]; got != want {
					t.Errorf("owner of %q = %q, want %q", unit, got, want)
				}
			}
		})
	}
}

func TestClaimUnit_ErrorNamesCurrentOwner(t *testing.T) {
	d := newTestDriver()

	if err := d.claimUnit("app.service", "task-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err := d.claimUnit("app.service", "task-b")
	if err == nil {
		t.Fatalf("expected a conflict error")
	}

	if !strings.Contains(err.Error(), "task-a") {
		t.Fatalf("error should name the current owner, got: %v", err)
	}
}
