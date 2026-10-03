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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcache"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	listAll        bool
	listDeleted    bool
	listRunning    bool
	sortByTime     bool
	filterPhase    string
	filterActivity string
	filterTemplate string
	sortField      string
	sortReverse    bool
	filterLabels   []string
	listCount      int

	// Attribute filters (Hub mode only — combine with each other and with
	// --phase/--activity/--template/--label using AND). ptone/scion#2146.
	filterOwner   string
	filterBroker  string
	filterHarness string

	// Relationship filters (Hub mode only, mutually exclusive with each
	// other). Empty means unset; scopeInferSentinel (via cobra's
	// NoOptDefVal) means "infer the reference" — see
	// resolveRelationshipReference. ptone/scion#2146.
	filterDescendants string
	filterAncestors   string
	filterLineage     string
)

var validSortFields = map[string]bool{
	"name": true, "phase": true, "created": true, "updated": true, "last-seen": true,
}

// listCmd represents the list command
var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List running scion agents",
	// Args rejects positional arguments so that `scion list --descendants foo`
	// fails loudly instead of silently dropping "foo": --descendants has a
	// NoOptDefVal, so without "=" the flag consumes no value and "foo"
	// parses as a bare positional argument instead of the reference agent.
	// Left unchecked, that positional is simply ignored (listCmd never reads
	// args), so the command would run with --descendants inferring the
	// caller instead of naming "foo" — a silent wrong-answer, not an error.
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return fmt.Errorf(
				"scion list does not take positional arguments (got %v). "+
					"If you meant to name a reference agent for --descendants, --ancestors, or --lineage, "+
					"use \"=\": e.g. --descendants=%s, not --descendants %s",
				args, args[0], args[0])
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateListFlags(); err != nil {
			return err
		}

		// Check if Hub should be used
		hubCtx, err := CheckHubAvailability(projectPath)
		if err != nil {
			// Check if this is because Hub is enabled but project not linked
			if handleUnlinkedProjectPrompt(cmd, args) {
				// User chose to link or disable - retry
				hubCtx, err = CheckHubAvailability(projectPath)
				if err != nil {
					return err
				}
			} else {
				return err
			}
		}

		if hubCtx != nil {
			return listAgentsViaHub(hubCtx)
		}

		// Local mode
		if err := rejectHubOnlyFiltersInLocalMode(); err != nil {
			return err
		}
		return listAgentsLocal()
	},
}

// rejectHubOnlyFiltersInLocalMode returns a clear error if any Hub-only
// filter flag is set while listing locally (ptone/scion#2146).
//
// Local listing has no store.AgentFilter to push these into — it filters an
// in-memory runtime.List() result by name/label only. Before this check,
// setting e.g. --owner or --ancestors in local mode was silently a no-op:
// the flag was simply never read outside listAgentsViaHub, so the command
// printed every agent as though it were the filtered set. A narrowing filter
// that silently narrows nothing makes the output *wider* than what was
// asked for, with no indication anything was ignored — dangerous when the
// output feeds a script (e.g. piped into a bulk stop or delete). Erroring
// is the only response that can't be misread as "no matches."
func rejectHubOnlyFiltersInLocalMode() error {
	type hubOnlyFlag struct {
		name string
		set  bool
	}
	for _, f := range []hubOnlyFlag{
		{"owner", filterOwner != ""},
		{"broker", filterBroker != ""},
		{"harness", filterHarness != ""},
		{"descendants", filterDescendants != ""},
		{"ancestors", filterAncestors != ""},
		{"lineage", filterLineage != ""},
	} {
		if f.set {
			return fmt.Errorf("--%s requires Hub mode (no Hub is configured for this project)", f.name)
		}
	}
	return nil
}

// listAgentsLocal lists agents using the local runtime
func listAgentsLocal() error {
	rt := runtime.GetRuntime(projectPath, profile)
	mgr := agent.NewManager(rt)

	filters := map[string]string{
		"scion.agent": "true",
	}

	if listAll {
		// Cross-project listing might need a way to find all projects.
		// For now, mgr.List handles current project and what's provided in filters.
	} else {
		projectDir, _ := config.GetResolvedProjectDir(projectPath)
		if projectDir != "" {
			filters["scion.project_path"] = projectDir
			filters["scion.project"] = config.GetProjectName(projectDir)
		}
	}

	agents, err := mgr.List(context.Background(), filters)
	if err != nil {
		return err
	}

	return displayAgents(agents, listAll, false)
}

