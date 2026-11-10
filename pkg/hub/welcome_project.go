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
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// The welcome project (ptone/scion#7534) is a one-time onboarding step: the
// hub installs the latest agent-team templates, pinned to commit shas at
// fetch, into a "welcome" project and starts a coordinator that greets the
// owner once. On the workstation the setup wizard drives it:
//
//	POST /api/v1/system/welcome/preview  resolve, download, verify, stage
//	POST /api/v1/system/welcome/install  {preview_id}: re-hash and install
//	POST /api/v1/system/welcome/skip     record that the owner skipped it
//
// All three routes are gated server-side by the hub.welcome_project
// experiment. The welcome_project ledger (welcome_ledger.go) makes the flow
// run at most once: after skipped, failed or a created project, nothing is
// created again, and a failure is never retried.

const (
	// welcomeProjectName is the name of the welcome project.
	welcomeProjectName = "welcome"
	// welcomeCoordinatorAgent is the name of the coordinator agent.
	welcomeCoordinatorAgent = "coordinator"
	// welcomeManifestAnnotation holds the installed manifest on the project.
	welcomeManifestAnnotation = "scion.welcome/manifest"

	// welcomePreviewTTL is how long a preview_id may be used.
	welcomePreviewTTL = 30 * time.Minute
	// welcomePreviewMax bounds the previews held in memory.
	welcomePreviewMax = 8
	// welcomeDetachedTimeout bounds the work that must finish even if the
	// client goes away: the install once its preview is consumed, and every
	// ledger write.
	welcomeDetachedTimeout = 10 * time.Minute
)

// welcomeDetached returns a context that keeps r's values (the caller's
// identity) but not its cancellation, bounded by welcomeDetachedTimeout. A
// client disconnect then cannot leave a half-built project, a failed
// rollback or a ledger stuck short of failed.
func welcomeDetached(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), welcomeDetachedTimeout)
}

// welcomePreview is one outstanding preview: single use, bound to the user
// who asked for it, expiring at expires.
type welcomePreview struct {
	id        string
	userID    string
	projectID string
	created   time.Time
	expires   time.Time
	staged    *welcomeStaged
}

// welcomeRuntimeState is the welcome flow's in-memory state and seams. The
// zero value is ready; nil seams select production behaviour.
type welcomeRuntimeState struct {
	// mu serializes preview, install and skip on this hub process; the
	// ledger's CAS writes guard against other processes.
	mu sync.Mutex
	// previews is keyed by the hex sha256 of the preview_id.
	previews map[string]*welcomePreview
	// stagingSwept records that this process's first preview swept the
	// staging dirs an earlier process left behind.
	stagingSwept bool

	// Seams; tests replace them.
	fetcher     welcomeArchiveFetcher
	resolveRef  func(ctx context.Context, owner, repo, ref string) (string, error)
	now         func() time.Time
	limits      welcomeLimits
	limitsSet   bool
	credentials func(ctx context.Context, project *store.Project, ownerID string) (bool, error)
	// importHook, when set, runs before each template import with the
	// template name and its staged directory; an error fails that import.
	importHook func(name, dir string) error
}

// welcomeRuntimeView is the resolved seams of one call.
type welcomeRuntimeView struct {
	fetcher    welcomeArchiveFetcher
	resolveRef func(ctx context.Context, owner, repo, ref string) (string, error)
	limits     welcomeLimits
}

func (s *Server) welcomeRuntime() welcomeRuntimeView {
	rt := &s.welcome
	v := welcomeRuntimeView{fetcher: rt.fetcher, resolveRef: rt.resolveRef, limits: rt.limits}
	if !rt.limitsSet {
		v.limits = defaultWelcomeLimits
	}
	if v.fetcher == nil {
		v.fetcher = newWelcomeArchiveFetcher(v.limits, slog.Default())
	}
	if v.resolveRef == nil {
		v.resolveRef = config.ResolveGitHubRefSHA
	}
	return v
}

func (s *Server) welcomeNow() time.Time {
	if s.welcome.now != nil {
		return s.welcome.now()
	}
	return time.Now()
}

func welcomePreviewKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// addWelcomePreview records a new preview and returns its id. Expired
// previews are evicted first; if the map is still full the oldest preview is
// evicted, so repeated previews cannot grow it without limit. Evicted
// previews' staging dirs are removed. A nil staged tree is refused, so every
// stored preview carries one. Callers hold s.welcome.mu.
func (s *Server) addWelcomePreview(userID, projectID string, staged *welcomeStaged) (*welcomePreview, error) {
	if staged == nil {
		return nil, errors.New("welcome preview has no staged tree")
	}
	rt := &s.welcome
	if rt.previews == nil {
		rt.previews = map[string]*welcomePreview{}
	}
	now := s.welcomeNow()
	s.evictExpiredWelcomePreviews(now)
	for len(rt.previews) >= welcomePreviewMax {
		oldestKey := ""
		var oldest time.Time
		for k, p := range rt.previews {
			if oldestKey == "" || p.created.Before(oldest) {
				oldestKey, oldest = k, p.created
			}
		}
		removeWelcomeStagingDir(rt.previews[oldestKey].staged.dir)
		delete(rt.previews, oldestKey)
	}
	var b [32]byte // 256 bits
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	p := &welcomePreview{
		id:        hex.EncodeToString(b[:]),
		userID:    userID,
		projectID: projectID,
		created:   now,
		expires:   now.Add(welcomePreviewTTL),
		staged:    staged,
	}
	rt.previews[welcomePreviewKey(p.id)] = p
	return p, nil
}

func (s *Server) evictExpiredWelcomePreviews(now time.Time) {
	for k, p := range s.welcome.previews {
		if !now.Before(p.expires) {
			removeWelcomeStagingDir(p.staged.dir)
			delete(s.welcome.previews, k)
		}
	}
}

// welcomePreviewDirs returns the staging dirs of the live previews. Callers
// hold s.welcome.mu. Previews live in memory only, so at the first preview
// call of a process it is empty; it is kept as a guard, so the sweep can
// never remove a live preview's dir if the sweep's timing changes.
func (s *Server) welcomePreviewDirs() map[string]bool {
	dirs := map[string]bool{}
	for _, p := range s.welcome.previews {
		if p.staged != nil && p.staged.dir != "" {
			dirs[p.staged.dir] = true
		}
	}
	return dirs
}

// dropWelcomePreviews forgets every preview and removes its staging dir.
func (s *Server) dropWelcomePreviews() {
	for k, p := range s.welcome.previews {
		removeWelcomeStagingDir(p.staged.dir)
		delete(s.welcome.previews, k)
	}
}

// takeWelcomePreview consumes the preview id for userID. An unknown, used or
// expired id, or one bound to another user, returns nil; another user's
// attempt does not consume the preview. Callers hold s.welcome.mu.
func (s *Server) takeWelcomePreview(id, userID string) *welcomePreview {
	s.evictExpiredWelcomePreviews(s.welcomeNow())
	if id == "" {
		return nil
	}
	key := welcomePreviewKey(id)
	p, ok := s.welcome.previews[key]
	if !ok {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(p.id), []byte(id)) != 1 {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(p.userID), []byte(userID)) != 1 {
		return nil
	}
	delete(s.welcome.previews, key)
	return p
}

// welcomePreviewResponse is the preview result shown before install.
type welcomePreviewResponse struct {
	PreviewID string          `json:"preview_id"`
	ExpiresAt time.Time       `json:"expires_at"`
	Source    welcomeSourceJS `json:"source"`
	Manifest  welcomeManifest `json:"manifest"`
	FileCount int             `json:"file_count"`
}

type welcomeSourceJS struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
}

// welcomeStateResponse reports the ledger state after install or skip.
type welcomeStateResponse struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// ProjectSlug and ProjectName are the project as actually created: when
	// the slug "welcome" is taken, create picks the next free one.
	ProjectSlug string           `json:"project_slug,omitempty"`
	ProjectName string           `json:"project_name,omitempty"`
	ProjectID   string           `json:"project_id,omitempty"`
	AgentID     string           `json:"agent_id,omitempty"`
	Manifest    *welcomeManifest `json:"manifest,omitempty"`
}

// welcomeInstallRequest is the install body.
type welcomeInstallRequest struct {
	PreviewID string `json:"preview_id"`
}

