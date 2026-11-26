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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
)

// moveBundleSettings holds test-overridable dependencies and timings for the
// move-bundle broker endpoints. Zero values take production defaults.
type moveBundleSettings struct {
	client            *http.Client
	fetchUploadURL    func(ctx context.Context, hubConn, agentID, rid string, req *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error)
	fetchDownloadURLs func(ctx context.Context, hubConn, agentID, rid string, req *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error)
	sleep             func(ctx context.Context, d time.Duration) error
	now               func() time.Time
	urlBackoff        []time.Duration
	partBackoff       []time.Duration
	scratchDir        string
	jobTTL            time.Duration
	// durabilityRuntime, when non-empty (tests only), overrides the runtime
	// name checked by precheck durability.
	durabilityRuntime string
}

func (st moveBundleSettings) resolved(s *Server) moveBundleSettings {
	if st.client == nil {
		st.client = defaultSignedDownloadClient
	}
	if st.fetchUploadURL == nil {
		st.fetchUploadURL = s.fetchMoveBundleUploadURL
	}
	if st.fetchDownloadURLs == nil {
		st.fetchDownloadURLs = s.fetchMoveBundleDownloadURLs
	}
	if st.sleep == nil {
		st.sleep = sleepCtx
	}
	if st.now == nil {
		st.now = time.Now
	}
	if st.urlBackoff == nil {
		st.urlBackoff = defaultSignedPageBackoff
	}
	if st.partBackoff == nil {
		st.partBackoff = defaultSignedFileBackoff
	}
	if st.jobTTL <= 0 {
		st.jobTTL = defaultMoveBundleJobTTL
	}
	return st
}

func (s *Server) moveBundleRegistry() *moveBundleJobRegistry {
	s.moveBundleOnce.Do(func() {
		st := s.moveBundle.resolved(s)
		s.moveBundleJobs.Store(newMoveBundleJobRegistry(st.now, st.jobTTL))
	})
	return s.moveBundleJobs.Load()
}

func (s *Server) moveBundleScratchBase() string {
	if s.moveBundle.scratchDir != "" {
		return s.moveBundle.scratchDir
	}
	if s.stateDir != "" {
		dir := filepath.Join(s.stateDir, "move-bundle-scratch")
		if err := os.MkdirAll(dir, 0o700); err == nil {
			return dir
		}
	}
	return ""
}

// sweepMoveBundleScratch removes any stale per-job scratch directories left
// under <stateDir>/move-bundle-scratch/ from a prior broker crash. No
// move-bundle job survives a broker restart, so nothing there is live.
func (s *Server) sweepMoveBundleScratch() {
	if s.stateDir == "" {
		return
	}
	base := filepath.Join(s.stateDir, "move-bundle-scratch")
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if rmErr := os.RemoveAll(filepath.Join(base, entry.Name())); rmErr != nil && s.agentLifecycleLog != nil {
			s.agentLifecycleLog.Warn("Move-bundle startup scratch sweep failed", "entry", entry.Name(), "error", rmErr)
		}
	}
}

// validMoveBundleRID checks that rid is a single clean path segment suitable
// for use in URLs and the ".scion-move-bundle-<rid>" marker filename.
func validMoveBundleRID(rid string) bool {
	if rid == "" || len(rid) > 128 || !isSingleCleanPathElement(rid) {
		return false
	}
	if strings.HasPrefix(rid, ".") {
		return false
	}
	for _, c := range rid {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

// handleMoveBundle routes /api/v1/agents/{id}/move-bundle/{subaction}:
//   - POST .../move-bundle/precheck
//   - POST .../move-bundle/export
//   - GET  .../move-bundle/export/{rid}
//   - POST .../move-bundle/import
//   - GET  .../move-bundle/import/{rid}
func (s *Server) handleMoveBundle(w http.ResponseWriter, r *http.Request, id, projectID, subaction string) {
	switch {
	case subaction == "precheck":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleMoveBundlePrecheck(w, r, id, projectID)
	case subaction == "export":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleMoveBundleExportStart(w, r, id, projectID)
	case strings.HasPrefix(subaction, "export/"):
		if r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet)
			return
		}
		rid := strings.TrimPrefix(subaction, "export/")
		s.handleMoveBundleJobStatus(w, r, id, projectID, api.MoveBundleJobKindExport, rid)
	case subaction == "import":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleMoveBundleImportStart(w, r, id, projectID)
	case strings.HasPrefix(subaction, "import/"):
		if r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet)
			return
		}
		rid := strings.TrimPrefix(subaction, "import/")
		s.handleMoveBundleJobStatus(w, r, id, projectID, api.MoveBundleJobKindImport, rid)
	default:
		NotFound(w, "MoveBundleAction")
	}
}

// moveBundleDirs holds the resolved host paths for an agent's bundle roots.
type moveBundleDirs = agent.MoveBundleAgentDirs

func (s *Server) moveBundleRuntimeInput(runtimeName, agentSlug, agentID, projectID, projectPath, projectSlug string) (agent.MoveBundleRuntimeInput, error) {
	if !isSingleCleanPathElement(agentSlug) {
		return agent.MoveBundleRuntimeInput{}, fmt.Errorf("invalid agent id")
	}
	if projectSlug != "" && !isSingleCleanPathElement(projectSlug) {
		return agent.MoveBundleRuntimeInput{}, fmt.Errorf("invalid projectSlug")
	}

	var rawProject string
	switch {
	case projectPath != "":
		rawProject = projectPath
	case projectSlug != "":
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return agent.MoveBundleRuntimeInput{}, fmt.Errorf("resolve global dir: %w", err)
		}
		rawProject = filepath.Join(globalDir, "projects", projectSlug)
	default:
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return agent.MoveBundleRuntimeInput{}, fmt.Errorf("resolve global dir: %w", err)
		}
		rawProject = globalDir
	}

	return agent.MoveBundleRuntimeInput{
		RuntimeName: runtimeName,
		AgentSlug:   agentSlug,
		AgentID:     agentID,
		ProjectPath: rawProject,
		ProjectID:   projectID,
		BrokerMode:  true,
	}, nil
}

func (s *Server) resolveMoveBundleDirs(ctx context.Context, runtimeName, agentSlug, agentID, projectID, projectPath, projectSlug string) (moveBundleDirs, error) {
	in, err := s.moveBundleRuntimeInput(runtimeName, agentSlug, agentID, projectID, projectPath, projectSlug)
	if err != nil {
		return moveBundleDirs{}, err
	}
	return agent.ResolveMoveBundleDirsForRuntime(ctx, in)
}

// isMoveBundleAgentRunning reports whether the runtime currently lists the
// agent in a running/starting state, so export can refuse to snapshot a live
// container.
var errMoveBundleNoManager = errors.New("no runtime manager available to verify agent is stopped")