// listAgentsViaHub lists agents using the Hub API
func listAgentsViaHub(hubCtx *HubContext) error {
	PrintUsingHub(hubCtx.Endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	parsedLabels, err := parseLabels(filterLabels)
	if err != nil {
		return err
	}

	opts := &hubclient.ListAgentsOptions{
		IncludeDeleted: listDeleted,
		Phase:          filterPhase,
		Labels:         parsedLabels,
	}
	if listCount > 0 {
		opts.Page.Limit = listCount
	}
	agentSvc := hubCtx.Client.Agents()

	if !listAll {
		// Get the project ID for the current project
		projectID, err := GetProjectID(hubCtx)
		if err != nil {
			return wrapHubError(err)
		}
		opts.ProjectID = projectID
		agentSvc = hubCtx.Client.ProjectAgents(projectID)
	}
	// This check only applies under --all: the project-scoped branch above
	// already handles !listAll, and a project-scoped agent token has its
	// own same-project carve-out (listProjectAgents,
	// pkg/hub/handlers_projects_core.go).
	if listAll && hubCtx.CredentialKind == hubsync.CredentialKindAgentToken && (filterDescendants != "" || filterAncestors != "" || filterLineage != "") {
		// A bare agent token has no hub-wide list authority: the global
		// endpoint --all drives the final listing through returns nothing
		// for it, which would otherwise look like a wrong, silently empty
		// list indistinguishable from "no descendants" (ptone/scion#2146).
		//
		// Keys on hubCtx.CredentialKind — the credential
		// hubsync.createHubClient actually selected, not CLI mode — so a
		// user-authenticated caller (OAuth, or dev auth on a localhost hub)
		// running inside an agent container is not blocked. This does not
		// catch an agent token that carries a group-derived agent.list
		// grant; "drop --all" is still the correct remedy in that case too.
		return fmt.Errorf("--all cannot be combined with --descendants/--ancestors/--lineage when authenticated with an agent token: " +
			"an agent token can only list agents in its own project; drop --all")
	}

	if filterOwner != "" {
		ownerID, err := resolveOwnerID(ctx, hubCtx.Client, filterOwner)
		if err != nil {
			return wrapHubError(err)
		}
		opts.OwnerID = ownerID
	}

	if filterBroker != "" {
		broker, err := resolveBrokerByNameOrID(ctx, hubCtx.Client, filterBroker)
		if err != nil {
			return wrapHubError(err)
		}
		opts.RuntimeBrokerID = broker.ID
	}

	if filterHarness != "" {
		opts.HarnessConfig = filterHarness
	}

	// Relationship flags. Mutually exclusive with each other — enforced by
	// cobra's MarkFlagsMutuallyExclusive at parse time (see init below) — so
	// at most one of these is non-empty here.
	switch {
	case filterDescendants != "":
		agentRef, userID, err := resolveRelationshipReference(ctx, hubCtx.Client, filterDescendants)
		if err != nil {
			return err
		}
		if userID != "" {
			// A user reference: Ancestry records the creator user directly
			// (see createAgent/handlers_agents_core.go and
			// createProjectAgent/handlers_projects_core.go, the two
			// handlers that build it), so the same AncestorID predicate
			// that works for an agent reference works unchanged here.
			opts.AncestorID = userID
		} else {
			refAgent, err := resolveReferenceAgent(ctx, agentSvc, agentRef)
			if err != nil {
				return wrapHubError(err)
			}
			opts.AncestorID = refAgent.ID
		}

	case filterAncestors != "":
		agentRef, userID, err := resolveRelationshipReference(ctx, hubCtx.Client, filterAncestors)
		if err != nil {
			return err
		}
		if userID != "" {
			// A user has no Ancestry chain of its own — nothing to list.
			return displayAgents(nil, listAll, true)
		}
		refAgent, err := resolveReferenceAgent(ctx, agentSvc, agentRef)
		if err != nil {
			return wrapHubError(err)
		}
		if len(refAgent.Ancestry) == 0 {
			// Ancestry is empty (a legacy agent with no recorded lineage —
			// production ancestry is always [creatorUser, ...ancestor
			// agents..., parent] and never empty for a normally-created
			// agent; even a root, user-created agent has Ancestry=[userID]).
			// Short-circuit locally rather than sending an empty id-list
			// query: an id query with zero "id" params is indistinguishable
			// on the wire from "no id restriction at all", which would
			// silently widen the result to every authorized agent instead
			// of none.
			return displayAgents(nil, listAll, true)
		}
		opts.IDs = refAgent.Ancestry

	case filterLineage != "":
		agentRef, userID, err := resolveRelationshipReference(ctx, hubCtx.Client, filterLineage)
		if err != nil {
			return err
		}
		if userID != "" {
			// A user has no Ancestry (no parent to walk to) and no single
			// project of its own — a user reference roots at itself and is
			// intentionally NOT project-bounded, same as --descendants for
			// that user: every agent the user has created, at any depth,
			// across every project the caller can see (--all) or the
			// current project (default).
			opts.LineageRootID, err = resolveLineageRootID(ctx, agentSvc, userID, nil)
			if err != nil {
				return wrapHubError(err)
			}
		} else {
			refAgent, err := resolveReferenceAgent(ctx, agentSvc, agentRef)
			if err != nil {
				return wrapHubError(err)
			}
			opts.LineageRootID, err = resolveLineageRootID(ctx, agentSvc, refAgent.ID, refAgent.Ancestry)
			if err != nil {
				return wrapHubError(err)
			}
			// Project-bound the query to the reference's own project,
			// matching cascadeMessageMode's scoping (ptone/scion#2146) — a
			// creation-tree neighborhood does not cross project boundaries.
			// Without --all this is already implied by
			// opts.ProjectID (set above); under --all it is the only thing
			// that keeps --lineage from searching every project.
			opts.ProjectID = refAgent.ProjectID
		}
	}

	resp, err := agentSvc.List(ctx, opts)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to list agents via Hub: %w", err))
	}

	// Warn on stderr when results are truncated
	if resp.Page.TotalCount > len(resp.Agents) {
		fmt.Fprintf(os.Stderr, "Warning: showing %d of %d agents. Use --count %d to see all.\n",
			len(resp.Agents), resp.Page.TotalCount, resp.Page.TotalCount)
	}

	// Convert Hub agents to local AgentInfo format
	agents := make([]api.AgentInfo, len(resp.Agents))
	for i, a := range resp.Agents {
		agents[i] = hubAgentToAgentInfo(a)
	}

	// Update agent name cache for completion
	updateAgentNameCache(resp.Agents)

	// Client-side enrichment: fetch broker/project names if not provided by Hub
	enrichAgentsClientSide(ctx, hubCtx.Client, agents)

	return displayAgents(agents, listAll, true)
}

