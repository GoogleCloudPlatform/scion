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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentplugin"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

const (
	defaultMaxFileSize = 10 * 1024 * 1024 // 10MB per file
	downloadTimeout    = 30 * time.Second
	stagingDirPrefix   = ".skill-staging-"
)

// SkillResolver resolves skill references to downloadable file sets.
type SkillResolver interface {
	// Resolve takes a batch of skill references and returns resolved skills.
	// Errors for individual skills are returned per-skill, not as a single error,
	// so optional skills can be skipped while required skills fail.
	Resolve(ctx context.Context, refs []api.SkillReference, opts ResolveOpts) (*ResolveResult, error)
}

// ResolveOpts provides context for scope-based resolution.
type ResolveOpts struct {
	ProjectID string
	UserID    string
}

// ResolveResult contains the batch resolution outcome.
type ResolveResult struct {
	Resolved []ResolvedSkill
	Errors   []ResolveError
}

// ResolveError represents a single skill that failed resolution.
// RetryAfter is a delay in whole seconds, as text, when one is known: for
// SkillErrCodeRateLimited, the time left on the credential's cooldown; for
// other causes, the upstream Retry-After header, if any. ProvisionAgent
// copies it to SkillResolutionError.RetryAfter. Status is the HTTP status
// of the upstream response that failed the resolution, when one is known,
// else 0; ProvisionAgent copies it to SkillResolutionError.Status.
type ResolveError struct {
	URI        string
	Code       string
	Message    string
	RetryAfter string
	Status     int
}

// Stable cause codes for ResolveError.Code and SkillResolutionError.Code.
// Resolvers that can distinguish these failure modes (currently
// GitHubSkillResolver) should set them so the create path can map a failure
// to the right HTTP status without string-matching Message. An empty or
// unrecognized code — including the Hub's own per-URI codes (storage_error,
// internal_error, federation_error) for PreResolvedSkills — is treated as an
// uncategorized resolution failure and kept on the existing 5xx path rather
// than guessed at (#2546 R3).
const (
	SkillErrCodeNotFound            = "not_found"
	SkillErrCodeRateLimited         = "rate_limited"
	SkillErrCodeTimeout             = "timeout"
	SkillErrCodeUpstreamUnavailable = "upstream_unavailable"
	SkillErrCodeUnreachable         = "unreachable"
	// SkillErrCodeForbidden is the per-URI code the Hub's batch skill
	// resolve returns for a gh:// ref when the caller may not resolve GitHub
	// skills for the project. It reaches the broker as a
	// SkillResolutionError.Code through PreResolvedSkills.
	SkillErrCodeForbidden = "forbidden"
	// SkillErrCodeResolveFailed is the uncategorized per-ref resolution
	// failure: the ref could not be resolved for a reason none of the codes
	// above describes. The create path keeps it on the 5xx path.
	SkillErrCodeResolveFailed = "resolve_failed"
	// SkillErrCodeInstallFailed is the code for a required skill that was
	// resolved but could not be downloaded, verified or installed into the
	// agent home. The create path keeps it on the 5xx path.
	SkillErrCodeInstallFailed = "install_failed"
	// SkillErrCodeInvalidURI is the code for a skill reference that does
	// not parse as a URI the resolver accepts.
	SkillErrCodeInvalidURI = "invalid_uri"

	// SkillErrCodeUnsupportedContent is the code for a gh:// skill whose
	// repository tree cannot be installed as a skill: it holds a git
	// submodule, more than MaxGitHubSkillFiles files or an unsafe path, or
	// GitHub truncated its listing (see IsGitHubSkillTreeError). The hub
	// and the local resolver both use it (ptone/scion#5781).
	SkillErrCodeUnsupportedContent = "unsupported_skill_content"
)

// SkillResolutionError is returned by ProvisionAgent when a required skill
// reference could not be resolved. It carries the ref URI and a stable Code
// (see the SkillErrCode* constants) alongside the human-readable Message, so
// the HTTP boundary (runtimebroker) can map it to the right status — naming
// the ref and the cause — instead of folding it into a generic 500/502.
// RetryAfter is a delay in whole seconds, as text, when one is known: for
// SkillErrCodeRateLimited, the time left on the credential's cooldown (see
// GitHubCooldown); for other causes, the upstream Retry-After header, if
// any. It is empty otherwise. The broker sends it as a Retry-After header
// only for SkillErrCodeRateLimited. Status is the HTTP status of the
// upstream response that failed the resolution, when one is known, else 0.
//
// Message is resolver text for the server-side log; it is not shown to the
// user (the agent runtime stores a fixed reason phrase instead).
type SkillResolutionError struct {
	URI        string
	Code       string
	Message    string
	RetryAfter string
	Status     int
}

func (e *SkillResolutionError) Error() string {
	if e.Code == SkillErrCodeInstallFailed {
		return fmt.Sprintf("required skill %q could not be installed: %s", e.URI, e.Message)
	}
	return fmt.Sprintf("required skill %q could not be resolved: %s", e.URI, e.Message)
}

// SkippedSkill records an optional skill that was resolved but could not be
// installed, and so was left out of the agent. installResolvedSkills returns
// them so a caller can surface them to the user; provisioning only logs them
// today (surfacing them to the creator is a follow-up).
type SkippedSkill struct {
	URI    string
	Reason string
}

// skillInstallFailedOnBroker is the fixed reason given to the user for an
// install failure whose error text is not known to be safe to show: local
// filesystem errors name broker paths. The full error is logged.
const skillInstallFailedOnBroker = "install failed on the broker"

// skillInstallError is an install failure whose public text is safe to show
// to the user who created the agent: it names no broker filesystem path and
// no download URL (a signed URL carries a token). Error returns the full
// text, for the broker log.
type skillInstallError struct {
	public string
	err    error
}

func (e *skillInstallError) Error() string { return e.err.Error() }
func (e *skillInstallError) Unwrap() error { return e.err }

// newSkillInstallError returns err with public as its user-facing text.
func newSkillInstallError(public string, err error) error {
	return &skillInstallError{public: public, err: err}
}

// publicInstallReason returns the user-facing text for an install failure:
// the public text of a skillInstallError in err's chain, or the fixed
// skillInstallFailedOnBroker for any other error.
func publicInstallReason(err error) string {
	var ie *skillInstallError
	if errors.As(err, &ie) {
		return ie.public
	}
	return skillInstallFailedOnBroker
}

// upstreamStatusError is a resolver error that came from a non-OK upstream
// HTTP response. Its text is the resolver's full message, for the log.
type upstreamStatusError struct {
	status int
	msg    string
}

func (e *upstreamStatusError) Error() string           { return e.msg }
func (e *upstreamStatusError) upstreamHTTPStatus() int { return e.status }