// isMoveBundleAgentRunning reports whether the named agent currently has a
// running/starting container on this broker. Fails closed with
// errMoveBundleNoManager if no runtime manager resolves.
func (s *Server) isMoveBundleAgentRunning(ctx context.Context, id, projectID string) (bool, error) {
	mgr := s.resolveManagerForAgent(ctx, id, projectID)
	if mgr == nil {
		return false, errMoveBundleNoManager
	}
	agents, err := mgr.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		return false, err
	}
	for _, a := range agents {
		if !matchesAgent(a, id, projectID) {
			continue
		}
		phase := strings.ToLower(strings.TrimSpace(a.Phase))
		status := strings.ToLower(strings.TrimSpace(a.ContainerStatus))
		if phase == "running" || phase == "starting" || phase == "cloning" ||
			strings.HasPrefix(status, "up") || status == "running" {
			return true, nil
		}
	}
	return false, nil
}

// isGitWorktreeWorkspace reports whether wsDir contains a `.git` regular file
// (a git worktree pointer referencing a host gitdir). Phase 1 bundle export
// refuses worktree workspaces so broken host gitdir pointers never travel.
func isGitWorktreeWorkspace(wsDir string) bool {
	root, err := os.OpenRoot(wsDir)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(".git")
	return err == nil && fi.Mode().IsRegular()
}

// resolveMoveBundleRuntimeName determines the runtime type name for durability
// checking on this broker.
func (s *Server) resolveMoveBundleRuntimeName(r *http.Request, id, projectID string) string {
	if s.moveBundle.durabilityRuntime != "" {
		return s.moveBundle.durabilityRuntime
	}
	if recorded := strings.TrimSpace(r.URL.Query().Get(api.RecordedRuntimeQueryParam)); recorded != "" {
		return recorded
	}
	if rt := s.resolveRuntimeForAgent(r.Context(), id, projectID); rt != nil {
		return rt.Name()
	}
	if _, rt, _ := s.defaultPair(); rt != nil {
		return rt.Name()
	}
	return ""
}

// moveBundleHomeRecordMissingReason is the user-facing refusal reason returned
// when an agent lacks the broker-owned move-bundle home record written at start.
const moveBundleHomeRecordMissingReason = "restart the agent once to enable bundle moves"

// loadMoveBundleExcluders builds both the workspace and $HOME excluders from
// the broker-owned start record (<agentDir>/.scion-move-bundle-home.json)
// unioned with any hub-supplied file-secret targets. It fails closed whenever
// the record is missing or incomplete, and expands targets directly against
// the record's resolved HomeAbs and WorkspaceAbs.
func loadMoveBundleExcluders(agentDir string, hubSecretTargets []string) (bundle.Excluder, bundle.Excluder, error) {
	rec, err := agent.ReadMoveBundleHomeRecord(agentDir)
	if err != nil {
		return nil, nil, err
	}
	seenHome := make(map[string]struct{}, len(rec.HomeExcludes)+len(hubSecretTargets))
	var homeLiterals []string
	addHome := func(rel string) {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			return
		}
		if _, ok := seenHome[rel]; ok {
			return
		}
		seenHome[rel] = struct{}{}
		homeLiterals = append(homeLiterals, rel)
	}
	for _, rel := range rec.HomeExcludes {
		addHome(rel)
	}
	for _, rel := range scionrt.PortableHomeExcludes(scionrt.RunConfig{UnixUsername: rec.UnixUsername}) {
		addHome(rel)
	}

	seenWS := make(map[string]struct{}, len(rec.WorkspaceExcludes)+len(hubSecretTargets))
	var wsLiterals []string
	addWS := func(rel string) {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			return
		}
		if _, ok := seenWS[rel]; ok {
			return
		}
		seenWS[rel] = struct{}{}
		wsLiterals = append(wsLiterals, rel)
	}
	for _, rel := range rec.WorkspaceExcludes {
		addWS(rel)
	}

	for _, target := range hubSecretTargets {
		if rel, ok := agent.ExpandHomeRelTarget(target, rec.HomeAbs); ok {
			addHome(rel)
		}
		if rel, ok := agent.ExpandAbsRelTarget(target, rec.WorkspaceAbs); ok {
			addWS(rel)
		}
	}

	wsEx := bundle.WorkspaceExcluderWithOptions(bundle.WorkspaceExcluderOptions{
		ExtraLiterals: wsLiterals,
		WorkspaceAbs:  rec.WorkspaceAbs,
	})
	homeEx := bundle.HomeExcluder(bundle.HomeExcluderOptions{
		ExtraLiterals: homeLiterals,
		TempNamePred:  scionrt.IsHomeTempName,
		AgentHomeAbs:  rec.HomeAbs,
	})
	return wsEx, homeEx, nil
}

// loadMoveBundleHomeExcluder is a convenience wrapper around
// loadMoveBundleExcluders returning only the $HOME excluder.
func loadMoveBundleHomeExcluder(agentDir string, hubSecretTargets []string) (bundle.Excluder, error) {
	_, homeEx, err := loadMoveBundleExcluders(agentDir, hubSecretTargets)
	return homeEx, err
}

// precheckUnportableReason maps a bundle.Estimate failure to a class-only
// user-facing refusal reason containing no host paths and no symlink targets.
func precheckUnportableReason(rootName string, err error) string {
	var unportablePath *bundle.ErrUnportablePath
	if errors.As(err, &unportablePath) {
		return fmt.Sprintf("source %s contains an unsupported path", rootName)
	}
	var unportableHome *bundle.ErrUnportableHome
	if errors.As(err, &unportableHome) {
		return fmt.Sprintf("source %s contains an unportable symlink or hardlink", rootName)
	}
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Sprintf("broker lacks read permission on source %s", rootName)
	}
	return fmt.Sprintf("source %s cannot be bundled", rootName)
}

