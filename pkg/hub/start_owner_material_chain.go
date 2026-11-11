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
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Start-time owner material and the agent's recorded chain.
//
// At create, start and restart the dispatcher reads user-scope material
// (secrets and env vars) keyed on agent.OwnerID. A row the owner shares
// with progeny (AllowProgeny) is held to the user-scope secret parent
// ceiling (startSecretResolveOpts) for secrets, and is unchanged for env
// vars. A row not shared with progeny is the owner's own material: it is
// delivered only along the agent's recorded chain. The rule, per row of
// owner R:
//
//   - the agent's stored ancestry, when it has one, starts at R
//     (Ancestry[0], the provenance root the runtime read is keyed on);
//   - the agent's recorded delegation chain, followed edge by edge in the
//     agent's project, ends at user R.
//
// When either does not hold, the row is withheld from the start (a
// lower-scope row with the same key stays in effect). This is the case of
// an agent whose authority was re-recorded to another user (for example a
// reincarnation by another user): its ancestry still starts at its creator,
// but its chain now ends at the other user, so the creator's own rows stop
// reaching it.
//
// The rule is about who the chain ends at, not what it allows: the chain's
// permission clauses (effect ceilings, held or inactive principals, project
// admission) are not applied to these rows, so a user's own agent at any
// role keeps the user's own rows as before.
//
// An agent whose chain is not recorded is withheld too, with the cohort as
// the reason: its own edge is missing (legacy_no_edge), is the migration
// sentinel (legacy_sentinel_edge), carries no recorded provenance
// (legacy_unadopted_edge), is one of several active edges
// (legacy_ambiguous_edges), or records a principal that does not
// type-check (legacy_inconsistent_principal).
//
// An adopted edge (source system_migration) and an edge recorded from a
// user access token (source uat) are recorded edges: their delegator is
// the user, so the owner's own agent passes. A row's CreatedBy is not
// consulted (rows written before it was recorded have none).
//
// Fail closed: a store error while following the chain, a depth or cycle
// violation, or a hop above the agent's own edge without recorded
// provenance (including several active edges there) withholds every such
// row.
//
// One verdict per dispatch: each dispatch entry point installs a memo
// (withOwnerChainMemo), so every site of one create, start or restart uses
// the same verdict and the chain is followed once.
//
// The read-only reports (the env source labels, the required-key probe,
// the env-gather response, and the TZ shown by the agent view, the edit
// response and the labels: viewAgentTZ) apply the same row decisions
// without counting or logging (ownerChainCheck.allowsRow for env vars,
// allowsSecretRow for secrets, including the owner's secrets shared with
// progeny that start withholds), so they never report a withheld row as
// present. Only delivery sites report.

// Owner material chain outcomes.
type ownerChainOutcome int

const (
	// ownerChainEndsAtUser: the recorded chain ends at terminalUserID.
	ownerChainEndsAtUser ownerChainOutcome = iota
	// ownerChainLegacy: the agent's own edge is not recorded (reason is
	// the cohort). Its rows are withheld.
	ownerChainLegacy
	// ownerChainWithhold: the chain cannot be followed (reason).
	ownerChainWithhold
)

// Withhold reasons and legacy cohorts of the owner material chain rule.
const (
	ownerChainReasonTerminalNotOwner = "terminal_user_not_owner"
	ownerChainReasonRootNotOwner     = "provenance_root_not_owner"
	ownerChainReasonUnrecordedAbove  = "unrecorded_hop_above_agent"
	ownerChainReasonInvalid          = "chain_invalid"
	ownerChainReasonLookupError      = "chain_lookup_error"

	ownerChainCohortAmbiguousEdges        = "legacy_ambiguous_edges"
	ownerChainCohortInconsistentPrincipal = "legacy_inconsistent_principal"
)

// ownerChainVerdict is the result of following an agent's recorded chain.
type ownerChainVerdict struct {
	outcome        ownerChainOutcome
	terminalUserID string // ownerChainEndsAtUser
	reason         string // withhold reason, or legacy cohort
}

// ownerChainVerdictObserver is a test seam: when it holds a function, that
// function is called with the agent ID each time ownerChainVerdict follows
// a chain. Only tests store one; it is empty in production. It is atomic so
// a test may set it while others run.
var ownerChainVerdictObserver atomic.Pointer[func(agentID string)]