// upstreamStatus returns the HTTP status of the upstream response recorded
// in err's chain, or 0 when none is known.
func upstreamStatus(err error) int {
	var se interface{ upstreamHTTPStatus() int }
	for err != nil {
		if errors.As(err, &se) {
			if st := se.upstreamHTTPStatus(); st != 0 {
				return st
			}
		}
		err = errors.Unwrap(err)
	}
	return 0
}

// ResolvedSkill is a skill that was successfully resolved to downloadable files.
type ResolvedSkill struct {
	Name               string
	URI                string
	As                 string
	Version            string
	Hash               string // Bundle content hash v1 (sha256:...)
	HashV2             string // Bundle content hash v2 (ptone/scion#5935); verified when set (see verifySkillBundleHashes)
	Scope              string // Injection scope (hub, user, project, template, platform, "")
	Files              []ResolvedFile
	Deprecated         bool   `json:"-"`
	DeprecationMessage string `json:"-"`
	ReplacementURI     string `json:"-"`
	Optional           bool   `json:"-"` // Propagated from SkillReference for collision log level

	// githubCredentialRef is set by GitHubSkillResolver when it returns a
	// skill with at least one file whose Content is missing (an entry loaded
	// from the on-disk resolution cache). It holds the gh:// URI of the
	// request, which names at most the secret to use, never its value. The
	// install step passes it to the GitHub credential lookup in the context
	// (see ContextWithGitHubCredentialLookup) to find the same credential
	// the resolver used, so a private repo read with a named credential is
	// downloaded with that credential rather than the default one.
	githubCredentialRef string
}

// DestName returns the directory name to use when installing this skill.
func (rs *ResolvedSkill) DestName() (string, error) {
	name := rs.Name
	if rs.As != "" {
		name = rs.As
	}
	if err := api.ValidateSkillName(name); err != nil {
		return "", fmt.Errorf("invalid skill destination name %q: %w", name, err)
	}
	return name, nil
}

// ResolvedFile represents a single file within a resolved skill bundle.
type ResolvedFile struct {
	Path string
	URL  string
	Hash string
	Size int64
	// Mode is the file's recorded mode as the Hub serves it: "0755"
	// (executable) or "0644" (ptone/scion#5657). Resolvers that know no
	// mode, and Hubs from before modes were carried, leave it empty; the
	// install step writes such a file 0644 (transfer.NormalizeMode). Only
	// the executable bit is ever applied, never setuid, setgid or sticky.
	Mode string `json:",omitempty"`
	// Content holds the pre-fetched file bytes when available (e.g. from the
	// GitHub resolver, which downloads each file during resolution for hashing).
	// If non-nil, installOneSkill writes this directly and skips the network
	// re-download, preventing unauthenticated requests that would 404 on
	// private repos.
	//
	// The json:"-" tag intentionally excludes Content from the on-disk
	// GitHubResolutionCache. Entries loaded from disk (e.g. after a broker
	// restart within the TTL window) have Content == nil and fall back to
	// downloadSkillFile. For those, installOneSkill looks up the credential
	// the GitHub resolver would use for the skill's ref (see
	// ResolvedSkill.githubCredentialRef) so private repos still download.
	Content []byte `json:"-"`
}

// --- Context injection ---

type skillResolverContextKey struct{}

// ContextWithSkillResolver returns a new context with the SkillResolver attached.
func ContextWithSkillResolver(ctx context.Context, r SkillResolver) context.Context {
	return context.WithValue(ctx, skillResolverContextKey{}, r)
}

// SkillResolverFromContext retrieves the SkillResolver from the context, or nil if not set.
func SkillResolverFromContext(ctx context.Context) SkillResolver {
	r, _ := ctx.Value(skillResolverContextKey{}).(SkillResolver)
	return r
}

// ResolverNamer is an optional interface a SkillResolver can implement
// to provide a name for the resolution record.
type ResolverNamer interface {
	ResolverName() string
}

// resolverName returns the name to record for a resolver. If the resolver
// implements ResolverNamer, its name is used; otherwise "unknown".
func resolverName(r SkillResolver) string {
	if n, ok := r.(ResolverNamer); ok {
		return n.ResolverName()
	}
	return "unknown"
}

type resolveProjectIDKey struct{}
type resolveUserIDKey struct{}

// ContextWithResolveProjectID returns a context carrying the project ID for skill resolution.
func ContextWithResolveProjectID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, resolveProjectIDKey{}, id)
}

// ResolveProjectIDFromContext retrieves the project ID for skill resolution.
func ResolveProjectIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(resolveProjectIDKey{}).(string)
	return v
}

// ContextWithResolveUserID returns a context carrying the user ID for skill resolution.
func ContextWithResolveUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, resolveUserIDKey{}, id)
}

// ResolveUserIDFromContext retrieves the user ID for skill resolution.
func ResolveUserIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(resolveUserIDKey{}).(string)
	return v
}

type gitHubTokenKey struct{}

// ContextWithGitHubToken returns a context carrying a GitHub token for the
// install phase.
//
// When the Hub resolves a gh:// skill it returns raw.githubusercontent.com
// URLs but, deliberately, not the credential behind them — a Hub-minted App
// token must not travel in an API response body or be persisted to the
// broker's on-disk resolution cache. The broker therefore supplies its own
// GITHUB_TOKEN here, which downloadSkillFile presents when fetching from
// GitHub. Public repos need no token; private repos require one that can read
// the repo in question.
func ContextWithGitHubToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, gitHubTokenKey{}, token)
}

type gitHubCredentialLookupKey struct{}

// GitHubCredentialLookup returns the GitHub credential to use for the given
// gh:// URI, or "" if none applies. It is used at install time for skills
// whose file content was not kept (see ResolvedSkill.githubCredentialRef).
type GitHubCredentialLookup func(uri string) string

// ContextWithGitHubCredentialLookup returns a context carrying lookup for the
// install phase. The broker sets it to GitHubSkillResolver.CredentialForURI
// of the resolver it uses for the same request, so install finds the same
// credential the resolver used without that value travelling on the
// resolved skill.
func ContextWithGitHubCredentialLookup(ctx context.Context, lookup GitHubCredentialLookup) context.Context {
	return context.WithValue(ctx, gitHubCredentialLookupKey{}, lookup)
}

// Labels naming where a skill download credential came from. They appear in
// logs and errors in place of the credential itself. The GitHub resolver
// supplies more specific labels (secret names, never values) through the
// credential source lookup; gitHubCredSourceResolver is the fallback when it
// has none.
const (
	gitHubCredSourceDefault  = "the GITHUB_TOKEN credential"
	gitHubCredSourceResolver = "the credential the GitHub skill resolver uses for this skill"
)

type gitHubCredentialSourceLookupKey struct{}

// gitHubCredentialSourceLookup returns a label naming where the credential
// GitHubCredentialLookup returns for a gh:// URI comes from, or "".
type gitHubCredentialSourceLookup func(uri string) string