func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, dst)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// handleMoveBundlePrecheck implements POST /api/v1/agents/{id}/move-bundle/precheck.
func (s *Server) handleMoveBundlePrecheck(w http.ResponseWriter, r *http.Request, id, projectID string) {
	var req api.MoveBundlePrecheckRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}
	if err := req.Limits.Validate(); err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	if projectID != "" && req.ProjectID != "" && projectID != req.ProjectID {
		ValidationError(w, "projectId query parameter does not match request body", nil)
		return
	}
	effProjectID := firstNonEmpty(projectID, req.ProjectID)
	rtName := s.resolveMoveBundleRuntimeName(r, id, effProjectID)
	agentUUID := firstNonEmpty(req.AgentID, r.URL.Query().Get(api.AgentIDQueryParam))
	projectPath := firstNonEmpty(req.ProjectPath, r.URL.Query().Get("projectPath"))
	projectSlug := firstNonEmpty(req.ProjectSlug, r.URL.Query().Get("projectSlug"))

	rtIn, err := s.moveBundleRuntimeInput(rtName, id, agentUUID, effProjectID, projectPath, projectSlug)
	if err != nil {
		s.agentLifecycleLog.Warn("Move-bundle precheck: failed to resolve agent directories",
			"agent_id", id, "error", err)
		ValidationError(w, "cannot resolve agent directories", nil)
		return
	}
	dur, durErr := agent.CheckMoveBundleSourceDurability(r.Context(), rtIn)
	if durErr != nil {
		if errors.Is(durErr, fs.ErrNotExist) {
			NotFound(w, "Agent")
			return
		}
		s.agentLifecycleLog.Warn("Move-bundle precheck: durability check failed",
			"agent_id", id, "error", durErr)
		ValidationError(w, "cannot resolve agent directories", nil)
		return
	}
	if !dur.Durable {
		writeJSON(w, http.StatusOK, api.MoveBundlePrecheckResponse{
			Durable: false,
			Reason:  dur.Reason,
		})
		return
	}
	if reg := s.moveBundleJobs.Load(); reg != nil && reg.hasRunningForAgent(effProjectID, id) {
		writeJSON(w, http.StatusOK, api.MoveBundlePrecheckResponse{
			Durable: false,
			Reason:  "a previous move-bundle job is still running",
		})
		return
	}

	dirs, err := agent.ResolveMoveBundleDirsForRuntime(r.Context(), rtIn)
	if err != nil {
		s.agentLifecycleLog.Warn("Move-bundle precheck: failed to resolve agent directories",
			"agent_id", id, "error", err)
		ValidationError(w, "cannot resolve agent directories", nil)
		return
	}
	if dirs.SharedWorkspace {
		writeJSON(w, http.StatusOK, api.MoveBundlePrecheckResponse{
			Durable: false,
			Reason:  "shared layout workspace is not yet supported for bundle moves",
		})
		return
	}
	if _, statErr := os.Stat(dirs.AgentDir); errors.Is(statErr, fs.ErrNotExist) {
		NotFound(w, "Agent")
		return
	}
	if isGitWorktreeWorkspace(dirs.WorkspaceDir) {
		writeJSON(w, http.StatusOK, api.MoveBundlePrecheckResponse{
			Durable: false,
			Reason:  "worktree workspace is not supported for bundle moves",
		})
		return
	}

	wsEx, homeEx, exErr := loadMoveBundleExcluders(dirs.AgentDir, req.SecretFileTargets)
	if exErr != nil {
		s.agentLifecycleLog.Warn("Move-bundle precheck: home exclusion record unavailable",
			"agent_id", id, "error", exErr)
		writeJSON(w, http.StatusOK, api.MoveBundlePrecheckResponse{
			Durable: false,
			Reason:  moveBundleHomeRecordMissingReason,
		})
		return
	}

	wsEst, wsErr := estimateMoveBundleRoot(r.Context(), dirs.WorkspaceDir, wsEx, req.Limits.WorkspaceLimits(), false, 0)
	if wsErr != nil {
		if errors.Is(wsErr, fs.ErrNotExist) {
			NotFound(w, "Agent workspace")
			return
		}
		if errors.Is(wsErr, context.Canceled) || errors.Is(wsErr, context.DeadlineExceeded) {
			InternalError(w)
			return
		}
		s.agentLifecycleLog.Warn("Move-bundle precheck: workspace cannot be bundled",
			"agent_id", id, "error", wsErr)
		writeJSON(w, http.StatusOK, api.MoveBundlePrecheckResponse{
			Durable: false,
			Reason:  precheckUnportableReason(bundle.RootWorkspace, wsErr),
		})
		return
	}

	resp := api.MoveBundlePrecheckResponse{
		Durable:   true,
		Workspace: wsEst,
	}
	if wsEst.Over != "" {
		resp.Over = bundle.RootWorkspace + "." + wsEst.Over
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Probe readability during the bounded, excluder-filtered Estimate walk
	// when home lives on a shared/NFS export with a distinct pod owner (e.g.
	// Kubernetes NFS home, R2), so excluded caches are skipped and entry/byte
	// limits bound the walk.
	checkHomeReadable := dirs.HomeOwner.UID > 0
	homeEst, homeErr := estimateMoveBundleRoot(r.Context(), dirs.HomeDir, homeEx, req.Limits.HomeLimits(), checkHomeReadable, dirs.HomeOwner.UID)
	if homeErr != nil {
		if errors.Is(homeErr, fs.ErrNotExist) {
			NotFound(w, "Agent home")
			return
		}
		if errors.Is(homeErr, context.Canceled) || errors.Is(homeErr, context.DeadlineExceeded) {
			InternalError(w)
			return
		}
		s.agentLifecycleLog.Warn("Move-bundle precheck: home cannot be bundled",
			"agent_id", id, "error", homeErr)
		writeJSON(w, http.StatusOK, api.MoveBundlePrecheckResponse{
			Durable: false,
			Reason:  precheckUnportableReason(bundle.RootHome, homeErr),
		})
		return
	}
	resp.Home = homeEst
	if homeEst.Over != "" {
		resp.Over = bundle.RootHome + "." + homeEst.Over
	}

	writeJSON(w, http.StatusOK, resp)
}

func estimateMoveBundleRoot(ctx context.Context, dir string, ex bundle.Excluder, lim bundle.Limits, checkReadable bool, expectedOwnerUID int) (api.MoveBundleRootEstimate, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return api.MoveBundleRootEstimate{}, err
	}
	defer func() { _ = root.Close() }()

	res, err := bundle.EstimateWithOptions(ctx, root, ex, lim, bundle.EstimateOptions{
		CheckReadable:    checkReadable,
		ExpectedOwnerUID: expectedOwnerUID,
	})
	est := api.MoveBundleRootEstimate{
		Bytes:   res.Bytes,
		Entries: res.Entries,
	}
	if err != nil {
		var limErr *bundle.ErrLimitExceeded
		if errors.As(err, &limErr) {
			est.Over = limErr.Field
			return est, nil
		}
		return est, err
	}
	return est, nil
}

// handleMoveBundleJobStatus implements GET /api/v1/agents/{id}/move-bundle/{export|import}/{rid}.
func (s *Server) handleMoveBundleJobStatus(w http.ResponseWriter, r *http.Request, id, projectID, kind, rid string) {
	if !validMoveBundleRID(rid) {
		ValidationError(w, "invalid reincarnationId", nil)
		return
	}
	agentKey := moveBundleAgentKey{ProjectID: projectID, AgentSlug: id, Kind: kind}
	job, ok := s.moveBundleRegistry().get(agentKey, rid)
	if !ok {
		NotFound(w, "MoveBundleJob")
		return
	}
	writeJSON(w, http.StatusOK, job.snapshot())
}

