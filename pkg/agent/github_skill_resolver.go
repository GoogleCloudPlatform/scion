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

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

const (
	githubAPIBase     = "https://api.github.com"
	githubRawBase     = "https://raw.githubusercontent.com"
	githubAPITimeout  = 30 * time.Second
	githubMaxFileSize = 10 * 1024 * 1024 // 10MB per file

	githubMaxRetries    = 4
	githubBaseBackoff   = 1 * time.Second
	githubMaxBackoff    = 30 * time.Second
	githubBackoffFactor = 2.0

	// githubRequestTimeout bounds a single metadata HTTP attempt (resolving a
	// commit SHA or listing directory contents), independent of both
	// githubAPITimeout above and the caller's own ctx deadline. Before this, a
	// stalled connection relied solely on githubAPITimeout (30s) to give up —
	// the same order of magnitude as the ~30s agent-create deadline, so one
	// hung request could consume the entire budget before doWithRetry's
	// budget-fitting logic (below) ever got a chance to back off or fail
	// fast. GitHub API requests normally complete in well under a second even
	// from a loaded broker, so 5s is generous slack for a single attempt
	// while still leaving room for several attempts inside a 30s create
	// deadline.
	githubRequestTimeout = 5 * time.Second

	// githubDownloadRequestTimeout bounds a single raw-file download attempt.
	// It is longer than githubRequestTimeout because a download transfers up
	// to githubMaxFileSize (10MB), not a small JSON/text response: at the 5s
	// metadata timeout, finishing a 10MB body would require ~16 Mbit/s from
	// the broker, which regressed the pre-fix 30s allowance for no benefit
	// (a legitimate slow-but-progressing transfer would be killed early). A
	// connection that never responds at all is still caught quickly via the
	// httpClient's ResponseHeaderTimeout (set in NewGitHubSkillResolver to
	// githubRequestTimeout), so this longer ctx timeout only ever bounds a
	// download that is actually receiving bytes (#2546 O2).
	githubDownloadRequestTimeout = 20 * time.Second

	// githubResolveBudget bounds a Resolve call when the caller's ctx carries
	// no deadline — the production shape: the broker's create ctx is built
	// with context.WithCancel(context.Background()) on the control-channel
	// path and r.Context() on the direct-HTTP path, and nothing between
	// createAgent and GitHubSkillResolver.Resolve ever adds a deadline. With
	// no deadline, ctx.Deadline() in doWithRetry below always reports
	// ok=false, so the budget-fitting fail-fast check — the actual fix for
	// #2546 — was dead code in production; only the CLI's outer ~30s
	// http.Client.Timeout eventually gave up, as a bare "context canceled".
	// 20s leaves slack under that 30s for hub/broker overhead (auth checks,
	// the control-channel tunnel round trip, cleanup) while still giving
	// doWithRetry's existing deadline-based logic a real deadline to work
	// with (#2546 R1).
	githubResolveBudget = 20 * time.Second
)

// GitHubSkillResolver resolves skills from GitHub repositories
// using the GitHub Contents API.
type GitHubSkillResolver struct {
	httpClient           *http.Client
	token                string            // Default GITHUB_TOKEN for authenticated requests
	provisionCredentials map[string]string // Per-URI named credentials from ProvisionCredentials
	apiBase              string            // Default: githubAPIBase, override in tests
	rawBase              string            // Default: githubRawBase, override in tests
	resolutionCache      *GitHubResolutionCache
	// cooldown holds requests back per credential identity after a GitHub
	// rate-limit response (see GitHubCooldown). The constructors set the
	// process-wide SharedGitHubCooldown; a resolver built without one gets
	// its own tracker on first use (see cooldownTracker).
	cooldown     *GitHubCooldown
	cooldownOnce sync.Once

	// Zero values fall back to githubResolveBudget, githubRequestTimeout and
	// githubDownloadRequestTimeout; tests shrink them to run quickly.
	resolveBudget   time.Duration
	requestTimeout  time.Duration
	downloadTimeout time.Duration

	// maxConcurrent bounds the refs one Resolve call resolves at once; zero
	// means githubResolveConcurrency.
	maxConcurrent int
}

// durationOr returns d, or def when d is unset.
func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// newGitHubHTTPClient clones http.DefaultTransport, so proxy settings
// (ProxyFromEnvironment), dial/TLS timeouts and pooling survive, and adds a
// ResponseHeaderTimeout of stall: a connection that never responds at all
// fails within stall, independent of the per-attempt ctx timeout, so the
// longer raw-download timeout (#2546 O2) does not reintroduce slow failure.
// If something has replaced http.DefaultTransport with a RoundTripper that
// is not an *http.Transport, it starts from fallbackHTTPTransport instead
// of panicking.
func newGitHubHTTPClient(stall time.Duration) *http.Client {
	var tr *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = dt.Clone()
	} else {
		tr = fallbackHTTPTransport()
	}
	tr.ResponseHeaderTimeout = stall
	return &http.Client{Timeout: githubAPITimeout, Transport: tr}
}

// fallbackHTTPTransport mirrors net/http's own DefaultTransport settings
// (proxy from environment, dial and TLS handshake timeouts, pooling), for
// when http.DefaultTransport cannot be cloned.
func fallbackHTTPTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// stallBound is how long an attempt may go without any response before it
// fails: the transport's ResponseHeaderTimeout, as set by
// newGitHubHTTPClient. Reading it from the transport keeps doWithRetry's
// budget reservation in step with the real bound (#2546 N1). Clients
// without one (e.g. test servers' clients) fall back to the metadata
// attempt timeout, which then bounds a stall instead.
func (r *GitHubSkillResolver) stallBound() time.Duration {
	if r.httpClient != nil {
		if tr, ok := r.httpClient.Transport.(*http.Transport); ok && tr.ResponseHeaderTimeout > 0 {
			return tr.ResponseHeaderTimeout
		}
	}
	return durationOr(r.requestTimeout, githubRequestTimeout)
}

// NewGitHubSkillResolver creates a resolver for gh:// and GitHub URL skills.
// Reads GITHUB_TOKEN from environment for authenticated API access.
// If a resolution cache directory is available, cached resolution results
// are reused to avoid redundant GitHub API calls.
func NewGitHubSkillResolver() *GitHubSkillResolver {
	return newGitHubSkillResolver(openDefaultResolutionCache())
}

// openDefaultResolutionCache opens the resolution cache in its default
// directory, or returns nil (with a warning) if that is not possible.
func openDefaultResolutionCache() *GitHubResolutionCache {
	cacheDir, cacheDirErr := GitHubResolutionCacheDir()
	if cacheDirErr != nil {
		// Print to stderr unconditionally: without a cache every request hits the
		// GitHub API fresh, directly contributing to rate-limit exhaustion.
		fmt.Fprintf(os.Stderr, "github: WARNING: failed to determine resolution cache dir: %v; proceeding without cache\n", cacheDirErr)
		return nil
	}
	cache, cacheErr := NewGitHubResolutionCache(cacheDir, DefaultResolutionCacheTTL)
	if cacheErr != nil {
		// Same rationale: operators need to see cache failures in production logs.
		fmt.Fprintf(os.Stderr, "github: WARNING: failed to initialize resolution cache at %s: %v (proceeding without cache)\n", cacheDir, cacheErr)
		return nil
	}
	return cache
}

