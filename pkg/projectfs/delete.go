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
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Deletion of project storage (ptone/scion#5835).
//
// Every tree a project or an agent owns has exactly one deleter, and every
// deletion is reported by one audit line (Audit) that names it:
//
//   - the hub deletes what lives on the shared storage and in object
//     storage: the project root <SubPathRoot>/<projectID>
//     (RemoveProjectTree), an agent's home and secrets and its
//     agents/<slug> directory once empty (RemoveAgentStorage), the
//     project's and an agent's object-storage prefixes, and the legacy
//     slug-keyed trees whose identity markers name the project;
//   - a broker deletes what lives on its own host: its project
//     directory and project-configs entry, its agents' state directories,
//     and the per-dir shared-dir claims of its Kubernetes runtimes; and,
//     on the share, an agent's own workspace or worktree, which needs the
//     provisioning lock and git bookkeeping only the broker has
//     (TreeAgentWorkspace).
//
// The local backend keeps its slug-keyed trees and their existing deleters
// unchanged; RemoveProjectTree and RemoveAgentStorage remove nothing there.

// Deleter names who deleted a tree, in the audit line.
type Deleter string

const (
	DeleterHub    Deleter = "hub"
	DeleterBroker Deleter = "broker"
)

// TreeKind names a deleted tree, in the audit line.
type TreeKind string

const (
	// TreeProjectRoot is <SubPathRoot>/<projectID> on a shared backend.
	TreeProjectRoot TreeKind = "project-root"
	// TreeAgentRoot is <SubPathRoot>/<projectID>/agents/<slug>.
	TreeAgentRoot TreeKind = "agent-root"
	// TreeAgentWorkspace is an agent's own workspace on the share
	// (agents/<slug>/workspace for clone-per-agent, or its worktree under
	// workspace/worktrees/<slug>), removed by the broker under the
	// provisioning lock with its git and branch bookkeeping; the hub then
	// removes what remains of the agent root.
	TreeAgentWorkspace TreeKind = "agent-workspace"
	// TreeAgentHome and TreeAgentSecrets are an agent's home-<agentID> and
	// secrets-<agentID> below its agent root, deleted on their own when
	// another agent of the same slug still uses the agent root.
	TreeAgentHome    TreeKind = "agent-home"
	TreeAgentSecrets TreeKind = "agent-secrets"
	// TreeLegacyHubProjects is a hub-managed project's retired
	// hub-projects/<slug> tree on the share (or a .migrated-<ts> copy).
	TreeLegacyHubProjects TreeKind = "legacy-hub-projects"
	// TreeLegacyLocalProject is a host's ~/.scion/projects/<slug> of a
	// project on a shared backend (or a .migrated-<ts> copy).
	TreeLegacyLocalProject TreeKind = "legacy-local-project"
	// TreeProjectDir is a broker's ~/.scion/projects/<slug>.
	TreeProjectDir TreeKind = "project-dir"
	// TreeProjectConfig is ~/.scion/project-configs/<slug>__<short id>.
	TreeProjectConfig TreeKind = "project-config"
	// TreeAgentState is an agent's broker-local state directory.
	TreeAgentState TreeKind = "agent-state"
	// TreeLegacyAgentDir is an agent's legacy in-project state directory
	// ~/.scion/projects/<slug>/.scion/agents/<name>.
	TreeLegacyAgentDir TreeKind = "legacy-agent-dir"
	// TreeObjectPrefix is an object-storage prefix.
	TreeObjectPrefix TreeKind = "object-prefix"
	// TreeClaim is a Kubernetes PersistentVolumeClaim.
	TreeClaim TreeKind = "pvc"
)

// Outcome is what a deletion did with one tree.
type Outcome string

const (
	OutcomeDeleted Outcome = "deleted"
	// OutcomeAbsent: there was nothing to delete.
	OutcomeAbsent Outcome = "absent"
	// OutcomeKept: the tree was deliberately left in place (Reason says
	// why), for example a legacy tree whose markers do not name the project.
	OutcomeKept   Outcome = "kept"
	OutcomeFailed Outcome = "failed"
)

// Deletion is the outcome for one tree.
type Deletion struct {
	Tree TreeKind
	// Path is the host path, object prefix or claim (namespace/name).
	Path    string
	Outcome Outcome
	Reason  string
	Err     error
}

// deletion builds a Deletion from a removal's result.
func deletion(tree TreeKind, path string, existed bool, err error) Deletion {
	d := Deletion{Tree: tree, Path: path, Outcome: OutcomeDeleted}
	switch {
	case err != nil:
		d.Outcome, d.Err = OutcomeFailed, err
	case !existed:
		d.Outcome = OutcomeAbsent
	}
	return d
}

// AuditMessage is the message of every storage deletion audit line.
const AuditMessage = "project storage deletion"

// Audit writes the one audit line of a deletion: who deleted (deleter),
// which tree, of which project (and agent, when set), and the outcome.
// A failure is logged at ERROR, a kept tree at WARN, the rest at INFO.
func Audit(ctx context.Context, logger *slog.Logger, deleter Deleter, projectID, agent string, d Deletion) {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []any{
		"audit", "storage_delete",
		"deleter", string(deleter),
		"tree", string(d.Tree),
		"project_id", projectID,
		"path", d.Path,
		"outcome", string(d.Outcome),
	}
	if agent != "" {
		attrs = append(attrs, "agent", agent)
	}
	if d.Reason != "" {
		attrs = append(attrs, "reason", d.Reason)
	}
	level := slog.LevelInfo
	switch d.Outcome {
	case OutcomeFailed:
		level = slog.LevelError
		if d.Err != nil {
			attrs = append(attrs, "error", d.Err.Error())
		}
	case OutcomeKept:
		level = slog.LevelWarn
	}
	logger.Log(ctx, level, AuditMessage, attrs...)
}

// Redact returns d fit for a log that must carry no host filesystem path:
// Path is made relative to the first of bases it lies below (only its last
// element when it lies below none; object prefixes and claim names are kept),
// every base is removed from Reason, and Err is replaced by its class
// (refused / no project tree, not exist, permission, other), never its
// text, which can name host paths.
func Redact(d Deletion, bases ...string) Deletion {
	if filepath.IsAbs(d.Path) {
		rel := ""
		for _, b := range bases {
			if b == "" {
				continue
			}
			if r, err := filepath.Rel(b, d.Path); err == nil && r != ".." && !strings.HasPrefix(r, "../") { //winsec:not-containment display only: picks a log-safe relative path for an audit line
				rel = r
				break
			}
		}
		if rel == "" {
			rel = filepath.Base(d.Path)
		}
		d.Path = rel
	}
	for _, b := range bases {
		if b != "" {
			d.Reason = strings.ReplaceAll(d.Reason, strings.TrimSuffix(b, "/")+"/", "")
			d.Reason = strings.ReplaceAll(d.Reason, b, "")
		}
	}
	if d.Err != nil {
		class := errorClass(d.Err)
		if d.Tree == TreeObjectPrefix && class == "removal failed" {
			class = "object storage delete failed"
		}
		d.Err = errors.New(class)
	}
	return d
}

// errorClass names the class of a deletion error without its text (which
// can name host paths): refusals, timeouts, the errno of a failed system
// call, not-exist, permission, else a generic failure.
func errorClass(err error) string {
	var errno syscall.Errno
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrNoProjectTree):
		return "refused: no project tree below the host base (share not mounted)"
	case errors.Is(err, ErrRefused):
		return "refused (unusable host base, or a symlink or non-directory in the path)"
	case errors.Is(err, fs.ErrNotExist):
		return "not exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.As(err, &errno):
		return "removal failed: " + errno.Error()
	default:
		return "removal failed"
	}
}

