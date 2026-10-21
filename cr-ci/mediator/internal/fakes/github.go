// Package fakes provides in-memory httptest fakes of the external services
// the mediator calls: GitHub, the Cloud Run Admin API, the metadata server,
// Secret Manager and Google's JWKS endpoint. They are for tests only and hold
// throwaway keys and fake values.
package fakes

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHubRun is a workflow run in the fake.
type GitHubRun struct {
	ID       int64
	Repo     string // owner/name
	HeadRepo string // owner/name; "" means null head_repository
	Event    string
	Status   string
	Jobs     []GitHubJob
}

// GitHubJob is a workflow job in the fake.
type GitHubJob struct {
	ID        int64
	Status    string
	Labels    []string
	CreatedAt time.Time
}

// GitHubRunner is a registered runner in the fake.
type GitHubRunner struct {
	ID     int64
	Name   string
	Status string
	Busy   bool
	Scope  string // "repo:owner/name" or "org:owner"
	Labels []string
}

// GitHub is a fake GitHub REST API.
type GitHub struct {
	Server *httptest.Server
	AppID  int64
	AppKey *rsa.PublicKey
	Now    func() time.Time

	mu         sync.Mutex
	runs       map[int64]*GitHubRun
	runners    map[int64]*GitHubRunner
	nextRunner int64
	nextToken  int
	jitSeq     int
	tokens     map[string]time.Time // issued installation tokens -> expiry

	// Failure injection.
	FailJIT       bool
	FailDelete    bool
	TokenLifetime time.Duration // default 1 h

	// Observations.
	TokenMints   int
	JITRequests  []map[string]any
	Deleted      []int64
	IssuedJWTs   []string
	IssuedTokens []string
	IssuedJIT    []string
	RunGets      int
	AuthFailures int
}

// NewGitHub starts a fake GitHub that accepts JWTs signed by the private key
// matching appKey.
func NewGitHub(appID int64, appKey *rsa.PublicKey) *GitHub {
	g := &GitHub{
		AppID:      appID,
		AppKey:     appKey,
		Now:        time.Now,
		runs:       map[int64]*GitHubRun{},
		runners:    map[int64]*GitHubRunner{},
		nextRunner: 1000,
		tokens:     map[string]time.Time{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", g.jwtOnly(g.installation))
	mux.HandleFunc("GET /orgs/{org}/installation", g.jwtOnly(g.installation))
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", g.jwtOnly(g.accessToken))
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs/{id}", g.tokenOnly(g.getRun))
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs", g.tokenOnly(g.listRuns))
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs/{id}/jobs", g.tokenOnly(g.listJobs))
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runners", g.tokenOnly(g.listRunners))
	mux.HandleFunc("GET /orgs/{org}/actions/runners", g.tokenOnly(g.listRunners))
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig", g.tokenOnly(g.jit))
	mux.HandleFunc("POST /orgs/{org}/actions/runners/generate-jitconfig", g.tokenOnly(g.jit))
	mux.HandleFunc("DELETE /repos/{owner}/{repo}/actions/runners/{id}", g.tokenOnly(g.deleteRunner))
	mux.HandleFunc("DELETE /orgs/{org}/actions/runners/{id}", g.tokenOnly(g.deleteRunner))
	g.Server = httptest.NewServer(mux)
	return g
}

// Close stops the server.
func (g *GitHub) Close() { g.Server.Close() }

// AddRun adds or replaces a workflow run.
func (g *GitHub) AddRun(r GitHubRun) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cp := r
	g.runs[r.ID] = &cp
}

// AddRunner registers a runner directly (as if left over from earlier).
func (g *GitHub) AddRunner(r GitHubRunner) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.ID == 0 {
		g.nextRunner++
		r.ID = g.nextRunner
	}
	cp := r
	g.runners[r.ID] = &cp
	return r.ID
}

// Runners returns the registered runners sorted by ID.
func (g *GitHub) Runners() []GitHubRunner {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []GitHubRunner
	for _, r := range g.runners {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RunnerByName returns the runner with name, if registered.
func (g *GitHub) RunnerByName(name string) (GitHubRunner, bool) {
	for _, r := range g.Runners() {
		if r.Name == name {
			return r, true
		}
	}
	return GitHubRunner{}, false
}

// Stats returns a snapshot of the counters (safe to call concurrently).
func (g *GitHub) Stats() (tokenMints, jitRequests, deletes int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.TokenMints, len(g.JITRequests), len(g.Deleted)
}

// Secrets returns every credential the fake issued or received.
func (g *GitHub) Secrets() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var s []string
	s = append(s, g.IssuedJWTs...)
	s = append(s, g.IssuedTokens...)
	s = append(s, g.IssuedJIT...)
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (g *GitHub) jwtOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !g.verifyJWT(jwt) {
			g.mu.Lock()
			g.AuthFailures++
			g.mu.Unlock()
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "A JSON web token could not be decoded"})
			return
		}
		g.mu.Lock()
		g.IssuedJWTs = append(g.IssuedJWTs, jwt)
		g.mu.Unlock()
		h(w, r)
	}
}

