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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// GitHub App installation association authorization.
//
// A project may use a GitHub App installation token only when the
// association between the project and the installation is AUTHORIZED. The
// same check runs when an association is recorded (project create, project
// clone, the installation webhook and discovery) and every time a token is
// minted (mintGitHubAppToken, which every delivery leg reaches), so an
// association recorded before this check existed is re-validated at its next
// mint and yields no token if it is not authorized. Nothing is migrated.
//
// An association is authorized when ALL of these hold:
//
//  1. Host: the project's git remote is on the GitHub host the App is
//     configured for (github.com, or the GHES/GHE.com host derived from
//     the configured API base URL), compared after canonicalization.
//  2. Coverage: the installation exists, is active, and its account is the
//     remote's owner. The installation token is scoped by bare repository
//     name, so without this an installation of orgA could mint orgA/foo
//     for a project whose remote is orgB/foo.
//  3. Proof, one of:
//     a. an admin binding: a hub admin bound this installation to this
//     project's current remote (PUT /projects/{id}/github-installation,
//     admin only) and that admin is still an active hub admin; or
//     b. the project creator's OWN GitHub token (user-scope GITHUB_TOKEN
//     secret) is accepted by GitHub for the repository: GET /repos with
//     that token reports at least pull. Write-capable token permissions
//     need push; a pull-only proof yields a read-only token. The App's
//     own installation token is never used as proof.
//
// Any inconclusive step (a store or secret lookup error, a GitHub error,
// timeout or rate limit) refuses: no association and no token.

// githubInstallationBindingAnnotation is the server-owned project annotation
// recording an admin binding. Only handleProjectGitHubInstallation writes it,
// and only for a hub admin. No generic annotation write path exists for
// projects, and project clone copies an allowlist of annotations that does
// not include it.
const githubInstallationBindingAnnotation = "scion.dev/github-installation-binding"

// githubInstallationBinding is the JSON value of
// githubInstallationBindingAnnotation.
type githubInstallationBinding struct {
	InstallationID int64     `json:"installation_id"`
	Remote         string    `json:"remote"` // util.NormalizeGitRemote of the bound remote
	BoundBy        string    `json:"bound_by"`
	BoundAt        time.Time `json:"bound_at"`
}

// GitHub App status error codes for an association that fails the check.
const (
	// githubAppErrAssociationUnauthorized: the association is not authorized
	// (host, coverage or proof failed definitively).
	githubAppErrAssociationUnauthorized = "association_unauthorized"
	// githubAppErrAssociationCheckFailed: the check could not complete
	// (lookup error, GitHub error, timeout or rate limit); no token is
	// minted until it succeeds.
	githubAppErrAssociationCheckFailed = "association_check_failed"
)

var (
	// errGitHubAssociationUnauthorized is a definitive refusal.
	errGitHubAssociationUnauthorized = errors.New("github app installation association is not authorized")
	// errGitHubAssociationCheckFailed is an inconclusive check; it refuses
	// too (fail closed).
	errGitHubAssociationCheckFailed = errors.New("github app installation association check failed")
)

// githubAssociationGrant is the result of an authorized association check.
type githubAssociationGrant struct {
	Owner string
	Repo  string
	// WriteAllowed reports whether the proof supports write-capable token
	// permissions (admin binding, or creator push access). When false, the
	// minted token is reduced to read-only permissions.
	WriteAllowed bool
	// Basis names the proof, for logs and the admin report.
	Basis string
}

// Proof basis names.
const (
	githubAssociationBasisAdminBinding = "admin_binding"
	githubAssociationBasisCreatorPush  = "creator_push"
	githubAssociationBasisCreatorPull  = "creator_pull"
)

// githubAppWebHost returns the canonical git host of the configured GitHub
// App (github.com unless an API base URL for GHES/GHE.com is configured).
func (s *Server) githubAppWebHost() string {
	s.mu.RLock()
	apiBase := s.config.GitHubAppConfig.APIBaseURL
	s.mu.RUnlock()
	return githubapp.WebHostForAPIBaseURL(apiBase)
}

// githubAppAPIBase returns the configured GitHub API base URL ("" means
// GitHub.com).
func (s *Server) githubAppAPIBase() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config.GitHubAppConfig.APIBaseURL
}

