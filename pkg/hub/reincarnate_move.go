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
	"net/http"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
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
	// moveCheckWorkspaceRecreatable decides, for a forced move, whether the
	// workspace can be provisioned fresh on the target. In an export move it
	// passes without a message: the workspace is carried, not recreated.
	moveCheckWorkspaceRecreatable = "workspace_recreatable"
	// moveCheckWorkspaceBundleable decides, for a bundle move, whether the
	// workspace sharing mode is eligible (clone-per-agent or empty-per-agent).
	moveCheckWorkspaceBundleable = "workspace_bundleable"
	// moveCheckBundleExperiment checks that hub.agent_move_bundle is enabled.
	moveCheckBundleExperiment = "bundle_experiment"
	// moveCheckBundleStorage checks that Hub object storage is configured and
	// issues HTTP(S) signed URLs usable by remote brokers (GCS).
	moveCheckBundleStorage = "bundle_storage"
	// moveCheckBundleSource probes the source broker for post-stop durability
	// and stat-only workspace/$HOME estimates within Hub limits.
	moveCheckBundleSource  = "bundle_source"
	moveCheckTargetProfile = "target_profile"
	moveCheckTargetHealth  = "target_health"
	moveCheckAccess        = "access"
	moveCheckCapability    = "capability"
	moveCheckCapacity      = "capacity"
)

// moveCheckOrder lists every move eligibility check in evaluation order for
// export and recreate modes.
var moveCheckOrder = []string{
	moveCheckRuntimeTarget,
	moveCheckWorkspaceMode,
	moveCheckWorkspaceStorage,
	moveCheckSameExport,
	moveCheckWorkspaceOnExport,
	moveCheckWorkspaceRecreatable,
	moveCheckTargetProfile,
	moveCheckTargetHealth,
	moveCheckAccess,
	moveCheckCapability,
	moveCheckCapacity,
}

// moveBundleCheckOrder lists every move eligibility check in evaluation order
// for bundle mode. bundle_experiment runs first among bundle-specific checks
// so an off-experiment hub reports that cleanly before workspace_bundleable;
// bundle_source runs after access and capability so no RPC is sent to an
// unreachable/incapable broker or for an unauthorized caller.
var moveBundleCheckOrder = []string{
	moveCheckRuntimeTarget,
	moveCheckWorkspaceMode,
	moveCheckWorkspaceStorage,
	moveCheckSameExport,
	moveCheckWorkspaceOnExport,
	moveCheckBundleExperiment,
	moveCheckWorkspaceBundleable,
	moveCheckBundleStorage,
	moveCheckTargetProfile,
	moveCheckTargetHealth,
	moveCheckAccess,
	moveCheckCapability,
	moveCheckBundleSource,
	moveCheckCapacity,
}

// moveEvalMode selects which transfer tier evaluateMoveChecks evaluates.
type moveEvalMode int

const (
	moveEvalExport moveEvalMode = iota
	moveEvalBundle
	moveEvalRecreate
)

// Move check results.
const (
	MoveCheckPassed       = "passed"
	MoveCheckFailed       = "failed"
	MoveCheckNotEvaluated = "not_evaluated"
	// MoveCheckWaived marks an export check a forced move does not need,
	// because the workspace is recreated on the target instead of carried.
	MoveCheckWaived = "waived"
)

// Workspace transfer modes of an eligible move (MoveVerdict.TransferMode).
const (
	// MoveTransferModeExport carries the workspace: the target finds the
	// same directory through a shared NFS export.
	MoveTransferModeExport = store.TransferModeExport
	// MoveTransferModeBundle copies workspace and $HOME through Hub object
	// storage after the source broker stops the agent.
	MoveTransferModeBundle = store.TransferModeBundle
	// MoveTransferModeRecreate provisions the workspace fresh on the target
	// (a forced move); the source's local state is given up.
	MoveTransferModeRecreate = store.TransferModeRecreate
)

// Move consequence kinds (MoveConsequence.Kind), in display order.
const (
	MoveConsequenceKept      = "kept"
	MoveConsequenceCarried   = "carried"
	MoveConsequenceRecreated = "recreated"
	MoveConsequenceLost      = "lost"
)

// moveForceWaivedMessage is the message of an export check waived by a
// forced move.
const moveForceWaivedMessage = "not needed: the workspace is recreated on the target (--force)"

// moveBundleWaivedMessage is the message of an export check waived by a
// bundle move.
const moveBundleWaivedMessage = "not needed: workspace and home are copied through hub storage"

// SurfacePassthroughMove labels the passthrough re-check run against the
// target broker of a cross-broker move.
const SurfacePassthroughMove = "passthrough-move"

