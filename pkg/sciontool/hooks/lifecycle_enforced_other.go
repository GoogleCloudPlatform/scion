//go:build !unix

/*
Copyright 2025 The Scion Authors.
*/

package hooks

import (
	"errors"
	"os"
	"os/exec"
)

// buildEnforcedCmd is unsupported on this platform; see lifecycle_enforced_unix.go.
func (m *LifecycleManager) buildEnforcedCmd(scriptFile *os.File, path, eventName string, asRoot bool) (*exec.Cmd, error) {
	return nil, errors.ErrUnsupported
}

// buildDroppedProvisionCmd is unsupported on this platform; see lifecycle_enforced_unix.go.
func (m *LifecycleManager) buildDroppedProvisionCmd(cmd *exec.Cmd, path string) (*exec.Cmd, error) {
	return nil, errors.ErrUnsupported
}
