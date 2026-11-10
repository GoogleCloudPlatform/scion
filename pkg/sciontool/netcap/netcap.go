/*
Copyright 2026 The Scion Authors.
*/

package netcap

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// capNetAdmin is CAP_NET_ADMIN's bit number (linux/capability.h).
const capNetAdmin = 12

// CapabilityName is the only identifying text DropNetAdmin's errors carry.
const CapabilityName = "CAP_NET_ADMIN"

// ErrStillHeld is wrapped by DropNetAdmin's error when the post-drop
// verification finds a thread that still holds the capability.
var ErrStillHeld = errors.New(CapabilityName + " still held after drop")

// sets is one thread's capability sets as reported by /proc/<tid>/status.
type sets struct {
	inh, prm, eff, bnd, amb uint64
}

func (s sets) holds(bit uint) bool {
	m := uint64(1) << bit
	return (s.inh|s.prm|s.eff|s.bnd|s.amb)&m != 0
}

// parseStatus extracts the capability sets from a /proc status file.
// CapInh, CapPrm, CapEff and CapBnd are required — a status file without
// them cannot prove the capability is gone, so it is an error rather than
// an all-zero result. CapAmb is optional (kernels before 4.3 lack ambient
// capabilities, and so cannot hold one).
func parseStatus(content string) (sets, error) {
	var s sets
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		var dst *uint64
		switch name {
		case "CapInh":
			dst = &s.inh
		case "CapPrm":
			dst = &s.prm
		case "CapEff":
			dst = &s.eff
		case "CapBnd":
			dst = &s.bnd
		case "CapAmb":
			dst = &s.amb
		default:
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(val), 16, 64)
		if err != nil {
			return sets{}, fmt.Errorf("parse %s: %w", name, err)
		}
		*dst = v
		seen[name] = true
	}
	for _, name := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd"} {
		if !seen[name] {
			return sets{}, fmt.Errorf("status lacks %s", name)
		}
	}
	return s, nil
}

// verifyNone returns an error if any of statuses (one per thread) holds
// bit in any set, or cannot be parsed, or if there are no statuses at all.
func verifyNone(statuses []string, bit uint) error {
	if len(statuses) == 0 {
		return fmt.Errorf("verify %s: no thread status read", CapabilityName)
	}
	for i, st := range statuses {
		s, err := parseStatus(st)
		if err != nil {
			return fmt.Errorf("verify %s: thread %d: %w", CapabilityName, i, err)
		}
		if s.holds(bit) {
			return fmt.Errorf("verify: thread %d: %w", i, ErrStillHeld)
		}
	}
	return nil
}

// anyHolds reports whether any of statuses holds bit in any set.
func anyHolds(statuses []string, bit uint) (bool, error) {
	if len(statuses) == 0 {
		return false, fmt.Errorf("check %s: no thread status read", CapabilityName)
	}
	for i, st := range statuses {
		s, err := parseStatus(st)
		if err != nil {
			return false, fmt.Errorf("check %s: thread %d: %w", CapabilityName, i, err)
		}
		if s.holds(bit) {
			return true, nil
		}
	}
	return false, nil
}

// allUsable reports whether every one of statuses holds bit in both its
// effective and its bounding set: effective so this process can act with
// it, bounding so a program it executes as root gets it too.
func allUsable(statuses []string, bit uint) (bool, error) {
	if len(statuses) == 0 {
		return false, fmt.Errorf("check %s: no thread status read", CapabilityName)
	}
	m := uint64(1) << bit
	for i, st := range statuses {
		s, err := parseStatus(st)
		if err != nil {
			return false, fmt.Errorf("check %s: thread %d: %w", CapabilityName, i, err)
		}
		if s.eff&m == 0 || s.bnd&m == 0 {
			return false, nil
		}
	}
	return true, nil
}
