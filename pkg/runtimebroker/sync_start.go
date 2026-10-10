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
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// defaultSyncStartSupersedeWait bounds how long a synchronous create waits
// for an earlier start of the same agent name on this broker (one it has
// just cancelled) to finish its own failure cleanup, before giving up. It
// matches the 60s budget that cleanup itself runs under on the async path
// (cleanupAbortedLaunch).
const defaultSyncStartSupersedeWait = 60 * time.Second

// errSyncStartSupersedeTimeout is returned by beginSyncStart when an earlier
// start of the same name did not finish within the wait.
var errSyncStartSupersedeTimeout = errors.New("an earlier start of this agent is still finishing")

// errInvalidLaunchSlug is returned by beginSyncStart when the agent's slug
// (or, without one, its name) is not a single path element and so cannot
// name a launch marker file. createAgent maps it to 400.
var errInvalidLaunchSlug = errors.New("agent slug must be a single path element")

// syncStart is the bookkeeping for one synchronous (non-async) create's
// Manager.Start, mirroring what runLaunch keeps for an async launch:
//
//   - a launchRegistry record under (projectID, slug), so a delete or stop
//     of the agent on this broker cancels the start (launchRegistry
//     CancelLocal), and a later start for the same name cancels this one and
//     waits for it to finish before touching any files;
//   - a launch marker for the slug holding this start's own owner value, so
//     the start's failure cleanup can check, immediately before removing
//     files by name, that no newer start (sync or async, on any replica
//     sharing the storage) has since taken the name.
type syncStart struct {
	key             launchKey
	rec             *launchRecord
	owner           string
	projectPath     string
	sharedWorkspace bool
	registry        *launchRegistry
}

// beginSyncStart registers a synchronous start for req and writes its launch
// marker. It returns a context derived from ctx that is cancelled when the
// agent is deleted or stopped on this broker, or when a newer start for the
// same name begins; the caller runs the start under it and must call finish
// exactly once when the create ends.
//
// If an earlier start of the same name is still running here, it is
// cancelled and waited for (bounded by ctx and the supersede wait) before
// the marker is written, so that start's failure cleanup always runs either
// entirely before this start writes its marker (and so before it provisions
// anything) or, after the marker write, sees that it no longer owns the name.
func (s *Server) beginSyncStart(ctx context.Context, req CreateAgentRequest, opts api.StartOptions) (context.Context, *syncStart, error) {
	slug := req.Slug
	if slug == "" {
		slug = req.Name
	}
	// The slug is joined onto the launch markers directory below. createAgent
	// already checks Name and Slug at the request boundary; this check keeps
	// the helper safe on its own, before any registry or filesystem use.
	if !isSingleCleanPathElement(slug) {
		return nil, nil, errInvalidLaunchSlug
	}
	startCtx, cancel := context.WithCancel(ctx)
	ss := &syncStart{
		key:             launchKey{ProjectID: req.ProjectID, Slug: slug},
		owner:           "sync-" + uuid.NewString(),
		projectPath:     opts.ProjectPath,
		sharedWorkspace: opts.SharedWorkspace,
		registry:        s.launchRegistry,
	}
	ss.rec = newLaunchRecord(ss.owner, req.ID, store.LaunchKindCreate, "", time.Time{}, cancel)
	ss.rec.RunID = opts.RunID

	var supersededDone <-chan struct{}
	if ss.registry != nil {
		supersededDone = ss.registry.Begin(ss.key, ss.rec)
	}
	if supersededDone != nil {
		wait := s.syncStartSupersedeWait
		if wait <= 0 {
			wait = defaultSyncStartSupersedeWait
		}
		timer := time.NewTimer(wait)
		select {
		case <-supersededDone:
			timer.Stop()
		case <-startCtx.Done():
			timer.Stop()
			ss.finish()
			return nil, nil, startCtx.Err()
		case <-timer.C:
			ss.finish()
			return nil, nil, errSyncStartSupersedeTimeout
		}
	}

	if ss.projectPath != "" {
		if err := writeLaunchMarker(ss.projectPath, ss.sharedWorkspace, slug, ss.owner); err != nil {
			// Without the marker the failure cleanup can never confirm
			// ownership and would leave the files behind, so fail here, as
			// runLaunch does, rather than start with no way to clean up.
			ss.finish()
			return nil, nil, fmt.Errorf("failed to write launch marker: %w", err)
		}
	}
	return startCtx, ss, nil
}

// ownsName reports, via a fresh read of the slug's launch marker, whether
// this start still owns the agent name. It must be checked immediately
// before removing any files by name. Always false without a project path,
// since no marker was written and nothing name-addressed may be removed.
func (ss *syncStart) ownsName() bool {
	if ss.projectPath == "" {
		return false
	}
	return launchMarkerMatches(ss.projectPath, ss.sharedWorkspace, ss.key.Slug, ss.owner)
}

// testHookSyncStartFinish, when set (tests only), runs at the start of
// syncStart.finish.
var testHookSyncStartFinish func(key launchKey)

// finish removes this start's marker (only if it still holds this start's
// owner value), then releases the registry record, which lets a newer start
// waiting in beginSyncStart proceed. The marker goes first so its
// check-and-remove cannot remove a marker the newer start writes.
func (ss *syncStart) finish() {
	if testHookSyncStartFinish != nil {
		testHookSyncStartFinish(ss.key)
	}
	if ss.projectPath != "" {
		removeLaunchMarkerIfMatches(ss.projectPath, ss.sharedWorkspace, ss.key.Slug, ss.owner)
	}
	if ss.registry != nil {
		ss.registry.Finish(ss.key, ss.rec)
	} else {
		close(ss.rec.done)
	}
	ss.rec.cancel()
}

// cancelLocalLaunch cancels any in-flight start (sync or async) registered
// on this broker under key. A server built without a launch registry (for
// example, constructed directly in a test rather than via New) has nothing
// to cancel.
func (s *Server) cancelLocalLaunch(key launchKey) {
	if s.launchRegistry == nil {
		return
	}
	s.launchRegistry.CancelLocal(key)
}

// cancelLocalLaunchForRun is cancelLocalLaunch for a delete naming run
// runID; see launchRegistry.CancelLocalForRun.
func (s *Server) cancelLocalLaunchForRun(key launchKey, runID string) bool {
	if s.launchRegistry == nil {
		return false
	}
	return s.launchRegistry.CancelLocalForRun(key, runID)
}
