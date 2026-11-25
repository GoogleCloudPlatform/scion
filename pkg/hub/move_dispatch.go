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
	"sort"

	agentpkg "github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
)

// agentMoveDispatcher is the broker dispatch a cross-broker move needs
// beyond AgentDispatcher. HTTPAgentDispatcher implements it; the reincarnate
// worker refuses a move with a dispatcher that does not.
type agentMoveDispatcher interface {
	// DispatchAgentProvisionForMove provisions the agent, without starting
	// it, on agent.RuntimeBrokerID, asking the broker to first confirm the
	// moved agent's workspace expectation: its existing workspace on its
	// mount of the NFS export (expectNFSWorkspace: agent.MoveWorkspaceAgentDir
	// or agent.MoveWorkspaceProject), or, for a forced move
	// (agent.MoveWorkspaceFreshAgentDir), that it has no files for the agent
	// yet. A failed confirmation is a 409 from the broker and an error here.
	// moveReincarnationID is required with agent.MoveWorkspaceFreshAgentDir
	// (the broker marks the agent directory it creates with it) and must be
	// "" otherwise.
	DispatchAgentProvisionForMove(ctx context.Context, agent *store.Agent, expectNFSWorkspace, moveReincarnationID string) error
	// DispatchAgentDeleteLocalOnly removes the agent's state on
	// agent.RuntimeBrokerID only (container, broker-local agent directory
	// and home), never its files on the NFS export or its branch. It is
	// sent only to a broker advertising AgentMove: an older broker would
	// ignore localOnly and delete the shared workspace.
	DispatchAgentDeleteLocalOnly(ctx context.Context, agent *store.Agent) error
	// DispatchAgentDeleteRecreated removes, on agent.RuntimeBrokerID, what a
	// failed forced move created there: the agent's container and its
	// files, including a workspace on the NFS export, but never its branch.
	// It names the agent's project and, when the broker recorded one, its
	// run. It is sent only to a broker advertising AgentMoveRecreate, which
	// confirmed it had no files for the agent before provisioning, so
	// everything it removes was created by the move. It carries the move's
	// reincarnation ID: the broker removes the agent's directories only when
	// every one that exists carries that move's marker, and otherwise
	// answers 409 api.BrokerErrCodeMoveMarkerMismatch and removes nothing.
	DispatchAgentDeleteRecreated(ctx context.Context, agent *store.Agent, moveReincarnationID string) error
	// DispatchAgentProvisionForBundleMove provisions the agent on
	// agent.RuntimeBrokerID for a bundle move: fresh-agent-dir expectation,
	// moveReincarnationID marker, and WorkspaceSource="move-bundle" (empty
	// workspace directory, home rendered normally). Refuses brokers without
	// AgentMove and AgentMoveBundle.
	DispatchAgentProvisionForBundleMove(ctx context.Context, agent *store.Agent, moveReincarnationID string) error
	// DispatchMoveBundlePrecheck probes source durability and stat-only
	// workspace/$HOME estimates against hub-supplied limits and hub-resolved
	// file-secret targets.
	DispatchMoveBundlePrecheck(ctx context.Context, agent *store.Agent, limits api.MoveBundleLimits, secretFileTargets []string) (*api.MoveBundlePrecheckResponse, error)
	// DispatchMoveBundleExport starts (or idempotently polls) a bundle export
	// job on agent.RuntimeBrokerID with hub-resolved file-secret targets.
	DispatchMoveBundleExport(ctx context.Context, agent *store.Agent, reincarnationID string, limits api.MoveBundleLimits, secretFileTargets []string) (*api.MoveBundleJobStatus, error)
	// DispatchMoveBundleExportStatus polls a running/finished export job.
	DispatchMoveBundleExportStatus(ctx context.Context, agent *store.Agent, reincarnationID string) (*api.MoveBundleJobStatus, error)
	// DispatchMoveBundleImport starts (or idempotently polls) a bundle import
	// job on agent.RuntimeBrokerID.
	DispatchMoveBundleImport(ctx context.Context, agent *store.Agent, reincarnationID string, manifest bundle.Manifest, manifestHash string, limits api.MoveBundleLimits) (*api.MoveBundleJobStatus, error)
	// DispatchMoveBundleImportStatus polls a running/finished import job.
	DispatchMoveBundleImportStatus(ctx context.Context, agent *store.Agent, reincarnationID string) (*api.MoveBundleJobStatus, error)
}