// contextWithGitHubCredentialSourceLookup returns a context carrying lookup
// for the install phase, alongside the GitHubCredentialLookup it describes.
func contextWithGitHubCredentialSourceLookup(ctx context.Context, lookup gitHubCredentialSourceLookup) context.Context {
	return context.WithValue(ctx, gitHubCredentialSourceLookupKey{}, lookup)
}

// credentialSourceLabel returns the label for uri's credential from the
// source lookup carried by ctx, or gitHubCredSourceResolver if there is none.
func credentialSourceLabel(ctx context.Context, uri string) string {
	if lookup, ok := ctx.Value(gitHubCredentialSourceLookupKey{}).(gitHubCredentialSourceLookup); ok && lookup != nil {
		if label := lookup(uri); label != "" {
			return label
		}
	}
	return gitHubCredSourceResolver
}

// gitHubCredential is the credential installOneSkill presents for a skill's
// raw downloads, with a label naming its source. The token is never
// formatted: every fmt verb prints only the source (see Format).
type gitHubCredential struct {
	token  string
	source string
}

func (c gitHubCredential) String() string {
	if c.token == "" {
		return "gitHubCredential(none)"
	}
	return "gitHubCredential(" + c.source + ")"
}

func (c gitHubCredential) GoString() string { return c.String() }

// Format prints String for every verb, so no verb (%d, %x, ...) falls back
// to printing the struct fields.
func (c gitHubCredential) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// gitHubDownloadToken returns the credential installOneSkill presents for
// skill's raw downloads: the credential the GitHub resolver uses for the
// skill's ref, when the skill came from that resolver without content and a
// lookup is available, and otherwise the context's default GitHub token.
// downloadSkillFile only ever sends either to GitHub hosts.
func gitHubDownloadToken(ctx context.Context, skill ResolvedSkill) gitHubCredential {
	if skill.githubCredentialRef != "" {
		if tok := credentialLookupFromContext(ctx)(skill.githubCredentialRef); tok != "" {
			return gitHubCredential{token: tok, source: credentialSourceLabel(ctx, skill.githubCredentialRef)}
		}
	}
	if tok := GitHubTokenFromContext(ctx); tok != "" {
		return gitHubCredential{token: tok, source: gitHubCredSourceDefault}
	}
	return gitHubCredential{}
}

// credentialLookupFromContext returns the GitHub credential lookup carried
// by ctx, or a lookup that always returns "" if there is none.
func credentialLookupFromContext(ctx context.Context) GitHubCredentialLookup {
	if lookup, ok := ctx.Value(gitHubCredentialLookupKey{}).(GitHubCredentialLookup); ok && lookup != nil {
		return lookup
	}
	return func(string) string { return "" }
}

// GitHubTokenFromContext retrieves the GitHub token for the install phase,
// or "" if none was set.
func GitHubTokenFromContext(ctx context.Context) string {
	v, _ := ctx.Value(gitHubTokenKey{}).(string)
	return v
}

// --- Resolution record types ---

// SkillResolutionRecord is written to agentHome/.scion/resolved-skills.json
// after successful skill installation.
type SkillResolutionRecord struct {
	ResolvedAt string                 `json:"resolvedAt"`
	Resolver   string                 `json:"resolver"`
	Skills     []SkillResolutionEntry `json:"skills"`
	Collisions []SkillCollisionEntry  `json:"collisions,omitempty"`
}

// SkillResolutionEntry records a single installed skill.
type SkillResolutionEntry struct {
	URI                string      `json:"uri"`
	Name               string      `json:"name"`
	As                 string      `json:"as,omitempty"`
	ResolvedVersion    string      `json:"resolvedVersion"`
	ContentHash        string      `json:"contentHash"`
	Scope              string      `json:"scope"`
	InstalledPath      string      `json:"installedPath"`
	Source             string      `json:"source"`
	Files              []FileEntry `json:"files"`
	Deprecated         bool        `json:"deprecated,omitempty"`
	DeprecationMessage string      `json:"deprecationMessage,omitempty"`
	ReplacementURI     string      `json:"replacementUri,omitempty"`
}

// FileEntry records a single file within an installed skill.
type FileEntry struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// --- Destination-name collision resolution ---

// scopeRank defines the precedence order for skill scopes when resolving
// destination-name collisions. Higher rank wins.
//
// Precedence (highest to lowest): project > template > user > hub > platform > (unset)
//
// Rationale: project-level settings take precedence for destination-name
// collisions, as approved in #654. Note: hub-side base-URI dedup
// (mergeSkillRefs) uses the opposite order (template > project); the two
// dedup passes operate on different dimensions (base URI vs dest name).
var scopeRank = map[string]int{
	"":         0,
	"platform": 1,
	"hub":      2,
	"user":     3,
	"template": 4,
	"project":  5,
}

// SkillCollisionEntry records a single destination-name collision that was
// resolved during skill installation. Persisted in resolved-skills.json.
type SkillCollisionEntry struct {
	DestName     string `json:"destName"`
	WinnerURI    string `json:"winnerUri"`
	WinnerScope  string `json:"winnerScope"`
	DroppedURI   string `json:"droppedUri"`
	DroppedScope string `json:"droppedScope"`
}