// githubOwnerRepo returns "owner/repo" for remote when its host is the
// configured GitHub host, and "" otherwise. Every association and mint path
// parses the remote through this, so a remote on any other host never
// matches an installation.
func (s *Server) githubOwnerRepo(remote string) string {
	owner, repo, ok := githubapp.RemoteOwnerRepo(remote, s.githubAppWebHost())
	if !ok {
		return ""
	}
	return owner + "/" + repo
}

// authorizeGitHubAppAssociation decides whether project may use
// installationID. It returns a grant, or an error wrapping
// errGitHubAssociationUnauthorized (definitive) or
// errGitHubAssociationCheckFailed (inconclusive). Both refuse.
func (s *Server) authorizeGitHubAppAssociation(ctx context.Context, project *store.Project, installationID int64) (githubAssociationGrant, error) {
	if project == nil || installationID == 0 {
		return githubAssociationGrant{}, fmt.Errorf("%w: no project or installation", errGitHubAssociationUnauthorized)
	}

	// 1. Host.
	host := s.githubAppWebHost()
	owner, repo, ok := githubapp.RemoteOwnerRepo(project.GitRemote, host)
	if !ok {
		return githubAssociationGrant{}, fmt.Errorf("%w: the project's git remote is not an owner/repo on the configured GitHub host %q", errGitHubAssociationUnauthorized, host)
	}

	// 2. Coverage.
	inst, err := s.store.GetGitHubInstallation(ctx, installationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return githubAssociationGrant{}, fmt.Errorf("%w: installation %d not found", errGitHubAssociationUnauthorized, installationID)
		}
		return githubAssociationGrant{}, fmt.Errorf("%w: installation lookup: %w", errGitHubAssociationCheckFailed, err)
	}
	if inst.Status != store.GitHubInstallationStatusActive {
		return githubAssociationGrant{}, fmt.Errorf("%w: installation %d is %s", errGitHubAssociationUnauthorized, installationID, inst.Status)
	}
	if inst.AccountLogin == "" || !strings.EqualFold(inst.AccountLogin, owner) {
		return githubAssociationGrant{}, fmt.Errorf("%w: installation %d belongs to account %q, not the remote owner %q", errGitHubAssociationUnauthorized, installationID, inst.AccountLogin, owner)
	}

	// 3a. Admin binding.
	bound, err := s.githubAdminBindingValid(ctx, project, installationID)
	if err != nil {
		return githubAssociationGrant{}, err
	}
	if bound {
		return githubAssociationGrant{Owner: owner, Repo: repo, WriteAllowed: true, Basis: githubAssociationBasisAdminBinding}, nil
	}

	// 3b. The creator's own GitHub identity.
	access, err := s.githubCreatorRepoAccess(ctx, project, installationID, owner, repo)
	if err != nil {
		return githubAssociationGrant{}, err
	}
	switch {
	case access.Push:
		return githubAssociationGrant{Owner: owner, Repo: repo, WriteAllowed: true, Basis: githubAssociationBasisCreatorPush}, nil
	case access.Pull:
		return githubAssociationGrant{Owner: owner, Repo: repo, WriteAllowed: false, Basis: githubAssociationBasisCreatorPull}, nil
	default:
		return githubAssociationGrant{}, fmt.Errorf("%w: the project creator's GitHub identity has no access to %s/%s", errGitHubAssociationUnauthorized, owner, repo)
	}
}

// githubAdminBindingValid reports whether project carries an admin binding
// for installationID on its current remote, made by a user who is still an
// active hub admin. A malformed binding is not a binding. A user lookup
// error is inconclusive.
func (s *Server) githubAdminBindingValid(ctx context.Context, project *store.Project, installationID int64) (bool, error) {
	raw := project.Annotations[githubInstallationBindingAnnotation]
	if raw == "" {
		return false, nil
	}
	var b githubInstallationBinding
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return false, nil
	}
	if b.InstallationID != installationID || b.BoundBy == "" {
		return false, nil
	}
	if b.Remote == "" || b.Remote != util.NormalizeGitRemote(project.GitRemote) {
		return false, nil
	}
	isAdmin, err := s.githubBindingUserIsAdmin(ctx, b.BoundBy)
	if err != nil {
		return false, fmt.Errorf("%w: binding admin lookup: %w", errGitHubAssociationCheckFailed, err)
	}
	return isAdmin, nil
}