// AuditAll writes one audit line per deletion.
func AuditAll(ctx context.Context, logger *slog.Logger, deleter Deleter, projectID, agent string, ds []Deletion) {
	for _, d := range ds {
		Audit(ctx, logger, deleter, projectID, agent, d)
	}
}

// DeleteAfterFile is the deferred-deletion tombstone in a project's
// provision/ directory: an RFC 3339 UTC time.
const DeleteAfterFile = ".scion-delete-after"

// DeferredDeletion is a project root that carries a tombstone.
type DeferredDeletion struct {
	ProjectID string
	// After is the tombstone's time; zero when it cannot be parsed (the
	// tree is then treated as due).
	After time.Time
	// Layout is the shared Layout the root is on.
	Layout Layout
	// Path is the project root's host path.
	Path string
}

// ErrRefused marks a failed Deletion that was refused before anything was
// removed (an unusable host base, a symlinked or non-directory component,
// a share outside the mount root). Retrying cannot help with it.
var ErrRefused = errors.New("projectfs: deletion refused")

// ErrNoProjectTree marks the refusal of a shared Layout whose host base
// exists but has no subpath root: the share is not mounted, or the hub has
// no project yet (callers that can tell, from their own records, treat the
// latter as expected).
var ErrNoProjectTree = errors.New("projectfs: no project tree below the host base")