var _ agentMoveDispatcher = (*HTTPAgentDispatcher)(nil)

// errBrokerLacksAgentMove is returned, before anything is sent, when a
// localOnly delete or a provision-for-move targets a broker that does not
// advertise AgentMove.
var errBrokerLacksAgentMove = errors.New("runtime broker does not advertise the agent move capability; refusing to send it a move request")

// errBrokerLacksAgentMoveRecreate is returned, before anything is sent,
// when a forced move's provision or rollback delete targets a broker that
// does not advertise AgentMoveRecreate. An older broker that does not know
// the fresh-agent-dir expectation must never be relied on to refuse it.
var errBrokerLacksAgentMoveRecreate = errors.New("runtime broker does not advertise the forced agent move capability; refusing to send it a forced move request")

// errBrokerLacksAgentMoveBundle is returned, before anything is sent, when a
// bundle move operation targets a broker that does not advertise
// AgentMoveBundle.
var errBrokerLacksAgentMoveBundle = errors.New("runtime broker does not advertise the agent move bundle capability; refusing to send it a move-bundle request")

// errProvisionSent marks a provision error returned after the request may
// have reached the broker: dispatchProvision's send boundary and every step
// after it. errors.Is matches it through provisionSentError, which keeps
// the wrapped error's text and chain. An unmarked provision error was
// returned before anything was sent.
var errProvisionSent = errors.New("provision sent to the broker")

// provisionSentError wraps a provision error returned at or after the send
// boundary: its text and Unwrap are the wrapped error's, and
// errors.Is(err, errProvisionSent) holds.
type provisionSentError struct{ err error }

func (e provisionSentError) Error() string        { return e.err.Error() }
func (e provisionSentError) Unwrap() error        { return e.err }
func (e provisionSentError) Is(target error) bool { return target == errProvisionSent }

// provisionSent marks err (non-nil) as returned after the send boundary.
func provisionSent(err error) error {
	return provisionSentError{err: err}
}

// requireMoveCapability loads agent.RuntimeBrokerID and returns
// errBrokerLacksAgentMove unless it advertises AgentMove, and, with
// recreate, errBrokerLacksAgentMoveRecreate unless it also advertises
// AgentMoveRecreate.
func (d *HTTPAgentDispatcher) requireMoveCapability(ctx context.Context, agent *store.Agent, recreate bool) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return fmt.Errorf("load runtime broker %s: %w", agent.RuntimeBrokerID, err)
	}
	if broker.Capabilities == nil || !broker.Capabilities.AgentMove {
		return errBrokerLacksAgentMove
	}
	if recreate && !broker.Capabilities.AgentMoveRecreate {
		return errBrokerLacksAgentMoveRecreate
	}
	return nil
}

// requireMoveFreshTargetCapability loads agent.RuntimeBrokerID and returns
// errBrokerLacksAgentMove or errBrokerLacksAgentMoveRecreate unless the broker
// advertises AgentMove and either AgentMoveRecreate or AgentMoveBundle (both
// implement marker-guarded fresh-target rollback deletes).
func (d *HTTPAgentDispatcher) requireMoveFreshTargetCapability(ctx context.Context, agent *store.Agent) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return fmt.Errorf("load runtime broker %s: %w", agent.RuntimeBrokerID, err)
	}
	if broker.Capabilities == nil || !broker.Capabilities.AgentMove {
		return errBrokerLacksAgentMove
	}
	if !broker.Capabilities.AgentMoveRecreate && !broker.Capabilities.AgentMoveBundle {
		return errBrokerLacksAgentMoveRecreate
	}
	return nil
}