// deduplicateByDestName resolves destination-name collisions among resolved
// skills using scope-based precedence. When two skills produce the same
// DestName(), the skill with the higher scope rank wins. On equal scope,
// the later entry wins (last-writer-wins, consistent with mergeSkillRefs).
//
// A required loser makes the winner required (Optional is cleared), so an
// optional winner that then fails to install fails the install instead of
// silently dropping the required declaration.
//
// Returns the deduplicated list (preserving winner order of first appearance)
// and a slice of collision entries for diagnostics.
func deduplicateByDestName(ctx context.Context, skills []ResolvedSkill) ([]ResolvedSkill, []SkillCollisionEntry) {
	type candidate struct {
		index int
		skill ResolvedSkill
		dest  string
	}

	winners := map[string]*candidate{} // destName → winning candidate
	var collisions []SkillCollisionEntry
	var passthrough []ResolvedSkill // skills with DestName errors; cannot participate in dedup

	for i, skill := range skills {
		dest, err := skill.DestName()
		if err != nil {
			// Cannot participate in dedup; pass through so the install loop
			// surfaces the DestName error with a clear diagnostic.
			passthrough = append(passthrough, skill)
			continue
		}

		existing, exists := winners[dest]
		if !exists {
			winners[dest] = &candidate{index: i, skill: skill, dest: dest}
			continue
		}

		// Resolve collision: higher scope wins; on tie, later entry wins.
		newRank := scopeRank[skill.Scope]
		existingRank := scopeRank[existing.skill.Scope]

		var winner, loser ResolvedSkill
		if newRank >= existingRank {
			winner = skill
			loser = existing.skill
			winners[dest] = &candidate{index: i, skill: skill, dest: dest}
		} else {
			winner = existing.skill
			loser = skill
		}

		// A required loser makes the winner required: otherwise an optional
		// winner that then fails to install would be skipped, and the
		// required declaration would be dropped without an error (#5020).
		winners[dest].skill.Optional = winners[dest].skill.Optional && loser.Optional

		collisions = append(collisions, SkillCollisionEntry{
			DestName:     dest,
			WinnerURI:    winner.URI,
			WinnerScope:  winner.Scope,
			DroppedURI:   loser.URI,
			DroppedScope: loser.Scope,
		})

		// The destination name comes from both references, so it is
		// withheld when either is redacted (see LoggedSkillName).
		destForLog := LoggedSkillName(winner.URI, dest)
		if SkillRefRedacted(loser.URI) {
			destForLog = SkillNameWithheld
		}

		// Log at appropriate level: Debug for optional losers, Warn otherwise.
		if loser.Optional {
			slog.DebugContext(ctx, "skill destination-name collision resolved (optional skill dropped)",
				"dest_name", destForLog,
				"winner_uri", LoggedSkillRef(winner.URI),
				"winner_scope", winner.Scope,
				"dropped_uri", LoggedSkillRef(loser.URI),
				"dropped_scope", loser.Scope,
			)
		} else {
			slog.WarnContext(ctx, "skill destination-name collision resolved",
				"dest_name", destForLog,
				"winner_uri", LoggedSkillRef(winner.URI),
				"winner_scope", winner.Scope,
				"dropped_uri", LoggedSkillRef(loser.URI),
				"dropped_scope", loser.Scope,
			)
		}
	}

	// Build deduplicated list preserving original order of winners.
	// Collect and sort by first-appearance index so the output is deterministic.
	sorted := make([]*candidate, 0, len(winners))
	for _, c := range winners {
		sorted = append(sorted, c)
	}
	slices.SortFunc(sorted, func(a, b *candidate) int {
		return a.index - b.index
	})
	deduplicated := make([]ResolvedSkill, 0, len(sorted)+len(passthrough))
	for _, c := range sorted {
		deduplicated = append(deduplicated, c.skill)
	}
	// Append skills with DestName errors so installResolvedSkills surfaces
	// the error rather than silently dropping the skill.
	deduplicated = append(deduplicated, passthrough...)

	return deduplicated, collisions
}

// --- Download, stage, verify, install ---

// installResolvedSkills downloads, verifies, and installs resolved skills
// into the agent's skill directory.
//
// A skill that fails to install is handled by its Optional flag. An optional
// skill is skipped: the failure is logged as a warning naming the skill and
// the reason, the skill is returned in the skipped list, and the remaining
// skills are still installed. A required skill fails the whole install with a
// *SkillResolutionError (Code SkillErrCodeInstallFailed) naming the skill.
// Its Message is user-facing (see publicInstallReason); the full error is
// logged.
func installResolvedSkills(
	ctx context.Context,
	skills []ResolvedSkill,
	skillsDest string,
	agentHome string,
) (*SkillResolutionRecord, []SkippedSkill, error) {
	// S6: Resolve destination-name collisions via scope-based precedence.
	// Previously this was a hard error; now collisions are resolved by dropping
	// the lower-scope skill and logging a warning.
	skills, collisions := deduplicateByDestName(ctx, skills)

	if err := os.MkdirAll(skillsDest, 0755); err != nil {
		return nil, nil, fmt.Errorf("failed to create skills directory: %w", err)
	}

	record := &SkillResolutionRecord{
		ResolvedAt: time.Now().UTC().Format(time.RFC3339),
		Resolver:   "mock",
		Collisions: collisions,
	}

	var skipped []SkippedSkill
	for _, skill := range skills {
		entry, err := installSkillAt(ctx, skill, skillsDest)
		if err != nil {
			reason := publicInstallReason(err)
			if skill.Optional {
				slog.WarnContext(ctx, "optional skill could not be installed; skipped",
					"skill_uri", LoggedSkillRef(skill.URI), "skill_name", LoggedSkillName(skill.URI, skill.Name),
					"reason", ResolverReasonForLog(skill.URI, reason),
					"error", ResolverReasonForLog(skill.URI, err.Error()))
				skipped = append(skipped, SkippedSkill{URI: skill.URI, Reason: reason})
				continue
			}
			slog.WarnContext(ctx, "required skill could not be installed",
				"skill_uri", LoggedSkillRef(skill.URI), "skill_name", LoggedSkillName(skill.URI, skill.Name),
				"reason", ResolverReasonForLog(skill.URI, reason),
				"error", ResolverReasonForLog(skill.URI, err.Error()))
			return nil, nil, &SkillResolutionError{URI: skill.URI, Code: SkillErrCodeInstallFailed, Message: reason}
		}
		record.Skills = append(record.Skills, *entry)

		if skill.Deprecated {
			msg := fmt.Sprintf("Warning: skill %s@%s is deprecated", LoggedSkillName(skill.URI, skill.Name), skill.Version)
			if skill.DeprecationMessage != "" {
				msg += ": " + skill.DeprecationMessage
			}
			if skill.ReplacementURI != "" {
				msg += fmt.Sprintf(" (replacement: %s)", skill.ReplacementURI)
			}
			fmt.Fprintln(os.Stderr, msg)
		}
	}

	return record, skipped, nil
}

// installSkillAt validates skill's destination name and installs it under
// skillsDest.
func installSkillAt(ctx context.Context, skill ResolvedSkill, skillsDest string) (*SkillResolutionEntry, error) {
	dest, err := skill.DestName()
	if err != nil {
		// The name comes from the skill, not the broker, so the text is safe.
		return nil, newSkillInstallError(err.Error(), err)
	}
	return installOneSkill(ctx, skill, dest, skillsDest)
}