func (g *GitHub) verifyJWT(jwt string) bool {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(g.AppKey, crypto.SHA256, digest[:], sig) != nil {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var c struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if json.Unmarshal(b, &c) != nil {
		return false
	}
	now := g.Now().Unix()
	return c.Iss == strconv.FormatInt(g.AppID, 10) && c.Iat <= now && c.Exp > now && c.Exp-c.Iat <= 600
}

func (g *GitHub) tokenOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
		g.mu.Lock()
		exp, known := g.tokens[tok]
		valid := ok && known && g.Now().Before(exp)
		if !valid {
			g.AuthFailures++
		}
		g.mu.Unlock()
		if !valid {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		h(w, r)
	}
}

func (g *GitHub) installation(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"id": 77})
}

func (g *GitHub) accessToken(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.nextToken++
	g.TokenMints++
	tok := fmt.Sprintf("ghs_FAKEinstalltoken%04d", g.nextToken)
	life := g.TokenLifetime
	if life == 0 {
		life = time.Hour
	}
	exp := g.Now().Add(life)
	g.tokens[tok] = exp
	g.IssuedTokens = append(g.IssuedTokens, tok)
	g.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": exp.UTC().Format(time.RFC3339)})
}

func repoRef(full string) any {
	if full == "" {
		return nil
	}
	return map[string]string{"full_name": full}
}

func (g *GitHub) runJSON(run *GitHubRun) map[string]any {
	return map[string]any{
		"id": run.ID, "event": run.Event, "status": run.Status,
		"repository": repoRef(run.Repo), "head_repository": repoRef(run.HeadRepo),
	}
}

func (g *GitHub) getRun(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	full := r.PathValue("owner") + "/" + r.PathValue("repo")
	g.mu.Lock()
	defer g.mu.Unlock()
	g.RunGets++
	run, ok := g.runs[id]
	if !ok || !strings.EqualFold(run.Repo, full) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, g.runJSON(run))
}

func (g *GitHub) listRuns(w http.ResponseWriter, r *http.Request) {
	full := r.PathValue("owner") + "/" + r.PathValue("repo")
	status := r.URL.Query().Get("status")
	g.mu.Lock()
	defer g.mu.Unlock()
	var ids []int64
	for id, run := range g.runs {
		if strings.EqualFold(run.Repo, full) && (status == "" || run.Status == status) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	runs := []any{}
	for _, id := range ids {
		runs = append(runs, g.runJSON(g.runs[id]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "workflow_runs": runs})
}

func (g *GitHub) listJobs(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	g.mu.Lock()
	defer g.mu.Unlock()
	run, ok := g.runs[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	jobs := []any{}
	for _, j := range run.Jobs {
		jobs = append(jobs, map[string]any{
			"id": j.ID, "run_id": run.ID, "status": j.Status, "labels": j.Labels,
			"created_at": j.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(jobs), "jobs": jobs})
}

func scopeOf(r *http.Request) string {
	if org := r.PathValue("org"); org != "" {
		return "org:" + strings.ToLower(org)
	}
	return "repo:" + strings.ToLower(r.PathValue("owner")+"/"+r.PathValue("repo"))
}

func (g *GitHub) listRunners(w http.ResponseWriter, r *http.Request) {
	scope := scopeOf(r)
	g.mu.Lock()
	defer g.mu.Unlock()
	var ids []int64
	for id, rn := range g.runners {
		if rn.Scope == "" || rn.Scope == scope {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []any{}
	for _, id := range ids {
		rn := g.runners[id]
		out = append(out, map[string]any{"id": rn.ID, "name": rn.Name, "status": rn.Status, "busy": rn.Busy})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "runners": out})
}

func (g *GitHub) jit(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad json"})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.JITRequests = append(g.JITRequests, req)
	if g.FailJIT {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "boom"})
		return
	}
	name, _ := req["name"].(string)
	for _, rn := range g.runners {
		if rn.Name == name {
			writeJSON(w, http.StatusConflict, map[string]string{"message": "Already exists"})
			return
		}
	}
	g.nextRunner++
	g.jitSeq++
	var labels []string
	if ls, ok := req["labels"].([]any); ok {
		for _, l := range ls {
			s, _ := l.(string)
			labels = append(labels, s)
		}
	}
	rn := &GitHubRunner{ID: g.nextRunner, Name: name, Status: "offline", Scope: scopeOf(r), Labels: labels}
	g.runners[rn.ID] = rn
	jit := fmt.Sprintf("FAKEJITCONFIG%04dxyzzy", g.jitSeq)
	g.IssuedJIT = append(g.IssuedJIT, jit)
	writeJSON(w, http.StatusCreated, map[string]any{
		"runner":             map[string]any{"id": rn.ID, "name": rn.Name, "status": rn.Status},
		"encoded_jit_config": jit,
	})
}

func (g *GitHub) deleteRunner(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.FailDelete {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "boom"})
		return
	}
	if _, ok := g.runners[id]; !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	delete(g.runners, id)
	g.Deleted = append(g.Deleted, id)
	w.WriteHeader(http.StatusNoContent)
}