// requireMoveBundleCapability loads agent.RuntimeBrokerID and returns
// errBrokerLacksAgentMove or errBrokerLacksAgentMoveBundle unless the broker
// advertises both AgentMove and AgentMoveBundle.
func (d *HTTPAgentDispatcher) requireMoveBundleCapability(ctx context.Context, agent *store.Agent) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return fmt.Errorf("load runtime broker %s: %w", agent.RuntimeBrokerID, err)
	}
	if broker.Capabilities == nil || !broker.Capabilities.AgentMove {
		return errBrokerLacksAgentMove
	}
	if !broker.Capabilities.AgentMoveBundle {
		return errBrokerLacksAgentMoveBundle
	}
	return nil
}

// DispatchAgentProvisionForBundleMove implements agentMoveDispatcher.
func (d *HTTPAgentDispatcher) DispatchAgentProvisionForBundleMove(ctx context.Context, agent *store.Agent, moveReincarnationID string) error {
	if moveReincarnationID == "" {
		return errors.New("DispatchAgentProvisionForBundleMove: moveReincarnationID is required")
	}
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}
	if err := d.placementBackstop(ctx, agent); err != nil {
		return err
	}
	if err := d.requireMoveBundleCapability(ctx, agent); err != nil {
		return err
	}
	return d.dispatchProvisionWithWorkspaceSource(ctx, agent, "DispatchAgentProvisionForBundleMove", false, agentpkg.MoveWorkspaceFreshAgentDir, moveReincarnationID, api.WorkspaceSourceMoveBundle, ReprovisionOptions{})
}

func (d *HTTPAgentDispatcher) prepareMoveBundleCall(ctx context.Context, agent *store.Agent) (context.Context, moveBundleBrokerClient, string, projectDispatchInfo, error) {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return ctx, nil, "", projectDispatchInfo{}, err
	}
	if err := d.placementBackstop(ctx, agent); err != nil {
		return ctx, nil, "", projectDispatchInfo{}, err
	}
	if err := d.requireMoveBundleCapability(ctx, agent); err != nil {
		return ctx, nil, "", projectDispatchInfo{}, err
	}
	mbClient, ok := d.client.(moveBundleBrokerClient)
	if !ok {
		return ctx, nil, "", projectDispatchInfo{}, errors.New("broker client does not support move-bundle dispatch")
	}
	ctx = withRecordedRuntime(ctx, agent.Runtime)
	ctx = withDispatchAgentID(ctx, agent.ID)
	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return ctx, nil, "", projectDispatchInfo{}, err
	}
	info, err := d.resolveDispatchProjectInfo(ctx, agent)
	if err != nil {
		return ctx, nil, "", projectDispatchInfo{}, fmt.Errorf("resolve project dispatch info: %w", err)
	}
	if agent.ProjectID != "" && info.projectID == "" {
		return ctx, nil, "", projectDispatchInfo{}, fmt.Errorf("resolve project dispatch info: project %s: %w", agent.ProjectID, store.ErrNotFound)
	}
	return ctx, mbClient, endpoint, info, nil
}

// ResolveMoveBundleSecretFileTargets returns the deduplicated, sorted list of
// file-type secret targets for agent across all applicable scopes using
// metadata-only store listing (ListSecrets). It never fetches or decrypts
// secret values and never runs or flushes the start-time owner-material gate.
// Always returns a non-nil slice on success (an explicit empty slice when no
// file-type secrets apply) so callers can pass it directly to
// DispatchMoveBundlePrecheck and DispatchMoveBundleExport.
func (d *HTTPAgentDispatcher) ResolveMoveBundleSecretFileTargets(ctx context.Context, agent *store.Agent) ([]string, error) {
	if d.store == nil || agent == nil {
		return []string{}, nil
	}
	type scopeRef struct {
		scope   string
		scopeID string
	}
	var scopes []scopeRef
	if isShellAgent(agent) {
		if agent.OwnerID != "" {
			scopes = append(scopes, scopeRef{store.ScopeUser, agent.OwnerID})
		}
	} else {
		scopes = append(scopes, scopeRef{store.ScopeHub, d.hubID})
		if agent.RuntimeBrokerID != "" {
			scopes = append(scopes, scopeRef{store.ScopeRuntimeBroker, agent.RuntimeBrokerID})
		}
		if agent.ProjectID != "" {
			scopes = append(scopes, scopeRef{store.ScopeProject, agent.ProjectID})
		}
		if agent.OwnerID != "" {
			scopes = append(scopes, scopeRef{store.ScopeUser, agent.OwnerID})
		}
		if d.agentSecretsEnabled() && agent.ID != "" {
			scopes = append(scopes, scopeRef{store.ScopeAgent, agent.ID})
		}
	}
	seen := make(map[string]struct{})
	targets := make([]string, 0)
	for _, sc := range scopes {
		secrets, err := d.store.ListSecrets(ctx, store.SecretFilter{
			Scope:   sc.scope,
			ScopeID: sc.scopeID,
			Type:    store.SecretTypeFile,
		})
		if err != nil {
			return nil, fmt.Errorf("list %s file-type secret targets: %w", sc.scope, err)
		}
		for _, sec := range secrets {
			if sec.Target == "" {
				continue
			}
			if _, dup := seen[sec.Target]; !dup {
				seen[sec.Target] = struct{}{}
				targets = append(targets, sec.Target)
			}
		}
	}
	sort.Strings(targets)
	return targets, nil
}

