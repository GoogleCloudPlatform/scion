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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	gouuid "github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var tracer = otel.Tracer("scion-hub")

// msgSANotAvailableInProject is the ONE answer given for both "this service
// account does not exist" and "it exists but is not reachable from this
// project", everywhere a caller names a service account by ID.
//
// THE WHOLE POINT IS THAT THE TWO CASES ARE INDISTINGUISHABLE. Answering
// them differently — by message OR by status code — makes the endpoint an
// existence oracle: a caller who may create agents in their own project can
// enumerate other projects' service account IDs by watching which ones fail
// differently. "Does not exist" and "exists but is not yours" are one answer.
// Both branches must also use the same helper, since the response's `code`
// field distinguishes ValidationError from BadRequest just as visibly as the
// message does.
//
// The rationale was already written down at the project-settings default-SA
// path, which has collapsed the two cases since it was written; the agent
// create and PATCH paths did not follow it. This const exists so the three
// sites cannot drift back apart: a future edit to one message is now an edit
// to all three, which is the only version of this that stays true.
//
// It is deliberately vaguer than the messages around it. Once the account IS
// reachable from the caller's project, being specific discloses nothing they
// could not already read, so the "not verified" message that follows each of
// these checks stays specific on purpose.
//
// ⚠️ THE COST, WHICH IS REAL AND NOT A FREE WIN, AND WHICH LANDS ON WHOEVER
// VERIFIES THIS NEXT: the response no longer says which branch refused. Scope
// refusal and nonexistence are one answer to a caller — the point — and they are
// also one answer to a reviewer, who is not the intended audience but gets the
// same view.
//
// So a test that seeds an unreachable account and asserts "400, this message" is
// now satisfied by a fixture that never persisted the account at all. Before the
// collapse that mistake announced itself, because nonexistence answered
// differently. It is now indistinguishable from success at testing the thing.
//
// VERIFICATION THEREFORE HAS TO CONTROL THE FIXTURE, NOT READ THE RESPONSE:
//   - that the collapse holds — same request, account present-but-unreachable
//     versus absent, answers identical. requireIndistinguishable in
//     sa_existence_oracle_test.go, applied to all three sites.
//   - that the predicate still RUNS — account genuinely present in both arms,
//     only reachability varied, reachable admitted and unreachable refused.
//     Named rather than gestured at, because a reader should be able to check
//     this rather than take it. The tightest pair is PATCH's, both subtests of
//     TestBypassAgents_UpdateAgentServiceAccountChecks over one fixture:
//     "service account from another project is rejected" against "verified
//     in-project service account is still accepted". Create's pair is
//     TestAgentCreate_HubScopedSA_AssignableByCreatorAndAdmin against
//     TestAgentCreate_OtherProjectSA_StillRejected, which varies the kind of
//     scope as well as reachability — weaker, but both arms persist the account,
//     which is the property that matters here. Without an admitted arm, every
//     refusal test would still pass over a deleted predicate, for the wrong
//     reason.
//
// The neighbouring distinction — authorization refusal versus scope refusal — is
// still observable and is pinned by assertDeniedByAuthzNotByScope in
// handlers_agents_gcp_hubscope_test.go. Only scope-versus-nonexistence went
// dark, and it went dark on purpose.
//
// The wording names both causes without saying which one applies, so the
// caller knows to check registration and their own access (ptone/scion#3335).
const msgSANotAvailableInProject = "GCP service account is not available; it is not registered in this project or you are not authorized to use it"

// parseLabelFilters parses label=key=value query parameters into a map and
// validates the resulting labels against constraint rules.
func parseLabelFilters(params []string) (map[string]string, error) {
	if len(params) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(params))
	for _, lp := range params {
		k, v, ok := strings.Cut(lp, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid label filter %q: must be key=value", lp)
		}
		m[k] = v
	}
	if err := labels.Validate(m); err != nil {
		return nil, fmt.Errorf("invalid label filter: %w", err)
	}
	return m, nil
}

// maxRelationshipIDs caps the id[] relationship filter (ptone/scion#2146).
// Unbounded, it is one IN(...) bind list per caller, limited
// only by the ~1 MB HTTP header size (roughly 25k UUIDs) — that both bloats
// the query next to AuthorizedProjectIDs and costs query-planning time far
// beyond what the real use (a CLI-resolved Ancestry chain, or a lineage
// root's own small candidate set) ever needs.
const maxRelationshipIDs = 256

// applyAgentAttributeAndRelationshipFilters reads the ownerId, ancestorId,
// harnessConfig, id, and lineageRootId query params shared by listAgents and
// listProjectAgents into filter. Factored into one place so the two list
// endpoints cannot drift on these narrowing-only filters (ptone/scion#2146).
//
// Every field this sets is combined with the rest of the caller's filter
// (including any authorization predicate, such as AuthorizedProjectIDs) using
// AND — see the field docs on store.AgentFilter. None of them may be used to
// widen a result beyond what the caller was already authorized to list.
//
// Returns a non-nil error (a caller-facing message, suitable for a 400) only
// when id[] exceeds maxRelationshipIDs.
func applyAgentAttributeAndRelationshipFilters(filter *store.AgentFilter, query url.Values) error {
	filter.RequestedOwnerID = query.Get("ownerId")
	filter.AncestorID = query.Get("ancestorId")
	filter.HarnessConfig = query.Get("harnessConfig")
	if ids := query["id"]; len(ids) > 0 {
		if len(ids) > maxRelationshipIDs {
			return fmt.Errorf("id: too many values (%d); maximum is %d", len(ids), maxRelationshipIDs)
		}
		// Canonicalize (dedupe + sort) before this reaches the cursor
		// binding, like every other set-like filter field (Finding 8) —
		// otherwise the same logical request with id= params in a
		// different order mints a different cursor binding, and a cursor
		// minted under one ordering is rejected when replayed under
		// another.
		filter.IDs = canonicalizeStringSlice(append([]string{}, ids...))
	}
	filter.LineageRootID = query.Get("lineageRootId")
	return nil
}

type ListAgentsResponse struct {
	Agents     []AgentWithCapabilities `json:"agents"`
	NextCursor string                  `json:"nextCursor,omitempty"`
	TotalCount int                     `json:"totalCount"`
	// TotalCountApproximate marks TotalCount as a lower bound rather than an
	// exact count: the agent-list rule's count pass stopped at
	// authorizedListMaxCandidates candidates (see listReadableAgents).
	TotalCountApproximate bool `json:"totalCountApproximate,omitempty"`
	// Sort and Dir echo the request's sort mode. Both are omitted unless
	// the request supplied "sort": legacy-mode responses never set these.
	Sort string `json:"sort,omitempty"`
	Dir  string `json:"dir,omitempty"`
	// Complete is set only when the request supplied "fit" (sorted mode): true
	// iff the unphased candidate set had at most fit members, in which case
	// Agents is its whole readable subset. On the project user path it is
	// also false when the complete response would exceed the per-request
	// decision budget for the caller (completeBranchMaxCandidates); the
	// response is then an ordinary paged one. A pointer
	// so "fit not sent" (nil, omitted) is distinguishable from "fit sent,
	// complete: false".
	Complete *bool `json:"complete,omitempty"`
	// Stats is populated only when the request supplied "stats=1". It is
	// computed over the request filter with Phase cleared, kept
	// label/scope/projectId/broker/includeDeleted, and (project user path)
	// read-filtered the same way the page is.
	Stats        *ListAgentsStats `json:"stats,omitempty"`
	ServerTime   time.Time        `json:"serverTime"`
	Capabilities *Capabilities    `json:"_capabilities,omitempty"`
}

// ListAgentsStats is the sorted-mode "stats" response block.
type ListAgentsStats struct {
	// Total is the readable, label(k=v)-filtered count, phase NOT
	// applied. It is exact unless TotalApproximate is set. For a user
	// caller on the global endpoint it is capped at 2,000: only the first
	// authorizedListMaxCandidates candidates are read.
	Total int `json:"total"`
	// Running is the count of phase == "running" among the same population,
	// always present regardless of the request's own phase filter.
	Running int `json:"running"`
	// TotalApproximate marks Total and Running as lower bounds: the
	// global endpoint read only the first authorizedListMaxCandidates
	// candidates (see buildGlobalAgentStats).
	TotalApproximate bool `json:"totalApproximate,omitempty"`
	// Agents is exactly the counted population as [id, phase] pairs. On
	// the global endpoint it is nil, and so omitted from the response,
	// when TotalApproximate is set (a user caller with more than 2,000
	// candidates), and for an agent caller when Total exceeds 2,000. The
	// project endpoint is already bounded by the 2,000 candidate
	// ceiling, so it is never omitted there.
	//
	// A *slice, not a slice: encoding/json's omitempty on a plain slice
	// can't distinguish "intentionally empty" (Total == 0, an empty but
	// present array) from "omitted" — both have len 0. omitempty on a
	// pointer checks only nilness, which is exactly the distinction this
	// field needs.
	Agents *[][2]string `json:"agents,omitempty"`
}

