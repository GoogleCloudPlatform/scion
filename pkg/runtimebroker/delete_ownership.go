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
	deleteProcessUID = os.Getuid
	deleteProcessGID = os.Getgid
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

// detectDeleteOwnershipMismatch reports whether err is a filesystem permission
// error whose failing path (or its parent directory) is owned by a different
// host UID than the Runtime Broker process.
//
// rootDir is joined onto a relative fs.PathError.Path (such as errors returned
// by os.Root.RemoveAll) before inspecting ownership.
func detectDeleteOwnershipMismatch(err error, rootDir string) (*deleteOwnershipMismatch, bool) {
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
	return failedOpText(op) + ": permission denied because files are owned by a different host UID than the runtime broker; fix file ownership on the broker host and retry (see ptone/scion#4403)"
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
		args := make([]any, 0, len(extra)+18)
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