// resolveOwnerID resolves --owner's value — a user's display name, email,
// Hub ID, or the literal "me" — to a Hub user ID. It follows the same
// ID-first-then-name-search convention as resolveBrokerByNameOrID (broker.go)
// so the three name-resolving flags (--owner, --broker, and agent-name
// resolution for the relationship flags) behave consistently.
func resolveOwnerID(ctx context.Context, client hubclient.Client, ownerRef string) (string, error) {
	ownerRef = strings.TrimSpace(ownerRef)

	if ownerRef == "me" {
		self, err := client.Auth().Me(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to resolve --owner me: %w", err)
		}
		return self.ID, nil
	}

	// Try as a direct user ID first.
	if user, err := client.Users().Get(ctx, ownerRef); err == nil {
		return user.ID, nil
	} else if !apiclient.IsNotFoundError(err) {
		return "", fmt.Errorf("failed to resolve --owner %q: %w", ownerRef, err)
	}

	// Fall back to a name/email search, paging through every result rather
	// than only the first page. Search is a substring match, so an exact
	// match can easily be pushed past the first page by other users sharing
	// the same substring (ptone/scion#2146).
	lower := strings.ToLower(ownerRef)
	var matches []hubclient.User
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxResolutionPages {
			return "", fmt.Errorf("--owner %q: too many matching users to search exhaustively; use the user ID instead", ownerRef)
		}
		resp, err := client.Users().List(ctx, &hubclient.ListUsersOptions{
			Search: ownerRef,
			Page:   apiclient.PageOptions{Cursor: cursor},
		})
		if err != nil {
			return "", fmt.Errorf("failed to search for user %q: %w", ownerRef, err)
		}
		for _, u := range resp.Users {
			if strings.ToLower(u.Email) == lower || strings.ToLower(u.DisplayName) == lower {
				matches = append(matches, u)
			}
		}
		if resp.Page.NextCursor == "" {
			break
		}
		cursor = resp.Page.NextCursor
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("--owner %q: no matching user found", ownerRef)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("--owner %q matches multiple users - use the user ID instead", ownerRef)
	}
}

// resolveRelationshipReference interprets a relationship flag's raw value.
// scopeInferSentinel is what cobra's NoOptDefVal substitutes when the flag is
// given with no explicit value (bare `--descendants`, as opposed to
// `--descendants=foo`) — see init() below.
//
// An explicit non-sentinel value always names an agent (by ID, slug, or
// name) and is returned as agentRef unchanged, in every CLI mode.
//
// A bare flag infers the reference (ptone/scion#2146 Q2):
//   - Agent mode: the calling agent, via SCION_AGENT_ID (the same env var
//     `scion whoami` treats as canonical) — returned as agentRef.
//   - Human or assistant mode: the calling user, resolved via the Hub's
//     current-session identity (`client.Auth().Me()`) — returned as userID.
//     There is no error case for "no calling principal" here: outside an
//     agent container the CLI is always driven by some authenticated user.
//
// Exactly one of agentRef/userID is non-empty on a nil error.
func resolveRelationshipReference(ctx context.Context, client hubclient.Client, flagValue string) (agentRef, userID string, err error) {
	if flagValue != scopeInferSentinel {
		return flagValue, "", nil
	}
	if resolveMode() == ModeAgent {
		id := os.Getenv("SCION_AGENT_ID")
		if id == "" {
			return "", "", fmt.Errorf("SCION_AGENT_ID is not set; cannot determine the calling agent")
		}
		return id, "", nil
	}
	self, err := client.Auth().Me(ctx)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve the calling user: %w", err)
	}
	return "", self.ID, nil
}