// MoveCheck is the result of one move eligibility check.
type MoveCheck struct {
	Name    string `json:"name"`
	Result  string `json:"result"` // passed | failed | not_evaluated | waived
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
	// TransferMode is how the workspace reaches the target: "export" (the
	// same directory through a shared NFS export), "bundle" (workspace and
	// $HOME copied through Hub object storage), or "recreate" (provisioned
	// fresh; local state is lost). Empty when not eligible. A hub that
	// predates --force never sets it.
	TransferMode string `json:"transferMode,omitempty"`
	// Consequences lists what the move keeps, carries, recreates and loses,
	// one entry per item, for the CLI to print. Set for every eligible move
	// in any mode.
	Consequences []MoveConsequence `json:"consequences,omitempty"`
	// BundleUnavailable explains why transfer mode "bundle" did not apply when
	// export failed on a waivable/placement check and bundle could not be used.
	BundleUnavailable string `json:"bundleUnavailable,omitempty"`
	// BundleEstimate carries the source broker's stat-only workspace and $HOME
	// size/entry estimate when transfer mode "bundle" is selected.
	BundleEstimate *MoveBundleEstimate `json:"bundleEstimate,omitempty"`
}

// MoveBundleEstimate is the stat-only size/entry estimate reported by the
// source broker during move-bundle precheck.
type MoveBundleEstimate struct {
	WorkspaceBytes   int64 `json:"workspaceBytes"`
	WorkspaceEntries int64 `json:"workspaceEntries"`
	HomeBytes        int64 `json:"homeBytes"`
	HomeEntries      int64 `json:"homeEntries"`
}

// MoveConsequence is one item a move keeps, carries, recreates or loses.
type MoveConsequence struct {
	Kind string `json:"kind"` // kept | carried | recreated | lost
	Item string `json:"item"` // stable key, e.g. "identity", "workspace", "home"
	Text string `json:"text"` // one human sentence
}

// moveRefusal is the HTTP response for the first failing move check.
type moveRefusal struct {
	Status  int
	Code    string
	Message string
	// Details holds additional error details merged into the response alongside
	// the move verdict (e.g., broker policy setting/operation/brokerId).
	Details map[string]interface{}
	// ForceWouldApply is set on a refusal without --force that only the
	// workspace placement caused (same_export, workspace_on_export, or the
	// placementCaused target_profile refusal), for an agent whose
	// workspace mode a forced move supports, onto a target that advertises
	// AgentMoveRecreate. It is written to the error details as
	// forceWouldApply.
	ForceWouldApply bool
	// placementCaused marks the export checks' target_profile refusal that
	// exists only to keep the workspace on the export: the Kubernetes
	// constraint for agent-directory workspaces. A forced move re-evaluates
	// it with the recreate rules. Internal; never on the wire.
	placementCaused bool
}

// moveProbes are the eligibility inputs that need the server (store,
// authorization, control channel). Each is called at most once, and only
// when its check is reached. None may write state.
type moveProbes struct {
	// Reachable reports whether a broker looks reachable now.
	Reachable func(b *store.RuntimeBroker) bool
	// PolicyRefused reports whether broker_policy.admin_managed_only refuses
	// moving an agent onto dst. Nil means no policy refusal.
	PolicyRefused func(dst *store.RuntimeBroker) bool
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
	// HostSARecord reports a non-empty message when the agent's assigned
	// GCP service account is a broker host service account record bound to
	// a broker other than dst. Nil when the agent has no assign-mode
	// identity.
	HostSARecord func(dst *store.RuntimeBroker) string
	// BundleSource probes the source broker for post-stop durability and
	// stat-only workspace/$HOME estimates against the Hub's move-bundle limits.
	BundleSource func(src *store.RuntimeBroker, agent *store.Agent) (*api.MoveBundlePrecheckResponse, error)
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

	// Force asks for a forced move: when the workspace cannot travel through
	// a shared export or bundle, it is recreated on the target. It never
	// changes a move the export or bundle checks allow.
	Force bool
	// EmptyPerAgent is true for an empty-per-agent workspace (CloneMode is
	// also true for it).
	EmptyPerAgent bool
	// WorkspaceMode is the project's workspace sharing mode, named in the
	// refusal of a bundle or forced move it does not support.
	WorkspaceMode string
	// GitRemote is the project's git remote, the source of a recreated
	// clone-per-agent workspace.
	GitRemote string
	// Branch is the agent's branch and DefaultBranch the branch a fresh
	// clone starts from when Branch was not pushed. Both only describe a
	// recreated workspace.
	Branch        string
	DefaultBranch string
	// BundleExperimentEnabled reports whether the hub.agent_move_bundle
	// experiment is enabled on the Hub.
	BundleExperimentEnabled bool
	// BundleStorageError is "" when the Hub has HTTP-signable object storage
	// configured (GCS), or the failure message for the bundle_storage check.
	BundleStorageError string
}

