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

package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// RuntimeBrokerService handles runtime broker operations.
type RuntimeBrokerService interface {
	// Create creates a new broker registration and returns a join token.
	// The join token must be used with Join() to complete registration.
	Create(ctx context.Context, req *CreateBrokerRequest) (*CreateBrokerResponse, error)

	// Join completes broker registration using a join token.
	// Returns the HMAC secret key for future authentication.
	Join(ctx context.Context, req *JoinBrokerRequest) (*JoinBrokerResponse, error)

	// List returns runtime brokers matching the filter criteria.
	List(ctx context.Context, opts *ListBrokersOptions) (*ListBrokersResponse, error)

	// Get returns a single runtime broker by ID.
	Get(ctx context.Context, brokerID string) (*RuntimeBroker, error)

	// Update updates broker metadata.
	Update(ctx context.Context, brokerID string, req *UpdateBrokerRequest) (*RuntimeBroker, error)

	// Delete removes a broker from all projects.
	Delete(ctx context.Context, brokerID string) error

	// ListProjects returns projects this broker contributes to.
	ListProjects(ctx context.Context, brokerID string) (*ListBrokerProjectsResponse, error)

	// Heartbeat sends a heartbeat for a broker.
	// The response is nil when the Hub replies with an empty body (a Hub
	// that predates BrokerHeartbeatResponse).
	Heartbeat(ctx context.Context, brokerID string, status *BrokerHeartbeat) (*BrokerHeartbeatResponse, error)

	// ReportMessageFailures reports hub messages that the broker accepted
	// into its delivery buffer but failed to deliver, so the hub can mark
	// them failed instead of leaving them "dispatched".
	ReportMessageFailures(ctx context.Context, brokerID string, req *MessageFailuresReport) error

	// ReportAgentLaunch sends one broker->hub launch report (design
	// t1-async-create-v11.md §3.2, §7 P1b-1): a claim, checkpoint, progress
	// update, keepalive, or terminal (succeeded/failed) for an async-launch
	// agent create. The returned error is non-nil only for a condition the
	// sender must treat as "unreachable, retry" (design §3.8.2 table): a
	// transport failure, a 5xx, or a 404 that is not the structured
	// agent_launch_unknown body (an old Hub node without this route,
	// design §5 N-9). Every other outcome — the 200 result, the 403 (another
	// broker owns the agent), the definitive 404 agent_launch_unknown, or the
	// 409 stale_launch with its reason — is returned in the result with a nil
	// error, so the sender can switch on it directly.
	ReportAgentLaunch(ctx context.Context, brokerID, agentID string, req *AgentLaunchReport) (*AgentLaunchReportResult, error)
}

// AgentLaunchReport is the broker->hub launch report wire type (design §3.2).
// It mirrors pkg/hub.AgentLaunchReport field for field; the two cannot share
// a Go type because pkg/hub cannot depend on pkg/hubclient (and vice versa).
type AgentLaunchReport struct {
	LaunchID   string                 `json:"launchId"`
	InstanceID string                 `json:"instanceId"`
	Seq        int64                  `json:"seq"`
	State      string                 `json:"state"` // claim | checkpoint | progress | succeeded | failed
	Phase      string                 `json:"phase,omitempty"`
	Step       string                 `json:"step,omitempty"`
	Message    string                 `json:"message,omitempty"`
	ErrorCode  string                 `json:"errorCode,omitempty"`
	Cause      string                 `json:"cause,omitempty"` // failed runtime_error only: an api.BrokerRuntimeCause code
	Agent      *AgentLaunchReportInfo `json:"agent,omitempty"` // succeeded only
	At         time.Time              `json:"at,omitzero"`
}

// AgentLaunchReport.State values.
const (
	AgentLaunchReportStateClaim      = "claim"
	AgentLaunchReportStateCheckpoint = "checkpoint"
	AgentLaunchReportStateProgress   = "progress"
	AgentLaunchReportStateSucceeded  = "succeeded"
	AgentLaunchReportStateFailed     = "failed"
)

