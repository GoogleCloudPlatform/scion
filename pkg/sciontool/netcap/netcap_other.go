//go:build !linux

/*
Copyright 2026 The Scion Authors.
*/

package netcap

import (
	"errors"
	"fmt"
)

// Held fails closed off Linux: there is no capability model to inspect.
func Held() (bool, error) {
	return false, fmt.Errorf("check %s: %w on this platform", CapabilityName, errors.ErrUnsupported)
}

// Usable fails closed off Linux: there is no capability model to inspect.
func Usable() (bool, error) {
	return false, fmt.Errorf("check %s: %w on this platform", CapabilityName, errors.ErrUnsupported)
}

// DropNetAdmin fails closed off Linux.
func DropNetAdmin() error {
	return fmt.Errorf("drop %s: %w on this platform", CapabilityName, errors.ErrUnsupported)
}
