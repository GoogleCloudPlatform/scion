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
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Delegated lists (.design/agent-delegation.md §11.8).
//
// A list request made with an agent delegated credential takes its scope
// only from the delegated list arm (delegatedListScope): the authorizing
// user's own list scope for the operation's primary permission,
// intersected with the grant boundary. Every candidate item is then decided
// by the item filter (delegatedListItemRead) for the route's item-filter
// permission, through the same delegated procedure as a single request,
// including the authorizing user's project admission on the item's own
// project. Paging goes through listAuthorizedOrAll's per-item scan, so
// items, totals and the sealed resume cursor all follow those decisions.
//
// A list request writes one primary decision record (the list arm) and one
// aggregated item-filter record (store.DecisionAuditRecordKindListFilter),
// flushed by routeGuard when the request ends.

// errCodeDelegatedListSortUnsupported is the 400 a delegated credential gets
// for a sorted (sort=) or ID-filtered (ids=) hub agent list: a delegated
// list serves the default order only.
const errCodeDelegatedListSortUnsupported = "delegated_list_sort_unsupported"

// errDelegatedListAdmissionSet is returned when a request's list admission
// is set a second time.
var errDelegatedListAdmissionSet = errors.New("delegated list admission already set")

// delegatedListMode is the shape of a delegated list scope.
type delegatedListMode int

const (
	// delegatedListNone lists nothing.
	delegatedListNone delegatedListMode = iota
	// delegatedListExplicit lists items of an explicit project set.
	delegatedListExplicit
	// delegatedListAllProjects lists items of any project, less the
	// excluded ones. Only a hub-bounded grant whose authorizing user has
	// an unrestricted list scope gets it.
	delegatedListAllProjects
)

// delegatedListScope is the tagged scope the delegated list arm returns.
// Whatever the mode, an item is returned only when its own project is in
// the scope, admits the authorizing user for the item permission, and the
// item filter allows the item.
type delegatedListScope struct {
	Mode delegatedListMode
	// ProjectIDs is the explicit project set (Explicit mode): the
	// authorizing user's set intersected with the grant boundary.
	ProjectIDs []string
	// Excluded lists projects an access constraint removes from an
	// unrestricted scope (AllProjects mode).
	Excluded []string
}

// admitsProject reports whether projectID is inside the scope.
func (sc delegatedListScope) admitsProject(projectID string) bool {
	if projectID == "" {
		return false
	}
	switch sc.Mode {
	case delegatedListExplicit:
		return slices.Contains(sc.ProjectIDs, projectID)
	case delegatedListAllProjects:
		return !slices.Contains(sc.Excluded, projectID)
	default:
		return false
	}
}

// listItemOutcome is the item filter's decision for one item.
type listItemOutcome uint8

const (
	listItemAllowed listItemOutcome = iota + 1
	listItemDenied
	listItemDroppedByAdmission
)

// listFilterCollector is the request-scoped state of a delegated list's
// item filter. routeGuard installs one for every admitted list route and
// flushes it once when the request ends. Each distinct item is decided and
// counted once, however many passes of the list examine it.
type listFilterCollector struct {
	itemPermission string

	mu sync.Mutex
	// started is set when the list arm runs; a request that never reached
	// it writes no aggregated record.
	started bool
	// complete is set when the handler finished its list.
	complete bool
	flushed  bool
	// resourceType is the item resource type, recorded on the aggregated
	// record.
	resourceType string
	scope        delegatedListScope
	scopeSet     bool
	// itemChecked and itemCode cache the per-request part of the item
	// decision (the route, chain and permission checks): "" when it
	// passed, otherwise the code every item is denied with.
	itemChecked bool
	itemCode    string
	itemState   *delegatedRequestState

	decided   map[string]listItemOutcome
	allowed   int
	denied    int
	dropped   int
	deniedIDs []string
	truncated bool
}

func newListFilterCollector(itemPermission string) *listFilterCollector {
	return &listFilterCollector{itemPermission: itemPermission, decided: map[string]listItemOutcome{}}
}