const (
	errCodeWelcomeDone           = "welcome_project_done"
	errCodeWelcomeFailed         = "welcome_project_failed"
	errCodeWelcomePreviewInvalid = "welcome_preview_invalid"
)

func writeWelcomeDone(w http.ResponseWriter, l *welcomeLedger) {
	state := ""
	if l != nil {
		state = l.doc.State
	}
	writeError(w, http.StatusConflict, errCodeWelcomeDone,
		"the welcome project step has already run", map[string]interface{}{"state": state})
}

// welcomeUser returns the calling user, writing 401 when there is none.
func welcomeUser(w http.ResponseWriter, r *http.Request) UserIdentity {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return nil
	}
	return user
}

// handleWelcomePreview handles POST /api/v1/system/welcome/preview.
func (s *Server) handleWelcomePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	user := welcomeUser(w, r)
	if user == nil {
		return
	}
	// Ledger reads and writes use a detached context, so the ledger is
	// always left consistent; the downloads use the request context and stop
	// when the client goes away, which leaves the ledger at claimed (below).
	ctx, cancel := welcomeDetached(r)
	defer cancel()
	s.welcome.mu.Lock()
	defer s.welcome.mu.Unlock()

	// A restart forgets its previews but not their staging dirs: the first
	// preview call of this process removes the dirs no live preview holds,
	// before any early return, so a final-state ledger still clears a dir a
	// crash left between its creation and its deferred removal. It runs under
	// s.welcome.mu, and staging dirs are created only by
	// welcomeResolveAndStage below, under the same lock, so no in-flight
	// stage's dir exists while it runs; live previews' dirs are kept.
	if !s.welcome.stagingSwept {
		s.welcome.stagingSwept = true
		sweepWelcomeStaging(s.welcomePreviewDirs())
	}

	l, err := s.loadWelcomeLedger(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "could not read the welcome project state", nil)
		return
	}
	if l.exists() && welcomeStateFinal(l.doc.State) {
		writeWelcomeDone(w, l)
		return
	}
	if l.exists() && l.doc.OwnerUserID != user.ID() {
		writeWelcomeNotOwner(w)
		return
	}

	// Claim: the project id is chosen once and saved, so a resumed install
	// creates the same project.
	projectID := api.NewUUID()
	if l.exists() && l.doc.ProjectID != "" {
		projectID = l.doc.ProjectID
	}
	if !l.exists() {
		claim := welcomeLedgerDoc{State: welcomeStateClaimed, OwnerUserID: user.ID(), ProjectID: projectID}
		if _, err := s.transitionWelcomeLedger(ctx, claim, ""); err != nil {
			writeWelcomeLedgerError(w, err)
			return
		}
	}

	staged, err := s.welcomeResolveAndStage(r.Context())
	if err != nil {
		// A client that went away (the request context cancelled from the
		// client side) leaves the claim as it is: the staging dir is already
		// removed and nothing is recorded, so a later preview by the same
		// owner proceeds from claimed. A deadline, including the step's own
		// bounded timeouts, or any other error still records failed.
		if errors.Is(r.Context().Err(), context.Canceled) {
			slog.Info("welcome project: preview cancelled by the client; the claim is kept")
			writeError(w, http.StatusServiceUnavailable, errCodeWelcomeFailed, "the preview was cancelled", nil)
			return
		}
		reason := welcomeReason(err)
		// The failed preview ends the step, so an earlier preview no longer
		// installs: forget it and remove its staging dir.
		s.dropWelcomePreviews()
		s.failWelcome(ctx, user.ID(), projectID, reason, welcomeStateClaimed, welcomeStatePreviewed)
		writeError(w, http.StatusUnprocessableEntity, errCodeWelcomeFailed, reason, nil)
		return
	}
	doc := welcomeLedgerDoc{State: welcomeStatePreviewed, OwnerUserID: user.ID(), ProjectID: projectID, Manifest: &staged.manifest}
	if _, err := s.transitionWelcomeLedger(ctx, doc, welcomeStateClaimed, welcomeStatePreviewed); err != nil {
		removeWelcomeStagingDir(staged.dir)
		writeWelcomeLedgerError(w, err)
		return
	}
	// One outstanding preview: a new preview replaces any earlier one, whose
	// staging dir is removed and whose preview_id no longer installs.
	s.dropWelcomePreviews()
	p, err := s.addWelcomePreview(user.ID(), projectID, staged)
	if err != nil {
		removeWelcomeStagingDir(staged.dir)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "could not record the preview", nil)
		return
	}
	slog.Info("welcome project: previewed", "repo", staged.manifest.AgentTeamRepo,
		"sha", staged.manifest.AgentTeamSHA, "files_sha256", staged.manifest.FilesSHA256)
	writeJSON(w, http.StatusOK, welcomePreviewResponse{
		PreviewID: p.id,
		ExpiresAt: p.expires.UTC(),
		Source: welcomeSourceJS{
			Repo: staged.manifest.AgentTeamRepo,
			Path: welcomeSource.TemplatesPath,
			SHA:  staged.manifest.AgentTeamSHA,
		},
		Manifest:  staged.manifest,
		FileCount: len(staged.files),
	})
}

