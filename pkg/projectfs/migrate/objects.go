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

//go:build unix

package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// LegacyObjectPrefix is the directory prefix of a hub-managed project's
// retired object-storage workspace copy, the only objects M3 deletes:
// [hubs/<hubID>/]workspaces/<projectID>/<legacy dir>/. The per-agent
// prefixes beside it (workspaces/<projectID>/<agentID>/, used by the CLI
// bootstrap and scion sync) are live and never match it. It returns ""
// (which CleanObjects refuses) for an ID that is not a single safe path
// element.
func LegacyObjectPrefix(hubID, projectID string) string {
	if !safeObjectElement(projectID) || (hubID != "" && !safeObjectElement(hubID)) {
		return ""
	}
	p := "workspaces/" + projectID + "/" + projectkeys.LegacyProjectWorkspaceObjectDir + "/"
	if hubID != "" {
		p = "hubs/" + hubID + "/" + p
	}
	return p
}

func safeObjectElement(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

// ObjectStore is the slice of the hub's object storage M3 needs.
type ObjectStore interface {
	// HasObjects reports whether any object exists under prefix.
	HasObjects(ctx context.Context, prefix string) (bool, error)
	// DeletePrefix deletes every object under prefix.
	DeletePrefix(ctx context.Context, prefix string) error
}

// ObjectOutcome is what M3 did with one project's legacy workspace objects.
type ObjectOutcome string

const (
	ObjectsNone        ObjectOutcome = "none"
	ObjectsDeleted     ObjectOutcome = "deleted"
	ObjectsWouldDelete ObjectOutcome = "would-delete"
	// ObjectsKept: M2 did not succeed for the project, so its objects are
	// kept (they may be the only copy of what M2 failed to move).
	ObjectsKept   ObjectOutcome = "kept"
	ObjectsFailed ObjectOutcome = "failed"
)

// ObjectResult is the M3 report of one project.
type ObjectResult struct {
	Prefix  string
	Outcome ObjectOutcome
	Err     error
}

// CleanObjects is M3 (design §Migration M3) for one project: the GCS copy
// of the hub-managed workspace under prefix is deleted only when M2
// succeeded for that project AND its workspace steps actually ran (m2 has
// no failed step and is not WorkspaceIncomplete; conflicts kept in a
// .migrated copy are not failures). A dry run deletes nothing. prefix must
// be a directory prefix ending in "/".
func CleanObjects(ctx context.Context, store ObjectStore, prefix string, m2 ProjectReport, dryRun bool) ObjectResult {
	res := ObjectResult{Prefix: prefix}
	if store == nil {
		res.Outcome = ObjectsNone
		return res
	}
	if prefix == "" || prefix == "/" || !strings.HasSuffix(prefix, "/") || strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "//") {
		res.Outcome = ObjectsFailed
		res.Err = fmt.Errorf("refusing object prefix %q: must be a non-empty relative directory prefix ending in /", prefix)
		return res
	}
	has, err := store.HasObjects(ctx, prefix)
	if err != nil {
		res.Outcome = ObjectsFailed
		res.Err = fmt.Errorf("list objects: %w", err)
		return res
	}
	if !has {
		res.Outcome = ObjectsNone
		return res
	}
	if m2.Failed() {
		res.Outcome = ObjectsKept
		res.Err = errors.New("M2 did not succeed for this project; objects kept")
		return res
	}
	if m2.WorkspaceIncomplete {
		res.Outcome = ObjectsKept
		res.Err = errors.New("the project's workspace was not migrated on this hub; objects kept")
		return res
	}
	if !m2.WorkspacePopulated {
		res.Outcome = ObjectsKept
		res.Err = errors.New("no workspace content found on the shared storage for this project; the object copy may be the only copy, so it is kept")
		return res
	}
	if dryRun {
		res.Outcome = ObjectsWouldDelete
		return res
	}
	if err := store.DeletePrefix(ctx, prefix); err != nil {
		res.Outcome = ObjectsFailed
		res.Err = fmt.Errorf("delete objects: %w", err)
		return res
	}
	res.Outcome = ObjectsDeleted
	return res
}