// resolveLineageRootID computes the --lineage root under the decided
// semantics (ptone/scion#2146, option (i) — a creation-tree neighborhood,
// NOT a messaging-permission query; see the flag help and cli.md, and
// store.AgentFilter.LineageRootID's doc for what the resulting predicate
// actually computes):
//
//   - Ancestry is empty: covers a user reference directly (a user never has
//     Ancestry) and a legacy, pre-ancestry-tracking agent. Root at self —
//     there is no parent to walk to.
//   - Ancestry has exactly one entry: that entry is EITHER the creating
//     user's ID (the common, top-level-agent case) OR another agent's ID.
//     The latter is real, not hypothetical: `createAgent`
//     (`handlers_agents_core.go`) and `createProjectAgent`
//     (`handlers_projects_core.go`) each set a child's Ancestry to
//     `creatorAgent.Ancestry + [creatorAgent.ID]` when the creator resolved
//     to an agent — so if that creator's OWN Ancestry was itself empty (its
//     `GetAgent` lookup failed at creation time, it was created by an
//     identity that is neither a user nor an agent, or it predates ancestry
//     tracking entirely), the child's Ancestry is `[creatorAgent.ID]`:
//     length 1, but an AGENT's ID, not a user's (ptone/scion#2146).
//     `len(ancestry)` alone cannot distinguish the two; only resolving what
//     the ID actually names can. So it is resolved through the same
//     authorized-list mechanism `resolveReferenceAgent` uses — never a bare
//     per-ID fetch outside list authorization (see
//     `isAncestryEntryAnAgent`) — and roots at that agent if found, or at
//     the reference itself otherwise (a user, or an agent the caller
//     cannot see — both must be indistinguishable to the caller, so both
//     root at self).
//   - Ancestry has two or more entries: the last entry is always the direct
//     parent AGENT's ID by construction — `createAgent`/`createProjectAgent`
//     only ever append when the creator resolved to an agent, so at this
//     length the appended, last entry is unconditionally that creator
//     agent's own ID,
//     never a user's (a user can only ever appear as Ancestry[0], the
//     original creator at the base of the chain, never later). No lookup
//     is needed at this length. (This is the same definition
//     `isDirectParentChild` uses in `pkg/hub/authorize_message.go` for
//     branch-mode messaging — reused here only as "the direct creator",
//     not for any messaging implication.)
//
// Only the length-1 case needs a network call, and that call resolves
// through the caller's own authorized list (the same choke point the final
// narrowing query uses), so it can never confirm an unauthorized ID is an
// agent, let alone use it as a lineage root undetected. Whatever ID this
// function returns is handed to store.AgentFilter.LineageRootID (via
// ListAgentsOptions.LineageRootID), which is itself ANDed with the caller's
// authorized scope, so an unauthorized or nonexistent root simply yields no
// matches rather than an error or a disclosure.
func resolveLineageRootID(ctx context.Context, agentSvc hubclient.AgentService, id string, ancestry []string) (string, error) {
	switch len(ancestry) {
	case 0:
		return id, nil
	case 1:
		parent, isAgent, err := isAncestryEntryAnAgent(ctx, agentSvc, ancestry[0])
		if err != nil {
			return "", err
		}
		if !isAgent {
			return id, nil
		}
		return parent.ID, nil
	default:
		return ancestry[len(ancestry)-1], nil
	}
}

// isAncestryEntryAnAgent resolves ancestryEntry — a literal principal ID
// taken from an Ancestry array, always a UUID, never a name or slug — to an
// agent via the same authorized-list IDs-narrowing mechanism
// resolveReferenceAgent uses (ptone/scion#2146), rather than a bare per-ID
// GET that would risk confirming an unauthorized ID is an agent.
// It returns (agent, true, nil) when ancestryEntry names a visible agent,
// and (nil, false, nil) — not an error — when it does not: almost always
// because it is a user's ID (the common case), but also an agent the caller
// is not authorized to see, which must be treated identically (never
// distinguished from "it's a user" by the caller). A non-nil error is
// reserved for a genuine resolution failure (e.g. a network error), never
// for a plain not-found/zero-result outcome.
func isAncestryEntryAnAgent(ctx context.Context, agentSvc hubclient.AgentService, ancestryEntry string) (match *hubclient.Agent, isAgent bool, err error) {
	resp, err := agentSvc.List(ctx, &hubclient.ListAgentsOptions{IDs: []string{ancestryEntry}})
	if err != nil {
		return nil, false, fmt.Errorf("failed to resolve ancestry entry %q: %w", ancestryEntry, err)
	}
	// Only trust a result whose ID actually equals ancestryEntry, mirroring
	// resolveReferenceAgent's own skepticism of the response shape — a Hub
	// that predates ptone/scion#2146 and silently ignores the `id` query
	// param would otherwise hand back an arbitrary agent, which this
	// authz-adjacent path (its result becomes the --lineage root) must never
	// trust blindly.
	var found *hubclient.Agent
	for i := range resp.Agents {
		if resp.Agents[i].ID == ancestryEntry {
			if found != nil {
				// IDs is a single-element set; the server should never
				// return more than one exact-ID match. Fail loud rather
				// than guess which one, same as resolveReferenceAgent.
				return nil, false, fmt.Errorf("ancestry entry %q unexpectedly matched more than one record", ancestryEntry)
			}
			found = &resp.Agents[i]
		}
	}
	if found == nil {
		return nil, false, nil
	}
	return found, true, nil
}

// maxResolutionPages bounds how many pages resolveOwnerID and
// resolveReferenceAgent will fetch while searching for an exact name/email
// match, so a misbehaving or never-terminating cursor cannot hang the CLI
// forever (ptone/scion#2146).
const maxResolutionPages = 1000