// evaluateMoveEligibility runs the move eligibility checks in order and
// stops at the first failure, which it returns as a refusal. It writes
// nothing; the real move runs the same function before any side effect.
//
// The export checks run first, whether or not the move is forced, so a move
// they allow always carries the workspace (transfer mode export). Only when
// they fail on workspace_storage_reported, same_export or
// workspace_on_export, or on the target_profile Kubernetes constraint that
// only keeps the workspace on the export (placementCaused), and the move is
// forced, are the checks run again for a recreated workspace: those three
// are waived, workspace_recreatable decides, target_profile and
// target_health apply their recreate rules, and every other check still
// applies. A target_health failure of the export checks is never waived:
// for any workspace a forced move supports, a recreated workspace would
// need the same mount.
func evaluateMoveEligibility(in moveEligibilityInput) (MoveVerdict, *moveRefusal) {
	v, ref := evaluateMoveChecks(in, moveEvalExport)
	if ref == nil {
		v.TransferMode = MoveTransferModeExport
		v.Consequences = buildMoveConsequences(MoveTransferModeExport, in, nil)
		return v, nil
	}
	failed := firstFailedMoveCheck(v)
	if !moveCheckWaivableByForce(failed) && !ref.placementCaused {
		return v, ref
	}
	vb, refb := evaluateMoveChecks(in, moveEvalBundle)
	if refb == nil {
		vb.TransferMode = MoveTransferModeBundle
		vb.Consequences = buildMoveConsequences(MoveTransferModeBundle, in, vb.BundleEstimate)
		return vb, nil
	}
	if !in.Force {
		_, notRecreatable := moveWorkspaceRecreatable(in)
		// The hint points only at a --force the target can accept.
		dstRecreates := in.Dst.Capabilities != nil && in.Dst.Capabilities.AgentMoveRecreate
		ref.ForceWouldApply = (failed == moveCheckSameExport || failed == moveCheckWorkspaceOnExport || ref.placementCaused) &&
			notRecreatable == "" && dstRecreates
		v.BundleUnavailable = refb.Message
		return v, ref
	}
	vr, refr := evaluateMoveChecks(in, moveEvalRecreate)
	if refr != nil {
		vr.BundleUnavailable = refb.Message
		return vr, refr
	}
	vr.TransferMode = MoveTransferModeRecreate
	vr.BundleUnavailable = refb.Message
	vr.Consequences = buildMoveConsequences(MoveTransferModeRecreate, in, nil)
	return vr, nil
}

// moveCheckWaivableByForce reports whether a forced move may waive a failure
// of the named check: the checks that exist only to keep the workspace on a
// shared export.
func moveCheckWaivableByForce(name string) bool {
	switch name {
	case moveCheckWorkspaceStorage, moveCheckSameExport, moveCheckWorkspaceOnExport:
		return true
	}
	return false
}

// firstFailedMoveCheck returns the name of the failed check in v, or "".
func firstFailedMoveCheck(v MoveVerdict) string {
	for _, c := range v.Checks {
		if c.Result == MoveCheckFailed {
			return c.Name
		}
	}
	return ""
}

// moveWorkspaceRecreatable decides whether a forced move can provision the
// agent's workspace fresh on the target. It returns the check's message
// when it can, or the refusal message when it cannot.
func moveWorkspaceRecreatable(in moveEligibilityInput) (passMsg, failMsg string) {
	switch {
	case in.CloneMode && in.EmptyPerAgent:
		return "fresh empty workspace on the target", ""
	case in.CloneMode && in.GitRemote != "":
		return fmt.Sprintf("fresh clone of %s on the target; branch %s is checked out if it was pushed, otherwise created from %s",
			displayGitRemote(in.GitRemote), valueOrUnknown(in.Branch), valueOrUnknown(in.DefaultBranch)), ""
	case in.CloneMode:
		return "", "--force recreates a clone-per-agent workspace from the project's git remote; this project has no git remote"
	default:
		return "", fmt.Sprintf("--force supports clone-per-agent and empty-per-agent workspaces; this agent's workspace is %s",
			valueOrUnknown(in.WorkspaceMode))
	}
}

// displayGitRemote is the project's git remote as move verdicts show it,
// normalized for display (util.NormalizeGitRemote).
func displayGitRemote(remote string) string {
	return util.NormalizeGitRemote(remote)
}

func valueOrUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// moveWorkspaceBundleable decides whether a bundle move can carry the agent's
// workspace (clone-per-agent or empty-per-agent, regardless of git remote).
func moveWorkspaceBundleable(in moveEligibilityInput) (passMsg, failMsg string) {
	if in.CloneMode {
		if in.EmptyPerAgent {
			return "empty-per-agent workspace and home can be bundled", ""
		}
		return "clone-per-agent workspace and home can be bundled", ""
	}
	return "", fmt.Sprintf("bundle transfer supports clone-per-agent and empty-per-agent workspaces; this agent's workspace is %s",
		valueOrUnknown(in.WorkspaceMode))
}

// isMoveBundleStorageSupported reports whether stor is configured and uses a
// provider that issues HTTP(S) signed URLs usable by remote brokers (today: GCS).
func isMoveBundleStorageSupported(stor storage.Storage) bool {
	return stor != nil && stor.Provider() == storage.ProviderGCS
}

