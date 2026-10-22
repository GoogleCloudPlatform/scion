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
	"net/http"

	agentpkg "github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
	return d.dispatchProvision(ctx, agent, "DispatchAgentProvisionForMove", false, expectNFSWorkspace, moveReincarnationID)
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
	if err := d.requireMoveCapability(ctx, agent, true); err != nil {
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