// resolveReferenceAgent resolves ref (an agent ID, slug, or name) to the full
// agent record. It resolves through agentSvc's authorized *list* — the same
// choke point the final narrowing query uses — rather than treating a
// single-resource GET as authoritative.
//
// GET is tried first as a fast, cheap path that is correct whenever it
// succeeds (notably when ref is the caller's own ID). But many agent
// identities are denied GET on any agent other than themselves with a plain
// 403, even though the identical agent is visible through the list endpoint
// (ptone/scion#2146) — using GET as the primary path silently failed
// --descendants=<peer> and --ancestors=<peer> for exactly the
// audience (agents naming a sibling) these flags exist for. So both 404 and
// 403 fall through to list-based resolution below, never just 404.
func resolveReferenceAgent(ctx context.Context, agentSvc hubclient.AgentService, ref string) (*hubclient.Agent, error) {
	if a, err := agentSvc.Get(ctx, ref); err == nil {
		return a, nil
	} else if !apiclient.IsNotFoundError(err) && !apiclient.IsForbiddenError(err) {
		return nil, fmt.Errorf("failed to resolve agent %q: %w", ref, err)
	}

	// If ref looks like an agent ID, ask the list endpoint to narrow
	// directly to it — the same IDs mechanism --ancestors already uses —
	// instead of paging through every agent for what is usually the common
	// case.
	if _, err := uuid.Parse(ref); err == nil {
		resp, err := agentSvc.List(ctx, &hubclient.ListAgentsOptions{IDs: []string{ref}})
		if err != nil {
			return nil, fmt.Errorf("failed to resolve agent %q: %w", ref, err)
		}
		// Only trust a result whose ID actually equals ref (ptone/scion#2146,
		// mirroring isAncestryEntryAnAgent's own hardening in the other
		// direction): a Hub that predates this change and silently ignores
		// the `id` query param would otherwise hand back an arbitrary agent
		// — in a project with
		// exactly one visible agent, that agent, for ANY UUID reference.
		// The final list query is still authz-bounded regardless, so this
		// is a correctness fix, not a leak fix.
		var match *hubclient.Agent
		for i := range resp.Agents {
			if resp.Agents[i].ID == ref {
				if match != nil {
					return nil, fmt.Errorf("agent %q unexpectedly matched more than one record", ref)
				}
				match = &resp.Agents[i]
			}
		}
		if match == nil {
			return nil, fmt.Errorf("agent %q not found", ref)
		}
		return match, nil
	}

	// Not a UUID: page through the full authorized list, matching by slug or
	// name, until an exact match is found or the list is exhausted
	// (ptone/scion#2146 — matching only the first page silently missed real
	// agents on a large, `--all`-scoped hub).
	var matches []hubclient.Agent
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxResolutionPages {
			return nil, fmt.Errorf("agent %q: too many agents to search exhaustively; use the agent ID instead", ref)
		}
		resp, err := agentSvc.List(ctx, &hubclient.ListAgentsOptions{Page: apiclient.PageOptions{Cursor: cursor}})
		if err != nil {
			return nil, fmt.Errorf("failed to look up agent %q: %w", ref, err)
		}
		for i := range resp.Agents {
			a := resp.Agents[i]
			if a.Slug == ref || a.Name == ref {
				matches = append(matches, a)
			}
		}
		if resp.Page.NextCursor == "" {
			break
		}
		cursor = resp.Page.NextCursor
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("agent %q not found", ref)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("agent %q matches multiple agents - use the agent ID instead", ref)
	}
}

// enrichAgentsClientSide populates project and RuntimeBrokerName fields client-side
// when the Hub doesn't provide them (for backwards compatibility with older Hubs).
func enrichAgentsClientSide(ctx context.Context, client hubclient.Client, agents []api.AgentInfo) {
	// Collect unique IDs that need enrichment
	brokerIDs := make(map[string]struct{})
	projectIDs := make(map[string]struct{})
	for _, a := range agents {
		if a.RuntimeBrokerName == "" && a.RuntimeBrokerID != "" {
			brokerIDs[a.RuntimeBrokerID] = struct{}{}
		}
		if a.Project == "" && a.ProjectID != "" {
			projectIDs[a.ProjectID] = struct{}{}
		}
	}

	// Fetch broker names
	brokerNames := make(map[string]string)
	for id := range brokerIDs {
		if broker, err := client.RuntimeBrokers().Get(ctx, id); err == nil {
			brokerNames[id] = broker.Name
		}
	}

	// Fetch project names
	projectNames := make(map[string]string)
	for id := range projectIDs {
		if project, err := client.Projects().Get(ctx, id); err == nil {
			projectNames[id] = project.Name
		}
	}

	// Apply enrichment
	for i := range agents {
		if agents[i].RuntimeBrokerName == "" {
			if name, ok := brokerNames[agents[i].RuntimeBrokerID]; ok {
				agents[i].RuntimeBrokerName = name
			}
		}
		if agents[i].Project == "" {
			if name, ok := projectNames[agents[i].ProjectID]; ok {
				agents[i].Project = name
			}
		}
	}
}