type listFilterCollectorKey struct{}

func contextWithListFilterCollector(ctx context.Context, c *listFilterCollector) context.Context {
	return context.WithValue(ctx, listFilterCollectorKey{}, c)
}

func listFilterCollectorFromContext(ctx context.Context) *listFilterCollector {
	c, _ := ctx.Value(listFilterCollectorKey{}).(*listFilterCollector)
	return c
}

// setDelegatedListAdmission records the request's list scope. The list arm
// sets it exactly once; a second call is a server error.
func (c *listFilterCollector) setDelegatedListAdmission(resourceType string, scope delegatedListScope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scopeSet {
		return errDelegatedListAdmissionSet
	}
	c.resourceType = resourceType
	c.scope = scope
	c.scopeSet = true
	return nil
}

// start marks that the list arm ran, so the request writes its aggregated
// record.
func (c *listFilterCollector) start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = true
}

// markComplete records that the handler finished its list.
func (c *listFilterCollector) markComplete() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.complete = true
}

// allowedItem reports whether the item filter allowed resource in this
// request.
func (c *listFilterCollector) allowedItem(resource Resource) bool {
	if c == nil || resource.ID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return resource.Type == c.resourceType && c.decided[resource.ID] == listItemAllowed
}

// record stores the outcome of a newly decided item.
func (c *listFilterCollector) record(id string, outcome listItemOutcome) {
	c.decided[id] = outcome
	switch outcome {
	case listItemAllowed:
		c.allowed++
		return
	case listItemDroppedByAdmission:
		c.dropped++
	default:
		c.denied++
	}
	if len(c.deniedIDs) < store.DecisionAuditListFilterMaxDeniedIDs {
		c.deniedIDs = append(c.deniedIDs, id)
	} else {
		c.truncated = true
	}
}

// summary returns the aggregated record's list-filter block and whether
// the record is due: the list arm ran and no record was written yet. It
// marks the collector flushed.
func (c *listFilterCollector) summary() (store.DecisionAuditListFilter, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started || c.flushed {
		return store.DecisionAuditListFilter{}, "", false
	}
	c.flushed = true
	return store.DecisionAuditListFilter{
		ItemPermission:     c.itemPermission,
		Allowed:            c.allowed,
		Denied:             c.denied,
		DroppedByAdmission: c.dropped,
		DeniedIDs:          append([]string(nil), c.deniedIDs...),
		Truncated:          c.truncated,
		Incomplete:         !c.complete,
	}, c.resourceType, true
}