// ErrShareEmpty marks, together with ErrNoProjectTree, a shared Layout whose
// host base has no entries at all: the share is not mounted (only the empty
// mount point is), or a mounted share was emptied. errors.Is(err,
// ErrNoProjectTree) holds for it too, so callers of ErrNoProjectTree see no
// change.
var ErrShareEmpty = errors.New("projectfs: the host base is empty")

// Retryable reports whether d failed in a way a later attempt may clear: a
// removal that failed part-way (for example while a terminating process was
// still writing), not a refusal.
func Retryable(d Deletion) bool {
	return d.Outcome == OutcomeFailed && !errors.Is(d.Err, ErrRefused)
}

// Failed reports whether any deletion in ds failed.
func Failed(ds []Deletion) bool {
	for _, d := range ds {
		if d.Outcome == OutcomeFailed {
			return true
		}
	}
	return false
}

// safeObjectElement reports whether s is usable as one element of an
// object-storage prefix.
func safeObjectElement(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

// ProjectObjectPrefix is the object-storage prefix of everything a hub
// keeps for projectID's workspaces (the retired project-level copy and the
// per-agent prefixes): [hubs/<hubID>/]workspaces/<projectID>/, the prefix
// storage.WorkspaceStoragePath writes under. hubID is empty for a hub
// without a hub ID. An unsafe element is an error, never a wider prefix.
func ProjectObjectPrefix(hubID, projectID string) (string, error) {
	if err := validProjectID(projectID); err != nil {
		return "", err
	}
	if hubID != "" && !safeObjectElement(hubID) {
		return "", fmt.Errorf("projectfs: invalid hub ID %q", hubID)
	}
	p := "workspaces/" + projectID + "/"
	if hubID != "" {
		p = "hubs/" + hubID + "/" + p
	}
	return p, nil
}

// AgentObjectPrefix is the object-storage prefix of one agent's workspace
// objects: ProjectObjectPrefix + <agentID>/. agentID must be a hub agent
// ID (ValidAgentID).
func AgentObjectPrefix(hubID, projectID, agentID string) (string, error) {
	p, err := ProjectObjectPrefix(hubID, projectID)
	if err != nil {
		return "", err
	}
	if !ValidAgentID(agentID) {
		return "", fmt.Errorf("projectfs: invalid agent ID %q", agentID)
	}
	return p + agentID + "/", nil
}

// ObjectDeleter is the slice of object storage a prefix deletion needs.
type ObjectDeleter interface {
	DeletePrefix(ctx context.Context, prefix string) error
}

// DeleteObjectPrefix deletes every object under prefix (from
// ProjectObjectPrefix or AgentObjectPrefix) and reports it.
func DeleteObjectPrefix(ctx context.Context, store ObjectDeleter, prefix string) Deletion {
	d := Deletion{Tree: TreeObjectPrefix, Path: prefix, Outcome: OutcomeDeleted}
	if prefix == "" || !strings.HasSuffix(prefix, "/") || !strings.Contains(prefix, "workspaces/") {
		d.Outcome, d.Err = OutcomeFailed, fmt.Errorf("projectfs: refusing object prefix %q", prefix)
		return d
	}
	if err := store.DeletePrefix(ctx, prefix); err != nil {
		d.Outcome, d.Err = OutcomeFailed, err
	}
	return d
}
