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

package runtimebroker

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Seams for tests to simulate foreign-owned files without root/chown.
var (
	deletePathOwner  = lstatPathOwner
	deleteRootOwner  = func(_ string, fi fs.FileInfo) (int, int, bool) { return fileInfoOwner(fi) }
	deleteProcessUID = os.Getuid
	deleteProcessGID = os.Getgid
)

// maxOwnershipWalkEntries bounds the read-only walk inside an os.Root when
// os.Root.RemoveAll fails with fs.ErrPermission (os.Root.RemoveAll reports
// only the top-level relative path, not the nested entry that failed).
// ownershipWalkReadDirBatch caps how many directory entries are read per
// ReadDir call so a single huge directory cannot defeat the entry bound.
const (
	maxOwnershipWalkEntries   = 10000
	ownershipWalkReadDirBatch = 128
)

// deleteOwnershipMismatch describes a filesystem permission error during
// agent or project deletion caused by files owned by a different host UID
// than the Runtime Broker process (ptone/scion#1706).
type deleteOwnershipMismatch struct {
	Path      string
	OwnerPath string
	OwnerUID  int
	OwnerGID  int
	BrokerUID int
	BrokerGID int
}

// deleteOwnershipError wraps a filesystem permission error with the nested
// foreign-owned entry discovered inside an os.Root after os.Root.RemoveAll
// failed (os.Root.RemoveAll only reports the top-level name it was called with).
type deleteOwnershipError struct {
	mismatch *deleteOwnershipMismatch
	err      error
}

func (e *deleteOwnershipError) Error() string { return e.err.Error() }
func (e *deleteOwnershipError) Unwrap() error { return e.err }

// findForeignOwnerInRoot performs a bounded, read-only walk of rel inside root
// without following symlinks, returning the first directory whose owner UID
// differs from the Runtime Broker process's UID. Returns nil when no foreign
// owner is found or the walk limit is reached.
func findForeignOwnerInRoot(root *os.Root, rootDir, rel string) *deleteOwnershipMismatch {
	brokerUID := deleteProcessUID()
	brokerGID := deleteProcessGID()

	queue := []string{filepath.Clean(rel)}
	visited := 0

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		fi, err := root.Lstat(cur)
		if err != nil || !fi.IsDir() {
			continue
		}

		fullCur := filepath.Join(rootDir, cur)
		if uid, gid, ok := deleteRootOwner(fullCur, fi); ok && uid != brokerUID {
			failingPath := fullCur
			if child := firstEntryInRootDir(root, cur); child != "" {
				failingPath = filepath.Join(fullCur, child)
			}
			return &deleteOwnershipMismatch{
				Path:      failingPath,
				OwnerPath: fullCur,
				OwnerUID:  uid,
				OwnerGID:  gid,
				BrokerUID: brokerUID,
				BrokerGID: brokerGID,
			}
		}

		f, err := root.Open(cur)
		if err != nil {
			continue
		}
		limitReached := false
		for {
			entries, readErr := f.ReadDir(ownershipWalkReadDirBatch)
			for _, de := range entries {
				visited++
				if visited > maxOwnershipWalkEntries {
					limitReached = true
					break
				}
				if de.Type()&fs.ModeSymlink != 0 {
					continue
				}
				if de.IsDir() {
					queue = append(queue, filepath.Join(cur, de.Name()))
				}
			}
			if limitReached || readErr != nil {
				break
			}
		}
		_ = f.Close()
		if limitReached {
			return nil
		}
	}
	return nil
}

// firstEntryInRootDir returns the name of the first entry in dir inside root,
// or "" if dir is empty or unreadable.
func firstEntryInRootDir(root *os.Root, dir string) string {
	f, err := root.Open(dir)
	if err != nil {
		return ""
	}
	entries, err := f.ReadDir(1)
	_ = f.Close()
	if err != nil || len(entries) == 0 {
		return ""
	}
	return entries[0].Name()
}

