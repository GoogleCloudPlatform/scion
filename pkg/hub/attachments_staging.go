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

package hub

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Web chat attachments are stored on the hub host under the attachment store's
// base directory, which is not mounted into agent containers. Before a message
// is dispatched, each attachment is copied into the project's "scratchpad"
// shared dir — the same volume the Discord and CLI integrations stage into —
// so the path the agent receives resolves inside its container.
//
// Host side:  <shared-dirs>/scratchpad/.attachments/_webchat/<id>/<name>
// Agent side: /scion-volumes/scratchpad/.attachments/_webchat/<id>/<name>
const (
	// attachmentSharedDirName is the shared dir web chat attachments are staged into.
	attachmentSharedDirName = "scratchpad"
	// attachmentStagingSubdir is the per-channel subdirectory under .attachments,
	// matching the _discord / _telegram convention used by the plugin brokers.
	attachmentStagingSubdir = "_webchat"
)

// attachmentStaging copies attachments from hub-local storage into a shared dir
// that is bind-mounted into agent containers.
//
// Staged copies are not garbage collected: deleting an attachment removes it
// from the attachment store but leaves the staged copy in the shared dir, the
// same way Discord downloads persist.
type attachmentStaging struct {
	hostDir  string // host-side directory holding staged copies
	agentDir string // container-visible directory for the same files
}

// newAttachmentStaging builds a staging target from the host path of the
// project's shared dir. inWorkspace mirrors api.SharedDir.InWorkspace, which
// moves the container mount point under the agent workspace.
func newAttachmentStaging(sharedDirHostPath string, inWorkspace bool) *attachmentStaging {
	if sharedDirHostPath == "" {
		return nil
	}
	// Shared dirs mount at /scion-volumes/<name>, or <workspace>/.scion-volumes/<name>
	// when declared in-workspace. Agents with a non-default container workspace
	// (git worktrees) still resolve the /workspace form, which is the documented
	// location for in-workspace shared dirs.
	agentBase := "/scion-volumes/" + attachmentSharedDirName
	if inWorkspace {
		agentBase = "/workspace/.scion-volumes/" + attachmentSharedDirName
	}
	return &attachmentStaging{
		hostDir:  filepath.Join(sharedDirHostPath, ".attachments", attachmentStagingSubdir),
		agentDir: path.Join(agentBase, ".attachments", attachmentStagingSubdir),
	}
}

// stage copies the file at srcPath into the staging directory and returns the
// container-visible path of the copy. Staging is keyed by attachment ID, so a
// file already staged (re-dispatch, fan-out to several agents) is reused.
func (a *attachmentStaging) stage(srcPath, id, filename string) (string, error) {
	if err := validStagingComponent(id); err != nil {
		return "", fmt.Errorf("attachment id: %w", err)
	}
	name := filepath.Base(filename)
	if err := validStagingComponent(name); err != nil {
		return "", fmt.Errorf("attachment name: %w", err)
	}

	destDir := filepath.Join(a.hostDir, id)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	destPath := filepath.Join(destDir, name)

	// O_EXCL both prevents following a symlink planted at destPath and tells us
	// the attachment was already staged.
	dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return path.Join(a.agentDir, id, name), nil
		}
		return "", fmt.Errorf("create staged file: %w", err)
	}
	defer func() { _ = dst.Close() }()

	src, err := os.Open(srcPath)
	if err != nil {
		_ = os.Remove(destPath)
		return "", fmt.Errorf("open attachment: %w", err)
	}
	defer func() { _ = src.Close() }()

	// No LimitReader: the size ceiling is enforced at upload time, and capping
	// the copy here would silently truncate an already-accepted attachment.
	if _, err := io.Copy(dst, src); err != nil {
		_ = os.Remove(destPath)
		return "", fmt.Errorf("copy attachment: %w", err)
	}

	return path.Join(a.agentDir, id, name), nil
}

// validStagingComponent rejects path elements that could escape the staging dir.
func validStagingComponent(s string) error {
	if s == "" || s == "." || s == ".." {
		return fmt.Errorf("invalid path component %q", s)
	}
	if strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("path separator in %q", s)
	}
	return nil
}

// resolveAttachmentStaging returns the staging target for a project, or nil when
// attachments cannot be made container-visible — the project has no scratchpad
// shared dir, or no host-side directory backing it exists on this machine (a
// remote runtime broker holds the agents' volumes). Callers fall back to the
// hub-local path, which is what agents running as host processes read.
func (s *Server) resolveAttachmentStaging(ctx context.Context, projectID string) *attachmentStaging {
	hostPath, inWorkspace := s.sharedDirHostPath(ctx, projectID, attachmentSharedDirName)
	if hostPath == "" {
		return nil
	}
	return newAttachmentStaging(hostPath, inWorkspace)
}

// sharedDirHostPath resolves the host-side directory backing one of a project's
// shared dirs, along with the shared dir's in-workspace flag. It returns an
// empty path when the project does not declare the shared dir or no directory
// backing it exists on this machine.
func (s *Server) sharedDirHostPath(ctx context.Context, projectID, name string) (string, bool) {
	if projectID == "" {
		return "", false
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil || project == nil {
		return "", false
	}

	inWorkspace := false
	found := false
	for _, sd := range project.SharedDirs {
		if sd.Name == name {
			inWorkspace = sd.InWorkspace
			found = true
			break
		}
	}
	if !found {
		return "", false
	}

	// Preferred: the hub's own resolution, which follows the project's .scion
	// marker or a co-located broker's local path.
	var candidates []string
	if res, err := s.resolveSharedDirPath(ctx, project, name); err == nil && res != nil && res.Path != "" {
		candidates = append(candidates, res.Path)
	}
	// Fallback: the conventional project-configs layout, the same computation
	// the Discord and Telegram brokers use to stage their downloads.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates,
			config.SharedDirHostPath(home, project.Slug, project.ID, name))
	}

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c, inWorkspace
		}
	}
	return "", false
}