func newGitHubSkillResolver(cache *GitHubResolutionCache) *GitHubSkillResolver {
	return &GitHubSkillResolver{
		httpClient:      newGitHubHTTPClient(githubRequestTimeout),
		token:           os.Getenv("GITHUB_TOKEN"),
		apiBase:         githubAPIBase,
		rawBase:         githubRawBase,
		resolutionCache: cache,
		cooldown:        SharedGitHubCooldown(),
	}
}

// cooldownTracker returns r.cooldown, creating a tracker private to this
// resolver when none was set, so a resolver built as a struct literal (as
// tests do) never shares cooldown state with any other.
func (r *GitHubSkillResolver) cooldownTracker() *GitHubCooldown {
	r.cooldownOnce.Do(func() {
		if r.cooldown == nil {
			r.cooldown = NewGitHubCooldown(nil)
		}
	})
	return r.cooldown
}

// NewGitHubSkillResolverWithCredentials constructs a resolver with an explicit
// default token and a named-credential map for per-URI lookup.
//
// Token resolution order for bare gh:// URIs (no ?token= suffix):
//  1. defaultToken (from req.ResolvedEnv["GITHUB_TOKEN"] — GitHub App or env-type secret).
//  2. GITHUB_TOKEN from the broker process environment (os.Getenv, set by NewGitHubSkillResolver).
//  3. GITHUB_TOKEN from provisionCredentials (project secret of any type).
//  4. No token — unauthenticated calls, subject to GitHub's 60 req/hr per-IP limit.
//
// provisionCredentials maps secret name → value; may be nil.
// If cache is non-nil, it is used as the singleton resolution cache (e.g., from
// the broker server struct) instead of the per-resolver cache created by
// NewGitHubSkillResolver. Pass nil to open the default cache for this resolver.
func NewGitHubSkillResolverWithCredentials(defaultToken string, provisionCredentials map[string]string, cache *GitHubResolutionCache) *GitHubSkillResolver {
	// Use the singleton cache when one is passed in (e.g. from the broker
	// server struct). Opening the default cache here as well would read, and
	// possibly rewrite, the cache file on every request for nothing.
	if cache == nil {
		cache = openDefaultResolutionCache()
	}
	r := newGitHubSkillResolver(cache)
	if defaultToken != "" {
		r.token = defaultToken
	} else if r.token == "" {
		// Neither an explicit token nor the broker-env GITHUB_TOKEN is available.
		// Fall back to a project-scoped provision credential named GITHUB_TOKEN.
		// This covers projects that store GITHUB_TOKEN as a secret but not as an
		// env-type secret (which would have been included in req.ResolvedEnv).
		if val := provisionCredentials["GITHUB_TOKEN"]; val != "" {
			r.token = val
		}
	}
	r.provisionCredentials = provisionCredentials
	return r
}