// handleMoveBundleExportStart implements POST /api/v1/agents/{id}/move-bundle/export.
func (s *Server) handleMoveBundleExportStart(w http.ResponseWriter, r *http.Request, id, projectID string) {
	var req api.MoveBundleExportRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}
	if !validMoveBundleRID(req.ReincarnationID) {
		ValidationError(w, "invalid or missing reincarnationId", nil)
		return
	}
	if err := req.Limits.Validate(); err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	if projectID != "" && req.ProjectID != "" && projectID != req.ProjectID {
		ValidationError(w, "projectId query parameter does not match request body", nil)
		return
	}
	effProjectID := firstNonEmpty(projectID, req.ProjectID)
	rtName := s.resolveMoveBundleRuntimeName(r, id, effProjectID)
	reqAgentID := firstNonEmpty(req.AgentID, r.URL.Query().Get(api.AgentIDQueryParam))
	projectPath := firstNonEmpty(req.ProjectPath, r.URL.Query().Get("projectPath"))
	projectSlug := firstNonEmpty(req.ProjectSlug, r.URL.Query().Get("projectSlug"))

	rtIn, err := s.moveBundleRuntimeInput(rtName, id, reqAgentID, effProjectID, projectPath, projectSlug)
	if err != nil {
		s.agentLifecycleLog.Warn("Move-bundle export: failed to resolve agent directories",
			"agent_id", id, "error", err)
		ValidationError(w, "cannot resolve agent directories", nil)
		return
	}
	dur, durErr := agent.CheckMoveBundleSourceDurability(r.Context(), rtIn)
	if durErr != nil {
		if errors.Is(durErr, fs.ErrNotExist) {
			NotFound(w, "Agent")
			return
		}
		s.agentLifecycleLog.Warn("Move-bundle export: durability check failed",
			"agent_id", id, "error", durErr)
		ValidationError(w, "cannot resolve agent directories", nil)
		return
	}
	if !dur.Durable {
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleUnsupported, dur.Reason, nil)
		return
	}

	running, runCheckErr := s.isMoveBundleAgentRunning(r.Context(), id, effProjectID)
	if runCheckErr != nil {
		if errors.Is(runCheckErr, errMoveBundleNoManager) {
			writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleJobConflict, "cannot verify agent is stopped: no runtime manager available", nil)
			return
		}
		s.agentLifecycleLog.Warn("Move-bundle export: failed to verify agent is stopped",
			"agent_id", id, "error", runCheckErr)
		InternalError(w)
		return
	}
	if running {
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleJobConflict, "agent must be stopped before bundle export", nil)
		return
	}

	dirs, err := agent.ResolveMoveBundleDirsForRuntime(r.Context(), rtIn)
	if err != nil {
		s.agentLifecycleLog.Warn("Move-bundle export: failed to resolve agent directories",
			"agent_id", id, "error", err)
		ValidationError(w, "cannot resolve agent directories", nil)
		return
	}
	if dirs.SharedWorkspace {
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleUnsupported, "shared layout workspace is not yet supported for bundle moves", nil)
		return
	}
	if isGitWorktreeWorkspace(dirs.WorkspaceDir) {
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleUnsupported, "worktree workspace is not supported for bundle moves", nil)
		return
	}

	wsEx, homeEx, exErr := loadMoveBundleExcluders(dirs.AgentDir, req.SecretFileTargets)
	if exErr != nil {
		s.agentLifecycleLog.Warn("Move-bundle export refused: home exclusion record unavailable",
			"agent_id", id, "error", exErr)
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleHomeRecordMissing, moveBundleHomeRecordMissingReason, nil)
		return
	}

	hubConn := s.resolveHubNameForLaunch(r)
	agentUUID := firstNonEmpty(req.AgentID, r.URL.Query().Get(api.AgentIDQueryParam), id)

	jobCtx, cancel := context.WithTimeout(context.Background(), req.Limits.Timeout())
	// Key jobs by query projectID so POST, GET status, and DELETE cancel always
	// resolve the exact same registry key.
	agentKey := moveBundleAgentKey{ProjectID: projectID, AgentSlug: id, Kind: api.MoveBundleJobKindExport}
	job, conflict, created := s.moveBundleRegistry().start(agentKey, req.ReincarnationID, cancel)
	if conflict {
		cancel()
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleJobConflict, "another move-bundle export job is already running for this agent", nil)
		return
	}
	if !created {
		cancel()
		writeJSON(w, http.StatusAccepted, job.snapshot())
		return
	}

	go s.runMoveBundleExport(jobCtx, cancel, job, hubConn, agentUUID, dirs, wsEx, homeEx, req)
	writeJSON(w, http.StatusAccepted, job.snapshot())
}

func (s *Server) runMoveBundleExport(
	ctx context.Context,
	cancel context.CancelFunc,
	job *moveBundleJob,
	hubConn string,
	agentID string,
	dirs moveBundleDirs,
	wsEx bundle.Excluder,
	homeEx bundle.Excluder,
	req api.MoveBundleExportRequest,
) {
	defer cancel()
	st := s.moveBundle.resolved(s)

	scratchDir, err := os.MkdirTemp(s.moveBundleScratchBase(), "mb-export-*")
	if err != nil {
		job.markFailed("bundle export failed: create scratch dir", "", st.now())
		return
	}
	defer func() { _ = os.RemoveAll(scratchDir) }()

	stallTimeout := req.Limits.PartStallTimeout()
	type rootResult struct {
		name string
		rm   bundle.RootManifest
		err  error
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	type openRootSpec struct {
		name string
		root *os.Root
		ex   bundle.Excluder
		lim  bundle.Limits
	}
	specs := []struct {
		name string
		dir  string
		ex   bundle.Excluder
		lim  bundle.Limits
	}{
		{bundle.RootWorkspace, dirs.WorkspaceDir, wsEx, req.Limits.WorkspaceLimits()},
		{bundle.RootHome, dirs.HomeDir, homeEx, req.Limits.HomeLimits()},
	}

	// Open both roots before launching any goroutine so a missing root fails
	// deterministically and never wastes a signed-URL fetch for the other root.
	opened := make([]openRootSpec, 0, len(specs))
	defer func() {
		for _, o := range opened {
			_ = o.root.Close()
		}
	}()
	for _, sp := range specs {
		root, openErr := os.OpenRoot(sp.dir)
		if openErr != nil {
			wrapped := fmt.Errorf("open %s root: %w", sp.name, openErr)
			s.agentLifecycleLog.Warn("Move-bundle export job failed",
				"agent_id", agentID, "reincarnation_id", req.ReincarnationID, "error", wrapped)
			job.markFailed(formatMoveBundleError("bundle export failed", wrapped), "", st.now())
			return
		}
		opened = append(opened, openRootSpec{name: sp.name, root: root, ex: sp.ex, lim: sp.lim})
	}

	results := make(chan rootResult, len(opened))
	var wg sync.WaitGroup
	for _, rt := range opened {
		wg.Add(1)
		go func(spec openRootSpec) {
			defer wg.Done()
			sink := &hubSignedPartSink{
				st:           st,
				hubConn:      hubConn,
				agentID:      agentID,
				rid:          req.ReincarnationID,
				rootName:     spec.name,
				stallTimeout: stallTimeout,
				job:          job,
			}
			rm, writeErr := bundle.Write(runCtx, spec.root, scratchDir, spec.ex, spec.lim, sink)
			if writeErr != nil {
				results <- rootResult{name: spec.name, err: &moveBundleRootError{root: spec.name, err: writeErr}}
				runCancel()
				return
			}
			results <- rootResult{name: spec.name, rm: rm}
		}(rt)
	}
	wg.Wait()
	close(results)

	manifest := bundle.Manifest{
		Version:         bundle.ManifestVersion,
		ReincarnationID: req.ReincarnationID,
		Roots:           make(map[string]bundle.RootManifest, 2),
	}
	var firstErr error
	for r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
			continue
		}
		if r.err == nil {
			manifest.Roots[r.name] = r.rm
		}
	}
	if firstErr != nil {
		s.agentLifecycleLog.Warn("Move-bundle export job failed",
			"agent_id", agentID, "reincarnation_id", req.ReincarnationID, "error", firstErr)
		job.markFailed(formatMoveBundleError("bundle export failed", firstErr), "", st.now())
		return
	}

	job.markSucceeded(&manifest, nil, st.now())
}

