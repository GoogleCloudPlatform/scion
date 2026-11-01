// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package projectfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ShareVisibleTimeout bounds ShareDirVisible's stat when ctx has no earlier
// deadline: a stat of a hung network mount can block indefinitely.
var ShareVisibleTimeout = 10 * time.Second

// ShareDirVisible reports whether hostBase, the mount point of a shared
// Layout's share on this host (Layout.HostBase), is a directory this
// process can see. It is the share-visibility predicate every broker-side
// check of a shared Layout uses, so the call sites cannot drift. The stat
// runs in its own goroutine, bounded by ctx and ShareVisibleTimeout; a stat
// that does not return in time is an error. The returned error wraps the
// stat error (errors.Is(err, fs.ErrNotExist) holds for a missing path).
//
// A stat that hangs (a hard network mount whose server is gone) cannot be
// interrupted: the call returns at the deadline, but its goroutine stays
// blocked in the stat until the kernel returns it, one goroutine per
// timed-out call. That is bounded by the callers' rate (one per dispatch or
// look-up) and is strictly better than the plain os.Stat it replaces, which
// blocked the caller itself.
//
// It proves only that a directory is there. The caller proves that it holds
// the expected project tree (for example the hub-seeded project-id under
// <ProjectRoot>/.scion).
func ShareDirVisible(ctx context.Context, hostBase string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, ShareVisibleTimeout)
	defer cancel()
	type result struct {
		info os.FileInfo
		err  error
	}
	done := make(chan result, 1)
	go func() {
		info, err := os.Stat(hostBase)
		done <- result{info, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		if !r.info.IsDir() {
			return fmt.Errorf("%s is not a directory", hostBase)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("checking %s: %w", hostBase, ctx.Err())
	}
}

// ShareSubPathRootDir is <hostBase>/<subPathRoot>, the directory every
// project tree of a shared Layout lives in, with subPathRoot defaulting to
// DefaultSubPathRoot as Layout does. A mount point without it is not the
// share: the start refusal, the broker's health check and the advisory
// mount-check log all test this one directory.
func ShareSubPathRootDir(hostBase, subPathRoot string) string {
	if subPathRoot == "" {
		subPathRoot = DefaultSubPathRoot
	}
	return filepath.Join(hostBase, subPathRoot)
}