// githubBindingUserIsAdmin reports whether userID may still bind
// installations: an active user whom the authorization service grants
// githubInstallationBindPermission, the same decision the binding request
// itself passed (role bindings, not the User.Role field alone). Used at mint
// time, when no request identity exists.
func (s *Server) githubBindingUserIsAdmin(ctx context.Context, userID string) (bool, error) {
	u, err := s.store.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && u == nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if u.Status != store.UserStatusActive {
		return false, nil
	}
	if s.authzService == nil {
		return false, nil
	}
	target, evidence, ok := routeGuardTarget(githubInstallationBindPermission)
	if !ok {
		return false, nil
	}
	identity := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "")
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:      principalContextForIdentity(identity),
		Credential:     credentialContextForIdentity(identity),
		Resource:       target,
		Action:         Action(githubInstallationBindPermission.Action),
		Permission:     githubInstallationBindPermission.Permission,
		TargetEvidence: evidence,
	})
	return decision.Allowed, nil
}

// githubCreatorRepoAccess asks GitHub, with the project creator's own
// user-scope GITHUB_TOKEN, what the creator may do on owner/repo. No creator,
// no secret backend or no stored token is a definitive refusal; a secret
// lookup error or an inconclusive GitHub answer is a failed check.
func (s *Server) githubCreatorRepoAccess(ctx context.Context, project *store.Project, installationID int64, owner, repo string) (githubapp.RepoAccess, error) {
	if project.CreatedBy == "" || s.secretBackend == nil {
		return githubapp.RepoAccess{}, fmt.Errorf("%w: no admin binding and no creator GitHub identity to verify", errGitHubAssociationUnauthorized)
	}
	sv, err := s.secretBackend.Get(ctx, "GITHUB_TOKEN", "user", project.CreatedBy)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return githubapp.RepoAccess{}, fmt.Errorf("%w: creator GitHub token lookup: %w", errGitHubAssociationCheckFailed, err)
	}
	if err != nil || sv == nil || sv.Value == "" {
		return githubapp.RepoAccess{}, fmt.Errorf("%w: no admin binding and the project creator has no GitHub token to verify repository access", errGitHubAssociationUnauthorized)
	}

	apiBase := s.githubAppAPIBase()
	key := githubAccessCacheKey(project.ID, installationID, apiBase, owner, repo, project.CreatedBy, sv.Value)
	if access, denied, ok := s.githubAccessCache.get(key); ok {
		if denied {
			return githubapp.RepoAccess{}, fmt.Errorf("%w: the project creator's GitHub identity has no access to %s/%s", errGitHubAssociationUnauthorized, owner, repo)
		}
		return access, nil
	}

	check := s.githubRepoAccessCheck
	if check == nil {
		check = githubapp.CheckUserRepoAccess
	}
	access, err := check(ctx, apiBase, sv.Value, owner, repo)
	if errors.Is(err, githubapp.ErrRepoAccessDenied) {
		s.githubAccessCache.put(key, project.ID, githubapp.RepoAccess{}, true)
		return githubapp.RepoAccess{}, fmt.Errorf("%w: the project creator's GitHub identity has no access to %s/%s", errGitHubAssociationUnauthorized, owner, repo)
	}
	if err != nil {
		// Inconclusive: not cached, so the next mint asks again.
		return githubapp.RepoAccess{}, fmt.Errorf("%w: %w", errGitHubAssociationCheckFailed, err)
	}
	s.githubAccessCache.put(key, project.ID, access, false)
	return access, nil
}

// githubAccessCacheKey identifies one access check. The token enters only as
// a hash, so the cache never holds credential material, and a changed token,
// creator, remote or installation is a different entry.
func githubAccessCacheKey(projectID string, installationID int64, apiBase, owner, repo, creatorID, token string) string {
	tokenHash := sha256.Sum256([]byte(token))
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s/%s\x00%s\x00%s",
		projectID, installationID, strings.ToLower(apiBase), strings.ToLower(owner), strings.ToLower(repo), creatorID, hex.EncodeToString(tokenHash[:]))))
	return hex.EncodeToString(h[:])
}

