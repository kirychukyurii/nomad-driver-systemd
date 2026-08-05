// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

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

// claimUnit records taskID as the sole owner of unit.
//
// It returns an error naming the current owner, and leaves ownership untouched,
// if a different task already owns the unit. Re-claiming a unit the same task
// already owns succeeds and changes nothing, which is what makes recovery
// idempotent.
func (d *Driver) claimUnit(unit, taskID string) error {
	d.unitOwnersLock.Lock()
	defer d.unitOwnersLock.Unlock()

	if owner, exists := d.unitOwners[unit]; exists && owner != taskID {
		return fmt.Errorf("unit %q is already managed by task %q", unit, owner)
	}

	d.unitOwners[unit] = taskID

	return nil
}

// releaseUnit drops any ownership claim on unit. Releasing an unclaimed unit
// does nothing.
func (d *Driver) releaseUnit(unit string) {
	d.unitOwnersLock.Lock()
	defer d.unitOwnersLock.Unlock()

	delete(d.unitOwners, unit)
}
