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
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Move eligibility checks for `scion reincarnate --broker` (moving an agent
// to another runtime broker on the same NFS export), in evaluation order.
// The order is part of the contract: the first failing check decides the
// response, and later checks may rely on earlier ones having passed (the
// export comparison needs both descriptors).
const (
	moveCheckRuntimeTarget     = "runtime_target"
	moveCheckWorkspaceMode     = "workspace_mode"
	moveCheckWorkspaceStorage  = "workspace_storage_reported"
	moveCheckSameExport        = "same_export"
	moveCheckWorkspaceOnExport = "workspace_on_export"
	moveCheckTargetProfile     = "target_profile"
	moveCheckTargetHealth      = "target_health"
	moveCheckAccess            = "access"
	moveCheckCapability        = "capability"
	moveCheckCapacity          = "capacity"
)

// moveCheckOrder lists every move eligibility check in evaluation order.
var moveCheckOrder = []string{
	moveCheckRuntimeTarget,
	moveCheckWorkspaceMode,
	moveCheckWorkspaceStorage,
	moveCheckSameExport,
	moveCheckWorkspaceOnExport,
	moveCheckTargetProfile,
	moveCheckTargetHealth,
	moveCheckAccess,
	moveCheckCapability,
	moveCheckCapacity,
}

// Move check results.
const (
	MoveCheckPassed       = "passed"
	MoveCheckFailed       = "failed"
	MoveCheckNotEvaluated = "not_evaluated"
)

// SurfacePassthroughMove labels the passthrough re-check run against the
// target broker of a cross-broker move.
const SurfacePassthroughMove = "passthrough-move"

// MoveCheck is the result of one move eligibility check.
type MoveCheck struct {
	Name    string `json:"name"`
	Result  string `json:"result"` // passed | failed | not_evaluated
	Message string `json:"message,omitempty"`
}

// MoveBrokerRef identifies a broker in a move verdict.
type MoveBrokerRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// MoveVerdict is the eligibility verdict for moving an agent to another
// broker. Checks always lists every check in evaluation order; checks after
// the first failure are not_evaluated.
type MoveVerdict struct {
	Eligible     bool          `json:"eligible"`
	SourceBroker MoveBrokerRef `json:"sourceBroker"`
	TargetBroker MoveBrokerRef `json:"targetBroker"`
	// Profile and RuntimeType are the target profile the agent would run
	// under, once the target_profile check has resolved them.
	Profile     string      `json:"profile,omitempty"`
	RuntimeType string      `json:"runtimeType,omitempty"`
	Checks      []MoveCheck `json:"checks"`
}

// moveRefusal is the HTTP response for the first failing move check.
type moveRefusal struct {
	Status  int
	Code    string
	Message string
}

// moveProbes are the eligibility inputs that need the server (store,
// authorization, control channel). Each is called at most once, and only
// when its check is reached. None may write state.
type moveProbes struct {
	// Reachable reports whether a broker looks reachable now.
	Reachable func(b *store.RuntimeBroker) bool
	// CanDispatch reports whether the caller may dispatch agents to dst.
	CanDispatch func(dst *store.RuntimeBroker) bool
	// CanUseAsProvider reports whether dst already serves the agent's
	// project, or the caller may link it as a provider (project update).
	CanUseAsProvider func(dst *store.RuntimeBroker) bool
	// ServesProject reports whether dst already serves the agent's project.
	ServesProject func(dst *store.RuntimeBroker) bool
	// Passthrough re-runs the GCP passthrough gate against dst. Nil when
	// the agent does not use passthrough. A non-empty message is a denial.
	Passthrough func(dst *store.RuntimeBroker) string
	// Capacity reports a non-empty message when dst has no room for
	// another agent under its agent limit.
	Capacity func(dst *store.RuntimeBroker) string
}

// moveEligibilityInput is everything evaluateMoveEligibility decides on.
type moveEligibilityInput struct {
	Agent *store.Agent
	Src   *store.RuntimeBroker
	Dst   *store.RuntimeBroker
	// WorkspaceModeError is the refusal message of the reincarnate
	// workspace gate, or "" when the agent's workspace mode is eligible.
	WorkspaceModeError string
	// CloneMode is true for a clone-per-agent workspace and false for a
	// shared-workspace or hub-managed one.
	CloneMode bool
	// Profile is the profile the agent asks for (explicit, else the
	// project's active profile); "" falls back to the target's default.
	Profile string
	// SelfMove is true when the caller is the agent being moved.
	SelfMove bool
	Probes   moveProbes
}

// evaluateMoveEligibility runs the move eligibility checks in order and
// stops at the first failure, which it returns as a refusal. It writes
// nothing; the real move runs the same function before any side effect.
func evaluateMoveEligibility(in moveEligibilityInput) (MoveVerdict, *moveRefusal) {
	v := MoveVerdict{
		SourceBroker: MoveBrokerRef{ID: in.Src.ID, Name: in.Src.Name},
		TargetBroker: MoveBrokerRef{ID: in.Dst.ID, Name: in.Dst.Name},
		Checks:       make([]MoveCheck, 0, len(moveCheckOrder)),
	}
	for _, name := range moveCheckOrder {
		v.Checks = append(v.Checks, MoveCheck{Name: name, Result: MoveCheckNotEvaluated})
	}
	idx := 0
	pass := func() { v.Checks[idx].Result = MoveCheckPassed; idx++ }
	fail := func(status int, code, msg string) (MoveVerdict, *moveRefusal) {
		v.Checks[idx].Result = MoveCheckFailed
		v.Checks[idx].Message = msg
		return v, &moveRefusal{Status: status, Code: code, Message: msg}
	}
	src, dst := in.Src, in.Dst
	srcName, dstName := brokerDisplayName(src), brokerDisplayName(dst)

	// 0. Runtime target: a pinned agent, or a move from or onto a flat
	// Runtime Broker, is not movable (flat Runtime Brokers serve a single
	// runtime target; a permitted move would re-pin explicitly).
	if in.Agent.IsPinned() || src.IsFlat() || dst.IsFlat() {
		return fail(http.StatusConflict, ErrCodeRuntimeTargetMoveUnsupported,
			"agents on flat Runtime Brokers, and moves onto a flat Runtime Broker, are not movable")
	}
	pass()

	// 1. Workspace mode: the reincarnate workspace gate, plus no linked
	// project (decided by the caller).
	if in.WorkspaceModeError != "" {
		return fail(http.StatusBadRequest, ErrCodeValidationError, in.WorkspaceModeError)
	}
	pass()

	// 2. Both brokers report a workspace storage descriptor.
	for _, b := range []*store.RuntimeBroker{src, dst} {
		if b.WorkspaceStorage == nil {
			return fail(http.StatusPreconditionFailed, ErrCodeUnsupportedCapability,
				fmt.Sprintf("broker %s does not advertise workspace storage; upgrade it", brokerDisplayName(b)))
		}
	}
	pass()

	// 3. Both brokers mount the same NFS export.
	// The configured export alone is not proof (the same address can be
	// two servers, and a PV decides what is mounted): both brokers must also
	// report the same export identity marker, read through their own mounts.
	if !api.SameWorkspaceExport(src.WorkspaceStorage, dst.WorkspaceStorage) {
		return fail(http.StatusConflict, ErrCodeConflict, sameExportMessage(src, dst))
	}
	for _, b := range []*store.RuntimeBroker{src, dst} {
		if b.WorkspaceStorage.NFS.ExportID == "" {
			return fail(http.StatusPreconditionFailed, ErrCodeUnsupportedCapability, fmt.Sprintf(
				"broker %s has not reported the export identity marker (%s/%s on its NFS export); upgrade it, or check that its export mount is healthy and writable",
				brokerDisplayName(b), b.WorkspaceStorage.NFS.SubPathRoot, api.ExportIDMarkerName))
		}
	}
	if srcID, dstID := src.WorkspaceStorage.NFS.ExportID, dst.WorkspaceStorage.NFS.ExportID; srcID != dstID {
		return fail(http.StatusConflict, ErrCodeConflict, fmt.Sprintf(
			"brokers %s and %s configure the same NFS export but see different export identity markers (%s and %s), so they do not mount the same directory. --broker requires both brokers to mount the same NFS export",
			srcName, dstName, srcID, dstID))
	}
	pass()

	// 4. The workspace is on the export on the source and would be on it
	// on the target. Clone-per-agent agent directories are on NFS only on
	// Kubernetes; shared and hub-managed workspaces are on NFS on any
	// runtime once the backend is nfs (checked above).
	ac := in.Agent.AppliedConfig
	if ac != nil && ac.WorkspaceStoragePath != "" {
		return fail(http.StatusConflict, ErrCodeConflict,
			"agent workspace is a GCS-synced copy, not on the NFS export; --broker requires the workspace on a shared NFS export")
	}
	if msg := workspacePlacementRefusal(in.Agent.WorkspacePlacement, srcName); msg != "" {
		return fail(http.StatusConflict, ErrCodeConflict, msg)
	}
	if in.CloneMode && !isKubernetesRuntimeType(in.Agent.Runtime) {
		runtime := in.Agent.Runtime
		if runtime == "" {
			runtime = "unknown"
		}
		return fail(http.StatusConflict, ErrCodeConflict, fmt.Sprintf(
			"clone-per-agent workspaces are on the NFS export only on Kubernetes; the agent runs on runtime %q on broker %s",
			runtime, srcName))
	}
	v.Checks[idx].Message = "the source broker recorded the workspace on the shared NFS export; the target confirms the workspace directory through its own mount at move time"
	pass()

	// 5. The target has the profile the agent would run under, available,
	// with a runtime type that places the workspace on the export.
	profileName, profileType, ok := resolveAgentRuntimeProfileType(dst, in.Profile)
	if !ok {
		msg := fmt.Sprintf("broker %s has no profile %q", dstName, in.Profile)
		if in.Profile == "" {
			msg = fmt.Sprintf("broker %s has no default profile; set a profile on the agent or project", dstName)
		}
		return fail(http.StatusConflict, ErrCodeConflict, msg)
	}
	v.Profile, v.RuntimeType = profileName, profileType
	if !brokerProfileAvailable(dst, profileName) {
		return fail(http.StatusConflict, ErrCodeConflict,
			fmt.Sprintf("profile %q is not available on broker %s", profileName, dstName))
	}
	if in.CloneMode && !isKubernetesRuntimeType(profileType) {
		return fail(http.StatusConflict, ErrCodeConflict, fmt.Sprintf(
			"profile %q on broker %s has runtime type %q; clone-per-agent workspaces are on the NFS export only on Kubernetes",
			profileName, dstName, profileType))
	}
	pass()

	// 6. The target's export mount is healthy and the target is reachable.
	if !dst.WorkspaceStorage.NFS.Healthy {
		return fail(http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail,
			fmt.Sprintf("broker %s reports its NFS export mount unhealthy", dstName))
	}
	if !in.Probes.Reachable(dst) {
		return fail(http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail,
			fmt.Sprintf("broker %s is unavailable", dstName))
	}
	pass()

	// 7. An agent moving itself may only move to a broker that already
	// serves its project (a self-move never links a provider), and never
	// with a passthrough identity. Any other caller may dispatch to the
	// target (and link it to the project if it is not a provider yet), and
	// passes the passthrough gate there.
	if in.SelfMove {
		// Probes.Passthrough is set exactly when the agent uses passthrough.
		if in.Probes.Passthrough != nil {
			return fail(http.StatusForbidden, ErrCodeForbidden,
				"this agent cannot move itself; ask a user to move you")
		}
		if !in.Probes.ServesProject(dst) {
			return fail(http.StatusConflict, ErrCodeConflict, fmt.Sprintf(
				"target broker %s does not serve this project; an agent can only move itself to a broker that already serves its project", dstName))
		}
	} else {
		if !in.Probes.CanDispatch(dst) {
			return fail(http.StatusForbidden, ErrCodeForbidden,
				fmt.Sprintf("you don't have permission to run agents on broker %s", dstName))
		}
		if !in.Probes.CanUseAsProvider(dst) {
			return fail(http.StatusForbidden, ErrCodeForbidden, fmt.Sprintf(
				"broker %s is not a provider for this project and you don't have permission to add it", dstName))
		}
		if in.Probes.Passthrough != nil {
			if msg := in.Probes.Passthrough(dst); msg != "" {
				return fail(http.StatusForbidden, ErrCodeForbidden,
					fmt.Sprintf("GCP passthrough identity is not allowed on broker %s: %s", dstName, msg))
			}
		}
	}
	pass()

	// 8. Both brokers support agent move, and the source is online so it
	// can clean up its local state.
	for _, b := range []*store.RuntimeBroker{dst, src} {
		if b.Capabilities == nil || !b.Capabilities.AgentMove {
			return fail(http.StatusPreconditionFailed, ErrCodeUnsupportedCapability,
				fmt.Sprintf("broker %s does not support agent move; upgrade it", brokerDisplayName(b)))
		}
	}
	if !in.Probes.Reachable(src) {
		return fail(http.StatusPreconditionFailed, ErrCodeRuntimeBrokerUnavail,
			fmt.Sprintf("source broker %s is unavailable; a move needs it online to clean up", srcName))
	}
	pass()

	// 9. The target has room under its agent limit (read-only).
	if msg := in.Probes.Capacity(dst); msg != "" {
		return fail(http.StatusTooManyRequests, ErrCodeQuotaExceeded, msg)
	}
	pass()

	v.Eligible = true
	return v, nil
}

// reprovisionPlacementHint tells the operator how to record a workspace
// placement the hub does not know.
const reprovisionPlacementHint = "re-provision the agent once (scion reincarnate without --broker) to record its workspace placement"

// isWorkspacePlacementOnExport reports whether a recorded workspace
// placement puts the workspace on the shared NFS export. Unknown ("") and
// unrecognised values are not on the export.
func isWorkspacePlacementOnExport(placement string) bool {
	return placement == api.WorkspacePlacementExport
}

// workspacePlacementRefusal returns why an agent with the recorded workspace
// placement cannot move, or "" when its workspace is on the export.
func workspacePlacementRefusal(placement, srcName string) string {
	switch {
	case isWorkspacePlacementOnExport(placement):
		return ""
	case placement == "":
		return "the agent's workspace placement is unknown (it has not started since placements were recorded); " + reprovisionPlacementHint
	case placement == api.WorkspacePlacementLocal:
		return fmt.Sprintf("the agent's workspace is not on the shared NFS export on broker %s (placement %q); if the broker has since moved workspaces to the export, %s",
			srcName, placement, reprovisionPlacementHint)
	default:
		return fmt.Sprintf("the agent's workspace placement %q is not recognised as the shared NFS export; %s", placement, reprovisionPlacementHint)
	}
}

func brokerDisplayName(b *store.RuntimeBroker) string {
	if b.Name != "" {
		return b.Name
	}
	return b.ID
}

func brokerProfileAvailable(b *store.RuntimeBroker, name string) bool {
	for _, p := range b.Profiles {
		if p.Name == name {
			return p.Available
		}
	}
	return false
}

// sameExportMessage names the source export and what the target mounts.
func sameExportMessage(src, dst *store.RuntimeBroker) string {
	where := "is not on an NFS export"
	if s := src.WorkspaceStorage; s.Backend == api.WorkspaceStorageBackendNFS && s.NFS != nil {
		where = fmt.Sprintf("is on NFS export %s:%s (subpath root %q)", s.NFS.Server, s.NFS.Export, s.NFS.SubPathRoot)
	}
	mounts := "backend " + dst.WorkspaceStorage.Backend
	if d := dst.WorkspaceStorage; d.Backend == api.WorkspaceStorageBackendNFS && d.NFS != nil {
		mounts = fmt.Sprintf("NFS export %s:%s (subpath root %q)", d.NFS.Server, d.NFS.Export, d.NFS.SubPathRoot)
	}
	return fmt.Sprintf("agent workspace %s on broker %s; broker %s mounts %s. --broker requires both brokers to mount the same NFS export",
		where, brokerDisplayName(src), brokerDisplayName(dst), mounts)
}

// resolveMoveTargetBroker resolves a --broker value to a runtime broker
// among the brokers the caller may see (moveTargetVisible; the agent's
// current broker, currentBrokerID, is always visible). Message-broker plugin
// records (label scion.io/plugin) are not runtime brokers and never match.
// It writes nothing and links nothing.
//
// A visible broker whose ID equals target wins. Otherwise target is matched
// against visible brokers' names (case-insensitive) and slugs (exact):
// exactly one match is the target; none returns a nil broker; more than one
// returns the candidates, sorted by ID. Brokers the caller cannot see are
// filtered out before matching, so they neither shadow a visible broker nor
// make a request ambiguous.
func (s *Server) resolveMoveTargetBroker(ctx context.Context, target, currentBrokerID, projectID string) (broker *store.RuntimeBroker, ambiguous []store.RuntimeBroker, err error) {
	candidate := func(b *store.RuntimeBroker) bool {
		return !isPluginBroker(b) && (b.ID == currentBrokerID || s.moveTargetVisible(ctx, b, projectID))
	}
	b, err := s.store.GetRuntimeBroker(ctx, target)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err
	}
	if err == nil && candidate(b) {
		return b, nil, nil
	}
	var matches []store.RuntimeBroker
	cursor := ""
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{},
			store.ListOptions{Limit: 200, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return nil, nil, err
		}
		for i := range page.Items {
			b := &page.Items[i]
			if (strings.EqualFold(b.Name, target) || b.Slug == target) && candidate(b) {
				matches = append(matches, *b)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	switch len(matches) {
	case 0:
		return nil, nil, nil
	case 1:
		return &matches[0], nil, nil
	default:
		sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
		return nil, matches, nil
	}
}

// writeMoveTargetAmbiguous writes the 409 for a --broker name or slug that
// matches more than one visible broker, listing those brokers' IDs.
func writeMoveTargetAmbiguous(w http.ResponseWriter, target string, candidates []store.RuntimeBroker) {
	summaries := make([]RuntimeBrokerSummary, 0, len(candidates))
	for _, b := range candidates {
		summaries = append(summaries, RuntimeBrokerSummary{ID: b.ID, Name: b.Name, Status: b.Status})
	}
	writeError(w, http.StatusConflict, ErrCodeRuntimeBrokerAmbiguous,
		fmt.Sprintf("%q matches %d runtime brokers; use the broker ID", target, len(candidates)),
		map[string]interface{}{"requestedBroker": target, "candidates": summaries})
}

// isPluginBroker reports whether a broker record is a message-broker plugin
// rather than a runtime broker.
func isPluginBroker(b *store.RuntimeBroker) bool {
	_, ok := b.Labels["scion.io/plugin"]
	return ok
}

// moveTargetVisible reports whether the caller may learn that dst exists and
// see its configuration in a move verdict. A broker the caller cannot see
// is answered exactly like an unknown one, so --broker is not an existence
// oracle and the verdict does not leak another broker's export, profiles or
// health. Visible means: dst auto-provides; or dst already serves the
// agent's project (projectID), as listed in the not-found response; or the
// caller is a user who may read dst; or the caller is an agent that may
// dispatch to dst. Dispatch and provider-link rights are still decided
// separately by the access check.
func (s *Server) moveTargetVisible(ctx context.Context, dst *store.RuntimeBroker, projectID string) bool {
	if dst.AutoProvide {
		return true
	}
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return false
	}
	switch identity.Type() {
	case "user", "dev":
		user, ok := identity.(UserIdentity)
		return ok && (s.brokerServesProject(ctx, dst.ID, projectID) ||
			s.authzService.CheckAccess(ctx, user, brokerResource(dst), ActionRead).Allowed)
	case "agent":
		return s.brokerServesProject(ctx, dst.ID, projectID) || s.canDispatchToBroker(ctx, dst)
	default:
		return false
	}
}

// writeMoveTargetNotFound writes the 404 for an unknown or not visible
// --broker value, listing the project's online brokers. The body depends
// only on the request and the project, never on the broker matched.
func (s *Server) writeMoveTargetNotFound(ctx context.Context, w http.ResponseWriter, target string, project *store.Project) {
	summaries := []RuntimeBrokerSummary{}
	if brokers, err := s.getAvailableBrokersForProject(ctx, project.ID); err == nil {
		for _, b := range brokers {
			summaries = append(summaries, RuntimeBrokerSummary{
				ID: b.ID, Name: b.Name, Status: b.Status, IsDefault: b.ID == project.DefaultRuntimeBrokerID,
			})
		}
	}
	writeError(w, http.StatusNotFound, ErrCodeRuntimeBrokerNotFound,
		fmt.Sprintf("runtime broker %q not found", target),
		map[string]interface{}{"requestedBroker": target, "availableBrokers": summaries})
}

// moveProbesFor builds the server-backed move eligibility probes for a
// request. They only read state; the passthrough probe records its
// authorization decision in the audit log like every passthrough gate.
func (s *Server) moveProbesFor(r *http.Request, project *store.Project, ac *store.AgentAppliedConfig) moveProbes {
	ctx := r.Context()
	p := moveProbes{
		// The loaded record decides; unlike brokerReachable this never fails
		// open, because a move refused here leaves the agent untouched.
		Reachable: s.brokerRecordReachable,
		CanDispatch: func(dst *store.RuntimeBroker) bool {
			return s.canDispatchToBroker(ctx, dst)
		},
		ServesProject: func(dst *store.RuntimeBroker) bool {
			return s.brokerServesProject(ctx, dst.ID, project.ID)
		},
		CanUseAsProvider: func(dst *store.RuntimeBroker) bool {
			if s.brokerServesProject(ctx, dst.ID, project.ID) {
				return true
			}
			identity := GetIdentityFromContext(ctx)
			if identity == nil {
				return false
			}
			return s.authzService.CheckAccess(ctx, identity, projectResource(project), ActionUpdate).Allowed
		},
		// Capacity is advisory: it reads the current count only, and an
		// unreadable limit or count reads as "room" (fails open). The
		// real move enforces the limit when it reserves the slot.
		Capacity: func(dst *store.RuntimeBroker) string {
			limitDef := s.lookupAgentLimitDefinition(ctx)
			if limitDef == nil {
				return ""
			}
			c := s.brokerCapacity(ctx, dst.ID, limitDef)
			if c.Source == BrokerLimitSourceNotEnforced || c.Limit == nil || c.Count == nil || *c.Count < *c.Limit {
				return ""
			}
			return fmt.Sprintf("broker %s is at its agent limit (%d of %d)", brokerDisplayName(dst), *c.Count, *c.Limit)
		},
	}
	if ac != nil && ac.GCPIdentity != nil && ac.GCPIdentity.MetadataMode == store.GCPMetadataModePassthrough {
		p.Passthrough = func(dst *store.RuntimeBroker) string {
			rec := httptest.NewRecorder()
			if s.authorizePassthroughIdentity(rec, r, dst, SurfacePassthroughMove) {
				return ""
			}
			return recordedErrorMessage(rec)
		}
	}
	return p
}

// recordedErrorMessage extracts the error message from a recorded error
// response, falling back to the status text.
func recordedErrorMessage(rec *httptest.ResponseRecorder) string {
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err == nil && resp.Error.Message != "" {
		return resp.Error.Message
	}
	return strings.ToLower(http.StatusText(rec.Code))
}

// writeMoveRefusal writes a move refusal with the verdict in
// error.details.verdict.
func writeMoveRefusal(w http.ResponseWriter, ref *moveRefusal, v MoveVerdict) {
	writeError(w, ref.Status, ref.Code, ref.Message, map[string]interface{}{"verdict": v})
}