// moveBundleStorageCheck returns "" when stor supports move-bundle signed
// URLs, or the refusal message for the bundle_storage check (design §3.1).
func moveBundleStorageCheck(stor storage.Storage) string {
	if stor == nil {
		return "hub object storage is not configured"
	}
	if !isMoveBundleStorageSupported(stor) {
		return fmt.Sprintf("hub object storage provider %q cannot issue signed URLs a remote broker can use", stor.Provider())
	}
	return ""
}

// formatMoveBundleOverLimit formats a precheck Over field into a human-readable
// limit-exceeded message.
func formatMoveBundleOverLimit(resp *api.MoveBundlePrecheckResponse) string {
	switch resp.Over {
	case "workspace.bytes":
		return fmt.Sprintf("workspace bytes (%s) exceed configured limit", formatMoveBundleBytes(resp.Workspace.Bytes))
	case "workspace.entries":
		return fmt.Sprintf("workspace entries (%d) exceed configured limit", resp.Workspace.Entries)
	case "home.bytes":
		return fmt.Sprintf("home bytes (%s) exceed configured limit", formatMoveBundleBytes(resp.Home.Bytes))
	case "home.entries":
		return fmt.Sprintf("home entries (%d) exceed configured limit", resp.Home.Entries)
	default:
		return fmt.Sprintf("%s exceeds configured limit", strings.ReplaceAll(resp.Over, ".", " "))
	}
}

// formatMoveBundleBytes renders a byte count in human-readable binary units.
func formatMoveBundleBytes(b int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(mib))
	case b >= kib:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(kib))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// evaluateMoveChecks runs the checks for one transfer mode (export, bundle, or
