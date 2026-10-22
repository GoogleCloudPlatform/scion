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

// Package experiments declares the hub-wide experiments (feature flags)
// registry. It has no dependency on pkg/hub, so a future broker or CLI
// design can import it directly.
package experiments

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"
)

// Layer identifies which part of the system an experiment gates.
type Layer string

const (
	// LayerWeb gates web UI code via isFeatureEnabled().
	LayerWeb Layer = "web"
	// LayerServer gates hub server code via Server.experimentEnabled().
	LayerServer Layer = "server"
)

// Stage is a cosmetic lifecycle badge shown in the Experiments tab. It has
// no behavioural effect.
type Stage string

const (
	StageAlpha Stage = "alpha"
	StageBeta  Stage = "beta"
)

// validStages is the exhaustive set of Stage values NewRegistry accepts.
var validStages = map[Stage]bool{StageAlpha: true, StageBeta: true}

// NamePattern is the required shape of an experiment name, e.g.
// "web.agent_edit" or "hub.foo.bar". pkg/config/opsettings uses the
// same pattern in its JSON schema for the "experiments" section, and pkg/hub
// uses ValidName to apply the identical rule to admin PUT payloads.
const NamePattern = `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`

var namePatternRe = regexp.MustCompile(NamePattern)

// ValidName reports whether name matches NamePattern.
func ValidName(name string) bool { return namePatternRe.MatchString(name) }

// reviewByLayout is the required YYYY-MM-DD shape of Experiment.ReviewBy.
const reviewByLayout = "2006-01-02"

// Experiment describes one hub-wide feature flag.
type Experiment struct {
	// Name is the stable identifier, e.g. "web.agent_edit". It must
	// match NamePattern. "web." is the convention for UI-only gates, "hub."
	// for anything that gates server behaviour (Layers is the source of
	// truth; the prefix is only a readability aid).
	Name string
	// Title is a short UI label, e.g. "Edit agent".
	Title string
	// Description is one or two sentences shown in the Experiments tab.
	Description string
	// Default is the value used when no admin override exists.
	Default bool
	// Layers lists which parts of the system this experiment gates.
	Layers []Layer
	// Stage is a cosmetic lifecycle badge; not behavioural.
	Stage Stage
	// Issue is the tracking issue, e.g. "ptone/scion#1662".
	Issue string
	// Owner is the team or handle responsible for graduating or removing it.
	Owner string
	// ReviewBy is a YYYY-MM-DD date; after it passes the tab shows "review
	// overdue".
	ReviewBy string
}

// HasLayer reports whether the experiment gates the given layer.
func (e Experiment) HasLayer(l Layer) bool {
	for _, layer := range e.Layers {
		if layer == l {
			return true
		}
	}
	return false
}

// ReviewOverdue reports whether now is strictly after the ReviewBy day: the
// tab shows "review overdue" starting the day after ReviewBy, not during
// ReviewBy itself. ReviewBy's layout is validated by NewRegistry; an
// unparsable value (only reachable by bypassing that validation) is treated
// as not overdue rather than panicking.
//
// The day boundary is UTC midnight regardless of now's Location: time.Parse
// yields ReviewBy at 00:00 UTC and the comparison is between instants, so
// the result depends only on the instant and hub replicas with different
// TZ settings agree.
func (e Experiment) ReviewOverdue(now time.Time) bool {
	t, err := time.Parse(reviewByLayout, e.ReviewBy)
	if err != nil {
		return false
	}
	return !now.Before(t.AddDate(0, 0, 1))
}

// K8sNFSHome gates the persistent NFS agent home on the Kubernetes runtime.
// The hub resolves it at each agent dispatch and sends it to the broker,
// which uses an NFS home only when it is on.
const K8sNFSHome = "hub.k8s_nfs_home"

// FlatRuntimeBrokers gates flat (single-target) Runtime Brokers: the hub
// accepts single-target Runtime Broker registrations, pins new agents to
// that target and refuses mismatched dispatches
// (.design/flat-runtime-brokers-contract.md). It is enforced in hub server
// code; Runtime Brokers do not need it (their flat behaviour comes from
// their own configuration).
const FlatRuntimeBrokers = "hub.flat_runtime_brokers"