// Access-check cache lifetimes: a grant is reused for a few minutes to keep
// GitHub calls bounded (one per project per window on the creator's own rate
// limit); a denial briefly, so a fixed token is picked up soon.
const (
	githubAccessCacheGrantTTL  = 10 * time.Minute
	githubAccessCacheDenialTTL = time.Minute
)

// githubRepoAccessCache caches access-check outcomes. The zero value is
// ready to use.
type githubRepoAccessCache struct {
	mu      sync.Mutex
	entries map[string]githubAccessCacheEntry
}

type githubAccessCacheEntry struct {
	projectID string
	access    githubapp.RepoAccess
	denied    bool
	expires   time.Time
}

func (c *githubRepoAccessCache) get(key string) (githubapp.RepoAccess, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return githubapp.RepoAccess{}, false, false
	}
	if !timeNow().Before(e.expires) {
		delete(c.entries, key)
		return githubapp.RepoAccess{}, false, false
	}
	return e.access, e.denied, true
}

func (c *githubRepoAccessCache) put(key, projectID string, access githubapp.RepoAccess, denied bool) {
	ttl := githubAccessCacheGrantTTL
	if denied {
		ttl = githubAccessCacheDenialTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]githubAccessCacheEntry)
	}
	now := timeNow()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = githubAccessCacheEntry{projectID: projectID, access: access, denied: denied, expires: now.Add(ttl)}
}

// invalidateProject drops every cached outcome for projectID. Called when
// the project's association changes.
func (c *githubRepoAccessCache) invalidateProject(projectID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if e.projectID == projectID {
			delete(c.entries, k)
		}
	}
}

// githubTokenPermissionsForGrant returns the permission set to request for
// a token minted under grant, from the project's stored permissions:
//
//   - Only the six known fields are ever requested, each as "read" or
//     "write"; any other stored value is dropped.
//   - "write" survives only when grant.WriteAllowed; otherwise it is
//     lowered to "read". Nothing is ever added above the stored value
//     except by the fallback below.
//   - The result is NEVER empty: GitHub grants every installation
//     permission when a token request carries no permissions. With nothing
//     usable stored, DefaultTokenPermissions intersected with the proven
//     level is used (write fields lowered to read for a pull-only grant),
//     and metadata:read if even that were empty.
func githubTokenPermissionsForGrant(stored *store.GitHubTokenPermissions, grant githubAssociationGrant) githubapp.TokenPermissions {
	level := func(v string) string {
		switch v {
		case "write":
			if grant.WriteAllowed {
				return "write"
			}
			return "read"
		case "read":
			return "read"
		default:
			return ""
		}
	}
	scope := func(p githubapp.TokenPermissions) githubapp.TokenPermissions {
		return githubapp.TokenPermissions{
			Contents:     level(p.Contents),
			PullRequests: level(p.PullRequests),
			Issues:       level(p.Issues),
			Metadata:     level(p.Metadata),
			Checks:       level(p.Checks),
			Actions:      level(p.Actions),
		}
	}
	var perms githubapp.TokenPermissions
	if stored != nil {
		perms = scope(githubapp.TokenPermissions{
			Contents:     stored.Contents,
			PullRequests: stored.PullRequests,
			Issues:       stored.Issues,
			Metadata:     stored.Metadata,
			Checks:       stored.Checks,
			Actions:      stored.Actions,
		})
	}
	if perms == (githubapp.TokenPermissions{}) {
		perms = scope(githubapp.DefaultTokenPermissions())
	}
	if perms == (githubapp.TokenPermissions{}) {
		perms = githubapp.TokenPermissions{Metadata: "read"}
	}
	return perms
}

// validGitHubTokenPermissionLevel reports whether v may be stored for a
// project GitHub token permission field.
func validGitHubTokenPermissionLevel(v string) bool {
	return v == "" || v == "read" || v == "write"
}

// validateGitHubTokenPermissions checks every field of a project GitHub token
// permission set, returning the name of the first invalid field.
func validateGitHubTokenPermissions(p store.GitHubTokenPermissions) (string, bool) {
	for _, f := range []struct{ name, value string }{
		{"contents", p.Contents},
		{"pull_requests", p.PullRequests},
		{"issues", p.Issues},
		{"metadata", p.Metadata},
		{"checks", p.Checks},
		{"actions", p.Actions},
	} {
		if !validGitHubTokenPermissionLevel(f.value) {
			return f.name, false
		}
	}
	return "", true
}