// hubSignedPartSink implements bundle.PartSink by asking the Hub for a
// per-part signed PUT URL and uploading the compressed part with retries and
// URL redaction.
type hubSignedPartSink struct {
	st           moveBundleSettings
	hubConn      string
	agentID      string
	rid          string
	rootName     string
	stallTimeout time.Duration
	job          *moveBundleJob
}

func (p *hubSignedPartSink) Put(ctx context.Context, index int, part io.ReadSeeker, size int64, hash string) error {
	urlResp, err := fetchUploadURLWithRetry(ctx, p.st, p.hubConn, p.agentID, p.rid, p.rootName, index, size, hash)
	if err != nil {
		return err
	}
	if err := signedPutPart(
		ctx, p.st.client, urlResp.URL, urlResp.Headers, part, size,
		p.rootName, index, p.stallTimeout, p.st.partBackoff, p.st.sleep,
	); err != nil {
		return err
	}
	p.job.addProgress(size, 1)
	return nil
}

func fetchUploadURLWithRetry(
	ctx context.Context,
	st moveBundleSettings,
	hubConn, agentID, rid, rootName string,
	partIndex int,
	size int64,
	hash string,
) (*api.MoveBundleUploadURLResponse, error) {
	req := &api.MoveBundleUploadURLRequest{
		Root:      rootName,
		PartIndex: partIndex,
		Size:      size,
		Hash:      hash,
	}
	for attempt := 0; ; attempt++ {
		resp, err := st.fetchUploadURL(ctx, hubConn, agentID, rid, req)
		if err == nil {
			if resp == nil || resp.URL == "" {
				return nil, &signedBundleTransferError{Stage: "upload_url", Root: rootName, PartIndex: partIndex, Class: signedClassDecode}
			}
			return resp, nil
		}
		mapped, retryable := mapBundleURLError("upload_url", rootName, partIndex, err)
		if !retryable || ctx.Err() != nil || attempt >= len(st.urlBackoff) {
			if cerr := ctx.Err(); cerr != nil {
				return nil, &signedBundleTransferError{Stage: "upload_url", Root: rootName, PartIndex: partIndex, Class: contextClass(cerr)}
			}
			return nil, mapped
		}
		if serr := st.sleep(ctx, st.urlBackoff[attempt]); serr != nil {
			return nil, &signedBundleTransferError{Stage: "upload_url", Root: rootName, PartIndex: partIndex, Class: contextClass(serr)}
		}
	}
}

func (s *Server) fetchMoveBundleUploadURL(ctx context.Context, hubConn, agentID, rid string, req *api.MoveBundleUploadURLRequest) (*api.MoveBundleUploadURLResponse, error) {
	s.hubMu.RLock()
	conn := s.hubConnections[hubConn]
	s.hubMu.RUnlock()
	if hubConn == "" || conn == nil || conn.HubClient == nil {
		return nil, errNoSignedDownloadHub
	}
	svc, ok := conn.HubClient.RuntimeBrokers().(hubclient.RuntimeBrokerMoveBundleService)
	if !ok {
		return nil, errNoSignedDownloadHub
	}
	return svc.MoveBundleUploadURL(ctx, conn.BrokerID, agentID, rid, req)
}

func (s *Server) fetchMoveBundleDownloadURLs(ctx context.Context, hubConn, agentID, rid string, req *api.MoveBundleDownloadURLsRequest) (*api.MoveBundleDownloadURLsResponse, error) {
	s.hubMu.RLock()
	conn := s.hubConnections[hubConn]
	s.hubMu.RUnlock()
	if hubConn == "" || conn == nil || conn.HubClient == nil {
		return nil, errNoSignedDownloadHub
	}
	svc, ok := conn.HubClient.RuntimeBrokers().(hubclient.RuntimeBrokerMoveBundleService)
	if !ok {
		return nil, errNoSignedDownloadHub
	}
	return svc.MoveBundleDownloadURLs(ctx, conn.BrokerID, agentID, rid, req)
}

func mapBundleURLError(stage, rootName string, partIndex int, err error) (*signedBundleTransferError, bool) {
	out := &signedBundleTransferError{Stage: stage, Root: rootName, PartIndex: partIndex}
	var apiErr *apiclient.APIError
	switch {
	case errors.Is(err, errNoSignedDownloadHub):
		out.Class = signedClassNoHub
		return out, false
	case errors.As(err, &apiErr):
		out.Status = apiErr.StatusCode
		out.Class = "endpoint_" + strconv.Itoa(apiErr.StatusCode)
		return out, apiErr.StatusCode >= 500
	}
	out.Class = transportClass(err)
	if out.Class == signedClassDecode {
		return out, false
	}
	return out, out.Class != signedClassCanceled
}

