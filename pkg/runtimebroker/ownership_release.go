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
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The server side of releasing a failed create's reservation
// (ptone/scion#5768); the store side is in ownership_abandon.go.

// ownedHold is one request's (or async launch's) in-flight hold on a
// run. AbandonCreateRun and RetireEndedCreate act only for the run's only
// holder: the Hub's same-request resend reuses the run ID, and a run
// another request is still building must not lose its reservation.
type ownedHold struct {
	s    *Server
	key  ownedRunKey
	once sync.Once
}

// holdOwnedRun takes a hold on the run (nil on a legacy Runtime Broker).
func (s *Server) holdOwnedRun(k ownedRunKey) *ownedHold {
	if s.ownership == nil {
		return nil
	}
	s.ownedHoldsMu.Lock()
	defer s.ownedHoldsMu.Unlock()
	if s.ownedHolds == nil {
		s.ownedHolds = map[ownedRunKey]int{}
	}
	s.ownedHolds[k]++
	return &ownedHold{s: s, key: k}
}

// release drops the hold and reports whether it was the run's only one.
// Only the first call drops it; later calls report false. The store calls
// it under the record's key lock, so the decision and the drop are one
// step.
func (h *ownedHold) release() bool {
	if h == nil {
		return false
	}
	sole := false
	h.once.Do(func() {
		h.s.ownedHoldsMu.Lock()
		defer h.s.ownedHoldsMu.Unlock()
		n := h.s.ownedHolds[h.key]
		sole = n == 1
		if n <= 1 {
			delete(h.s.ownedHolds, h.key)
		} else {
			h.s.ownedHolds[h.key] = n - 1
		}
	})
	return sole
}

// flatTargetType is the runtime target type of a flat instance ("" for a
// legacy Runtime Broker).
func (s *Server) flatTargetType() string {
	if s.config.FlatInstance == nil || s.config.FlatInstance.Instance.RuntimeTarget == nil {
		return ""
	}
	return s.config.FlatInstance.Instance.RuntimeTarget.Type
}

// ownedFilesProof is the instance's files proof (ownershipFilesProof).
func (s *Server) ownedFilesProof(projectID, agentName, projectPath, hubProjectID string) agent.FilesPresence {
	return ownershipFilesProof(s.flatTargetType(), projectID, agentName, projectPath, hubProjectID)
}

// OwnershipFilesProof returns the files proof the start-up retire
// (OwnershipStore.RetireAbandonedRecords) uses for the instance whose
// runtime target type is targetType.
func OwnershipFilesProof(targetType string) func(OwnershipRecord) agent.FilesPresence {
	return func(r OwnershipRecord) agent.FilesPresence {
		return ownershipFilesProof(targetType, r.ProjectID, r.AgentSlug, "", r.ProjectID)
	}
}

// ownershipFilesProof is the single files proof every release of a slug
// reservation relies on: the create-path abandon, the failure cleanups'
// removal check and the start-up retire. Only agent.FilesAbsent releases
// anything. It is agent.FilesUnknown:
//   - on any runtime target but Docker: a Kubernetes agent can keep state
//     the broker cannot see, such as an NFS home keyed by slug
//     (ptone/scion#5778);
//   - when the project directory is not known to be this project's: an
//     empty projectPath resolves only to exactly one hub-managed project
//     directory that identifies as projectID (a linked project's path is
//     not known here);
//   - for a project on the nfs workspace backend, whose export may not be
//     mounted when this runs;
//   - when agent.AgentFilesPresence cannot establish absence.
//
// agentName is the agent's slug: the Hub sends the same value as the
// agent's name, which is what its files are keyed by.
func ownershipFilesProof(targetType, projectID, agentName, projectPath, hubProjectID string) agent.FilesPresence {
	if targetType != config.RuntimeTargetTypeDocker || projectID == "" || agentName == "" {
		return agent.FilesUnknown
	}
	if projectPath == "" {
		projectPath = uniqueHubManagedProjectPath(projectID)
	}
	if projectPath == "" || !pathIdentifiesAs(projectPath, projectID) {
		return agent.FilesUnknown
	}
	projectDir := resolveDispatchProjectDir(projectPath, "")
	if projectDir == "" || dispatchUsesNFSWorkspace(projectDir) {
		return agent.FilesUnknown
	}
	return agent.AgentFilesPresence(agentName, projectPath, hubProjectID)
}

// uniqueHubManagedProjectPath returns the hub-managed project directory
// (<global>/projects/<name>) whose .scion identifies as projectID, or ""
// when none or more than one does.
func uniqueHubManagedProjectPath(projectID string) string {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return ""
	}
	base := filepath.Join(globalDir, "projects")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	found := ""
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		root := filepath.Join(base, e.Name())
		if projectIDAtPath(filepath.Join(root, config.DotScion)) != projectID {
			continue
		}
		if found != "" {
			return ""
		}
		found = root
	}
	return found
}