func writeWelcomeNotOwner(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, errCodeWelcomeDone, "the welcome project step belongs to another user", nil)
}

func writeWelcomeLedgerError(w http.ResponseWriter, err error) {
	if errors.Is(err, errWelcomeLedgerConflict) {
		writeError(w, http.StatusConflict, errCodeWelcomeDone, "the welcome project state changed; reload", nil)
		return
	}
	writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "could not save the welcome project state", nil)
}

// failWelcome records failed with reason, from one of the given states. It
// logs the reason only: no local path, no owner address.
func (s *Server) failWelcome(ctx context.Context, ownerID, projectID, reason string, from ...string) {
	doc := welcomeLedgerDoc{State: welcomeStateFailed, Reason: reason, OwnerUserID: ownerID, ProjectID: projectID}
	if _, err := s.transitionWelcomeLedger(ctx, doc, from...); err != nil {
		slog.Error("welcome project: could not record the failure", "reason", reason)
	}
	slog.Warn("welcome project: failed; it will not be retried", "reason", reason)
}

// handleWelcomeSkip handles POST /api/v1/system/welcome/skip. It is
// idempotent: a repeat after skipped answers the same. After any other final
// state it is refused, and while claimed or previewed only the user who
// claimed the step may skip it.
func (s *Server) handleWelcomeSkip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	user := welcomeUser(w, r)
	if user == nil {
		return
	}
	ctx, cancel := welcomeDetached(r)
	defer cancel()
	s.welcome.mu.Lock()
	defer s.welcome.mu.Unlock()

	l, err := s.loadWelcomeLedger(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "could not read the welcome project state", nil)
		return
	}
	// While claimed or previewed the step belongs to its first user, as for
	// preview and install.
	if l.exists() && !welcomeStateFinal(l.doc.State) && l.doc.OwnerUserID != user.ID() {
		writeWelcomeNotOwner(w)
		return
	}
	if l.exists() && l.doc.State == welcomeStateSkipped {
		writeJSON(w, http.StatusOK, welcomeStateResponse{State: welcomeStateSkipped})
		return
	}
	if l.exists() && welcomeStateFinal(l.doc.State) {
		writeWelcomeDone(w, l)
		return
	}
	doc := welcomeLedgerDoc{State: welcomeStateSkipped, OwnerUserID: user.ID()}
	if _, err := s.transitionWelcomeLedger(ctx, doc, "", welcomeStateClaimed, welcomeStatePreviewed); err != nil {
		writeWelcomeLedgerError(w, err)
		return
	}
	s.dropWelcomePreviews()
	slog.Info("welcome project: skipped")
	writeJSON(w, http.StatusOK, welcomeStateResponse{State: welcomeStateSkipped})
}