// Artifacts gates the artifact service (pkg/artifacts): the hub's
// /api/v1/artifacts routes answer 404 while it is off, and the web UI hides
// every artifact surface.
const Artifacts = "hub.artifacts"

// AttachmentArtifacts gates storing web-chat attachments as message-scoped
// artifacts (ptone/scion#4595): while it is on, POST /api/v1/chat/attachments
// stores each file through the artifact service, which must be installed
// with its settings section enabled (hub.artifacts need not be on). The hub
// checks it on every upload. Attachments stored while it was on stay
// readable after it is turned off.
const AttachmentArtifacts = "hub.attachment_artifacts"

// ChatScheduledSend gates scheduled send in native web chat: the Schedule
// send menu item and pending-message banners in the web UI, the hub's
// /api/v1/chat/conversations/{key}/scheduled routes (404 while off), and
// the delivery sweeper, which holds pending messages while it is off.
const ChatScheduledSend = "web.chat_scheduled_send"

// AuthorizationDecisionAuditV2 gates decision logging (remaining-audit P1).
// It is the only activation control and stays default-off; turning it on
// needs explicit owner approval.
const AuthorizationDecisionAuditV2 = "hub.authorization_decision_audit_v2"

// AgentEdit gates the web Edit agent page (/agents/{id}/edit) and the Edit
// button on the agent page. It is web-only for now: the hub's config PATCH
// accepts edits of agents with no container whether or not it is on.
const AgentEdit = "web.agent_edit"

// AgentDelegation gates agent delegation (.design/agent-delegation.md §10):
// the grant issuance and credential exchange routes, the delegated
// credential middleware arm, and the delegated decision procedure. While it
// is off, issuance and exchange answer 404 and every delegated credential is
// refused. Turning it off suspends stored grants; it does not revoke them.
const AgentDelegation = "hub.agent_delegation"

// FederationProjectVisitors gates project access for users of trusted
// external issuers (ptone/scion#4603). While it is off, a verified
// federated user token always yields the same federated user identity as
// before; while it is on, a token whose (issuer, subject) is linked to an
// active federated account yields that account's visitor identity. It also
// gates POST /api/v1/auth/federation-token (404 while off), where this
// hub's own users obtain user access credentials for peer hubs, and the
// acceptance of user access credentials from peer hub issuers with
// accept_user_tokens (refused while off; agent tokens from those issuers are
// unaffected).
const FederationProjectVisitors = "federation.project_visitors"

// AgentPlugins gates native install of Agent Plugins 1.0.0 bundles declared
// in a template's agent_plugins. The hub sends it to the broker at each
// dispatch; outside broker mode the settings key local_experiments decides.
// It is checked at provisioning and at every start, so turning it off
// removes Scion-installed plugins at the next start.
const AgentPlugins = "hub.agent_plugins"

// AsyncAgentLaunch gates asynchronous agent create (ptone/scion#4991): the
// hub sends a create to the broker as an async launch only while it is on
// and the request opted in (acceptAsyncLaunch). It is read at each create
// send, so toggling needs no restart; launches already accepted finish or
// are reaped either way.
const AsyncAgentLaunch = "hub.async_agent_launch"

// AsyncAgentStart gates asynchronous start, resume and restart of existing
// agents (ptone/scion#4669). It applies only while hub.async_agent_launch is
// on too. Turn it on only after every hub node runs a version that
// advertises async_start_v1 in the hub-instance registry; to roll back,
// turn it off and wait launch_timeout plus 15 seconds before downgrading
// any node. This release registers it only: every start stays synchronous
// whatever its value.
const AsyncAgentStart = "hub.async_agent_start"