// delegatedListScope is the delegated list arm (.design/agent-delegation.md
// §11.8) for the list permission permissionID on items of resourceType. It
// runs the route, chain and permission checks of a delegated request for
// permissionID (the operation's primary permission), resolves the
// authorizing user's own list scope for it and intersects that with the
// grant boundary. It writes the request's primary decision record and sets
// the request's list admission. Any failure, and an empty scope, is a
// delegatedListNone scope with a deny record; err is set only for a server
// error, after which the caller still answers with an empty list.
func (s *Server) delegatedListScope(ctx context.Context, d *DelegatedAgentIdentity, resourceType, permissionID string) (delegatedListScope, error) {
	a := s.authzService
	none := delegatedListScope{Mode: delegatedListNone}
	request := AuthzRequest{
		Principal:  principalContextForIdentity(d),
		Credential: credentialContextForIdentity(d),
		Resource:   Resource{Type: resourceType},
		Action:     ActionList,
		Permission: permissionID,
	}
	c := listFilterCollectorFromContext(ctx)
	deny := func(code string) {
		a.emitAgentDelegationDecision(ctx, request, a.agentDelegationDeny(request, d, code))
		if c != nil {
			_ = c.setDelegatedListAdmission(resourceType, none)
		}
	}
	if c == nil {
		// routeGuard installs a collector on every admitted list route.
		deny(agentDelegationCodeCredentialNotAdmitted)
		return none, errors.New("delegated list: no item filter for this request")
	}
	c.start()

	st, spec, entry, code := a.agentDelegationRouteChecks(ctx, d)
	if code == "" {
		code = a.agentDelegationPermissionChecks(d, st, spec, entry, permissionID, false)
	}
	if code != "" {
		deny(code)
		return none, nil
	}

	issuerScope, err := a.resolveListScopesForPrincipal(ctx, st.issuerPC, permissionID)
	if err != nil {
		deny(agentDelegationCodeLookupError)
		return none, nil
	}
	scopes := issuerScope.Scopes
	boundary := d.Boundary()
	switch boundary.Kind {
	case BoundaryKindProject:
		scopes = scopes.Intersection(ScopeSetExplicit(boundary.ProjectID))
	case BoundaryKindHub:
	default:
		deny(agentDelegationCodeGrantBoundaryInvalid)
		return none, nil
	}

	scope := delegatedListScope{Mode: delegatedListExplicit}
	switch {
	case scopes.IsNone():
		deny(agentDelegationCodeIssuerAuthority)
		return none, nil
	case scopes.IsAll():
		scope.Mode = delegatedListAllProjects
		scope.Excluded = canonicalizeStringSlice(append([]string(nil), issuerScope.ExcludedProjectIDs...))
	default:
		// An access constraint excluding a project from an unrestricted
		// scope still excludes it after the boundary intersection.
		for _, pid := range scopes.ProjectIDs() {
			if !slices.Contains(issuerScope.ExcludedProjectIDs, pid) {
				scope.ProjectIDs = append(scope.ProjectIDs, pid)
			}
		}
		if len(scope.ProjectIDs) == 0 {
			deny(agentDelegationCodeIssuerAuthority)
			return none, nil
		}
		scope.ProjectIDs = canonicalizeStringSlice(scope.ProjectIDs)
	}

	if err := c.setDelegatedListAdmission(resourceType, scope); err != nil {
		a.emitAgentDelegationDecision(ctx, request, a.agentDelegationDeny(request, d, agentDelegationCodeEvaluationError))
		return none, err
	}
	a.emitAgentDelegationDecision(ctx, request, a.agentDelegationAllow(request, d, st, nil))
	return scope, nil
}

// emitAgentDelegationDecision writes decision through Decide's audit exit
// (the same sampling rule and record builder), for a delegated decision
// made outside Decide.
func (a *AuthzService) emitAgentDelegationDecision(ctx context.Context, request AuthzRequest, decision Decision) {
	if a.decisionAuditEmitter != nil {
		a.emitDecisionAudit(ctx, request, decision)
	}
}

// delegatedListItemRead is the item filter of a delegated list, in the
// shape listAuthorizedOrAll's per-item scan takes. Each item is allowed only
// when the request's list admission is set, the route, chain and
// permission checks pass for the route's item-filter permission, the
// item's project is inside the list scope, and the authorizing user's live
// authority (including project admission on that project) allows the item
// permission on the item. It never calls Decide and writes no record of its
// own: outcomes go to the request's collector, which writes the one
// aggregated record.
func (s *Server) delegatedListItemRead(ctx context.Context, identity Identity, resources []Resource) ([]bool, error) {
	allowed := make([]bool, len(resources))
	d, _ := identity.(*DelegatedAgentIdentity)
	c := listFilterCollectorFromContext(ctx)
	if d == nil || c == nil {
		return allowed, nil
	}
	a := s.authzService

	// c.mu is held across the store reads below (the route checks and each
	// EvaluateBearerCeiling). That is safe: the collector belongs to this one
	// request, and nothing called under the lock re-enters the collector. Do
	// not call anything that reads it (allowedItem, capabilities) from here.
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.itemChecked {
		c.itemChecked = true
		st, spec, entry, code := a.agentDelegationRouteChecks(ctx, d)
		if code == "" {
			code = a.agentDelegationPermissionChecks(d, st, spec, entry, c.itemPermission, true)
		}
		c.itemCode, c.itemState = code, st
	}

	for i, r := range resources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if r.ID == "" {
			continue
		}
		if outcome, seen := c.decided[r.ID]; seen {
			allowed[i] = outcome == listItemAllowed
			continue
		}
		outcome := c.decideItem(ctx, a, d, r)
		c.record(r.ID, outcome)
		allowed[i] = outcome == listItemAllowed
	}
	return allowed, nil
}