// handleWelcomeInstall handles POST /api/v1/system/welcome/install. It never
// resolves again: it re-hashes the previewed staged tree, creates the
// project, imports the templates project-scoped and starts the coordinator
// when a broker and credentials are available. Any failure rolls the project
// back and records failed for good.
func (s *Server) handleWelcomeInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	user := welcomeUser(w, r)
	if user == nil {
		return
	}
	var req welcomeInstallRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}
	s.welcome.mu.Lock()
	defer s.welcome.mu.Unlock()

	p := s.takeWelcomePreview(req.PreviewID, user.ID())
	if p == nil {
		writeError(w, http.StatusBadRequest, errCodeWelcomePreviewInvalid,
			"the preview is unknown, used, expired or not yours; preview again", nil)
		return
	}
	defer removeWelcomeStagingDir(p.staged.dir)

	// The preview is consumed: from here the install runs to its end (or to
	// a recorded failure and rollback) even if the client goes away. Every
	// store call, in-process handler call and ledger write uses the detached
	// context, which keeps the caller's identity.
	ctx, cancel := welcomeDetached(r)
	defer cancel()
	r = r.WithContext(ctx)

	l, err := s.loadWelcomeLedger(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "could not read the welcome project state", nil)
		return
	}
	if !l.exists() || l.doc.State != welcomeStatePreviewed || l.doc.ProjectID != p.projectID || l.doc.OwnerUserID != user.ID() {
		writeWelcomeDone(w, l)
		return
	}
	manifest := p.staged.manifest

	fail := func(reason string, from ...string) {
		s.failWelcome(ctx, user.ID(), p.projectID, reason, from...)
		writeError(w, http.StatusUnprocessableEntity, errCodeWelcomeFailed, reason, nil)
	}

	// Re-hash the staged tree against the preview manifest.
	files, err := welcomeHashStaged(p.staged.dir, manifest.Templates)
	if err != nil {
		fail(welcomeReason(err), welcomeStatePreviewed)
		return
	}
	if !maps.Equal(files, p.staged.files) || welcomeFilesDigest(files) != manifest.FilesSHA256 {
		fail("the staged files changed after the preview", welcomeStatePreviewed)
		return
	}

	project, err := s.welcomeCreateProject(r, p.projectID, manifest)
	if err != nil {
		fail(welcomeReason(err), welcomeStatePreviewed)
		return
	}
	rollback := func() { s.welcomeRollbackProject(r, project.ID) }

	for _, tpl := range manifest.Templates {
		if err := s.welcomeImportTemplate(ctx, tpl, filepath.Join(p.staged.dir, tpl), project.ID); err != nil {
			rollback()
			fail("could not import template "+tpl, welcomeStatePreviewed)
			return
		}
	}

	created := welcomeLedgerDoc{State: welcomeStateCreated, OwnerUserID: user.ID(), ProjectID: project.ID,
		ProjectSlug: project.Slug, Manifest: &manifest}
	if _, err := s.transitionWelcomeLedger(ctx, created, welcomeStatePreviewed); err != nil {
		rollback()
		writeWelcomeLedgerError(w, err)
		return
	}
	slog.Info("welcome project: installed", "project_id", project.ID, "repo", manifest.AgentTeamRepo,
		"sha", manifest.AgentTeamSHA, "skills", welcomeSkillsLog(manifest.Skills), "files_sha256", manifest.FilesSHA256)

	state, agentID, err := s.welcomeStartCoordinator(r, project, user)
	if err != nil {
		rollback()
		fail(welcomeReason(err), welcomeStateCreated)
		return
	}
	final := created
	final.State, final.AgentID = state, agentID
	if _, err := s.transitionWelcomeLedger(ctx, final, welcomeStateCreated); err != nil {
		writeWelcomeLedgerError(w, err)
		return
	}
	slog.Info("welcome project: coordinator", "project_id", project.ID, "state", final.State)
	writeJSON(w, http.StatusOK, welcomeStateResponse{
		State: final.State, ProjectID: project.ID, ProjectSlug: project.Slug, ProjectName: project.Name,
		AgentID: final.AgentID, Manifest: &manifest,
	})
}

func welcomeSkillsLog(pins []welcomeSkillPin) []string {
	out := make([]string, 0, len(pins))
	for _, p := range pins {
		out = append(out, p.Repo+"@"+p.SHA)
	}
	sort.Strings(out)
	return out
}