// AgentLaunchReportInfo is the succeeded report's agent echo (design §3.2's
// AgentLaunchReport.Agent, a RemoteAgentInfo on the Hub side). The Hub
// applies the resolved config (template, harness config and auth, harness
// config source, image, profile, runtime) as it does for a synchronous
// create (ptone/scion#4818), plus RunID and WorkspacePlacement; it ignores
// the rest.
type AgentLaunchReportInfo struct {
	ID              string `json:"id,omitempty"`
	Slug            string `json:"slug,omitempty"`
	ContainerID     string `json:"containerId,omitempty"`
	Name            string `json:"name,omitempty"`
	Template        string `json:"template,omitempty"`
	HarnessConfig   string `json:"harnessConfig,omitempty"`
	HarnessAuth     string `json:"harnessAuth,omitempty"`
	Image           string `json:"image,omitempty"`
	Runtime         string `json:"runtime,omitempty"`
	RuntimeState    string `json:"runtimeState,omitempty"`
	Profile         string `json:"profile,omitempty"`
	Phase           string `json:"phase,omitempty"`
	Activity        string `json:"activity,omitempty"`
	ContainerStatus string `json:"containerStatus,omitempty"`
	// HarnessConfigSource is which harness-config source the start resolved
	// (wire key matches RemoteAgentInfo.HarnessConfigSource).
	HarnessConfigSource string `json:"harnessConfigSource,omitempty"`
	// RunID is the run the launched entry is labelled with
	// (ptone/scion#3176), so the hub can settle exactly that run.
	RunID string `json:"runId,omitempty"`
	// WorkspacePlacement is where the launch's start placed the agent's
	// workspace (api.WorkspacePlacementExport or WorkspacePlacementLocal).
	WorkspacePlacement string `json:"workspacePlacement,omitempty"`
}

// AgentLaunchReportResult is ApplyLaunchReport's answer (design §3.2),
// decoded from whichever of the four response shapes the Hub returned.
type AgentLaunchReportResult struct {
	// HTTPStatus is 0 for the 200 case; otherwise 403, 404 or 409.
	HTTPStatus int
	// Result is set when HTTPStatus == 0: "applied" | "duplicate" | "completed".
	Result string
	// Code is set for 404/409: "agent_launch_unknown" | "stale_launch".
	Code string
	// Reason is set for a 409 stale_launch: superseded | deleted | stopped |
	// timed_out | lost | failed | not_launched | other_owner.
	Reason string
}

// AgentLaunchReportResult.Result values.
const (
	AgentLaunchReportResultApplied   = "applied"
	AgentLaunchReportResultDuplicate = "duplicate"
	AgentLaunchReportResultCompleted = "completed"
)

// AgentLaunchReportResult.Code values.
const (
	AgentLaunchReportCodeUnknownLaunch = "agent_launch_unknown"
	AgentLaunchReportCodeStaleLaunch   = "stale_launch"
)

// AgentLaunchReportResult.Reason values (409 stale_launch only).
const (
	AgentLaunchReportReasonSuperseded  = "superseded"
	AgentLaunchReportReasonDeleted     = "deleted"
	AgentLaunchReportReasonStopped     = "stopped"
	AgentLaunchReportReasonTimedOut    = "timed_out"
	AgentLaunchReportReasonLost        = "lost"
	AgentLaunchReportReasonFailed      = "failed"
	AgentLaunchReportReasonNotLaunched = "not_launched"
	AgentLaunchReportReasonOtherOwner  = "other_owner"
)