// hubAgentToAgentInfo converts a Hub API Agent to a local AgentInfo
func hubAgentToAgentInfo(a hubclient.Agent) api.AgentInfo {
	// Map to Phase/Activity for api.AgentInfo.
	// Prefer structured Phase/Activity fields; fall back to legacy Status field.
	phase, activity := hubAgentPhaseActivity(a.Phase, a.Activity, a.Status)

	// Prefer slug for display name to ensure consistent case-insensitive naming
	displayName := a.Slug
	if displayName == "" {
		displayName = a.Name
	}
	info := api.AgentInfo{
		ID:                a.ID,
		Slug:              a.Slug,
		ContainerID:       a.ContainerID,
		Name:              displayName,
		Template:          a.Template,
		HarnessConfig:     a.HarnessConfig,
		HarnessAuth:       a.HarnessAuth,
		Project:           a.Project,
		ProjectID:         a.ProjectID,
		Labels:            a.Labels,
		Annotations:       a.Annotations,
		Phase:             phase,
		Activity:          activity,
		ContainerStatus:   a.ContainerStatus,
		Image:             a.Image,
		Detached:          a.Detached,
		Runtime:           a.Runtime,
		RuntimeBrokerID:   a.RuntimeBrokerID,
		RuntimeBrokerName: a.RuntimeBrokerName,
		RuntimeBrokerType: a.RuntimeBrokerType,
		RuntimeState:      a.RuntimeState,
		WebPTYEnabled:     a.WebPTYEnabled,
		TaskSummary:       a.TaskSummary,
		Created:           a.Created,
		Updated:           a.Updated,
		LastSeen:          a.LastSeen,
		LastActivityEvent: a.LastActivityEvent,
		DeletedAt:         a.DeletedAt,
		CreatedBy:         a.CreatedBy,
		OwnerID:           a.OwnerID,
		StateVersion:      a.StateVersion,
	}

	// Fall back to AppliedConfig fields if top-level fields are empty
	// (for backward compatibility with older Hubs that don't enrich these)
	if info.HarnessConfig == "" && a.AppliedConfig != nil && a.AppliedConfig.HarnessConfig != "" {
		info.HarnessConfig = a.AppliedConfig.HarnessConfig
	}
	if info.HarnessAuth == "" && a.AppliedConfig != nil && a.AppliedConfig.HarnessAuth != "" {
		info.HarnessAuth = a.AppliedConfig.HarnessAuth
	}

	// Convert Kubernetes info if present
	if a.Kubernetes != nil {
		info.Kubernetes = &api.AgentK8sMetadata{
			Cluster:   a.Kubernetes.Cluster,
			Namespace: a.Kubernetes.Namespace,
			PodName:   a.Kubernetes.PodName,
			SyncedAt:  a.Kubernetes.SyncedAt,
		}
	}

	return info
}

// displayAgents displays agents in the requested format
// hubMode indicates if the listing is from Hub (shows BROKER column)
// filterRunningAgents returns only agents whose phase is not stopped or error.
func filterRunningAgents(agents []api.AgentInfo) []api.AgentInfo {
	filtered := make([]api.AgentInfo, 0, len(agents))
	for _, a := range agents {
		p := state.Phase(a.Phase)
		if p == state.PhaseStopped || p == state.PhaseError {
			continue
		}
		filtered = append(filtered, a)
	}
	return filtered
}

// validateListFlags checks that filter and sort flag values are valid.
func validateListFlags() error {
	if listCount < 0 {
		return fmt.Errorf("invalid --count value %d: must be non-negative", listCount)
	}
	if filterPhase != "" {
		filterPhase = strings.ToLower(filterPhase)
		if !state.Phase(filterPhase).IsValid() {
			valid := make([]string, 0, len(state.Phases()))
			for _, p := range state.Phases() {
				valid = append(valid, string(p))
			}
			return fmt.Errorf("invalid phase %q; valid values: %s", filterPhase, strings.Join(valid, ", "))
		}
	}
	if filterActivity != "" {
		filterActivity = strings.ToLower(filterActivity)
		if !state.Activity(filterActivity).IsValid() {
			valid := make([]string, 0, len(state.Activities()))
			for _, a := range state.Activities() {
				valid = append(valid, string(a))
			}
			return fmt.Errorf("invalid activity %q; valid values: %s", filterActivity, strings.Join(valid, ", "))
		}
	}
	if sortField != "" {
		sortField = strings.ToLower(sortField)
		if !validSortFields[sortField] {
			valid := make([]string, 0, len(validSortFields))
			for k := range validSortFields {
				valid = append(valid, k)
			}
			sort.Strings(valid)
			return fmt.Errorf("invalid sort field %q; valid values: %s", sortField, strings.Join(valid, ", "))
		}
	}
	return nil
}

// filterAgentsByFlags applies --phase, --activity, and --template filters.
func filterAgentsByFlags(agents []api.AgentInfo) []api.AgentInfo {
	if filterPhase == "" && filterActivity == "" && filterTemplate == "" {
		return agents
	}
	filtered := make([]api.AgentInfo, 0, len(agents))
	for _, a := range agents {
		if filterPhase != "" && !strings.EqualFold(a.Phase, filterPhase) {
			continue
		}
		if filterActivity != "" && !strings.EqualFold(a.Activity, filterActivity) {
			continue
		}
		if filterTemplate != "" && !strings.EqualFold(a.Template, filterTemplate) {
			continue
		}
		filtered = append(filtered, a)
	}
	return filtered
}

// sortAgentsByField sorts agents by the --sort field.
func sortAgentsByField(agents []api.AgentInfo) {
	if sortField == "" {
		return
	}
	sort.SliceStable(agents, func(i, j int) bool {
		var less bool
		switch sortField {
		case "name":
			less = strings.ToLower(agents[i].Name) < strings.ToLower(agents[j].Name)
		case "phase":
			less = agents[i].Phase < agents[j].Phase
		case "created":
			less = agents[i].Created.Before(agents[j].Created)
		case "updated":
			less = agents[i].Updated.Before(agents[j].Updated)
		case "last-seen":
			ti := agents[i].LastActivityEvent
			if ti.IsZero() {
				ti = agents[i].LastSeen
			}
			tj := agents[j].LastActivityEvent
			if tj.IsZero() {
				tj = agents[j].LastSeen
			}
			less = ti.Before(tj)
		default:
			return false
		}
		// Timestamps default to descending (newest first); name/phase default to ascending
		descByDefault := sortField == "created" || sortField == "updated" || sortField == "last-seen"
		if descByDefault != sortReverse {
			return !less
		}
		return less
	})
}