// DispatchMoveBundlePrecheck implements agentMoveDispatcher.
func (d *HTTPAgentDispatcher) DispatchMoveBundlePrecheck(ctx context.Context, agent *store.Agent, limits api.MoveBundleLimits, secretFileTargets []string) (*api.MoveBundlePrecheckResponse, error) {
	ctx, mbClient, endpoint, info, err := d.prepareMoveBundleCall(ctx, agent)
	if err != nil {
		return nil, err
	}
	req := &api.MoveBundlePrecheckRequest{
		ProjectID:         agent.ProjectID,
		ProjectPath:       info.projectPath,
		ProjectSlug:       info.projectSlug,
		SecretFileTargets: secretFileTargets,
		Limits:            limits,
	}
	return mbClient.PrecheckMoveBundle(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, req)
}

// DispatchMoveBundleExport implements agentMoveDispatcher.
func (d *HTTPAgentDispatcher) DispatchMoveBundleExport(ctx context.Context, agent *store.Agent, reincarnationID string, limits api.MoveBundleLimits, secretFileTargets []string) (*api.MoveBundleJobStatus, error) {
	if reincarnationID == "" {
		return nil, errors.New("DispatchMoveBundleExport: reincarnationID is required")
	}
	ctx, mbClient, endpoint, info, err := d.prepareMoveBundleCall(ctx, agent)
	if err != nil {
		return nil, err
	}
	req := &api.MoveBundleExportRequest{
		ReincarnationID:   reincarnationID,
		AgentID:           agent.ID,
		ProjectID:         agent.ProjectID,
		ProjectPath:       info.projectPath,
		ProjectSlug:       info.projectSlug,
		SecretFileTargets: secretFileTargets,
		Limits:            limits,
	}
	return mbClient.StartMoveBundleExport(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, req)
}

// DispatchMoveBundleExportStatus implements agentMoveDispatcher.
func (d *HTTPAgentDispatcher) DispatchMoveBundleExportStatus(ctx context.Context, agent *store.Agent, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	if reincarnationID == "" {
		return nil, errors.New("DispatchMoveBundleExportStatus: reincarnationID is required")
	}
	ctx, mbClient, endpoint, _, err := d.prepareMoveBundleCall(ctx, agent)
	if err != nil {
		return nil, err
	}
	return mbClient.GetMoveBundleExportStatus(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, reincarnationID)
}

// DispatchMoveBundleImport implements agentMoveDispatcher.
func (d *HTTPAgentDispatcher) DispatchMoveBundleImport(ctx context.Context, agent *store.Agent, reincarnationID string, manifest bundle.Manifest, manifestHash string, limits api.MoveBundleLimits) (*api.MoveBundleJobStatus, error) {
	if reincarnationID == "" {
		return nil, errors.New("DispatchMoveBundleImport: reincarnationID is required")
	}
	ctx, mbClient, endpoint, info, err := d.prepareMoveBundleCall(ctx, agent)
	if err != nil {
		return nil, err
	}
	req := &api.MoveBundleImportRequest{
		ReincarnationID: reincarnationID,
		AgentID:         agent.ID,
		ProjectID:       agent.ProjectID,
		ProjectPath:     info.projectPath,
		ProjectSlug:     info.projectSlug,
		Manifest:        manifest,
		ManifestHash:    manifestHash,
		Limits:          limits,
	}
	return mbClient.StartMoveBundleImport(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, req)
}