func installOneSkill(ctx context.Context, skill ResolvedSkill, dest, skillsDest string) (*SkillResolutionEntry, error) {
	// Every source (cache or download) is staged first; finalDest is
	// touched only by the final rename in commitStagedSkill, so a failed
	// install leaves neither a half-written skill dir nor a removed
	// pre-existing one behind (#5020: an optional skill's failure no longer
	// fails the create, so such leftovers would reach the agent).
	finalDest := filepath.Join(skillsDest, dest)
	stagingDir, err := os.MkdirTemp(skillsDest, stagingDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create staging directory: %w", err)
	}
	defer func() {
		// Clean up staging dir on any failure
		_ = os.RemoveAll(stagingDir)
	}()
	skillStagingDir := filepath.Join(stagingDir, dest)

	// S3: validate every path from the resolve response before anything
	// uses it. The cache-hit branch below reads and chmods these paths, so
	// the check must come before it, not only before the downloads
	// (ptone/scion#5657).
	for _, f := range skill.Files {
		if err := validateFilePath(f.Path); err != nil {
			// The path comes from the skill bundle, so the text is safe.
			msg := fmt.Sprintf("unsafe file path in skill %q: %v", skill.URI, err)
			return nil, newSkillInstallError(msg, errors.New(msg))
		}
	}

	// Check cache before downloading
	cache := SkillCacheFromContext(ctx)
	if cache != nil && skill.Hash != "" {
		if cachedPath, hit := cache.Get(skillCacheKey(skill)); hit {
			copyErr := cache.CopyToDir(cachedPath, skillStagingDir)
			if copyErr == nil {
				copyErr = verifyInstalledSkillHash(skillStagingDir, skill)
			}
			if copyErr == nil && skill.HashV2 != "" {
				// verifyInstalledSkillHash checked every listed file that
				// has a per-file hash, so v2 over the listed hashes is v2
				// of the copied files. A file listed without a hash puts
				// an empty hash into the v2 preimage, so the check fails
				// and the skill is downloaded and verified instead.
				listed := make([]transfer.FileInfo, len(skill.Files))
				for i, f := range skill.Files {
					listed[i] = transfer.FileInfo{Path: f.Path, Hash: f.Hash}
				}
				copyErr = verifySkillBundleHashV2(skill, listed)
			}
			if copyErr == nil {
				// The entry key already covers the executable bits, but
				// set each file's mode again so the install does not depend
				// on how the cached copy was written.
				copyErr = applySkillFileModes(skillStagingDir, skill)
			}
			if copyErr == nil {
				if err := commitStagedSkill(skillStagingDir, finalDest, dest); err != nil {
					return nil, err
				}
				util.Debugf("provision: skill installed from cache: %s@%s", LoggedSkillName(skill.URI, skill.Name), skill.Version)
				slog.InfoContext(ctx, "skill_content_cache: cache hit",
					"skill", LoggedSkillName(skill.URI, skill.Name), "version", skill.Version,
					"hash", truncHash(skill.Hash), "cache_hit", true)
				return buildSkillEntry(skill, dest, skillsDest)
			}
			// Cache entry unusable (copy or hash verification failed) — re-downloading.
			util.Debugf("provision: cached skill unusable, falling through to download: %s", ResolverReasonForLog(skill.URI, copyErr.Error()))
			_ = os.RemoveAll(skillStagingDir)
			slog.InfoContext(ctx, "skill_content_cache: cache error, re-downloading",
				"skill", LoggedSkillName(skill.URI, skill.Name), "version", skill.Version,
				"hash", truncHash(skill.Hash), "cache_hit", false)
		} else {
			slog.InfoContext(ctx, "skill_content_cache: cache miss, will download",
				"skill", LoggedSkillName(skill.URI, skill.Name), "version", skill.Version,
				"hash", truncHash(skill.Hash), "cache_hit", false)
		}
	}

	if err := os.MkdirAll(skillStagingDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create skill staging dir: %w", err)
	}

	// Credential for raw.githubusercontent.com downloads: for a skill the
	// broker's GitHub resolver served without content, the credential it
	// uses for that ref; otherwise the default token, used for gh:// skills
	// the Hub resolved on our behalf. Empty for public repos.
	ghCred := gitHubDownloadToken(ctx, skill)

	var fileEntries []FileEntry

	for _, f := range skill.Files {
		// f.Path was validated (S3) at the top of the function.
		destPath := filepath.Join(skillStagingDir, f.Path)

		// Create parent directories for nested files
		if dir := filepath.Dir(destPath); dir != skillStagingDir {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, fmt.Errorf("failed to create directory for %s: %w", f.Path, err)
			}
		}

		// S5: Write pre-fetched content or download with transport constraints.
		// GitHubSkillResolver carries content bytes from the authenticated
		// resolution-phase download; using them here avoids a second,
		// unauthenticated raw.githubusercontent.com request that would 404 on
		// private repos.
		//
		// Note: entries loaded from the on-disk resolution cache have
		// Content == nil (json:"-" strips it), so they fall through to
		// downloadSkillFile with the credential chosen above.
		if f.Content != nil {
			if err := writeSkillFileContent(f.Content, destPath); err != nil {
				return nil, wrapSkillFileError("failed to write", f.Path, err)
			}
		} else if err := downloadSkillFile(ctx, f.URL, destPath, defaultMaxFileSize, ghCred); err != nil {
			return nil, wrapSkillFileError("failed to download", f.Path, err)
		}

		// Both writers create the file 0644 (under the umask); give it
		// the recorded mode, so an executable skill script stays
		// executable (ptone/scion#5657).
		if err := os.Chmod(destPath, transfer.NormalizeMode(f.Mode)); err != nil { //winsec:not-protection sets a skill file's recorded mode bits; skills are installed only by agent provisioning and start
			return nil, wrapSkillFileError("failed to set mode of", f.Path, err)
		}

		// S2: Verify per-file hash, in whichever format the resolver supplied.
		actualHash, err := hashFileAs(destPath, f.Hash)
		if err != nil {
			return nil, fmt.Errorf("failed to hash %s: %w", f.Path, err)
		}
		if f.Hash != "" && actualHash != f.Hash {
			msg := fmt.Sprintf("hash mismatch for file %q in skill %q: expected %s, got %s",
				f.Path, skill.URI, f.Hash, actualHash)
			return nil, newSkillInstallError(msg, errors.New(msg))
		}

		fileEntries = append(fileEntries, FileEntry{
			Path: f.Path,
			Hash: actualHash,
		})
	}

	// S2: Verify the bundle hashes: v1 (transfer.ComputeContentHashV1,
	// what the hub's contentHash carries) and, when the hub sent one, v2
	// (ptone/scion#5935), each over the hashes of the downloaded files.
	installed := make([]transfer.FileInfo, len(fileEntries))
	for i, fe := range fileEntries {
		installed[i] = transfer.FileInfo{Path: fe.Path, Hash: fe.Hash}
	}
	if err := verifySkillBundleHashes(skill, installed); err != nil {
		return nil, err
	}

	if err := commitStagedSkill(skillStagingDir, finalDest, dest); err != nil {
		return nil, err
	}

	// Populate cache after successful download+verify+install
	if cache != nil && skill.Hash != "" {
		populateSkillCache(cache, skill, finalDest)
	}

	return buildSkillEntry(skill, dest, skillsDest)
}

// verifySkillBundleHashes checks the bundle hashes of skill against files,
// the path and hash of each installed file: v1 when skill.Hash is set and
// v2 when skill.HashV2 is set (ptone/scion#5935); any mismatch fails
// closed. The v2 preimage takes each file's mode from the resolve metadata
// (skill.Files), normalised with transfer.NormalizedModeString as on the
// hub, so a missing mode counts as "0644" on both sides; it is the mode the
// file is installed with, never one read back from disk.
func verifySkillBundleHashes(skill ResolvedSkill, files []transfer.FileInfo) error {
	if skill.Hash != "" {
		if got := transfer.ComputeContentHashV1(files); got != skill.Hash {
			msg := fmt.Sprintf("bundle hash mismatch for skill %q: expected %s, got %s",
				skill.URI, skill.Hash, got)
			return newSkillInstallError(msg, errors.New(msg))
		}
	}
	if skill.HashV2 != "" {
		return verifySkillBundleHashV2(skill, files)
	}
	return nil
}