// ownedCreateAbandon is a flat create request's release state: filled
// when its run is reserved, consumed by the first error response
// (abandonOnErrorWriter) or by createAgent's unwinding, and disarmed once
// the request starts the agent (Manager.Start, or the async handoff). A
// request handles it on one goroutine.
type ownedCreateAbandon struct {
	armed        bool
	key          ownedRunKey
	slug, name   string
	projectPath  string
	hubProjectID string
	began        beganRun
	// mayHaveWrittenFiles is set before every step that can write an agent
	// file; until then a request that created the record wrote nothing.
	mayHaveWrittenFiles bool
	hold                *ownedHold
	// handedOff: an async launch took the run and the hold over.
	handedOff bool
}

func (a *ownedCreateAbandon) markWriting() {
	if a != nil {
		a.mayHaveWrittenFiles = true
	}
}

func (a *ownedCreateAbandon) disarm() {
	if a != nil {
		a.armed = false
	}
}

// releaseHold drops the request's hold when nothing consumed it.
func (a *ownedCreateAbandon) releaseHold() {
	if a != nil {
		a.hold.release()
	}
}

// abandonOwnedCreate releases a failed create's reservation
// (AbandonCreateRun). Absence of files is proven by the request itself
// only when it created the record and wrote no agent file; otherwise by
// ownedFilesProof, computed under the agent's files lock before the
// host-wide reservation lock is taken. The record revision read before the
// proof must still hold at the commit.
func (s *Server) abandonOwnedCreate(ctx context.Context, a *ownedCreateAbandon) {
	if s.ownership == nil || a == nil || !a.armed {
		return
	}
	a.armed = false
	wroteNothing := a.began.created && !a.mayHaveWrittenFiles
	expect := a.began.revision
	if wroteNothing {
		// Another request of the agent may have written the record (and
		// files) since: then the proof decides. AbandonCreateRun checks the
		// revision again under the key lock.
		if rec, ok, err := s.ownership.Get(a.key.projectID, a.key.agentID); err != nil || !ok || rec.Revision != a.began.revision {
			wroteNothing = false
		}
	}
	files := agent.FilesUnknown
	if !wroteNothing {
		lockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		unlock, err := s.lockAgentFiles(lockCtx, a.projectPath, a.name)
		cancel()
		if err == nil {
			defer unlock()
			if rec, ok, gerr := s.ownership.Get(a.key.projectID, a.key.agentID); gerr == nil && ok {
				expect = rec.Revision
				files = s.ownedFilesProof(a.key.projectID, a.slug, a.projectPath, a.hubProjectID)
			}
		}
	}
	l := s.locks()
	l.reserveMu.Lock()
	outcome, err := s.ownership.AbandonCreateRun(a.key.projectID, a.key.agentID, a.key.runID, wroteNothing, expect, files, a.hold.release)
	l.reserveMu.Unlock()
	switch {
	case err != nil:
		s.agentLifecycleLog.Warn("Failed create: its slug reservation could not be released",
			"agent_id", a.key.agentID, "project_id", a.key.projectID, "run_id", a.key.runID, "error", err)
	case outcome == abandonRecordDeleted:
		s.agentLifecycleLog.Info("Failed create: slug reservation released",
			"agent_id", a.key.agentID, "project_id", a.key.projectID, "run_id", a.key.runID, "slug", a.slug)
	case outcome == abandonRunRemoved:
		s.agentLifecycleLog.Info("Failed create: run removed; the slug stays reserved until the agent's files are removed by its delete",
			"agent_id", a.key.agentID, "project_id", a.key.projectID, "run_id", a.key.runID, "slug", a.slug, "files", files.String())
	}
}

// retireEndedCreate retires a failed create's record after its cleanup
// ended the run and proved its files removed (RetireEndedCreate). release
// is the caller's hold on the run, dropped in the same step.
func (s *Server) retireEndedCreate(k ownedRunKey, removed bool, hold *ownedHold) {
	if s.ownership == nil || k.runID == "" {
		return
	}
	files := agent.FilesUnknown
	if removed {
		files = agent.FilesAbsent
	}
	l := s.locks()
	l.reserveMu.Lock()
	retired, err := s.ownership.RetireEndedCreate(k.projectID, k.agentID, files, hold.release)
	l.reserveMu.Unlock()
	switch {
	case err != nil:
		s.agentLifecycleLog.Warn("Failed create: its ownership record could not be retired",
			"agent_id", k.agentID, "project_id", k.projectID, "run_id", k.runID, "error", err)
	case retired:
		s.agentLifecycleLog.Info("Failed create: ownership record retired and slug released",
			"agent_id", k.agentID, "project_id", k.projectID, "run_id", k.runID)
	}
}