// handleMoveBundleImportStart implements POST /api/v1/agents/{id}/move-bundle/import.
func (s *Server) handleMoveBundleImportStart(w http.ResponseWriter, r *http.Request, id, projectID string) {
	var req api.MoveBundleImportRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}
	if !validMoveBundleRID(req.ReincarnationID) {
		ValidationError(w, "invalid or missing reincarnationId", nil)
		return
	}
	if err := req.Limits.Validate(); err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	if err := req.Manifest.Validate(); err != nil {
		ValidationError(w, "invalid manifest: "+err.Error(), nil)
		return
	}
	if req.Manifest.ReincarnationID != req.ReincarnationID {
		ValidationError(w, "manifest reincarnationId does not match request reincarnationId", nil)
		return
	}
	_, computedHash, err := bundle.CanonicalManifest(req.Manifest)
	if err != nil {
		ValidationError(w, "canonicalize manifest: "+err.Error(), nil)
		return
	}
	if req.ManifestHash == "" || req.ManifestHash != computedHash {
		ValidationError(w, "manifestHash does not match manifest", nil)
		return
	}

	if projectID != "" && req.ProjectID != "" && projectID != req.ProjectID {
		ValidationError(w, "projectId query parameter does not match request body", nil)
		return
	}
	effProjectID := firstNonEmpty(projectID, req.ProjectID)
	rtName := s.resolveMoveBundleRuntimeName(r, id, effProjectID)
	reqAgentID := firstNonEmpty(req.AgentID, r.URL.Query().Get(api.AgentIDQueryParam))
	dirs, err := s.resolveMoveBundleDirs(
		r.Context(),
		rtName,
		id,
		reqAgentID,
		effProjectID,
		firstNonEmpty(req.ProjectPath, r.URL.Query().Get("projectPath")),
		firstNonEmpty(req.ProjectSlug, r.URL.Query().Get("projectSlug")),
	)
	if err != nil {
		s.agentLifecycleLog.Warn("Move-bundle import: failed to resolve agent directories",
			"agent_id", id, "error", err)
		ValidationError(w, "cannot resolve agent directories", nil)
		return
	}
	if dirs.SharedWorkspace {
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleUnsupported, "shared layout workspace is not yet supported for bundle moves", nil)
		return
	}

	// Confirm the agent directory was provisioned for this exact move
	// (fresh-agent-dir writes agent.MoveMarkerFile with req.ReincarnationID).
	moveMarker, moveMarkerErr := agent.ReadMoveMarker(dirs.AgentDir)
	if moveMarkerErr != nil {
		if _, statErr := os.Stat(dirs.AgentDir); errors.Is(statErr, fs.ErrNotExist) {
			NotFound(w, "Agent directory")
			return
		}
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveMarkerMismatch, "agent directory was not provisioned for this move", nil)
		return
	}
	if moveMarker != req.ReincarnationID {
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveMarkerMismatch, "agent directory move marker does not match reincarnationId", nil)
		return
	}
	if dirs.ExportAgentDir != "" {
		exportMarker, exportMarkerErr := agent.ReadMoveMarker(dirs.ExportAgentDir)
		if exportMarkerErr != nil || exportMarker != req.ReincarnationID {
			writeError(w, http.StatusConflict, api.BrokerErrCodeMoveMarkerMismatch, "export agent directory move marker does not match reincarnationId", nil)
			return
		}
	}

	imported, markerErr := hasMoveBundleImportedMarker(dirs.AgentDir, req.ReincarnationID)
	if markerErr != nil {
		if errors.Is(markerErr, fs.ErrNotExist) {
			NotFound(w, "Agent directory")
			return
		}
		InternalError(w)
		return
	}
	if imported {
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleAlreadyImported, "move-bundle already imported for this reincarnationId", nil)
		return
	}

	hubConn := s.resolveHubNameForLaunch(r)
	agentUUID := firstNonEmpty(req.AgentID, r.URL.Query().Get(api.AgentIDQueryParam), id)

	jobCtx, cancel := context.WithTimeout(context.Background(), req.Limits.Timeout())
	// Key jobs by query projectID so POST, GET status, and DELETE cancel always
	// resolve the exact same registry key.
	agentKey := moveBundleAgentKey{ProjectID: projectID, AgentSlug: id, Kind: api.MoveBundleJobKindImport}
	job, conflict, created := s.moveBundleRegistry().start(agentKey, req.ReincarnationID, cancel)
	if conflict {
		cancel()
		writeError(w, http.StatusConflict, api.BrokerErrCodeMoveBundleJobConflict, "another move-bundle import job is already running for this agent", nil)
		return
	}
	if !created {
		cancel()
		writeJSON(w, http.StatusAccepted, job.snapshot())
		return
	}

	var totalParts int
	var totalBytes int64
	for _, rm := range req.Manifest.Roots {
		totalParts += len(rm.Parts)
		for _, p := range rm.Parts {
			totalBytes += p.Size
		}
	}
	job.setTotals(totalBytes, totalParts)

	go s.runMoveBundleImport(jobCtx, cancel, job, hubConn, agentUUID, dirs, req)
	writeJSON(w, http.StatusAccepted, job.snapshot())
}