// verifySkillBundleHashV2 checks skill.HashV2 against files; see
// verifySkillBundleHashes.
func verifySkillBundleHashV2(skill ResolvedSkill, files []transfer.FileInfo) error {
	modes := make(map[string]string, len(skill.Files))
	for _, f := range skill.Files {
		modes[f.Path] = f.Mode
	}
	withModes := make([]transfer.FileInfo, len(files))
	for i, f := range files {
		f.Mode = transfer.NormalizedModeString(modes[f.Path])
		withModes[i] = f
	}
	if got := transfer.ComputeContentHashV2(withModes); got != skill.HashV2 {
		msg := fmt.Sprintf("bundle hash v2 mismatch for skill %q: expected %s, got %s",
			skill.URI, skill.HashV2, got)
		return newSkillInstallError(msg, errors.New(msg))
	}
	return nil
}

// commitStagedSkill moves the verified skill at staged to finalDest,
// replacing any existing directory there (S3: atomic install).
func commitStagedSkill(staged, finalDest, dest string) error {
	if _, err := os.Stat(finalDest); err == nil {
		if err := os.RemoveAll(finalDest); err != nil {
			return fmt.Errorf("failed to remove existing skill dir %s: %w", dest, err)
		}
	}
	if err := os.Rename(staged, finalDest); err != nil {
		return fmt.Errorf("failed to install skill %s: %w", dest, err)
	}
	return nil
}

// wrapSkillFileError wraps err, a failure to write or download the skill file
// at path (relative to the skill), as "<action> <path>: ...". It keeps a
// user-facing text only when err has one; a local filesystem error, which
// names a broker path, gets none, so the user sees the fixed reason.
func wrapSkillFileError(action, path string, err error) error {
	wrapped := fmt.Errorf("%s %s: %w", action, path, err)
	var ie *skillInstallError
	if errors.As(err, &ie) {
		return newSkillInstallError(fmt.Sprintf("%s %s: %s", action, path, ie.public), wrapped)
	}
	return wrapped
}

// buildSkillEntry creates a SkillResolutionEntry for a successfully installed skill.
func buildSkillEntry(skill ResolvedSkill, dest, skillsDest string) (*SkillResolutionEntry, error) {
	scope := skill.Scope
	if scope == "" {
		// Fall back to URI-derived scope when the injection scope is not set.
		parsed, err := api.ParseSkillURI(skill.URI)
		if err == nil {
			scope = parsed.Scope
		}
	}

	var fileEntries []FileEntry
	for _, f := range skill.Files {
		fileEntries = append(fileEntries, FileEntry{
			Path: f.Path,
			Hash: f.Hash,
		})
	}

	return &SkillResolutionEntry{
		URI:                skill.URI,
		Name:               skill.Name,
		As:                 skill.As,
		ResolvedVersion:    skill.Version,
		ContentHash:        skill.Hash,
		Scope:              scope,
		InstalledPath:      filepath.ToSlash(filepath.Join(filepath.Base(skillsDest), dest)),
		Source:             "registry",
		Files:              fileEntries,
		Deprecated:         skill.Deprecated,
		DeprecationMessage: skill.DeprecationMessage,
		ReplacementURI:     skill.ReplacementURI,
	}, nil
}

// skillFileModes returns the normalised mode of each file of skill, keyed
// by its path in the skill.
func skillFileModes(skill ResolvedSkill) map[string]os.FileMode {
	modes := make(map[string]os.FileMode, len(skill.Files))
	for _, f := range skill.Files {
		modes[f.Path] = transfer.NormalizeMode(f.Mode)
	}
	return modes
}

// skillCacheKey is the skill cache entry name for skill. The bundle hash
// (transfer.ComputeContentHashV1, which skills keep; see the hub's
// computeSkillContentHash) does not cover file modes, so a skill with an
// executable file is keyed by a hash derived from the bundle hash and its
// sorted executable paths: an entry cached before modes were carried
// (every file 0644) is then never served for it (ptone/scion#5657). A
// skill with no executable file keeps the bare bundle hash, so its
// existing entries stay valid. Both forms are canonical "sha256:<hex64>"
// names.
//
// Templates and harness-configs key their cache entries with
// templatecache.EntryKey, the v2 content hash of their files
// (ptone/scion#5658). Skills keep this v1-based scheme even when the hub
// sends a v2 hash (ptone/scion#5935), so existing entries stay valid; a
// cache hit is still checked against v2 (see installOneSkill). The
// derivation is unchanged from ptone/scion#5657.
func skillCacheKey(skill ResolvedSkill) string {
	var exec []string
	for p, m := range skillFileModes(skill) {
		if m&0o111 != 0 {
			exec = append(exec, p)
		}
	}
	if len(exec) == 0 {
		return skill.Hash
	}
	slices.Sort(exec)
	var b strings.Builder
	b.WriteString("scion-cache-entry-v1\x00")
	b.WriteString(skill.Hash)
	for _, p := range exec {
		b.WriteString("\x00x:")
		b.WriteString(p)
	}
	return transfer.HashBytes([]byte(b.String()))
}

// applySkillFileModes sets each file of skill under dir to its normalised
// mode. The chmods go through an os.Root opened on dir, so a path that
// escapes dir (through "..", an absolute path or a symlink) is refused
// rather than changing a file outside it; callers validate the paths
// first as well.
func applySkillFileModes(dir string, skill ResolvedSkill) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("failed to open skill directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	for _, f := range skill.Files {
		if err := root.Chmod(filepath.FromSlash(f.Path), transfer.NormalizeMode(f.Mode)); err != nil { //winsec:not-protection sets a skill file's recorded mode bits; skills are installed only by agent provisioning and start
			return fmt.Errorf("failed to set mode of %s: %w", f.Path, err)
		}
	}
	return nil
}

// populateSkillCache stores downloaded skill files in the cache, with
// their modes, under skillCacheKey.
func populateSkillCache(cache interface {
	PutWithModes(string, map[string][]byte, map[string]os.FileMode) (string, error)
}, skill ResolvedSkill, installedDir string) {
	files := make(map[string][]byte, len(skill.Files))
	for _, f := range skill.Files {
		content, err := os.ReadFile(filepath.Join(installedDir, f.Path))
		if err != nil {
			util.Debugf("provision: failed to read skill file for caching: %s: %s", f.Path, ResolverReasonForLog(skill.URI, err.Error()))
			return
		}
		files[f.Path] = content
	}
	key := skillCacheKey(skill)
	if _, err := cache.PutWithModes(key, files, skillFileModes(skill)); err != nil {
		util.Debugf("provision: failed to cache skill %s@%s: %v", LoggedSkillName(skill.URI, skill.Name), skill.Version, err)
	} else {
		util.Debugf("provision: cached skill %s@%s (%s)", LoggedSkillName(skill.URI, skill.Name), skill.Version, key)
	}
}

