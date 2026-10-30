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
// readable after it is turned off. Its compiled default is off, but a hub
// whose artifact service is ready (local blob storage included) or whose
// resource storage is an object store resolves it on unless an override is
// stored (ptone/scion#5827, ptone/scion#5828; pkg/hub
// deploymentExperimentDefault).
const AttachmentArtifacts = "hub.attachment_artifacts"

// ChatScheduledSend gates scheduled send in native web chat: the Schedule
// send menu item and pending-message banners in the web UI, the hub's
// /api/v1/chat/conversations/{key}/scheduled routes (404 while off), and
// the delivery sweeper, which holds pending messages while it is off.
const ChatScheduledSend = "web.chat_scheduled_send"

// ChatStarredMessages gates starred messages in native web chat: the
// message Star/Unstar action, the private starred indicator, the
// conversation header's starred-message list, the starred-only center and
// the rail's Starred filter in the web UI, and the hub's /api/v1/chat/stars
// and /api/v1/chat/starred-conversations routes and the starred=true
// history variant (404 while off). Stored stars are kept while it is off.
const ChatStarredMessages = "web.chat_starred_messages"

// AuthorizationDecisionAuditV2 gates decision logging (remaining-audit P1).
// It is the only activation control and stays default-off; turning it on
// needs explicit owner approval.
const AuthorizationDecisionAuditV2 = "hub.authorization_decision_audit_v2"

// AgentEdit gates what the web Edit agent page (/agents/{id}/edit) adds
// beyond the retired Configure page: the grouping of its fields by when an
// edit applies, and the summary strip counting them. The page itself, and
// the Edit button on the agent page, are always available. It is web-only:
// the hub's config PATCH accepts edits of agents with no container whether
// or not it is on.
const AgentEdit = "web.agent_edit"

// AgentEditHeld gates held config edits (ptone/scion#3976): while it is on,
// a config edit of an agent with a live or transitional container is kept
// as the agent's pending config and applied at its next start, a
// provision-rendered key is kept pending, and DELETE
// /api/v1/agents/{id}/pending-config discards pending edits. A
// reincarnation does not apply pending edits yet (ptone/scion#3978), so it
// must stay off until that lands. While it is off the hub refuses a config
// edit of such an agent, as before, and applies no stored pending config;
// a config edit it writes removes the pending keys that edit names, so a
// stale pending value cannot later override it. It has its own default-off
// switch
// because the Edit page experiment (AgentEdit) is turned on separately; the
// web layer only mirrors the hub's check.
const AgentEditHeld = "hub.agent_edit_held"

// AgentDelegation gates agent delegation (.design/agent-delegation.md §10):
// the grant issuance and credential exchange routes, the delegated
// credential middleware arm, and the delegated decision procedure. While it
// is off, issuance and exchange answer 404 and every delegated credential is
// refused. Turning it off suspends stored grants; it does not revoke them.
const AgentDelegation = "hub.agent_delegation"

// FederationProjectVisitors gates project access for users of trusted
// external issuers (ptone/scion#4603). It has both layers: the web layer
// shows the project Visitors tab and the per-issuer visitor access switch,
// and the server layer gates the hub behaviour below. While it is off, a
// verified federated user token always yields the same federated user identity as
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
// and the request opted in (acceptAsyncLaunch). It is on by default
// (ptone/scion#6640); an admin override turns it off. It is read at each
// create send, so toggling needs no restart; launches already accepted
// finish or are reaped either way.
const AsyncAgentLaunch = "hub.async_agent_launch"

// AsyncAgentStart gates asynchronous start, resume and restart of existing
// agents (ptone/scion#4669). It applies only while hub.async_agent_launch is
// on too. Turn it on only after every hub node runs a version that
// advertises async_start_v1 in the hub-instance registry; to roll back,
// turn it off and wait launch_timeout plus 15 seconds before downgrading
// any node. While any live hub instance lacks async_start_v1, starts stay
// synchronous whatever its value.
const AsyncAgentStart = "hub.async_agent_start"

// AgentBadges gates the agent badges (ptone/scion#5503): a harness vendor
// mark and a model size chip in the upper-left of agent cards, table rows
// and graph nodes. It is web-only and presentation-only: the hub sends the
// harnessVendor and modelSize fields whether or not it is on.
const AgentBadges = "web.agent_badges"