// recreate).
func evaluateMoveChecks(in moveEligibilityInput, mode moveEvalMode) (MoveVerdict, *moveRefusal) {
	order := moveCheckOrder
	if mode == moveEvalBundle {
		order = moveBundleCheckOrder
	}
	v := MoveVerdict{
		SourceBroker: MoveBrokerRef{ID: in.Src.ID, Name: in.Src.Name},
		TargetBroker: MoveBrokerRef{ID: in.Dst.ID, Name: in.Dst.Name},
		Checks:       make([]MoveCheck, 0, len(order)),
	}
	for _, name := range order {
		v.Checks = append(v.Checks, MoveCheck{Name: name, Result: MoveCheckNotEvaluated})
	}
	idx := 0
	pass := func() { v.Checks[idx].Result = MoveCheckPassed; idx++ }
	waiveMsg := moveForceWaivedMessage
	if mode == moveEvalBundle {
		waiveMsg = moveBundleWaivedMessage
	}
	waive := func() {
		v.Checks[idx].Result = MoveCheckWaived
		v.Checks[idx].Message = waiveMsg
		idx++
	}
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

	switch mode {
	case moveEvalRecreate:
		// 2-4. Waived: the workspace is not carried, so the brokers need not
		// share an export and the workspace need not be on one.
		waive()
		waive()
		waive()
		// 4a. The workspace mode can be recreated on the target.
		msg, failMsg := moveWorkspaceRecreatable(in)
		if failMsg != "" {
			return fail(http.StatusConflict, ErrCodeConflict, failMsg)
		}
		v.Checks[idx].Message = msg
		pass()
	case moveEvalBundle:
		// 2-4. Waived: workspace and home are copied through Hub object storage.
		waive()
		waive()
		waive()
		// 4a. bundle_experiment: hub.agent_move_bundle must be enabled.
		if !in.BundleExperimentEnabled {
			return fail(http.StatusPreconditionFailed, ErrCodeExperimentDisabled,
				fmt.Sprintf("bundle transfer requires the %s experiment", experiments.AgentMoveBundle))
		}
		pass()
		// 4b. workspace_bundleable: clone-per-agent and empty-per-agent only.
		msg, failMsg := moveWorkspaceBundleable(in)
		if failMsg != "" {
			return fail(http.StatusConflict, ErrCodeConflict, failMsg)
		}
		v.Checks[idx].Message = msg
		pass()
		// 4c. bundle_storage: Hub object storage configured and HTTP-signable.
		if in.BundleStorageError != "" {
			return fail(http.StatusConflict, api.BrokerErrCodeMoveBundleUnsupported, in.BundleStorageError)
		}
		pass()
	default:
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
					"broker %s has not reported the export identity marker (<subpath_root>/%s on its NFS export); upgrade it, or check that its export mount is healthy and writable",
					brokerDisplayName(b), api.ExportIDMarkerName))
			}
		}
		if srcID, dstID := src.WorkspaceStorage.NFS.ExportID, dst.WorkspaceStorage.NFS.ExportID; srcID != dstID {
			// The refusal names the field only; the values go to the log.
			slog.Warn("refusing move: brokers see different export identity markers",
				"diff", projectfs.LayoutIssueSummary([]projectfs.LayoutIssue{projectfs.ExportIDIssue(srcName, srcID, dstName, dstID)}, projectfs.ViewLog))
			return fail(http.StatusConflict, ErrCodeConflict, fmt.Sprintf(
				"brokers %s and %s configure the same NFS export but see different export identity markers, so they do not mount the same directory (%s). --broker requires both brokers to mount the same NFS export",
				srcName, dstName, projectfs.LayoutIssuesRemediation))
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
		// 4a. Nothing is recreated in an export move.
		pass()
	}

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
	// The Kubernetes constraint only keeps the workspace on the export; a
	// bundled or recreated workspace does not need it.
	if mode == moveEvalExport && in.CloneMode && !isKubernetesRuntimeType(profileType) {
		v, ref := fail(http.StatusConflict, ErrCodeConflict, fmt.Sprintf(
			"profile %q on broker %s has runtime type %q; clone-per-agent workspaces are on the NFS export only on Kubernetes",
			profileName, dstName, profileType))
		ref.placementCaused = true
		return v, ref
	}
	pass()

	// 6. The target's export mount is healthy (when needed) and the target is
	// reachable. Bundle mode requires only Probes.Reachable(dst).
	if mode != moveEvalBundle && moveNeedsTargetExportMount(dst, mode == moveEvalRecreate, in.CloneMode, profileType) && !targetExportMountHealthy(dst) {
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
		if in.Probes.PolicyRefused != nil && in.Probes.PolicyRefused(dst) {
			msg := brokerPolicyMessage(brokerPolicyOpMove, dst, "")
			v, ref := fail(http.StatusForbidden, ErrCodeBrokerPolicyAdminManagedOnly, msg)
			ref.Details = brokerPolicyDetails(brokerPolicyOpMove, dst)
			return v, ref
		}
		if !in.Probes.ServesProject(dst) {
			return fail(http.StatusConflict, ErrCodeConflict, fmt.Sprintf(
				"target broker %s does not serve this project; an agent can only move itself to a broker that already serves its project", dstName))
		}
	} else {
		if in.Probes.PolicyRefused != nil && in.Probes.PolicyRefused(dst) {
			msg := brokerPolicyMessage(brokerPolicyOpMove, dst, "")
			v, ref := fail(http.StatusForbidden, ErrCodeBrokerPolicyAdminManagedOnly, msg)
			ref.Details = brokerPolicyDetails(brokerPolicyOpMove, dst)
			return v, ref
		}
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
	// A broker host service account record stays with its broker, for
	// every caller.
	if in.Probes.HostSARecord != nil {
		if msg := in.Probes.HostSARecord(dst); msg != "" {
			return fail(http.StatusConflict, ErrCodeConflict, msg)
		}
	}
	pass()

	// 8. Both brokers support agent move, and the source is online so it
	// can clean up its local state (or export the bundle).
	for _, b := range []*store.RuntimeBroker{dst, src} {
		if b.Capabilities == nil || !b.Capabilities.AgentMove {
			return fail(http.StatusPreconditionFailed, ErrCodeUnsupportedCapability,
				fmt.Sprintf("broker %s does not support agent move; upgrade it", brokerDisplayName(b)))
		}
	}
	if mode == moveEvalBundle {
		for _, b := range []*store.RuntimeBroker{dst, src} {
			if b.Capabilities == nil || !b.Capabilities.AgentMoveBundle {
				return fail(http.StatusPreconditionFailed, ErrCodeUnsupportedCapability,
					fmt.Sprintf("broker %s does not support bundle move; upgrade it", brokerDisplayName(b)))
			}
		}
	}
	// A forced move needs a target that refuses to provision over files
	// already there (source cleanup is a localOnly delete, which AgentMove
	// covers).
	// Unreachable defence in depth: the AgentMove check above refuses nil Capabilities.
	if mode == moveEvalRecreate && (dst.Capabilities == nil || !dst.Capabilities.AgentMoveRecreate) {
		return fail(http.StatusPreconditionFailed, ErrCodeUnsupportedCapability,
			fmt.Sprintf("broker %s does not support forced moves; upgrade it", dstName))
	}
	if !in.Probes.Reachable(src) {
		return fail(http.StatusPreconditionFailed, ErrCodeRuntimeBrokerUnavail,
			fmt.Sprintf("source broker %s is unavailable; a move needs it online to clean up", srcName))
	}
	pass()

	// 8a. bundle_source: probe source broker for post-stop durability and
	// stat-only size/entry estimates against Hub limits.
	if mode == moveEvalBundle {
		if in.Probes.BundleSource == nil {
			return fail(http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail,
				"source bundle precheck is unavailable")
		}
		precheck, err := in.Probes.BundleSource(src, in.Agent)
		if err != nil {
			return fail(http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail,
				moveBundlePrecheckErrorMessage(err))
		}
		if precheck == nil {
			return fail(http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail,
				"source bundle precheck returned no response")
		}
		if !precheck.Durable {
			reason := precheck.Reason
			if reason == "" {
				reason = "source runtime does not preserve workspace and home across stop"
			}
			return fail(http.StatusConflict, ErrCodeConflict, reason)
		}
		if precheck.Over != "" {
			return fail(http.StatusConflict, ErrCodeConflict,
				fmt.Sprintf("bundle exceeds limit: %s", formatMoveBundleOverLimit(precheck)))
		}
		v.BundleEstimate = &MoveBundleEstimate{
			WorkspaceBytes:   precheck.Workspace.Bytes,
			WorkspaceEntries: precheck.Workspace.Entries,
			HomeBytes:        precheck.Home.Bytes,
			HomeEntries:      precheck.Home.Entries,
		}
		v.Checks[idx].Message = fmt.Sprintf("source runtime is durable (~%s total)",
			formatMoveBundleBytes(precheck.Workspace.Bytes+precheck.Home.Bytes))
		pass()
	}

	// 9. The target has room under its agent limit (read-only).
	if msg := in.Probes.Capacity(dst); msg != "" {
		return fail(http.StatusTooManyRequests, ErrCodeQuotaExceeded, msg)
	}
	pass()

	v.Eligible = true
	return v, nil
}

