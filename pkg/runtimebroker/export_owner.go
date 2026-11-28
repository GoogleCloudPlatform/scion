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
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// exportOwnerMarkerMaxBytes bounds how much of the owner marker file is read.
const exportOwnerMarkerMaxBytes = 64

// shareHealthOwnerMismatch is the leading text of the project_storage health
// check value when this broker's effective agent uid/gid differs from the
// canonical owner recorded beside .scion-export-id. Its leading word
// ("warning") is what the heartbeat carries (api.NormalizeBrokerHealthReport);
// the rest names only the uid:gid pairs and configuration fields, never paths.
const shareHealthOwnerMismatch = "warning: owner_mismatch"

// exportOwnerReadTimeout bounds how long checkShareHealth waits for the owner
// marker read/create on a shared mount.
const exportOwnerReadTimeout = exportIDReadTimeout

// parseExportOwner parses "<uid>:<gid>" from a marker file body. Both ids must
// be non-negative base-10 integers.
func parseExportOwner(data []byte) (uid, gid int, err error) {
	raw := strings.TrimSpace(string(data))
	uidStr, gidStr, ok := strings.Cut(raw, ":")
	if !ok || strings.Contains(gidStr, ":") {
		return 0, 0, fmt.Errorf("expected <uid>:<gid>, got %q", raw)
	}
	u, err := strconv.Atoi(uidStr)
	if err != nil || u < 0 {
		return 0, 0, fmt.Errorf("invalid uid %q", uidStr)
	}
	g, err := strconv.Atoi(gidStr)
	if err != nil || g < 0 {
		return 0, 0, fmt.Errorf("invalid gid %q", gidStr)
	}
	return u, g, nil
}

// readExportOwnerMarker reads and validates the "<uid>:<gid>" pair in marker.
func readExportOwnerMarker(marker string) (uid, gid int, err error) {
	f, err := os.Open(marker)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, exportOwnerMarkerMaxBytes+1))
	if err != nil {
		return 0, 0, fmt.Errorf("read export owner marker %s: %w", marker, err)
	}
	if len(data) > exportOwnerMarkerMaxBytes {
		return 0, 0, fmt.Errorf("export owner marker %s is larger than %d bytes", marker, exportOwnerMarkerMaxBytes)
	}
	uid, gid, err = parseExportOwner(data)
	if err != nil {
		return 0, 0, fmt.Errorf("export owner marker %s: %w", marker, err)
	}
	return uid, gid, nil
}

// ownerMismatchWarning returns the /healthz project_storage warning value when
// the broker's effective agent uid/gid differs from the recorded project
// storage owner, or "" when they match. It never includes filesystem paths.
func ownerMismatchWarning(agentUID, agentGID, recordedUID, recordedGID int) string {
	if agentUID == recordedUID && agentGID == recordedGID {
		return ""
	}
	return fmt.Sprintf(
		"%s (agent uid:gid %d:%d differs from recorded project storage owner %d:%d; align server.project_storage.nfs.uid/gid or the broker runtime uid/gid across brokers sharing this store)",
		shareHealthOwnerMismatch, agentUID, agentGID, recordedUID, recordedGID,
	)
}

// readOrCreateExportOwnerWithContext bounds readOrCreateExportOwner by ctx and
// exportOwnerReadTimeout so a stalled shared mount cannot hang the health loop.
func readOrCreateExportOwnerWithContext(ctx context.Context, dir string, uid, gid int) (ownerUID, ownerGID int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	type result struct {
		uid, gid int
		err      error
	}
	done := make(chan result, 1)
	go func() {
		u, g, rerr := readOrCreateExportOwner(dir, uid, gid)
		done <- result{uid: u, gid: g, err: rerr}
	}()
	timer := time.NewTimer(exportOwnerReadTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.uid, r.gid, r.err
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	case <-timer.C:
		return 0, 0, context.DeadlineExceeded
	}
}
