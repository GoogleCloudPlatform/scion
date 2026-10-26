//go:build !unix

/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// EnforcedHooksDir is the root-owned directory enforced hooks run from.
var EnforcedHooksDir = "/run/scion/hooks"

// ErrScriptRefused reports a hook script that is a symlink or not a regular
// file.
var ErrScriptRefused = errors.New("hooks: script is a symlink or not a regular file")

// Enforced hook execution walks the hooks directory with openat and
// O_NOFOLLOW and execs through /proc/self/fd, none of which this platform
// has. Enforced hooks only run inside agent containers.

func unsupportedEnforced(what string) error {
	return fmt.Errorf("hooks: %s: %w on this platform", what, errors.ErrUnsupported)
}

func openChainNoFollow(dir string) (fd int, chain []NodeOwnership, err error) {
	return -1, nil, unsupportedEnforced("open hooks directory " + dir)
}

func openScriptNoFollow(parentFd int, name string) (f *os.File, ownership NodeOwnership, err error) {
	return nil, NodeOwnership{}, unsupportedEnforced("open hook script " + name)
}

func execViaFd(f *os.File, displayPath string) (*exec.Cmd, error) {
	return nil, unsupportedEnforced("exec " + displayPath)
}

func fdIsExecutable(f *os.File) (bool, error) {
	return false, unsupportedEnforced("stat hook script")
}

func closeFd(fd int) error { return nil }