// DispatchMoveBundleImportStatus implements agentMoveDispatcher.
func (d *HTTPAgentDispatcher) DispatchMoveBundleImportStatus(ctx context.Context, agent *store.Agent, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	if reincarnationID == "" {
		return nil, errors.New("DispatchMoveBundleImportStatus: reincarnationID is required")
	}
	ctx, mbClient, endpoint, _, err := d.prepareMoveBundleCall(ctx, agent)
	if err != nil {
		return nil, err
	}
	return mbClient.GetMoveBundleImportStatus(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, reincarnationID)
}

// DispatchAgentProvisionForMove implements agentMoveDispatcher.
func (d *HTTPAgentDispatcher) DispatchAgentProvisionForMove(ctx context.Context, agent *store.Agent, expectNFSWorkspace, moveReincarnationID string) error {
	if expectNFSWorkspace == "" {
		return errors.New("DispatchAgentProvisionForMove: the expected NFS workspace is required")
	}
	// A broker without AgentMove would ignore the expected workspace and
	// provision an empty one, and one without AgentMoveRecreate is not
	// known to confirm that it has no files for a forced move's agent;
	// nothing is sent to either.
	if err := d.requireMoveCapability(ctx, agent, expectNFSWorkspace == agentpkg.MoveWorkspaceFreshAgentDir); err != nil {
		return err
	}
	if (moveReincarnationID != "") != (expectNFSWorkspace == agentpkg.MoveWorkspaceFreshAgentDir) {
		return errors.New("DispatchAgentProvisionForMove: the move reincarnation ID goes with, and only with, the fresh-agent-dir expectation")
	}
	return d.dispatchProvision(ctx, agent, "DispatchAgentProvisionForMove", false, expectNFSWorkspace, moveReincarnationID, ReprovisionOptions{})
}

// DispatchAgentDeleteLocalOnly implements agentMoveDispatcher.
func (d *HTTPAgentDispatcher) DispatchAgentDeleteLocalOnly(ctx context.Context, agent *store.Agent) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}
	// Flat placement backstop. Pinned agents never move
	// (reincarnate_move.go refuses them upstream); this is defence in
	// depth for a corrupted pin.
	if err := d.placementBackstop(ctx, agent); err != nil {
		return err
	}
	if err := d.requireMoveCapability(ctx, agent, false); err != nil {
		return err
	}
	ctx = withRecordedRuntime(ctx, agent.Runtime)
	ctx = withDispatchAgentID(ctx, agent.ID)
	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}
	// As DispatchAgentDelete: a linked project's path lets the broker find
	// a file-only agent (container gone).
	if info, _ := d.resolveDispatchProjectInfo(ctx, agent); info.projectPath != "" {
		ctx = withDeleteProjectPath(ctx, info.projectPath)
	}
	err = d.client.DeleteAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, DeleteAgentOptions{
		DeleteFiles: true,
		LocalOnly:   true,
		RunID:       agent.RunID,
	})
	if errors.Is(err, ErrLifecycleDeferred) {
		return fmt.Errorf("localOnly delete not supported for a cross-node broker: %w", err)
	}
	if errors.Is(err, ErrDeleteRunMismatch) {
		// The run this cleanup names has no entry on the broker; another
		// run holds the name there (for example the moved agent's new run,
		// seen through a runtime namespace shared with the target). That
		// run is left alone, and no row is finalized by this cleanup, so
		// it succeeds as for any delete 404 (ptone/scion#3080).
		d.log.Info("Dispatcher: localOnly delete found another run holding the name; nothing to remove",
			"agent_id", agent.ID, "agent", agent.Slug, "broker_id", agent.RuntimeBrokerID, "error", err)
		return nil
	}
	return err
}

