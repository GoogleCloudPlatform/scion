//go:build !unix

/*
Copyright 2025 The Scion Authors.
*/

package log

import (
	"errors"
	"fmt"
	"os"
)

// openLogFileNoFollow always fails with errors.ErrUnsupported. The unix
// version opens path through directory descriptors with O_NOFOLLOW at every
// component and refuses anything but a single-link regular file; this
// platform offers neither, and the agent log is only written inside agent
// containers, which never run here. There is no plain-open fallback: the
// caller reports the error and logging continues on stderr.
func openLogFileNoFollow(path string, mode os.FileMode) (*os.File, error) {
	return nil, fmt.Errorf("open log file %s: %w on this platform", path, errors.ErrUnsupported)
}