func (s *Server) runMoveBundleImport(
	ctx context.Context,
	cancel context.CancelFunc,
	job *moveBundleJob,
	hubConn string,
	agentID string,
	dirs moveBundleDirs,
	req api.MoveBundleImportRequest,
) {
	defer cancel()
	st := s.moveBundle.resolved(s)

	scratchDir, err := os.MkdirTemp(s.moveBundleScratchBase(), "mb-import-*")
	if err != nil {
		job.markFailed("bundle import failed: create scratch dir", "", st.now())
		return
	}
	defer func() { _ = os.RemoveAll(scratchDir) }()

	// Create sibling staging directories ".<base>.bundle-<32hex>" on the same
	// filesystem as WorkspaceDir and HomeDir.
	wsStageName, err := newMoveBundleStageName(filepath.Base(dirs.WorkspaceDir))
	if err != nil {
		job.markFailed("bundle import failed: generate workspace staging name", "", st.now())
		return
	}
	homeStageName, err := newMoveBundleStageName(filepath.Base(dirs.HomeDir))
	if err != nil {
		job.markFailed("bundle import failed: generate home staging name", "", st.now())
		return
	}
	wsStagePath := filepath.Join(filepath.Dir(dirs.WorkspaceDir), wsStageName)
	homeStagePath := filepath.Join(filepath.Dir(dirs.HomeDir), homeStageName)

	if err := os.Mkdir(wsStagePath, 0o755); err != nil {
		job.markFailed("bundle import failed: create workspace staging dir", "", st.now())
		return
	}
	if err := os.Mkdir(homeStagePath, 0o755); err != nil {
		_ = os.RemoveAll(wsStagePath)
		job.markFailed("bundle import failed: create home staging dir", "", st.now())
		return
	}

	keepStagingOnCrash := false
	defer func() {
		if !keepStagingOnCrash {
			_ = os.RemoveAll(wsStagePath)
			_ = os.RemoveAll(homeStagePath)
		}
	}()

	pool := &moveBundleURLPool{
		st:      st,
		hubConn: hubConn,
		agentID: agentID,
		rid:     req.ReincarnationID,
	}

	stallTimeout := req.Limits.PartStallTimeout()
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	roots := []struct {
		name      string
		stagePath string
		lim       bundle.Limits
	}{
		{bundle.RootWorkspace, wsStagePath, req.Limits.WorkspaceLimits()},
		{bundle.RootHome, homeStagePath, req.Limits.HomeLimits()},
	}

	errCh := make(chan error, len(roots))
	var wg sync.WaitGroup
	for _, rt := range roots {
		wg.Add(1)
		go func(name, stagePath string, lim bundle.Limits) {
			defer wg.Done()
			stageRoot, openErr := os.OpenRoot(stagePath)
			if openErr != nil {
				errCh <- fmt.Errorf("open %s staging root: %w", name, openErr)
				runCancel()
				return
			}
			defer func() { _ = stageRoot.Close() }()

			src := &hubSignedPartSource{
				pool:         pool,
				st:           st,
				rootName:     name,
				scratchDir:   scratchDir,
				stallTimeout: stallTimeout,
				job:          job,
			}
			if exErr := bundle.ExtractFromManifest(runCtx, stageRoot, scratchDir, req.Manifest, name, lim, src); exErr != nil {
				errCh <- &moveBundleRootError{root: name, err: exErr}
				runCancel()
				return
			}
		}(rt.name, rt.stagePath, rt.lim)
	}
	wg.Wait()
	close(errCh)

	for exErr := range errCh {
		if exErr != nil {
			s.agentLifecycleLog.Warn("Move-bundle import extract failed",
				"agent_id", agentID, "reincarnation_id", req.ReincarnationID, "error", exErr)
			job.markFailed(formatMoveBundleError("bundle import failed", exErr), "", st.now())
			return
		}
	}

	// Commit workspace (empty dir; fail on any conflicting top-level name).
	if err := commitMoveBundleWorkspace(dirs.WorkspaceDir, wsStageName); err != nil {
		if errors.Is(err, errMoveBundleCommitCrash) {
			keepStagingOnCrash = true
		}
		s.agentLifecycleLog.Warn("Move-bundle import workspace commit failed",
			"agent_id", agentID, "reincarnation_id", req.ReincarnationID, "error", err)
		job.markFailed(formatMoveBundleError("bundle import failed", err), "", st.now())
		return
	}
	if dirs.WorkspaceOwner.UID > 0 || dirs.WorkspaceOwner.GID > 0 {
		if err := chownMoveBundleTree(dirs.WorkspaceDir, dirs.WorkspaceOwner.UID, dirs.WorkspaceOwner.GID); err != nil {
			s.agentLifecycleLog.Warn("Move-bundle import workspace chown failed",
				"agent_id", agentID, "reincarnation_id", req.ReincarnationID,
				"uid", dirs.WorkspaceOwner.UID, "gid", dirs.WorkspaceOwner.GID, "error", err)
			job.markFailed(formatMoveBundleError("bundle import failed", err), "", st.now())
			return
		}
	}

	// Commit home (provision-rendered files win; skip and count).
	skipped, err := commitMoveBundleHome(dirs.HomeDir, homeStageName)
	if err != nil {
		if errors.Is(err, errMoveBundleCommitCrash) {
			keepStagingOnCrash = true
		}
		s.agentLifecycleLog.Warn("Move-bundle import home commit failed",
			"agent_id", agentID, "reincarnation_id", req.ReincarnationID, "error", err)
		job.markFailed(formatMoveBundleError("bundle import failed", err), "", st.now())
		return
	}
	if dirs.HomeOwner.UID > 0 || dirs.HomeOwner.GID > 0 {
		if err := chownMoveBundleTree(dirs.HomeDir, dirs.HomeOwner.UID, dirs.HomeOwner.GID); err != nil {
			s.agentLifecycleLog.Warn("Move-bundle import home chown failed",
				"agent_id", agentID, "reincarnation_id", req.ReincarnationID,
				"uid", dirs.HomeOwner.UID, "gid", dirs.HomeOwner.GID, "error", err)
			job.markFailed(formatMoveBundleError("bundle import failed", err), "", st.now())
			return
		}
	}

	if err := moveBundleCommitHook("before_marker"); err != nil {
		if errors.Is(err, errMoveBundleCommitCrash) {
			keepStagingOnCrash = true
		}
		s.agentLifecycleLog.Warn("Move-bundle import pre-marker hook failed",
			"agent_id", agentID, "reincarnation_id", req.ReincarnationID, "error", err)
		job.markFailed(formatMoveBundleError("bundle import failed", err), "", st.now())
		return
	}

	// Write imported marker only after both roots commit.
	if err := writeMoveBundleImportedMarker(dirs.AgentDir, req.ReincarnationID); err != nil {
		s.agentLifecycleLog.Warn("Move-bundle import marker write failed",
			"agent_id", agentID, "reincarnation_id", req.ReincarnationID, "error", err)
		job.markFailed(formatMoveBundleError("bundle import failed", err), "", st.now())
		return
	}

	job.markSucceeded(nil, &api.MoveBundleImportSkipCounts{HomeProvisionWins: skipped}, st.now())
}

type moveBundlePartKey struct {
	root  string
	index int
}

// moveBundleURLPool caches signed GET URLs for all parts of a bundle import
// and refreshes them from the Hub when deadline-60s passes or when the object
// store answers 400/401/403.
type moveBundleURLPool struct {
	st      moveBundleSettings
	hubConn string
	agentID string
	rid     string

	mu                    sync.Mutex
	urls                  map[moveBundlePartKey]string
	deadline              time.Time
	idleRefreshes         int
	completedSinceRefresh int
}

func (p *moveBundleURLPool) getURL(ctx context.Context, rootName string, partIndex int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.urls == nil || p.deadline.Sub(p.st.now()) < signedDownloadRefreshMargin {
		if err := p.refreshLocked(ctx, rootName, partIndex); err != nil {
			return "", err
		}
	}
	u, ok := p.urls[moveBundlePartKey{root: rootName, index: partIndex}]
	if !ok || u == "" {
		return "", &signedBundleTransferError{Stage: "download_urls", Root: rootName, PartIndex: partIndex, Class: signedClassInvalidEntry}
	}
	return u, nil
}