type CreateAgentRequest struct {
	Name            string            `json:"name"`
	ProjectID       string            `json:"projectId"`
	RuntimeBrokerID string            `json:"runtimeBrokerId,omitempty"` // Optional: uses project's default if not specified
	Template        string            `json:"template"`
	HarnessConfig   string            `json:"harnessConfig,omitempty"` // Explicit harness config name (used during sync when template may not be on Hub)
	HarnessAuth     string            `json:"harnessAuth,omitempty"`   // Late-binding override for auth_selected_type
	Profile         string            `json:"profile,omitempty"`       // Settings profile for the runtime broker to use
	Task            string            `json:"task,omitempty"`
	Branch          string            `json:"branch,omitempty"`
	Workspace       string            `json:"workspace,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Config          *api.ScionConfig  `json:"config,omitempty"`
	Attach          bool              `json:"attach,omitempty"`        // If true, signals interactive attach mode to the broker/harness
	ProvisionOnly   bool              `json:"provisionOnly,omitempty"` // If true, provision only (write task to prompt.md) without starting
	// WorkspaceFiles is populated for non-git workspace bootstrap.
	// When present, the Hub generates signed upload URLs instead of dispatching immediately.
	WorkspaceFiles []transfer.FileInfo `json:"workspaceFiles,omitempty"`
	// GatherEnv enables the env-gather flow where the broker evaluates env
	// completeness and may return a 202 requiring the CLI to supply missing values.
	GatherEnv bool `json:"gatherEnv,omitempty"`
	// Notify subscribes the creating agent/user to status notifications for the new agent.
	Notify bool `json:"notify,omitempty"`
	// CleanupMode controls stale-existing-agent cleanup behavior during create:
	// "strict" (default) fails create if broker cleanup fails; "force" continues.
	CleanupMode string `json:"cleanupMode,omitempty"`
	// Resume signals that the caller wants to resume an existing stopped agent
	// rather than create a brand-new one. When true and a stopped agent with
	// the same name exists, the Hub recovers it instead of creating fresh.
	Resume bool `json:"resume,omitempty"`
	// ForceResume permits resuming an agent the Hub considers failed
	// (phase=error), such as one whose host crashed mid-run. Without it such an
	// agent is rejected as a duplicate. Only meaningful alongside Resume, and
	// deliberately does NOT cover phase=running: a live agent must not be
	// recreated out from under itself. The harness receives its resume flag so
	// the prior session is continued rather than restarted fresh.
	ForceResume bool `json:"forceResume,omitempty"`
	// NoAuth indicates the agent should start with zero injected credentials.
	// When true, the Hub skips secret resolution and the broker skips credential injection.
	NoAuth bool `json:"noAuth,omitempty"`
	// AgentRole specifies the requested authorization role for the agent.
	// Valid values: "none", "readonly", "baseline", "full".
	// When omitted, user requests use the configured default and agent requests
	// inherit the parent role. The project maximum caps both paths.
	AgentRole string `json:"agentRole,omitempty"`
	// MessageMode specifies the initial message mode for the agent.
	// Valid values: "none", "lineage", "branch", "project", "hub".
	// When omitted, resolved from template, parent inheritance, or "project" default.
	MessageMode string `json:"messageMode,omitempty"`
	// GCPIdentity specifies the GCP identity assignment for the agent.
	// Controls metadata server behavior and optional service account binding.
	GCPIdentity *GCPIdentityAssignment `json:"gcp_identity,omitempty"`
	// AcceptAsyncLaunch is the client's non-blocking-launch opt-in.
	// Persisted as store.Agent.LaunchAsyncOptIn because env finalize and
	// workspace finalize launch in later requests. It takes effect only when
	// hub.asyncAgentLaunch is on (see dispatchLaunching). Scheduled creates
	// set the opt-in server-side rather than from client input.
	AcceptAsyncLaunch bool `json:"acceptAsyncLaunch,omitempty"`
}

// GCPIdentityAssignment specifies GCP identity configuration for agent creation.
type GCPIdentityAssignment struct {
	MetadataMode     string `json:"metadata_mode"`                // "block", "passthrough", "assign"
	ServiceAccountID string `json:"service_account_id,omitempty"` // Required when mode is "assign"
}

type CreateAgentResponse struct {
	Agent    *store.Agent `json:"agent"`
	Warnings []string     `json:"warnings,omitempty"`
	// UploadURLs is populated during workspace bootstrap (non-git projects).
	// The CLI uploads files to these URLs, then calls finalize to trigger dispatch.
	UploadURLs []transfer.UploadURLInfo `json:"uploadUrls,omitempty"`
	// Expires indicates when the upload URLs expire.
	Expires *time.Time `json:"expires,omitempty"`
	// EnvGather is populated when the broker returns 202, indicating env
	// vars need to be gathered from the CLI before the agent can start.
	EnvGather *EnvGatherResponse `json:"envGather,omitempty"`
}

// EnvGatherResponse contains env requirements relayed from the broker.
type EnvGatherResponse struct {
	AgentID     string                   `json:"agentId"`
	Required    []string                 `json:"required"`
	HubHas      []EnvSource              `json:"hubHas"`
	BrokerHas   []string                 `json:"brokerHas"`
	Needs       []string                 `json:"needs"`
	SecretInfo  map[string]SecretKeyInfo `json:"secretInfo,omitempty"`
	HubWarnings []string                 `json:"hubWarnings,omitempty"`
}

// EnvSource tracks which scope provided an env var key.
type EnvSource struct {
	Key   string `json:"key"`
	Scope string `json:"scope"`
}

// SubmitEnvRequest is the request body for submitting gathered env vars.
type SubmitEnvRequest struct {
	Env map[string]string `json:"env"`
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listAgents(w, r)
	case http.MethodPost:
		s.createAgent(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	if !checkAgentReadScope(w, r) {
		return
	}
	// The listing performs no authorization-state writes, so one input memo
	// serves every authorization decision this request makes.
	r = r.WithContext(withAuthzInputMemo(r.Context()))

	ctx := r.Context()
	query := r.URL.Query()
	identity := GetIdentityFromContext(ctx)

	// Sorted mode validates sort and dir before either short-circuit below,
	// so an invalid value is a 400 for every caller and the short-circuit
	// echo only ever carries validated values. Without sort, nothing here
	// runs and both short-circuits return the legacy empty list.
	sorted := isSortedModeRequest(query)
	if sorted {
		perfSetEndpoint(ctx, perfEndpointAgentsGlobalSorted)
	} else {
		perfSetEndpoint(ctx, perfEndpointAgentsGlobalLegacy)
	}
	var sortParam, dirParam string
	if sorted {
		var ok bool
		if sortParam, dirParam, ok = parseSortAndDir(w, query); !ok {
			return
		}
	}

	// writeShortCircuit writes the empty list for an unauthenticated or
	// None-scope caller. In sorted mode it first validates the remaining
	// sorted parameters (fit, cursor exclusion), so an invalid fit is a 400
	// for these callers exactly as for a caller with scope.
	writeShortCircuit := func() {
		p := agentListParams{view: legacyAgentListView(query)}
		if sorted {
			var ok bool
			if p, ok = parseAgentListParamsAfterSortDir(w, query, agentListLimit(query), sortParam, dirParam); !ok {
				return
			}
		}
		writeAgentList(w, p.view, sortedShortCircuitResponse(p))
	}

	// RS2: Unauthenticated callers get an empty list immediately.
	if identity == nil {
		writeShortCircuit()
		return
	}

	// RS2: Resolve authorization scope FIRST — before building filter or cursor
	// binding. This is the single authoritative scope decision for the request.
	scopeDone := perfPhaseStart(ctx, perfPhaseListScopeAuthz)
	scopeResult, err := s.authzService.ResolveListScopes(ctx, identity, "agent.list")
	scopeDone()
	if err != nil {
		slog.WarnContext(ctx, "listAgents: authorization scope resolution failed (fail-closed)",
			"error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"unable to resolve authorization", nil)
		return
	}

	if scopeResult.Scopes.IsNone() {
		// Legitimate no-authority result. Return empty list without querying
		// the store. No broad resource query is issued for None.
		writeShortCircuit()
		return
	}

	// RS2: Slug-to-ID resolution for projectId filter.
	// If projectId looks like a slug (not a UUID), resolve it to a project ID.
	// The resolution must not become a cross-scope existence oracle: nonexistent
	// and unauthorized slugs are externally indistinguishable — both silently
	// produce an unmatched filter that returns zero results.
	projectID := query.Get("projectId")
	if projectID != "" && gouuid.Validate(projectID) != nil {
		// Looks like a slug. Resolve within the authorized scope to prevent
		// the slug lookup from disclosing projects outside the caller's authority.
		project, lookupErr := s.store.GetProjectBySlug(ctx, projectID)
		if lookupErr != nil || project == nil {
			// Slug not found — indistinguishable from unauthorized.
			projectID = ""
		} else if !scopeResult.Scopes.Contains(project.ID) {
			// Project exists but is outside the caller's authorized scope.
			// Treat identically to "not found" to prevent an existence oracle.
			projectID = ""
		} else {
			projectID = project.ID
		}
		// When slug resolution fails (not found or unauthorized), leave
		// projectID empty. The store filter will return all authorized agents
		// (no project restriction), which is correct: it prevents the caller
		// from distinguishing "slug doesn't exist" from "slug exists but I
		// can't see it".
		if projectID == "" {
			// Force empty result: filter to an impossible project ID so the
			// caller cannot infer slug existence from result count changes.
			projectID = "00000000-0000-0000-0000-000000000000"
		}
	}

	filter := store.AgentFilter{
		ProjectID:       projectID,
		RuntimeBrokerID: query.Get("runtimeBrokerId"),
		Phase:           query.Get("phase"),
		IncludeDeleted:  query.Get("includeDeleted") == "true",
	}
	if err := applyAgentAttributeAndRelationshipFilters(&filter, query); err != nil {
		BadRequest(w, err.Error())
		return
	}

	if labelParams := query["label"]; len(labelParams) > 0 {
		parsed, err := parseLabelFilters(labelParams)
		if err != nil {
			BadRequest(w, err.Error())
			return
		}
		filter.Labels = parsed
	}

	// RS2: Push the authorized project predicate into the store filter.
	// This INTERSECTS with all other caller filters — it never replaces
	// or unions with them. For All, AuthorizedProjectIDs remains nil, but
	// project-scoped constraint exclusions still apply.
	if !scopeResult.Scopes.IsAll() {
		filter.AuthorizedProjectIDs = canonicalizeStringSlice(scopeResult.Scopes.ProjectIDs())
	}
	if len(scopeResult.ExcludedProjectIDs) > 0 {
		filter.ExcludedProjectIDs = canonicalizeStringSlice(append([]string{}, scopeResult.ExcludedProjectIDs...))
	}

	classifyDone := perfPhaseStart(ctx, perfPhaseListScopeAuthz)
	classification, err := s.resolveProjectListClassification(
		ctx, identity, query.Get("scope"), query.Get("mine") == "true", scopeResult, "listAgents")
	classifyDone()
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"unable to resolve authorization", nil)
		return
	}
	filter.MemberOrOwnerProjectIDs = classification.OwnedProjectIDs
	filter.MemberProjectIDs = classification.SharedProjectIDs
	filter.ExcludedProjectIDs = append(filter.ExcludedProjectIDs, classification.ExcludedOwnedProjectIDs...)

	limit := agentListLimit(query)

	// Finding 8: Canonicalize all set-like filter fields before hashing.
	if len(filter.ExcludedProjectIDs) > 0 {
		filter.ExcludedProjectIDs = canonicalizeStringSlice(filter.ExcludedProjectIDs)
	}
	if len(filter.MemberOrOwnerProjectIDs) > 1 {
		filter.MemberOrOwnerProjectIDs = canonicalizeStringSlice(filter.MemberOrOwnerProjectIDs)
	}
	if len(filter.MemberProjectIDs) > 1 {
		filter.MemberProjectIDs = canonicalizeStringSlice(filter.MemberProjectIDs)
	}

	if sorted {
		// Sorted mode: the SQL scope predicate baked into filter above
		// (AuthorizedProjectIDs, classification, etc.) narrows the
		// candidates, and listAgentsSorted applies the same per-agent read
		// rule as the legacy branch below. Dispatched after every gate and
		// filter-building step above, so caps/messageability for returned
		// rows run through the same identity and filter the legacy branch
		// uses.
		// sort and dir were already validated above; only the remaining
		// parameters are parsed here, at the same point in the request as
		// before, so the order of 400s is unchanged.
		params, ok := parseAgentListParamsAfterSortDir(w, query, limit, sortParam, dirParam)
		if !ok {
			return
		}
		s.listAgentsSorted(w, r, filter, params, identity)
		return
	}

	// RS2: Cursor binding computed AFTER authorization scope and all caller
	// filters are set. The binding includes the authorization predicate and
	// principal/credential context so a cursor minted before an authority,
	// group, constraint, or credential-scope change cannot be replayed.
	cursorBinding := scopedCursorBinding("agents", filter, identity)
	cursor := query.Get("cursor")
	if cursor != "" {
		if err := validateAuthorizedListCursor(cursor, cursorBinding); err != nil {
			BadRequest(w, err.Error())
			return
		}
	}

	// Agent-list rule (ptone/scion#3346): for a user caller, an agent appears
	// in an agent list, its pages and its totalCount only if the caller can
	// read that agent. listAgents and listProjectAgents both apply it, so the
	// two endpoints return the same set for the same project. The SQL scope
	// predicate above narrows the candidates; listAgentsLegacyPage then
	// keeps only the readable ones.
	result, err := s.listAgentsLegacyPage(ctx, identity, filter, cursor, cursorBinding, limit)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	agents, scopeCap := s.buildGlobalAgentPage(ctx, identity, result.Items)

	writeAgentList(w, legacyAgentListView(query), ListAgentsResponse{
		Agents:                agents,
		NextCursor:            result.NextCursor,
		TotalCount:            result.TotalCount,
		TotalCountApproximate: result.TotalCountApproximate,
		ServerTime:            time.Now().UTC(),
		Capabilities:          scopeCap,
	})
}

// addAgentCreateIfAnyProjectAllows adds "create" to the agents list's
// scope-level capabilities when the caller may create an agent in at least one
// project they can reach.
//
// Agent creation is inherently project-scoped: `POST /api/v1/agents` decides
// against the target project via authorizeAgentCreate, and `agent.create` lives
// in project-scoped roles rather than in hub-member or hub-admin. A hub-scope
// capability check therefore answers "no" for every principal except a
// super-admin (who passes via the Decide step-1 bypass), which hid the
// "New Agent" button from the page whose subject is agents.
//
// This reports what the enforcement point would decide, on behalf of a page
// that has no project in hand. It grants nothing: creation is still authorized
// per project at the API, so a caller who gets "create" here and then targets a
// project they may not create in is still refused.
//
// Skipped entirely when the caller already has create, which is both the
// super-admin case and any future role that carries it at hub scope.
//
// Uses a batch approach (3 queries total, independent of project count) that
// also correctly handles group-derived permissions:
//  1. Resolve the user's effective groups.
//  2. Fetch all role bindings for the user and their groups in one query.
//  3. Batch-load the corresponding role definitions and check for agent.create.
func (s *Server) addAgentCreateIfAnyProjectAllows(
	ctx context.Context,
	identity Identity,
	caps *Capabilities,
) {
	if caps == nil {
		return
	}
	for _, a := range caps.Actions {
		if a == string(ActionCreate) {
			return
		}
	}

	// Only human callers. Agents create agents through the delegation path,
	// which has its own ceiling checks, and this page is not their surface.
	user, isUser := identity.(UserIdentity)
	if !isUser {
		return
	}

	// Resolve the user's direct and group-derived principals.
	principals := []store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: user.ID()}}
	groups, err := s.store.GetEffectiveGroups(ctx, user.ID())
	if err != nil {
		// GetEffectiveGroups may return ErrNotFound for users with no groups.
		// Only warn on unexpected errors.
		slog.WarnContext(ctx, "listAgents: could not resolve groups for agent-create capability",
			"error", err)
		// Fall through with just the direct user principal.
	}
	for _, g := range groups {
		principals = append(principals, store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: g})
	}

	// Fetch all role bindings for these principals in a single query.
	bindings, err := s.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		// Advisory only — the button stays hidden and the per-project create
		// path still works. Not worth failing the list response.
		slog.WarnContext(ctx, "listAgents: could not resolve role bindings for agent-create capability",
			"error", err)
		return
	}

	// Filter to active project-scoped bindings and collect unique role definition IDs.
	now := time.Now()
	roleDefIDsMap := make(map[string]struct{})
	hasActiveProjectBinding := false
	for _, rb := range bindings {
		if rb.ScopeType != store.RoleScopeProject || rb.ScopeID == "" {
			continue
		}
		if !isBindingActive(rb, now) {
			continue
		}
		hasActiveProjectBinding = true
		roleDefIDsMap[rb.RoleDefinitionID] = struct{}{}
	}

	if !hasActiveProjectBinding {
		return
	}

	// Batch-load the role definitions.
	roleDefIDs := make([]string, 0, len(roleDefIDsMap))
	for id := range roleDefIDsMap {
		roleDefIDs = append(roleDefIDs, id)
	}
	roleDefs, err := s.store.GetRoleDefinitionsByIDs(ctx, roleDefIDs)
	if err != nil {
		slog.WarnContext(ctx, "listAgents: could not resolve role definitions for agent-create capability",
			"error", err)
		return
	}

	// Check if any of the active roles grant the "agent.create" permission.
	targetPermission, err := resolveResourcePermission("agent", ActionCreate)
	if err != nil {
		slog.WarnContext(ctx, "listAgents: agent-create permission is not resolvable", "error", err)
		return
	}
	for _, rd := range roleDefs {
		if rd == nil {
			continue
		}
		for _, perm := range rd.Permissions {
			if perm == targetPermission {
				caps.Actions = append(caps.Actions, string(ActionCreate))
				return
			}
		}
	}
}

// syncToGCSForWorkspaceUpload is gcp.SyncToGCS by default; a test replaces
// it with a recording stub so the upload-guard wiring below can be verified
// without a real GCS bucket or network access, and so a test can tell "the
// guard refused this and the upload never ran" apart from "the upload itself
// failed" -- the two are indistinguishable from the HTTP response alone,
// since both are logged as warnings rather than surfaced as request errors.
var syncToGCSForWorkspaceUpload = gcp.SyncToGCS

// resolveHubManagedWorkspaceForUpload validates workspace -- normally
// agent.AppliedConfig.Workspace, which starts as the caller-supplied
// req.Workspace (buildAppliedConfig) and is only ever replaced by the
// resolved hub-managed project path when the caller left it empty -- against
// projectSlug's own managed path before it is used as a GCS upload source. A
// non-empty, caller-supplied workspace otherwise reaches the upload
// unresolved and unvalidated. Returns the resolved, symlink-free path to
// upload, or an error naming why workspace was refused.
func (s *Server) resolveHubManagedWorkspaceForUpload(workspace, projectSlug string) (string, error) {
	workspaceRoot, err := s.hubManagedProjectPath(projectSlug)
	if err != nil {
		return "", err
	}
	return runtime.ValidateWorkspaceSource(workspace, workspaceRoot)
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateAgentRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if req.ProjectID == "" {
		ValidationError(w, "projectId is required", nil)
		return
	}

	// Resolve project slug to UUID if needed (mirrors listAgents pattern).
	if gouuid.Validate(req.ProjectID) != nil {
		project, err := s.store.GetProjectBySlug(ctx, req.ProjectID)
		if err != nil {
			if err == store.ErrNotFound {
				NotFound(w, "Project")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		req.ProjectID = project.ID
	}

	if req.CleanupMode != "" && req.CleanupMode != "strict" && req.CleanupMode != "force" {
		ValidationError(w, "cleanupMode must be 'strict' or 'force'", nil)
		return
	}

	// Validate GCP identity assignment structure (field-level; SA resolution happens in createAgentInProject)
	if req.GCPIdentity != nil {
		switch req.GCPIdentity.MetadataMode {
		case store.GCPMetadataModeBlock, store.GCPMetadataModePassthrough:
			if req.GCPIdentity.ServiceAccountID != "" {
				ValidationError(w, "service_account_id must be empty when metadata_mode is '"+req.GCPIdentity.MetadataMode+"'", nil)
				return
			}
		case store.GCPMetadataModeAssign:
			if req.GCPIdentity.ServiceAccountID == "" {
				ValidationError(w, "service_account_id is required when metadata_mode is 'assign'", nil)
				return
			}
		default:
			ValidationError(w, "metadata_mode must be 'block', 'passthrough', or 'assign'", nil)
			return
		}
	}

	if req.AgentRole != "" && !ValidAgentRole(AgentRole(req.AgentRole)) {
		ValidationError(w, fmt.Sprintf("invalid agentRole %q: must be one of none, readonly, baseline, full", req.AgentRole), nil)
		return
	}

	if err := labels.Validate(req.Labels); err != nil {
		ValidationError(w, "Invalid labels: "+err.Error(), nil)
		return
	}

	// Authorization for every caller kind. The branch this replaced had no else,
	// so a caller that was neither agent nor user fell through ungated (#591).
	// Do not wrap this in an identity-kind test.
	if !s.authorizeAgentCreate(w, r, req.ProjectID) {
		return
	}

	// Attribution only (CreatedBy, creator name, ancestry, --notify subscriber).
	// Authorization is done above; nothing below this point is a gate.
	var createdBy string
	var creatorName string
	var ancestry []string
	var notifySubscriberType, notifySubscriberID string // For --notify subscription
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		createdBy = agentIdent.ID()
		// Resolve human-readable creator name and ancestry from the calling agent
		if creatorAgent, err := s.store.GetAgent(ctx, agentIdent.ID()); err == nil {
			creatorName = creatorAgent.Name
			notifySubscriberType = store.SubscriberTypeAgent
			notifySubscriberID = creatorAgent.Slug
			// Build ancestry: creator's ancestry + creator's ID
			ancestry = append(ancestry, creatorAgent.Ancestry...)
			ancestry = append(ancestry, creatorAgent.ID)
		}
	} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		createdBy = userIdent.ID()
		creatorName = userIdent.Email()
		notifySubscriberType = store.SubscriberTypeUser
		notifySubscriberID = userIdent.ID()
		// User-created agents: ancestry is [userID]
		ancestry = []string{userIdent.ID()}
	}

	s.createAgentInProject(w, r, req, req.ProjectID, createdBy, creatorName, ancestry, notifySubscriberType, notifySubscriberID)
}

// validateGCPIdentityRequest performs the field-level validation of a requested
// GCP identity assignment: the mode must be one of the three known modes, and
// the service account ID must be present exactly for "assign". It writes a
// ValidationError and returns false when the request is malformed.
//
// It lives here, on the path both create routes funnel through, rather than in
// each route. It used to be in createAgent only, so POST
// /api/v1/projects/{id}/agents — the route the CLI actually uses — accepted an
// unrecognised metadata_mode outright and accepted a service_account_id
// alongside "block" (#591, design §3.3). Duplicating the block per route would
// reproduce the drift that caused that; one chokepoint cannot drift, and a
// future third caller of createAgentInProject is covered by construction.
//
// It rejects rather than normalises. An unknown mode is a malformed request and
// saying so is the signal; coercing it to "block" would hide exactly the
// cross-layer disagreement documented in design §8.4. (The broker and sidecar
// correctly fall back to "block" — they only know the value is unusable,
// whereas here we know the caller's intent is malformed.)
func validateGCPIdentityRequest(w http.ResponseWriter, cfg *GCPIdentityAssignment) bool {
	if cfg == nil {
		return true
	}
	switch cfg.MetadataMode {
	case store.GCPMetadataModeBlock, store.GCPMetadataModePassthrough:
		if cfg.ServiceAccountID != "" {
			ValidationError(w, "service_account_id must be empty when metadata_mode is '"+cfg.MetadataMode+"'", nil)
			return false
		}
	case store.GCPMetadataModeAssign:
		if cfg.ServiceAccountID == "" {
			ValidationError(w, "service_account_id is required when metadata_mode is 'assign'", nil)
			return false
		}
	default:
		// Covers the empty mode, i.e. `"gcp_identity": {}`, which previously
		// fell through the config-building switch below and silently dropped
		// the project's configured default mode.
		ValidationError(w, "metadata_mode must be 'block', 'passthrough', or 'assign'", nil)
		return false
	}
	return true
}

// checkAndReserveQuota performs a single named quota check-and-reserve via
// s.quotaService, translating the result into an HTTP response. It reports
// whether the caller may proceed (true) or has already written an error
// response and must return (false).
//
// Factored out because createAgentInProject chains two independent limits
// (the per-broker agent ceiling, then the per-project agent cap) and both
// need identical ErrQuotaExceeded / ErrQuotaLockContention / other-error
// mapping — inlining it twice would let the two drift apart silently.
func (s *Server) checkAndReserveQuota(ctx context.Context, w http.ResponseWriter, limitName, subjectID, scopeType, scopeID, resourceID string) bool {
	ok, _ := s.reserveQuotaHTTP(ctx, w, limitName, subjectID, scopeType, scopeID, resourceID)
	return ok
}

// quotaExceededMessage is the error message for a request refused because
// limitName is at its cap. Every path that refuses a request over a quota
// uses it, so callers see the same text regardless of the path.
func quotaExceededMessage(limitName string) string {
	return "quota exceeded: " + limitName
}

// reserveQuotaHTTP is checkAndReserveQuota that also reports whether this
// call created a new reservation (see QuotaService.Reserve). created is
// only meaningful when ok is true.
func (s *Server) reserveQuotaHTTP(ctx context.Context, w http.ResponseWriter, limitName, subjectID, scopeType, scopeID, resourceID string) (ok, created bool) {
	if s.quotaService == nil {
		return true, false
	}
	created, err := s.quotaService.Reserve(ctx, limitName, subjectID, scopeType, scopeID, resourceID)
	if err == nil {
		return true, created
	}
	writeQuotaReserveError(w, limitName, err)
	return false, false
}

// writeQuotaReserveError writes the response for a failed quota reservation
// of limitName: 429 at the limit or on lock contention, 500 otherwise.
func writeQuotaReserveError(w http.ResponseWriter, limitName string, err error) {
	switch {
	case errors.Is(err, store.ErrQuotaExceeded):
		writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded,
			quotaExceededMessage(limitName), nil)
	case errors.Is(err, ErrQuotaLockContention):
		writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded,
			"quota check temporarily unavailable, please retry", nil)
	default:
		writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "quota check failed", nil)
	}
}

// releaseAgentQuotas releases resourceID's create-time quota reservations:
// the per-broker agent ceiling (#1303), when a runtime broker was assigned,
// and the per-project agent limit. Both are released explicitly rather than
// relying on Release being resourceID-scoped and clearing every reservation
// tied to resourceID regardless of limitName — that behavior is an
// implementation detail of the store today, not a contract. Release is a
// no-op for a limit that was never reserved, so this is safe to call even
// when only one of the two reservations was ever made (ptone/scion#1986).
//
// The release runs on a context detached from ctx's cancellation, under its
// own quotaReleaseTimeout (ptone/scion#2087). Every caller is a cleanup or
// rollback step: on a create-failure path ctx is often the request's context,
// which is already canceled by the time the failure is handled (the client
// disconnected, or a dispatch hit its control-channel timeout). Releasing on
// that ctx would fail with "context canceled" and strand the reservation —
// and when store.DeleteAgent already succeeded there is no agent row left
// for the normal delete path to reclaim it from.
func (s *Server) releaseAgentQuotas(ctx context.Context, resourceID, runtimeBrokerID string) {
	if s.quotaService == nil {
		return
	}
	ctx, cancel := detachedCleanupContext(ctx, quotaReleaseTimeout)
	defer cancel()
	if runtimeBrokerID != "" {
		s.quotaService.Release(ctx, store.LimitMaxAgentsPerBroker, resourceID)
	}
	s.quotaService.Release(ctx, "max_agents_per_project", resourceID)
}

// Budgets for best-effort cleanup that must outlive the request that
// triggered it (ptone/scion#2087). Each step gets its own budget on a fresh
// detached context, so a slow runtime delete cannot starve the store delete
// or the quota release that follow it.
const (
	// quotaReleaseTimeout bounds releaseAgentQuotas (two store writes).
	quotaReleaseTimeout = 5 * time.Second

	// createCleanupStoreTimeout bounds the store.DeleteAgent of a failed
	// create's row.
	createCleanupStoreTimeout = 5 * time.Second

	// createCleanupRuntimeTimeout bounds the runtime-side delete of a failed
	// create (DispatchAgentDelete / managedAgentDelete). It is deliberately
	// longer than dispatchDeleteTimeout (15s): a cross-node delete is routed
	// to a deferred dispatch whose rolling wait is dispatchDeleteTimeout, and
	// this budget must leave that wait intact rather than cut it short.
	createCleanupRuntimeTimeout = 30 * time.Second
)

// detachedCleanupContext returns a context that keeps ctx's values but not
// its cancellation or deadline, bounded instead by timeout. It is for cleanup
// that must run to completion even when the request that triggered it has
// been canceled.
func detachedCleanupContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// createRollback describes a committed create to roll back with
// cleanupFailedCreate.
type createRollback struct {
	Agent           *store.Agent
	RuntimeBrokerID string
	// CreateAuditID is the ID of the create's audit record; empty when
	// unknown.
	CreateAuditID string
	// Stage is the create stage that failed (one of the createStage*
	// values); it is recorded in the compensation audit record.
	Stage string
	// Cause is the failure that triggered the rollback.
	Cause error
	// RevokeCredentials must be false when the failure surfaced as an error
	// from a dispatcher call that minted the credential: those calls
	// (DispatchAgentCreateWithGather and friends) revoke on their own error
	// return, and revoking again here would be a redundant second revoke.
	// It is true for a failure the dispatcher reported as success (missing
	// env vars), where no such revoke fired.
	RevokeCredentials bool
	// DeleteRuntime deletes the agent's runtime-side resources; nil when
	// the create has none.
	DeleteRuntime func(context.Context) error
}

// cleanupFailedCreate is the single best-effort cleanup for a create that
// failed after commitAgentCreate wrote its rows and its quota reservations
// were taken (ptone/scion#2087, ptone/scion#1986). In order it:
//
//  1. revokes the agent's credentials, when rb.RevokeCredentials is set;
//  2. deletes the agent's runtime-side resources via rb.DeleteRuntime, when
//     non-nil (DispatchAgentDelete for a broker agent, managedAgentDelete for
//     a managed one);
//  3. compensates the committed create (compensateAgentCreate): one
//     transaction that deletes the agent row, deactivates its delegation
//     edge with cause create_compensation and writes an
//     agent_create_dispatch_failed audit record naming rb.CreateAuditID and
//     rb.Stage; and
//  4. releases its quota reservations.
//
// Every step runs on a context detached from ctx with its own short budget,
// so a canceled request cannot skip any of them. The cleanup runs
// synchronously, before the caller writes its error response, so it may delay
// that response by up to ~45s in the worst case (5s revoke + 30s runtime
// delete + 5s compensation + 5s release), plus about 20s for the fallback
// below (up to three row-delete attempts with backoff, then marking the row
// failed or deactivating its edges).
// That can exceed a client's own timeout (the CLI's is 30s), in which case
// the client sees a timeout rather than the create error — a deliberate
// trade for not leaking the agent's row, runtime resources and reservations.
//
// Failures of steps 1, 2 and 4 are logged and otherwise ignored, with one
// exception: when the broker refuses step 2 because it holds a different
// run of the agent (ptone/scion#3080) and refuseDeleteRunMismatch refuses,
// steps 3 and 4 are skipped, and the row is left in phase error with its
// quotas held (markCreateCleanupRefused). When the
// compensation transaction fails, the failure is logged at ERROR with a
// correlation ID and the compensation's op ID, and a fallback runs: a
// standalone best-effort deactivation of the agent's edges (same cause and
// op ID), then a delete of the agent row on its own. The correlation ID is
// returned so the caller can report it (writeCreateFailure). The return
// value is "" when the compensation committed.
func (s *Server) cleanupFailedCreate(ctx context.Context, rb createRollback) (compensationFailureCorrelationID string) {
	agent := rb.Agent
	if agent == nil {
		// Nothing identifies the records to roll back. Report it like a
		// failed compensation, so writeCreateFailure answers 500 with the
		// correlation ID.
		compensationFailureCorrelationID = compensationFailureID(ctx)
		logCompensationFailure(ctx, "", compensationFailureCorrelationID, "",
			fmt.Errorf("%w: no agent in create rollback (stage %q)", errAgentCreateWriteInvalid, rb.Stage))
		return compensationFailureCorrelationID
	}
	if rb.RevokeCredentials {
		// Detaches from ctx and applies its own timeout internally.
		revokeAgentCredentialsBestEffort(ctx, s.store, agent.ID, agentCredentialRevokeReasonCreateFailed)
	}
	// Each step's detached context is scoped to its own closure so its
	// deferred cancel fires when that step ends, not when the whole cleanup
	// does.
	if rb.DeleteRuntime != nil {
		func() {
			sctx, cancel := detachedCleanupContext(ctx, createCleanupStoreTimeout)
			defer cancel()
			if _, err := s.recordRunIntent(sctx, agent, store.RunIntentStopped); err != nil {
				s.agentLifecycleLog.Warn("Create-failure cleanup: run intent write failed", "agent_id", agent.ID, "error", err)
			}
		}()
		var refused *DeleteRunMismatchError
		func() {
			rctx, cancel := detachedCleanupContext(ctx, createCleanupRuntimeTimeout)
			defer cancel()
			if err := rb.DeleteRuntime(rctx); err != nil {
				s.agentLifecycleLog.Warn("Create-failure cleanup: runtime delete failed", "agent_id", agent.ID, "error", err)
				errors.As(err, &refused)
			}
		}()
		// The broker holds a different run of the agent than the row
		// records and deleted nothing (ptone/scion#3080): removing the row
		// would leave that run with no row. The row is kept in phase error
		// and its quotas stay held (ptone's ruling on ptone/scion#2550 P5
		// Q1); deleting the agent later goes through the delete engine.
		// The cleanup is best-effort, and whether that overrides the
		// refusal is decided in one place, refuseDeleteRunMismatch.
		if refused != nil && refuseDeleteRunMismatch(false, true) {
			s.agentLifecycleLog.Warn("Create-failure cleanup: broker holds a different run than the hub recorded; keeping the agent row",
				"agent_id", agent.ID, "hub_run_id", refused.RequestedRunID, "broker_run_id", refused.CurrentRunID, "stage", rb.Stage)
			s.markCreateCleanupRefused(ctx, agent.ID, refused)
			return ""
		}
	}
	func() {
		sctx, cancel := detachedCleanupContext(ctx, createCleanupStoreTimeout)
		defer cancel()
		opID := api.NewUUID()
		err := s.compensateAgentCreate(sctx, createCompensation{
			Agent:           agent,
			OriginalAuditID: rb.CreateAuditID,
			OpID:            opID,
			Stage:           rb.Stage,
			Cause:           rb.Cause,
		})
		if err == nil {
			return
		}
		compensationFailureCorrelationID = compensationFailureID(ctx)
		logCompensationFailure(ctx, agent.ID, compensationFailureCorrelationID, opID, err)
		// Fallback, sharing one store budget. First remove the row so a
		// failed compensation does not also leave the agent in place. Only
		// once the row is gone, deactivate its edges on their own (this
		// succeeds when the transaction failed on another write, for example
		// the audit insert). When the delete fails, the row keeps its active
		// edge, so it stays held to its recorded ceiling. When the
		// deactivation fails, the edge stays active with a deleted delegate,
		// which nothing reads as authority (the walk, the provenance lookup
		// and the mint all need the agent row).
		// The row delete is retried a few times; if it still fails, the row
		// is left visibly failed rather than in phase created with no
		// message.
		if derr := s.deleteFailedCreateRow(ctx, agent.ID); derr != nil {
			s.agentLifecycleLog.Warn("Create-failure cleanup: agent row delete failed", "agent_id", agent.ID, "error", derr)
			mctx, mcancel := detachedCleanupContext(ctx, createCleanupStoreTimeout)
			defer mcancel()
			if err := s.store.UpdateAgentStatus(mctx, agent.ID, store.AgentStatusUpdate{
				Phase:   string(state.PhaseError),
				Message: createRowRemoveFailedMessage,
			}); err != nil {
				s.agentLifecycleLog.Warn("Create-failure cleanup: marking the row failed also failed", "agent_id", agent.ID, "error", err)
			}
			return
		}
		fctx, fcancel := detachedCleanupContext(ctx, createCleanupStoreTimeout)
		defer fcancel()
		now := time.Now()
		if _, derr := s.store.DeactivateDelegationEdgesForDelegate(fctx, store.DelegationPrincipalAgent, agent.ID, store.Deactivation{
			Cause: store.EdgeDeactivationCreateCompensation,
			At:    &now,
			OpID:  opID,
		}); derr != nil {
			s.agentLifecycleLog.Warn("Create-failure cleanup: edge deactivation failed", "agent_id", agent.ID, "op_id", opID, "error", derr)
		}
	}()
	// Detaches from ctx and applies its own timeout internally.
	s.releaseAgentQuotas(ctx, agent.ID, rb.RuntimeBrokerID)
	return compensationFailureCorrelationID
}

// markCreateCleanupRefused leaves a failed create's row, whose runtime
// delete the broker refused because it holds another run
// (ptone/scion#3080), in phase error with the refusal's message.
//
// The write is guarded on the run the refused delete named
// (IfRunID: refused.RequestedRunID, ptone/scion#2550): when the row has
// moved on since (to the broker's run, or any newer one), it records a live
// run, which must not be marked failed, so nothing is written. Failures
// are logged.
func (s *Server) markCreateCleanupRefused(ctx context.Context, agentID string, refused *DeleteRunMismatchError) {
	mctx, cancel := detachedCleanupContext(ctx, createCleanupStoreTimeout)
	defer cancel()
	err := s.store.UpdateAgentStatus(mctx, agentID, store.AgentStatusUpdate{
		Phase:   string(state.PhaseError),
		Message: createCleanupRefusedMessage(refused),
		IfRunID: refused.RequestedRunID,
	})
	switch {
	case errors.Is(err, store.ErrRunChanged):
		current := ""
		if a, gerr := s.store.GetAgent(mctx, agentID); gerr == nil {
			current = a.RunID
		}
		s.agentLifecycleLog.Warn("Create-failure cleanup: the agent moved to another run after the refused delete; not marking it failed",
			"agent_id", agentID, "hub_run_id", refused.RequestedRunID, "broker_run_id", refused.CurrentRunID, "current_run_id", current)
	case err != nil:
		s.agentLifecycleLog.Warn("Create-failure cleanup: marking the kept row failed also failed", "agent_id", agentID, "error", err)
	}
}

// createCleanupRefusedMessage is the kept row's message after a refused
// create-failure cleanup.
func createCleanupRefusedMessage(refused *DeleteRunMismatchError) string {
	return "Create failed and its cleanup was refused: " + deleteRunMismatchMessage(refused) + ". Delete the agent to retry."
}

// deleteFailedCreateRow removes a failed create's agent row, trying
// createCleanupDeleteAttempts times with a growing backoff, each attempt on
// its own detached context. A row already gone counts as removed.
func (s *Server) deleteFailedCreateRow(ctx context.Context, agentID string) error {
	var err error
	for attempt := 0; attempt < createCleanupDeleteAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * createCleanupDeleteBackoff)
		}
		func() {
			sctx, cancel := detachedCleanupContext(ctx, createCleanupStoreTimeout)
			defer cancel()
			err = s.store.DeleteAgent(sctx, agentID)
		}()
		if err == nil || errors.Is(err, store.ErrNotFound) {
			return nil
		}
	}
	return err
}

// Create-failure cleanup: when the compensation fails, the fallback row
// delete is tried createCleanupDeleteAttempts times, backing off
// createCleanupDeleteBackoff more each time; a row that could not be removed
// is left in phase error with createRowRemoveFailedMessage.
const (
	createCleanupDeleteAttempts  = 3
	createCleanupDeleteBackoff   = 200 * time.Millisecond
	createRowRemoveFailedMessage = "Create failed and the agent record could not be removed; delete the agent to retry."
)

// writeCreateFailure writes a create-failure response. When the create's
// compensation failed (correlationID != ""), it writes a 500 that carries the
// correlation ID, because the create's records may be left behind;
// otherwise it writes the failure's own response.
//
// The 500 takes precedence over every original response, including a 409
// delete_in_progress: when the rollback is incomplete, the caller needs the
// correlation ID to report the leftover records.
func writeCreateFailure(w http.ResponseWriter, correlationID string, writeOriginal func()) {
	if correlationID == "" {
		writeOriginal()
		return
	}
	writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
		"The agent could not be created, and rolling back its records did not complete; report the correlation ID to an administrator",
		map[string]interface{}{"correlation_id": correlationID})
}

// dispatchDeleteFailedCreate returns cleanupFailedCreate's deleteRuntime step
// for a broker-dispatched create: remove the agent's provisioned files and
// branch on the broker so orphaned local state doesn't trigger spurious
// sync-registration attempts.
//
// The delete names the run the agent's row records when the cleanup runs,
// and its previous runs (failedCreateDeleteTarget), not the caller's copy:
// a create handed to another node mints its own run there, so the copy can
// name a run that never reached the broker (ptone/scion#2550).
func dispatchDeleteFailedCreate(st store.Store, dispatcher AgentDispatcher, agent *store.Agent) func(context.Context) error {
	return func(ctx context.Context) error {
		return dispatcher.DispatchAgentDelete(ctx, failedCreateDeleteTarget(ctx, st, agent), true, true, false, time.Time{})
	}
}

// failedCreateDeleteTarget returns the agent a failed create's cleanup
// deletes: a copy of agent with the run ID and previous runs its row records
// now. The row is the failed create's own, still in place (the compensation
// that removes it runs after the delete), so its run is the one the create
// left there: this node's minted run, the owning node's for a cross-node
// create, or the broker's after a settle. The delete sends that run and is
// itself run-scoped, so it never removes a newer run of the same name. When
// the row cannot be read (gone, or a store error), agent is used as is, as
// before.
func failedCreateDeleteTarget(ctx context.Context, st store.Store, agent *store.Agent) *store.Agent {
	if st == nil || agent.ID == "" {
		return agent
	}
	row, err := st.GetAgent(ctx, agent.ID)
	if err != nil {
		return agent
	}
	target := *agent
	target.RunID = row.RunID
	target.PreviousRunIDs = append([]string(nil), row.PreviousRunIDs...)
	return &target
}

// errInvalidDisplayName is returned by commitAgentCreate when slug
// fails api.ValidateDisplayName. It is deliberately distinct from
// store.ErrInvalidInput: the transaction that follows can also fail with
// store.ErrInvalidInput for unrelated reasons (e.g. a foreign-key violation
// if the project row disappears under a concurrent delete), and that error's
// text comes from the store layer, not from validating the display name --
// it must not be reported to the caller as if it were one.
var errInvalidDisplayName = errors.New("invalid display name")

// applyCreateEffectCeiling computes the source credential's frozen ceiling
// and provenance for an interactive agent create, and caps role to what the
// ceiling covers (childRoleWithinCeiling). roleExplicit is true when the
// request named a role. On a denial it writes the response (403 with
// details.denied_by="delegation_ceiling", or 503 for a lookup fault) and
// returns ok=false; nothing has been written to the store at that point.
func (s *Server) applyCreateEffectCeiling(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	role AgentRole,
	roleExplicit bool,
) (ceiling store.EffectCeiling, prov store.AuthorityProvenance, capped AgentRole, ok bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	ceiling, prov, err := s.authzService.sourceEffectCeiling(ctx, identity)
	if err != nil {
		cause, structural := ceilingDenyCauseForError(err)
		if !structural {
			slog.ErrorContext(ctx, "agent create: effect ceiling lookup failed",
				"project_id", projectID, "error", err)
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
				"Unable to evaluate the credential's delegation ceiling; retry later", nil)
			return store.EffectCeiling{}, store.AuthorityProvenance{}, "", false
		}
		logAuthzDenial(r, identity, resource, ActionCreate,
			"effect ceiling denied: "+string(cause)+": "+err.Error())
		writeForbiddenDenial(w, ceilingSourceDenialMessage(cause), DeniedByDelegationCeiling)
		return store.EffectCeiling{}, store.AuthorityProvenance{}, "", false
	}

	capped, cause, allowed := childRoleWithinCeiling(ceiling, role, roleExplicit)
	if !allowed {
		msg := reasonNoUsableRole
		if roleExplicit {
			msg = fmt.Sprintf("the credential's scopes do not cover agent role %q; request a role the token covers, request role=none explicitly, or create from a session", role)
		}
		logAuthzDenial(r, identity, resource, ActionCreate,
			"effect ceiling denied: "+string(cause)+": "+msg)
		writeForbiddenDenial(w, msg, DeniedByDelegationCeiling)
		return store.EffectCeiling{}, store.AuthorityProvenance{}, "", false
	}
	return ceiling, prov, capped, true
}

// ceilingSourceDenialMessage is the neutral response message for a source
// credential whose ceiling cannot be recorded.
func ceilingSourceDenialMessage(cause DenyCause) string {
	switch cause {
	case DenyCauseCeilingSourceNotAllowed:
		return "This credential kind cannot delegate agent authority"
	case DenyCauseCeilingUnrecorded:
		return "The credential's scope ceiling version is not supported; reissue the token"
	default:
		return "The creating agent's delegation record is missing or inconsistent"
	}
}

// projectMaxAgentRole returns the project's maximum agent role annotation,
// or full when it is unset or invalid. Shared by create and by reincarnate
// --role, so both cap a requested role against the same value.
func projectMaxAgentRole(project *store.Project) AgentRole {
	if project != nil && project.Annotations != nil {
		if maxStr, ok := project.Annotations[projectSettingMaxAgentRole]; ok && maxStr != "" {
			if ValidAgentRole(AgentRole(maxStr)) {
				return AgentRole(maxStr)
			}
		}
	}
	return AgentRoleFull
}

// callerAgentRoleCeiling returns the stored role of the calling agent
// (the no-escalation ceiling for any role it grants) and its message mode.
// It fails closed: a lookup failure or an invalid stored role yields
// baseline, so a transient error never grants maximum privileges. Shared by
// create and by reincarnate --role.
func (s *Server) callerAgentRoleCeiling(ctx context.Context, callerAgentID string) (role AgentRole, messageMode string) {
	callerAgent, err := s.store.GetAgent(ctx, callerAgentID)
	if err != nil {
		// Fail-closed: default to baseline on lookup failure so that
		// transient errors do not grant maximum privileges.
		slog.Warn("Failed to read parent agent for role ceiling",
			"parent_agent_id", callerAgentID, "error", err)
		return AgentRoleBaseline, ""
	}
	role, _ = agentRoleAndScopes(callerAgent)
	messageMode = callerAgent.MessageMode

	// Validate stored role to guard against corrupted data.
	if !ValidAgentRole(role) {
		slog.Warn("Parent agent has invalid stored role, defaulting to baseline",
			"parent_agent_id", callerAgentID, "stored_role", role)
		role = AgentRoleBaseline
	}
	return role, messageMode
}

func (s *Server) createAgentInProject(
	w http.ResponseWriter,
	r *http.Request,
	req CreateAgentRequest,
	projectID string,
	createdBy string,
	creatorName string,
	ancestry []string,
	notifySubscriberType string,
	notifySubscriberID string,
) {
	ctx := r.Context()
	ctx, span := tracer.Start(ctx, "hub.agent.create")
	defer span.End()
	// Note: HTTP error status is recorded by the otelhttp parent span.
	span.SetAttributes(
		attribute.String("scion.agent.name", req.Name),
		attribute.String("scion.project.id", projectID),
	)
	hubCreateStart := time.Now()

	// Field-level GCP identity validation, before any persistence or SA
	// resolution. Both create routes reach it here.
	if !validateGCPIdentityRequest(w, req.GCPIdentity) {
		return
	}

	// Verify project exists and get its configuration
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Reject agent creation in template projects
	if project.IsTemplate() {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"cannot create agents in a template project", nil)
		return
	}

	// Empty-per-agent projects give every agent its own private directory
	// (design #2703 §2.4). A relative workspace path would otherwise resolve
	// against the shared project dir, so it is not accepted.
	if project.IsEmptyPerAgent() && req.Workspace != "" {
		ValidationError(w, `"Empty directory per agent" (empty-per-agent) projects do not take a workspace path`, nil)
		return
	}

	// Resolve effective agent role using the authority lattice.
	// Computed early (before broker resolution) so that fail-loud 403 on
	// role over-requests fires before resource-intensive operations.
	var effectiveRole AgentRole
	var parentRole AgentRole     // empty for user-created agents; set in agent-caller branch
	var parentMessageMode string // parent's message_mode for inheritance (D10)
	requestedRole := AgentRole(req.AgentRole)

	// Read project max agent role from annotations (default: full)
	projectMax := projectMaxAgentRole(project)

	// Read default agent role: project annotation → hub default → full.
	// Applied only when no explicit role is requested.
	defaultAgentRole := AgentRoleFull
	foundProjectDefault := false
	if project != nil && project.Annotations != nil {
		if defStr, ok := project.Annotations[projectSettingDefaultAgentRole]; ok && defStr != "" {
			if ValidAgentRole(AgentRole(defStr)) {
				defaultAgentRole = AgentRole(defStr)
				foundProjectDefault = true
			}
		}
	}
	if !foundProjectDefault {
		// No project-level default set; fall back to hub-level default
		if hubDefault := s.hubAgentDefaults().DefaultAgentRole; hubDefault != "" {
			if ValidAgentRole(AgentRole(hubDefault)) {
				defaultAgentRole = AgentRole(hubDefault)
			}
		}
	}

	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		// Agent caller: read parent agent's stored role for no-escalation ceiling.
		parentRole, parentMessageMode = s.callerAgentRoleCeiling(ctx, agentIdent.ID())

		// Log the parent role for audit trail
		slog.Info("Agent creating sub-agent",
			"parent_agent_id", agentIdent.ID(),
			"parent_role", parentRole,
			"requested_role", requestedRole,
			"project_max", projectMax,
		)

		if requestedRole == "" {
			requestedRole = parentRole // default: inherit parent's role
		}

		// Enforce no-escalation: sub-agent role cannot exceed parent's role.
		// Fail-loud so template misconfiguration is visible (a template requesting
		// "full" for a sub-agent under a "baseline" parent is almost certainly wrong).
		if req.AgentRole != "" && CompareRoles(AgentRole(req.AgentRole), parentRole) > 0 {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				fmt.Sprintf("Cannot grant sub-agent role %q: parent agent role is %q",
					req.AgentRole, parentRole), nil)
			return
		}

		effectiveRole = minRole(requestedRole, parentRole, projectMax)
	} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		// User caller: project max is the creation-time limiter.
		// The live delegation ceiling (Phase 1G) handles user authority bounding
		// at decision time rather than at role resolution time.
		if requestedRole == "" {
			requestedRole = defaultAgentRole
		}

		// Fail-loud: reject explicit over-request against project max.
		if req.AgentRole != "" && CompareRoles(AgentRole(req.AgentRole), projectMax) > 0 {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				fmt.Sprintf("Cannot grant agent role %q: project maximum is %q",
					req.AgentRole, projectMax), nil)
			return
		}

		effectiveRole = minRole(requestedRole, projectMax)
	} else {
		// No identity (should not happen in practice) - default to configured default
		if requestedRole == "" {
			requestedRole = defaultAgentRole
		}
		effectiveRole = requestedRole
	}

	// CanDelegate check (Phase 1F): ensure the actor has sufficient authority
	// to delegate the effective agent role and scopes.
	var delegateDecision Decision
	if s.authzService != nil {
		actorIdentity := GetIdentityFromContext(ctx)
		if actorIdentity != nil {
			// Resolve additional scopes from the template (if any).
			var additionalScopes []AgentTokenScope
			// The effective role determines the scopes the agent will get.
			grantDesc := GrantDescriptor{
				Type:        GrantTypeAgentDelegation,
				AgentRole:   string(effectiveRole),
				AgentScopes: additionalScopes,
				ProjectID:   projectID,
				ScopeType:   store.RoleScopeProject,
				ScopeID:     projectID,
			}
			delegateDecision = s.authzService.CanDelegate(ctx, actorIdentity, grantDesc)
			if !delegateDecision.Allowed {
				logAuthzDenial(r, actorIdentity, Resource{
					Type:       "agent",
					ParentType: "project",
					ParentID:   projectID,
				}, ActionCreate, "CanDelegate denied: "+delegateDecision.Reason)
				writeForbidden(w, "Cannot delegate agent authority you do not hold: "+delegateDecision.Reason)
				return
			}
		}
	}

	// Effect ceiling: freeze the source credential's ceiling and provenance
	// for the delegation edge, and cap the child's role to what the ceiling
	// covers. This runs after CanDelegate and before the role→NoAuth mapping
	// so that the stored role, the edge role and NoAuth derive from the same
	// value.
	var edgeCeiling store.EffectCeiling
	var edgeProvenance store.AuthorityProvenance
	if s.authzService != nil {
		var ok bool
		edgeCeiling, edgeProvenance, effectiveRole, ok = s.applyCreateEffectCeiling(w, r, projectID, effectiveRole, req.AgentRole != "")
		if !ok {
			return
		}
	}

	// Map role=none to NoAuth behavior
	if effectiveRole == AgentRoleNone {
		req.NoAuth = true
	}

	// Resolve the runtime broker
	runtimeBrokerID, err := s.resolveRuntimeBroker(ctx, w, req.RuntimeBrokerID, project)
	if err != nil {
		// Error response already written by resolveRuntimeBroker
		return
	}

	// Enforce broker-level dispatch authorization: only the broker owner can create agents on it
	if runtimeBrokerID != "" {
		if !s.checkBrokerDispatchAccess(ctx, w, runtimeBrokerID) {
			return
		}
	}

	// Empty-per-agent projects only dispatch to brokers that advertise the
	// capability (design #2703 D3): 412 before anything is persisted.
	if !s.requireEmptyPerAgentBrokerCapability(ctx, w, project, runtimeBrokerID) {
		return
	}

	// Validate GCP passthrough mode. Two independent checks:
	//  1. Broker-owner/admin restriction.
	//  2. actAs on the broker host service account (requires the broker to
	//     have its host SA registered).
	//
	// Both are enforced by authorizePassthroughIdentity. The ownership check
	// is deliberately a hand-rolled comparison rather than policy-based
	// authorization — see passthrough_gate.go for the reasoning.
	if req.GCPIdentity != nil && req.GCPIdentity.MetadataMode == store.GCPMetadataModePassthrough && runtimeBrokerID != "" {
		broker, err := s.store.GetRuntimeBroker(ctx, runtimeBrokerID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if !s.authorizePassthroughIdentity(w, r, broker, SurfacePassthroughCreate) {
			return
		}
	}

	// Validate GCP identity SA assignment: verify the SA exists, belongs to this project, and is verified.
	var resolvedGCPSA *store.GCPServiceAccount
	if req.GCPIdentity != nil && req.GCPIdentity.MetadataMode == store.GCPMetadataModeAssign {
		sa, err := s.store.GetGCPServiceAccount(ctx, req.GCPIdentity.ServiceAccountID)
		if err != nil {
			// errors.Is, not ==, and that is load-bearing rather than style. A
			// wrapped ErrNotFound would miss a == comparison and fall through to
			// writeErrorFromErr, which answers ErrNotFound with 404 — reopening
			// the existence oracle this branch exists to close, silently and from
			// a change in another package. See msgSANotAvailableInProject.
			if errors.Is(err, store.ErrNotFound) {
				ValidationError(w, msgSANotAvailableInProject, nil)
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		// Scope-aware admissibility (P4 item F). This was `sa.ScopeID != projectID`,
		// which never read sa.Scope and so was not a scope check at all: it
		// compared a hub-scoped account's hub instance ID against a project ID and
		// rejected it. ReachableFromProject keeps project-scoped accounts confined
		// to their own project and admits hub-scoped ones from anywhere, which is
		// what makes a hub-wide account assignable.
		if !sa.ReachableFromProject(projectID) {
			ValidationError(w, msgSANotAvailableInProject, nil)
			return
		}
		if !gcpServiceAccountVerified(sa) {
			ValidationError(w, "GCP service account is not verified; verify it before assigning to agents", nil)
			return
		}

		// Authorization: ActionAssign in Hub policy, plus iam.serviceAccounts.actAs
		// on the caller in GCP. "Can see it" is no longer sufficient — reading a
		// service account and being allowed to run as it are different grants.
		// SA management (create/mint/delete) is gated on ActionManage elsewhere.
		//
		// The whole gate lives in authorizeSAAssignment, including the ordering of
		// the two layers, so the create and PATCH paths cannot drift apart on the
		// part that matters. Read it there before changing either call.
		if !s.authorizeSAAssignment(w, r, sa, SurfaceAgentCreate) {
			return
		}

		resolvedGCPSA = sa
	}

	// Check if the agent already exists (e.g. created via "scion create" for later start).
	// If it exists in "created" status, start it instead of creating a duplicate.
	// If it doesn't exist, fall through to create it.
	slug, err := api.ValidateAgentName(req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", err.Error(), nil)
		return
	}
	existingAgent, err := s.store.GetAgentBySlug(ctx, projectID, slug)
	if err != nil && err != store.ErrNotFound {
		writeErrorFromErr(w, err, "")
		return
	}

	// Collect warnings the dispatcher raises (hub-side TZ drops and the
	// broker's hub-only env warnings) so they reach this response, including
	// when an existing agent is started, resumed or recovered below.
	ctx, dispatchWarns := withDispatchWarnings(ctx)

	switch s.handleExistingAgent(ctx, w, existingAgent, project, runtimeBrokerID, req, notifySubscriberType, notifySubscriberID, createdBy) {
	case existingAgentStarted, existingAgentErrored:
		return // Response already written.
	case existingAgentConflict:
		Conflict(w, fmt.Sprintf("agent %q already exists in this project", slug))
		return
	case existingAgentDeleted:
		// Fall through to create a new agent below.
	case existingAgentNone:
		// No existing agent — fall through to create.
	}

	// Apply project-level default template if no template specified in request
	if req.Template == "" && project != nil && project.Annotations != nil {
		if dt := project.Annotations[projectSettingDefaultTemplate]; dt != "" {
			req.Template = dt
		}
	}

	// Hub operational default template — lowest tier. Below the request and
	// below the project annotation, both of which have already had their chance
	// above. In file mode hubAgentDefaults() is always the zero value, so this
	// rung never fires there and file-mode dispatch is unchanged (design
	// §3.2.4). The scheduler-dispatch path in server.go carries the identical
	// rung; design §5.2 risk (d) is the two paths diverging again.
	templateFromHubDefault := false
	if req.Template == "" {
		if d := s.hubAgentDefaults(); d.DefaultTemplate != "" {
			req.Template = d.DefaultTemplate
			templateFromHubDefault = true
		}
	}

	// Global fallback: when no template name arrived from the request, the
	// project annotation, or the hub operational defaults, try resolving a
	// template named "default". This ensures a fresh hosted hub with no
	// agent_defaults configured can still resolve its built-in template.
	// Like templateFromHubDefault, a missing "default" template degrades
	// (warn + continue with no template) rather than failing the create.
	templateFromImplicitDefault := false
	if req.Template == "" {
		req.Template = "default"
		templateFromImplicitDefault = true
	}

	// Resolve template if specified - the client may pass either a template ID or name
	//
	// DEGRADATION RULE (design §3.2.2) — when, and only when, the name came
	// from the hub operational default, an unusable template must log a warning
	// and continue with no template instead of failing the create. A hub-wide
	// default naming a template that has since been deleted would otherwise
	// mean "nobody in this deployment can create an agent" — an operational
	// setting turned into an outage. Provenance comes from the
	// templateFromHubDefault flag set above and is never inferred by re-reading
	// the setting, which cannot distinguish a hub default from a user who
	// happened to name the same template.
	//
	// TWO of the three unusable-template exits below degrade — not-found, and
	// the file-less (still-pending) template. The design named the 404 as the
	// instance; the file-less case is the same thing, because a stale hub
	// default pointing at a pending template blocks every create in the
	// deployment exactly as one pointing at a deleted template does.
	//
	// The store-error exit deliberately does NOT degrade, and the asymmetry is
	// the point rather than an oversight — do not "finish the job" by adding it:
	//
	//   - The other two are DETERMINISTIC. The template genuinely is unusable,
	//     it will still be unusable on the next create, and the operator gets
	//     the same self-describing warning every time until they fix the
	//     setting. That trades a permanent outage for a permanent, visible
	//     warning.
	//
	//   - A store error is TRANSIENT, and it is an I-don't-know rather than a
	//     this-is-broken. A DB blip is no evidence that the setting is stale.
	//     Degrading it would mean some creates silently get no template and
	//     others get one depending on store weather — intermittent silent
	//     misconfiguration, harder to diagnose than the clean failure it
	//     replaced, because the agent comes up looking fine and then behaves
	//     differently from its siblings for a reason its own record cannot
	//     explain.
	//
	//   - The deployment-outage argument does not carry here either: if
	//     resolveTemplate is returning store errors then creates are already
	//     failing for infrastructure reasons, and failing loudly is correct in
	//     that state. This rule exists for stale SETTINGS, not unhealthy stores.
	//
	// This is a house convention, not a local judgement call. The pre-start hook
	// resolution in handlers_agent_create_helpers.go draws the same line and
	// writes down the same reasoning: "The hub fallback is entered only on a
	// definitive 'no project hook' (ErrNotFound). Any other project-lookup
	// failure (DB blip, duplicate rows) is ambiguous [...] On an ambiguous error
	// we log and stage nothing." Same principle — never treat an ambiguous error
	// as evidence — with one honest difference: that code can only log and do
	// nothing, whereas here the caller is still on the other end of an HTTP
	// request, so the ambiguous branch can and should return a real error.
	var resolvedTemplate *store.Template
	if req.Template != "" {
		resolvedTemplate, err = s.resolveTemplate(ctx, req.Template, projectID)
		// SECURITY-GATE (ptone/scion#1916): a resolved candidate is not yet
		// known to be one the caller may read — resolveTemplate's by-ID arm
		// looks across every scope. Fold a denial into the same "not found"
		// branch immediately below so a template that exists but is unreadable
		// degrades exactly like one that does not exist.
		if err == nil && resolvedTemplate != nil && !s.authorizeResolvedTemplate(ctx, GetIdentityFromContext(ctx), resolvedTemplate) {
			resolvedTemplate = nil
			err = store.ErrNotFound
		}
		switch {
		case err != nil && err != store.ErrNotFound:
			// Always hard-fails, hub-default provenance included. See above.
			writeErrorFromErr(w, err, "")
			return

		case resolvedTemplate == nil:
			// Template was requested but not found — check if the broker has
			// local access and can resolve it from its own filesystem.
			brokerHasLocal := false
			if runtimeBrokerID != "" {
				provider, err := s.store.GetProjectProvider(ctx, projectID, runtimeBrokerID)
				if err == nil && provider.LocalPath != "" {
					brokerHasLocal = true
				}
			}
			switch {
			case brokerHasLocal:
				// Template will be resolved locally by the broker
			case templateFromHubDefault:
				s.warnHubDefaultTemplateUnusable(ctx, req.Template, projectID, "not found")
				req.Template = ""
			case templateFromImplicitDefault:
				// The implicit "default" fallback template doesn't exist — this
				// is normal on hubs that haven't created one. Continue with no
				// template. No warning: unlike a hub default (which is operator-
				// configured and should resolve), the implicit fallback is
				// speculative.
				req.Template = ""
			default:
				NotFound(w, "Template")
				return
			}

		case len(resolvedTemplate.Files) == 0 && resolvedTemplate.ContentHash == "":
			// Guard: reject dispatch when the resolved template has no files and
			// no content hash. This catches templates stuck in 'pending' state
			// before they reach broker hydration (where the failure is opaque).
			name := resolvedTemplate.Slug
			if name == "" {
				name = resolvedTemplate.Name
			}
			if !templateFromHubDefault && !templateFromImplicitDefault {
				ValidationError(w, "template "+name+" has no files — sync template files first with: scion template sync "+name, nil)
				return
			}
			s.warnHubDefaultTemplateUnusable(ctx, req.Template, projectID,
				"template "+name+" has no files — sync template files first with: scion template sync "+name)
			req.Template, resolvedTemplate = "", nil
		}
	}

	agent := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            slug,
		Name:            slug,
		Template:        req.Template,
		ProjectID:       projectID,
		RuntimeBrokerID: runtimeBrokerID,
		Phase:           string(state.PhaseCreated),
		Labels:          req.Labels,
		CreatedBy:       createdBy,
		OwnerID:         createdBy,
		Ancestry:        ancestry,
		// Async agent create (design §3.2): persisted so a later env/workspace
		// finalize request can still see the client's opt-in. Read by
		// dispatchLaunching (launch_dispatch.go).
		LaunchAsyncOptIn: req.AcceptAsyncLaunch,
	}

	// Store human-friendly slug instead of UUID for display
	if resolvedTemplate != nil && resolvedTemplate.Slug != "" {
		agent.Template = resolvedTemplate.Slug
	}

	if req.Config != nil && req.Config.ThinkingLevel != nil {
		if tl := *req.Config.ThinkingLevel; tl < 0 || tl > 100 {
			BadRequest(w, "thinking_level must be between 0 and 100")
			return
		}
	}

	// Harness-config resolution (request > project annotation > template
	// default) happens later, in deriveAgentConfig, along with the rest of
	// create's config-resolution pipeline — not here.
	agent.AppliedConfig = s.buildAppliedConfig(req, creatorName, effectiveRole)

	// Resolve message_mode (D10 spawn defaults):
	//   1. Explicit req.MessageMode from CLI flag → use it (after validation).
	//   2. Template specifies message_mode → use it.
	//   3. Parent agent exists → inherit parent's message_mode.
	//   4. Otherwise → default to "project" (handled by Ent schema default).
	if req.MessageMode != "" {
		if !store.IsValidMessageMode(req.MessageMode) {
			ValidationError(w, "invalid message mode: "+req.MessageMode, nil)
			return
		}
		agent.MessageMode = req.MessageMode
	} else if resolvedTemplate != nil && resolvedTemplate.Config != nil && resolvedTemplate.Config.MessageMode != "" {
		if !store.IsValidMessageMode(resolvedTemplate.Config.MessageMode) {
			ValidationError(w, "invalid template message mode: "+resolvedTemplate.Config.MessageMode, nil)
			return
		}
		agent.MessageMode = resolvedTemplate.Config.MessageMode
	} else if parentMessageMode != "" {
		agent.MessageMode = parentMessageMode
	} else {
		agent.MessageMode = store.MessageModeProject
	}

	// Hub-mode grant guard: for creation, any effective "hub" is a new grant,
	// even if it came from a template, parent inheritance, or a default.
	// The guard runs after effective mode resolution.
	if agent.MessageMode == store.MessageModeHub {
		identity := GetIdentityFromContext(ctx)
		decision := s.AuthorizeMessageModeGrant(ctx, identity, projectID, agent.MessageMode)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Cannot grant hub message mode: "+decision.Reason, nil)
			return
		}
	}

	// Populate GCP identity in applied config.
	// When no GCP identity is specified anywhere in the ladder below, the
	// applied config is left unset (see the final default: arm) rather than
	// an explicit "block" record, so the broker applies its own
	// runtime-aware default — "block" on every runtime except Kubernetes
	// (unchanged; agents still cannot access the underlying compute identity
	// via the GCE metadata server unless explicitly opted into "passthrough"
	// or "assign"), "passthrough" on Kubernetes, which does not support
	// "block" (ptone/scion#2328 phase 1).
	if req.GCPIdentity != nil {
		switch req.GCPIdentity.MetadataMode {
		case store.GCPMetadataModeAssign:
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode:        store.GCPMetadataModeAssign,
				ServiceAccountID:    resolvedGCPSA.ID,
				ServiceAccountEmail: resolvedGCPSA.Email,
				ProjectID:           resolvedGCPSA.ProjectID,
			}
		case store.GCPMetadataModePassthrough:
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModePassthrough,
			}
		case store.GCPMetadataModeBlock:
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModeBlock,
			}
		default:
			// Unreachable: validateGCPIdentityRequest rejects any other mode at
			// the top of this function. Asserted rather than assumed — before
			// the hoist this switch had no default arm, and on the project
			// route that accident was the *only* thing standing between an
			// unrecognised mode and the agent container (design §8.4). An
			// unrecognised mode reaching here now means the validation was
			// removed or bypassed, which is a server bug, not a client one.
			slog.Error("unreachable: unvalidated GCP metadata mode reached agent config build",
				"metadata_mode", req.GCPIdentity.MetadataMode, "project_id", projectID)
			InternalError(w)
			return
		}
	} else if profileName, profileSAID := s.projectProfileDefaultSA(ctx, runtimeBrokerID, project, agent.AppliedConfig.Profile); profileSAID != "" {
		// No explicit GCP identity, and the project sets a default service
		// account for the profile this agent runs under. It is more specific
		// than the project-wide default below and wins over it, including an
		// explicit project "block" (block is not offered on Kubernetes,
		// ptone/scion#2328, where per-profile defaults matter most).
		cfg, ok := s.resolveDefaultSAAssignment(ctx, w, r, projectID, profileSAID, SurfaceProjectDefault, profileDefaultTier(profileName))
		if !ok {
			return
		}
		agent.AppliedConfig.GCPIdentity = cfg
		pinResolvedProfile(agent.AppliedConfig, profileName)
		slog.Debug("GCP identity chosen by default", "source", "project-profile-default",
			"project_id", projectID, "agent", agent.Name, "profile", profileName, "sa_id", cfg.ServiceAccountID)
	} else {
		// No explicit GCP identity — check project default, then fall back to
		// the hub default, then to unset (the broker applies its runtime
		// default) if the hub has none either.
		projectSettings := projectSettingsFromAnnotations(project)
		switch projectSettings.DefaultGCPIdentityMode {
		case store.GCPMetadataModePassthrough:
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModePassthrough,
			}
		case store.GCPMetadataModeAssign:
			if projectSettings.DefaultGCPIdentityServiceAccountID != "" {
				cfg, ok := s.resolveDefaultSAAssignment(ctx, w, r, projectID,
					projectSettings.DefaultGCPIdentityServiceAccountID, SurfaceProjectDefault, defaultTierProject)
				if !ok {
					return
				}
				agent.AppliedConfig.GCPIdentity = cfg
				slog.Debug("GCP identity chosen by default", "source", "project-default",
					"project_id", projectID, "agent", agent.Name, "sa_id", cfg.ServiceAccountID)
			} else {
				agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
					MetadataMode: store.GCPMetadataModeBlock,
				}
			}
		case store.GCPMetadataModeBlock:
			// Project explicitly set "block" — that is the operator's decision
			// and it stops the ladder here. Do NOT fall through to the hub
			// default: an explicit "block" is different from "no project
			// setting at all", and only the latter defers further down the
			// chain.
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModeBlock,
			}
		default:
			// No project default configured (empty string) — fall back to the
			// hub-level operational default, then to unset (the broker
			// applies its runtime default) if the hub has none either.
			// Mirrors the project-default case above one rung down the
			// ladder: explicit request -> project default -> hub default ->
			// unset (see hub_agent_defaults.go).
			hubDefaults := s.hubAgentDefaults()
			switch hubDefaults.DefaultGCPIdentityMode {
			case store.GCPMetadataModePassthrough:
				// Hub-default passthrough is confined to the embedded broker
				// (see hubDefaultPassthroughAllowed for why).
				//
				// The effective profile is computed here, before
				// deriveAgentConfig would otherwise stamp AppliedConfig.Profile
				// from the project's active-profile annotation: the gate must
				// see the same profile the agent will actually dispatch under,
				// not just what the request named (see
				// effectiveRuntimeProfileName).
				effectiveProfile := effectiveRuntimeProfileName(agent.AppliedConfig.Profile, project)
				// Only write an explicit record when the grant is allowed.
				// On denial, leave AppliedConfig.GCPIdentity unset instead of
				// an explicit "block" (ptone/scion#2328): the operator never
				// chose "block" here — they chose "passthrough" and it was
				// denied — so this is treated exactly like "no hub default at
				// all", letting the broker apply its own runtime-aware
				// default (unchanged "block" on every runtime except
				// Kubernetes; "passthrough" on Kubernetes, since Kubernetes
				// does not support "block"). An explicit per-agent
				// passthrough request that fails its own equivalent check
				// still errors clearly elsewhere (translatePassthroughForSandbox
				// / the passthrough authorization gate) — this arm only
				// covers the hub-wide default, which has no caller to report
				// an error to.
				if allowed, resolvedProfile := s.hubDefaultPassthroughAllowed(ctx, runtimeBrokerID, projectID, agent.Name, effectiveProfile); allowed {
					// Pin the exact profile the gate checked so the broker
					// cannot dispatch under a different one later: once
					// AppliedConfig.Profile is set, deriveAgentConfig's
					// applyProjectDefaults leaves it alone.
					if agent.AppliedConfig.Profile == "" {
						agent.AppliedConfig.Profile = resolvedProfile
					}
					// CreateInputs.Profile is captured a few lines up in
					// buildAppliedConfig, before this gate runs, so the pin
					// above never reaches it on its own. scion reincarnate
					// replays CreateInputs, not the live AppliedConfig
					// (design §3.3 Amendment A1), and would otherwise
					// re-derive an empty profile against whatever the
					// project's active profile is *at reincarnate time* —
					// silently losing the pin while keeping the carried-over
					// passthrough grant. Only fill it when still empty, so an
					// explicit request profile (already captured there) is
					// never overwritten.
					if agent.AppliedConfig.CreateInputs != nil && agent.AppliedConfig.CreateInputs.Profile == "" {
						agent.AppliedConfig.CreateInputs.Profile = resolvedProfile
					}
					agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
						MetadataMode: store.GCPMetadataModePassthrough,
						// RequireLocalRuntime asks the broker to re-check the
						// resolved runtime itself once it knows it: the hub
						// resolves runtimeBrokerID's profile from the broker's
						// own registration data (resolveAgentRuntimeProfileType),
						// which the broker's own dispatch-time settings can
						// differ from. Only ever set on a hub-default grant —
						// this whole block only runs when
						// hubDefaultPassthroughAllowed returned true. Explicit
						// and project-level passthrough are never flagged.
						RequireLocalRuntime: true,
					}
				}
			case store.GCPMetadataModeAssign:
				if hubDefaults.DefaultGCPIdentityServiceAccountID != "" {
					cfg, ok := s.resolveDefaultSAAssignment(ctx, w, r, projectID,
						hubDefaults.DefaultGCPIdentityServiceAccountID, SurfaceHubDefault, defaultTierHub)
					if !ok {
						return
					}
					agent.AppliedConfig.GCPIdentity = cfg
					slog.Debug("GCP identity chosen by default", "source", "hub-default",
						"project_id", projectID, "agent", agent.Name, "sa_id", cfg.ServiceAccountID)
				} else {
					agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
						MetadataMode: store.GCPMetadataModeBlock,
					}
				}
			case store.GCPMetadataModeBlock:
				// Hub explicitly configured "block" as its own default — an
				// explicit choice, kept as an explicit record (rejected on
				// the Kubernetes runtime by the broker, ptone/scion#2328
				// phase 1, same as an explicit per-agent or project-default
				// "block").
				agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
					MetadataMode: store.GCPMetadataModeBlock,
				}
			default:
				// No hub default configured at all (empty) — leave
				// AppliedConfig.GCPIdentity unset rather than writing an
				// explicit "block" record. This lets the broker apply its
				// own runtime-aware default: "block" on every runtime except
				// Kubernetes (unchanged), "passthrough" on Kubernetes, since
				// Kubernetes does not support "block" (ptone/scion#2328
				// phase 1). Distinguishing "nothing configured" from
				// "explicitly block" here is what makes that possible — see
				// the store.GCPMetadataModeBlock case above and the project
				// rung's explicit-block case earlier in this function, both
				// of which still write an explicit record.
			}
		}
	}

	// Passthrough-to-assign translation for cloudrun-sandbox runtimes.
	//
	// gVisor sandboxes cannot reach the real GCE metadata server at
	// 169.254.169.254, so passthrough mode produces no credentials. When
	// the target broker runs a cloudrun-sandbox profile, translate
	// passthrough to assign using the broker's host service account —
	// semantically equivalent (same identity) and the assign machinery
	// works inside the sandbox.
	//
	// The translation runs after both the explicit and project-default
	// identity paths, so it covers every surface that sets passthrough.
	if agent.AppliedConfig != nil &&
		agent.AppliedConfig.GCPIdentity != nil &&
		agent.AppliedConfig.GCPIdentity.MetadataMode == store.GCPMetadataModePassthrough &&
		runtimeBrokerID != "" {
		if err := s.translatePassthroughForSandbox(ctx, agent, runtimeBrokerID); err != nil {
			slog.ErrorContext(ctx, "passthrough-to-assign translation failed",
				"agent", agent.Name, "broker", runtimeBrokerID, "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError,
				"failed to configure GCP identity for sandbox runtime: "+err.Error(), nil)
			return
		}
	}

	if req.Config != nil {
		agent.Image = req.Config.Image
		if req.Config.Detached != nil {
			agent.Detached = *req.Config.Detached
		} else {
			agent.Detached = true
		}
	} else {
		agent.Detached = true
	}

	// Apply project-level defaults, hub operational defaults, and the
	// template/harness-config derivation pipeline. See deriveAgentConfig.
	// It fails only when workspace storage did not respond; that is answered
	// with 503 here, before any quota reservation or agent row exists.
	if err := s.deriveAgentConfig(ctx, agent, project, resolvedTemplate); err != nil {
		if !writeWorkspaceStorageUnavailable(w, err) {
			InternalError(w)
		}
		return
	}

	// Quota enforcement, in order:
	//  1. Per-broker agent ceiling (ptone/scion#1303). Exceeding the ceiling
	//     must reject the create outright — before any per-project
	//     reservation, before store.CreateAgent, and well before broker
	//     dispatch — so a caller past the ceiling never gets a 201 that host
	//     resource exhaustion (OOM/SIGBUS cascade destroying the whole
	//     Instance the broker runs on) then makes good on seconds later. This
	//     is a safety gate on the infrastructure that spawns agent
	//     containers, so it is scoped to the resolved runtime broker
	//     (QuotaScopeBroker) rather than to the creator or the project: every
	//     create dispatched to the same broker competes for the same
	//     counter, regardless of who created it or which project it lands
	//     in, and a different broker (a different host, with its own
	//     capacity) has its own independent counter. Skipped entirely when
	//     runtimeBrokerID is empty (hub-direct managed agents that run
	//     without a runtime broker) — there is no broker capacity to guard,
	//     and reserving against scope_id="" would create a bogus shared
	//     bucket that every hub-direct agent competes for.
	//  2. Per-project agent limit, as before — a fairness quota, unrelated in
	//     scope and purpose to the safety gate above.
	// If step 2 fails we must roll back step 1's reservation ourselves: we
	// have not reached store.CreateAgent yet, so its failure-path Release
	// below never runs for this response.
	if runtimeBrokerID != "" {
		if !s.checkAndReserveQuota(ctx, w, store.LimitMaxAgentsPerBroker, runtimeBrokerID, store.QuotaScopeBroker, runtimeBrokerID, agent.ID) {
			return
		}
	}
	if !s.checkAndReserveQuota(ctx, w, "max_agents_per_project", createdBy, "project", projectID, agent.ID) {
		s.releaseAgentQuotas(ctx, agent.ID, runtimeBrokerID)
		return
	}

	// The agent row, identity keys, delegation edge (with the frozen
	// provenance and ceiling), the create audit record and the optional
	// notification subscription commit in one transaction, or none do.
	edgeDelegatorType := store.DelegationPrincipalUser
	if GetAgentIdentityFromContext(ctx) != nil {
		edgeDelegatorType = store.DelegationPrincipalAgent
	}
	createAudit := &store.MutationAuditRecord{
		MutationType:      mutationTypeAgentDelegation,
		CanDelegateResult: "allow",
		CanDelegateReason: delegateDecision.Reason,
	}
	var subscription *store.NotificationSubscription
	if req.Notify {
		subscription = newNotifySubscription(projectID, notifySubscriberType, notifySubscriberID, createdBy)
	}
	if err := s.commitAgentCreate(ctx, agentCreateWrite{
		Ceiling:    edgeCeiling,
		Provenance: edgeProvenance,
		Agent:      agent,
		Slug:       slug,
		Edge: &store.DelegationEdge{
			DelegatorType: edgeDelegatorType,
			DelegatorID:   createdBy,
			DelegateType:  store.DelegationPrincipalAgent,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       projectID,
			Role:          string(effectiveRole),
			Active:        true,
		},
		Audit:        createAudit,
		Subscription: subscription,
	}); err != nil {
		s.releaseAgentQuotas(ctx, agent.ID, runtimeBrokerID)
		if errors.Is(err, errInvalidDisplayName) {
			writeError(w, http.StatusBadRequest, "invalid_name", err.Error(), nil)
			return
		}
		if errors.Is(err, errAgentOwnerUserMissing) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"cannot create the agent: the user or agent it belongs to no longer exists", nil)
			return
		}
		if errors.Is(err, errAgentCreateWriteInvalid) {
			slog.ErrorContext(ctx, "agent create: incomplete create write", "error", err)
			InternalError(w)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	// cleanup rolls back the committed create through compensateAgentCreate
	// (see cleanupFailedCreate) and reports a compensation correlation ID,
	// or "" when the rollback succeeded.
	// The caller supplies the stage, the cause and the per-site steps.
	cleanup := func(rb createRollback) string {
		rb.Agent = agent
		rb.RuntimeBrokerID = runtimeBrokerID
		rb.CreateAuditID = createAudit.ID
		return s.cleanupFailedCreate(ctx, rb)
	}

	// Empty-per-agent agents start in an empty private directory (design
	// #2703), so a workspace bootstrap upload from the CLI's local directory
	// is ignored with a warning rather than rejected: a 400 would break
	// `scion start` run from a linked non-git directory.
	var warnings []string
	if project.IsEmptyPerAgent() && len(req.WorkspaceFiles) > 0 {
		s.agentLifecycleLog.Warn("Ignoring workspace files for empty-per-agent project",
			"agent_id", agent.ID, "project_id", project.ID, "files", len(req.WorkspaceFiles))
		warnings = append(warnings, api.WarningEmptyPerAgentWorkspaceFilesIgnored)
		req.WorkspaceFiles = nil
	}

	// Workspace bootstrap mode: if WorkspaceFiles are provided with a task,
	// generate signed upload URLs instead of dispatching immediately.
	// The CLI will upload files, then call finalize to trigger dispatch.
	//
	// Exception: if the target broker has a LocalPath for this project, the broker
	// can access the workspace directly from the filesystem — skip the upload
	// and fall through to the normal dispatch path.
	if len(req.WorkspaceFiles) > 0 && req.Task != "" {
		// Check if the target broker has local filesystem access to this project
		hasLocalPath := false
		if runtimeBrokerID != "" {
			provider, err := s.store.GetProjectProvider(ctx, projectID, runtimeBrokerID)
			if err == nil && provider.LocalPath != "" {
				hasLocalPath = true
				s.agentLifecycleLog.Debug("Workspace bootstrap: broker has local path, skipping upload",
					"agent_id", agent.ID,
					"broker", runtimeBrokerID, "localPath", provider.LocalPath)
			}
		}

		if !hasLocalPath && !s.isEmbeddedBroker(runtimeBrokerID) {
			stor := s.GetStorage()
			// Both failures below come after the row and reservations were
			// written but before any dispatch: nothing to delete on a broker
			// (nil deleteRuntime) and no credential minted yet (minting happens
			// in the dispatcher), so no revoke.
			if stor == nil {
				corrID := cleanup(createRollback{Stage: createStageStorage, Cause: errors.New("storage not configured for workspace bootstrap")})
				writeCreateFailure(w, corrID, func() { RuntimeError(w, "Storage not configured for workspace bootstrap") })
				return
			}

			storagePath := storage.WorkspaceStoragePath(s.HubID(), agent.ProjectID, agent.ID)
			uploadURLs, existingFiles, err := generateWorkspaceUploadURLs(ctx, stor, storagePath, req.WorkspaceFiles)
			if err != nil {
				corrID := cleanup(createRollback{Stage: createStageUploadURL, Cause: err})
				writeCreateFailure(w, corrID, func() { RuntimeError(w, "Failed to generate upload URLs: "+err.Error()) })
				return
			}

			// Set agent to provisioning phase (not dispatched yet)
			agent.Phase = string(state.PhaseProvisioning)
			if err := s.store.UpdateAgent(ctx, agent); err != nil {
				s.agentLifecycleLog.Warn("Failed to update agent status to provisioning", "agent_id", agent.ID, "error", err)
			}

			// A delete that won the race answers 409, as the synchronous
			// broker create does (ptone/scion#3099, ptone/scion#3454): no
			// agent body and no upload URLs, so the client does not upload
			// to a deleted agent. Nothing was dispatched, so there is
			// nothing to compensate; the signed URLs simply go unused.
			if !s.publishAgentCreatedIfLive(ctx, agent) {
				s.agentLifecycleLog.Info("Hub: agent was deleted while it was being created (workspace bootstrap); answering 409",
					"agent_id", agent.ID, "agent", agent.Name)
				writeDeletedDuringCreate(w, agent.ID, nil)
				return
			}

			expires := time.Now().Add(SignedURLExpiry)
			s.enrichAgent(ctx, agent, project, nil)

			if len(existingFiles) > 0 {
				s.agentLifecycleLog.Debug("Workspace bootstrap: files already in storage", "agent_id", agent.ID, "count", len(existingFiles))
			}

			writeJSON(w, http.StatusCreated, CreateAgentResponse{
				Agent:      redactedAgentCopy(ctx, s, agent),
				Warnings:   warnings,
				UploadURLs: uploadURLs,
				Expires:    &expires,
			})
			return
		}
	}

	// Hub-native/shared-workspace project remote broker support: if the project has
	// a managed workspace and the workspace path is set, upload it to GCS so
	// a remote broker can download it. Empty-per-agent projects have no
	// project workspace to ship: each agent's directory is broker-local.
	if syncsHubProjectWorkspace(project) && agent.AppliedConfig != nil && agent.AppliedConfig.Workspace != "" {
		// The local-path read, the upload and the swap write run detached
		// from the client (ptone/scion#1961) under their own budget: a
		// client that gives up here must not make a broker with a local
		// path read as one without, or leave the launch below to go ahead
		// with the unswapped hub-local workspace.
		uctx, ucancel := context.WithTimeout(detachLaunchFromClient(ctx), hubWorkspaceUploadTimeout)
		hasLocalPath := false
		if runtimeBrokerID != "" {
			provider, err := s.store.GetProjectProvider(uctx, project.ID, runtimeBrokerID)
			if err == nil && provider.LocalPath != "" {
				hasLocalPath = true
			}
		}

		if !hasLocalPath && !s.isEmbeddedBroker(runtimeBrokerID) {
			stor := s.GetStorage()
			if stor != nil {
				resolvedWorkspace, workspaceErr := s.resolveHubManagedWorkspaceForUpload(agent.AppliedConfig.Workspace, project.Slug)
				if errors.Is(workspaceErr, errWorkspaceContentTimeout) {
					// Workspace storage did not respond. Dispatching without
					// the upload would leave the remote broker resolving the
					// workspace against its own stale or empty project copy,
					// so the create fails here and answers 503 without the
					// path. As with the workspace-bootstrap failures above,
					// the agent row and quotas exist, nothing has been
					// dispatched (nil DeleteRuntime) and no credential has
					// been minted (no revoke). A failed compensation answers
					// 500 with its correlation ID instead (writeCreateFailure).
					s.agentLifecycleLog.Warn("Workspace storage did not respond; failing agent create",
						"agent_id", agent.ID, "project_id", project.ID, "error", workspaceErr)
					ucancel()
					corrID := cleanup(createRollback{Stage: createStageWorkspaceStorage, Cause: workspaceErr})
					writeCreateFailure(w, corrID, func() { writeWorkspaceStorageUnavailable(w, workspaceErr) })
					return
				} else if workspaceErr != nil {
					s.agentLifecycleLog.Warn("Skipping GCS upload of invalid hub-managed project workspace",
						"agent_id", agent.ID,
						"project_id", project.ID, "error", workspaceErr)
				} else {
					storagePath := storage.ProjectWorkspaceStoragePath(s.HubID(), project.ID)
					if err := syncToGCSForWorkspaceUpload(uctx, resolvedWorkspace, stor.Bucket(), storagePath+"/files"); err != nil {
						if errors.Is(err, context.DeadlineExceeded) && uctx.Err() != nil {
							// The upload ran past our own budget: fail the
							// create rather than launch with the hub-local
							// workspace. Nothing was dispatched and no
							// credential minted yet.
							ucancel()
							corrID := cleanup(createRollback{Stage: createStageWorkspaceUpload, Cause: err})
							writeCreateFailure(w, corrID, func() { RuntimeError(w, "Timed out uploading the project workspace: "+err.Error()) })
							return
						}
						s.agentLifecycleLog.Warn("Failed to upload hub-managed project workspace to GCS",
							"agent_id", agent.ID,
							"project_id", project.ID, "error", err)
					} else {
						// Swap workspace to storage path for remote broker
						agent.AppliedConfig.Workspace = ""
						agent.AppliedConfig.WorkspaceStoragePath = storagePath
						// The upload above is always GCS (gcp.SyncToGCS), so
						// stor.Bucket() names the GCS bucket whatever stor's
						// provider; no workspaceDownloadBucket check is needed.
						agent.AppliedConfig.WorkspaceStorageBucket = stor.Bucket()
						if err := s.store.UpdateAgent(detachLaunchFromClient(ctx), agent); err != nil {
							s.agentLifecycleLog.Warn("Failed to update agent with workspace storage path", "agent_id", agent.ID, "error", err)
						}
					}
				}
			}
		}
		ucancel()
	}

	// Managed agent path: bypass broker dispatch entirely and handle directly.
	if req.Profile == ManagedAgentsProfile {
		logAttrs := []any{"agent_id", agent.ID, "agent", agent.Name, "elapsed", time.Since(hubCreateStart).String()}
		if parentRole != "" {
			logAttrs = append(logAttrs, "parent_agent_role", string(parentRole))
		}
		s.agentLifecycleLog.Info("Hub: managed agent create (hub-direct)",
			logAttrs...)

		task := ""
		if agent.AppliedConfig != nil {
			task = agent.AppliedConfig.Task
		}
		if err := s.managedAgentCreate(ctx, agent, task); err != nil {
			// managedAgentCreate mints no agent credential: nothing to revoke.
			corrID := cleanup(createRollback{Stage: createStageManaged, Cause: err, DeleteRuntime: func(cctx context.Context) error {
				return s.managedAgentDelete(cctx, agent)
			}})
			writeCreateFailure(w, corrID, func() { RuntimeError(w, "Failed to create managed agent: "+err.Error()) })
			return
		}

		agent.Phase = string(state.PhaseRunning)
		if task == "" {
			agent.Activity = "waiting_for_input"
		} else {
			agent.Activity = "working"
		}
		recorded := true
		if err := s.store.UpdateAgent(ctx, agent); err != nil {
			recorded = false
			s.agentLifecycleLog.Warn("Failed to update managed agent after create", "agent_id", agent.ID, "error", err)
		}

		// A delete that won the race answers 409 with no agent body, as
		// the synchronous broker create does (ptone/scion#3099,
		// ptone/scion#3454).
		if !s.publishAgentCreatedIfLive(ctx, agent) {
			s.agentLifecycleLog.Info("Hub: managed agent was deleted while it was being created; answering 409",
				"agent_id", agent.ID, "agent", agent.Name)
			writeDeletedDuringCreate(w, agent.ID, s.compensateManagedCreate(ctx, agent, recorded))
			return
		}
		s.enrichAgent(ctx, agent, project, nil)

		writeJSON(w, http.StatusCreated, CreateAgentResponse{
			Agent: redactedAgentCopy(ctx, s, agent),
		})
		return
	}

	// Dispatch to runtime broker if available.
	// Unless provision-only is requested, do a full create+start via DispatchAgentCreate.
	// Otherwise provision only — set up dirs, worktree, templates without launching the container.
	preDispatchAttrs := []any{"agent_id", agent.ID, "agent", agent.Name, "elapsed", time.Since(hubCreateStart).String()}
	if parentRole != "" {
		preDispatchAttrs = append(preDispatchAttrs, "parent_agent_role", string(parentRole))
	}
	s.agentLifecycleLog.Info("Hub: pre-dispatch setup complete",
		preDispatchAttrs...)
	// From here the launch no longer follows the client (ptone/scion#1961):
	// a client that gives up mid-dispatch must not cancel the broker launch,
	// the post-dispatch phase writes, or a real failure's rollback. Each
	// dispatch below is bounded by syncDispatch instead.
	ctx = detachLaunchFromClient(ctx)
	// acceptedLaunch is set when the broker accepted the create for
	// asynchronous launch; the launch then reports back to the hub, which
	// handles a delete that won the race (see compensateLandedRun).
	acceptedLaunch := false
	if dispatcher := s.GetDispatcher(); dispatcher != nil {
		// A create is a start, unless it only provisions. A create-and-start
		// runs under a start claim (createUnderClaim), which records run
		// intent running; a provision-only create records stopped.
		if req.ProvisionOnly {
			if _, err := s.recordRunIntent(ctx, agent, store.RunIntentStopped); err != nil {
				corrID := cleanup(createRollback{Stage: createStageRunIntent, Cause: err})
				writeCreateFailure(w, corrID, func() { writeRunIntentError(w, err, agent.ID) })
				return
			}
		}
		if !req.ProvisionOnly {
			// Use env-gather dispatch if requested
			if req.GatherEnv {
				s.agentLifecycleLog.Debug("Hub: env-gather requested, using DispatchAgentCreateWithGather",
					"agent_id", agent.ID,
					"agent", agent.Name, "broker", agent.RuntimeBrokerID)
				// The create runs under its start claim; the dispatch is bounded
				// by syncDispatch, derived from the claim's context.
				created, err := s.createUnderClaim(ctx, agent, func(ctx context.Context) (out *CreateDispatchResult, err error) {
					err = syncDispatch(ctx, func(dctx context.Context) error {
						out, err = dispatcher.DispatchAgentCreateWithGather(dctx, agent)
						return err
					})
					return out, err
				})
				envReqs := created.EnvRequirements()
				if errors.Is(err, ErrLaunchInvalidPhase) {
					// A stop or delete reached the record before the launch
					// began. Nothing was sent to the broker; the record is
					// left to that operation.
					writeLaunchInvalidPhase(w, err, agent.ID)
					return
				} else if errors.Is(err, errStartClaimWrite) {
					// The start claim (this create's run-intent write)
					// failed, or a delete holds the row: nothing was
					// dispatched. Rolled back as a failed intent write.
					corrID := cleanup(createRollback{Stage: createStageRunIntent, Cause: err})
					writeCreateFailure(w, corrID, func() { writeRunIntentError(w, err, agent.ID) })
					return
				} else if !errors.Is(err, store.ErrDeleteInProgress) && s.writeStartClaimError(ctx, w, err, agent.ID) {
					// Refused by the start claim before dispatch (held, or
					// not eligible), or the claim was lost while the create
					// ran: a stop superseded it, and whatever was
					// dispatched now belongs to that stop and the
					// start-claim reaper. Either way the record is kept,
					// not cleaned up as a failed create. A delete that
					// claimed the row during the dispatch is a dispatch
					// failure, cleaned up below.
					return
				} else if err != nil {
					// Dispatch failed — clean up provisioned files on the broker
					// and delete the agent record so orphaned local files don't
					// trigger spurious sync-registration attempts. No revoke here:
					// DispatchAgentCreateWithGather already revoked any credential
					// it minted on this error return.
					corrID := cleanup(createRollback{Stage: createStageDispatchEnvGather, Cause: err, DeleteRuntime: dispatchDeleteFailedCreate(s.store, dispatcher, agent)})
					writeCreateFailure(w, corrID, func() { dispatchCreateErrorResponse(w, err, agent.ID) })
					return
				} else if created.AcceptedLaunch() != nil {
					// Accepted for asynchronous launch: the row is already
					// provisioning; persist only the non-status fields.
					acceptedLaunch = true
					warnings = append(warnings, s.adoptAcceptedLaunch(ctx, agent)...)
				} else if envReqs != nil {
					// Broker returned 202: needs env gather
					agent.Phase = string(state.PhaseProvisioning)
					if err := s.updateAgentAfterDispatch(ctx, agent); err != nil {
						s.agentLifecycleLog.Warn("Failed to update agent phase for env-gather", "agent_id", agent.ID, "error", err)
					}

					// A delete that won the race answers 409, as the
					// final publish below does (ptone/scion#3099): no
					// env should be gathered for a deleted agent.
					if !s.publishAgentCreatedIfLive(ctx, agent) {
						s.agentLifecycleLog.Info("Hub: agent was deleted while it was being created; answering 409",
							"agent_id", agent.ID, "agent", agent.Name)
						writeDeletedDuringCreate(w, agent.ID, dispatchWarns.Warnings())
						return
					}

					s.enrichAgent(ctx, agent, project, nil)
					hubEnvGather := s.buildEnvGatherResponse(ctx, agent, envReqs)

					writeJSON(w, http.StatusAccepted, CreateAgentResponse{
						Agent:     redactedAgentCopy(ctx, s, agent),
						Warnings:  append(warnings, dispatchWarns.Warnings()...),
						EnvGather: hubEnvGather,
					})
					return
				} else {
					if !s.preserveTerminalPhase(ctx, agent) {
						if agent.Phase == string(state.PhaseCreated) {
							agent.Phase = string(state.PhaseProvisioning)
						}
						if err := s.updateAgentAfterDispatch(ctx, agent); err != nil {
							warnings = append(warnings, "Failed to update agent phase: "+err.Error())
						}
					}
				}
			} else {
				// The create runs under its start claim; the dispatch is bounded
				// by syncDispatch, derived from the claim's context.
				created, err := s.createUnderClaim(ctx, agent, func(ctx context.Context) (out *CreateDispatchResult, err error) {
					err = syncDispatch(ctx, func(dctx context.Context) error {
						out, err = dispatcher.DispatchAgentCreateWithGather(dctx, agent)
						return err
					})
					return out, err
				})
				envReqs := created.EnvRequirements()
				if errors.Is(err, ErrLaunchInvalidPhase) {
					// A stop or delete reached the record before the launch
					// began. Nothing was sent to the broker; the record is
					// left to that operation.
					writeLaunchInvalidPhase(w, err, agent.ID)
					return
				} else if errors.Is(err, errStartClaimWrite) {
					// The start claim (this create's run-intent write)
					// failed, or a delete holds the row: nothing was
					// dispatched. Rolled back as a failed intent write.
					corrID := cleanup(createRollback{Stage: createStageRunIntent, Cause: err})
					writeCreateFailure(w, corrID, func() { writeRunIntentError(w, err, agent.ID) })
					return
				} else if !errors.Is(err, store.ErrDeleteInProgress) && s.writeStartClaimError(ctx, w, err, agent.ID) {
					// Refused by the start claim before dispatch (held, or
					// not eligible), or the claim was lost while the create
					// ran: a stop superseded it, and whatever was
					// dispatched now belongs to that stop and the
					// start-claim reaper. Either way the record is kept,
					// not cleaned up as a failed create. A delete that
					// claimed the row during the dispatch is a dispatch
					// failure, cleaned up below.
					return
				} else if err != nil {
					// Dispatch failed — clean up provisioned files on the broker
					// and delete the agent record so orphaned local files don't
					// trigger spurious sync-registration attempts. No revoke here:
					// DispatchAgentCreateWithGather already revoked any credential
					// it minted on this error return.
					corrID := cleanup(createRollback{Stage: createStageDispatch, Cause: err, DeleteRuntime: dispatchDeleteFailedCreate(s.store, dispatcher, agent)})
					writeCreateFailure(w, corrID, func() { dispatchCreateErrorResponse(w, err, agent.ID) })
					return
				} else if created.AcceptedLaunch() != nil {
					// Accepted for asynchronous launch: the row is already
					// provisioning; persist only the non-status fields.
					acceptedLaunch = true
					warnings = append(warnings, s.adoptAcceptedLaunch(ctx, agent)...)
				} else if envReqs != nil && len(envReqs.Needs) > 0 {
					// Broker reported missing required env vars — fail the dispatch.
					// Clean up the provisioning agent and its files so orphaned
					// local state doesn't trigger spurious sync-registration.
					//
					// DispatchAgentCreateWithGather returned this as a value, not
					// an error, so its own revoke-on-failure defer did not fire —
					// the cleanup revokes the credential it minted instead
					// (RevokeCredentials), before the row is deleted
					// (ptone/scion#1956: a create that fails after the mint must
					// not leave the credential valid for its full TTL).
					corrID := cleanup(createRollback{Stage: createStageMissingEnv, Cause: errors.New("broker reported missing required environment variables"), RevokeCredentials: true, DeleteRuntime: dispatchDeleteFailedCreate(s.store, dispatcher, agent)})
					writeCreateFailure(w, corrID, func() { MissingEnvVars(w, envReqs.Needs, s.buildEnvGatherResponse(ctx, agent, envReqs)) })
					return
				} else {
					if !s.preserveTerminalPhase(ctx, agent) {
						if agent.Phase == string(state.PhaseCreated) {
							agent.Phase = string(state.PhaseProvisioning)
						}
						if err := s.updateAgentAfterDispatch(ctx, agent); err != nil {
							warnings = append(warnings, "Failed to update agent phase: "+err.Error())
						}
					}
				}
			}
		} else {
			// Provision-only: set up agent filesystem without starting
			if err := syncDispatch(ctx, func(dctx context.Context) error {
				return dispatcher.DispatchAgentProvision(dctx, agent)
			}); err != nil {
				if errors.Is(err, errAgentTokenRecord) {
					// The agent's token could not be recorded, so it was not
					// handed to the broker. Fail the create and roll it back,
					// as a full create does.
					s.agentLifecycleLog.Warn("Provision-only create failed: agent token not issued",
						"agent_id", agent.ID, "agent", agent.Name, "broker", agent.RuntimeBrokerID, "error", err)
					corrID := cleanup(createRollback{Stage: createStageProvision, Cause: err, DeleteRuntime: dispatchDeleteFailedCreate(s.store, dispatcher, agent)})
					writeCreateFailure(w, corrID, func() { dispatchCreateErrorResponse(w, err, agent.ID) })
					return
				}
				if isSkillResolutionDispatchError(err) {
					// A required skill could not be resolved, so the agent
					// can never start from this provision. Fail the create the
					// same way a full create does: remove the broker files and
					// the agent row, and relay the broker's status. No revoke
					// here: DispatchAgentProvision already revoked any
					// credential it minted on this error return. Other
					// provision failures stay warnings.
					s.agentLifecycleLog.Warn("Provision-only create failed: required skill could not be resolved",
						"agent_id", agent.ID, "agent", agent.Name, "broker", agent.RuntimeBrokerID, "error", err)
					corrID := cleanup(createRollback{Stage: createStageProvision, Cause: err, DeleteRuntime: dispatchDeleteFailedCreate(s.store, dispatcher, agent)})
					writeCreateFailure(w, corrID, func() { dispatchCreateErrorResponse(w, err, agent.ID) })
					return
				}
				warnings = append(warnings, api.ProvisionFailedWarningPrefix+err.Error())
			} else {
				agent.Phase = string(state.PhaseCreated)
				if err := s.updateAgentAfterDispatch(ctx, agent); err != nil {
					warnings = append(warnings, "Failed to update agent phase: "+err.Error())
				}
			}
		}
	}

	dispatchAttrs := []any{"agent_id", agent.ID, "agent", agent.Name, "totalElapsed", time.Since(hubCreateStart).String()}
	if parentRole != "" {
		dispatchAttrs = append(dispatchAttrs, "parent_agent_role", string(parentRole))
	}
	s.agentLifecycleLog.Info("Hub: dispatch complete",
		dispatchAttrs...)

	// Re-read the agent from the database before publishing the "created" event.
	// A concurrent status update (e.g. sciontool reporting a clone error) may have
	// changed the phase between our last UpdateAgent and now. Publishing the stale
	// in-memory object would send a "created" SSE event with the wrong phase,
	// and since the frontend may have already dropped the earlier "status" event
	// (it ignores status events for agents not yet in state), the UI would never
	// reflect the error.
	// A delete that claimed the row meanwhile suppresses it (ptone/scion#2972).
	//
	// A synchronous create that lost to a delete answers 409
	// delete_in_progress rather than 201 (ptone/scion#3099): the row is gone,
	// soft-deleted or held by a delete, so the agent was not created. The
	// dispatch has already run the compensating delete of a run that landed
	// (compensateLandedRun); its outcome is in the dispatch warnings.
	if !s.publishAgentCreatedIfLive(ctx, agent) && !acceptedLaunch {
		s.agentLifecycleLog.Info("Hub: agent was deleted while it was being created; answering 409",
			"agent_id", agent.ID, "agent", agent.Name)
		writeDeletedDuringCreate(w, agent.ID, dispatchWarns.Warnings())
		return
	}

	// Enrich agent with project and broker names for display
	s.enrichAgent(ctx, agent, project, nil)

	writeJSON(w, http.StatusCreated, CreateAgentResponse{
		Agent:    redactedAgentCopy(ctx, s, agent),
		Warnings: append(warnings, dispatchWarns.Warnings()...),
	})
}

// adoptAcceptedLaunch persists an accepted asynchronous launch's dispatched
// copy (the accepted branch) and replaces *agent with the persisted
// row, so callers respond with the provisioning phase and the launch. It
// returns a warning when the write failed.
func (s *Server) adoptAcceptedLaunch(ctx context.Context, agent *store.Agent) []string {
	persisted, err := s.persistAcceptedLaunch(ctx, agent)
	if persisted != nil && persisted != agent {
		*agent = *persisted
	}
	if err != nil {
		s.agentLifecycleLog.Warn("Failed to persist agent after accepted launch", "agent_id", agent.ID, "error", err)
		return []string{"Failed to update agent after launch was accepted: " + err.Error()}
	}
	return nil
}

// launchInFlightInputsWarning is the Warnings entry for a request that found
// a launch already in flight and therefore did not apply its inputs.
const launchInFlightInputsWarning = "agent is already launching; request inputs were not applied"

// writeLaunchInvalidPhase answers a create, env submit or workspace finalize
// whose launch could not begin because the agent left the created and
// provisioning phases (for example it was stopped meanwhile). When a delete
// holds the row (err wraps store.ErrDeleteInProgress) it answers 409
// delete_in_progress instead (ptone/scion#2550).
func writeLaunchInvalidPhase(w http.ResponseWriter, err error, agentID string) {
	if refusal := deleteClaimedDuringDispatch(err, agentID); refusal != nil {
		refusal.write(w)
		return
	}
	writeError(w, http.StatusConflict, "invalid_state",
		"agent is no longer in a phase that can be launched (it may have been stopped or deleted)", nil)
}

// preserveTerminalPhase re-reads the agent from the database and, if a
// concurrent status update has moved the agent to a terminal phase (error or
// stopped), preserves that phase on the in-memory agent so the subsequent
// UpdateAgent call does not overwrite it with the broker-reported phase.
// This prevents a race where sciontool reports an error (e.g. git clone
// failure) while the broker dispatch is still in flight.
//
// It returns true when the row was soft-deleted while the dispatch was in
// flight. The caller then skips the post-dispatch write: the row belongs to
// the delete, and writing the in-memory agent would clear its DeletedAt.
func (s *Server) preserveTerminalPhase(ctx context.Context, agent *store.Agent) (softDeleted bool) {
	current, err := s.store.GetAgent(ctx, agent.ID)
	if err != nil {
		return false
	}
	// A soft-deleted row is left alone: adopting its StateVersion would let
	// the caller's write (zero DeletedAt in memory) win the CAS and clear
	// deleted_at.
	if !current.DeletedAt.IsZero() {
		return true
	}
	p := state.Phase(current.Phase)
	if p == state.PhaseError || p == state.PhaseStopped {
		agent.Phase = current.Phase
		agent.Activity = current.Activity
		agent.Message = current.Message
		agent.StateVersion = current.StateVersion
	}
	return false
}

func (s *Server) updateAgentAfterDispatch(ctx context.Context, agent *store.Agent) error {
	// One retry is intentional here: we only need to recover the common case
	// where a single concurrent status update bumps StateVersion while dispatch
	// is in flight. If a second write wins the race too, return the conflict to
	// the caller rather than spinning in a longer CAS loop inside the request.
	err := s.store.UpdateAgent(ctx, agent)
	if err == nil || !errors.Is(err, store.ErrVersionConflict) {
		return err
	}

	latest, getErr := s.store.GetAgent(ctx, agent.ID)
	if getErr != nil {
		return getErr
	}
	if !latest.DeletedAt.IsZero() {
		// Soft-deleted while the dispatch was in flight: the row belongs to
		// the delete, so the dispatch result is not written.
		return nil
	}

	mergeDispatchedAgent(latest, agent)
	return s.store.UpdateAgent(ctx, latest)
}

func mergeDispatchedAgent(dst, src *store.Agent) {
	if src.Template != "" {
		dst.Template = src.Template
	}
	if src.Image != "" {
		dst.Image = src.Image
	}
	if src.Runtime != "" {
		dst.Runtime = src.Runtime
	}
	if src.AppliedConfig != nil {
		dst.AppliedConfig = src.AppliedConfig
	}
	if src.Message != "" {
		dst.Message = src.Message
	}
	if src.TaskSummary != "" {
		dst.TaskSummary = src.TaskSummary
	}

	if isTerminalAgentPhase(dst.Phase) {
		return
	}
	// A delete holds the re-read row (design ptone/scion#2483 §2.1: phase
	// writers outside UpdateAgentStatus respect the deletion predicate). Its
	// claim owns the status fields: copying the dispatch's phase would move
	// a deleting row on, e.g. created to running, or overwrite the claim's
	// stopping (ptone/scion#3055). The delete engine works from its claim
	// snapshot (broker, run ID), and the claim's own state_version bump is
	// what sends a pre-claim dispatch write here, so skipping the status
	// fields is enough.
	if deleteStopNoop(dst) {
		return
	}
	if src.Phase != "" {
		dst.Phase = src.Phase
	}
	if src.Activity != "" {
		dst.Activity = src.Activity
	}
	if src.ContainerStatus != "" {
		dst.ContainerStatus = src.ContainerStatus
	}
	if src.RuntimeState != "" {
		dst.RuntimeState = src.RuntimeState
	}
	// A resume that hit a version conflict re-reads the row as dst and
	// retries with src (the in-memory agent the dispatch already ran
	// against, including the caller's clear of a stale exit reason/code
	// from the prior generation). Carry that clear through for a running
	// resume, the same as the other running-phase fields above — otherwise
	// the retry's full-row write would keep dst's stale values instead.
	if src.Phase == string(state.PhaseRunning) {
		dst.ExitReason = src.ExitReason
		dst.ExitCode = src.ExitCode
		// Likewise the caller's clear of the prior generation's message
		// and stalled marker (empty values included), so the retry writes
		// what the first attempt would have (ptone/scion#2014).
		dst.Message = src.Message
		dst.StalledFromActivity = src.StalledFromActivity
	}
}

func isTerminalAgentPhase(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseStopped, state.PhaseError:
		return true
	case state.PhaseCreated,
		state.PhaseProvisioning,
		state.PhaseCloning,
		state.PhaseStarting,
		state.PhaseRunning,
		state.PhaseStopping:
		return false
	default:
		return false
	}
}

// buildEnvGatherResponse converts a broker's env requirements into the Hub-level
// response format, enriching it with scope information from the dispatcher.
func (s *Server) buildEnvGatherResponse(ctx context.Context, agent *store.Agent, brokerReqs *RemoteEnvRequirementsResponse) *EnvGatherResponse {
	// TZ is never gathered: never forward it to the CLI as a need, even if
	// an older broker reported one the dispatcher did not already remove.
	takeTZGatherNeed(brokerReqs)
	resp := &EnvGatherResponse{
		AgentID:   agent.ID,
		Required:  brokerReqs.Required,
		BrokerHas: brokerReqs.BrokerHas,
		Needs:     brokerReqs.Needs,
	}

	// Build hubHas with scope info
	// Try to determine the scope for each key the Hub provided
	for _, key := range brokerReqs.HubHas {
		if key == agentTZEnvKey {
			// TZ is labelled with the rung of the agent TZ chain.
			resp.HubHas = append(resp.HubHas, EnvSource{Key: key, Scope: s.agentTZ(ctx, agent).Source})
			continue
		}
		source := EnvSource{Key: key, Scope: "hub"}

		// Check if we can determine a more specific scope
		if agent.OwnerID != "" {
			vars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "user", ScopeID: agent.OwnerID, Key: key})
			if err == nil && len(vars) > 0 {
				source.Scope = "user"
			}
		}
		if source.Scope == "hub" && agent.ProjectID != "" {
			vars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "project", ScopeID: agent.ProjectID, Key: key})
			if err == nil && len(vars) > 0 {
				source.Scope = "project"
			}
		}
		if source.Scope == "hub" {
			// Check if it came from config
			if agent.AppliedConfig != nil {
				if _, ok := agent.AppliedConfig.Env[key]; ok {
					source.Scope = "config"
				}
			}
		}
		if source.Scope == "hub" && s.secretBackend != nil {
			if agent.OwnerID != "" {
				metas, err := s.secretBackend.List(ctx, secret.Filter{
					Scope: "user", ScopeID: agent.OwnerID, Name: key,
				})
				if err == nil && len(metas) > 0 {
					source.Scope = "secret"
				}
			}
			if source.Scope == "hub" && agent.ProjectID != "" {
				metas, err := s.secretBackend.List(ctx, secret.Filter{
					Scope: "project", ScopeID: agent.ProjectID, Name: key,
				})
				if err == nil && len(metas) > 0 {
					source.Scope = "secret"
				}
			}
		}
		resp.HubHas = append(resp.HubHas, source)
	}

	// Relay SecretInfo from broker
	if len(brokerReqs.SecretInfo) > 0 {
		resp.SecretInfo = make(map[string]SecretKeyInfo, len(brokerReqs.SecretInfo))
		for k, v := range brokerReqs.SecretInfo {
			resp.SecretInfo[k] = SecretKeyInfo{
				Description: v.Description,
				Source:      v.Source,
				Type:        v.Type,
			}
		}
	}

	// Cross-check: for each key the broker says it "needs", check whether the
	// Hub actually has it in storage (env_vars table or secret backend).  If
	// found, this indicates a resolution mismatch — the dispatch should have
	// included it but didn't.
	for _, key := range brokerReqs.Needs {
		// Check env_vars table
		if agent.OwnerID != "" {
			vars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "user", ScopeID: agent.OwnerID, Key: key})
			if err == nil && len(vars) > 0 {
				resp.HubWarnings = append(resp.HubWarnings,
					fmt.Sprintf("%s is stored in Hub env storage (user scope) but was not included in the dispatch — this may indicate a resolution issue", key))
				continue
			}
		}
		if agent.ProjectID != "" {
			vars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "project", ScopeID: agent.ProjectID, Key: key})
			if err == nil && len(vars) > 0 {
				resp.HubWarnings = append(resp.HubWarnings,
					fmt.Sprintf("%s is stored in Hub env storage (project scope) but was not included in the dispatch — this may indicate a resolution issue", key))
				continue
			}
		}
		// Check hub-scope env_vars
		if hubID := s.HubID(); hubID != "" {
			vars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "hub", ScopeID: hubID, Key: key})
			if err == nil && len(vars) > 0 {
				resp.HubWarnings = append(resp.HubWarnings,
					fmt.Sprintf("%s is stored in Hub env storage (hub scope) but was not included in the dispatch — injection_mode may be as_needed", key))
				continue
			}
		}
		// Check secret backend
		if s.secretBackend != nil {
			if agent.OwnerID != "" {
				metas, err := s.secretBackend.List(ctx, secret.Filter{Scope: "user", ScopeID: agent.OwnerID, Name: key})
				if err == nil && len(metas) > 0 {
					resp.HubWarnings = append(resp.HubWarnings,
						fmt.Sprintf("%s is stored in Hub secrets (user scope) but was not included in the dispatch — this may indicate a resolution issue", key))
					continue
				}
			}
			if agent.ProjectID != "" {
				metas, err := s.secretBackend.List(ctx, secret.Filter{Scope: "project", ScopeID: agent.ProjectID, Name: key})
				if err == nil && len(metas) > 0 {
					resp.HubWarnings = append(resp.HubWarnings,
						fmt.Sprintf("%s is stored in Hub secrets (project scope) but was not included in the dispatch — this may indicate a resolution issue", key))
					continue
				}
			}
			// Check hub-scope secrets
			if hubID := s.HubID(); hubID != "" {
				metas, err := s.secretBackend.List(ctx, secret.Filter{Scope: "hub", ScopeID: hubID, Name: key})
				if err == nil && len(metas) > 0 {
					resp.HubWarnings = append(resp.HubWarnings,
						fmt.Sprintf("%s is stored in Hub secrets (hub scope) but was not included in the dispatch — injection_mode may be as_needed", key))
					continue
				}
			}
		}
	}

	return resp
}

// submitAgentEnv handles POST /api/v1/projects/{projectId}/agents/{agentId}/env
// CLI submits gathered env vars after receiving a 202 env-gather response.
func (s *Server) submitAgentEnv(w http.ResponseWriter, r *http.Request, projectID, agentID string) {
	ctx := r.Context()

	var req SubmitEnvRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if len(req.Env) == 0 {
		ValidationError(w, "env map is required and must not be empty", nil)
		return
	}

	for key := range req.Env {
		if secret.IsReservedEnvTarget(key) {
			ValidationError(w, "target is reserved for scion's own control-plane environment variables", map[string]interface{}{
				"field": "target",
				"value": key,
			})
			return
		}
	}

	// Resolve agent
	agent, err := s.store.GetAgentBySlug(ctx, projectID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if agent.ProjectID != projectID {
				NotFound(w, "Agent")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// A launch is already in flight (for example a duplicate submit): answer
	// with the current agent and do not dispatch again.
	if agent.IsInFlight() {
		project, _ := s.store.GetProject(ctx, projectID)
		s.enrichAgent(ctx, agent, project, nil)
		writeJSON(w, http.StatusOK, CreateAgentResponse{
			Agent:    redactedAgentCopy(ctx, s, agent),
			Warnings: []string{launchInFlightInputsWarning},
		})
		return
	}

	// Verify agent is in a state that expects env submission
	if agent.Phase != string(state.PhaseProvisioning) && agent.Phase != string(state.PhaseCreated) {
		writeError(w, http.StatusConflict, "invalid_state",
			fmt.Sprintf("agent is in '%s' phase; env submission only valid during provisioning", agent.Phase), nil)
		return
	}

	// Dispatch finalize-env to the broker
	dispatcher := s.GetDispatcher()
	if dispatcher == nil || agent.RuntimeBrokerID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"cannot finalize env: no runtime broker available", nil)
		return
	}

	// From here the launch no longer follows the client (ptone/scion#1961):
	// the CLI creates with GatherEnv, so this submit is often where the
	// launch actually runs. The dispatch is bounded by syncDispatch.
	ctx = detachLaunchFromClient(ctx)
	ctx, dispatchWarns := withDispatchWarnings(ctx)
	// The finalize starts the agent: it runs under a start claim, its
	// dispatch bounded by syncDispatch, derived from the claim's context.
	finalized, err := s.createUnderClaim(ctx, agent, func(ctx context.Context) (out *CreateDispatchResult, err error) {
		err = syncDispatch(ctx, func(dctx context.Context) error {
			out, err = dispatcher.DispatchFinalizeEnv(dctx, agent, req.Env)
			return err
		})
		return out, err
	})
	if errors.Is(err, ErrLaunchInvalidPhase) {
		writeLaunchInvalidPhase(w, err, agent.ID)
		return
	}
	if s.writeStartClaimError(ctx, w, err, agent.ID) {
		return
	}
	if err != nil {
		if writeAgentTokenRecordError(w, err) {
			return
		}
		var stillMissing *ErrEnvStillMissing
		if errors.As(err, &stillMissing) {
			MissingEnvVars(w, stillMissing.Requirements.Needs,
				s.buildEnvGatherResponse(ctx, agent, stillMissing.Requirements))
			return
		}
		if ref := deleteClaimedDuringDispatch(err, agent.ID); ref != nil {
			ref.write(w)
			return
		}
		// finalize-env creates the agent on the broker, so it can meet the
		// same workspace-bucket refusal as create (ptone/scion#3422).
		if relayWorkspaceStorageUnconfigured(w, err) {
			return
		}
		RuntimeError(w, "Failed to finalize env on runtime broker: "+err.Error())
		return
	}

	var warnings []string
	if finalized.AcceptedLaunch() != nil {
		// Accepted for asynchronous launch: the launch reports move the
		// phase on; do not write running here.
		warnings = s.adoptAcceptedLaunch(ctx, agent)
	} else {
		// A delete that won after the broker run landed answers 409, as
		// the synchronous create does (ptone/scion#3099,
		// ptone/scion#3518). The dispatch has already run the compensating
		// delete of the landed run; its outcome is in the dispatch
		// warnings. An accepted launch is settled by its launch report.
		if s.deleteWonAfterLanding(ctx, agent.ID) {
			s.agentLifecycleLog.Info("Hub: agent was deleted while its env submit launched it; answering 409",
				"agent_id", agent.ID, "agent", agent.Name)
			writeDeletedDuringCreate(w, agent.ID, dispatchWarns.Warnings())
			return
		}
		// Update agent phase from broker response
		if agent.Phase == string(state.PhaseProvisioning) || agent.Phase == string(state.PhaseCreated) {
			agent.Phase = string(state.PhaseRunning)
		}
		if err := s.updateAgentAfterDispatch(ctx, agent); err != nil {
			s.agentLifecycleLog.Warn("Failed to update agent phase after env submit", "agent_id", agent.ID, "error", err)
		}
	}

	// Enrich and return
	project, _ := s.store.GetProject(ctx, projectID)
	s.enrichAgent(ctx, agent, project, nil)

	writeJSON(w, http.StatusOK, CreateAgentResponse{
		Agent:    redactedAgentCopy(ctx, s, agent),
		Warnings: append(dispatchWarns.Warnings(), warnings...),
	})
}

// resolveAgentRuntime returns the runtime type of the broker profile the
// agent actually applied, matched by name against the broker's advertised
// profiles (ptone/scion#2262). Display-time enrichment previously defaulted
// to the type of the first *available* profile on the broker, which is
// wrong whenever a broker advertises more than one profile type (e.g.
// docker and kubernetes): an agent dispatched to the kubernetes profile
// could be shown as docker just because docker sorted first.
//
// If the agent's applied profile is unknown, or doesn't match any profile
// name the broker advertises, this falls back to the broker's profile type
// only when every advertised profile shares that same non-empty type — that
// case is unambiguous regardless of which profile the agent used. If the
// broker advertises more than one type, resolveAgentRuntime returns ""
// rather than guessing from an unrelated profile.
func resolveAgentRuntime(agent *store.Agent, broker *store.RuntimeBroker) string {
	if agent == nil || broker == nil {
		return ""
	}
	if agent.AppliedConfig != nil && agent.AppliedConfig.Profile != "" {
		for _, p := range broker.Profiles {
			if p.Name == agent.AppliedConfig.Profile {
				return p.Type
			}
		}
	}
	return commonProfileType(broker.Profiles)
}

// commonProfileType returns the shared type of profiles, or "" if profiles
// is empty or its entries don't all share the same non-empty type.
func commonProfileType(profiles []store.BrokerProfile) string {
	var t string
	for _, p := range profiles {
		if p.Type == "" {
			return ""
		}
		if t == "" {
			t = p.Type
		} else if t != p.Type {
			return ""
		}
	}
	return t
}

// enrichAgents populates Project and RuntimeBrokerName fields for a slice of agents.
// This provides human-readable names from the related IDs for display purposes.
func (s *Server) enrichAgents(ctx context.Context, agents []store.Agent) {
	if len(agents) == 0 {
		return
	}
	defer perfPhaseStart(ctx, perfPhaseEnrich)()

	// Collect unique project, broker, and template IDs
	projectIDs := make(map[string]struct{})
	brokerIDs := make(map[string]struct{})
	templateIDs := make(map[string]struct{})
	for _, a := range agents {
		if a.ProjectID != "" {
			projectIDs[a.ProjectID] = struct{}{}
		}
		if a.RuntimeBrokerID != "" {
			brokerIDs[a.RuntimeBrokerID] = struct{}{}
		}
		if a.AppliedConfig != nil && a.AppliedConfig.TemplateID != "" {
			templateIDs[a.AppliedConfig.TemplateID] = struct{}{}
		}
	}

	// Fetch projects
	projectNames := make(map[string]string)
	for id := range projectIDs {
		if project, err := s.store.GetProject(ctx, id); err == nil {
			projectNames[id] = project.Name
		}
	}

	// Fetch brokers
	brokerInfo := make(map[string]*store.RuntimeBroker)
	for id := range brokerIDs {
		if broker, err := s.store.GetRuntimeBroker(ctx, id); err == nil {
			brokerInfo[id] = broker
		}
	}

	// Fetch templates for slug enrichment
	templateSlugs := make(map[string]string)
	for id := range templateIDs {
		if tmpl, err := s.store.GetTemplate(ctx, id); err == nil && tmpl.Slug != "" {
			templateSlugs[id] = tmpl.Slug
		}
	}

	// Enrich agents
	now := time.Now()
	// Evaluated once for the whole list: the deletion detail fields are
	// for platform admins only (ptone/scion#3122). Every list builder and
	// the compact view read the items built here, so the redaction is
	// upstream of toCompact.
	seesDeletionDetail := callerSeesDeletionDetail(ctx)
	// The `suspension` view (ptone/scion#3433), one hold read per project.
	suspensions := s.agentSuspensionViews(ctx, agents)
	for i := range agents {
		agents[i].Suspension = suspensions[agents[i].ID]
		// The client-facing `launch` view (design §3.2), computed fresh per response.
		agents[i].Launch = store.ComputeAgentLaunch(&agents[i], now)
		// The client-facing `deletion` view (design ptone/scion#2483 §2.2).
		agents[i].Deletion = deletionViewForCaller(&agents[i], now, seesDeletionDetail)
		agents[i].ProvisionedOnly = store.ComputeAgentProvisionedOnly(&agents[i])
		// Populate harness config from applied config
		if agents[i].HarnessConfig == "" && agents[i].AppliedConfig != nil && agents[i].AppliedConfig.HarnessConfig != "" {
			agents[i].HarnessConfig = agents[i].AppliedConfig.HarnessConfig
		}
		if name, ok := projectNames[agents[i].ProjectID]; ok {
			agents[i].Project = name
		}
		if broker, ok := brokerInfo[agents[i].RuntimeBrokerID]; ok {
			agents[i].RuntimeBrokerName = broker.Name
			// Populate Runtime from the agent's own applied profile if not
			// already set.
			if agents[i].Runtime == "" {
				if rt := resolveAgentRuntime(&agents[i], broker); rt != "" {
					agents[i].Runtime = rt
				}
			}
		}
		// Enrich template slug from TemplateID if Template is a UUID or empty
		if agents[i].AppliedConfig != nil && agents[i].AppliedConfig.TemplateID != "" {
			if slug, ok := templateSlugs[agents[i].AppliedConfig.TemplateID]; ok {
				agents[i].Template = slug
			}
		}
	}
}

// enrichAgent populates Project and RuntimeBrokerName fields for a single agent.
// project and broker parameters are optional pre-fetched values to avoid redundant lookups.
func (s *Server) enrichAgent(ctx context.Context, agent *store.Agent, project *store.Project, broker *store.RuntimeBroker) {
	if agent == nil {
		return
	}

	// The client-facing `launch` view (design §3.2), computed fresh at
	// response time so remainingSeconds reflects "now", not whenever the row
	// was last written.
	now := time.Now()
	agent.Launch = store.ComputeAgentLaunch(agent, now)
	// The client-facing `deletion` view (design ptone/scion#2483 §2.2),
	// with its detail fields for platform admins only (ptone/scion#3122).
	agent.Deletion = deletionViewForCaller(agent, now, callerSeesDeletionDetail(ctx))
	agent.ProvisionedOnly = store.ComputeAgentProvisionedOnly(agent)
	// The `suspension` view (ptone/scion#3433): set while the agent is held.
	agent.Suspension = s.agentSuspensionView(ctx, agent.ID)

	// Populate harness config and auth from applied config
	if agent.AppliedConfig != nil {
		if agent.HarnessConfig == "" && agent.AppliedConfig.HarnessConfig != "" {
			agent.HarnessConfig = agent.AppliedConfig.HarnessConfig
		}
		if agent.HarnessAuth == "" && agent.AppliedConfig.HarnessAuth != "" {
			agent.HarnessAuth = agent.AppliedConfig.HarnessAuth
		}
	}

	// Populate project name
	if project != nil {
		agent.Project = project.Name
	} else if agent.ProjectID != "" {
		if g, err := s.store.GetProject(ctx, agent.ProjectID); err == nil {
			agent.Project = g.Name
		}
	}

	// Populate broker info
	if broker != nil {
		agent.RuntimeBrokerName = broker.Name
		if agent.Runtime == "" {
			if rt := resolveAgentRuntime(agent, broker); rt != "" {
				agent.Runtime = rt
			}
		}
	} else if agent.RuntimeBrokerID != "" {
		b, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
		if err != nil {
			s.agentLifecycleLog.Debug("failed to get runtime broker for enrichment", "agent_id", agent.ID, "brokerID", agent.RuntimeBrokerID, "error", err)
		} else {
			agent.RuntimeBrokerName = b.Name
			s.agentLifecycleLog.Debug("enriched agent with broker name", "agent_id", agent.ID, "slug", agent.Slug, "brokerName", b.Name)
			if agent.Runtime == "" {
				if rt := resolveAgentRuntime(agent, b); rt != "" {
					agent.Runtime = rt
				}
			}
		}
	}

	// Enrich template slug from TemplateID
	if agent.AppliedConfig != nil && agent.AppliedConfig.TemplateID != "" {
		if tmpl, err := s.store.GetTemplate(ctx, agent.AppliedConfig.TemplateID); err == nil && tmpl.Slug != "" {
			agent.Template = tmpl.Slug
		}
	}
}

func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	// routeGuard resolves the agent sub-route once and stores it in the
	// request context; dispatch switches on that value only.
	route, ok := requireAgentSubRoute(w, r)
	if !ok {
		return
	}
	id := route.AgentID

	switch route.RouteID {
	case AgentRouteStopAll:
		s.handleStopAllAgents(w, r, "")

	case AgentRoutePTY:
		// PTY connections (WebSocket upgrade and auth preflight). The
		// handler distinguishes preflight from upgrade by its headers.
		s.handleAgentPTY(w, r)

	case AgentRouteWorkspace:
		// Workspace routes (GET for status and POST for sync operations)
		// require user authentication.
		if GetUserIdentityFromContext(r.Context()) == nil {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "This action requires user authentication", nil)
			return
		}
		workspaceAction, ok := route.Suffix.DecodedOpaque()
		if !ok {
			NotFound(w, "Workspace action")
			return
		}
		s.handleWorkspaceRoutes(w, r, id, strings.TrimPrefix(workspaceAction, "/"))

	case AgentRouteGroups:
		s.handleAgentGroups(w, r, id)

	case AgentRoutePorts:
		s.handleAgentPorts(w, r, id, "")
	case AgentRoutePortsTunnel:
		s.handleAgentPorts(w, r, id, "tunnel")
	case AgentRoutePortItem:
		s.handleAgentPorts(w, r, id, route.Suffix.Param)
	case AgentRoutePortProxy:
		subpath, ok := route.Suffix.DecodedOpaque()
		if !ok {
			NotFound(w, "Port")
			return
		}
		s.handleAgentPorts(w, r, id, route.Suffix.Param+"/proxy"+subpath)

	case AgentRouteLogs:
		// Agent logs relay (GET, proxied to broker).
		s.handleAgentLogs(w, r, id)
	case AgentRouteCloudLogs:
		s.handleAgentCloudLogs(w, r, id)
	case AgentRouteCloudLogsStream:
		s.handleAgentCloudLogsStream(w, r, id)
	case AgentRouteMessageLogs:
		s.handleAgentMessageLogs(w, r, id)
	case AgentRouteMessageLogsStream:
		s.handleAgentMessageLogsStream(w, r, id)

	case AgentRouteMessagesStream:
		// Per-agent messages (GET). Both the list and the real-time stream
		// are backed by the hub message store / event bus and work without
		// Cloud Logging being configured.
		s.handleAgentMessagesStream(w, r, id)
	case AgentRouteMessages:
		s.handleAgentMessages(w, r, id)

	case AgentRouteSecrets:
		// Agent-scoped secrets: /api/v1/agents/{id}/secrets[/{key}].
		key, ok := route.Suffix.DecodedOpaque()
		if !ok {
			NotFound(w, "Secret")
			return
		}
		s.handleAgentSecrets(w, r, id, key)

	case AgentRouteMetricsSummary:
		s.handleAgentMetricsSummary(w, r, id)

	case AgentRouteHoldLift:
		s.handleAgentHoldLift(w, r, id)

	case AgentRouteActionStatus:
		s.handleAgentAction(w, r, id, api.AgentActionStatus)
	case AgentRouteActionStart:
		s.handleAgentAction(w, r, id, api.AgentActionStart)
	case AgentRouteActionStop:
		s.handleAgentAction(w, r, id, api.AgentActionStop)
	case AgentRouteActionSuspend:
		s.handleAgentAction(w, r, id, api.AgentActionSuspend)
	case AgentRouteActionRestart:
		s.handleAgentAction(w, r, id, api.AgentActionRestart)
	case AgentRouteActionMessage:
		s.handleAgentAction(w, r, id, api.AgentActionMessage)
	case AgentRouteActionExec:
		s.handleAgentAction(w, r, id, api.AgentActionExec)
	case AgentRouteActionRestore:
		s.handleAgentAction(w, r, id, api.AgentActionRestore)
	case AgentRouteActionEnv:
		s.handleAgentAction(w, r, id, api.AgentActionEnv)
	case AgentRouteActionTokenRefresh:
		s.handleAgentAction(w, r, id, api.AgentActionTokenRefresh)
	case AgentRouteActionRefreshToken:
		s.handleAgentAction(w, r, id, api.AgentActionRefreshToken)
	case AgentRouteActionOutbound:
		s.handleAgentAction(w, r, id, api.AgentActionOutboundMessage)
	case AgentRouteActionMetrics:
		s.handleAgentAction(w, r, id, api.AgentActionMetrics)
	case AgentRouteActionMessageMode:
		s.handleAgentAction(w, r, id, api.AgentActionSetMessageMode)
	case AgentRouteActionReincarnate:
		s.handleAgentAction(w, r, id, api.AgentActionReincarnate)
	case AgentRouteActionResetAuth:
		s.handleAgentAction(w, r, id, api.AgentActionResetAuth)
	case AgentRouteActionKeys:
		s.handleAgentAction(w, r, id, api.AgentActionKeys)

	case AgentRouteRoot:
		switch r.Method {
		case http.MethodGet:
			s.getAgent(w, r, id)
		case http.MethodPatch:
			s.updateAgent(w, r, id)
		case http.MethodDelete:
			s.deleteAgent(w, r, id)
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
		}

	default:
		// A route resolved for another path form (or a row with no
		// dispatch here) is not served by this handler.
		NotFound(w, "Agent route")
	}
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, id string) {
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// If the caller is an agent, enforce project isolation.
	//
	// This check MUST run before s.authorize: cross-project access is answered
	// with 404 rather than 403 so the response does not disclose that the agent
	// exists. Authorizing first would turn that 404 into a 403 and leak
	// existence (design §4.2).
	isSelf := false
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agent.ProjectID != agentIdent.ProjectID() {
			writeAgentNotFound(w)
			return
		}
		isSelf = agentIdent.ID() == agent.ID
	}
	// CO1: agent.read carries no AgentScopes mapping, so an agent identity is
	// denied reading any *other* agent here, except an agent it directly
	// launched in its project (authz_launcher_read.go). An agent reading its
	// own record is exempt, matching getProjectAgent's self-read contract (see
	// TestReadEndpoint_ProjectScopedAgents_WithReadScope_Allowed): this is the
	// route the in-container CLI uses for `scion whoami --full` (GetSelf), and
	// both routes return the same writeAgentGetResponse body, so the exemption
	// exposes nothing the project route doesn't already. The project:read
	// scope check above still applies to self-reads.
	if !isSelf {
		if !s.authorizeSingleAgentRead(w, r, agent) {
			return
		}
	}

	s.writeAgentGetResponse(w, r, agent)
}

// writeAgentNotFound is the single-agent GET answer for an agent that
// does not exist. An agent caller gets the same answer for an agent it
// may not read, so the two cannot be told apart.
func writeAgentNotFound(w http.ResponseWriter) {
	writeErrorFromErr(w, store.ErrNotFound, "")
}

// authorizeSingleAgentRead is the agent.read gate shared by the
// single-agent GET routes (getAgent, getProjectAgent). The resource
// carries the agent record read from the store above, which the launcher
// status-read rule decides from (authz_launcher_read.go). A user caller's
// denial is the usual 403; an agent caller's denial is writeAgentNotFound.
func (s *Server) authorizeSingleAgentRead(w http.ResponseWriter, r *http.Request, agent *store.Agent) bool {
	ctx := r.Context()
	resource := agentStatusReadResource(agent)
	if GetAgentIdentityFromContext(ctx) == nil {
		return s.authorize(w, r, resource, ActionRead)
	}
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}
	decision := s.authzService.CheckAccess(ctx, identity, resource, ActionRead)
	if !decision.Allowed {
		logAuthzDenial(r, identity, resource, ActionRead, decision.Reason)
		// Existence timing may differ; accepted for agent-ID existence,
		// revisit if IDs become higher-value.
		writeAgentNotFound(w)
		return false
	}
	return true
}

// writeAgentGetResponse builds and writes the standard single-agent response
// body (capabilities, harness info, messageability, env redaction) shared by
// every route that returns a single agent. It performs no authorization --
// callers must gate before calling this (see getAgent and getProjectAgent
// above, which apply the same rule).
func (s *Server) writeAgentGetResponse(w http.ResponseWriter, r *http.Request, agent *store.Agent) {
	ctx := r.Context()

	// Show a TZ that an older hub persisted in the env records as the legacy
	// pin it becomes, so the configure page never round-trips it as an env
	// entry. In memory only: the next write persists the adoption.
	adoptLegacyTZ(agent.AppliedConfig)

	// Enrich agent with project and broker names
	s.enrichAgent(ctx, agent, nil, nil)
	resolvedHarness, harnessCaps := s.resolveAgentHarnessCapabilities(ctx, agent)

	// Compute capabilities for this agent
	resp := AgentWithCapabilities{
		Agent:               *agent,
		ResolvedHarness:     resolvedHarness,
		HarnessCapabilities: &harnessCaps,
		CloudLogging:        s.logQueryService != nil,
	}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, agentResource(agent))

		// Compute detailed messageability with reachable counts for the detail endpoint.
		projectAgentsResult, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: agent.ProjectID}, store.ListOptions{})
		if err == nil {
			resp.Messageability = s.ComputeMessageabilityDetail(ctx, identity, agent, projectAgentsResult.Items)
		} else {
			resp.Messageability = s.ComputeMessageability(ctx, identity, agent)
		}
	}

	resp.AppliedConfig = redactAppliedConfigEnvForResponse(resp.AppliedConfig, s.envViewAllowed(ctx, GetIdentityFromContext(ctx), agent, resp.Cap))

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	s.applyAgentUpdate(w, r, agent)
}

// applyAgentUpdate is the single code path behind every route that mutates a
// single agent's mutable fields (name/labels/annotations/taskSummary/config/
// GCP identity) -- the global PATCH /api/v1/agents/{id} and the
// project-scoped PATCH /api/v1/projects/{id}/agents/{agentId}. Both routes
// resolve their own *store.Agent and pass it in here, so both get the same
// authorization and the same field-level rules; the project-scoped route
// used to reimplement a smaller, unauthorized subset of this instead.
func (s *Server) applyAgentUpdate(w http.ResponseWriter, r *http.Request, agent *store.Agent) {
	ctx := r.Context()

	// This handler had no authorization of any kind before #591: any
	// authenticated caller could rename, relabel and rewrite the config of any
	// agent on the hub, including its GCP identity.
	if !s.authorize(w, r, agentResource(agent), ActionUpdate) {
		return
	}

	var updates struct {
		Name         string                 `json:"name,omitempty"`
		Labels       map[string]string      `json:"labels,omitempty"`
		Annotations  map[string]string      `json:"annotations,omitempty"`
		TaskSummary  string                 `json:"taskSummary,omitempty"`
		Config       *api.ScionConfig       `json:"config,omitempty"`
		GCPIdentity  *GCPIdentityAssignment `json:"gcp_identity,omitempty"`
		StateVersion int64                  `json:"stateVersion"`
		// ExplicitTimezone pins (an IANA zone name) or unpins ("") the
		// agent's container timezone. Absent leaves the pin unchanged. It
		// is accepted in any phase and applies at the next start.
		ExplicitTimezone *string `json:"explicitTimezone,omitempty"`
	}

	// The body is read into a buffer, rather than decoded straight off
	// r.Body via readJSON, because recordExplicitEdits (ptone/scion#2493)
	// needs a second, raw look at the same bytes: which keys the request's
	// "config" object actually contains, not just what updates.Config
	// decoded to (every ScionConfig field is `omitempty`, so an omitted key
	// and an explicit zero value are otherwise indistinguishable).
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
	}
	if err := json.Unmarshal(body, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// presentConfigKeys mirrors the keys present in the request's raw
	// "config" JSON object, for recordExplicitEdits' "present keys only"
	// rule (see its doc comment). Left nil (and therefore inert) when
	// updates.Config is nil or the raw object can't be recovered for
	// whatever reason -- recordExplicitEdits treats a nil/empty map as
	// "nothing present", which is always the safe direction here.
	//
	// Keys are lower-cased: encoding/json itself matches struct field names
	// case-insensitively when there is no exact match (so a non-canonical
	// caller's `"System_Prompt"` still decodes into cfg.SystemPrompt), and
	// the presence check must agree with that or a non-canonical-case
	// request would decode the field but be read as "absent" and silently
	// dropped from CreateInputs.
	var presentConfigKeys map[string]bool
	if updates.Config != nil {
		var rawTop struct {
			Config json.RawMessage `json:"config"`
		}
		if err := json.Unmarshal(body, &rawTop); err == nil && len(rawTop.Config) > 0 {
			var rawFields map[string]json.RawMessage
			if err := json.Unmarshal(rawTop.Config, &rawFields); err == nil {
				presentConfigKeys = make(map[string]bool, len(rawFields))
				for k := range rawFields {
					presentConfigKeys[strings.ToLower(k)] = true
				}
			}
		}
	}

	// Check version for optimistic locking
	if updates.StateVersion != 0 && updates.StateVersion != agent.StateVersion {
		Conflict(w, "Version conflict - resource was modified")
		return
	}

	if updates.ExplicitTimezone != nil {
		if !agent.DeletedAt.IsZero() {
			Conflict(w, "explicitTimezone cannot be updated for deleted agents")
			return
		}
		// Validate before any write so a bad zone changes nothing.
		if _, err := applyExplicitTimezoneEdit(&store.AgentAppliedConfig{}, *updates.ExplicitTimezone); err != nil {
			ValidationError(w, err.Error(), map[string]interface{}{"field": "explicitTimezone"})
			return
		}
	}

	// warnings are returned with the updated agent.
	var warnings []string

	// Adopt a TZ that an older hub persisted in the env records into
	// ExplicitTimezone before anything below reads or writes the agent's
	// TZ, and in particular before config.env's TZ is stripped.
	adoptLegacyTZ(agent.AppliedConfig)

	// Apply updates
	if updates.Name != "" {
		if _, err := api.ValidateDisplayName(updates.Name); err != nil {
			ValidationError(w, "Invalid name: "+err.Error(), nil)
			return
		}
		agent.Name = updates.Name
	}
	if updates.Labels != nil {
		if err := labels.Validate(updates.Labels); err != nil {
			ValidationError(w, "Invalid labels: "+err.Error(), nil)
			return
		}
		agent.Labels = updates.Labels
	}
	if updates.Annotations != nil {
		agent.Annotations = updates.Annotations
	}
	if updates.TaskSummary != "" {
		agent.TaskSummary = updates.TaskSummary
	}

	// Apply config updates (only allowed for non-deleted agents in 'created' or 'stopped' phase;
	// starting a stopped agent always recreates its container from AppliedConfig).
	if updates.Config != nil {
		if !agent.DeletedAt.IsZero() {
			Conflict(w, "Config cannot be updated for deleted agents")
			return
		}
		if agent.Phase != string(state.PhaseCreated) && agent.Phase != string(state.PhaseStopped) {
			Conflict(w, "Config can only be updated for agents in 'created' or 'stopped' phase")
			return
		}
		resolvedHarness, harnessCaps := s.resolveAgentHarnessCapabilities(ctx, agent)
		if issues := validateConfigAgainstHarnessCapabilities(updates.Config, harnessCaps); len(issues) > 0 {
			ValidationError(w, "Config contains unsupported fields for harness "+resolvedHarness, map[string]interface{}{
				"harness": resolvedHarness,
				"fields":  issues,
			})
			return
		}
		if agent.AppliedConfig == nil {
			agent.AppliedConfig = &store.AgentAppliedConfig{}
		}
		cfg := updates.Config
		if cfg.ThinkingLevel != nil {
			if tl := *cfg.ThinkingLevel; tl < 0 || tl > 100 {
				BadRequest(w, "thinking_level must be between 0 and 100")
				return
			}
		}
		if cfg.Model != "" {
			// Resolved here, ahead of both recordExplicitEdits and the live
			// write below, so both compare/store the same resolved value
			// the requester's alias (if any) maps to -- not the raw alias
			// they typed. This also ensures InlineConfig (set later)
			// carries the resolved value.
			cfg.Model = s.resolveModelAliasForAgent(ctx, agent, cfg.Model)
		}

		// old is a snapshot of the live config exactly as it stood before
		// any of the writes below, for recordExplicitEdits' diff (Option C,
		// ptone/scion#2493, invariant E). Taken after thinking-level
		// validation and model-alias resolution (pure reads) but before the
		// first assignment into agent.AppliedConfig itself.
		old := *agent.AppliedConfig

		// config.env never sets the agent timezone; explicitTimezone does.
		// The strip runs here, between the `old` snapshot above and the
		// recordExplicitEdits call below -- never after it. See
		// recordExplicitEdits' doc comment for why. An empty value is a
		// marker the configure page may round-trip, so it is dropped
		// silently.
		if v, ok := cfg.Env[agentTZEnvKey]; ok {
			delete(cfg.Env, agentTZEnvKey)
			if v != "" {
				warnings = append(warnings, configEnvTZIgnoredWarning)
			}
		}
		if agent.AppliedConfig.CreateInputs != nil {
			// canViewAgentEnv is the same attach-equivalent-access gate the
			// GET response's Env redaction uses (ResponseView). A caller who
			// fails it never saw the live Env to begin with, so their
			// request's env map cannot be trusted to list every key that
			// still exists live -- see recordExplicitEdits' env-removal gate.
			canAttachEnv := canViewAgentEnv(ctx, s, agent)
			recordExplicitEdits(agent.AppliedConfig.CreateInputs, &old, cfg, presentConfigKeys,
				dispatchImageRegistry(s.GetDispatcher()), canAttachEnv)
		}

		if cfg.Image != "" {
			agent.AppliedConfig.Image = cfg.Image
		}
		if cfg.Model != "" {
			agent.AppliedConfig.Model = cfg.Model
		}
		// Always apply thinking level from config (nil = explicit unset)
		agent.AppliedConfig.ThinkingLevel = cfg.ThinkingLevel
		if cfg.Task != "" {
			agent.AppliedConfig.Task = cfg.Task
		}
		if cfg.AuthSelectedType != "" {
			agent.AppliedConfig.HarnessAuth = cfg.AuthSelectedType
		}
		if cfg.Env != nil {
			// AppliedConfig.Env is a copy, so the auto-expose resolution
			// below never leaks a derived value into InlineConfig.Env.
			agent.AppliedConfig.Env = maps.Clone(cfg.Env)
			project, err := s.store.GetProject(ctx, agent.ProjectID)
			if err != nil {
				slog.WarnContext(ctx, "applyAgentUpdate: project lookup failed; auto-expose project tier not re-derived",
					"agent", agent.ID, "project", agent.ProjectID, "error", err)
				project = nil
			}
			applyPatchAutoExposeEnv(agent.AppliedConfig, &old, project, cfg.Env)
		}
		// Narrow carve-out, ptone/scion#2493 R3-1/R4-1 -- NOT part of
		// recordExplicitEdits/invariant E above, which has already run and
		// correctly left CreateInputs alone for whichever of these fields
		// were absent. This instead protects the LIVE InlineConfig value:
		// the configure page no longer echoes an untouched telemetry
		// control or an untouched env (R1-1, R2-1), so without this, the
		// unconditional wholesale InlineConfig replace just below would wipe
		// them -- an explicit telemetry opt-out, a project's env/telemetry
		// stamp from create, or (for a legacy agent with no CreateInputs) a
		// create-time explicit env key that `scion reincarnate` has no other
		// record of at all. See carryForwardAbsentPageOwnedFields' doc
		// comment for the field-by-field sweep. A present key (the user
		// actually touched that field) always wins via cfg as already
		// decoded; this only fills in a field the request left absent.
		carryForwardAbsentPageOwnedFields(cfg, &old, presentConfigKeys)
		dropEchoedInlineImage(cfg, &old, dispatchImageRegistry(s.GetDispatcher()))
		agent.AppliedConfig.InlineConfig = cfg
	}

	// Apply GCP identity update (only allowed for agents in 'created' phase)
	if updates.GCPIdentity != nil {
		if agent.Phase != string(state.PhaseCreated) {
			Conflict(w, "GCP identity can only be updated for agents in 'created' phase")
			return
		}
		if agent.AppliedConfig == nil {
			agent.AppliedConfig = &store.AgentAppliedConfig{}
		}
		switch updates.GCPIdentity.MetadataMode {
		case store.GCPMetadataModeBlock:
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModeBlock,
			}
		case store.GCPMetadataModePassthrough:
			// Passthrough exposes the broker's own GCP identity to the agent
			// container. The create path (see createAgentInProject) enforces
			// broker-owner/admin + actAs restriction for passthrough. This
			// PATCH path must enforce the same restriction — without it,
			// "create without passthrough, then PATCH it in" bypasses the
			// create-path gate. Both checks live in
			// authorizePassthroughIdentity.
			if agent.RuntimeBrokerID == "" {
				ValidationError(w, "GCP identity passthrough requires a runtime broker, but this agent has no broker assigned", nil)
				return
			}
			broker, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if !s.authorizePassthroughIdentity(w, r, broker, SurfacePassthroughPatch) {
				return
			}
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModePassthrough,
			}
			// Passthrough-to-assign translation for cloudrun-sandbox runtimes
			// (same logic as the create path — see createAgentInProject).
			if err := s.translatePassthroughForSandbox(ctx, agent, agent.RuntimeBrokerID); err != nil {
				slog.ErrorContext(ctx, "passthrough-to-assign translation failed (PATCH)",
					"agent", agent.ID, "broker", agent.RuntimeBrokerID, "error", err)
				writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError,
					"failed to configure GCP identity for sandbox runtime: "+err.Error(), nil)
				return
			}
		case store.GCPMetadataModeAssign:
			if updates.GCPIdentity.ServiceAccountID == "" {
				ValidationError(w, "service_account_id is required when metadata_mode is 'assign'", nil)
				return
			}
			sa, err := s.store.GetGCPServiceAccount(ctx, updates.GCPIdentity.ServiceAccountID)
			if err != nil {
				// Was writeErrorFromErr(w, err, "GCP service account not found"),
				// which was wrong twice over. That third parameter is requestID,
				// not a message, so the string shipped in the response's requestId
				// field while the message read "Resource not found" — and the
				// status was 404, against 400 for the not-reachable branch twelve
				// lines down. Existence and reachability were distinguishable here
				// by status code even more plainly than by wording.
				//
				// errors.Is, not ==: see the create path.
				if errors.Is(err, store.ErrNotFound) {
					ValidationError(w, msgSANotAvailableInProject, nil)
					return
				}
				writeErrorFromErr(w, err, "")
				return
			}
			// These two checks mirror the create path (see the assign branch of
			// createAgentInProject) deliberately, character for character. Without
			// them, "create with no service account, then PATCH one in" walks
			// straight around the hardened create path — it needs only update
			// rights on the agent, which the creator has by definition.
			//
			// Kept as a near-duplicate rather than factored into a shared helper on
			// purpose. That duplication has now paid for itself once: the ScopeID
			// equality it describes became scope-aware in P4 item F, and the site
			// was found by grepping for the create path's shape. The property is
			// worth preserving — keep these greppably identical to the create path.
			if !sa.ReachableFromProject(agent.ProjectID) {
				ValidationError(w, msgSANotAvailableInProject, nil)
				return
			}
			if !gcpServiceAccountVerified(sa) {
				ValidationError(w, "GCP service account is not verified; verify it before assigning to agents", nil)
				return
			}
			// Parity with the create path, and now literally the same call.
			// PATCH is the surface that most needs it: reassigning an existing
			// agent's identity is the cheapest way to acquire a service account
			// you could not have been given at create time.
			if !s.authorizeSAAssignment(w, r, sa, SurfaceAgentPatch) {
				return
			}
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode:        store.GCPMetadataModeAssign,
				ServiceAccountID:    sa.ID,
				ServiceAccountEmail: sa.Email,
				ProjectID:           sa.ProjectID,
			}
		default:
			ValidationError(w, "invalid metadata_mode: must be 'block', 'passthrough', or 'assign'", nil)
			return
		}
	}

	// Writer (b) of ExplicitTimezone. Unlike config edits it is accepted in
	// any phase; a running container keeps its TZ until the next start.
	if updates.ExplicitTimezone != nil {
		if agent.AppliedConfig == nil {
			agent.AppliedConfig = &store.AgentAppliedConfig{}
		}
		// Warn only when the edit changes the zone the agent resolves to
		// while a container is live; any other phase picks the zone up at
		// its next start anyway. A same-zone re-pin that only clears the
		// legacy label, or an unpin that falls back to the same zone, leaves
		// the container as it is.
		live := phaseHasLiveContainer(agent.Phase)
		var zoneBefore string
		if live {
			zoneBefore = s.agentTZ(ctx, agent).TZ
		}
		changed, err := applyExplicitTimezoneEdit(agent.AppliedConfig, *updates.ExplicitTimezone)
		if err != nil {
			ValidationError(w, err.Error(), map[string]interface{}{"field": "explicitTimezone"})
			return
		}
		if live && changed && s.agentTZ(ctx, agent).TZ != zoneBefore {
			warnings = append(warnings, explicitTimezoneNextStartWarning)
		}
	}

	if updates.Name != "" {
		// Name and its identity key are written in the same transaction:
		// the key row is what makes the key's per-project uniqueness a
		// database invariant, so it must never be able to drift from the
		// Name it was computed from. agent.Name is already updates.Name at
		// this point (set above), so this is {Slug, the display name's key}
		// when they differ, or the single row {Slug} when the display name's
		// key already equals it (e.g. a freshly created agent) -- the same
		// api.IdentityKeysFor create and restore use, so all three writers
		// agree on this set byte-for-byte.
		keys := api.IdentityKeysFor(agent.Slug, agent.Name)
		err := s.store.WithTx(ctx, func(tx store.Store) error {
			if err := tx.UpdateAgent(ctx, agent); err != nil {
				return err
			}
			return tx.ReplaceAgentIdentityKeys(ctx, agent.ID, agent.ProjectID, keys)
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
	} else if err := s.store.UpdateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	tz := s.agentTZ(ctx, agent)
	writeJSON(w, http.StatusOK, agentUpdateResponse{
		Agent:            redactedAgentCopy(ctx, s, agent),
		ResolvedTimezone: tz.TZ,
		TimezoneSource:   tz.Source,
		Warnings:         warnings,
	})
}

// agentUpdateResponse is the agent PATCH response: the updated agent plus
// the container timezone it will get at its next start, the rung of the
// agent TZ chain that supplies it, and any warnings about the request.
// resolvedTimezone is "" (source "none") when no TZ will be sent.
type agentUpdateResponse struct {
	*store.Agent
	ResolvedTimezone string   `json:"resolvedTimezone"`
	TimezoneSource   string   `json:"timezoneSource"`
	Warnings         []string `json:"warnings,omitempty"`
}

// checkBrokerAvailability verifies the agent's runtime broker is reachable.
// Returns true if the broker is available (or no broker is assigned).
// Returns false and writes a 503 error response if the broker is offline.
func (s *Server) checkBrokerAvailability(w http.ResponseWriter, r *http.Request, agent *store.Agent) bool {
	if s.brokerReachable(r.Context(), agent) {
		return true
	}
	RuntimeBrokerUnavailable(w, agent.RuntimeBrokerID, nil)
	return false
}

// brokerReachable reports whether the agent's runtime broker looks reachable,
// without writing a response. It returns true when no broker is assigned, and
// when the broker status cannot be read.
func (s *Server) brokerReachable(ctx context.Context, agent *store.Agent) bool {
	if agent.RuntimeBrokerID == "" {
		return true
	}

	// Check real-time WebSocket connectivity first (no DB query needed)
	if s.controlChannel != nil && s.controlChannel.IsConnected(agent.RuntimeBrokerID) {
		return true
	}

	// Fall back to DB status check (covers co-located mode where there's no WebSocket)
	broker, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		s.agentLifecycleLog.Warn("Failed to check broker status", "brokerID", agent.RuntimeBrokerID, "error", err)
		// If we can't verify, let it through rather than blocking
		return true
	}

	return s.brokerRecordReachable(broker)
}

// brokerRecordReachable reports whether an already-loaded broker looks
// reachable: connected over the control channel, or marked online in the
// store. It is the shared rule behind brokerReachable (lifecycle actions) and
// the explicit-broker check in resolveRuntimeBroker (agent create), so the
// two cannot disagree about what "offline" means.
func (s *Server) brokerRecordReachable(broker *store.RuntimeBroker) bool {
	if s.controlChannel != nil && s.controlChannel.IsConnected(broker.ID) {
		return true
	}
	return broker.Status == store.BrokerStatusOnline
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, id string) {
	agent, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	s.performAgentDelete(w, r, agent)
}

// performAgentDelete deletes an agent through the backend-driven engine
// (design ptone/scion#2483 §2.4): load, authorize, DeletedAt → 204, an
// active delete → join, broker availability → 503, claim, then start the
// engine and wait up to deleteSyncWait for its outcome. A fast delete answers
// 204 (or 502/409 on failure) exactly as before; a slow one answers 202
// {agentId, deletion} and its outcome arrives as events. A second DELETE
// joins the first and never gets 409 for the overlap.
func (s *Server) performAgentDelete(w http.ResponseWriter, r *http.Request, agent *store.Agent) {
	ctx := r.Context()
	ctx, span := tracer.Start(ctx, "hub.agent.delete")
	defer span.End()
	r = r.WithContext(ctx)
	// Note: HTTP error status is recorded by the otelhttp parent span.
	span.SetAttributes(
		attribute.String("scion.agent.id", agent.ID),
	)
	deadline := time.Now().Add(deleteSyncWaitFor(r))

	// Authorize through the shared agent-target rule: agent.delete on this
	// agent for every caller kind (an agent caller also needs the lifecycle
	// scope within the agent's project, and the delegation ceiling of every
	// live ancestor applies). Unknown caller kinds are denied.
	identity := GetIdentityFromContext(ctx)
	switch identity.(type) {
	case UserIdentity, AgentIdentity:
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"This action requires user or agent authentication", nil)
		return
	}
	if denial := s.authorizeAgentTargetAction(ctx, identity, agent, ActionDelete); denial != nil {
		writeAgentTargetDenial(w, r, identity, agent, ActionDelete, denial)
		return
	}
	// Whether the response may carry the deletion detail (failure code,
	// error text, claim): platform admins only (ptone/scion#3122).
	// Evaluated once here and passed to every writer below.
	isAdmin := callerSeesDeletionDetail(ctx)

	query := r.URL.Query()
	// Default deleteFiles and removeBranch to true for full cleanup.
	// Callers can explicitly set them to "false" to preserve files/branches.
	params := agentDeleteParams{
		deleteFiles:  query.Get("deleteFiles") != "false",
		removeBranch: query.Get("removeBranch") != "false",
		force:        query.Get("force") == "true",
		requestedBy:  identity.ID(),
	}

	for attempt := 0; ; attempt++ {
		// Idempotency: an already-deleted agent returns 204.
		if !agent.DeletedAt.IsZero() {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// A live delete: join it rather than starting a second one.
		if deletionActive(agent) {
			s.joinAgentDeletion(w, r, agent.ID, agent.DeletionClaim, deadline, isAdmin)
			return
		}

		// Verify the broker is reachable before claiming, to avoid orphaned
		// containers. Force bypasses this so stuck agents can always be
		// cleaned up; managed agents have no broker; a created row with no
		// launch in flight dispatches best-effort (ptone/scion#2635).
		createdNoLaunch := agent.Phase == string(state.PhaseCreated) && agent.LaunchState != store.LaunchStateActive
		if !isManagedAgentRuntime(agent.Runtime) && !createdNoLaunch && !params.force && !s.checkBrokerAvailability(w, r, agent) {
			return
		}

		plan, err := s.claimAgentDeletion(ctx, agent.ID, params)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if plan != nil {
			s.events.PublishAgentStatus(ctx, plan.snapshot)
			done := s.runAgentDeletion(ctx, plan)
			s.awaitAgentDeletion(w, r, plan, done, deadline, isAdmin)
			return
		}

		// The claim affected no row: re-read and decide.
		fresh, err := s.store.GetAgent(ctx, agent.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		agent = fresh
		if attempt >= 2 && agent.DeletedAt.IsZero() && !deletionActive(agent) {
			// Three claim misses on a row that is neither deleted nor
			// deleting (a racing write each time): let the row decide rather
			// than answering 409 to a DELETE. Join for a claim after the one
			// read here, so an older failed or cleared marker is not taken
			// as this delete's outcome; with no later claim the join answers
			// 202 at the deadline.
			s.joinAgentDeletion(w, r, agent.ID, agent.DeletionClaim+1, deadline, isAdmin)
			return
		}
	}
}

// awaitAgentDeletion waits for this request's engine up to deadline and
// writes the outcome; on a lost claim it joins whichever delete holds the row.
// isAdmin is callerSeesDeletionDetail for the request (see writeDeletionFailure).
func (s *Server) awaitAgentDeletion(w http.ResponseWriter, r *http.Request, plan *agentDeletionPlan, done <-chan deletionOutcome, deadline time.Time, isAdmin bool) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case out := <-done:
		switch out.kind {
		case deletionOutcomeDeleted:
			w.WriteHeader(http.StatusNoContent)
		case deletionOutcomeFailed:
			writeDeletionFailure(w, plan.snapshot.ID, out.code, out.message, out.retryAfter, isAdmin)
		default: // lost: someone else holds the row now
			s.joinAgentDeletion(w, r, plan.snapshot.ID, plan.claim, deadline, isAdmin)
		}
	case <-timer.C:
		s.writeDeleteAccepted(w, plan.snapshot.ID, isAdmin)
	case <-r.Context().Done():
		// The client went away; the engine keeps running detached.
	}
}

// cancelScheduledEventsForAgent cancels all pending scheduled events that
// target the given agent, preventing orphaned events from firing after deletion.
func (s *Server) cancelScheduledEventsForAgent(ctx context.Context, agent *store.Agent) {
	result, err := s.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{
		ProjectID: agent.ProjectID,
		Status:    store.ScheduledEventPending,
	}, store.ListOptions{Limit: 1000})
	if err != nil {
		s.agentLifecycleLog.Warn("Failed to list scheduled events for cleanup",
			"agent_id", agent.ID, "error", err)
		return
	}

	var cancelled int
	for _, evt := range result.Items {
		if !eventTargetsAgent(evt, agent) {
			continue
		}
		if err := s.store.UpdateScheduledEventStatus(ctx, evt.ID,
			store.ScheduledEventCancelled, nil, "target agent deleted"); err != nil {
			s.agentLifecycleLog.Warn("Failed to cancel scheduled event",
				"event_id", evt.ID, "agent_id", agent.ID, "error", err)
			continue
		}
		if s.scheduler != nil {
			if cancelErr := s.scheduler.CancelEvent(ctx, evt.ID); cancelErr != nil {
				s.agentLifecycleLog.Warn("Failed to cancel in-memory scheduler timer",
					"event_id", evt.ID, "error", cancelErr)
			}
		}
		cancelled++
	}

	if cancelled > 0 {
		s.agentLifecycleLog.Info("Cancelled scheduled events for deleted agent",
			"agent_id", agent.ID, "agent_name", agent.Name, "cancelled", cancelled)
	}
}

// eventTargetsAgent reports whether a scheduled event's payload targets
// agent, by ID or Slug only. Name is a mutable display field, not an
// identifier, so it must not be used to select an agent's scheduled events.
func eventTargetsAgent(evt store.ScheduledEvent, agent *store.Agent) bool {
	var payload struct {
		AgentID   string `json:"agentId"`
		AgentName string `json:"agentName"`
	}
	if err := json.Unmarshal([]byte(evt.Payload), &payload); err != nil {
		return false
	}
	if payload.AgentID != "" && payload.AgentID == agent.ID {
		return true
	}
	if payload.AgentName != "" && payload.AgentName == agent.Slug {
		return true
	}
	return false
}

func (s *Server) handleAgentAction(w http.ResponseWriter, r *http.Request, id, action string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx, span := tracer.Start(r.Context(), "hub.agent.action")
	defer span.End()
	// Note: HTTP error status is recorded by the otelhttp parent span.
	span.SetAttributes(
		attribute.String("scion.agent.id", id),
		attribute.String("scion.agent.action", action),
	)
	r = r.WithContext(ctx)

	// For actions other than status/token refresh and outbound-message
	// (self-access), we require user or agent authentication
	// with appropriate scopes. Self-access endpoints enforce their own auth checks.
	selfAccess := action == api.AgentActionStatus ||
		action == api.AgentActionMetrics ||
		action == api.AgentActionTokenRefresh ||
		action == api.AgentActionRefreshToken ||
		action == api.AgentActionOutboundMessage

	// --- set_message_mode action: own permission model (D7) ---
	// Mode changes are human-only and use agent.set_message_mode permission,
	// not lifecycle authorization. Must be routed before the generic authz block.
	if action == api.AgentActionSetMessageMode {
		s.handleSetMessageMode(w, r, id)
		return
	}

	// --- reincarnate action: own permission model (design §3.8, decision D2) ---
	// A self-reincarnation is allowed for any role with no scope check, which
	// the generic lifecycle-authz block below does not support. Must be
	// routed before it, like set_message_mode above.
	if action == api.AgentActionReincarnate {
		s.handleReincarnateAgent(w, r, id)
		return
	}

	// --- Message action: routed through authorizeAgentMessage (D1) ---
	// Messaging is a first-class axis, split from lifecycle/attach. The choke
	// point handles user senders, agent senders, mode checks, and piercing.
	if action == api.AgentActionMessage {
		identity := GetIdentityFromContext(r.Context())
		if identity == nil {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "This action requires user or agent authentication", nil)
			return
		}

		// Self-message is handled inside authorizeAgentMessage as a
		// self-access exemption, separate from system-plane (D8).
		isSystemPlane := false

		targetAgent, err := s.store.GetAgent(r.Context(), id)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// Raw keystroke delivery through /message has been removed. A body
		// carrying the retired raw field (top level or structured_message)
		// is rejected with raw_input_removed before authorizeAgentMessage
		// or any decode, persistence or dispatch runs; every other body
		// falls through with its bytes restored.
		if s.rejectRetiredRawMessageBody(w, r, rawIngressAgentMessage,
			agentKeysAuditTarget{AgentID: targetAgent.ID, ProjectID: targetAgent.ProjectID},
			rawInputRemovedReplacement, rawTombstonePreAuthMaxBodyBytes, "structured_message") {
			return
		}

		allowed, reason, decision := s.authorizeAgentMessage(r.Context(), identity, targetAgent, isSystemPlane)
		messaging.RecordStep(r.Context(), "message_authorized")
		if !allowed {
			// Use typed denial code from the decision (agent path) or
			// fall back to reason-based mapping (user path).
			denialCode := mapReasonToCode(reason)
			if decision != nil && decision.Code != "" {
				denialCode = string(decision.Code)
			}
			slog.Warn("message authorization denied",
				"sender_type", identity.Type(),
				"sender_id", identity.ID(),
				"target_agent", id,
				"reason", reason,
				"denial_code", denialCode,
			)
			writeError(w, http.StatusForbidden, ErrCodeMessageDenied,
				"Message delivery denied", map[string]interface{}{
					"reason":        denialCode,
					"senderMode":    s.getSenderMode(r.Context(), identity),
					"recipientMode": targetAgent.MessageMode,
				})
			return
		}
		// Authorization passed — fall through to action dispatch below.
		goto actionDispatch
	}

	// --- Keys action: ExecuteAgentKeys (task 2.2, contract §3) ---
	// This is the sole authoritative operation for the keys action on this
	// route: bounded strict body decode, one minted operation ID, target
	// resolution (this route's own lookup, same as every other top-level
	// action), authorizeAgentKeys, admission and one typed dispatch. It
	// writes its own response for every outcome and never falls through to
	// actionDispatch -- unlike every other action below, keys does not
	// share the generic switch's dispatch handlers or its differently-shaped
	// !selfAccess 403 (see execute_agent_keys.go for the full flow).
	//
	// No separate nil-identity guard: authorizeAgentKeys already fails
	// closed (keys_denied) on a nil identity, and the shared auth
	// middleware answers an unauthenticated request with 401 before this
	// handler ever runs — an extra guard here would be dead code.
	if action == api.AgentActionKeys {
		s.handleAgentActionKeysTopLevel(w, r, id)
		return
	}

	if !selfAccess {
		userIdent := GetUserIdentityFromContext(r.Context())
		agentIdent := GetAgentIdentityFromContext(r.Context())
		if userIdent == nil && agentIdent == nil {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "This action requires user or agent authentication", nil)
			return
		}
		// Every caller needs the action's exact permission on the target
		// through the shared agent-target rule.
		targetAgent, err := s.store.GetAgent(r.Context(), id)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		identity := GetIdentityFromContext(r.Context())
		authzAction := agentActionPermission(action)
		if denial := s.authorizeAgentTargetAction(r.Context(), identity, targetAgent, authzAction); denial != nil {
			writeAgentTargetDenial(w, r, identity, targetAgent, authzAction, denial)
			return
		}
	}

actionDispatch:

	switch action {
	case api.AgentActionStatus:
		s.updateAgentStatus(w, r, id)
	case api.AgentActionStart, api.AgentActionStop, api.AgentActionSuspend, api.AgentActionRestart:
		s.handleAgentLifecycle(w, r, id, action)
	case api.AgentActionMessage:
		s.handleAgentMessage(w, r, id)
	case api.AgentActionExec:
		s.handleAgentExec(w, r, id)
	case api.AgentActionResetAuth:
		s.handleAgentResetAuth(w, r, id)
	case api.AgentActionRestore:
		s.restoreAgent(w, r, id)
	case api.AgentActionTokenRefresh:
		s.handleAgentTokenRefresh(w, r, id)
	case api.AgentActionRefreshToken:
		s.handleAgentGitHubTokenRefresh(w, r, id)
	case api.AgentActionOutboundMessage:
		s.handleAgentOutboundMessage(w, r, id)
	case api.AgentActionMetrics:
		s.handleAgentMetrics(w, r, id)
	case api.AgentActionMessages:
		// Defence-in-depth: this action is normally intercepted earlier in
		// handleAgentRoute (before the POST-only gate) so that GET requests
		// are served. This case handles the unlikely path where the request
		// reaches handleAgentAction directly.
		s.handleAgentMessages(w, r, id)
	case api.AgentActionEnv:
		agent, err := s.store.GetAgent(r.Context(), id)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		s.submitAgentEnv(w, r, agent.ProjectID, id)
	default:
		NotFound(w, "Action")
	}
}

func (s *Server) handleAgentExec(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	var req struct {
		Command []string `json:"command"`
		Timeout int      `json:"timeout,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if len(req.Command) == 0 {
		ValidationError(w, "command is required", nil)
		return
	}
	if req.Timeout < 0 {
		ValidationError(w, "timeout must be non-negative", nil)
		return
	}

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Managed agents: return formatted interaction state instead of exec.
	if isManagedAgentRuntime(agent.Runtime) {
		output, err := formatManagedAgentLook(ctx, agent)
		if err != nil {
			RuntimeError(w, "Failed to get managed agent state: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Output   string `json:"output"`
			ExitCode int    `json:"exitCode"`
		}{
			Output:   output,
			ExitCode: 0,
		})
		return
	}

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		ServiceNotReady(w, "Exec dispatch is not available yet — the server may still be starting up")
		return
	}

	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		ServiceNotReady(w, "Agent has no runtime broker assigned — the server may still be starting up")
		return
	}

	// Read before the dispatch: the reconcile below only counts the agent
	// as unseen if nothing reported it from here on. last_seen may be
	// stamped by another hub replica, so this assumes replica clocks agree
	// to well within an exec's duration (NTP). No margin is subtracted: skew
	// can only matter for a status report racing a real disappearance
	// (start, restart and stop are caught by the run ID, state_version and
	// start-claim guards), and the next heartbeat corrects that case.
	dispatchedAt := time.Now()
	output, exitCode, err := dispatcher.DispatchAgentExec(ctx, agent, req.Command, req.Timeout)
	if err != nil {
		if isBrokerAgentNotFound(err) {
			// The broker answered that the agent has no running container
			// (e.g. its pod is gone): a state conflict, not a broker
			// failure (ptone/scion#3443). Record it on the agent too
			// (ptone/scion#3470). This runs before the 409 on purpose: the
			// response is buffered until the handler returns anyway, and a
			// caller that re-reads the agent after the 409 sees the
			// reconciled phase. The write is one list query plus one
			// conditional update, bounded by execReconcileTimeout.
			s.reconcileExecAgentNotFound(ctx, agent, dispatchedAt)
			writeError(w, http.StatusConflict, ErrCodeAgentNotRunning,
				"Agent has no running container on its runtime broker; start or restart the agent and retry", nil)
			return
		}
		if writeBrokerRuntimeUnavailable(w, err, agent.Runtime) {
			return
		}
		RuntimeError(w, "Failed to execute command on runtime broker: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exitCode"`
	}{
		Output:   output,
		ExitCode: exitCode,
	})
}

// handleAgentTokenRefresh handles POST /api/v1/agents/{id}/token/refresh.
// An agent can refresh its own token before it expires to get a new token
// with a fresh expiry. This is a self-access operation: the agent must present
// a valid token whose subject matches the target agent ID.
func (s *Server) handleAgentTokenRefresh(w http.ResponseWriter, r *http.Request, id string) {
	agentIdent := GetAgentIdentityFromContext(r.Context())
	if agentIdent == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"agent authentication required for token refresh", nil)
		return
	}

	// Enforce self-access: agents can only refresh their own token
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"agents can only refresh their own token", nil)
		return
	}

	// Require the token refresh scope
	if !agentIdent.HasScope(ScopeAgentTokenRefresh) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"missing required scope: agent:token:refresh", nil)
		return
	}

	if s.agentTokenService == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"agent token service not available", nil)
		return
	}

	// Phase 1H: Agent-token authentication requires a successful
	// credential-status evaluation, including on refresh. The credential-ID
	// and legacy markers set by UnifiedAuthMiddleware are trusted when
	// present, but their absence must not skip the check: re-derive the
	// verified JTI from the validated token and evaluate status directly.
	oldCredentialID := GetAgentCredentialIDFromContext(r.Context())
	isLegacy := IsLegacyTokenFromContext(r.Context())

	if oldCredentialID != "" {
		// The credential-ID marker is present: the middleware already found
		// an active credential for this token. Re-check it hasn't been
		// revoked since, and require the lookup itself to succeed — a store
		// error here must not silently allow the refresh to proceed.
		cred, credErr := s.store.GetAgentCredentialByJTIHash(r.Context(),
			hashJTI(agentIdent.TokenID()))
		switch {
		case credErr == nil && cred == nil:
			// The store reported success without a credential record, so
			// the credential's status could not be determined.
			slog.Error("Token refresh: credential status lookup returned no credential",
				"agent_id", id)
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
				"unable to verify credential status", nil)
			return
		case credErr == nil && cred.RevokedAt != nil:
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"token has been revoked", nil)
			return
		case credErr != nil:
			slog.Error("Token refresh: credential status lookup failed",
				"agent_id", id, "error", credErr)
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
				"unable to verify credential status", nil)
			return
		}
	} else if !isLegacy {
		// Neither marker is present. Evaluate status directly from the
		// validated token's JTI.
		cred, legacy, credErr := evaluateAgentCredentialStatus(r.Context(), s.store, agentIdent.TokenID())
		switch {
		case errors.Is(credErr, errAgentCredentialRevoked):
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"token has been revoked", nil)
			return
		case credErr != nil:
			slog.Error("Token refresh: credential status lookup failed",
				"agent_id", id, "error", credErr)
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
				"unable to verify credential status", nil)
			return
		case legacy:
			// Legacy compatibility path, retained pending a product
			// decision: the isLegacy checks below apply.
			isLegacy = true
		default:
			oldCredentialID = cred.ID
		}
	}

	// Look up the agent record to re-derive scopes from the stored role.
	// This is critical for backward compatibility: legacy agents created
	// before the role system have tokens with old scope sets (missing
	// ScopeProjectRead, etc.). Copying old scopes verbatim on refresh
	// would perpetuate the gap. AuthorizeAgentToken re-derives the
	// scopes from the stored role and bounds them by the agent's chain
	// ceiling, with the stored ancestry.
	agent, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		slog.Warn("Token refresh: failed to look up agent for role-based scope derivation",
			"agent_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to look up agent record", nil)
		return
	}

	// For legacy tokens, verify the agent is still authorized
	if isLegacy {
		if !agent.DeletedAt.IsZero() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"agent has been deleted", nil)
			return
		}
		if agent.Phase == "suspended" {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"agent is suspended", nil)
			return
		}
	}

	// The refreshed token keeps the run binding of the presented one: a
	// token issued without a run stays without one.
	grant, err := authorizeAgentTokenAt(r.Context(), s, s.store, agent, mintSiteRefresh)
	var newToken string
	if err == nil {
		newToken, err = signAndRecordAgentToken(r.Context(), s, s.store, *grant, presentedAgentTokenRunID(r.Context()))
	}
	if err != nil {
		if writeAgentTokenIssueError(w, err) {
			return
		}
		slog.Error("Token refresh: token not issued", "agent_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to generate refreshed token", nil)
		return
	}

	// Revoke the old credential now that a new token has been issued (best-effort)
	if oldCredentialID != "" {
		if err := s.store.RevokeAgentCredential(r.Context(), oldCredentialID, "system", "refreshed"); err != nil {
			slog.Warn("Failed to revoke old credential after refresh",
				"agent_id", id, "credential_id", oldCredentialID, "error", err)
		}
	}

	// Parse the new token to extract the expiry for the response.
	newClaims, err := s.agentTokenService.ValidateAgentToken(newToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to validate refreshed token", nil)
		return
	}
	expiresAt := newClaims.Expiry.Time()

	// Build the generalized tokens[] array.
	// App tokens are always present; transport tokens are added when
	// the hub has a transport minter configured.
	tokens := []RefreshTokenEntry{
		{
			Layer:     "app",
			Type:      "scion_access",
			Value:     newToken,
			ExpiresIn: int(time.Until(expiresAt).Seconds()),
		},
	}

	// Mint a transport token if transport auth is configured. A mint
	// failure does not fail the refresh (the app token is still valid), but
	// it is reported to the agent in transportError so it can be surfaced
	// there (sciontool doctor, agent logs) rather than only in hub logs.
	// The underlying error stays in hub logs; the agent gets a fixed,
	// non-sensitive description.
	transportError := ""
	if s.transportMinter != nil && s.transportAudience != "" {
		tToken, tExpiry, tErr := s.transportMinter.MintIDToken(r.Context(), s.transportAudience)
		if tErr != nil {
			slog.Warn("Failed to mint transport token during refresh",
				"agent_id", id, "error", tErr)
			transportError = TransportMintFailedMessage
		} else if tToken == "" {
			slog.Warn("Transport token minter returned an empty token during refresh",
				"agent_id", id)
			transportError = TransportMintFailedMessage
		} else {
			tokens = append(tokens, RefreshTokenEntry{
				Layer:     "transport",
				Type:      "google_oidc",
				Value:     tToken,
				ExpiresIn: int(time.Until(tExpiry).Seconds()),
				Audience:  s.transportAudience,
			})
		}
	}

	// Response includes both the legacy single-token fields (backward compat)
	// and the generalized tokens[] array. Old clients ignore tokens[];
	// new clients prefer tokens[].
	resp := map[string]interface{}{
		"token":      newToken,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
		"tokens":     tokens,
	}
	if transportError != "" {
		resp["transportError"] = transportError
	}
	// Conduit grant verification keys (hub.conduit on): the agent's target
	// refreshes its key set here as well as from each Welcome. Omitted when
	// the experiment is off or the keys are unavailable.
	if keys := s.conduitRefreshGrantKeys(r.Context()); len(keys) > 0 {
		resp["conduit_grant_keys"] = keys
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAgentResetAuth handles POST /api/v1/agents/{id}/reset-auth.
// It generates a fresh token and pushes it into the running agent container
// via the runtime broker, restarting the agent's token refresh loop without
// a full container restart.
func (s *Server) handleAgentResetAuth(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	disp := s.GetDispatcher()
	if disp == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"agent dispatcher not configured", nil)
		return
	}

	if err := disp.DispatchAgentResetAuth(ctx, agent); err != nil {
		slog.Error("Failed to reset agent auth", "agent_id", id, "error", err)
		if writeAgentTokenRecordError(w, err) {
			return
		}
		if writeBrokerRuntimeUnavailable(w, err, agent.Runtime) {
			return
		}
		if writeAgentTokenIssueError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"auth reset failed: "+err.Error(), nil)
		return
	}

	slog.Info("Agent auth reset dispatched", "agent_id", id)
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "Auth reset dispatched successfully",
	})
}

func isContainerNameConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return (strings.Contains(msg, "container name") && strings.Contains(msg, "already in use")) ||
		strings.Contains(msg, "is already in use by container")
}

// skillResolutionErrorCode is the broker's code for a required-skill
// resolution failure.
const skillResolutionErrorCode = api.BrokerErrCodeSkillResolution

// workspaceStorageUnconfiguredErrorCode is the broker's code for a create
// with no bucket to download the workspace upload from.
const workspaceStorageUnconfiguredErrorCode = api.BrokerErrCodeWorkspaceStorageUnconfigured

// dispatchCreateErrorResponse classifies a failed create/provision dispatch to
// the runtime broker and writes the matching HTTP response.
//
// Every dispatch failure used to fold into RuntimeError's 502, regardless of
// what the broker actually reported. The broker now returns 404 naming the
// resource when a configured harness-config or template name does not
// resolve anywhere it looked (pkg/runtimebroker/handlers.go), and that
// status code survives the HTTP hop as a *brokerStatusError — so it is
// checked here before falling back to the generic "runtime broker failed"
// 502 every other failure still gets (ptone/scion#1316 fault 3).
//
// A required-skill resolution failure is relayed verbatim: the broker's
// status, message, details and Retry-After, with no hub prefix (#2546 R2).
//
// A delete that claimed agentID while the create was in flight refuses the
// dispatch's run-ID write (store.ErrDeleteInProgress); that answers 409
// delete_in_progress, as start does (ptone/scion#2550).
func dispatchCreateErrorResponse(w http.ResponseWriter, err error, agentID string) {
	if writeAgentTokenRecordError(w, err) {
		return
	}
	if ref := deleteClaimedDuringDispatch(err, agentID); ref != nil {
		ref.write(w)
		return
	}
	if writeAgentTokenIssueError(w, err) {
		return
	}

	switch {
	case isContainerNameConflict(err):
		Conflict(w, "Agent name is already in use by a stopped container. Please delete the existing agent or choose a different name.")
	case relaySkillResolutionError(w, err):
		// Response already written.
	case relayWorkspaceStorageUnconfigured(w, err):
		// Response already written.
	case isBrokerStatus(err, http.StatusNotFound):
		message := err.Error()
		var se *brokerStatusError
		if errors.As(err, &se) {
			message = se.brokerErrorMessage()
		}
		writeError(w, http.StatusNotFound, ErrCodeNotFound, "Failed to dispatch to runtime broker: "+message, nil)
	default:
		RuntimeError(w, "Failed to dispatch to runtime broker: "+err.Error())
	}
}

// skillResolutionDispatchError returns the broker's typed required-skill
// resolution failure (error code skill_resolution_failed) carried by err.
func skillResolutionDispatchError(err error) (*brokerStatusError, bool) {
	var se *brokerStatusError
	if errors.As(err, &se) && se.brokerErrorCode() == skillResolutionErrorCode {
		return se, true
	}
	return nil, false
}

// isSkillResolutionDispatchError reports whether err is the broker's typed
// required-skill resolution failure (error code skill_resolution_failed).
func isSkillResolutionDispatchError(err error) bool {
	_, ok := skillResolutionDispatchError(err)
	return ok
}

// relaySkillResolutionError writes the broker's required-skill resolution
// failure verbatim -- its status, code, message, details and Retry-After,
// with no hub prefix -- and reports whether it did. For any other error it
// writes nothing and returns false, so the caller keeps its own handling.
//
// The broker reports a skill the caller cannot read as not_found (404), the
// same as a skill that does not exist, so relaying the status unchanged
// keeps the two indistinguishable.
//
// Only the skill and cause details reach the client. The broker also sends
// its start markers (startAttempted, runId, currentRunId); the dispatcher
// reads those to settle the agent's run ID before this point, and they are
// not part of the error the client sees.
func relaySkillResolutionError(w http.ResponseWriter, err error) bool {
	se, ok := skillResolutionDispatchError(err)
	if !ok {
		return false
	}
	if se.RetryAfter != "" {
		w.Header().Set("Retry-After", se.RetryAfter)
	}
	writeError(w, se.StatusCode, skillResolutionErrorCode, se.brokerErrorMessage(), skillResolutionClientDetails(se.brokerErrorDetails()))
	return true
}

// relayWorkspaceStorageUnconfigured writes the broker's refusal to create an
// agent whose workspace upload it has no bucket for, keeping the broker's
// status (422), code and message instead of the generic 502, and reports
// whether it did (ptone/scion#3422).
func relayWorkspaceStorageUnconfigured(w http.ResponseWriter, err error) bool {
	var se *brokerStatusError
	if !errors.As(err, &se) || se.brokerErrorCode() != workspaceStorageUnconfiguredErrorCode {
		return false
	}
	writeError(w, se.StatusCode, workspaceStorageUnconfiguredErrorCode, "Failed to dispatch to runtime broker: "+se.brokerErrorMessage(), nil)
	return true
}

// skillResolutionClientDetails keeps the skill and cause entries of a
// broker skill resolution failure's details, or returns nil when neither is
// present.
func skillResolutionClientDetails(details map[string]interface{}) map[string]interface{} {
	var out map[string]interface{}
	for _, k := range []string{"skill", "cause"} {
		if v, ok := details[k]; ok {
			if out == nil {
				out = make(map[string]interface{}, 2)
			}
			out[k] = v
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Passthrough-to-assign translation for cloudrun-sandbox runtimes
// ---------------------------------------------------------------------------

// brokerHasCloudRunSandboxProfile reports whether any profile on the broker has
// the "cloudrun-sandbox" runtime type. This indicates the broker runs gVisor
// sandboxes that cannot reach the real GCE metadata server.
func brokerHasCloudRunSandboxProfile(broker *store.RuntimeBroker) bool {
	for _, p := range broker.Profiles {
		if p.Type == "cloudrun-sandbox" {
			return true
		}
	}
	return false
}

// translatePassthroughForSandbox translates a passthrough GCP identity config
// to assign mode when the target broker runs a cloudrun-sandbox runtime.
//
// Inside gVisor sandboxes the real GCE metadata server at 169.254.169.254 is
// unreachable, so passthrough mode produces no credentials. The translation
// uses the broker's registered host service account — semantically equivalent
// to passthrough (same identity) — and the assign machinery (metadata
// emulator → hub gcp-token endpoint → IAM impersonation) works inside the
// sandbox.
//
// The method is a no-op when:
//   - the agent's config is not passthrough,
//   - the broker has no cloudrun-sandbox profile, or
//   - the broker has no host SA registered (leaves passthrough as-is with a
//     warning — identical to pre-fix behavior).
//
// On success the agent's AppliedConfig.GCPIdentity is rewritten in place to
// assign mode with the broker's host SA, and a GCPServiceAccount record is
// created if one does not already exist for the email.
func (s *Server) translatePassthroughForSandbox(
	ctx context.Context,
	agent *store.Agent,
	brokerID string,
) error {
	if agent.AppliedConfig == nil || agent.AppliedConfig.GCPIdentity == nil {
		return nil
	}
	if agent.AppliedConfig.GCPIdentity.MetadataMode != store.GCPMetadataModePassthrough {
		return nil
	}

	if brokerID == "" {
		return fmt.Errorf("cannot translate passthrough: no broker ID")
	}

	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		return fmt.Errorf("load broker %s: %w", brokerID, err)
	}
	if !brokerHasCloudRunSandboxProfile(broker) {
		return nil // not a sandbox runtime — passthrough works as-is
	}

	if broker.GCPHostServiceAccountEmail == "" {
		// The broker has no host SA registered. The passthrough gate should
		// have caught this for explicit passthrough requests; for project-
		// default passthrough the gate doesn't run. Log and leave as-is —
		// the agent won't get credentials, same as the pre-fix behavior.
		slog.WarnContext(ctx, "cloudrun-sandbox broker has no host SA — cannot translate passthrough to assign",
			"broker_id", broker.ID, "broker_name", broker.Name)
		return nil
	}

	// Ensure a GCPServiceAccount record exists for the broker's host SA.
	sa, err := s.ensureHostSARecord(ctx, broker)
	if err != nil {
		return fmt.Errorf("ensure host SA record: %w", err)
	}

	slog.InfoContext(ctx, "translated passthrough to assign for cloudrun-sandbox",
		"agent", agent.Name, "broker", broker.Name,
		"sa_email", sa.Email, "sa_id", sa.ID)

	agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
		MetadataMode:        store.GCPMetadataModeAssign,
		ServiceAccountID:    sa.ID,
		ServiceAccountEmail: sa.Email,
		ProjectID:           sa.ProjectID,
	}

	// Mark the translation so operators and the UI can distinguish a
	// hub-translated assign from a user-requested one.
	if agent.Annotations == nil {
		agent.Annotations = make(map[string]string)
	}
	agent.Annotations["scion.dev/gcp-identity-translated-from"] = "passthrough"

	return nil
}

// ensureHostSARecord looks up or creates a hub-scoped GCPServiceAccount
// record for the broker's host service account. This is needed so that the
// assign flow (JWT scope minting, gcp-token endpoint) works with a real
// ServiceAccountID foreign key.
//
// The record is hub-scoped because the broker's host SA is an infrastructure
// identity, not project-specific — any project dispatching to this broker
// should be able to use it for passthrough translation.
func (s *Server) ensureHostSARecord(
	ctx context.Context,
	broker *store.RuntimeBroker,
) (*store.GCPServiceAccount, error) {
	// Look up by email first.
	existing, err := s.store.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Email: broker.GCPHostServiceAccountEmail,
		Scope: store.ScopeHub,
	})
	if err != nil {
		return nil, fmt.Errorf("lookup SA by email %s: %w", broker.GCPHostServiceAccountEmail, err)
	}
	if len(existing) > 0 {
		return &existing[0], nil
	}

	// Create a new hub-scoped record for the broker's host SA.
	// ScopeID is provenance for hub-scoped accounts (which hub instance
	// registered it). Use the broker ID as provenance — more specific than
	// the hub ID and stable across redeployments.
	projectID := broker.GCPHostProjectID
	if projectID == "" {
		projectID = projectIDFromServiceAccountEmail(broker.GCPHostServiceAccountEmail)
	}

	sa := &store.GCPServiceAccount{
		ID:          gouuid.New().String(),
		Scope:       store.ScopeHub,
		ScopeID:     broker.ID,
		Email:       broker.GCPHostServiceAccountEmail,
		ProjectID:   projectID,
		DisplayName: fmt.Sprintf("Broker host SA (%s)", broker.Name),
		DefaultScopes: []string{
			"https://www.googleapis.com/auth/cloud-platform",
		},
		// The hub can already impersonate this SA (it's the broker's host
		// identity), so mark as verified. The actAs check already passed
		// in authorizePassthroughIdentity for explicit passthrough requests.
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedBy:          "system:passthrough-translation",
		CreatedAt:          time.Now(),
		Managed:            false, // not created by Hub SA provisioning
	}

	if err := s.store.CreateGCPServiceAccount(ctx, sa); err != nil {
		// Race condition: another request may have created the record
		// between our lookup and create. Try the lookup again.
		existing, lookupErr := s.store.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
			Email: broker.GCPHostServiceAccountEmail,
			Scope: store.ScopeHub,
		})
		if lookupErr != nil {
			return nil, fmt.Errorf("create SA %s: %w (retry lookup: %v)",
				broker.GCPHostServiceAccountEmail, err, lookupErr)
		}
		if len(existing) > 0 {
			return &existing[0], nil
		}
		return nil, fmt.Errorf("create SA %s: %w", broker.GCPHostServiceAccountEmail, err)
	}

	slog.InfoContext(ctx, "created hub-scoped GCPServiceAccount for broker host SA",
		"sa_id", sa.ID, "sa_email", sa.Email, "broker", broker.Name)
	return sa, nil
}