// normalizeGitHubName uppercases a GitHub name and replaces hyphens and dots
// with underscores to produce a valid env-var-style key segment.
func normalizeGitHubName(name string) string {
	s := strings.ToUpper(name)
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

// deriveGitHubTokenKey converts a GitHub owner/repo pair into the conventional
// project secret key name: GH_{OWNER}__{REPO}.
func deriveGitHubTokenKey(owner, repo string) string {
	return "GH_" + normalizeGitHubName(owner) + "__" + normalizeGitHubName(repo)
}

// deriveGitHubOwnerKey converts a GitHub owner into the owner-level
// fallback key: GH_{OWNER}.
func deriveGitHubOwnerKey(owner string) string {
	return "GH_" + normalizeGitHubName(owner)
}

// tokenForRef returns the appropriate GitHub token for the given ref.
//
// Resolution precedence:
//  1. Explicit ?token=SECRET_NAME on the URI — looked up in provisionCredentials.
//  2. Repo-specific convention key GH_{OWNER}__{REPO} from provisionCredentials.
//  3. Owner-level convention key GH_{OWNER} from provisionCredentials.
//  4. Default GITHUB_TOKEN cascade (r.token).
//  5. Empty string — unauthenticated; works for public repos.
//
// If ?token= is present but the named secret is missing, an error is returned.
// Missing convention keys are not errors — the resolver silently falls through.
func (r *GitHubSkillResolver) tokenForRef(ref *GitHubSkillRef) (string, error) {
	token, source, err := r.lookupToken(ref)
	if err != nil {
		return "", err
	}
	switch source {
	case tokenSourceRepoKey:
		util.Debugf("github: using credential %s for %s/%s", deriveGitHubTokenKey(ref.Owner, ref.Repo), ref.Owner, ref.Repo)
	case tokenSourceOwnerKey:
		util.Debugf("github: using credential %s for %s/%s", deriveGitHubOwnerKey(ref.Owner), ref.Owner, ref.Repo)
	case tokenSourceDefault:
		util.Debugf("github: no convention credential for %s/%s, using default", ref.Owner, ref.Repo)
	case tokenSourceNone:
		fmt.Fprintf(os.Stderr, "github: WARNING: no credential available for %s/%s, attempting unauthenticated\n", ref.Owner, ref.Repo)
	}
	return token, nil
}

type tokenSource int

const (
	tokenSourceNone tokenSource = iota
	tokenSourceSecretParam
	tokenSourceRepoKey
	tokenSourceOwnerKey
	tokenSourceDefault
)

// lookupToken is the credential lookup behind tokenForRef, without logging,
// so the install step can repeat it (see CredentialForURI).
func (r *GitHubSkillResolver) lookupToken(ref *GitHubSkillRef) (string, tokenSource, error) {
	// Priority 1: Explicit ?token= override.
	if ref.TokenSecretName != "" {
		// In Go, reading from a nil map is safe and returns "". Both nil map and
		// missing/empty key produce the same error: the secret is unavailable.
		if val := r.provisionCredentials[ref.TokenSecretName]; val != "" {
			return val, tokenSourceSecretParam, nil
		}
		return "", tokenSourceNone, fmt.Errorf("secret %q not found in ProvisionCredentials; ensure it is set at project scope", ref.TokenSecretName)
	}

	// Priority 2: Repo-specific convention key (GH_OWNER__REPO).
	if val := r.provisionCredentials[deriveGitHubTokenKey(ref.Owner, ref.Repo)]; val != "" {
		return val, tokenSourceRepoKey, nil
	}

	// Priority 3: Owner-level convention key (GH_OWNER).
	if val := r.provisionCredentials[deriveGitHubOwnerKey(ref.Owner)]; val != "" {
		return val, tokenSourceOwnerKey, nil
	}

	// Priority 4: Default GITHUB_TOKEN cascade.
	if r.token != "" {
		return r.token, tokenSourceDefault, nil
	}

	// Priority 5: No credential available — unauthenticated.
	return "", tokenSourceNone, nil
}

// CredentialForURI returns the credential this resolver uses for the gh://
// skill URI, by the same lookup as resolution (lookupToken), or "" if the
// URI does not parse, a named secret is unavailable, or no credential
// applies. The broker installs it as the GitHubCredentialLookup for the
// request, for skills this resolver served without file content.
func (r *GitHubSkillResolver) CredentialForURI(uri string) string {
	ghRef, err := ParseGitHubSkillURI(uri)
	if err != nil {
		return ""
	}
	token, _, err := r.lookupToken(ghRef)
	if err != nil {
		return ""
	}
	return token
}

// WithInstallCredentials returns ctx with what the install step needs to
// download files of gh:// skills: this resolver's CredentialForURI as the
// GitHub credential lookup, and defaultToken (when non-empty) as the default
// GitHub token for skills resolved elsewhere, such as by the Hub. Callers
// pass the context returned here to provisioning, which also resolves skills
// with it.
func (r *GitHubSkillResolver) WithInstallCredentials(ctx context.Context, defaultToken string) context.Context {
	if defaultToken != "" {
		ctx = ContextWithGitHubToken(ctx, defaultToken)
	}
	return ContextWithGitHubCredentialLookup(ctx, r.CredentialForURI)
}

// FlushCache writes any resolution cache entries still waiting for their
// delayed write (see GitHubResolutionCache.Flush). Short-lived processes
// call it before exiting so persistence does not depend on the delay.
func (r *GitHubSkillResolver) FlushCache() {
	if r.resolutionCache != nil {
		r.resolutionCache.Flush()
	}
}

// hasAllFileContent reports whether every file of skill carries its
// content, i.e. install needs no download for it.
func hasAllFileContent(skill ResolvedSkill) bool {
	for _, f := range skill.Files {
		if f.Content == nil {
			return false
		}
	}
	return true
}

func (r *GitHubSkillResolver) ResolverName() string { return "github" }

// PreferFallback implements agent.RouteFilter. It reports whether ref needs a
// credential the Hub cannot hold — an explicit ?token= secret (which lives
// only in the broker's ProvisionCredentials) or a GH_{OWNER} /
// GH_{OWNER}__{REPO} convention credential — so the ref should be routed
// directly to this resolver instead of making a Hub round trip that can only
// fail or fall back (the Hub's resolveGitHubSkill rejects ?token= refs
// outright, and has no access to ProvisionCredentials at all). Parses the URI
// and checks r.provisionCredentials; no I/O, no GitHub call.
func (r *GitHubSkillResolver) PreferFallback(ref api.SkillReference) bool {
	ghRef, err := ParseGitHubSkillURI(ref.URI)
	if err != nil {
		// Not this resolver's concern to diagnose early — let the primary
		// report the parse error as it does today.
		return false
	}
	return r.credentialSource(ghRef) != ""
}

func (r *GitHubSkillResolver) Resolve(ctx context.Context, refs []api.SkillReference, opts ResolveOpts) (*ResolveResult, error) {
	// Impose our own budget when the caller gave none, so the fail-fast logic
	// in doWithRetry — which only ever fires when ctx.Deadline() reports a
	// deadline — actually runs in production (#2546 R1).
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, durationOr(r.resolveBudget, githubResolveBudget))
		defer cancel()
	}

	return r.resolveAll(ctx, refs, opts), nil
}

// githubResolveConcurrency bounds how many refs a single Resolve call
// resolves at once. On a cold cache each ref costs several GitHub round
// trips; resolving them one after another made a template with many refs
// pay the summed latency inside the create deadline. A small bound keeps
// the burst of requests per create modest, since all refs share one
// credential's rate limit and cooldown.
const githubResolveConcurrency = 4

// refOutcome is the result of resolving one ref: exactly one of skill and
// rerr is set.
type refOutcome struct {
	skill *ResolvedSkill
	rerr  *ResolveError
}

// resolveAll resolves refs with at most r.concurrency() in flight and
// returns the results in input order: Resolved holds the successes and
// Errors the failures, each in the order their refs appear in refs, the
// same as resolving them one after another. Every ref is attempted, also
// after ctx is done, so each one keeps the outcome serial resolution gave
// it. All started work is waited for before returning.
func (r *GitHubSkillResolver) resolveAll(ctx context.Context, refs []api.SkillReference, opts ResolveOpts) *ResolveResult {
	outcomes := make([]refOutcome, len(refs))
	sem := make(chan struct{}, r.concurrency())
	var wg sync.WaitGroup

	for i, ref := range refs {
		ghRef, err := ParseGitHubSkillURI(ref.URI)
		if err != nil {
			outcomes[i].rerr = &ResolveError{
				URI: ref.URI, Code: "invalid_uri", Message: err.Error(),
			}
			continue
		}

		// Wait for a free slot. There is deliberately no ctx check here: a
		// ref queued behind slow ones still runs once ctx is done, so it
		// gets the outcome it would get if resolved on its own (a fresh
		// cache hit is served, and anything needing GitHub fails fast as a
		// timeout or cancellation through the usual paths). Every started
		// ref is bound by ctx, so slots always free up.
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, ref api.SkillReference, ghRef *GitHubSkillRef) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if p := recover(); p != nil {
					outcomes[i] = panicOutcome(ref, p)
				}
			}()
			outcomes[i] = r.resolveRef(ctx, ghRef, ref, opts)
		}(i, ref, ghRef)
	}
	wg.Wait()

	result := &ResolveResult{}
	for _, o := range outcomes {
		if o.rerr != nil {
			result.Errors = append(result.Errors, *o.rerr)
			continue
		}
		result.Resolved = append(result.Resolved, *o.skill)
	}
	return result
}

// concurrency returns the per-Resolve bound on refs in flight.
func (r *GitHubSkillResolver) concurrency() int {
	if r.maxConcurrent > 0 {
		return r.maxConcurrent
	}
	return githubResolveConcurrency
}