// githubAssociationErrorCode maps an authorization error to the project
// GitHub App status error code.
func githubAssociationErrorCode(err error) string {
	if errors.Is(err, errGitHubAssociationCheckFailed) {
		return githubAppErrAssociationCheckFailed
	}
	return githubAppErrAssociationUnauthorized
}

// setGitHubInstallationAssociation records installationID on project with
// an unchecked status and drops cached access outcomes for it.
func (s *Server) setGitHubInstallationAssociation(project *store.Project, installationID int64) {
	id := installationID
	project.GitHubInstallationID = &id
	project.GitHubAppStatus = &store.GitHubAppProjectStatus{
		State:       store.GitHubAppStateUnchecked,
		LastChecked: timeNow(),
	}
	s.invalidateGitHubAssociation(project.ID)
}

// errGitHubTokenSourceDenied refuses a github-token-source-project label.
var errGitHubTokenSourceDenied = errors.New("github token source project not authorized for this agent")

// AuthorizeGitHubTokenSourceProject implements GitHubAppTokenMinter. The
// label is free-form agent input, so it is honoured only when the agent's
// authoritative source user (the terminal user of its provenance chain)
// is active and holds agent.create on sourceProject: the same decision a
// direct agent creation there takes, which would already deliver a token
// from that project's installation. Read access is not enough. Any lookup
// failure refuses. The source project's own association is still checked
// by mintGitHubAppToken.
func (s *Server) AuthorizeGitHubTokenSourceProject(ctx context.Context, agent *store.Agent, sourceProject *store.Project) error {
	if agent == nil || sourceProject == nil || agent.ID == "" || sourceProject.ID == "" {
		return fmt.Errorf("%w: no agent or source project", errGitHubTokenSourceDenied)
	}
	if s.authzService == nil {
		return fmt.Errorf("%w: no authorization service", errGitHubTokenSourceDenied)
	}
	source, err := s.authzService.executionSourceResolver().ResolveExecutionSource(ctx, agent)
	if err != nil || source == nil {
		return fmt.Errorf("%w: agent has no authoritative source user: %v", errGitHubTokenSourceDenied, err)
	}
	if source.Status != store.UserStatusActive {
		return fmt.Errorf("%w: source user %s is not active", errGitHubTokenSourceDenied, source.ID)
	}
	identity := NewAuthenticatedUser(source.ID, source.Email, source.DisplayName, source.Role, "")
	if decision := s.agentCreateDecision(ctx, identity, sourceProject.ID); !decision.Allowed {
		return fmt.Errorf("%w: source user %s may not create agents in project %s", errGitHubTokenSourceDenied, source.ID, sourceProject.ID)
	}
	return nil
}

// githubAssociationReportPath is the sub-path of the admin-only
// GET /api/v1/github-app/installations/ subtree serving the association
// report.
const githubAssociationReportPath = "association-report"