func (p *moveBundleURLPool) refreshOnAuthFailure(ctx context.Context, rootName string, partIndex int, failedURL string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// If another goroutine already refreshed this URL while we were failing,
	// reuse the fresh URL directly.
	if cur, ok := p.urls[moveBundlePartKey{root: rootName, index: partIndex}]; ok && cur != "" && cur != failedURL && p.deadline.Sub(p.st.now()) >= signedDownloadRefreshMargin {
		return cur, nil
	}
	if err := p.refreshLocked(ctx, rootName, partIndex); err != nil {
		return "", err
	}
	u, ok := p.urls[moveBundlePartKey{root: rootName, index: partIndex}]
	if !ok || u == "" {
		return "", &signedBundleTransferError{Stage: "download_urls", Root: rootName, PartIndex: partIndex, Class: signedClassInvalidEntry}
	}
	return u, nil
}

func (p *moveBundleURLPool) markCompleted() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completedSinceRefresh++
	p.idleRefreshes = 0
}

func (p *moveBundleURLPool) refreshLocked(ctx context.Context, rootName string, partIndex int) error {
	if p.urls != nil {
		if p.completedSinceRefresh == 0 {
			p.idleRefreshes++
		} else {
			p.idleRefreshes = 0
		}
		if p.idleRefreshes > signedDownloadMaxIdleRefreshes {
			return &signedBundleTransferError{Stage: "download_urls", Root: rootName, PartIndex: partIndex, Class: signedClassExpired}
		}
		p.completedSinceRefresh = 0
	}

	var resp *api.MoveBundleDownloadURLsResponse
	for attempt := 0; ; attempt++ {
		r, err := p.st.fetchDownloadURLs(ctx, p.hubConn, p.agentID, p.rid, &api.MoveBundleDownloadURLsRequest{})
		if err == nil {
			if r == nil {
				return &signedBundleTransferError{Stage: "download_urls", Root: rootName, PartIndex: partIndex, Class: signedClassDecode}
			}
			resp = r
			break
		}
		mapped, retryable := mapBundleURLError("download_urls", rootName, partIndex, err)
		if !retryable || ctx.Err() != nil || attempt >= len(p.st.urlBackoff) {
			if cerr := ctx.Err(); cerr != nil {
				return &signedBundleTransferError{Stage: "download_urls", Root: rootName, PartIndex: partIndex, Class: contextClass(cerr)}
			}
			return mapped
		}
		if serr := p.st.sleep(ctx, p.st.urlBackoff[attempt]); serr != nil {
			return &signedBundleTransferError{Stage: "download_urls", Root: rootName, PartIndex: partIndex, Class: contextClass(serr)}
		}
	}

	now := p.st.now()
	urls := make(map[moveBundlePartKey]string, len(resp.Parts))
	var earliest time.Time
	for _, part := range resp.Parts {
		if part.URL == "" {
			return &signedBundleTransferError{Stage: "download_urls", Root: part.Root, PartIndex: part.Index, Class: signedClassInvalidEntry}
		}
		urls[moveBundlePartKey{root: part.Root, index: part.Index}] = part.URL
		if !part.Expires.IsZero() && (earliest.IsZero() || part.Expires.Before(earliest)) {
			earliest = part.Expires
		}
	}
	var deadline time.Time
	switch {
	case resp.ExpiresInSeconds > 0:
		deadline = now.Add(time.Duration(resp.ExpiresInSeconds) * time.Second)
	case !resp.Expires.IsZero():
		deadline = resp.Expires
	default:
		deadline = earliest
	}
	p.urls = urls
	p.deadline = deadline
	return nil
}

// hubSignedPartSource implements bundle.PartSource using moveBundleURLPool
// and signedGetOnce with retries, stall timeout, URL refresh, and strict URL
// redaction.
type hubSignedPartSource struct {
	pool         *moveBundleURLPool
	st           moveBundleSettings
	rootName     string
	scratchDir   string
	stallTimeout time.Duration
	job          *moveBundleJob
}

func (s *hubSignedPartSource) Get(ctx context.Context, p bundle.Part) (io.ReadCloser, error) {
	targetURL, err := s.pool.getURL(ctx, s.rootName, p.Index)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		rc, status, class, retryable := signedGetOnce(ctx, s.st.client, targetURL, s.scratchDir, p.Size, s.stallTimeout)
		if class == "" {
			s.pool.markCompleted()
			s.job.addProgress(p.Size, 1)
			return rc, nil
		}
		if status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden {
			freshURL, refErr := s.pool.refreshOnAuthFailure(ctx, s.rootName, p.Index, targetURL)
			if refErr != nil {
				return nil, refErr
			}
			targetURL = freshURL
			attempt = -1 // fresh URL gets its own retry budget
			continue
		}
		if !retryable || ctx.Err() != nil || attempt >= len(s.st.partBackoff) {
			if cerr := ctx.Err(); cerr != nil {
				return nil, &signedBundleTransferError{Stage: "get_part", Root: s.rootName, PartIndex: p.Index, Class: contextClass(cerr)}
			}
			return nil, &signedBundleTransferError{Stage: "get_part", Root: s.rootName, PartIndex: p.Index, Status: status, Class: class}
		}
		if serr := s.st.sleep(ctx, s.st.partBackoff[attempt]); serr != nil {
			return nil, &signedBundleTransferError{Stage: "get_part", Root: s.rootName, PartIndex: p.Index, Class: contextClass(serr)}
		}
	}
}

type moveBundleRootError struct {
	root string
	err  error
}

func (e *moveBundleRootError) Error() string {
	return e.root + ": " + e.err.Error()
}

func (e *moveBundleRootError) Unwrap() error {
	return e.err
}

// formatMoveBundleError formats a class-only job failure message while
// guaranteeing that no signed URL, raw *url.Error, or absolute host path ever
// reaches the user-visible job status error string (detailed errors are logged
// on the broker only).
func formatMoveBundleError(prefix string, err error) string {
	var transferErr *signedBundleTransferError
	if errors.As(err, &transferErr) {
		return prefix + ": " + transferErr.Error()
	}
	var limErr *bundle.ErrLimitExceeded
	if errors.As(err, &limErr) {
		var rootErr *moveBundleRootError
		if errors.As(err, &rootErr) && rootErr.root != "" {
			return strings.Replace(limErr.Error(), "bundle exceeds limit: ", "bundle exceeds limit: "+rootErr.root+" ", 1)
		}
		return limErr.Error()
	}
	var unportablePath *bundle.ErrUnportablePath
	if errors.As(err, &unportablePath) {
		return prefix + ": unsupported path"
	}
	var unportableHome *bundle.ErrUnportableHome
	if errors.As(err, &unportableHome) {
		return prefix + ": unportable symlink or hardlink"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return prefix + ": timed out"
	}
	if errors.Is(err, context.Canceled) {
		return prefix + ": canceled"
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return prefix + ": filesystem operation failed"
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return prefix + ": filesystem operation failed"
	}
	return prefix + ": internal failure"
}