func displayAgents(agents []api.AgentInfo, all bool, hubMode bool) error {
	if listRunning {
		agents = filterRunningAgents(agents)
	}

	// Resolve human-friendly template names from raw values that may
	// contain cache paths or remote URIs (mirrors the 813307c fix for
	// the tmux footer).
	for i := range agents {
		agents[i].Template = config.FriendlyTemplateName(agents[i].Template)
	}

	// Apply --phase, --activity, --template filters
	agents = filterAgentsByFlags(agents)

	if sortField != "" {
		sortAgentsByField(agents)
	} else if sortByTime {
		sort.Slice(agents, func(i, j int) bool {
			ti := agents[i].LastActivityEvent
			if ti.IsZero() {
				ti = agents[i].LastSeen
			}
			tj := agents[j].LastActivityEvent
			if tj.IsZero() {
				tj = agents[j].LastSeen
			}
			return ti.After(tj)
		})
	}

	if outputFormat == "json" {
		if agents == nil {
			agents = []api.AgentInfo{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(agents)
	}

	if len(agents) == 0 {
		if all {
			fmt.Println("No active agents found across any projects.")
		} else {
			fmt.Println("No active agents found in the current project.")
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if hubMode {
		_, _ = fmt.Fprintln(w, "NAME\tTEMPLATE\tHARNESS-CFG\tRUNTIME\tPROJECT\tBROKER\tPHASE\tCONTAINER\tLAST ACTIVITY")
	} else {
		_, _ = fmt.Fprintln(w, "NAME\tTEMPLATE\tHARNESS-CFG\tRUNTIME\tPROJECT\tPHASE\tCONTAINER\tLAST ACTIVITY")
	}
	for _, a := range agents {
		phase := a.Phase
		if phase == "" {
			phase = "unknown"
		}
		if phase == string(state.PhaseStopped) && state.Activity(a.Activity).IsTerminal() {
			phase = a.Activity
		}
		containerStatus := a.ContainerStatus
		if containerStatus == "created" && a.ID == "" {
			containerStatus = "none"
		}
		harnessConfig := a.HarnessConfig
		if harnessConfig == "" {
			harnessConfig = "-"
		}
		activityTime := a.LastActivityEvent
		if activityTime.IsZero() {
			activityTime = a.LastSeen
		}
		lastActivity := formatLastActivity(a.Activity, activityTime)
		// Use broker name if available, otherwise fall back to ID
		broker := a.RuntimeBrokerName
		if broker == "" {
			broker = a.RuntimeBrokerID
		}
		if hubMode {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Template, harnessConfig, a.Runtime, a.Project, broker, phase, containerStatus, lastActivity)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Template, harnessConfig, a.Runtime, a.Project, phase, containerStatus, lastActivity)
		}
	}
	_ = w.Flush()
	return nil
}

// formatLastActivity formats a status and timestamp as a combined "activity, time ago" string.
func formatLastActivity(status string, t time.Time) string {
	timePart := "-"
	if !t.IsZero() {
		timePart = clitime.Ago(t)
	}
	if status == "" || status == "WORKING" || status == "working" {
		return timePart
	}
	if timePart == "-" {
		return status
	}
	return fmt.Sprintf("%s, %s", status, timePart)
}

// handleUnlinkedProjectPrompt checks if the error is due to an unlinked project and prompts the user.
// Returns true if the user made a choice that might resolve the issue (link or disable).
func handleUnlinkedProjectPrompt(cmd *cobra.Command, args []string) bool {
	// Resolve project path to check settings
	resolvedPath, isGlobal, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return false
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return false
	}

	// Only handle this case if Hub is enabled but project is not linked
	if !settings.IsHubEnabled() {
		return false
	}

	// Check if project is actually registered on the Hub
	endpoint := GetHubEndpoint(settings)
	if endpoint == "" {
		return false
	}

	client, err := getHubClient(settings)
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check Hub connectivity first
	if _, err := client.Health(ctx); err != nil {
		return false // Hub not reachable, different error
	}

	// Check if project is registered — prefer hub.projectId over project_id
	projectID := settings.GetHubProjectID()
	if projectID == "" {
		projectID = settings.ProjectID
	}
	if projectID == "" {
		projectID = config.GenerateProjectID()
	}

	linked, err := isProjectLinkedToHub(ctx, client, projectID)
	if err != nil || linked {
		return false // Error checking or project is already linked
	}

	// Get project name for display
	var projectName string
	if isGlobal {
		projectName = "global"
	} else {
		projectName = config.GetProjectName(resolvedPath)
	}

	// Show prompt
	choice := hubsync.ShowProjectLinkOrDisablePrompt(projectName, autoConfirm)

	switch choice {
	case hubsync.LinkOrDisableLink:
		// Run the link command
		if err := runHubLink(cmd, args); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to link project: %v\n", err)
			return false
		}
		return true
	case hubsync.LinkOrDisableDisable:
		// Disable Hub for this project
		if err := config.UpdateSetting(resolvedPath, "hub.enabled", "false", isGlobal); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to disable Hub: %v\n", err)
			return false
		}
		statusln("Hub integration disabled for this project.")
		return true
	default:
		return false
	}
}