// welcomeInternalCall runs a hub handler in process for the request's
// caller, so every authorization, ownership and rollback rule of that route
// applies to the welcome flow exactly as to the caller's own request.
func welcomeInternalCall(r *http.Request, method, target string, body interface{}, h func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequestWithContext(r.Context(), method, target, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = r.RemoteAddr
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// welcomeCreateProject creates the welcome project with the claimed id, as
// the caller, then sets its shared dirs (scratchpad, explicitly) and the
// manifest annotation. A project that already has this id (a resumed
// install) is reused.
func (s *Server) welcomeCreateProject(r *http.Request, projectID string, manifest welcomeManifest) (*store.Project, error) {
	ctx := r.Context()
	rec := welcomeInternalCall(r, http.MethodPost, "/api/v1/projects",
		CreateProjectRequest{ID: projectID, Name: welcomeProjectName}, s.createProject)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		return nil, welcomeFail("could not create the welcome project", nil)
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		s.welcomeRollbackProject(r, projectID)
		return nil, welcomeFail("could not read the welcome project", err)
	}
	annotation, err := json.Marshal(manifest)
	if err != nil {
		s.welcomeRollbackProject(r, projectID)
		return nil, welcomeFail("could not record the manifest", err)
	}
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[welcomeManifestAnnotation] = string(annotation)
	project.SharedDirs = []api.SharedDir{{Name: "scratchpad"}}
	if err := s.store.UpdateProject(ctx, project); err != nil {
		s.welcomeRollbackProject(r, projectID)
		return nil, welcomeFail("could not update the welcome project", err)
	}
	return project, nil
}

// welcomeRollbackProject deletes the welcome project, as the caller, through
// the project delete route (forced, so agents do not hold it).
func (s *Server) welcomeRollbackProject(r *http.Request, projectID string) {
	rec := welcomeInternalCall(r, http.MethodDelete, "/api/v1/projects/"+projectID+"?force=true", nil,
		func(w http.ResponseWriter, req *http.Request) { s.deleteProject(w, req, projectID) })
	if rec.Code >= 300 {
		slog.Error("welcome project: rollback failed", "project_id", projectID, "status", rec.Code)
		return
	}
	slog.Info("welcome project: rolled back", "project_id", projectID)
}

// welcomeImportTemplate imports one staged template directory project-scoped.
func (s *Server) welcomeImportTemplate(ctx context.Context, name, dir, projectID string) error {
	if s.welcome.importHook != nil {
		if err := s.welcome.importHook(name, dir); err != nil {
			return err
		}
	}
	return s.bootstrapSingleTemplate(ctx, name, dir, store.TemplateScopeProject, projectID)
}

// welcomeCoordinatorTask is the coordinator's task for the owner.
func welcomeCoordinatorTask(user UserIdentity) string {
	name := strings.TrimSpace(user.DisplayName())
	if name == "" {
		name = "the owner"
	}
	return "Greet " + name + " (user:" + user.ID() + ") once with scion message; ask what project they want to start; then wait."
}

// welcomeStartCoordinator starts the coordinator in the welcome project when
// it can: with no broker it returns pending_broker, and with no credential
// for the coordinator's harness pending_credentials, creating no agent in
// either case. It never creates an agent without credentials (NoAuth).
func (s *Server) welcomeStartCoordinator(r *http.Request, project *store.Project, user UserIdentity) (state, agentID string, err error) {
	ctx := r.Context()
	providers, err := s.store.GetProjectProviders(ctx, project.ID)
	if err != nil {
		return "", "", welcomeFail("could not read the project brokers", err)
	}
	if len(providers) == 0 {
		return welcomeStatePendingBroker, "", nil
	}
	tmpl, err := s.store.GetTemplateBySlug(ctx, api.Slugify(welcomeSource.Coordinator), store.TemplateScopeProject, project.ID)
	if err != nil {
		return "", "", welcomeFail("the coordinator template was not installed", err)
	}
	check := s.welcome.credentials
	if check == nil {
		check = s.welcomeCoordinatorHasCredentials
	}
	ok, err := check(ctx, project, user.ID())
	if err != nil {
		return "", "", welcomeFail("could not check the coordinator credentials", err)
	}
	if !ok {
		return welcomeStatePendingCredentials, "", nil
	}

	req := CreateAgentRequest{
		Name:      welcomeCoordinatorAgent,
		Template:  tmpl.ID,
		Task:      welcomeCoordinatorTask(user),
		AgentRole: string(AgentRoleFull),
		NoAuth:    false,
	}
	// The create may never fall back to no credentials: with the fallback
	// forbidden it fails before any agent is stored or dispatched.
	cr := r.WithContext(withNoAuthFallbackForbidden(r.Context()))
	rec := welcomeInternalCall(cr, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", req,
		func(w http.ResponseWriter, hr *http.Request) { s.createProjectAgent(w, hr, project.ID) })
	if guard := noAuthFallbackGuardFrom(cr.Context()); guard != nil && guard.refused {
		return "", "", welcomeFail("the coordinator has no credentials for its harness; it was not started", nil)
	}
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		return "", "", welcomeFail("could not start the coordinator", nil)
	}
	agent, err := s.store.GetAgentBySlug(ctx, project.ID, api.Slugify(welcomeCoordinatorAgent))
	if err != nil {
		return "", "", welcomeFail("could not read the coordinator", err)
	}
	if agent.AppliedConfig != nil && agent.AppliedConfig.NoAuth {
		// Defence in depth: the forbidden fallback above should make this
		// unreachable. The caller rolls the project (and this agent) back.
		return "", "", welcomeFail("the coordinator would start without credentials", nil)
	}
	return welcomeStateStarted, agent.ID, nil
}