// compiled is the production experiment list. It is reachable only through
// Default(); there is no package-level Lookup/All, so hub code cannot bypass
// the Registry instance it was given (ptone/scion#2217).
var compiled = []Experiment{
	{
		Name:        AsyncAgentStart,
		Title:       "Asynchronous agent start",
		Description: "Lets the hub answer a start, resume or restart of an existing agent as soon as the Runtime Broker accepts it, like an asynchronous create; it applies only while hub.async_agent_launch is also on. Turn it on only after every hub node runs a version that supports it (each advertises async_start_v1). To roll back, turn it off and wait launch_timeout plus 15 seconds before downgrading any node. In this release starts stay synchronous whatever its value.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#4669",
		Owner:       "async-start",
		ReviewBy:    "2027-01-12",
	},
	{
		Name:        AsyncAgentLaunch,
		Title:       "Asynchronous agent create",
		Description: "Lets the hub answer an agent create as soon as the Runtime Broker accepts it, while the broker finishes the launch in the background; clients that opt in (scion start and resume, the web create form, scheduled creates) follow its progress. Provision-only creates, reprovisioning and brokers without async launch support stay synchronous. Turning it off makes new creates synchronous; launches already accepted finish or time out as before.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#4991",
		Owner:       "async-start",
		ReviewBy:    "2027-01-12",
	},
	{
		Name:        FederationProjectVisitors,
		Title:       "Project visitors from trusted issuers",
		Description: "Lets a user of a trusted external issuer who is linked to a local federated account act as that account (a project visitor), and lets this hub's users obtain short-lived user access credentials for the peer hubs in server.oidc.user_token_audiences, and accepts user access credentials from peer hub issuers that have accept_user_tokens. While off, federated user tokens behave exactly as before, no user access credentials are issued, no user access credentials are accepted from peer hubs, and stored accounts are kept.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#4603",
		Owner:       "federation",
		ReviewBy:    "2027-01-12",
	},
	{
		Name:        AgentDelegation,
		Title:       "Agent delegation",
		Description: "Lets a user issue an agent delegation grant and lets the bound agent exchange it for a short-lived delegated credential. While off, issuance and exchange answer 404 and delegated credentials are refused; stored grants are kept.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#2131",
		Owner:       "authz",
		ReviewBy:    "2027-01-31",
	},
	{
		Name:        AuthorizationDecisionAuditV2,
		Title:       "Authorization decision audit v2",
		Description: "Records authorization decisions in the existing project/agent audit domain to the configured structured logger, asynchronously; losses are counted, not retried.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#2379",
		Owner:       "audit-update",
		ReviewBy:    "2026-11-30",
	},
	{
		Name:        "web.gcs_links",
		Title:       "gs:// link previews",
		Description: "Linkifies a gs://bucket/object URI an agent posts in chat and lets the viewer fetch and preview that object through the hub. Gates both the linkifier (LayerWeb) and the GET /api/v1/gcs/object endpoint itself (LayerServer): the endpoint also requires a configured GCP token generator, so this experiment alone does not make links fetchable on a hub without one.",
		Default:     false,
		Layers:      []Layer{LayerWeb, LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#2545",
		Owner:       "native-chat",
		ReviewBy:    "2026-12-30",
	},
	{
		Name:        K8sNFSHome,
		Title:       "Persistent agent home on Kubernetes",
		Description: "Lets Kubernetes agents keep their home directory on the NFS export of their profile's shared-dir storage, across stops and restarts. Takes effect only where server.home_storage, or a profile or runtime home_storage_backend, selects nfs. Agents keep the home storage they were created with.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#2615",
		Owner:       "k8s-runtime",
		ReviewBy:    "2027-01-04",
	},
	{
		Name:        FlatRuntimeBrokers,
		Title:       "Flat Runtime Brokers",
		Description: "Lets a Runtime Broker serve exactly one runtime target with a stable identity: the hub accepts single-target Runtime Broker registrations, pins new agents to that target and rejects mismatched dispatches. Existing profile-based Runtime Brokers are unchanged. The web layer only lets the web UI read the flag (presentation only); every hub check is LayerServer. With it on, the web UI shows the Brokers area.",
		Default:     false,
		Layers: []Layer{
			LayerServer,
			LayerWeb,
		},
		Stage:    StageAlpha,
		Issue:    "ptone/scion#2926",
		Owner:    "runtime-broker",
		ReviewBy: "2027-03-31",
	},
	{
		Name:        Artifacts,
		Title:       "Artifacts",
		Description: "Lets agents and users publish files and bundles with stable, versioned references, and view them in the web UI. Gates the artifact page and other web surfaces (LayerWeb) and the hub's /api/v1/artifacts routes (LayerServer), which answer 404 while it is off.",
		Default:     false,
		Layers:      []Layer{LayerWeb, LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#3202",
		Owner:       "artifacts",
		ReviewBy:    "2027-01-05",
	},
	{
		Name:        AttachmentArtifacts,
		Title:       "Attachments as artifacts",
		Description: "Stores files uploaded in web chat in the artifact service as attachments of the message they are sent in: each one is readable exactly by the readers of that message, and is removed when the message is deleted. Gates the hub's attachment upload path (LayerServer; it also needs the artifact service with its settings section enabled, but not hub.artifacts) and is reserved for the web surfaces that follow (LayerWeb). Attachments stored while it was on stay readable after it is turned off.",
		Default:     false,
		Layers:      []Layer{LayerWeb, LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#4594",
		Owner:       "artifacts",
		ReviewBy:    "2027-01-12",
	},
	{
		Name:        "hub.conduit",
		Title:       "Conduit connection layer",
		Description: "Enables the hub surfaces of Conduit, the unified agent/broker connection layer: Ed25519 stream grants, the GET /api/v1/conduit/grant-keys endpoint, the agent conduit session endpoint GET /api/v1/conduit and the in-process relay (read at startup; turning it on or off for the relay needs a restart). No existing connection path changes.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#2774",
		Owner:       "conduit",
		ReviewBy:    "2027-03-31",
	},
	{
		Name:        ChatScheduledSend,
		Title:       "Scheduled send in chat",
		Description: "Adds Schedule send to the chat Send button menu: the message is held by the hub and sent as the user at the chosen time; until then only the sender sees it, with a Cancel button. Gates the menu item and banners (LayerWeb), the hub's scheduled-message routes, which answer 404 while it is off, and delivery (LayerServer): while it is off, pending messages are held, neither sent nor failed. A message found due more than 60 minutes late (for example after the experiment was off) is not sent; it fails as missed and the sender can send it now.",
		Default:     false,
		Layers:      []Layer{LayerWeb, LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#3666",
		Owner:       "native-chat",
		ReviewBy:    "2027-01-31",
	},
	{
		Name:        AgentEdit,
		Title:       "Edit agent",
		Description: "Adds an Edit button on the agent page and the Edit page at /agents/{id}/edit, which edits a created, stopped, failed or suspended agent's model and limits, and shows for every field whether it applies at the next start or resume, needs a reincarnation, or is fixed.",
		Default:     false,
		Layers:      []Layer{LayerWeb},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#3952",
		Owner:       "web",
		ReviewBy:    "2027-01-07",
	},
	{
		Name:        AgentPlugins,
		Title:       "Agent Plugins (native install)",
		Description: "Installs Agent Plugins 1.0.0 bundles declared in a template's agent_plugins into harnesses that support them natively (codex, copilot, muse-code, claude). While off, agent_plugins is ignored with a start warning and previously installed plugins are removed at the next start.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#4933",
		Owner:       "harness",
		ReviewBy:    "2027-01-13",
	},
}

// compiledRetired lists names that PUT rejects true/false for, ignores and
// prunes if still stored, and that must never be reused.
//
// Retiring or graduating an experiment also requires removing its entry from
// testAdminExperimentPolicy in pkg/hub/test_admin_policy.go; otherwise the
// TestAdminPolicy classification check reports a stale entry and fails.
var compiledRetired = []string{
	"web.access_boundaries_read",
	"web.access_boundaries_authoring",
	// Graduated: the terminal workspace is always on (ptone/scion#4917).
	"web.terminal_workspace",
	// Graduated: project thread groups are always on (ptone/scion#5546).
	"hub.chat_thread_groups",
}

// Registry is an immutable, validated set of experiments. It is the only way
// to read them: there are no package-level Lookup/All functions, so hub code
// cannot bypass the instance it was given, and tests using t.Parallel()
// cannot affect each other through a shared global.
type Registry struct {
	byName  map[string]Experiment
	ordered []Experiment
	retired map[string]bool
}

// NewRegistry validates active and retired together and returns an error on
// the first invariant violation:
//   - names are unique and match NamePattern
//   - no active name appears in the retired list
//   - every entry has a non-empty Title, Description, Issue and Owner, at
//     least one Layer, a ReviewBy matching YYYY-MM-DD, and a recognized Stage
//
// The returned Registry owns a private copy of each entry's Layers slice, so
// mutating a slice the caller passed in afterward cannot affect the Registry.
func NewRegistry(active []Experiment, retired []string) (*Registry, error) {
	retiredSet := make(map[string]bool, len(retired))
	for _, name := range retired {
		retiredSet[name] = true
	}

	byName := make(map[string]Experiment, len(active))
	ordered := make([]Experiment, 0, len(active))
	for _, e := range active {
		if !ValidName(e.Name) {
			return nil, fmt.Errorf("experiments: invalid name %q: must match %s", e.Name, NamePattern)
		}
		if _, dup := byName[e.Name]; dup {
			return nil, fmt.Errorf("experiments: duplicate name %q", e.Name)
		}
		if retiredSet[e.Name] {
			return nil, fmt.Errorf("experiments: %q is both active and retired", e.Name)
		}
		if e.Title == "" || e.Description == "" || e.Issue == "" || e.Owner == "" || e.ReviewBy == "" {
			return nil, fmt.Errorf("experiments: %q is missing a required field (title, description, issue, owner, review_by)", e.Name)
		}
		if len(e.Layers) == 0 {
			return nil, fmt.Errorf("experiments: %q has no layers", e.Name)
		}
		if _, err := time.Parse(reviewByLayout, e.ReviewBy); err != nil {
			return nil, fmt.Errorf("experiments: %q has an invalid review_by %q: must match YYYY-MM-DD", e.Name, e.ReviewBy)
		}
		if !validStages[e.Stage] {
			return nil, fmt.Errorf("experiments: %q has an invalid stage %q", e.Name, e.Stage)
		}
		e.Layers = slices.Clone(e.Layers)
		byName[e.Name] = e
		ordered = append(ordered, e)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	return &Registry{byName: byName, ordered: ordered, retired: retiredSet}, nil
}

// All returns every active experiment in stable order (by Name). The
// returned slice, and each entry's Layers slice, are copies: mutating either
// does not affect the Registry or any other caller's result.
func (r *Registry) All() []Experiment {
	out := slices.Clone(r.ordered)
	for i := range out {
		out[i].Layers = slices.Clone(out[i].Layers)
	}
	return out
}

// Lookup returns the named active experiment, or false if it is unknown or
// retired. The returned Experiment's Layers slice is a copy; mutating it
// does not affect the Registry or any other caller's result.
func (r *Registry) Lookup(name string) (Experiment, bool) {
	e, ok := r.byName[name]
	if ok {
		e.Layers = slices.Clone(e.Layers)
	}
	return e, ok
}

// IsRetired reports whether name was retired and must never resolve again.
func (r *Registry) IsRetired(name string) bool { return r.retired[name] }

// Resolve returns the effective value of the named experiment given the
// admin overrides and whether the overrides document is malformed: an
// unknown or retired name is off; a malformed document turns off every
// experiment with a server layer (fail closed); an override wins over the
// registry default. The hub and a separate relay both resolve through it.
func (r *Registry) Resolve(name string, overrides map[string]bool, malformed bool) bool {
	exp, ok := r.Lookup(name)
	if !ok {
		return false
	}
	if malformed && exp.HasLayer(LayerServer) {
		return false
	}
	if v, set := overrides[name]; set {
		return v
	}
	return exp.Default
}

// Retired returns every retired name in sorted order. The returned slice is
// a copy. Tests use it to check that a stored override for any retired name
// is ignored and pruned, without naming each retired experiment.
func (r *Registry) Retired() []string {
	out := make([]string, 0, len(r.retired))
	for name := range r.retired {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// defaultRegistry builds the production registry exactly once.
var defaultRegistry = sync.OnceValue(func() *Registry {
	r, err := NewRegistry(compiled, compiledRetired)
	if err != nil {
		// The invariant tests run this exact construction and fail first,
		// so an invalid compiled list never ships.
		panic(fmt.Sprintf("experiments: invalid compiled registry: %v", err))
	}
	return r
})

// Default returns the compiled production registry, built once. It panics if
// the compiled list is invalid.
func Default() *Registry { return defaultRegistry() }