func verifyInstalledSkillHash(dir string, skill ResolvedSkill) error {
	for _, f := range skill.Files {
		path := filepath.Join(dir, f.Path)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("missing file %s: %w", f.Path, err)
		}
		if f.Hash == "" {
			continue
		}
		computed := hashBytesAs(data, f.Hash)
		if computed != f.Hash {
			return fmt.Errorf("hash mismatch for %s: expected %s, got %s", f.Path, f.Hash, computed)
		}
	}
	return nil
}

// hashFileAs hashes the file at path using the same algorithm as the expected
// hash, so the two are directly comparable.
//
// Two formats reach the install phase:
//
//   - "sha256:<hex64>" — produced by the Hub's registry storage and by the
//     local GitHubSkillResolver, which downloads content during resolution.
//   - a bare git blob object ID (40 hex chars) — produced by the Hub's gh://
//     resolution cache, which copies the "sha" field straight out of GitHub's
//     git trees API and so never sees the file bytes.
//
// An empty expected hash means the resolver published no per-file digest; the
// file is still hashed as sha256 so the resolution record has a usable value,
// and the caller skips comparison.
func hashFileAs(path, expected string) (string, error) {
	if transfer.IsGitBlobHash(expected) {
		return transfer.GitBlobHashFile(path)
	}
	return transfer.HashFile(path)
}

// hashBytesAs is the in-memory counterpart to hashFileAs.
func hashBytesAs(data []byte, expected string) string {
	if transfer.IsGitBlobHash(expected) {
		return transfer.GitBlobHashBytes(data)
	}
	return transfer.HashBytes(data)
}

// validateFilePath checks that a relative path is safe for extraction.
func validateFilePath(path string) error {
	if path == "" {
		return fmt.Errorf("empty file path")
	}

	// Check for NUL bytes
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("path contains NUL byte")
	}

	// Check for backslashes
	if strings.Contains(path, "\\") {
		return fmt.Errorf("path contains backslash: %q", path)
	}

	// Clean the path and check for absolute paths
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) {
		return fmt.Errorf("absolute path not allowed: %q", path)
	}

	// Check for .. components
	for _, component := range strings.Split(cleaned, string(filepath.Separator)) {
		if component == ".." {
			return fmt.Errorf("path traversal not allowed: %q", path)
		}
	}

	// Check for OS-reserved names (Windows-safe, defensive)
	reserved := map[string]bool{
		"CON": true, "PRN": true, "AUX": true, "NUL": true,
		"COM1": true, "COM2": true, "COM3": true, "COM4": true,
		"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
		"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
		"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	}
	baseName := strings.ToUpper(filepath.Base(cleaned))
	// Strip extension for reserved name check
	if idx := strings.IndexByte(baseName, '.'); idx >= 0 {
		baseName = baseName[:idx]
	}
	if reserved[baseName] {
		return fmt.Errorf("OS-reserved file name not allowed: %q", path)
	}

	return nil
}

// isGitHubHost reports whether host belongs to GitHub, and is therefore a
// permitted recipient of a GitHub token.
func isGitHubHost(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" ||
		strings.HasSuffix(host, ".github.com") ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

// downloadSkillFile downloads a single file from a URL to a local path.
//
// ghCred's token, when non-empty, is sent as a bearer credential — but only to
// GitHub hosts. Registry skills are served from signed object-store URLs that
// have no business seeing a GitHub token, so the host check is what keeps the
// credential from leaking to an unrelated origin named in a Hub response.
//
// GitHub answers 404 (or 401/403) for a public file when the credential sent
// with it is rejected, for example an expired or revoked user token. So when
// a credential was sent and the answer is one of those, the download is
// retried exactly once, at the same URL, without credentials: public skills
// still install, and the caller's hash checks apply to the result as usual.
// If the retry fails too, the error is a *gitHubCredentialRejectedError. The
// retry uses the same client, so the cross-host redirect ban applies to it.
func downloadSkillFile(ctx context.Context, fileURL, destPath string, maxSize int64, ghCred gitHubCredential) error {
	parsed, err := url.Parse(fileURL)
	if err != nil {
		// The parse error quotes the URL, which may be signed, so neither
		// text includes it.
		return newSkillInstallError("invalid download URL", errors.New("invalid download URL"))
	}

	// S5: HTTPS only (except localhost)
	if parsed.Scheme != "https" {
		host := parsed.Hostname()
		if parsed.Scheme == "http" && isLocalhost(host) {
			// Allow HTTP for localhost
		} else {
			msg := fmt.Sprintf("HTTPS required for skill downloads (got %s)", parsed.Scheme)
			return newSkillInstallError(msg, errors.New(msg))
		}
	}

	client := &http.Client{
		Transport: skillDownloadTransport,
		Timeout:   downloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// S5: No cross-host redirects
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("cross-host redirect not allowed: %s → %s", via[0].URL.Host, req.URL.Host)
			}
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	sendCred := ghCred.token != "" && isGitHubHost(parsed.Hostname())
	resp, err := getSkillFile(ctx, client, fileURL, parsed.Host, ghCred.token, sendCred)
	if err != nil {
		return err
	}

	if sendCred && isRejectedCredentialStatus(resp.StatusCode) {
		// One retry, same URL, no credential (see the doc comment above).
		authStatus := resp.StatusCode
		_ = resp.Body.Close()
		resp, err = getSkillFile(ctx, client, fileURL, parsed.Host, "", false)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			// The rejected-credential error names only the file, statuses
			// and credential source, so users see it verbatim.
			rej := &gitHubCredentialRejectedError{
				File:       filepath.Base(destPath),
				Source:     ghCred.source,
				AuthStatus: authStatus,
				AnonStatus: resp.StatusCode,
			}
			return newSkillInstallError(rej.Error(), rej)
		}
		// S5: no URL in logs, and never the credential.
		slog.WarnContext(ctx, "skill download: GitHub rejected the configured credential; the download succeeded without credentials",
			"file", filepath.Base(destPath), "status", authStatus, "credential_source", ghCred.source)
	}
	// Deferred only now: resp is not reassigned after this point.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("download failed with status %d", resp.StatusCode)
		return newSkillInstallError(msg, errors.New(msg))
	}

	// S5: Enforce size limit. bodyReader records a read error so a failure
	// while receiving the body is reported as a download failure, not as a
	// local write failure (ptone/scion#5033).
	body := &bodyReader{r: resp.Body}
	limitedReader := io.LimitReader(body, maxSize+1)

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() { _ = f.Close() }()

	n, err := io.Copy(f, limitedReader)
	if body.err != nil {
		// The read error names no URL (a *url.Error is reduced to its
		// cause), but is kept out of the user text all the same.
		cause := body.err.Error()
		var ue *url.Error
		if errors.As(body.err, &ue) {
			cause = transportFailureCause(ue)
		}
		return newSkillInstallError("download failed while reading the response",
			fmt.Errorf("download from %s failed while reading the response: %s", parsed.Host, cause))
	}
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	if n > maxSize {
		_ = f.Close()
		_ = os.Remove(destPath)
		msg := fmt.Sprintf("file exceeds maximum size of %d bytes", maxSize)
		return newSkillInstallError(msg, errors.New(msg))
	}

	// S5: Do not log the URL (may contain signed tokens)
	util.Debugf("provision: downloaded skill file %s (%d bytes)", filepath.Base(destPath), n)

	return nil
}

