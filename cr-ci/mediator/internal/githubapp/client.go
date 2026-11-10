package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the GitHub REST API origin.
const DefaultBaseURL = "https://api.github.com"

const (
	apiVersion  = "2022-11-28"
	perPage     = 100
	maxPages    = 50
	maxBodySize = 10 << 20
	errBodySize = 300
)

// Scope selects the runner endpoints: repository-level or organization-level.
type Scope string

const (
	ScopeRepo Scope = "repo"
	ScopeOrg  Scope = "org"
)

// Repo is an owner/name pair.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// APIError is a non-2xx GitHub response. Body is a truncated copy of the
// error body (GitHub error bodies carry no credentials).
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// IsStatus reports whether err is an APIError with the given status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// Client is a GitHub App client. Construct it with New; it is safe for
// concurrent use.
type Client struct {
	baseURL    string
	http       *http.Client
	appID      int64
	scope      Scope
	privateKey func(context.Context) (*rsa.PrivateKey, error)
	now        func() time.Time

	mu         sync.Mutex
	installIDs map[string]int64       // "repo:owner/name" or "org:owner" -> installation ID
	tokens     map[int64]installToken // installation ID -> cached token
}

type installToken struct {
	token   string
	expires time.Time
}

// Options configures a Client.
type Options struct {
	BaseURL string       // default DefaultBaseURL
	HTTP    *http.Client // default: 30 s timeout
	AppID   int64
	Scope   Scope
	// PrivateKey returns the App private key (from Secret Manager in
	// production). It is called whenever a JWT is needed; callers cache.
	PrivateKey func(context.Context) (*rsa.PrivateKey, error)
	Now        func() time.Time // default time.Now
}

// New returns a Client.
func New(o Options) *Client {
	c := &Client{
		baseURL:    strings.TrimSuffix(o.BaseURL, "/"),
		http:       o.HTTP,
		appID:      o.AppID,
		scope:      o.Scope,
		privateKey: o.PrivateKey,
		now:        o.Now,
		installIDs: map[string]int64{},
		tokens:     map[int64]installToken{},
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// ---- authentication --------------------------------------------------------

func (c *Client) appJWT(ctx context.Context) (string, error) {
	key, err := c.privateKey(ctx)
	if err != nil {
		return "", fmt.Errorf("loading github app private key: %w", err)
	}
	return signAppJWT(key, c.appID, c.now())
}

func (c *Client) installKey(r Repo) string {
	if c.scope == ScopeOrg {
		return "org:" + strings.ToLower(r.Owner)
	}
	return "repo:" + strings.ToLower(r.String())
}

func (c *Client) installationID(ctx context.Context, r Repo) (int64, error) {
	key := c.installKey(r)
	c.mu.Lock()
	id, ok := c.installIDs[key]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	jwt, err := c.appJWT(ctx)
	if err != nil {
		return 0, err
	}
	path := "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name) + "/installation"
	if c.scope == ScopeOrg {
		path = "/orgs/" + url.PathEscape(r.Owner) + "/installation"
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, path, jwt, nil, &out); err != nil {
		return 0, err
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("github: no installation id for %s", r)
	}
	c.mu.Lock()
	c.installIDs[key] = out.ID
	c.mu.Unlock()
	return out.ID, nil
}

// installationToken returns a cached installation token for r, minting a new
// one when none is cached or the cached one expires within five minutes.
func (c *Client) installationToken(ctx context.Context, r Repo) (string, int64, error) {
	id, err := c.installationID(ctx, r)
	if err != nil {
		return "", 0, err
	}
	c.mu.Lock()
	t, ok := c.tokens[id]
	c.mu.Unlock()
	if ok && c.now().Add(tokenRefreshMargin).Before(t.expires) {
		return t.token, id, nil
	}
	jwt, err := c.appJWT(ctx)
	if err != nil {
		return "", 0, err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := "/app/installations/" + strconv.FormatInt(id, 10) + "/access_tokens"
	if err := c.do(ctx, http.MethodPost, path, jwt, nil, &out); err != nil {
		if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusUnauthorized) {
			// The App may have been reinstalled under a new installation
			// ID: look it up again on the next call.
			c.mu.Lock()
			delete(c.installIDs, c.installKey(r))
			delete(c.tokens, id)
			c.mu.Unlock()
		}
		return "", 0, err
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return "", 0, errors.New("github: installation token response is missing token or expires_at")
	}
	c.mu.Lock()
	c.tokens[id] = installToken{token: out.Token, expires: out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, id, nil
}

// api calls an installation-authenticated endpoint. A 401 drops the cached
// token so the next call mints a fresh one.
func (c *Client) api(ctx context.Context, r Repo, method, path string, body, out any) error {
	tok, id, err := c.installationToken(ctx, r)
	if err != nil {
		return err
	}
	err = c.do(ctx, method, path, "token "+tok, body, out)
	if IsStatus(err, http.StatusUnauthorized) {
		c.mu.Lock()
		delete(c.tokens, id)
		c.mu.Unlock()
	}
	return err
}

// do performs one request. auth is a full Authorization value, or a bare JWT
// (then sent as "Bearer <jwt>").
func (c *Client) do(ctx context.Context, method, path, auth string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(auth, "token ") {
		auth = "Bearer " + auth
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "cr-ci-mediator")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, stripQuery(path), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, errBodySize))
		return &APIError{Method: method, Path: stripQuery(path), Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodySize))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodySize)).Decode(out); err != nil {
		// Never include the body: success bodies can carry tokens.
		return fmt.Errorf("github %s %s: decoding response failed", method, stripQuery(path))
	}
	return nil
}