// moveNeedsTargetExportMount reports whether the target_health check must
// require a healthy NFS export mount on dst. An export move always does. A
// recreated workspace does only when dst places the agent directory on the
// export: an agent-directory workspace (clone-per-agent, empty-per-agent)
// under a Kubernetes profile, with an nfs workspace storage backend. An nfs
// backend with no NFS details still requires the mount, and
// targetExportMountHealthy reads it as unhealthy (503). A target that reports
// no workspace storage at all does not: a forced move waives
// workspace_storage_reported for it, and requiring the mount here would undo
// that waiver.
func moveNeedsTargetExportMount(dst *store.RuntimeBroker, recreate, agentDir bool, profileType string) bool {
	if !recreate {
		return true
	}
	ws := dst.WorkspaceStorage
	return ws != nil && ws.Backend == api.WorkspaceStorageBackendNFS &&
		agentDir && isKubernetesRuntimeType(profileType)
}

// targetExportMountHealthy reports whether dst reports a healthy NFS export
// mount. A missing descriptor reads as unhealthy.
func targetExportMountHealthy(dst *store.RuntimeBroker) bool {
	ws := dst.WorkspaceStorage
	return ws != nil && ws.NFS != nil && ws.NFS.Healthy
}

// buildMoveConsequences lists what an eligible move keeps, carries,
// recreates and loses, grouped by kind in display order. Tests pin the item
// keys; the text is for people.
func buildMoveConsequences(mode string, in moveEligibilityInput, est *MoveBundleEstimate) []MoveConsequence {
	srcName, dstName := brokerDisplayName(in.Src), brokerDisplayName(in.Dst)
	gen := in.Agent.Generation
	c := func(kind, item, text string) MoveConsequence {
		return MoveConsequence{Kind: kind, Item: item, Text: text}
	}
	out := []MoveConsequence{
		c(MoveConsequenceKept, "identity", fmt.Sprintf("agent ID, name, generation chain (%d -> %d), creator and ancestry", gen, gen+1)),
		c(MoveConsequenceKept, "relationships", "conversations, subscriptions, notification routes and messages addressed to the agent"),
		c(MoveConsequenceKept, "config", "the applied configuration (template, harness, profile, env, labels), re-resolved as for any reincarnate"),
	}
	switch mode {
	case MoveTransferModeExport:
		out = append(out,
			c(MoveConsequenceKept, "workspace", fmt.Sprintf("the workspace on the shared NFS export, found in place by %s", dstName)),
			c(MoveConsequenceKept, "local-work", "uncommitted files and unpushed commits in the workspace"),
		)
	case MoveTransferModeBundle:
		out = append(out,
			c(MoveConsequenceCarried, "workspace", fmt.Sprintf("copied through hub storage to %s, including .git, uncommitted changes and unpushed commits", dstName)),
			c(MoveConsequenceCarried, "home", fmt.Sprintf("copied through hub storage to %s, except secrets, credentials and caches, which are regenerated or reinjected on %s", dstName, dstName)),
		)
		if est != nil {
			out = append(out, c(MoveConsequenceCarried, "bundle-size", fmt.Sprintf(
				"estimated snapshot size: ~%s (workspace %s, %d entries; home %s, %d entries)",
				formatMoveBundleBytes(est.WorkspaceBytes+est.HomeBytes),
				formatMoveBundleBytes(est.WorkspaceBytes), est.WorkspaceEntries,
				formatMoveBundleBytes(est.HomeBytes), est.HomeEntries,
			)))
		}
	}
	out = append(out, c(MoveConsequenceCarried, "handoff", "the handoff (if given) is delivered as the new generation's first task, with the reincarnation preamble"))
	switch mode {
	case MoveTransferModeBundle:
		out = append(out,
			c(MoveConsequenceRecreated, "harness-session", "a fresh harness session; conversation history files are kept in $HOME"),
			c(MoveConsequenceRecreated, "credentials", fmt.Sprintf("agent token, secrets and harness auth issued fresh to the new container on %s", dstName)),
		)
	case MoveTransferModeRecreate:
		ws := fmt.Sprintf("fresh clone of %s on %s", valueOrUnknown(displayGitRemote(in.GitRemote)), dstName)
		if in.EmptyPerAgent {
			ws = fmt.Sprintf("fresh empty directory on %s", dstName)
		}
		out = append(out,
			c(MoveConsequenceRecreated, "workspace", ws),
			c(MoveConsequenceRecreated, "home", fmt.Sprintf("regenerated from the template and harness config on %s", dstName)),
			c(MoveConsequenceRecreated, "harness-session", "a new harness session; no conversation history"),
			c(MoveConsequenceRecreated, "credentials", fmt.Sprintf("agent token, secrets and harness auth issued fresh to the new container on %s", dstName)),
			c(MoveConsequenceLost, "local-work",
				fmt.Sprintf("uncommitted files, unpushed commits and anything else in the workspace on %s", srcName)),
		)
	default:
		out = append(out,
			c(MoveConsequenceRecreated, "home", fmt.Sprintf("regenerated from the template and harness config on %s", dstName)),
			c(MoveConsequenceRecreated, "harness-session", "a new harness session; no conversation history"),
			c(MoveConsequenceRecreated, "credentials", fmt.Sprintf("agent token, secrets and harness auth issued fresh to the new container on %s", dstName)),
		)
	}
	out = append(out, c(MoveConsequenceLost, "process-state", "running processes, open ports and in-container state"))
	return out
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

// sameExportMessage is the refusal of a move between brokers that do not
// report the same NFS export. It names the differing fields only
// (projectfs.ViewPublic); the values are logged.
func sameExportMessage(src, dst *store.RuntimeBroker) string {
	srcName, dstName := brokerDisplayName(src), brokerDisplayName(dst)
	issues := projectfs.ExportIssues(srcName, src.WorkspaceStorage, dstName, dst.WorkspaceStorage)
	slog.Warn("refusing move: brokers do not report the same NFS export",
		"source", src.ID, "target", dst.ID, "diff", projectfs.LayoutIssueSummary(issues, projectfs.ViewLog))
	return fmt.Sprintf("agent workspace on broker %s is not on the NFS export broker %s mounts (%s). --broker requires both brokers to mount the same NFS export",
		srcName, dstName, projectfs.LayoutIssueSummary(issues, projectfs.ViewPublic))
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

// pluginBrokerLabel marks a broker record that represents a message-broker
// plugin (e.g. a chat integration) rather than a runtime broker. See
// isPluginBroker in broker_kind.go.
const pluginBrokerLabel = "scion.io/plugin"

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
// --broker value, listing the project's available brokers (online provider
// rows whose broker is present, getAvailableBrokersForProject). The body
// depends only on the request and the project, never on the broker matched.
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
		Reachable: func(b *store.RuntimeBroker) bool { return s.brokerRecordReachable(ctx, b) },
		PolicyRefused: func(dst *store.RuntimeBroker) bool {
			return s.brokerPolicyRefuses(dst)
		},
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
	p.HostSARecord = s.hostSARecordMoveProbe(ctx, ac)
	p.BundleSource = s.moveBundleSourceProbe(ctx)
	if ac != nil && ac.GCPIdentity != nil && ac.GCPIdentity.MetadataMode == store.GCPMetadataModePassthrough {
		projectID := ""
		if project != nil {
			projectID = project.ID
		}
		p.Passthrough = func(dst *store.RuntimeBroker) string {
			if d := s.evaluatePassthroughIdentity(r.Context(), r, dst, projectID, SurfacePassthroughMove); d != nil {
				return d.msg
			}
			return ""
		}
	}
	return p
}

// hostSARecordMoveProbe returns the move probe that refuses a target broker
// other than the one the agent's broker host service account record is bound
// to, or nil when the agent has no assign-mode identity. The move handler and
// the move worker's recheck both use it. A record that cannot be read refuses
// the move (fails closed); like the other eligibility probes it answers with
// the probe's own status rather than a store-fault code.
func (s *Server) hostSARecordMoveProbe(ctx context.Context, ac *store.AgentAppliedConfig) func(dst *store.RuntimeBroker) string {
	if ac == nil || ac.GCPIdentity == nil || ac.GCPIdentity.MetadataMode != store.GCPMetadataModeAssign ||
		ac.GCPIdentity.ServiceAccountID == "" {
		return nil
	}
	saID := ac.GCPIdentity.ServiceAccountID
	return func(dst *store.RuntimeBroker) string {
		sa, err := s.store.GetGCPServiceAccount(ctx, saID)
		if errors.Is(err, store.ErrNotFound) {
			return ""
		}
		if err != nil {
			return "the agent's GCP identity could not be checked; retry later"
		}
		if store.HostSATranslationRecordNotBoundTo(sa, dst.ID) {
			if sa.ScopeID == "" {
				return fmt.Sprintf("this agent's GCP identity is a broker host service account record with no broker "+
					"binding and cannot move to broker %s; recreate the agent there with GCP identity passthrough",
					brokerDisplayName(dst))
			}
			return fmt.Sprintf("this agent's GCP identity is the host service account of broker %s (the broker it was "+
				"created for) and cannot move to broker %s; recreate the agent there with GCP identity passthrough",
				sa.ScopeID, brokerDisplayName(dst))
		}
		return ""
	}
}

// MoveBundleExportLimits returns the Hub-configured api.MoveBundleLimits for
// precheck and export requests (with design §3.7 defaults for unset fields).
func (s *Server) MoveBundleExportLimits() api.MoveBundleLimits {
	return s.config.MoveBundle.ExportLimits()
}

// MoveBundleImportLimits returns the Hub-configured api.MoveBundleLimits for
// import requests (with design §3.7 defaults for unset fields).
func (s *Server) MoveBundleImportLimits() api.MoveBundleLimits {
	return s.config.MoveBundle.ImportLimits()
}

// moveBundleSecretFileTargets resolves the file-type secret targets for agent
// using the server's dispatcher (if it exposes ResolveMoveBundleSecretFileTargets).
// Returns a non-nil empty slice when none apply. Part D reuses this helper for
// DispatchMoveBundleExport.
func (s *Server) moveBundleSecretFileTargets(ctx context.Context, agent *store.Agent) ([]string, error) {
	type secretTargetResolver interface {
		ResolveMoveBundleSecretFileTargets(ctx context.Context, agent *store.Agent) ([]string, error)
	}
	if r, ok := s.GetDispatcher().(secretTargetResolver); ok {
		return r.ResolveMoveBundleSecretFileTargets(ctx, agent)
	}
	return []string{}, nil
}

// errMoveBundleSecretTargetsFailed marks a secret-target resolution error in
// moveBundleSourceProbe so moveBundlePrecheckErrorMessage maps it to a fixed
// client-safe message without leaking backend details.
var errMoveBundleSecretTargetsFailed = errors.New("could not resolve the agent's secret targets")

// moveBundlePrecheckErrorMessage returns a client-safe refusal message for a
// failed bundle_source probe. Raw transport errors (which may contain internal
// broker endpoint URLs), store errors, and secret-backend errors never reach
// API responses; broker HTTP status errors are sanitized via dispatchFailureText.
func moveBundlePrecheckErrorMessage(err error) string {
	if errors.Is(err, errMoveBundleSecretTargetsFailed) {
		return errMoveBundleSecretTargetsFailed.Error()
	}
	var se *brokerStatusError
	if errors.As(err, &se) && se != nil {
		return "source bundle precheck failed: " + dispatchFailureText(se)
	}
	return "source bundle precheck failed"
}

// moveBundleSourceProbe returns the moveProbes.BundleSource callback that
// calls DispatchMoveBundlePrecheck on the source broker using the Hub's
// configured move-bundle limits and hub-resolved file-type secret targets.
func (s *Server) moveBundleSourceProbe(ctx context.Context) func(src *store.RuntimeBroker, agent *store.Agent) (*api.MoveBundlePrecheckResponse, error) {
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return nil
	}
	md, ok := dispatcher.(agentMoveDispatcher)
	if !ok {
		return nil
	}
	limits := s.MoveBundleExportLimits()
	return func(src *store.RuntimeBroker, agent *store.Agent) (*api.MoveBundlePrecheckResponse, error) {
		srcAgent := withBroker(agent, src.ID)
		secretTargets, err := s.moveBundleSecretFileTargets(ctx, srcAgent)
		if err != nil {
			s.agentLifecycleLog.Warn("move bundle precheck: failed to resolve secret file targets",
				"agent_id", agent.ID, "source_broker_id", src.ID, "error", err)
			return nil, errMoveBundleSecretTargetsFailed
		}
		resp, err := md.DispatchMoveBundlePrecheck(ctx, srcAgent, limits, secretTargets)
		if err != nil {
			s.agentLifecycleLog.Warn("move bundle precheck: broker precheck failed",
				"agent_id", agent.ID, "source_broker_id", src.ID, "error", err)
			return nil, err
		}
		return resp, nil
	}
}

// writeMoveRefusal writes a move refusal with the verdict in
// error.details.verdict.
func writeMoveRefusal(w http.ResponseWriter, ref *moveRefusal, v MoveVerdict) {
	details := map[string]interface{}{"verdict": v}
	for k, val := range ref.Details {
		details[k] = val
	}
	if ref.ForceWouldApply {
		details["forceWouldApply"] = true
	}
	writeError(w, ref.Status, ref.Code, ref.Message, details)
}