// githubAssociationReportEntry is one associated project in the report.
type githubAssociationReportEntry struct {
	ProjectID      string `json:"project_id"`
	ProjectName    string `json:"project_name"`
	GitRemote      string `json:"git_remote"`
	InstallationID int64  `json:"installation_id"`
	Authorized     bool   `json:"authorized"`
	Basis          string `json:"basis,omitempty"`
	WriteAllowed   bool   `json:"write_allowed"`
	ErrorCode      string `json:"error_code,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// Admin association report bounds. A full build enumerates every project
// and may ask GitHub once per associated project (on the creator's own rate
// limit, through the access-check cache), so it is never run per request:
//
//   - The report is cached and reused for githubAssociationReportTTL, the
//     lifetime of a cached grant on the mint path.
//   - It is invalidated per project by the same hook as the mint-path
//     cache (invalidateGitHubAssociation): a project whose association
//     changed is re-evaluated alone at the next read, with no enumeration.
//   - Full builds, whether the cache expired or an admin asked for an
//     explicit refresh (?refresh=true), run at most once per
//     githubAssociationReportMinRebuildInterval hub-wide; an explicit
//     refresh inside the interval is refused with 429 and Retry-After.
//     Concurrent reads share one build.
//   - A build pages through at most githubAssociationReportMaxProjects
//     projects and says when more exist past that (truncated).
//
// The report is informational and never authorizes anything: every mint
// re-runs authorizeGitHubAppAssociation live. Inputs that change without an
// association change (an installation suspended, a binding admin demoted,
// a creator storing a new GitHub token) show in the report after the TTL or
// an explicit refresh, as they do on the mint path's cached grants.
const (
	githubAssociationReportTTL                = githubAccessCacheGrantTTL
	githubAssociationReportMinRebuildInterval = time.Minute
	githubAssociationReportMaxProjects        = 10000
	// githubAssociationReportPageSize is the ListProjects page a build
	// reads at a time; the store caps a page at 1000.
	githubAssociationReportPageSize = 500
)

// errGitHubAssociationReportRateLimited refuses a full report build inside
// githubAssociationReportMinRebuildInterval.
var errGitHubAssociationReportRateLimited = errors.New("github app association report rebuild rate limited")

// githubAssociationReport is one served association report.
type githubAssociationReport struct {
	Entries     []githubAssociationReportEntry
	GeneratedAt time.Time
	Truncated   bool
}

// githubAssociationReportCache holds the last association report. The zero
// value is empty and ready to use.
type githubAssociationReportCache struct {
	// buildMu serializes reads that build or update the report, so
	// concurrent reads share one build. It is never held by invalidate,
	// so an association change does not wait for GitHub.
	buildMu sync.Mutex

	mu            sync.Mutex
	valid         bool
	entries       map[string]githubAssociationReportEntry // by project ID
	generatedAt   time.Time
	lastFullBuild time.Time
	truncated     bool
	seq           uint64
	dirty         map[string]uint64 // project ID -> seq of its latest invalidation

	// maxProjects overrides githubAssociationReportMaxProjects when
	// positive (tests).
	maxProjects int
}

// invalidate marks projectID for re-evaluation at the next read.
func (c *githubAssociationReportCache) invalidate(projectID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dirty == nil {
		c.dirty = make(map[string]uint64)
	}
	c.seq++
	c.dirty[projectID] = c.seq
}

// invalidateGitHubAssociation drops every cached association outcome for
// projectID: the mint path's access checks and the admin report's entry.
// Every association change goes through here.
func (s *Server) invalidateGitHubAssociation(projectID string) {
	s.githubAccessCache.invalidateProject(projectID)
	s.githubAssociationReportCache.invalidate(projectID)
}

// githubAssociationReportEntryFor evaluates p against
// authorizeGitHubAppAssociation, without changing anything.
func (s *Server) githubAssociationReportEntryFor(ctx context.Context, p *store.Project) githubAssociationReportEntry {
	e := githubAssociationReportEntry{
		ProjectID:      p.ID,
		ProjectName:    p.Name,
		GitRemote:      p.GitRemote,
		InstallationID: *p.GitHubInstallationID,
	}
	grant, authzErr := s.authorizeGitHubAppAssociation(ctx, p, *p.GitHubInstallationID)
	if authzErr != nil {
		e.ErrorCode = githubAssociationErrorCode(authzErr)
		e.Reason = authzErr.Error()
	} else {
		e.Authorized = true
		e.Basis = grant.Basis
		e.WriteAllowed = grant.WriteAllowed
	}
	return e
}

// githubAssociationReport returns the report of every project associated
// with a GitHub App installation, evaluated against
// authorizeGitHubAppAssociation without changing anything. Admins use it to
// find associations (including ones recorded before the check existed) that
// no longer yield a token, and fix them by an admin binding or the creator
// storing a GitHub token.
//
// It serves the cached report when it is still fresh, re-evaluating only
// projects invalidated since; otherwise, or when refresh is set, it builds
// the report again, subject to the rebuild interval. A refused rebuild
// returns errGitHubAssociationReportRateLimited and the wait.
func (s *Server) githubAssociationReport(ctx context.Context, refresh bool) (githubAssociationReport, time.Duration, error) {
	c := &s.githubAssociationReportCache
	c.buildMu.Lock()
	defer c.buildMu.Unlock()

	now := timeNow()
	c.mu.Lock()
	fresh := c.valid && now.Before(c.generatedAt.Add(githubAssociationReportTTL))
	nextBuild := c.lastFullBuild.Add(githubAssociationReportMinRebuildInterval)
	limited := !c.lastFullBuild.IsZero() && now.Before(nextBuild)
	startSeq := c.seq
	dirty := make([]string, 0, len(c.dirty))
	for id := range c.dirty {
		dirty = append(dirty, id)
	}
	c.mu.Unlock()

	switch {
	case refresh || !fresh:
		if limited && (refresh || !c.valid) {
			return githubAssociationReport{}, nextBuild.Sub(now), errGitHubAssociationReportRateLimited
		}
		if !limited {
			if err := s.buildGitHubAssociationReport(ctx, now, startSeq); err != nil {
				return githubAssociationReport{}, 0, err
			}
			break
		}
		// Expired but inside the rebuild interval (the clock moved
		// past the TTL early): serve the last report, updated below.
		fallthrough
	default:
		if err := s.updateGitHubAssociationReport(ctx, dirty, startSeq); err != nil {
			return githubAssociationReport{}, 0, err
		}
	}
	return c.snapshot(), 0, nil
}

// buildGitHubAssociationReport enumerates the associated projects and
// replaces the cached report. Invalidations made during the build stay
// pending, so the next read re-evaluates those projects. A build whose
// context ends commits nothing.
func (s *Server) buildGitHubAssociationReport(ctx context.Context, now time.Time, startSeq uint64) error {
	limit := s.githubAssociationReportCache.maxProjects
	if limit <= 0 {
		limit = githubAssociationReportMaxProjects
	}
	entries := make(map[string]githubAssociationReportEntry)
	listed, truncated, cursor := 0, false, ""
pages:
	for {
		// A cancelled request turns every remaining check into a refusal;
		// commit nothing, so the cache keeps no such entries and the
		// rebuild interval is not used up.
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit:          githubAssociationReportPageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return err
		}
		for i := range page.Items {
			if listed == limit {
				truncated = true
				break pages
			}
			listed++
			p := &page.Items[i]
			if p.GitHubInstallationID == nil {
				continue
			}
			entries[p.ID] = s.githubAssociationReportEntryFor(ctx, p)
		}
		if page.NextCursor == "" {
			break
		}
		if listed == limit {
			// More projects exist past the cap.
			truncated = true
			break
		}
		cursor = page.NextCursor
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c := &s.githubAssociationReportCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.valid = true
	c.entries = entries
	c.generatedAt = now
	c.lastFullBuild = now
	c.truncated = truncated
	c.clearDirtyLocked(startSeq)
	return nil
}

// updateGitHubAssociationReport re-evaluates the invalidated projects of
// the cached report: a project that no longer exists or has no
// installation leaves the report, any other is evaluated again.
func (s *Server) updateGitHubAssociationReport(ctx context.Context, dirty []string, startSeq uint64) error {
	if len(dirty) == 0 {
		return nil
	}
	updated := make(map[string]*githubAssociationReportEntry, len(dirty))
	for _, id := range dirty {
		p, err := s.store.GetProject(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (p == nil || p.GitHubInstallationID == nil)) {
			updated[id] = nil
			continue
		}
		if err != nil {
			return err
		}
		e := s.githubAssociationReportEntryFor(ctx, p)
		updated[id] = &e
	}
	// As in a full build: a cancelled request commits nothing and the
	// invalidations stay pending.
	if err := ctx.Err(); err != nil {
		return err
	}
	c := &s.githubAssociationReportCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]githubAssociationReportEntry)
	}
	for id, e := range updated {
		if e == nil {
			delete(c.entries, id)
		} else {
			c.entries[id] = *e
		}
	}
	c.clearDirtyLocked(startSeq)
	return nil
}

// clearDirtyLocked drops invalidations made at or before seq. c.mu is held.
func (c *githubAssociationReportCache) clearDirtyLocked(seq uint64) {
	for id, at := range c.dirty {
		if at <= seq {
			delete(c.dirty, id)
		}
	}
}

// snapshot returns the cached report, entries ordered by project name and
// ID.
func (c *githubAssociationReportCache) snapshot() githubAssociationReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := make([]githubAssociationReportEntry, 0, len(c.entries))
	for _, e := range c.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ProjectName != entries[j].ProjectName {
			return entries[i].ProjectName < entries[j].ProjectName
		}
		return entries[i].ProjectID < entries[j].ProjectID
	})
	return githubAssociationReport{Entries: entries, GeneratedAt: c.generatedAt, Truncated: c.truncated}
}