func stripQuery(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// ---- Actions endpoints -----------------------------------------------------

// Run is the subset of a workflow run the mediator uses.
type Run struct {
	ID             int64    `json:"id"`
	Event          string   `json:"event"`
	Status         string   `json:"status"`
	Repository     *RepoRef `json:"repository"`
	HeadRepository *RepoRef `json:"head_repository"`
}

// RepoRef is a repository reference inside a run.
type RepoRef struct {
	FullName string `json:"full_name"`
}

// Job is the subset of a workflow job the mediator uses.
type Job struct {
	ID         int64     `json:"id"`
	RunID      int64     `json:"run_id"`
	Status     string    `json:"status"`
	Labels     []string  `json:"labels"`
	RunnerName string    `json:"runner_name"`
	CreatedAt  time.Time `json:"created_at"`
}

// Runner is a registered self-hosted runner.
type Runner struct {
	ID     int64         `json:"id"`
	Name   string        `json:"name"`
	Status string        `json:"status"` // "online" or "offline"
	Busy   bool          `json:"busy"`
	Labels []RunnerLabel `json:"labels"`
}

// RunnerLabel is one label of a registered runner.
type RunnerLabel struct {
	Name string `json:"name"`
}

func repoPath(r Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

func (c *Client) runnersPath(r Repo) string {
	if c.scope == ScopeOrg {
		return "/orgs/" + url.PathEscape(r.Owner) + "/actions/runners"
	}
	return repoPath(r) + "/actions/runners"
}

// GetRun returns workflow run runID of r.
func (c *Client) GetRun(ctx context.Context, r Repo, runID int64) (*Run, error) {
	var run Run
	if err := c.api(ctx, r, http.MethodGet, repoPath(r)+"/actions/runs/"+strconv.FormatInt(runID, 10), nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// GetJob returns workflow job jobID of r.
func (c *Client) GetJob(ctx context.Context, r Repo, jobID int64) (*Job, error) {
	var j Job
	if err := c.api(ctx, r, http.MethodGet, repoPath(r)+"/actions/jobs/"+strconv.FormatInt(jobID, 10), nil, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// ListRuns returns the workflow runs of r with the given status (for
// example "queued" or "in_progress").
func (c *Client) ListRuns(ctx context.Context, r Repo, status string) ([]Run, error) {
	var all []Run
	for page := 1; page <= maxPages; page++ {
		var out struct {
			TotalCount   int   `json:"total_count"`
			WorkflowRuns []Run `json:"workflow_runs"`
		}
		q := url.Values{"status": {status}, "per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		if err := c.api(ctx, r, http.MethodGet, repoPath(r)+"/actions/runs?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.WorkflowRuns...)
		if len(out.WorkflowRuns) < perPage || len(all) >= out.TotalCount {
			return all, nil
		}
	}
	return nil, fmt.Errorf("github: more than %d pages of %s runs in %s", maxPages, status, r)
}

// ListJobs returns the jobs of the latest attempt of run runID.
func (c *Client) ListJobs(ctx context.Context, r Repo, runID int64) ([]Job, error) {
	var all []Job
	for page := 1; page <= maxPages; page++ {
		var out struct {
			TotalCount int   `json:"total_count"`
			Jobs       []Job `json:"jobs"`
		}
		q := url.Values{"filter": {"latest"}, "per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		if err := c.api(ctx, r, http.MethodGet, repoPath(r)+"/actions/runs/"+strconv.FormatInt(runID, 10)+"/jobs?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Jobs...)
		if len(out.Jobs) < perPage || len(all) >= out.TotalCount {
			return all, nil
		}
	}
	return nil, fmt.Errorf("github: more than %d pages of jobs in run %d", maxPages, runID)
}

// ListRunners returns the self-hosted runners visible for r (repo runners, or
// the org's runners with org scope).
func (c *Client) ListRunners(ctx context.Context, r Repo) ([]Runner, error) {
	var all []Runner
	for page := 1; page <= maxPages; page++ {
		var out struct {
			TotalCount int      `json:"total_count"`
			Runners    []Runner `json:"runners"`
		}
		q := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		if err := c.api(ctx, r, http.MethodGet, c.runnersPath(r)+"?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Runners...)
		if len(out.Runners) < perPage || len(all) >= out.TotalCount {
			return all, nil
		}
	}
	return nil, fmt.Errorf("github: more than %d pages of runners for %s", maxPages, r)
}

// JITRequest is the generate-jitconfig request body.
type JITRequest struct {
	Name          string   `json:"name"`
	RunnerGroupID int64    `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder"`
}

// GenerateJITConfig registers a just-in-time runner and returns its runner ID
// and encoded JIT config. The config is a credential: never log it.
func (c *Client) GenerateJITConfig(ctx context.Context, r Repo, req JITRequest) (runnerID int64, jitConfig string, err error) {
	var out struct {
		Runner           Runner `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := c.api(ctx, r, http.MethodPost, c.runnersPath(r)+"/generate-jitconfig", req, &out); err != nil {
		return 0, "", err
	}
	if out.Runner.ID == 0 || out.EncodedJITConfig == "" {
		return out.Runner.ID, "", errors.New("github: generate-jitconfig response is missing runner.id or encoded_jit_config")
	}
	return out.Runner.ID, out.EncodedJITConfig, nil
}

// DeleteRunner removes a runner registration. A runner that is already gone
// is not an error.
func (c *Client) DeleteRunner(ctx context.Context, r Repo, runnerID int64) error {
	err := c.api(ctx, r, http.MethodDelete, c.runnersPath(r)+"/"+strconv.FormatInt(runnerID, 10), nil, nil)
	if IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