// SSEHiddenTabClose gates closing the web UI's events streams in a tab that
// has been hidden for 60 s (ptone/scion#5235). It is web-only: the hub
// serves streams the same way whether or not it is on.
const SSEHiddenTabClose = "web.sse_hidden_tab_close"

// SecretRotation gates opt-in Hub server keyring configuration
// (server.keyring_file, ptone/scion#1530): a hub configured with a keyring
// file refuses to start while it is off. It is checked once at startup
// (the keyring is loaded once, before any listener opens); turning it off
// later changes nothing until the next restart. Readers of persisted
// keyring-format data must never depend on it. The design names it
// "server.secret-rotation"; NamePattern allows no hyphen.
const SecretRotation = "server.secret_rotation"

// ContentHashV2 gates the content hash the hub records for template and
// harness-config commits (ptone/scion#5658): while it is on, commits record
// transfer.ComputeContentHashV2 (paths, normalised modes and file hashes);
// while it is off, transfer.ComputeContentHashV1 (file hashes only). The hub
// checks it at every commit, so toggling needs no restart. Rows keep the
// version they were last committed with, and the commit compare-and-swap is
// a strict equality on the stored value, so mixed rows are safe in both
// directions. Skill hashes do not follow it (see SkillContentHashV2).
// Brokers and CLIs do not read it: their cache keys and sync checks do not
// depend on the hash version.
// While it is off, a rename-only or chmod-only re-upload is not visible to
// the expected-hash check (the behaviour before ptone/scion#5658). Enable it
// once every CLI and agent image is at or after the release that added it;
// ptone/scion#5995 makes it the default and removes the gate.
const ContentHashV2 = "hub.content_hash_v2"

// SkillContentHashV2 gates the skill content hash v2 transition
// (ptone/scion#5935). Skill hash v2 is transfer.ComputeContentHashV2 over a
// skill's files (paths, normalised modes and per-file hashes); v1
// (transfer.ComputeContentHashV1, file hashes only) stays the recorded
// skill_versions.content_hash and the contentHash field of every response,
// because brokers before ptone/scion#5935 verify an installed bundle by
// recomputing v1 and existing registry pins hold v1. v2 is never stored: it
// is derived from a version's immutable file list. While it is on, the hub
// adds contentHashV2 to skill resolve and skill version responses, accepts a
// v2 value wherever it accepts a skill content hash ("@sha256:" version
// lookup and pinned-registry trust; v1 is tried first). For a federated
// skill, at every trust level, it recomputes v1 and v2 from the registry's
// file list (paths, per-file hashes, modes) and fails closed when the
// claimed contentHash or a claimed contentHashV2 disagrees, and it sends
// only its own recomputed v2. A pinned registry must list a hash for every
// file and its pin must equal the recomputed v1 or v2; a skill that has no
// contentHash and cannot be recomputed fails at every trust level; a
// trusted registry that lists no per-file hashes but does send a
// contentHash is accepted without contentHashV2 (the broker still verifies
// v1). While it is off the API is unchanged and a claimed v1 that
// disagrees with the files is only logged. Brokers verify contentHashV2 when a
// response carries it and do not read the experiment. ptone/scion#5995 makes
// it the default and removes the gate.
const SkillContentHashV2 = "hub.skill_content_hash_v2"

// ArtifactReviewEditor gates the graphical review editor for Markdown
// artifacts (ptone/scion#5913). It is web-only: every save goes through the
// unchanged review publish, and the hub's review checks are the same
// whether or not it is on.
const ArtifactReviewEditor = "web.artifact_review_editor"

// UserDeleteForceCascade gates the forced user delete (ptone/scion#2143):
// DELETE /api/v1/users/{id}?force=true, which revokes the user's authority,
// records a deletion tombstone and stops the user's agents, the GET
// /api/v1/users/{id}/deletion resource, and the hub's reconciler that
// completes such deletions. The hub checks it (the force parameter with
// experimentEnabled, the sub-route with requireExperiment); the web layer
// only mirrors it to show the force option.
const UserDeleteForceCascade = "hub.user_delete_force_cascade"