// welcomeCoordinatorHasCredentials reports whether the coordinator's harness
// has the credentials it needs, by the same check agent create uses for its
// no-auth fallback (hasRequiredAuthCredentials). A harness config that cannot
// be resolved counts as no credentials.
func (s *Server) welcomeCoordinatorHasCredentials(ctx context.Context, project *store.Project, ownerID string) (bool, error) {
	tmpl, err := s.store.GetTemplateBySlug(ctx, api.Slugify(welcomeSource.Coordinator), store.TemplateScopeProject, project.ID)
	if err != nil {
		return false, err
	}
	hcName := templateHarnessConfigName(tmpl)
	if hcName == "" {
		hcName = s.hubAgentDefaults().DefaultHarnessConfig
	}
	if hcName == "" {
		return false, nil
	}
	hc, err := s.store.GetHarnessConfigBySlug(ctx, hcName, store.HarnessConfigScopeProject, project.ID)
	if errors.Is(err, store.ErrNotFound) {
		hc, err = s.store.GetHarnessConfigBySlug(ctx, hcName, store.HarnessConfigScopeGlobal, "")
	}
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	probe := &store.Agent{ProjectID: project.ID, OwnerID: ownerID, AppliedConfig: &store.AgentAppliedConfig{}}
	var authMeta *config.HarnessAuthMetadata
	if hc.Config != nil {
		authMeta = hc.Config.AuthMeta
	}
	return s.hasRequiredAuthCredentials(ctx, probe, hc.Harness, authMeta)
}

// VerifyWelcomeProjectTemplates re-hashes the stored bytes of the welcome
// project's templates against the ledger's manifest (files_sha256). It
// downloads every file of every manifest template from template storage and
// hashes the content itself, so it does not trust the stored hash metadata.
// It returns false when, for a template in the manifest's set, a stored
// file's bytes changed, a file was added, removed or renamed, or the template
// was removed since install. It reads only the manifest's templates, so a
// template added to the project outside that set is not detected.
// It is a library function with no route or caller in this change; a later
// phase surfaces it (drift detection, sec residual 2).
func (s *Server) VerifyWelcomeProjectTemplates(ctx context.Context) (bool, error) {
	l, err := s.loadWelcomeLedger(ctx)
	if err != nil {
		return false, err
	}
	if !l.exists() || l.doc.Manifest == nil || l.doc.ProjectID == "" {
		return false, errors.New("no installed welcome project")
	}
	stor := s.GetStorage()
	if stor == nil {
		return false, errors.New("no template storage")
	}
	files := map[string]string{}
	for _, name := range l.doc.Manifest.Templates {
		tmpl, err := s.store.GetTemplateBySlug(ctx, api.Slugify(name), store.TemplateScopeProject, l.doc.ProjectID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		for _, f := range tmpl.Files {
			data, err := readTemplateObject(ctx, stor, templateObjectPath(tmpl, f), defaultWelcomeLimits.MaxFile)
			if errors.Is(err, storage.ErrNotFound) || errors.Is(err, errTemplateObjectTooLarge) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			files[name+"/"+f.Path] = transfer.HashBytes(data)
		}
	}
	return welcomeFilesDigest(files) == l.doc.Manifest.FilesSHA256, nil
}