// filesRemovedAfterCleanup reports whether a failed create's agent files
// are proven gone right after its cleanup removed them (removal returned no
// error): deleteAgentFiles also returns nil when it skipped something. The
// caller holds the agent's files lock.
func (s *Server) filesRemovedAfterCleanup(projectID, slug, projectPath, hubProjectID string) bool {
	return s.ownedFilesProof(projectID, slug, projectPath, hubProjectID) == agent.FilesAbsent
}

// abandonOnErrorWriter runs onError before the first error status
// (>= 400) of a flat create's response is written, so the create's
// reservation is released before the caller (the Hub) can act on the
// response.
type abandonOnErrorWriter struct {
	http.ResponseWriter
	onError func()
	wrote   bool
}

func (w *abandonOnErrorWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		if code >= http.StatusBadRequest && w.onError != nil {
			w.onError()
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *abandonOnErrorWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *abandonOnErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush passes through to the underlying writer when it can flush.
func (w *abandonOnErrorWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ownershipStartContextError maps a refused reservation to the start
// context's 409 (ptone/scion#5769). A slug held by another agent carries
// the stable code api.BrokerErrCodeAgentSlugHeld and names the holder.
func (s *Server) ownershipStartContextError(err error, slug string) *startContextError {
	msg := "The agent's ownership cannot be recorded by this Runtime Broker instance"
	instance := s.ownership.RuntimeBrokerID()
	var elsewhere *slugReservedElsewhereError
	if errors.As(err, &elsewhere) {
		instance = elsewhere.instance
	}
	var held *slugHeldError
	sce := &startContextError{Status: http.StatusConflict, OriginalErr: err}
	switch {
	case errors.Is(err, ErrOwnershipSlugPending):
		msg = fmt.Sprintf("Agent slug %q is still reserved on Runtime Broker instance %s: the cleanup of an earlier delete of this slug is not yet confirmed", slug, instance)
	case errors.As(err, &held):
		msg = fmt.Sprintf("Agent slug %q is held in this project by another agent (ID %s) of Runtime Broker instance %s. Delete that agent to free the name, or use a different name.", slug, held.holder, instance)
		if s.flatTargetType() == config.RuntimeTargetTypeDocker {
			msg += " If no agent with this name exists in the Hub and the project is hub-managed and not on nfs workspace storage, restarting the Runtime Broker server that hosts this instance may release it."
		}
		sce.Code = api.BrokerErrCodeAgentSlugHeld
		sce.Details = map[string]interface{}{"slug": slug, "holderAgentId": held.holder, "runtimeBrokerId": instance}
	case elsewhere != nil && elsewhere.readErr == nil && elsewhere.holder != "":
		msg = fmt.Sprintf("Agent slug %q is reserved in this project by agent %s of Runtime Broker instance %s of the same host", slug, elsewhere.holder, instance)
		sce.Code = api.BrokerErrCodeAgentSlugHeld
		sce.Details = map[string]interface{}{"slug": slug, "holderAgentId": elsewhere.holder, "runtimeBrokerId": instance}
	case elsewhere != nil:
		msg = fmt.Sprintf("Agent slug %q is reserved in this project by Runtime Broker instance %s of the same host", slug, instance)
	}
	sce.Message = msg
	return sce
}

// wrapOwnedCreate returns createAgent's response writer and release state
// on a flat instance (the writer unchanged and nil otherwise).
func (s *Server) wrapOwnedCreate(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *ownedCreateAbandon) {
	if s.ownership == nil {
		return w, nil
	}
	a := &ownedCreateAbandon{}
	ctx := r.Context()
	return &abandonOnErrorWriter{ResponseWriter: w, onError: func() { s.abandonOwnedCreate(ctx, a) }}, a
}

// finishOwnedCreate runs when createAgent returns or unwinds. A request
// that is still armed and wrote no response (a panic before the start)
// releases its reservation here; then the request's hold is dropped unless
// an async launch took it over.
func (s *Server) finishOwnedCreate(ctx context.Context, w http.ResponseWriter, a *ownedCreateAbandon) {
	if a == nil {
		return
	}
	if aw, ok := w.(*abandonOnErrorWriter); ok && !aw.wrote && a.armed {
		s.abandonOwnedCreate(ctx, a)
	}
	if !a.handedOff {
		a.releaseHold()
	}
}

// handOff records that an async launch took the request's run (and hold)
// over.
func (a *ownedCreateAbandon) handOff() {
	if a != nil {
		a.armed = false
		a.handedOff = true
	}
}

// testHookCreateBeforeWorkspaceDownload, when set (tests only), runs in a
// synchronous create right before its GCS workspace download, with the
// start options the download receives.
var testHookCreateBeforeWorkspaceDownload func(opts api.StartOptions)