// decideItem decides one item that was not decided yet in this request.
// c.mu is held.
func (c *listFilterCollector) decideItem(ctx context.Context, a *AuthzService, d *DelegatedAgentIdentity, r Resource) listItemOutcome {
	if !c.scopeSet || r.Type != c.resourceType {
		return listItemDroppedByAdmission
	}
	if c.itemCode != "" {
		return listItemDenied
	}
	projectID, ok := projectOfListItem(r)
	if !ok || !c.scope.admitsProject(projectID) {
		return listItemDroppedByAdmission
	}
	_, code := a.agentDelegationTargetCheck(ctx, d, c.itemState, c.itemPermission, r, TargetScopeEvidence{})
	switch code {
	case "":
		return listItemAllowed
	case agentDelegationCodeIssuerProjectAccess, agentDelegationCodeOutsideBoundary:
		return listItemDroppedByAdmission
	default:
		return listItemDenied
	}
}

// projectOfListItem returns the project an item of a delegated list belongs
// to: its project parent. An item without one is never listed.
func projectOfListItem(r Resource) (string, bool) {
	if r.ParentType != "project" || r.ParentID == "" {
		return "", false
	}
	return r.ParentID, true
}

// flushListFilter writes the request's aggregated item-filter record, once,
// through the shared record builder. routeGuard defers it for every
// admitted list route, so it also runs when the handler panics or the
// request is cancelled. The record says incomplete exactly when the handler
// did not mark its list complete; a client that goes away after a complete
// response does not change that. It is emitted on a context that is not
// cancelled. A request that never reached the list arm writes no record.
func (s *Server) flushListFilter(ctx context.Context, c *listFilterCollector) {
	summary, resourceType, due := c.summary()
	if !due {
		return
	}
	a := s.authzService
	if a == nil || a.decisionAuditEmitter == nil {
		return
	}
	d, _ := GetIdentityFromContext(ctx).(*DelegatedAgentIdentity)
	if d == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	request := AuthzRequest{
		Principal:  principalContextForIdentity(d),
		Credential: credentialContextForIdentity(d),
		Resource:   Resource{Type: resourceType},
		Action:     ActionRead,
		Permission: summary.ItemPermission,
	}
	decision := Decision{
		Allowed:         true,
		Reason:          "agent delegation: list filter",
		AlwaysAudit:     true,
		AgentDelegation: agentDelegationAttribution(d, ""),
	}
	decision = decorateDecision(decision, request, request.Principal, request.Credential, auditPermissionID(request))
	record := BuildDecisionAuditRecord(ctx, request, decision)
	record.Sampled = a.DecisionAuditSampleRate < 1.0
	record.RecordKind = store.DecisionAuditRecordKindListFilter
	record.ListFilter = &summary
	a.decisionAuditEmitter.EmitDecisionAudit(ctx, record)
}

// listDelegated pages a delegated list through listAuthorizedOrAll's
// per-item scan (authorizeEach is always true, so the store's direct path
// is never taken), with the item filter as the per-item decision. cursor is
// the request cursor exactly as the client sent it: listAuthorizedOrAll
// opens it against binding and seals the cursor it returns, once, so the
// caller returns NextCursor unchanged. cursorFor must issue inner cursors
// with the same binding.
func listDelegated[T any](
	ctx context.Context,
	s *Server,
	d *DelegatedAgentIdentity,
	cursor string,
	limit int,
	binding string,
	fetch func(context.Context, store.ListOptions) (*store.ListResult[T], error),
	resource func(*T) Resource,
	cursorFor func(*T) string,
) (authorizedListResult[T], error) {
	return listAuthorizedOrAll(ctx, d, cursor, limit, binding, s.listCursorSealer, true,
		fetch, resource, cursorFor, s.delegatedListItemRead)
}