// isProjectLinkedToHub checks if a project is linked to the Hub.
func isProjectLinkedToHub(ctx context.Context, client hubclient.Client, projectID string) (bool, error) {
	if projectID == "" {
		return false, nil
	}

	_, err := client.Projects().Get(ctx, projectID)
	if err != nil {
		errStr := err.Error()
		if containsStr(errStr, "404") || containsStr(errStr, "not found") {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// containsStr is a simple case-sensitive substring check.
func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// hubAgentPhaseActivity returns the phase and activity for a Hub agent,
// preferring the structured Phase/Activity fields from the API response
// and falling back to deriving them from the legacy Status field.
func hubAgentPhaseActivity(phase, activity, status string) (string, string) {
	if phase != "" {
		return phase, activity
	}
	return hubStatusToPhaseActivity(status)
}

// hubStatusToPhaseActivity maps a hubclient Status string to Phase and Activity.
// The Hub API may return a single Status field that represents either a phase
// or an activity (e.g. "running", "stopped", "waiting_for_input").
func hubStatusToPhaseActivity(status string) (string, string) {
	// Terminal activities (crashed, limits_exceeded) belong to the stopped phase.
	a := state.Activity(status)
	if a.IsTerminal() {
		return string(state.PhaseStopped), status
	}
	// Check if the status is a known activity (only valid during running phase)
	if a.IsValid() && a != "" {
		return string(state.PhaseRunning), status
	}
	// Check if it is a known phase
	p := state.Phase(status)
	if p.IsValid() {
		return status, ""
	}
	// Unknown value — treat as phase for backward compat
	if status == "" {
		return "", ""
	}
	return status, ""
}

// updateAgentNameCache updates the agent name cache with the given Hub agents.
// This is called after successful Hub API calls to keep the completion cache fresh.
func updateAgentNameCache(agents []hubclient.Agent) {
	if len(agents) == 0 {
		return
	}

	// Extract agent names
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}

	// Generate cache key for the current project path
	resolvedPath, _ := config.GetResolvedProjectDir(projectPath)
	if resolvedPath == "" {
		return
	}

	cacheKey := agentcache.GenerateCacheKey(resolvedPath)

	// Write to cache (silently ignore errors)
	_ = agentcache.WriteCache(cacheKey, names)
}

func init() {
	rootCmd.AddCommand(listCmd)
	listCmd.Flags().BoolVarP(&listAll, "all", "a", false, "List all agents across all projects")
	listCmd.Flags().BoolVar(&listDeleted, "deleted", false, "Include soft-deleted agents in listing")
	listCmd.Flags().BoolVarP(&listRunning, "running", "r", false, "Only show agents that are not stopped or errored")
	listCmd.Flags().BoolVarP(&sortByTime, "time", "t", false, "Sort by last activity, most recent first")
	listCmd.Flags().StringVar(&filterPhase, "phase", "", "Filter by lifecycle phase (running, stopped, error, ...)")
	listCmd.Flags().StringVar(&filterActivity, "activity", "", "Filter by runtime activity (thinking, waiting_for_input, ...)")
	listCmd.Flags().StringVar(&filterTemplate, "template", "", "Filter by template name")
	listCmd.Flags().StringVar(&sortField, "sort", "", "Sort by field (name, phase, created, updated, last-seen)")
	listCmd.Flags().BoolVar(&sortReverse, "reverse", false, "Reverse sort order")
	listCmd.Flags().StringArrayVar(&filterLabels, "label", nil, "Filter by label in key=value format (repeatable)")
	listCmd.Flags().IntVar(&listCount, "count", 0, "Maximum number of agents to return (default: server limit)")

	// Attribute filters (Hub mode only).
	listCmd.Flags().StringVar(&filterOwner, "owner", "", "Filter by owner: a user ID, name, email, or the reserved value \"me\" (Hub mode only)")
	listCmd.Flags().StringVar(&filterBroker, "broker", "", "Filter by runtime broker name or ID (Hub mode only)")
	listCmd.Flags().StringVar(&filterHarness, "harness", "", "Filter by harness-config name (Hub mode only)")

	// Relationship filters (Hub mode only). NoOptDefVal lets both
	// `--descendants` and `--descendants=<agent>` parse: the bare form
	// infers the reference (calling agent in agent mode, calling user
	// otherwise) — see resolveRelationshipReference.
	listCmd.Flags().StringVar(&filterDescendants, "descendants", "", "List every agent descended from the reference (default: self) (Hub mode only)")
	listCmd.Flags().Lookup("descendants").NoOptDefVal = scopeInferSentinel
	listCmd.Flags().StringVar(&filterAncestors, "ancestors", "", "List the agents in the reference's ancestry chain (default: self) (Hub mode only)")
	listCmd.Flags().Lookup("ancestors").NoOptDefVal = scopeInferSentinel
	listCmd.Flags().StringVar(&filterLineage, "lineage", "", "List the reference's creation-tree neighborhood: its direct parent agent plus all of that parent's descendants, bounded to the reference's project. A reference whose parent is a user, or whose only recorded parent is an agent you cannot see, is its own root (default: self) (Hub mode only)")
	listCmd.Flags().Lookup("lineage").NoOptDefVal = scopeInferSentinel
	listCmd.MarkFlagsMutuallyExclusive("descendants", "ancestors", "lineage")
}