// panicOutcome reports a panic recovered while resolving ref. The resolve
// goroutine is not covered by the request handler's own recovery, so the
// panic is turned into a per-ref error instead of ending the process. The
// returned message is generic and carries nothing from the panic value,
// which may include credential material; the value and stack go to the log.
// The log names the ref by its URI, which ParseGitHubSkillURI has already
// accepted: it names at most a secret (?token=NAME), never a secret value,
// and is the same string returned in ResolveError.URI.
func panicOutcome(ref api.SkillReference, p any) refOutcome {
	slog.Error("github: panic during skill resolution",
		"uri", ref.URI, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
	return refOutcome{rerr: &ResolveError{
		URI: ref.URI, Code: "resolve_failed",
		Message: "internal error during GitHub skill resolution",
	}}
}

// resolveRef resolves one parsed ref and classifies a failure into the
// ResolveError the caller reports for it.
func (r *GitHubSkillResolver) resolveRef(ctx context.Context, ghRef *GitHubSkillRef, ref api.SkillReference, opts ResolveOpts) refOutcome {
	resolved, err := r.resolveOne(ctx, ghRef, ref, opts.ProjectID, opts.UserID)
	if err == nil {
		return refOutcome{skill: resolved}
	}
	code := "resolve_failed"
	var retryAfter string
	var rl *GitHubRateLimitError
	var rerr *githubResolveError
	switch {
	case errors.As(err, &rl):
		// Checked first: a rate-limit error may also be wrapped in a
		// githubResolveError, and its RetryAt is the cooldown's own end.
		code = GitHubRateLimitedCode
		retryAfter = r.cooldownRetryAfter(rl)
	case errors.As(err, &rerr):
		// Classify the failure into a stable cause code when possible
		// (set by doWithRetry/listContents/resolveCommitSHA below), so
		// the create path can map a required-skill failure to the
		// right status instead of a generic 500/502.
		code = rerr.code
		retryAfter = rerr.retryAfter
	}
	return refOutcome{rerr: &ResolveError{
		URI: ref.URI, Code: code, Message: err.Error(), RetryAfter: retryAfter,
	}}
}

// resolutionCacheKey returns a canonical cache key for a skill ref.
// credentialFingerprint's full digest of the token is included so that
// different credentials produce separate cache entries, preventing
// cross-credential cache sharing of private content. The raw token is never
// used as a key value.
func resolutionCacheKey(ghRef *GitHubSkillRef, token string) string {
	var tokenSuffix string
	if token != "" {
		tokenSuffix = "#" + credentialFingerprint(token)
	}
	return fmt.Sprintf("gh://%s/%s/%s@%s%s",
		ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, tokenSuffix)
}

func (r *GitHubSkillResolver) resolveOne(ctx context.Context, ghRef *GitHubSkillRef, ref api.SkillReference, projectID, userID string) (*ResolvedSkill, error) {
	// Resolve credential first — before cache check.
	// This ensures: (1) missing credentials fail immediately, (2) the token
	// hash is available for the cache key, isolating cache entries per credential.
	token, err := r.tokenForRef(ghRef)
	if err != nil {
		return nil, err
	}

	cacheKey := resolutionCacheKey(ghRef, token)
	cooldownID := GitHubCooldownIdentity(token)

	fetch := func(fctx context.Context) (ResolvedSkill, error) {
		return r.fetchOne(fctx, ghRef, ref, token)
	}
	// refreshAllowed gates the background refresh of a stale entry: while
	// this credential is in a rate-limit cooldown the stale value is served
	// without starting one.
	refreshAllowed := func() bool {
		_, active := r.cooldownTracker().Active(cooldownID)
		return !active
	}

	var resolved ResolvedSkill
	if r.resolutionCache != nil {
		effectiveRef := ghRef.Ref
		if effectiveRef == "" {
			effectiveRef = "HEAD"
		}
		isBranchRef := !isFullCommitSHA(effectiveRef)
		credID := r.flightIdentity(ghRef, projectID, userID, token)
		// flightKey folds credID in after the ref: credID already includes a
		// fingerprint of token's own value (see flightIdentity), so two
		// different credential values for the same ref never share a flight.
		flightKey := fmt.Sprintf("gh://%s/%s/%s@%s|%s", ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, credID)

		// logRef carries no credential-derived material — unlike flightKey and
		// credID, it is safe to put in a log line or an error message that
		// might reach a caller (see ResolveWithFetch/coalesceFetch). It names
		// the ref and the general kind of source that supplied the credential
		// ("named" or "default"), without the credential's value, its
		// fingerprint, the secret's own name, or the project/user scope.
		sourceKind := "default"
		if r.credentialSource(ghRef) != "" {
			sourceKind = "named"
		}
		logRef := ghRef.Raw + " (" + sourceKind + ")"

		// A cached entry loaded from disk has no file content, so install
		// downloads its files using the credential the install context's
		// lookup returns for this URI (see gitHubDownloadToken). When this
		// request was resolved with a credential and that lookup cannot
		// return the same one, such an entry is not used: the ref is
		// resolved again, with content, so install never downloads a
		// credential-scoped skill with a different credential or none.
		var accept func(ResolvedSkill) bool
		if token != "" && credentialLookupFromContext(ctx)(ghRef.Raw) != token {
			accept = hasAllFileContent
			// Keep these callers out of flights started by callers that
			// accept a content-less entry (see coalesceFetchAccept).
			flightKey += "|with-content"
		}

		skill, err := r.resolutionCache.resolveWithFetchAccept(ctx, cacheKey, flightKey, credID, logRef, isBranchRef, refreshAllowed, accept, fetch)
		if err != nil {
			return nil, withRateLimitRef(err, ghRef.Raw)
		}
		resolved = skill
	} else {
		skill, err := fetch(ctx)
		if err != nil {
			return nil, withRateLimitRef(err, ghRef.Raw)
		}
		resolved = skill
	}

	// Always carry this call's per-ref fields over: a cache or in-flight hit
	// may have been produced for a different ref sharing this URI and
	// credential, with a different As, Scope or Optional. Scope decides
	// which skill wins a name collision and Optional how a failure is
	// reported, so neither may come from another caller's ref.
	resolved.As = ref.As
	resolved.Scope = ref.Scope
	resolved.Optional = ref.Optional

	// A skill loaded from the on-disk cache has no file content, so install
	// downloads its files. Record this call's URI (it names the secret to
	// use, never its value) so install looks up the same credential for
	// those downloads (see gitHubDownloadToken).
	resolved.githubCredentialRef = ""
	if !hasAllFileContent(resolved) {
		resolved.githubCredentialRef = ghRef.Raw
	}
	return &resolved, nil
}

// withRateLimitRef returns err as a *GitHubRateLimitError naming ref when err
// is one, so the caller sees which ref hit the limit; other errors are
// returned unchanged.
func withRateLimitRef(err error, ref string) error {
	var rl *GitHubRateLimitError
	if errors.As(err, &rl) {
		return rl.WithRef(ref)
	}
	return err
}

// cooldownRetryAfter renders the time left until rl.RetryAt, by the
// cooldown tracker's clock, as a whole number of seconds (rounded up, at
// least 1), the form ResolveError.RetryAfter carries.
func (r *GitHubSkillResolver) cooldownRetryAfter(rl *GitHubRateLimitError) string {
	secs := int64(math.Ceil(rl.RetryAt.Sub(r.cooldownTracker().now()).Seconds()))
	if secs < 1 {
		secs = 1
	}
	return strconv.FormatInt(secs, 10)
}

// fetchOne performs the actual GitHub API work for ghRef — resolving the
// commit SHA, listing the skill directory, and downloading each file — with
// no cache or coalescing concerns of its own. It is the fetch callback
// GitHubResolutionCache.ResolveWithFetch calls on a cache miss or stale
// refresh (see resolveOne).
func (r *GitHubSkillResolver) fetchOne(ctx context.Context, ghRef *GitHubSkillRef, ref api.SkillReference, token string) (ResolvedSkill, error) {
	commitSHA, err := r.resolveCommitSHA(ctx, ghRef, token)
	if err != nil {
		return ResolvedSkill{}, fmt.Errorf("failed to resolve ref for %s: %w", ghRef.Raw, err)
	}

	contents, err := r.listContents(ctx, ghRef, commitSHA, token)
	if err != nil {
		return ResolvedSkill{}, fmt.Errorf("failed to list skill contents for %s: %w", ghRef.Raw, err)
	}

	if len(contents) == 0 {
		return ResolvedSkill{}, fmt.Errorf("skill %q not found in repo %s/%s (empty directory at %s)",
			ghRef.SkillName, ghRef.Owner, ghRef.Repo, ghRef.SkillPath)
	}

	var resolvedFiles []ResolvedFile
	var fileInfos []transfer.FileInfo

	expectedPrefix := ghRef.SkillPath + "/"
	for _, entry := range contents {
		if entry.Type != "file" {
			continue
		}
		if !strings.HasPrefix(entry.Path, expectedPrefix) {
			continue
		}

		content, err := r.downloadRawFile(ctx, ghRef, commitSHA, entry.Path, token)
		if err != nil {
			return ResolvedSkill{}, fmt.Errorf("failed to download %s for skill %s: %w", entry.Path, ghRef.Raw, err)
		}

		hash := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
		relPath := strings.TrimPrefix(entry.Path, ghRef.SkillPath+"/")

		resolvedFiles = append(resolvedFiles, ResolvedFile{
			Path:    relPath,
			URL:     r.rawContentURL(ghRef, commitSHA, entry.Path),
			Hash:    hash,
			Size:    int64(len(content)),
			Content: content, // Carry bytes so install phase skips unauthenticated re-download.
		})
		fileInfos = append(fileInfos, transfer.FileInfo{Path: relPath, Hash: hash})
	}

	if len(resolvedFiles) == 0 {
		return ResolvedSkill{}, fmt.Errorf("skill %q in repo %s/%s contains no files",
			ghRef.SkillName, ghRef.Owner, ghRef.Repo)
	}

	bundleHash := transfer.ComputeContentHash(fileInfos)

	return ResolvedSkill{
		Name:     ghRef.SkillName,
		URI:      ghRef.Raw,
		As:       ref.As,
		Version:  commitSHA[:12],
		Hash:     bundleHash,
		Scope:    ref.Scope,
		Files:    resolvedFiles,
		Optional: ref.Optional,
	}, nil
}

// credentialSource returns the named credential source that would supply
// ghRef's token, mirroring tokenForRef's precedence for its two named
// lookups (an explicit ?token= and the GH_* convention keys) — including
// checking for a non-empty value, exactly as tokenForRef does, so an empty
// secret does not falsely report a named source in use. It does not evaluate
// the final default-token cascade (r.token / the GITHUB_TOKEN convention
// credential): "" means no named override applies, not "unauthenticated".
func (r *GitHubSkillResolver) credentialSource(ghRef *GitHubSkillRef) string {
	if ghRef.TokenSecretName != "" {
		return "token:" + ghRef.TokenSecretName
	}
	repoKey := deriveGitHubTokenKey(ghRef.Owner, ghRef.Repo)
	if val := r.provisionCredentials[repoKey]; val != "" {
		return "cred:" + repoKey
	}
	ownerKey := deriveGitHubOwnerKey(ghRef.Owner)
	if val := r.provisionCredentials[ownerKey]; val != "" {
		return "cred:" + ownerKey
	}
	return ""
}

// flightIdentity returns a stable label for the credential actually used to
// fetch ghRef with token. It keys single-flight coalescing and the
// per-credential in-flight cap (GitHubResolutionCache.ResolveWithFetch).
//
// The identity always includes a hash of token's own value, for every
// source. That is the one thing that makes two different credential values
// never merge, regardless of how the value reached this resolver — and there
// are several such paths for the default (no named override) source alone:
// an explicit GITHUB_TOKEN set on the agent's own applied config or its
// template, a project secret used to fill that same env key when it would
// otherwise be absent, a GitHub App installation token minted fresh for this
// create, the same mint redirected to a different project's installation via
// the source-project label, the broker process's own GITHUB_TOKEN, and a
// provision-credential fallback of the same name — every one of these ends
// up as r.token by the time tokenForRef's default cascade runs, and none of
// them is special-cased here: the hash treats them alike. Named sources
// (?token= and the GH_* convention keys) resolve to a project secret value
// instead, which should already be the same for every caller in one project,
// but are hashed too rather than trusting that.
//
// projectID and userID are layered on top of the hash, not as a substitute
// for it: they are not required for the no-two-values-merge invariant (the
// hash alone already gives that), but keeping them means a project or user
// isolation regression still shows up as a flight merge even when two
// callers happen to present the exact same token value — the case the hash
// alone cannot tell apart, since identical values hash identically.
// TestGitHubSkillResolver_ScopeLayeringIsolation pins exactly this: one fixed
// token value, checked across a different project (default source), a
// different user within one project (default source), and a different
// project under a named source, asserting each pair still gets independent
// flights. Falls back to a fixed label when a scope is unavailable (empty
// ProjectID: the CLI path, which uses its own per-process cache anyway;
// empty UserID: a caller that never carries one) — the hash still makes that
// fallback a non-issue for the no-two-values-merge invariant.
//
// token == "" means the request is unauthenticated: that case is safe to
// share across every caller regardless of project or user (anonymous public
// access), so it is deliberately not scoped at all.
func (r *GitHubSkillResolver) flightIdentity(ghRef *GitHubSkillRef, projectID, userID, token string) string {
	if token == "" {
		return "anon"
	}
	tokenScope := credentialFingerprint(token)

	if src := r.credentialSource(ghRef); src != "" {
		return scopeOrDefault(projectID, "no-project") + "|" + src + "|" + tokenScope
	}

	return scopeOrDefault(projectID, "no-project") + "|" + scopeOrDefault(userID, "no-user") + "|default|" + tokenScope
}

// scopeOrDefault returns scope, or fallback when scope is empty.
func scopeOrDefault(scope, fallback string) string {
	if scope == "" {
		return fallback
	}
	return scope
}

// credentialFingerprint returns the full hex-encoded SHA-256 digest of a
// credential value. It is the one place that turns a credential value into a
// map-key component, used by both flightIdentity (so two different values
// never share a flight or a credential-cap slot) and resolutionCacheKey (so
// they never share a cache entry either). The full digest is used, not a
// truncated prefix: these are in-memory map keys only, so the extra bytes
// cost nothing, and a truncated prefix would make "two different values
// never merge" merely probabilistic instead of guaranteed.
func credentialFingerprint(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// githubContentEntry is the JSON structure returned by the GitHub Contents API.
type githubContentEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	Size        int    `json:"size"`
	DownloadURL string `json:"download_url"`
}

// isFullCommitSHA reports whether s is a complete 40-character lowercase
// hexadecimal commit SHA. Such a ref is already fully resolved and requires
// no GitHub API call to "resolve" it further.
func isFullCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (r *GitHubSkillResolver) resolveCommitSHA(ctx context.Context, ghRef *GitHubSkillRef, token string) (string, error) {
	ref := ghRef.Ref
	if ref == "" {
		ref = "HEAD"
	}

	// Warn before any API call path — including listContents and downloadRawFile
	// called by the parent resolveOne after this function returns. Even when the
	// full-SHA short-circuit below skips the SHA-lookup call, those subsequent
	// calls still go out unauthenticated; the operator needs advance notice.
	// Unauthenticated GitHub API calls are limited to 60/hr per outbound IP
	// (shared across all broker instances on Cloud Run / Cloud NAT).
	// To fix: set GITHUB_TOKEN in the project secrets or the broker's environment.
	if token == "" {
		fmt.Fprintf(os.Stderr, "github: WARNING: no GITHUB_TOKEN configured for %s; "+
			"making unauthenticated GitHub API call (limit: 60 req/hr per IP). "+
			"Set a GITHUB_TOKEN project secret or broker env var to increase the limit.\n", ghRef.Raw)
	}

	// Short-circuit: if the ref is already a full 40-char lowercase hex commit SHA,
	// no API call is needed — the ref IS the resolved SHA.
	if isFullCommitSHA(ref) {
		util.Debugf("github: ref %s is already a full SHA, skipping resolveCommitSHA API call", ref)
		return ref, nil
	}

	reqURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s", r.apiBase,
		url.PathEscape(ghRef.Owner), url.PathEscape(ghRef.Repo), url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3.sha")
	r.setAuthHeader(req, token)

	resp, err := r.doWithRetry(ctx, req, durationOr(r.requestTimeout, githubRequestTimeout), token)
	if err != nil {
		return "", fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", &githubResolveError{
			code: SkillErrCodeNotFound,
			msg:  fmt.Sprintf("ref %q not found in repo %s/%s", ghRef.Ref, ghRef.Owner, ghRef.Repo),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", r.apiError(resp, "resolve commit")
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", fmt.Errorf("failed to read commit SHA: %w", err)
	}
	sha := strings.TrimSpace(string(body))
	if len(sha) != 40 {
		return "", fmt.Errorf("unexpected commit SHA format: %q", sha)
	}
	return sha, nil
}

func (r *GitHubSkillResolver) listContents(ctx context.Context, ghRef *GitHubSkillRef, commitSHA string, token string) ([]githubContentEntry, error) {
	escapedPath := escapePathSegments(ghRef.SkillPath)
	reqURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		r.apiBase, url.PathEscape(ghRef.Owner), url.PathEscape(ghRef.Repo), escapedPath, url.QueryEscape(commitSHA))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	r.setAuthHeader(req, token)

	resp, err := r.doWithRetry(ctx, req, durationOr(r.requestTimeout, githubRequestTimeout), token)
	if err != nil {
		return nil, fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, &githubResolveError{
			code: SkillErrCodeNotFound,
			msg: fmt.Sprintf("skill %q not found in repo %s/%s at ref %s (expected directory at %s)",
				ghRef.SkillName, ghRef.Owner, ghRef.Repo, commitSHA[:12], ghRef.SkillPath),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, r.apiError(resp, "list contents")
	}

	var entries []githubContentEntry
	limited := io.LimitReader(resp.Body, 5*1024*1024)
	if err := json.NewDecoder(limited).Decode(&entries); err != nil {
		return nil, fmt.Errorf("failed to decode GitHub API response: %w", err)
	}
	return entries, nil
}

func (r *GitHubSkillResolver) downloadRawFile(ctx context.Context, ghRef *GitHubSkillRef, commitSHA, filePath string, token string) ([]byte, error) {
	reqURL := r.rawContentURL(ghRef, commitSHA, filePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	r.setAuthHeader(req, token)

	resp, err := r.doWithRetry(ctx, req, durationOr(r.downloadTimeout, githubDownloadRequestTimeout), token)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, &githubResolveError{
			code: SkillErrCodeNotFound,
			msg:  fmt.Sprintf("file %s not found in repo %s/%s at %s", filePath, ghRef.Owner, ghRef.Repo, commitSHA[:12]),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, r.apiError(resp, fmt.Sprintf("downloading %s", filePath))
	}

	content, err := io.ReadAll(io.LimitReader(resp.Body, int64(githubMaxFileSize)+1))
	if err != nil {
		// A transfer cut off by the per-attempt or resolve-budget deadline is
		// a timeout, so classify it as such (mapping to 504) rather than
		// leaving it as an unclassified resolve_failed; other read errors
		// stay unclassified (#2546 O1).
		if classifyNetworkError(err) == SkillErrCodeTimeout {
			return nil, &githubResolveError{
				code: SkillErrCodeTimeout,
				msg:  fmt.Sprintf("failed to read %s: %v", filePath, err),
				err:  err,
			}
		}
		return nil, fmt.Errorf("failed to read file content: %w", err)
	}
	if int64(len(content)) > int64(githubMaxFileSize) {
		return nil, fmt.Errorf("file %s exceeds maximum size of %d bytes", filePath, githubMaxFileSize)
	}
	return content, nil
}

func (r *GitHubSkillResolver) rawContentURL(ghRef *GitHubSkillRef, commitSHA, filePath string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		r.rawBase, ghRef.Owner, ghRef.Repo, commitSHA, escapePathSegments(filePath))
}

func escapePathSegments(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

func (r *GitHubSkillResolver) setAuthHeader(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// githubResolveError is a classified GitHub resolution failure, carrying a
// stable cause code (see the SkillErrCode* constants in skill_resolver.go) so
// Resolve can set ResolveError.Code — and provision.go's SkillResolutionError
// can in turn pick an HTTP status — without string-matching msg. retryAfter
// carries the raw Retry-After header value of the response that ended the
// call, when the server sent one; empty otherwise. err is
// the underlying cause, when there is one, so errors.Is and errors.As can
// reach it through Unwrap; msg alone is still what Error returns.
type githubResolveError struct {
	code       string
	msg        string
	retryAfter string
	err        error
}

func (e *githubResolveError) Error() string { return e.msg }

func (e *githubResolveError) Unwrap() error { return e.err }

// classifyRetryCause maps the response (and, when resp is nil, the
// network-level error) that triggered a retry or a fail-fast to a stable
// cause code:
//   - a response: upstream_unavailable. isRetryableResponse admits only 5xx,
//     so GitHub itself is failing, not the caller (#2546 R3). Rate-limit
//     responses never get here: GitHubCooldown.Do turns them into a
//     *GitHubRateLimitError, which doWithRetry returns without retrying.
//   - no response at all: delegated to classifyNetworkError, which tells a
//     genuine deadline apart from a DNS/connection/TLS failure.
func classifyRetryCause(resp *http.Response, err error) string {
	if resp == nil {
		return classifyNetworkError(err)
	}
	return SkillErrCodeUpstreamUnavailable
}

// classifyNetworkError distinguishes "no response arrived in time" — the
// overall ctx deadline expiring, or the httpClient's own ResponseHeaderTimeout
// firing on a stalled raw download (#2546 O2) — from every other
// network-level failure: DNS resolution, connection refused, TLS handshake
// errors. Only the latter group gets the unreachable cause; a timeout is kept
// distinct rather than folded into a blanket "unreachable" (#2546 N1). A nil
// err (not expected in practice; doWithRetry only reaches this classification
// when an attempt actually failed) is treated as a deadline for safety.
func classifyNetworkError(err error) string {
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		return SkillErrCodeTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return SkillErrCodeTimeout
	}
	return SkillErrCodeUnreachable
}

// retryAfterDuration parses resp's Retry-After header (integer seconds, as
// GitHub sends it) without capping it at githubMaxBackoff, so callers can
// tell a merely-long backoff apart from one longer than we are ever willing
// to wait (#2546 O3). ok is false when resp is nil or carries no parseable
// Retry-After.
func retryAfterDuration(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 0, false
	}
	seconds, err := strconv.ParseInt(ra, 10, 64)
	if err != nil || seconds < 0 {
		return 0, false
	}
	// Saturate rather than overflow: a huge value must still read as longer
	// than githubMaxBackoff.
	return secondsUpTo(seconds, time.Duration(math.MaxInt64)), true
}

// cancelOnCloseBody ties a per-attempt context's cancel func to the lifetime
// of the response body it guards: doOnce's bounded timeout must stay alive
// while the body is being read, but must not leak once the caller is done
// with it. Closing the body is the caller's existing signal for "done
// reading" (every call site already defers resp.Body.Close()), so piggy-
// backing cancellation on it needs no new caller-visible API.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// doOnce performs a single HTTP attempt bounded by attemptTimeout (see
// githubRequestTimeout and githubDownloadRequestTimeout). The timeout is
// applied via the request context rather than relying solely on
// r.httpClient's own Timeout, so it composes with (and is independent of) the
// caller's ctx deadline: whichever is shorter wins.
//
// The attempt goes through the cooldown tracker for identity: during a
// cooldown nothing is sent and a *GitHubRateLimitError is returned, and a
// rate-limit response starts a cooldown and is returned as one (see
// GitHubCooldown.Do).
func (r *GitHubSkillResolver) doOnce(ctx context.Context, req *http.Request, attemptTimeout time.Duration, identity string) (*http.Response, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	cloned := req.Clone(attemptCtx)
	resp, err := r.cooldownTracker().Do(r.httpClient, cloned, identity)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// doWithRetry executes an HTTP request with retry and exponential backoff
// for transient failures (5xx responses and transport errors). On
// retryable responses it respects the Retry-After header when present.
//
// Rate limits are not retried: every attempt goes through the shared
// GitHubCooldown for token's identity (see doOnce), so a rate-limit response
// (429, or a 403 reporting exhaustion or a secondary limit) starts a cooldown
// for that credential and returns a *GitHubRateLimitError straight away, and
// while a cooldown is in effect no request is sent at all. Waiting out the
// limit here would stall the caller (and, at provision time, the whole batch
// of refs behind it) for as long as GitHub asks, which can exceed the create
// deadline.
//
// Each attempt is bounded by attemptTimeout (see doOnce), so a single stalled
// request cannot by itself consume the whole create-deadline budget. Before
// sleeping for a backoff, doWithRetry also checks whether the delay still
// fits inside ctx's remaining deadline; if it doesn't, it fails immediately
// with a classified, ref-naming error instead of sleeping into a context
// cancellation that would otherwise surface only as an opaque "context
// canceled". Separately, when the server's own Retry-After exceeds
// githubMaxBackoff, doWithRetry fails fast rather than sleeping the capped
// backoff and retrying.
//
// Each failed attempt is also recorded as the latest cause of the shared
// fetch running under ctx (see recordAttemptCause), so a caller that stops
// waiting for that fetch on its own deadline can report it.
func (r *GitHubSkillResolver) doWithRetry(ctx context.Context, req *http.Request, attemptTimeout time.Duration, token string) (*http.Response, error) {
	identity := GitHubCooldownIdentity(token)
	var lastResp *http.Response
	var lastErr error
	noun, kind := "GitHub API request to", "api"
	if r.rawBase != "" && strings.HasPrefix(req.URL.String(), r.rawBase) {
		noun, kind = "GitHub raw download of", "raw"
	}

	for attempt := 0; attempt <= githubMaxRetries; attempt++ {
		if attempt > 0 {
			// A caller that cancelled while the previous attempt was in
			// flight gets context.Canceled back, as from the backoff select
			// below, not a typed failure classified from that attempt.
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, ctx.Err()
			}
			// A cooldown started for this identity since the previous attempt
			// (by another ref or caller sharing the credential) means the
			// next attempt would not be sent; fail now rather than sleep the
			// backoff first.
			if retryAt, cooling := r.cooldownTracker().Active(identity); cooling {
				return nil, &GitHubRateLimitError{RetryAt: retryAt, Unauthenticated: GitHubCooldownIdentityIsAnonymous(identity)}
			}
			status := -1
			retryAfterHeader := ""
			if lastResp != nil {
				status = lastResp.StatusCode
				retryAfterHeader = lastResp.Header.Get("Retry-After")
			}

			// The server has said explicitly when to come back; if that is
			// further out than our backoff cap, retrying now (capped at
			// githubMaxBackoff) would come back before it asked.
			if ra, ok := retryAfterDuration(lastResp); ok && ra > githubMaxBackoff {
				slog.Warn("github: retry-after exceeds backoff cap, failing fast",
					"method", req.Method, "path", req.URL.Path,
					"status", status, "retry_after", retryAfterHeader, "cap", githubMaxBackoff)
				return nil, &githubResolveError{
					code:       classifyRetryCause(lastResp, nil),
					retryAfter: retryAfterHeader,
					msg: fmt.Sprintf(
						"%s %s failed (status %d) and asked to retry after %s, "+
							"past the %s backoff cap: failing fast instead of retrying early",
						noun, req.URL.Path, status, ra, githubMaxBackoff),
				}
			}

			delay := retryDelay(lastResp, attempt)

			// Fail fast when the backoff, plus the stall bound a further
			// attempt needs to show it is alive, would run past ctx's
			// deadline, rather than sleeping most or all of it away only to
			// have the next request canceled. Reserving the stall bound, not
			// the full attempt timeout, keeps raw downloads retryable: their
			// attempt timeout equals the default budget, and a transfer still
			// in progress is bounded by ctx itself (#2546 RQ1).
			if dl, ok := ctx.Deadline(); ok {
				stall := r.stallBound()
				if remaining := time.Until(dl); delay+stall >= remaining {
					slog.Warn("github: skill resolution out of budget before backoff, failing fast",
						"method", req.Method, "path", req.URL.Path,
						"status", status, "retry_after", retryAfterHeader,
						"backoff", delay, "remaining_budget", remaining)
					return nil, &githubResolveError{
						code:       classifyRetryCause(lastResp, lastErr),
						retryAfter: retryAfterHeader,
						msg:        budgetExceededMessage(noun, req.URL.Path, status, retryAfterHeader, delay, remaining),
						err:        lastErr,
					}
				}
			}

			slog.Warn("github: retrying request after backoff",
				"kind", kind, "method", req.Method, "path", req.URL.Path,
				"status", status, "retry_after", retryAfterHeader,
				"backoff", delay, "attempt", attempt, "max_attempts", githubMaxRetries)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		resp, err := r.doOnce(ctx, req, attemptTimeout, identity)
		if err != nil {
			var rl *GitHubRateLimitError
			if errors.As(err, &rl) {
				return nil, err
			}
			lastErr = err
			lastResp = nil
			recordAttemptCause(ctx, &githubResolveError{
				code: classifyNetworkError(err),
				msg:  fmt.Sprintf("%s %s got no response", noun, req.URL.Path),
			})
			continue
		}

		if !isRetryableResponse(resp) {
			return resp, nil
		}

		if attempt == githubMaxRetries {
			return resp, nil
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		lastResp = resp
		lastErr = nil
		recordAttemptCause(ctx, &githubResolveError{
			code:       classifyRetryCause(resp, nil),
			retryAfter: resp.Header.Get("Retry-After"),
			msg:        fmt.Sprintf("%s %s failed (status %d)", noun, req.URL.Path, resp.StatusCode),
		})
	}

	// The only way to reach here is attempt == githubMaxRetries having just
	// `continue`d past a doOnce network-level error, which always sets
	// lastErr — classify it (DNS/connection/TLS vs. a genuine deadline) so
	// retries-exhausted network failures are as actionable as any other cause
	// (#2546 N1). A caller that cancelled during that last attempt gets
	// context.Canceled instead, as on the paths above; a deadline still
	// classifies as timeout via classifyNetworkError.
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, ctx.Err()
	}
	return nil, &githubResolveError{code: classifyNetworkError(lastErr), msg: lastErr.Error(), err: lastErr}
}

// budgetExceededMessage builds the error text for doWithRetry's ctx-budget
// fail-fast path. status is -1 when the previous attempt failed at the
// network level (no response at all) rather than returning an HTTP status, in
// which case retryAfter is also always empty; both render as "no response"
// rather than the misleading "status -1, retry-after " (#2546 N3). The
// deadline is described as the "request" deadline, not the "create" deadline,
// since the resolver also runs outside of create — on start, restart and
// reprovision (#2546 N2). noun names the path kind ("GitHub API request to"
// or "GitHub raw download of"), and a status is reported as a plain failure:
// only 5xx responses are retried, so only they take this path.
func budgetExceededMessage(noun, path string, status int, retryAfter string, delay, remaining time.Duration) string {
	outcome := fmt.Sprintf("failed (status %d)", status)
	if retryAfter != "" {
		outcome = fmt.Sprintf("failed (status %d, retry-after %s)", status, retryAfter)
	}
	if status == -1 {
		outcome = "got no response"
	}
	return fmt.Sprintf(
		"%s %s %s: next backoff %s would exceed the %s left before the request deadline",
		noun, path, outcome, delay, remaining)
}

// isRetryableResponse returns true for HTTP responses that should be
// retried: 5xx server errors. Rate-limit responses (429, or a 403 reporting
// exhaustion or a secondary limit) are never retried; GitHubCooldown.Do
// turns them into a *GitHubRateLimitError before they get here.
func isRetryableResponse(resp *http.Response) bool {
	return resp.StatusCode >= 500
}

// retryDelay calculates the backoff duration for a retry attempt.
// Uses the Retry-After header when present, otherwise exponential backoff.
func retryDelay(resp *http.Response, attempt int) time.Duration {
	if resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if seconds, err := strconv.ParseInt(ra, 10, 64); err == nil && seconds >= 0 {
				return secondsUpTo(seconds, githubMaxBackoff)
			}
		}
	}
	backoff := time.Duration(float64(githubBaseBackoff) * math.Pow(githubBackoffFactor, float64(attempt-1)))
	if backoff > githubMaxBackoff {
		backoff = githubMaxBackoff
	}
	return backoff
}