// detectDeleteOwnershipMismatch reports whether err is a filesystem permission
// error whose failing path (or its parent directory) is owned by a different
// host UID than the Runtime Broker process.
//
// rootDir is joined onto a relative fs.PathError.Path before inspecting
// ownership. Errors already annotated by removeProjectConfigsSubtree
// (*deleteOwnershipError) return their recorded mismatch directly.
func detectDeleteOwnershipMismatch(err error, rootDir string) (*deleteOwnershipMismatch, bool) {
	var oe *deleteOwnershipError
	if errors.As(err, &oe) && oe.mismatch != nil {
		return oe.mismatch, true
	}
	if !errors.Is(err, fs.ErrPermission) {
		return nil, false
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path == "" {
		return nil, false
	}

	targetPath := pe.Path
	if !filepath.IsAbs(targetPath) && rootDir != "" {
		targetPath = filepath.Join(rootDir, targetPath)
	}

	brokerUID := deleteProcessUID()
	brokerGID := deleteProcessGID()

	// Unlinking a file or subdirectory requires write permission on its parent
	// directory (e.g., deleting .../home/.bashrc fails when .../home is owned
	// by UID 1000 mode 0755 and the broker runs as UID 1002). Check the parent
	// first, then fall back to the target path itself (e.g., an unreadable dir).
	parent := filepath.Dir(targetPath)
	if uid, gid, ok := deletePathOwner(parent); ok && uid != brokerUID {
		return &deleteOwnershipMismatch{
			Path:      targetPath,
			OwnerPath: parent,
			OwnerUID:  uid,
			OwnerGID:  gid,
			BrokerUID: brokerUID,
			BrokerGID: brokerGID,
		}, true
	}
	if uid, gid, ok := deletePathOwner(targetPath); ok && uid != brokerUID {
		return &deleteOwnershipMismatch{
			Path:      targetPath,
			OwnerPath: targetPath,
			OwnerUID:  uid,
			OwnerGID:  gid,
			BrokerUID: brokerUID,
			BrokerGID: brokerGID,
		}, true
	}
	return nil, false
}

// deleteOwnershipMismatchMessage returns the path-free client message for a
// delete operation blocked by a host UID ownership mismatch.
func deleteOwnershipMismatchMessage(op string) string {
	return failedOpText(op) + ": permission denied because files are owned by a different host UID than the runtime broker; fix ownership of the agent home on the broker host so the broker user can delete it and retry"
}

// writeDeleteOpError handles a filesystem deletion error from deleteProject or
// deleteAgent. When err is a permission error caused by foreign-owned files,
// it logs the exact failing path and UID/GID mismatch server-side and returns
// a clear, path-free 500 runtime_error (HTTP 500 is used because the failure
// is a host/server-side ownership mismatch requiring operator intervention,
// not a client 4xx error and not a 409 which the hub's CleanupProject treats
// as a project directory intentionally kept because agents remain). For all
// other errors it delegates to writeRuntimeOpError.
func (s *Server) writeDeleteOpError(w http.ResponseWriter, ctx context.Context, op string, err error, rootDir string, extra ...any) {
	if m, ok := detectDeleteOwnershipMismatch(err, rootDir); ok {
		// 8 fixed key/value pairs (16) + extra + "error"/err (2).
		args := make([]any, 0, 16+len(extra)+2)
		args = append(args,
			"op", op,
			"reason", "file_ownership_mismatch",
			"path", m.Path,
			"owner_path", m.OwnerPath,
			"owner_uid", m.OwnerUID,
			"owner_gid", m.OwnerGID,
			"broker_uid", m.BrokerUID,
			"broker_gid", m.BrokerGID,
		)
		args = append(args, extra...)
		args = append(args, "error", err)
		s.agentLifecycleLog.Error("runtime op failed", args...)
		trace.SpanFromContext(ctx).SetStatus(codes.Error, err.Error())
		RuntimeError(w, deleteOwnershipMismatchMessage(op))
		return
	}
	s.writeRuntimeOpError(w, ctx, op, err, extra...)
}