// listAgentsDelegated is the hub agent list (GET /api/v1/agents) for an
// agent delegated credential. It serves the default (legacy) order only.
// The scope comes from the delegated list arm for agent.list; every agent
// is then decided for agent.read. Totals and the sealed cursor count only
// returned agents. Each returned agent carries only the read capability,
// its environment is redacted, and the response has no scope capabilities.
func (s *Server) listAgentsDelegated(w http.ResponseWriter, r *http.Request, d *DelegatedAgentIdentity) {
	if !admitDelegatedListHandler(w, r, opAgentList, hubAgentCollectionTarget) {
		return
	}
	ctx, query := r.Context(), r.URL.Query()
	perfSetEndpoint(ctx, perfEndpointAgentsGlobalLegacy)
	if query.Get("sort") != "" || query.Get("ids") != "" {
		writeError(w, http.StatusBadRequest, errCodeDelegatedListSortUnsupported,
			"a delegated credential lists agents in the default order only; sort and ids are not supported", nil)
		return
	}
	limit, err := parseAuthorizedListLimit(query.Get("limit"))
	if err != nil {
		BadRequest(w, err.Error())
		return
	}
	view := legacyAgentListView(query)
	c := listFilterCollectorFromContext(ctx)
	writeEmpty := func() {
		if c != nil {
			c.markComplete()
		}
		s.writeAgentList(ctx, w, view, ListAgentsResponse{
			Agents:       []AgentWithCapabilities{},
			ServerTime:   time.Now().UTC(),
			Capabilities: &Capabilities{Actions: []string{}},
		})
	}

	// Any refusal by the list arm, and a server error, answers 200 with an
	// empty list plus the arm's deny record (.design/agent-delegation.md
	// §11.8: any error denies, an empty list plus a decision audit), never a
	// 403 that would tell the caller which check failed.
	scope, err := s.delegatedListScope(ctx, d, "agent", "agent.list")
	if err != nil {
		slog.WarnContext(ctx, "listAgentsDelegated: list scope failed; answering an empty list", "error", err)
	}
	if err != nil || scope.Mode == delegatedListNone {
		writeEmpty()
		return
	}
	// The delegated caller is not a user, so it owns no project and shares
	// none: the mine and shared classifications list nothing.
	if q := query.Get("scope"); q == "mine" || q == "shared" || query.Get("mine") == "true" {
		writeEmpty()
		return
	}

	filter, ok := s.agentListQueryFilter(ctx, w, query, scope.admitsProject)
	if !ok {
		return
	}
	if scope.Mode == delegatedListExplicit {
		filter.AuthorizedProjectIDs = append([]string(nil), scope.ProjectIDs...)
	}
	if len(scope.Excluded) > 0 {
		filter.ExcludedProjectIDs = append([]string(nil), scope.Excluded...)
	}

	binding := scopedCursorBinding("agents", filter, d)
	result, err := listDelegated(ctx, s, d, query.Get("cursor"), limit, binding,
		func(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
			defer perfPhaseStart(ctx, perfPhaseListDBRead)()
			return s.store.ListAgents(ctx, filter, opts)
		},
		agentResource,
		func(a *store.Agent) string { return authorizedListCursor(a.Created, a.ID, binding) })
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	agents, scopeCap := s.buildGlobalAgentPage(ctx, d, result.Items)
	if c != nil {
		c.markComplete()
	}
	s.writeAgentList(ctx, w, view, ListAgentsResponse{
		Agents:                agents,
		NextCursor:            result.NextCursor,
		TotalCount:            result.TotalCount,
		TotalCountApproximate: result.TotalCountApproximate,
		ServerTime:            time.Now().UTC(),
		Capabilities:          scopeCap,
	})
}