// bodyReader wraps a download response body and records the first read
// error other than io.EOF, so a failed read can be told apart from a failed
// write in an io.Copy.
type bodyReader struct {
	r   io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF && b.err == nil {
		b.err = err
	}
	return n, err
}

// transportFailureCause returns the cause of a client.Do failure as text that
// names no URL: ue.Err's text (ue itself quotes the URL), with a malformed
// redirect Location, which net/http quotes and which may be signed too,
// replaced. A nil ue.Err gives a fixed text rather than a panic.
func transportFailureCause(ue *url.Error) string {
	if ue.Err == nil {
		return "unknown error"
	}
	cause := ue.Err.Error()
	if strings.Contains(cause, "Location header") {
		cause = "invalid redirect Location"
	}
	return cause
}

// skillDownloadTransport is the HTTP transport downloadSkillFile uses; nil
// means http.DefaultTransport. Tests replace it to serve GitHub hosts locally.
var skillDownloadTransport http.RoundTripper

// getSkillFile sends one GET for fileURL (whose host is host), with token as
// a bearer credential when withCred is set. Errors never include the URL,
// which may be signed: only host and the cause.
func getSkillFile(ctx context.Context, client *http.Client, fileURL, host, token string, withCred bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		// Not err: it can quote the URL.
		return nil, fmt.Errorf("failed to create request for %s", host)
	}
	if withCred {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		// client.Do returns a *url.Error, whose text quotes the full URL; a
		// signed URL carries a token in its query. Neither the user text nor
		// the logged error includes it: only the host and the cause.
		var ue *url.Error
		if !errors.As(err, &ue) {
			return nil, newSkillInstallError("download failed", fmt.Errorf("download from %s failed", host))
		}
		cause := transportFailureCause(ue)
		public := "download failed: " + cause
		return nil, newSkillInstallError(public, fmt.Errorf("download from %s failed: %s", host, cause))
	}
	return resp, nil
}

// isRejectedCredentialStatus reports whether status is one GitHub returns
// for a request whose credential it rejects or finds insufficient.
func isRejectedCredentialStatus(status int) bool {
	return status == http.StatusUnauthorized ||
		status == http.StatusForbidden ||
		status == http.StatusNotFound
}

// gitHubCredentialRejectedError reports a skill file download from GitHub
// that failed with the configured credential and again without one. Source
// names where the credential came from; the credential itself is never part
// of the error.
type gitHubCredentialRejectedError struct {
	File       string
	Source     string
	AuthStatus int
	AnonStatus int
}

func (e *gitHubCredentialRejectedError) Error() string {
	return fmt.Sprintf("download of %s failed: GitHub rejected %s, or it was not enough to read the file (status %d), "+
		"and a retry without credentials got status %d; the credential may be expired, revoked or rate limited, "+
		"or the repository is private and the credential cannot read it",
		e.File, e.Source, e.AuthStatus, e.AnonStatus)
}

// writeSkillFileContent writes pre-fetched content bytes directly to destPath,
// bypassing the network download. This is used when the resolver already holds
// the file bytes in memory (e.g. GitHubSkillResolver downloads each file during
// resolution for hashing) so that no second unauthenticated request is needed.
//
// Callers are responsible for pre-bounding content to a safe size. The GitHub
// resolver enforces githubMaxFileSize (10 MB) before populating ResolvedFile.Content;
// the check here is a defensive backstop for any future resolver that sets Content.
func writeSkillFileContent(content []byte, destPath string) error {
	if int64(len(content)) > defaultMaxFileSize {
		msg := fmt.Sprintf("pre-fetched content exceeds maximum size of %d bytes", defaultMaxFileSize)
		return newSkillInstallError(msg, errors.New(msg))
	}
	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() { _ = f.Close() }() // safety net for panics / early returns
	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	// Explicit close to catch flush errors (e.g. disk full). The deferred
	// close above is a no-op after an explicit close, so it is safe to leave.
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}
	util.Debugf("provision: wrote pre-fetched skill file %s (%d bytes)", filepath.Base(destPath), len(content))
	return nil
}

func isLocalhost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// writeResolutionRecord writes the resolution record to disk.
func writeResolutionRecord(path string, record *SkillResolutionRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// enumerateLocalSkills lists skills already present in the skills directory
// (from template's local skills/ dir) and returns them as resolution entries
// with source "local".
func enumerateLocalSkills(agentHome, skillsDir string) []SkillResolutionEntry {
	skillsPath := filepath.Join(agentHome, skillsDir)
	entries, err := os.ReadDir(skillsPath)
	if err != nil {
		return nil
	}

	var result []SkillResolutionEntry
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		// An Agent Plugins bundle installed in the skills dir (claude) is
		// not a skill; the harness loads it as a plugin.
		if fi, err := os.Lstat(filepath.Join(skillsPath, e.Name(), agentplugin.ManifestFile)); err == nil && fi.Mode().IsRegular() {
			continue
		}
		entry := SkillResolutionEntry{
			Name:          e.Name(),
			InstalledPath: filepath.ToSlash(filepath.Join(skillsDir, e.Name())),
			Source:        "local",
		}
		result = append(result, entry)
	}
	return result
}

// collectRequiredSkillURIs returns URIs of non-optional skill references.
func collectRequiredSkillURIs(skills []api.SkillReference) []string {
	var uris []string
	for _, s := range skills {
		if !s.Optional {
			uris = append(uris, s.URI)
		}
	}
	return uris
}

// platformSkillScheme is the scheme of the display-only entries the Hub
// seeds for its platform (binary-embedded) skills. NOTE: this must be kept
// in sync with platformSkillURIPrefix in pkg/hub/platform_skills_seed.go.
const platformSkillScheme = "scion-platform"

// resolvableSkillRefs returns skills without the optional display-only
// platform entries (scheme scion-platform). Those entries only list the
// platform skills; provisioning installs them separately, and no resolver
// serves the scheme. A required entry is kept, so it still fails closed.
// skills itself is not modified.
func resolvableSkillRefs(skills []api.SkillReference) []api.SkillReference {
	out := make([]api.SkillReference, 0, len(skills))
	for _, s := range skills {
		if s.Optional && detectScheme(s.URI) == platformSkillScheme {
			continue
		}
		out = append(out, s)
	}
	return out
}