// DispatchAgentDeleteRecreated implements agentMoveDispatcher.
func (d *HTTPAgentDispatcher) DispatchAgentDeleteRecreated(ctx context.Context, agent *store.Agent, moveReincarnationID string) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}
	if moveReincarnationID == "" {
		return errors.New("DispatchAgentDeleteRecreated: the move reincarnation ID is required")
	}
	// Flat placement backstop, as for the localOnly delete.
	if err := d.placementBackstop(ctx, agent); err != nil {
		return err
	}
	if err := d.requireMoveFreshTargetCapability(ctx, agent); err != nil {
		return err
	}
	ctx = withRecordedRuntime(ctx, agent.Runtime)
	ctx = withDispatchAgentID(ctx, agent.ID)
	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}
	if info, _ := d.resolveDispatchProjectInfo(ctx, agent); info.projectPath != "" {
		ctx = withDeleteProjectPath(ctx, info.projectPath)
	}
	// Project-scoped always; run-scoped when the target recorded a run.
	// Without a run the broker resolves the agent by name within the
	// project, which on this broker can only be the agent the move created.
	err = d.client.DeleteAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, DeleteAgentOptions{
		DeleteFiles:         true,
		RemoveBranch:        false,
		RunID:               agent.RunID,
		MoveReincarnationID: moveReincarnationID,
	})
	if errors.Is(err, ErrLifecycleDeferred) {
		return fmt.Errorf("forced move rollback delete not supported for a cross-node broker: %w", err)
	}
	return err
}

// moveTargetUntouched reports whether a forced move's provision failed
// before the target created anything for the agent. That is every provision
// error not marked errProvisionSent (it was returned before the request was
// sent: a move-capability refusal, a hub-side check, a deferred route), and
// a sent one that is the target's own refusal, a 409 whose broker error
// code is exactly api.BrokerErrCodeMoveTargetNotFresh (it already has files
// for the agent, or cannot confirm it has none). A rollback then removes
// nothing on the target, whose files (if any) the move did not create. The
// default is untouched, so a provision error that is not explicitly marked
// sent never causes a target delete.
func moveTargetUntouched(err error) bool {
	if !errors.Is(err, errProvisionSent) {
		return true
	}
	var se *brokerStatusError
	return errors.As(err, &se) && se != nil && se.StatusCode == http.StatusConflict &&
		se.brokerErrorCode() == api.BrokerErrCodeMoveTargetNotFresh
}

// isMoveMarkerMismatch reports whether err is the broker's refusal of a
// forced move's rollback delete because the agent's files there were not
// created by the move (a 409 with exactly api.BrokerErrCodeMoveMarkerMismatch).
func isMoveMarkerMismatch(err error) bool {
	var se *brokerStatusError
	return errors.As(err, &se) && se != nil && se.StatusCode == http.StatusConflict &&
		se.brokerErrorCode() == api.BrokerErrCodeMoveMarkerMismatch
}

// errReprovisionHomeIntact marks a failed home-resetting reprovision that
// certainly did not reset the agent's home (ptone/scion#7757).
var errReprovisionHomeIntact = errors.New("the agent home was not reset")

// reprovisionLeftHomeIntact reports whether err, from a home-resetting
// reprovision dispatch, certainly left the agent's home intact: it failed
// on the hub before the request was sent, the broker answered with an
// error that says the home is intact (details
// api.BrokerErrDetailHomeReset = api.BrokerHomeResetIntact, which the
// broker sets on every error answer to such a request until it has moved
// the home), or the broker never reprovisioned (env still missing, or no
// home reset confirmed). Any other failure, a timeout or an answer without
// the detail included, may have reset the home and is treated as having
// done so: a re-render of the previous config cannot bring the old home
// back anyway.
func reprovisionLeftHomeIntact(err error) bool {
	if err == nil {
		return false
	}
	if !errors.Is(err, errProvisionSent) || errors.Is(err, errReprovisionHomeIntact) {
		return true
	}
	var se *brokerStatusError
	return errors.As(err, &se) && se != nil && se.brokerHomeResetDetail() == api.BrokerHomeResetIntact
}

// brokerHomeResetDetail returns the api.BrokerErrDetailHomeReset detail of
// a broker JSON error body, or "" when there is none.
func (e *brokerStatusError) brokerHomeResetDetail() string {
	var body struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(e.Body), &body); err != nil {
		return ""
	}
	v, _ := body.Error.Details[api.BrokerErrDetailHomeReset].(string)
	return v
}