// ownerChainVerdict follows agent's recorded delegation edges in its project
// up to the first user delegator. It reads edges only; it allows nothing
// by itself.
func (a *AuthzService) ownerChainVerdict(ctx context.Context, agent *store.Agent) ownerChainVerdict {
	if obs := ownerChainVerdictObserver.Load(); obs != nil && agent != nil {
		(*obs)(agent.ID)
	}
	if a == nil || a.store == nil || agent == nil || agent.ID == "" {
		return ownerChainVerdict{outcome: ownerChainWithhold, reason: ownerChainReasonLookupError}
	}
	ctx = maskAuthzInputs(ctx)
	visited := make(map[string]bool, maxDelegationDepth+1)
	delegateID := agent.ID
	for depth := 0; ; depth++ {
		if depth > maxDelegationDepth || visited[delegateID] {
			return ownerChainVerdict{outcome: ownerChainWithhold, reason: ownerChainReasonInvalid}
		}
		visited[delegateID] = true
		active, err := a.activeProjectEdges(ctx, delegateID, agent.ProjectID)
		if err != nil {
			return ownerChainVerdict{outcome: ownerChainWithhold, reason: ownerChainReasonLookupError}
		}
		cohort := ""
		var edge *store.DelegationEdge
		switch len(active) {
		case 0:
			cohort = legacyDeliveryCohortNoEdge
		case 1:
			edge = active[0]
			switch {
			case isMigrationSentinel(edge):
				cohort = legacyDeliveryCohortSentinelEdge
			case !knownProvenanceVersion(edge.ProvenanceVersion) ||
				(edge.Kind != store.EffectCeilingPrincipal && edge.Kind != store.EffectCeilingBounded):
				cohort = legacyDeliveryCohortUnadoptedEdge
			case checkEdgePrincipal(edge) != nil:
				cohort = ownerChainCohortInconsistentPrincipal
			}
		default:
			cohort = ownerChainCohortAmbiguousEdges
		}
		if cohort != "" {
			if depth == 0 {
				return ownerChainVerdict{outcome: ownerChainLegacy, reason: cohort}
			}
			return ownerChainVerdict{outcome: ownerChainWithhold, reason: ownerChainReasonUnrecordedAbove}
		}
		switch edge.DelegatorType {
		case store.DelegationPrincipalUser:
			return ownerChainVerdict{outcome: ownerChainEndsAtUser, terminalUserID: edge.DelegatorID}
		case store.DelegationPrincipalAgent:
			delegateID = edge.DelegatorID
		default:
			// checkEdgePrincipal admits only user and agent delegators.
			return ownerChainVerdict{outcome: ownerChainWithhold, reason: ownerChainReasonInvalid}
		}
	}
}

// ownerRowDecision applies the rule to one row of owner rowOwner. It
// returns whether the row is delivered, and the withhold reason or the
// legacy cohort ("" for a plain delivery).
func ownerRowDecision(v ownerChainVerdict, ancestry []string, rowOwner string) (deliver bool, legacy bool, reason string) {
	if rowOwner == "" {
		return false, false, ownerChainReasonRootNotOwner
	}
	if len(ancestry) > 0 && ancestry[0] != rowOwner {
		return false, false, ownerChainReasonRootNotOwner
	}
	switch v.outcome {
	case ownerChainLegacy:
		return true, true, v.reason
	case ownerChainEndsAtUser:
		if v.terminalUserID != rowOwner {
			return false, false, ownerChainReasonTerminalNotOwner
		}
		return true, false, ""
	default:
		return false, false, v.reason
	}
}

// ownerChainMemoKey keys the per-dispatch verdict memo in a context.
type ownerChainMemoKey struct{}

// ownerChainMemo holds the verdicts computed during one dispatch: the
// owner material chain verdict by agent, and the user secret chain verdict
// (userSecretChainVerdict) by agent and project.
type ownerChainMemo struct {
	mu         sync.Mutex
	verdicts   map[string]ownerChainVerdict
	userChains map[string]userSecretChain
}

// withOwnerChainMemo returns ctx with a verdict memo for one dispatch, or
// ctx itself when it already carries one (a nested dispatch call).
func withOwnerChainMemo(ctx context.Context) context.Context {
	if _, ok := ctx.Value(ownerChainMemoKey{}).(*ownerChainMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, ownerChainMemoKey{}, &ownerChainMemo{verdicts: make(map[string]ownerChainVerdict)})
}

