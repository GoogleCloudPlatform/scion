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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Web chat attachments are stored on the hub (the artifact service, or the
// hub-private AttachmentStore under ~/.scion/attachments), neither of which
// is mounted into agent containers. Before a message is dispatched, each
// attachment is projected into the project's "scratchpad" shared dir, the
// same tree the chat plugins and `scion message --attach` write into, so
// sibling agents can find it (ptone/scion#5827).
//
// The scratchpad is resolved through the layout authority (pkg/projectfs) on
// the hub's shared-dir Layout, so on a shared (nfs) backend the projection
// lands in the directory every agent of the project mounts, and every path
// below it is opened through Resolved.Open. Container paths come from the
// Mount's Target (projectfs.AttachmentContainerPath), never from a literal.
//
// Host side:  Resolve(scratchpad)/.attachments/_webchat/<id>/<name>
// Agent side: <scratchpad Mount.Target>/.attachments/_webchat/<id>/<name>
const (
	// attachmentSharedDirName is the shared dir web chat attachments are
	// projected into.
	attachmentSharedDirName = projectfs.ScratchpadName
	// attachmentStagingSubdir is the per-channel subdirectory under
	// .attachments, matching the _discord / _telegram plugin channels.
	attachmentStagingSubdir = projectfs.AttachmentChannelWebChat
)

// attachmentStaging copies attachments into a project's scratchpad shared
// dir, which is mounted into agent containers.
//
// Projections are swept by the artifact maintenance pass once they are older
// than messaging.attachment_inbox_retention_days (sweepAttachmentProjections).
type attachmentStaging struct {
	// resolved is the scratchpad as the layout authority resolved it; every
	// operation opens it through resolved.Open.
	resolved projectfs.Resolved
	// create creates the scratchpad leaf when it is missing (shared
	// backends: the leaf is in the tree agents mount, created with
	// pkg/shareddirs' mode and ACL).
	create bool
	// mount is the scratchpad Mount; its Target is the container path.
	mount projectfs.Mount
	// channel is the subdirectory under .attachments.
	channel string
}

// newAttachmentStaging builds a staging target for the resolved scratchpad
// of mount.
func newAttachmentStaging(resolved projectfs.Resolved, mount projectfs.Mount, create bool) *attachmentStaging {
	return &attachmentStaging{resolved: resolved, create: create, mount: mount, channel: attachmentStagingSubdir}
}

// agentPath is the container path of staged file name of attachment id.
func (a *attachmentStaging) agentPath(id, name string) (string, error) {
	return projectfs.AttachmentContainerPath(a.mount, a.channel, id, name)
}

// stageFrom copies the bytes open returns into the staging directory and
// returns the container-visible path of the copy. Staging is keyed by
// attachment ID, so a file already staged (re-dispatch, fan-out to several
// agents) is reused once verified. open is called when the file is not
// staged yet, or to verify an existing copy.
//
// The destination is resolved through an os.Root anchored on the shared dir
// (Resolved.Open). Agents mount that dir read-write, so every component
// below it is content the hub does not control: ".attachments" or the
// per-id directory can be replaced with a link, and a path-based MkdirAll
// would follow it and write the copy wherever it pointed. The root covers
// every component, and the id and name checks keep this to a single
// directory level either way.
func (a *attachmentStaging) stageFrom(open func() (io.ReadCloser, error), id, filename string) (string, error) {
	if err := validStagingComponent(id); err != nil {
		return "", fmt.Errorf("attachment id: %w", err)
	}
	name := filepath.Base(filename)
	if err := validStagingComponent(name); err != nil {
		return "", fmt.Errorf("attachment name: %w", err)
	}
	destDir, err := projectfs.AttachmentRelDir(a.channel, id)
	if err != nil {
		return "", err
	}
	agentPath, err := a.agentPath(id, name)
	if err != nil {
		return "", err
	}

	root, err := a.resolved.Open(a.create)
	if err != nil {
		return "", fmt.Errorf("open shared dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	if err := root.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	destPath := path.Join(destDir, name)

	// O_EXCL both refuses a symlink planted at destPath and tells us the
	// attachment may already be staged. An existing file is reused only when
	// it is a regular file holding exactly the attachment's bytes: agents
	// write this directory, so anything else there may have been planted.
	dst, err := root.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			if verr := verifyStagedCopy(root, destPath, open); verr != nil {
				return "", verr
			}
			return agentPath, nil
		}
		return "", fmt.Errorf("create staged file: %w", err)
	}
	defer func() { _ = dst.Close() }()

	src, err := open()
	if err != nil {
		_ = root.Remove(destPath)
		return "", fmt.Errorf("open attachment: %w", err)
	}
	defer func() { _ = src.Close() }()

	// No LimitReader: the size ceiling is enforced at upload time, and capping
	// the copy here would silently truncate an already-accepted attachment.
	if _, err := io.Copy(dst, src); err != nil {
		_ = root.Remove(destPath)
		return "", fmt.Errorf("copy attachment: %w", err)
	}

	return agentPath, nil
}

// errStagedCopyMismatch: a file already at the staging destination is not
// the attachment (not a regular file, or other bytes); it is not reused.
var errStagedCopyMismatch = errors.New("a different file is already staged under this attachment's name")

// verifyStagedCopy checks that destPath inside root is a regular file (not a
// symlink) whose bytes equal what open returns.
func verifyStagedCopy(root *os.Root, destPath string, open func() (io.ReadCloser, error)) error {
	info, err := root.Lstat(destPath)
	if err != nil {
		return fmt.Errorf("check staged file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errStagedCopyMismatch
	}
	staged, err := root.Open(destPath)
	if err != nil {
		return fmt.Errorf("open staged file: %w", err)
	}
	defer func() { _ = staged.Close() }()
	if fi, err := staged.Stat(); err != nil || !os.SameFile(fi, info) {
		return errStagedCopyMismatch
	}
	src, err := open()
	if err != nil {
		return fmt.Errorf("open attachment: %w", err)
	}
	defer func() { _ = src.Close() }()
	same, err := sameContent(staged, src)
	if err != nil {
		return fmt.Errorf("compare staged file: %w", err)
	}
	if !same {
		return errStagedCopyMismatch
	}
	return nil
}

// sameContent reports whether a and b yield the same bytes.
func sameContent(a, b io.Reader) (bool, error) {
	const chunk = 32 << 10
	bufA, bufB := make([]byte, chunk), make([]byte, chunk)
	for {
		na, errA := io.ReadFull(a, bufA)
		nb, errB := io.ReadFull(b, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		endA := errors.Is(errA, io.EOF) || errors.Is(errA, io.ErrUnexpectedEOF)
		endB := errors.Is(errB, io.EOF) || errors.Is(errB, io.ErrUnexpectedEOF)
		if errA != nil && !endA {
			return false, errA
		}
		if errB != nil && !endB {
			return false, errB
		}
		if endA || endB {
			return endA && endB, nil
		}
	}
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

// resolveAttachmentStaging returns the staging target for a project, or nil
// when attachments cannot be made container-visible (resolveScratchpad).
// Callers fall back to the hub-local path, which is what agents running as
// host processes read.
func (s *Server) resolveAttachmentStaging(ctx context.Context, projectID string) *attachmentStaging {
	sp := s.resolveScratchpad(ctx, projectID)
	if sp == nil {
		return nil
	}
	return newAttachmentStaging(sp.resolution.Resolved, sp.mount, sp.resolution.onSharedBackend())
}

// scratchpadResolveErrorLogged and scratchpadEphemeralLogged each dedup
// their own log line in resolveScratchpad, once per project: the function
// can run once per message dispatch, and logging every call would spam the
// hub log for an actively-used project.
var (
	scratchpadResolveErrorLogged sync.Map // key: project ID (Warn: shared storage resolution failed)
	scratchpadEphemeralLogged    sync.Map // key: project ID (Info: scratchpad on ephemeral disk)
)

// resolvedScratchpad is a project's scratchpad as the hub reaches it.
type resolvedScratchpad struct {
	project    *store.Project
	mount      projectfs.Mount
	resolution *sharedDirResolution
}

// resolveScratchpad resolves a project's scratchpad shared dir through the
// layout authority (resolveSharedDirMount on the hub's shared-dir Layout)
// for attachment projection and agent --attach ingest. It returns nil when
//   - the project does not exist or does not declare a scratchpad;
//   - resolution fails (a shared-storage failure is logged once per project
//     and never falls back to a local directory);
//   - the scratchpad is on local disk on a Cloud Run Service, which no agent
//     mounts and which an instance recycle discards;
//   - on the local backend, no directory backs it on this host (a remote
//     runtime broker holds the agents' volumes); nothing is vivified there.
//
// On a shared (nfs) backend the scratchpad is the directory every agent of
// the project mounts; a missing leaf is created by the first projection
// (Resolved.Open with create).
func (s *Server) resolveScratchpad(ctx context.Context, projectID string) *resolvedScratchpad {
	if projectID == "" {
		return nil
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil || project == nil {
		return nil
	}
	return s.resolveProjectScratchpad(ctx, project)
}

// resolveProjectScratchpad is resolveScratchpad for a project the caller
// already holds.
func (s *Server) resolveProjectScratchpad(ctx context.Context, project *store.Project) *resolvedScratchpad {
	projectID := project.ID
	mount, err := projectfs.DeclaredSharedDir(project.SharedDirs, attachmentSharedDirName, "")
	if err != nil {
		s.projectsLogger().DebugContext(ctx, "resolveScratchpad: project has no scratchpad shared dir",
			"project_id", projectID)
		return nil
	}
	res, err := s.resolveSharedDirMount(ctx, project, mount)
	if err != nil {
		var storageErr *sharedDirStorageResolveError
		if errors.As(err, &storageErr) {
			if _, logged := scratchpadResolveErrorLogged.LoadOrStore(projectID, struct{}{}); !logged {
				s.projectsLogger().WarnContext(ctx, "resolveScratchpad: shared dir storage resolution failed; refusing to fall back to a local directory",
					"project_id", projectID, "error", err)
			}
			return nil
		}
		s.projectsLogger().DebugContext(ctx, "resolveScratchpad: scratchpad not reachable from the hub",
			"project_id", projectID, "error", err)
		return nil
	}
	if !res.Resolved.Backend.Durable() && cloudRunRequiresDurableWrites() {
		if _, logged := scratchpadEphemeralLogged.LoadOrStore(projectID, struct{}{}); !logged {
			s.projectsLogger().InfoContext(ctx, "resolveScratchpad: the scratchpad resolves to this instance's ephemeral disk; attachments are not projected",
				"project_id", projectID)
		}
		return nil
	}
	if !res.onSharedBackend() {
		if info, err := os.Stat(res.Path); err != nil || !info.IsDir() {
			s.projectsLogger().DebugContext(ctx, "resolveScratchpad: no scratchpad directory on this host",
				"project_id", projectID)
			return nil
		}
	}
	return &resolvedScratchpad{project: project, mount: mount, resolution: res}
}

// projectionAudienceIsProject reports whether a message on thread key of
// project projectID is read by the whole project, so that projecting its
// attachments into the project's scratchpad (readable by every agent that
// mounts it) widens nothing: true only for a project topic thread of that
// project. Direct conversations ("dm:" keys) and group conversations, whose
// readers are their participants, and any lookup failure answer false.
func (s *Server) projectionAudienceIsProject(ctx context.Context, key, projectID string) bool {
	if key == "" || projectID == "" || strings.HasPrefix(key, "dm:") {
		return false
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return false
	}
	topic, err := wcs.GetTopic(ctx, key)
	if err != nil || topic == nil {
		return false
	}
	return topic.ProjectID == projectID
}
