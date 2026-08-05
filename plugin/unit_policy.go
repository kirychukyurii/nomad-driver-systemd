package plugin

import (
	"fmt"
	"regexp"
)

// unitPolicy decides which systemd units tasks are allowed to manage.
//
// A policy is immutable once compiled and so is safe to share between
// goroutines.
type unitPolicy struct {
	allowed []*regexp.Regexp
	denied  []*regexp.Regexp
}

// compileUnitPolicy builds a policy from the allowed_units and denied_units
// pattern lists.
//
// It returns an error naming the offending pattern if any of them is not a valid
// regular expression, rather than letting it match nothing.
func compileUnitPolicy(allowed, denied []string) (*unitPolicy, error) {
	p := &unitPolicy{}

	for _, pattern := range allowed {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile allowed_units pattern %q: %w", pattern, err)
		}

		p.allowed = append(p.allowed, re)
	}

	for _, pattern := range denied {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile denied_units pattern %q: %w", pattern, err)
		}

		p.denied = append(p.denied, re)
	}

	return p, nil
}

// check reports whether unit may be managed, returning an error naming the rule
// that rejected it.
//
// A denied pattern always wins, even over a matching allowed pattern. If any
// allowed pattern is configured, the policy becomes an allowlist and a unit
// matching none of them is rejected. A policy with no patterns at all permits
// every unit, which is the default for an operator who has not configured either
// list.
func (p *unitPolicy) check(unit string) error {
	for _, re := range p.denied {
		if re.MatchString(unit) {
			return fmt.Errorf("unit %q matches denied_units pattern %q", unit, re.String())
		}
	}

	if len(p.allowed) == 0 {
		return nil
	}

	for _, re := range p.allowed {
		if re.MatchString(unit) {
			return nil
		}
	}

	return fmt.Errorf("unit %q does not match any allowed_units pattern", unit)
}