// compiled is the production experiment list. It is reachable only through
// Default(); there is no package-level Lookup/All, so hub code cannot bypass
// the Registry instance it was given (ptone/scion#2217).
var compiled = []Experiment{
	{
		Name:        AsyncAgentStart,
		Title:       "Asynchronous agent start",
		Description: "Lets the hub answer a start, resume or restart of an existing agent as soon as the Runtime Broker accepts it, like an asynchronous create; it applies only while hub.async_agent_launch is also on. Turn it on only after every hub node runs a version that supports it (each advertises async_start_v1). While any live hub instance lacks async_start_v1, starts stay synchronous. To roll back, turn it off and wait launch_timeout plus 15 seconds before downgrading any node.",
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
		Description: "Lets the hub answer an agent create as soon as the Runtime Broker accepts it, while the broker finishes the launch in the background; clients that opt in (scion start and resume, the web create form, scheduled creates) follow its progress. Provision-only creates, reprovisioning and brokers without async launch support stay synchronous. On by default; turning it off makes new creates synchronous, and launches already accepted finish or time out as before.",
		Default:     true,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#4991",
		Owner:       "async-start",
		ReviewBy:    "2027-01-12",
	},
	{
		Name:        FederationProjectVisitors,
		Title:       "Project visitors from trusted issuers",
		Description: "Lets a user of a trusted external issuer who is linked to a local federated account act as that account (a project visitor), and lets this hub's users obtain short-lived user access credentials for the peer hubs in server.oidc.user_token_audiences, and accepts user access credentials from peer hub issuers that have accept_user_tokens. While off, federated user tokens behave exactly as before, no user access credentials are issued, no user access credentials are accepted from peer hubs, and stored accounts are kept. Web layer: shows the project Visitors tab and the visitor access switch on Admin > Federation. Server layer: the project visitor grant and rule routes (404 while off), visitor identities and user access credentials; the hub checks it on every request, whatever the UI shows.",
		Default:     false,
		Layers:      []Layer{LayerWeb, LayerServer},
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
		Description: "Stores files uploaded in web chat in the artifact service as attachments of the message they are sent in: the stored file is readable exactly by the readers of that message, and is removed when the message is deleted. It is delivered into each recipient agent's inbox; for a message in a project topic thread a copy is also placed in the project's scratchpad shared directory, where every agent and member with access to that directory can read it (direct and group conversations get inbox delivery only). Gates the hub's attachment upload path (LayerServer; it also needs the artifact service with its settings section enabled, but not hub.artifacts) and is reserved for the web surfaces that follow (LayerWeb). Attachments stored while it was on stay readable after it is turned off. Defaults on wherever the artifact service is ready, local blob storage included, and on hubs whose resource storage is an object store (GCS).",
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
		Name:        ChatStarredMessages,
		Title:       "Starred messages in chat",
		Description: "Lets each user star chat messages privately and find them again from the conversation header. Gates the Star message action, the starred indicator and the header list (LayerWeb) and the hub's starred-message routes, which answer 404 while it is off (LayerServer). Stars are stored per user and are never shown to anyone else; turning it off keeps them.",
		Default:     true,
		Layers:      []Layer{LayerWeb, LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#5797",
		Owner:       "native-chat",
		ReviewBy:    "2027-01-31",
	},
	{
		Name:        AgentEdit,
		Title:       "Edit agent",
		Description: "On the Edit agent page, adds a view that groups the fields by when an edit applies (next start or resume, held, needs a reincarnation, immediately, fixed) and a summary strip counting them. The page and its by-function view are always available.",
		Default:     true,
		Layers:      []Layer{LayerWeb},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#3952",
		Owner:       "web",
		ReviewBy:    "2027-01-07",
	},
	{
		Name:        AgentEditHeld,
		Title:       "Held edits for running agents",
		Description: "Lets an agent config edit be saved while the agent is running or starting: the hub keeps it as the agent's pending config and applies it at the next start, restart or resume. System prompt, agent instructions, skills and services edits are kept pending, unless the agent's Runtime Broker re-renders them at start (changedFields), in which case they are applied like other edits; a reincarnation does not apply pending edits yet, so what a start does not apply stays pending, and the rest is applied at the next start after a reincarnation, over the values it set. Keep it off until reincarnation applies pending edits. Pending edits can be discarded. While off, a config edit of a running agent is refused as before; stored pending edits are kept but not applied, except that an edit saved while off removes the pending keys it names.",
		Default:     false,
		Layers:      []Layer{LayerServer, LayerWeb},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#3952",
		Owner:       "web",
		ReviewBy:    "2027-01-20",
	},
	{
		Name:        AgentBadges,
		Title:       "Agent badges",
		Description: "Shows a harness vendor mark and the configured model size (S, M, L, XL) in the upper-left of each agent in the agent list, agent cards and graph views; the tooltip names the harness and the size. Multi-vendor harnesses (OpenCode, Copilot, Hermes) show the harness mark only. Presentation only: the hub sends the fields whether or not it is on.",
		Default:     false,
		Layers:      []Layer{LayerWeb},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#5503",
		Owner:       "web",
		ReviewBy:    "2027-01-19",
	},
	{
		Name:        SSEHiddenTabClose,
		Title:       "Close live updates in hidden tabs",
		Description: "A browser tab hidden for 60 seconds closes its live-update streams (page, agent list and terminal metadata) and reopens them, refreshing what it shows, when it is shown again. Terminal connections stay open. Web only; the hub is unaffected.",
		Default:     false,
		Layers:      []Layer{LayerWeb},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#5235",
		Owner:       "web",
		ReviewBy:    "2027-01-22",
	},
	{
		Name:        ArtifactReviewEditor,
		Title:       "Graphical review editor",
		Description: "Opens a Markdown artifact review in a graphical editor that shows marks inline, records typing, deleting and replacing as suggestions, and has a Graphical and Source toggle. Needs hub.artifacts. Web only; the hub saves and checks reviews the same way whether or not it is on. Do not turn it on before the page script policy (ptone/scion#5912) is deployed on the hub.",
		Default:     false,
		Layers:      []Layer{LayerWeb},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#5913",
		Owner:       "artifacts",
		ReviewBy:    "2027-01-21",
	},
	{
		Name:        UserDeleteForceCascade,
		Title:       "Force user delete",
		Description: "Lets an admin delete a user who has agents: DELETE /api/v1/users/{id}?force=true revokes the user's authority at once (status deleting, role bindings, access tokens, delegations), stops the user's agents through the agent delete, and removes the user once every agent is stopped or removed by an operator. Needs agent.delete on every one of those agents. While off, force is ignored (a user with agents is refused as before) and the deletion resource answers 404; a deletion accepted while it was on runs to completion.",
		Default:     false,
		Layers:      []Layer{LayerServer, LayerWeb},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#2143",
		Owner:       "authz",
		ReviewBy:    "2027-01-31",
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
	{
		Name:        SecretRotation,
		Title:       "Server keyring (session cookie pilot)",
		Description: "Lets a hub start with a versioned keyring file (server.keyring_file). In this release only web session cookies use it: several signing generations can be read at once, with deadlines, while every other purpose keeps the legacy server secret. Checked at startup; a hub with a keyring file refuses to start while it is off. Full server-secret rotation is not available yet.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#1530",
		Owner:       "secret-rotation",
		ReviewBy:    "2027-01-31",
	},
	{
		Name:        ContentHashV2,
		Title:       "Content hash v2 (paths and modes)",
		Description: "While on, template and harness-config commits record a content hash that covers each file's path and executable bit as well as its content, so a rename-only or chmod-only commit changes the hash and a client that diffed against the earlier version gets 409 instead of overwriting it. While off (the default), commits record the earlier hash, which covers file contents only: a rename-only or chmod-only re-upload is not visible to the hub's expected-hash check, as before. Turn it on only when every CLI and agent image that talks to this hub is at or after the release that added it; an older CLI compares its own earlier hash with the hub's and, without a terminal, fails scion start, resume and create with -t for a template it also has locally. Skill hashes do not change either way.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#5658",
		Owner:       "templates",
		// ptone/scion#5995 makes it the default and removes the gate.
		ReviewBy: "2027-01-21",
	},
	{
		Name:        SkillContentHashV2,
		Title:       "Skill content hash v2 (paths and modes)",
		Description: "While on, skill resolve and skill version responses also carry contentHashV2, a skill content hash that covers each file's path and executable bit as well as its content, and the hub accepts either hash form for an @sha256: version and for a pinned registry hash, so a skill can be pinned by a hash that a rename-only or chmod-only change would break. A federated skill is also checked against the hashes the hub recomputes from the registry's file list, and fails if the registry's claimed hash disagrees; a pinned registry must list a hash for every file, and a skill that sends no content hash and lists no per-file hashes is rejected from any registry. contentHash stays the earlier hash (file contents only), which older brokers verify and existing pins hold. While off (the default), responses are unchanged and pins match the earlier hash only. Brokers at or after this release verify contentHashV2 whenever a response carries it.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#5935",
		Owner:       "skills",
		// ptone/scion#5995 makes it the default and removes the gate.
		ReviewBy: "2027-01-24",
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