// ownerChainVerdictFor is ownerChainVerdict, memoized for the dispatch ctx
// carries (withOwnerChainMemo). Without a memo it computes the verdict.
func (a *AuthzService) ownerChainVerdictFor(ctx context.Context, agent *store.Agent) ownerChainVerdict {
	m, ok := ctx.Value(ownerChainMemoKey{}).(*ownerChainMemo)
	if !ok || m == nil || agent == nil {
		return a.ownerChainVerdict(ctx, agent)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.verdicts[agent.ID]; ok {
		return v
	}
	v := a.ownerChainVerdict(ctx, agent)
	m.verdicts[agent.ID] = v
	return v
}

// userSecretChainVerdictFor is userSecretChainVerdict without an explain
// trace, memoized for the dispatch ctx carries (withOwnerChainMemo), so the
// start-time sites of one dispatch decide every shared owner row on one
// verdict. Without a memo it computes the verdict.
func (a *AuthzService) userSecretChainVerdictFor(ctx context.Context, principal AgentIdentity) userSecretChain {
	m, ok := ctx.Value(ownerChainMemoKey{}).(*ownerChainMemo)
	if !ok || m == nil || isNilIdentity(principal) || principal.ID() == "" {
		return a.userSecretChainVerdict(ctx, principal, nil)
	}
	key := principal.ID() + "/" + principal.ProjectID()
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.userChains[key]; ok {
		return c
	}
	c := a.userSecretChainVerdict(ctx, principal, nil)
	if m.userChains == nil {
		m.userChains = make(map[string]userSecretChain)
	}
	m.userChains[key] = c
	return c
}

// Outcome of the rule for a withheld row (the metric's outcome
// attribute). A delivery has none.
const ownerRowOutcomeWithheld = "withheld"

// ownerRowOutcome is the rule's decision for one row: whether it is
// delivered, its outcome ("" for a plain delivery) and the withhold reason
// or cohort.
type ownerRowOutcome struct {
	deliver bool
	outcome string
	reason  string
}

// ownerChainCheck applies the rule to one agent's rows. The verdict is
// computed on the first row. It counts and logs nothing. A nil check allows
// every row: no authorization service is configured, the agent has no
// owner, or it has no ID yet (the create-time availability probe, before
// the agent is stored; nothing is delivered to it, and every later start
// applies the rule).
type ownerChainCheck struct {
	a       *AuthzService
	agent   *store.Agent
	done    bool
	verdict ownerChainVerdict
	// sharedDone and sharedWithheld cache sharedChainWithheld.
	sharedDone     bool
	sharedWithheld bool
}

func newOwnerChainCheck(a *AuthzService, agent *store.Agent) *ownerChainCheck {
	if a == nil || agent == nil || agent.OwnerID == "" || agent.ID == "" {
		return nil
	}
	return &ownerChainCheck{a: a, agent: agent}
}

// decide applies the rule to a row of rowOwner not shared with progeny:
//
//	(deliver, legacy, reason) = ownerRowDecision(verdict, agent.Ancestry, rowOwner)
//	if deliver && !legacy:  deliver
//	else:                   withhold (reason; for a chain that is not
//	                        recorded, its cohort, e.g. legacy_no_edge)
//
// The rule only adds withhold outcomes to what is delivered without it.
func (c *ownerChainCheck) decide(ctx context.Context, rowOwner string) ownerRowOutcome {
	if c == nil {
		return ownerRowOutcome{deliver: true}
	}
	if !c.done {
		c.verdict = c.a.ownerChainVerdictFor(ctx, c.agent)
		c.done = true
	}
	deliver, legacy, reason := ownerRowDecision(c.verdict, c.agent.Ancestry, rowOwner)
	if deliver && !legacy {
		return ownerRowOutcome{deliver: true}
	}
	return ownerRowOutcome{outcome: ownerRowOutcomeWithheld, reason: reason}
}

// decideShared applies the rule to a row of rowOwner SHARED with progeny,
// for an agent with no stored ancestry. The parent ceiling has no ancestry
// to hold such a row to, so it follows the verdict instead (erratum 2b
// section 3a makes no ancestry exception). Only a recorded chain that ends
// at rowOwner keeps the row. Anything else withholds it, failing closed:
//   - a chain that is not recorded (a legacy cohort): chain_unrecorded,
//     as the runtime read refuses it;
//   - a recorded chain that ends at another user: terminal_user_not_owner;
//   - a chain that cannot be followed: the verdict's reason (for example
//     chain_lookup_error or unrecorded_hop_above_agent).
func (c *ownerChainCheck) decideShared(ctx context.Context, rowOwner string) ownerRowOutcome {
	if c == nil {
		return ownerRowOutcome{outcome: ownerRowOutcomeWithheld, reason: ownerChainReasonLookupError}
	}
	if !c.done {
		c.verdict = c.a.ownerChainVerdictFor(ctx, c.agent)
		c.done = true
	}
	switch c.verdict.outcome {
	case ownerChainEndsAtUser:
		if rowOwner != "" && c.verdict.terminalUserID == rowOwner {
			return ownerRowOutcome{deliver: true}
		}
		return ownerRowOutcome{outcome: ownerRowOutcomeWithheld, reason: ownerChainReasonTerminalNotOwner}
	case ownerChainLegacy:
		return ownerRowOutcome{outcome: ownerRowOutcomeWithheld, reason: ReasonChainUnrecorded}
	default:
		reason := c.verdict.reason
		if reason == "" {
			reason = ownerChainReasonInvalid
		}
		return ownerRowOutcome{outcome: ownerRowOutcomeWithheld, reason: reason}
	}
}

// allowsRow reports, without counting or logging, whether a user-scope env
// var row of rowOwner is delivered under the rule: an env var shared with
// progeny is not the rule's to decide (start delivers it: allowEnvRow).
// Secret rows use allowsSecretRow, because start withholds some shared
// secrets.
func (c *ownerChainCheck) allowsRow(ctx context.Context, allowProgeny bool, rowOwner string) bool {
	if c == nil || allowProgeny {
		return true
	}
	return c.decide(ctx, rowOwner).deliver
}

// allowsSecretRow reports, without counting or logging, whether an owner
// secret of rowOwner is delivered at start: a row not shared with progeny
// on the rule (decide), a shared row as allowsSharedSecret decides it. A
// nil check allows every row.
func (c *ownerChainCheck) allowsSecretRow(ctx context.Context, allowProgeny bool, rowOwner string) bool {
	if c == nil {
		return true
	}
	if allowProgeny {
		return c.allowsSharedSecret(ctx, rowOwner)
	}
	return c.decide(ctx, rowOwner).deliver
}

// allowsSharedSecret reports, without counting or logging, whether an
// owner secret of rowOwner SHARED with progeny is delivered at start, as
// startSecretResolveOpts decides it before the row's own parent ceiling:
// for an agent with no stored ancestry, on the owner material chain verdict
// (decideShared); otherwise it is withheld when the runtime chain verdict
// is not recorded or cannot be evaluated (sharedChainWithheld). The
// read-only views and the census both decide shared rows here, so neither
// can drift from start. A nil check allows the row.
func (c *ownerChainCheck) allowsSharedSecret(ctx context.Context, rowOwner string) bool {
	if c == nil {
		return true
	}
	if len(c.agent.Ancestry) == 0 {
		return c.decideShared(ctx, rowOwner).deliver
	}
	return !c.sharedChainWithheld(ctx)
}

// sharedChainWithheld reports, for an agent with stored ancestry, whether
// the runtime chain verdict withholds every secret shared with progeny at
// start (sharedChainWithholds). It is computed once per check, through the
// dispatch memo when ctx carries one (userSecretChainVerdictFor), so a
// view decides all its rows on one verdict. A nil check withholds nothing.
func (c *ownerChainCheck) sharedChainWithheld(ctx context.Context) bool {
	if c == nil {
		return false
	}
	if !c.sharedDone {
		ceilingErr, unrecorded := sharedChainWithholds(c.a.userSecretChainVerdictFor(ctx, &storedAgentIdentity{agent: c.agent}))
		c.sharedWithheld = ceilingErr || unrecorded
		c.sharedDone = true
	}
	return c.sharedWithheld
}

// sharedChainWithholds reports why a runtime chain verdict withholds every
// secret shared with progeny at start: the chain could not be evaluated
// (ceilingErr), or it is not recorded (unrecorded, chain_unrecorded, as
// the runtime read refuses it; erratum 2b section 3a). Start
// (startSecretResolveOpts) and allowsSharedSecret both use it.
func sharedChainWithholds(chain userSecretChain) (ceilingErr, unrecorded bool) {
	return chain.decision.DenyCause == DenyCauseCeilingError, !chain.decision.Allowed && chain.unrecorded
}

// Kinds counted by the owner material gate.
const (
	ownerMaterialKindSecret = "secret"
	ownerMaterialKindEnvVar = "env_var"
)

// Sites the owner material gate reports (the metric's site attribute).
const (
	ownerMaterialSiteResolveSecrets = "resolve_secrets"
	ownerMaterialSiteAsNeeded       = "as_needed"
	ownerMaterialSiteLaunchSecrets  = "launch_secrets"
	ownerMaterialSiteLaunchEnv      = "launch_env"
	ownerMaterialSiteEnvStorage     = "env_storage"
	ownerMaterialSiteCloneToken     = "clone_token"
	ownerMaterialSiteTZ             = "tz"
)

// ownerMaterialChainWarnInterval is the shortest gap between two Warn lines
// for the same agent, site and reason.
const ownerMaterialChainWarnInterval = 10 * time.Minute

type ownerMaterialCounts struct{ secrets, envVars int }

func (c *ownerMaterialCounts) add(kind string) {
	if kind == ownerMaterialKindEnvVar {
		c.envVars++
		return
	}
	c.secrets++
}

// ownerMaterialGate applies the owner material chain rule to the non-shared
// user-scope rows of one start-time site, for one agent, and counts the
// withheld rows by reason; flush reports them. A nil gate allows every row (no authorization service
// is configured).
type ownerMaterialGate struct {
	d     *HTTPAgentDispatcher
	check *ownerChainCheck

	counts map[ownerMaterialCountKey]*ownerMaterialCounts
}

// ownerMaterialCountKey groups counted rows by outcome and reason.
type ownerMaterialCountKey struct{ outcome, reason string }

func (d *HTTPAgentDispatcher) newOwnerMaterialGate(agent *store.Agent) *ownerMaterialGate {
	if d == nil {
		return nil
	}
	check := newOwnerChainCheck(d.authzService, agent)
	if check == nil {
		return nil
	}
	return &ownerMaterialGate{d: d, check: check}
}

// allow reports whether a non-shared user-scope row of rowOwner, of kind
// (ownerMaterialKindSecret or ownerMaterialKindEnvVar), may be delivered.
func (g *ownerMaterialGate) allow(ctx context.Context, kind, rowOwner string) bool {
	if g == nil {
		return true
	}
	o := g.check.decide(ctx, rowOwner)
	if o.outcome != "" {
		g.count(o.outcome, o.reason, kind)
	}
	return o.deliver
}

// withholdShared counts an owner row shared with progeny that start-time
// delivery withholds because the agent's chain is not recorded
// (chain_unrecorded, as the runtime read refuses it), and returns false.
// A nil gate counts nothing and still withholds.
func (g *ownerMaterialGate) withholdShared(kind string) bool {
	if g != nil {
		g.count(ownerRowOutcomeWithheld, ReasonChainUnrecorded, kind)
	}
	return false
}

// allowSharedNoAncestry reports whether an owner row shared with progeny,
// of rowOwner and kind, may be delivered to an agent with no stored
// ancestry (ownerChainCheck.decideShared), and counts a withheld row. A nil
// gate withholds and counts nothing. newOwnerMaterialGate returns nil only
// when newOwnerChainCheck does: no authorization service (start installs no
// owner path then), no owner (no owner path), or an agent without an ID
// (the create-time probe, before the agent is stored). An owned agent with
// no ID and no stored ancestry is unreachable, since every create path
// stores a non-empty ancestry for an owned agent (round 8 notes, item 5).
// So a nil gate here is a fault, and the row fails closed.
func (g *ownerMaterialGate) allowSharedNoAncestry(ctx context.Context, kind, rowOwner string) bool {
	if g == nil {
		return false
	}
	o := g.check.decideShared(ctx, rowOwner)
	if o.outcome != "" {
		g.count(o.outcome, o.reason, kind)
	}
	return o.deliver
}

// count adds one row of kind under outcome and reason.
func (g *ownerMaterialGate) count(outcome, reason, kind string) {
	if g.counts == nil {
		g.counts = make(map[ownerMaterialCountKey]*ownerMaterialCounts)
	}
	k := ownerMaterialCountKey{outcome: outcome, reason: reason}
	if g.counts[k] == nil {
		g.counts[k] = &ownerMaterialCounts{}
	}
	g.counts[k].add(kind)
}

// allowEnvRow applies the rule to a stored env var row listed under filter:
// a user-scope row not shared with progeny (plain or secret-flagged) must
// pass allow; every other row is not the gate's to decide.
func (g *ownerMaterialGate) allowEnvRow(ctx context.Context, filter store.EnvVarFilter, v store.EnvVar) bool {
	if g == nil || filter.Scope != store.ScopeUser || v.AllowProgeny {
		return true
	}
	return g.allow(ctx, ownerMaterialKindEnvVar, envRowOwner(filter, v))
}

// ownerRowScope is a row's owner: its scope ID, or the scope it was looked
// up in when the row does not carry one.
func ownerRowScope(scopeID, lookedUp string) string {
	if scopeID != "" {
		return scopeID
	}
	return lookedUp
}

// envRowOwner is the owner of a user-scope env var row listed under filter.
func envRowOwner(filter store.EnvVarFilter, v store.EnvVar) string {
	if v.ScopeID != "" {
		return v.ScopeID
	}
	return filter.ScopeID
}

// ownerMaterialWithheldMsg is the Warn message of the owner material chain
// rule, for every withhold reason; the reason field carries the cause.
const ownerMaterialWithheldMsg = "start-time user material withheld by the owner material chain rule"

// flush reports the rows counted since the last flush, for site, and
// resets the counts. Every count is recorded in the metric. The Warn lines
// name the agent, the counts and the reason, never a key or a value: one
// per agent, site and reason.
func (g *ownerMaterialGate) flush(ctx context.Context, site string) {
	if g == nil {
		return
	}
	a := g.d.authzService
	agent := g.check.agent
	log := g.d.log
	if log == nil {
		log = slog.Default()
	}
	for k, c := range g.counts {
		a.recordOwnerMaterialChain(ctx, site, k.outcome, k.reason, c)
		if !g.d.admitOwnerChainWarn(agent.ID+"/"+site, k.outcome+"/"+k.reason) {
			continue
		}
		log.WarnContext(ctx, ownerMaterialWithheldMsg,
			"site", site, "agent_id", agent.ID, "reason", k.reason,
			"secrets", c.secrets, "env_vars", c.envVars)
	}
	g.counts = nil
}

// admitOwnerChainWarn rate limits the owner material chain Warn per key and
// cause. Without a limiter every line is logged.
func (d *HTTPAgentDispatcher) admitOwnerChainWarn(key, cause string) bool {
	if d.ownerChainWarn == nil {
		return true
	}
	record, _ := d.ownerChainWarn.admit(key, cause)
	return record
}

func newOwnerMaterialChainLimiter() *hookRefusalLimiter {
	return newHookRefusalLimiter(ownerMaterialChainWarnInterval, time.Now)
}

// ownerMaterialChainMetrics counts the rule's row evaluations that withhold
// a row.
type ownerMaterialChainMetrics interface {
	RecordOwnerMaterialChain(ctx context.Context, site, outcome, reason, kind string, n int64)
}

// OTelOwnerMaterialChainMetrics counts them as
// scion.hub.user_material.owner_chain, unit {evaluation}. One start
// evaluates a row at each site that reads it, so the counter counts
// evaluations per site, not distinct rows. Attributes, each with a fixed
// set of values: site (resolve_secrets, as_needed, launch_secrets,
// launch_env, env_storage, clone_token, tz), outcome (withheld), reason (a
// withhold reason or cohort) and kind (secret, env_var).
type OTelOwnerMaterialChainMetrics struct {
	total metric.Int64Counter
}

// NewOTelOwnerMaterialChainMetrics creates the counter.
func NewOTelOwnerMaterialChainMetrics(mp metric.MeterProvider) (*OTelOwnerMaterialChainMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel owner material chain metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter("scion.hub.user_material.owner_chain",
		metric.WithUnit("{evaluation}"),
		metric.WithDescription("start-time evaluations of an owner's unshared user-scope row that withheld it, per site"))
	if err != nil {
		return nil, fmt.Errorf("creating user_material.owner_chain counter: %w", err)
	}
	return &OTelOwnerMaterialChainMetrics{total: c}, nil
}

// RecordOwnerMaterialChain implements ownerMaterialChainMetrics.
func (m *OTelOwnerMaterialChainMetrics) RecordOwnerMaterialChain(ctx context.Context, site, outcome, reason, kind string, n int64) {
	m.total.Add(ctx, n, metric.WithAttributes(
		attribute.String("site", site),
		attribute.String("outcome", outcome),
		attribute.String("reason", reason),
		attribute.String("kind", kind),
	))
}

// SetOwnerMaterialChainMetrics installs the owner material chain counter.
func (s *Server) SetOwnerMaterialChainMetrics(m ownerMaterialChainMetrics) {
	if s == nil || s.authzService == nil {
		return
	}
	s.authzService.ownerChainMetrics = m
}

// OwnerMaterialChainWithheld returns the number of row evaluations the
// owner material chain rule withheld since the process started (counted
// per site, as the metric is).
func (a *AuthzService) OwnerMaterialChainWithheld() int64 {
	if a == nil {
		return 0
	}
	return a.ownerChainWithheld.Load()
}

func (a *AuthzService) recordOwnerMaterialChain(ctx context.Context, site, outcome, reason string, c *ownerMaterialCounts) {
	if a == nil || c == nil {
		return
	}
	if outcome == ownerRowOutcomeWithheld {
		a.ownerChainWithheld.Add(int64(c.secrets + c.envVars))
	}
	m := a.ownerChainMetrics
	if m == nil {
		return
	}
	if c.secrets > 0 {
		m.RecordOwnerMaterialChain(ctx, site, outcome, reason, ownerMaterialKindSecret, int64(c.secrets))
	}
	if c.envVars > 0 {
		m.RecordOwnerMaterialChain(ctx, site, outcome, reason, ownerMaterialKindEnvVar, int64(c.envVars))
	}
}

// testAdminScopes is one evaluation of testAdminRootScopes for an agent.
// A start-time site computes it once and passes the same value to the
// owner material chain rule's keep decision (startSecretResolveOpts) and to
// the test-admin scope restriction it applies (applyTestAdminSecretScopes,
// or the launch selection's withholding), so the two cannot diverge.
type testAdminScopes struct {
	restricted bool
	allowed    map[string]bool
	root       string
}

// testAdminScopesFor evaluates testAdminRootScopes once for agent.
func (d *HTTPAgentDispatcher) testAdminScopesFor(ctx context.Context, agent *store.Agent) testAdminScopes {
	restricted, allowed, root := d.testAdminRootScopes(ctx, agent)
	return testAdminScopes{restricted: restricted, allowed: allowed, root: root}
}

// withholdsUser reports whether the restriction withholds the user scope.
// A nil value (no restriction applied afterwards) withholds nothing.
func (t *testAdminScopes) withholdsUser() bool {
	return t != nil && t.restricted && !t.allowed[store.ScopeUser]
}

// ownerCloneTokenAllowed applies the start-time user-scope checks
// (startSecretResolveOpts: the parent ceiling for a shared row, the owner
// material chain rule for a row not shared) to the owner's GITHUB_TOKEN row
// before the NoAuth clone-token fallback reads it by name. A missing row,
// or one whose metadata cannot be read, is not read either way: the read
// that follows would find nothing usable, and refusing on a fault is fail
// closed. Without an authorization service the row is not checked, as
// Resolve does not check it then.
func (d *HTTPAgentDispatcher) ownerCloneTokenAllowed(ctx context.Context, agent *store.Agent) bool {
	// The by-name read applies no test-admin scope restriction afterwards,
	// so the check runs without the test-admin keep (nil).
	opts, gate := d.startSecretResolveOpts(ctx, agent, nil)
	if opts == nil || opts.UserScopeCheck == nil {
		return true
	}
	defer gate.flush(ctx, ownerMaterialSiteCloneToken)
	meta, err := d.secretBackend.GetMeta(ctx, "GITHUB_TOKEN", store.ScopeUser, agent.OwnerID)
	if err != nil || meta == nil {
		return false
	}
	return opts.UserScopeCheck(*meta)
}
