//go:build !unix

/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"errors"
	"fmt"
)

// writeProvisionStatusInMemOutputs fails closed on this platform, which has
// no in-memory harness volume and no descriptor-relative directory
// operations: nothing is created or written.
func writeProvisionStatusInMemOutputs(path string, data []byte) error {
	return fmt.Errorf("write provision status %s: %w on this platform", path, errors.ErrUnsupported)
}