// apiError builds the error for a terminal non-OK response: either the last
// of githubMaxRetries retryable responses, or a non-retryable error status.
// Retries-exhausted 5xx responses are classified via githubResolveError so
// callers can map them to the right status without string-matching; every
// other status (401, 422, ...) keeps the existing uncategorized error, which
// resolves to the same 5xx path it always has (#2546 R3).
//
// The rate_limited branch is a defensive fallback only: GitHubCooldown.Do
// turns rate-limit responses into a *GitHubRateLimitError before any caller
// sees them, so it is unreachable today. It is kept so that a rate-limit
// response that ever got past the cooldown would still be reported as
// rate_limited (and not retried) rather than as a generic failure.
func (r *GitHubSkillResolver) apiError(resp *http.Response, action string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0") {
		return &githubResolveError{
			code:       SkillErrCodeRateLimited,
			retryAfter: resp.Header.Get("Retry-After"),
			msg: fmt.Sprintf("GitHub API rate limited while %s (retry-after=%s, resets at %s); set GITHUB_TOKEN for higher limits",
				action, resp.Header.Get("Retry-After"), resp.Header.Get("X-RateLimit-Reset")),
		}
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return &githubResolveError{
			code: SkillErrCodeUpstreamUnavailable,
			msg:  fmt.Sprintf("GitHub API error (%d) while %s, retries exhausted: %s", resp.StatusCode, action, string(body)),
		}
	}
	return fmt.Errorf("GitHub API error (%d) while %s: %s", resp.StatusCode, action, string(body))
}