// MessageFailure is one buffered delivery that failed on the broker.
type MessageFailure struct {
	MessageID string `json:"messageId"`
	AgentID   string `json:"agentId,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// MessageFailuresReport is the body of a message-failures report.
type MessageFailuresReport struct {
	Failures []MessageFailure `json:"failures"`
}

// runtimeBrokerService is the implementation of RuntimeBrokerService.
type runtimeBrokerService struct {
	c *client
}

// ListBrokersOptions configures runtime broker list filtering.
type ListBrokersOptions struct {
	Status    string // Filter by status (online, offline)
	ProjectID string // Filter by project contribution
	Name      string // Exact match on broker name (case-insensitive)
	Page      apiclient.PageOptions
}

// ListBrokersResponse is the response from listing runtime brokers.
type ListBrokersResponse struct {
	Brokers []RuntimeBroker
	Page    apiclient.PageResult
}

// UpdateBrokerRequest is the request for updating a runtime broker.
type UpdateBrokerRequest struct {
	Name        string            `json:"name,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ListBrokerProjectsResponse is the response from listing broker projects.
type ListBrokerProjectsResponse struct {
	Projects []BrokerProjectInfo `json:"projects"`
}

// BrokerHeartbeat is the heartbeat payload.
type BrokerHeartbeat struct {
	Status   string             `json:"status"`
	Projects []ProjectHeartbeat `json:"projects,omitempty"`
	// Capabilities refreshes the broker's reported capabilities on every
	// heartbeat (design §3.4 Amendment A2.2(b)). Chosen over a hub->broker live /info query:
	// no such query path exists today, and adding one would mean a new
	// authenticated hub-initiated call plus endpoint resolution and timeout
	// handling on the `scion reincarnate` pre-flight path, for a value that
	// changes at most once per broker binary upgrade. Piggybacking on the
	// heartbeat the broker already sends every few seconds gets the same
	// "never stale for long" property for free. A CompleteBrokerJoin-time
	// snapshot alone (the pre-A2 state) is never refreshed for an
	// already-registered broker until it re-registers with --force.
	Capabilities *BrokerCapabilities `json:"capabilities,omitempty"`
	// Inventory reports, per runtime target, whether the agent list in
	// Projects is that target's complete inventory. The Hub only treats an
	// agent missing from Projects as having no container when the agent's
	// recorded target is listed here as complete. An older broker omits the
	// field, and the Hub then never draws that conclusion.
	Inventory *BrokerInventory `json:"inventory,omitempty"`
	// WorkspaceStorage refreshes the broker's workspace storage descriptor
	// (backend, NFS export identity and share health) on every heartbeat.
	// An older broker omits it and the hub keeps the stored value.
	WorkspaceStorage *api.BrokerWorkspaceStorage `json:"workspaceStorage,omitempty"`
	// ProfileAttach refreshes the attach capability of the broker's
	// registered profiles (store.BrokerProfile.Attach) on every heartbeat,
	// so a change is seen without re-registering. It lists only profiles
	// whose attach support the broker knows; a profile it cannot answer
	// for yet is left out, and the hub keeps that profile's stored value.
	// An older broker omits the field and the hub keeps every stored
	// value.
	ProfileAttach []ProfileAttachState `json:"profileAttach,omitempty"`
	// ProfileSAMappings: see ProfileSAMappingsState. Sent only when a
	// profile's report changed, or when the Hub asked for it.
	ProfileSAMappings []ProfileSAMappingsState `json:"profileSAMappings,omitempty"`
	// ProfileSAMappingsHashes carries the hash of every Kubernetes
	// profile's report on every heartbeat, so the Hub can confirm its
	// stored report is current. An older broker omits it.
	ProfileSAMappingsHashes []ProfileSAMappingsHash `json:"profileSAMappingsHashes,omitempty"`
	// StartsInFlight lists the agent starts still running on the broker
	// when this heartbeat was built, read before the agents were listed, so
	// a start that finishes between the two reads is either listed here or
	// its container is in Projects. Meaningful only when
	// Capabilities.StartsInFlight is true; an older broker omits both.
	StartsInFlight []StartInFlight `json:"startsInFlight,omitempty"`
	// DefaultProfile refreshes the broker's default (active) profile name
	// on every heartbeat. Nil (an older broker) keeps the stored value; a
	// non-nil empty string reports that the broker has no active profile.
	DefaultProfile *string `json:"defaultProfile,omitempty"`
	// Health is the broker's report of its own health (default runtime,
	// NFS mounts), refreshed on every heartbeat. It never changes the
	// broker's online/offline status. An older broker omits it and the
	// hub keeps the stored value; an older hub ignores it.
	Health *api.BrokerHealthReport `json:"health,omitempty"`
	// RuntimeTargetPresentation is a flat Runtime Broker instance's full
	// FC4-a presentation (config.PresentInstance), sent on every heartbeat
	// as a snapshot, so a lost update or a Hub restore is repaired by the
	// next beat. The Hub stores it read-only, only from the broker's own
	// credentials, and only when the fence below advances. A legacy broker
	// and an older broker omit it; the Hub then keeps the stored value.
	RuntimeTargetPresentation *api.RuntimeTargetPresentation `json:"runtimeTargetPresentation,omitempty"`
	// PresentationTargetID is the instance's runtime target ID
	// (RuntimeTargetDescriptor.ID); the Hub stores the presentation only
	// when it equals the row's stored target.
	PresentationTargetID string `json:"presentationTargetId,omitempty"`
	// PresentationEpoch is the reporting process's start time in Unix
	// nanoseconds, fixed for the process life; PresentationSeq is its
	// per-process heartbeat counter. Together they fence the write: an
	// older process can never overwrite a newer one's snapshot.
	PresentationEpoch int64 `json:"presentationEpoch,omitempty"`
	PresentationSeq   int64 `json:"presentationSeq,omitempty"`
}

// StartInFlight identifies one agent start running on a broker.
type StartInFlight struct {
	ProjectID string `json:"projectId"`
	Slug      string `json:"slug"`
}

// ProfileAttachState is one profile's attach capability in a heartbeat.
type ProfileAttachState struct {
	// Name is the profile name, matching store.BrokerProfile.Name.
	Name string `json:"name"`
	// Attach reports whether the profile's runtime supports interactive
	// attach.
	Attach bool `json:"attach"`
}

// BrokerInventory describes which runtime targets a heartbeat's agent list
// covers.
type BrokerInventory struct {
	// Targets has one entry per runtime target the broker manages (its
	// default runtime and each auxiliary runtime). It is empty when the
	// heartbeat is filtered (multi-hub mode) and so claims no target.
	Targets []InventoryTarget `json:"targets,omitempty"`
	// RunLabels is true when every listed runtime entry is reported with
	// its scion.run_id label (AgentHeartbeat.RunID), including the entries
	// of an agent listed on more than one target (AgentHeartbeat.OtherEntries).
	// An older broker omits it, and the Hub then never adopts a run from
	// this heartbeat (ptone/scion#3661).
	RunLabels bool `json:"runLabels,omitempty"`
}

// InventoryTarget is one runtime target in a heartbeat inventory.
type InventoryTarget struct {
	// ID identifies the target: the runtime name for Docker, Podman and
	// similar runtimes, and the runtime name with the cluster context and
	// namespace for Kubernetes.
	ID string `json:"id"`
	// Runtime is the runtime name (runtime.Runtime.Name()).
	Runtime string `json:"runtime,omitempty"`
	// Complete is true only when the target was listed without error, so an
	// agent on this target that is absent from the heartbeat has no
	// container.
	Complete bool `json:"complete"`
}

// ProjectHeartbeat is per-project status in a heartbeat.
type ProjectHeartbeat struct {
	ProjectID  string           `json:"projectId"`
	AgentCount int              `json:"agentCount"`
	Agents     []AgentHeartbeat `json:"agents,omitempty"`
}

// AgentHeartbeat is per-agent status in a heartbeat.
type AgentHeartbeat struct {
	Slug            string `json:"slug"` // Agent's URL-safe identifier
	Status          string `json:"status"`
	Phase           string `json:"phase,omitempty"`
	Activity        string `json:"activity,omitempty"`
	ContainerStatus string `json:"containerStatus,omitempty"`
	Message         string `json:"message,omitempty"`     // Error or status message from agent-info.json
	HarnessAuth     string `json:"harnessAuth,omitempty"` // Resolved auth method from container labels
	Profile         string `json:"profile,omitempty"`     // Settings profile used
	ExitCode        *int   `json:"exitCode,omitempty"`    // Structured exit code from runtime (nil = unknown)
	ExitReason      string `json:"exitReason,omitempty"`  // Terminal reason (see state.ExitReason), e.g. "crashed", "preempted", "oom_killed"
	// RuntimeTarget is the ID of the inventory target whose listing reported
	// this agent (see InventoryTarget.ID).
	RuntimeTarget string `json:"runtimeTarget,omitempty"`
	// RunID is the listed runtime entry's scion.run_id label
	// (api.AgentInfo.RunID); empty for an unlabelled entry. An older broker
	// omits it.
	RunID string `json:"runId,omitempty"`
	// OtherEntries are the agent's entries listed by other targets of this
	// broker, which the heartbeat does not report as agents of their own
	// (one agent per project and name), each with its target and label.
	OtherEntries []AgentRuntimeEntry `json:"otherEntries,omitempty"`
}

// AgentRuntimeEntry is one further runtime entry of an agent in a heartbeat.
type AgentRuntimeEntry struct {
	// RuntimeTarget is the ID of the inventory target that listed it.
	RuntimeTarget string `json:"runtimeTarget,omitempty"`
	// RunID is its scion.run_id label; empty when unlabelled.
	RunID string `json:"runId,omitempty"`
}

// CreateBrokerRequest is the request to create a new broker registration.
type CreateBrokerRequest struct {
	BrokerID     string            `json:"brokerId,omitempty"` // Optional stable broker UUID supplied by the client
	Name         string            `json:"name"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	AutoProvide  bool              `json:"autoProvide,omitempty"` // Automatically add as provider for new projects
	// RuntimeTarget is the flat Runtime Broker registration descriptor
	// (.design/flat-runtime-brokers-contract.md section 6); nil is legacy.
	RuntimeTarget *api.RuntimeTargetDescriptor `json:"runtimeTarget,omitempty"`
	// JoinTokenTTLSeconds is the join token lifetime in seconds. Zero uses
	// the hub default; otherwise the hub accepts 300 to 86400.
	JoinTokenTTLSeconds int `json:"joinTokenTtlSeconds,omitempty"`
	// PreserveSettings asks the hub to only issue a join token when the
	// name matches an existing broker, leaving its settings unchanged.
	PreserveSettings bool `json:"preserveSettings,omitempty"`
}

// CreateBrokerResponse is returned when creating a new broker.
type CreateBrokerResponse struct {
	BrokerID     string `json:"brokerId"`
	JoinToken    string `json:"joinToken"`
	ExpiresAt    string `json:"expiresAt"`
	Reregistered bool   `json:"reregistered,omitempty"`
	// RuntimeTarget is the Hub's acknowledgement of the stored flat target
	// binding; nil from a legacy row or a Hub that predates the contract.
	RuntimeTarget *api.RuntimeTargetDescriptor `json:"runtimeTarget,omitempty"`
	// Reissued is true when an earlier, unused join token for this broker
	// was replaced and no longer works.
	Reissued bool `json:"reissued,omitempty"`
	// KeptSettings lists requested values the Hub did not apply because
	// the stored value is kept (fields managed through the update API).
	KeptSettings []KeptSetting `json:"keptSettings,omitempty"`
}

// KeptSetting is a requested registration value the Hub kept unchanged.
type KeptSetting struct {
	// Field is gcpHostServiceAccountEmail, gcpHostProjectId or labels.<key>.
	Field          string `json:"field"`
	StoredValue    string `json:"storedValue"`
	RequestedValue string `json:"requestedValue"`
	// Reason is "set"; "pinned" for a value cleared through the update
	// API; or "email_required" for a project sent without a service account.
	Reason string `json:"reason"`
}

// Message is the one-line operator message for a kept setting.
func (k KeptSetting) Message() string {
	if k.Reason == "email_required" {
		return fmt.Sprintf("not applied %s=%q: send gcpHostServiceAccountEmail together with the project", k.Field, k.RequestedValue)
	}
	return fmt.Sprintf("kept %s=%q (requested %q); change it through the Runtime Broker update API (PATCH /api/v1/runtime-brokers/{id})", k.Field, k.StoredValue, k.RequestedValue)
}

// PrintKeptSettings writes one Message line per kept setting to w.
func PrintKeptSettings(w io.Writer, kept []KeptSetting) {
	for _, k := range kept {
		_, _ = fmt.Fprintln(w, k.Message())
	}
}

// JoinBrokerRequest is the request to complete broker registration.
type JoinBrokerRequest struct {
	BrokerID     string          `json:"brokerId"`
	JoinToken    string          `json:"joinToken"`
	Hostname     string          `json:"hostname"`
	Version      string          `json:"version"`
	Capabilities []string        `json:"capabilities,omitempty"`
	Profiles     []BrokerProfile `json:"profiles,omitempty"`
	// WorkspaceStorage is the broker's workspace storage descriptor at
	// registration time. Share health is refreshed by heartbeats.
	WorkspaceStorage *api.BrokerWorkspaceStorage `json:"workspaceStorage,omitempty"`
	// DefaultProfile is the broker's default (active) profile name. Nil
	// (an older broker) keeps the stored value.
	DefaultProfile *string `json:"defaultProfile,omitempty"`
	// RuntimeTarget is the flat Runtime Broker descriptor; nil is legacy.
	RuntimeTarget *api.RuntimeTargetDescriptor `json:"runtimeTarget,omitempty"`
}

// JoinBrokerResponse is returned after completing broker registration.
type JoinBrokerResponse struct {
	SecretKey   string `json:"secretKey"` // Base64-encoded HMAC secret
	HubEndpoint string `json:"hubEndpoint"`
	BrokerID    string `json:"brokerId"`
	// RuntimeTarget is the Hub's acknowledgement of the stored flat target
	// binding; nil from a legacy row or a Hub that predates the contract.
	RuntimeTarget *api.RuntimeTargetDescriptor `json:"runtimeTarget,omitempty"`
}

// Create creates a new broker registration and returns a join token.
func (s *runtimeBrokerService) Create(ctx context.Context, req *CreateBrokerRequest) (*CreateBrokerResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/brokers", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[CreateBrokerResponse](resp)
}

// Join completes broker registration using a join token.
func (s *runtimeBrokerService) Join(ctx context.Context, req *JoinBrokerRequest) (*JoinBrokerResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/brokers/join", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[JoinBrokerResponse](resp)
}

// List returns runtime brokers matching the filter criteria.
func (s *runtimeBrokerService) List(ctx context.Context, opts *ListBrokersOptions) (*ListBrokersResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		if opts.ProjectID != "" {
			query.Set("projectId", opts.ProjectID)
		}
		if opts.Name != "" {
			query.Set("name", opts.Name)
		}
		opts.Page.ToQuery(query)
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/runtime-brokers", query, nil)
	if err != nil {
		return nil, err
	}

	type listResponse struct {
		Brokers    []RuntimeBroker `json:"brokers"`
		NextCursor string          `json:"nextCursor,omitempty"`
		TotalCount int             `json:"totalCount,omitempty"`
	}

	result, err := apiclient.DecodeRequired[listResponse](resp)
	if err != nil {
		return nil, err
	}

	return &ListBrokersResponse{
		Brokers: result.Brokers,
		Page: apiclient.PageResult{
			NextCursor: result.NextCursor,
			TotalCount: result.TotalCount,
		},
	}, nil
}

// Get returns a single runtime broker by ID.
func (s *runtimeBrokerService) Get(ctx context.Context, brokerID string) (*RuntimeBroker, error) {
	resp, err := s.c.get(ctx, "/api/v1/runtime-brokers/"+brokerID, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[RuntimeBroker](resp)
}

// Update updates broker metadata.
func (s *runtimeBrokerService) Update(ctx context.Context, brokerID string, req *UpdateBrokerRequest) (*RuntimeBroker, error) {
	resp, err := s.c.patch(ctx, "/api/v1/runtime-brokers/"+brokerID, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[RuntimeBroker](resp)
}

// Delete removes a broker from all projects.
func (s *runtimeBrokerService) Delete(ctx context.Context, brokerID string) error {
	resp, err := s.c.delete(ctx, "/api/v1/runtime-brokers/"+brokerID, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// ListProjects returns projects this broker contributes to.
func (s *runtimeBrokerService) ListProjects(ctx context.Context, brokerID string) (*ListBrokerProjectsResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/runtime-brokers/"+brokerID+"/projects", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListBrokerProjectsResponse](resp)
}

// Heartbeat sends a heartbeat for a broker.
func (s *runtimeBrokerService) Heartbeat(ctx context.Context, brokerID string, status *BrokerHeartbeat) (*BrokerHeartbeatResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/runtime-brokers/"+brokerID+"/heartbeat", status, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, apiclient.ParseErrorResponse(resp)
	}
	// An older Hub replies 200 with an empty body. A body that does not
	// decode is treated the same way: the heartbeat itself was accepted.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var out BrokerHeartbeatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil
	}
	return &out, nil
}

// ReportMessageFailures reports buffered deliveries that failed on the broker.
func (s *runtimeBrokerService) ReportMessageFailures(ctx context.Context, brokerID string, req *MessageFailuresReport) error {
	resp, err := s.c.post(ctx, "/api/v1/runtime-brokers/"+url.PathEscape(brokerID)+"/message-failures", req, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// ReportAgentLaunch posts one launch report. See the RuntimeBrokerService
// doc comment for the error-vs-result split.
func (s *runtimeBrokerService) ReportAgentLaunch(ctx context.Context, brokerID, agentID string, req *AgentLaunchReport) (*AgentLaunchReportResult, error) {
	path := "/api/v1/runtime-brokers/" + url.PathEscape(brokerID) + "/agents/" + url.PathEscape(agentID) + "/launch"
	resp, err := s.c.post(ctx, path, req, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			Result string `json:"result"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, fmt.Errorf("decode launch report response: %w", err)
		}
		return &AgentLaunchReportResult{Result: body.Result}, nil

	case http.StatusForbidden:
		return &AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, nil

	case http.StatusNotFound, http.StatusConflict:
		var body struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode == http.StatusNotFound && body.Code != AgentLaunchReportCodeUnknownLaunch {
			// design §5 N-9: a plain 404 from a Hub node without this route
			// (e.g. a mixed-version HA cluster) means "no endpoint", which the
			// broker must treat as retryable, never as a definitive unknown
			// launch.
			return nil, &apiclient.APIError{StatusCode: resp.StatusCode, Code: "no_endpoint", Message: "launch report route not found"}
		}
		if resp.StatusCode == http.StatusConflict && body.Code != AgentLaunchReportCodeStaleLaunch {
			// A 409 the wire contract does not define (its code is not
			// stale_launch) is not something the sender can classify by
			// Reason; treat it as retryable rather than guessing.
			return nil, &apiclient.APIError{StatusCode: resp.StatusCode, Code: "unrecognized_conflict", Message: "409 response had an unrecognized code"}
		}
		return &AgentLaunchReportResult{HTTPStatus: resp.StatusCode, Code: body.Code, Reason: body.Reason}, nil

	case http.StatusBadRequest, http.StatusUnauthorized:
		// These are definitive protocol/auth failures, never transient like
		// an unreachable Hub or a 5xx -- retrying them would not help, so the
		// sender must not loop on them like it does for
		// errLaunchReportUnreachable. Reported as a result (nil error), not
		// an error, so the caller's gate classification sees a definitive
		// answer rather than treating it as retryable.
		return &AgentLaunchReportResult{HTTPStatus: resp.StatusCode}, nil

	default:
		return nil, apiclient.ParseErrorResponse(resp)
	}
}
