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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// One evaluation, three entry points (design v2.21.40 §3.9).
//
// evaluateConduitStream decides whether a user stream's principal still
// holds the authorization the stream was opened with, against current
// state. It takes no grant: the renewal entry points verify the presented
// grant first (renewConduitStream) and pass in what its signed claims say,
// and the in-process re-check passes in what the node recorded at open.
// Every entry point acts on the returned class and adds no decision of its
// own:
//
//   - hub-held renewal: renewConduitStream (verify, evaluate, mint);
//   - relay-held renewal: recheckRelayStream, an adapter over
//     renewConduitStream;
//   - in-process re-check: checkConduitUserStream;
//   - legacy port proxy request: checkLegacyPortRequest, at the start of
//     each user request the port-forward tunnel serves (the in-process
//     re-check covers the request while it is open).

// conduitStreamEval is the input to evaluateConduitStream.
type conduitStreamEval struct {
	// AgentID is the agent the stream reaches.
	AgentID string
	// ProjectID is the project the stream was authorized in. The agent's
	// current project must equal it.
	ProjectID string
	// Identity returns the principal to evaluate. It is called after the
	// agent lookup and the project compare. An error wrapping
	// errConduitPrincipalInactive is a deny; any other error is
	// unavailable.
	Identity func() (Identity, error)
	// Kind is the grant stream kind.
	Kind string
	// ViaTunnel marks a stream opened through the tunnel path.
	ViaTunnel bool
	// Port is the agent-local port of a tcp stream.
	Port int
	// ConfirmDeny, if set, is asked before an action deny is reported;
	// false makes the decision unavailable instead.
	ConfirmDeny func(agent *store.Agent) bool
	// LogAttrs are added to the evaluation's log lines.
	LogAttrs []any
}

// evaluateConduitStream evaluates in and returns the class, a short reason
// for logs and metrics, and the cause. The cause is in the renewal error
// vocabulary, so ClassifyConduitRenewal of it agrees with the class:
//
//   - conduitAuthzAllowed, nil;
//   - conduitAuthzTargetGone, errConduitAgentGone: the agent is gone;
//   - conduitAuthzDenied, an error wrapping errConduitForbidden (the
//     agent moved to another project, or an action is no longer held) or
//     errConduitPrincipalInactive;
//   - conduitAuthzUnavailable, an error wrapping errConduitAuthzUnavailable
//     (a lookup failed) or errConduitRenewalUnavailable (the agent's saved
//     placement refuses it).
func (s *Server) evaluateConduitStream(ctx context.Context, in conduitStreamEval) (conduitAuthzVerdict, string, error) {
	agent, err := s.store.GetAgent(ctx, in.AgentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return conduitAuthzTargetGone, "agent not found", errConduitAgentGone
	case err != nil:
		return conduitAuthzUnavailable, "agent lookup failed", fmt.Errorf("%w: agent lookup: %v", errConduitAuthzUnavailable, err)
	case agent == nil:
		// The store returned neither an agent nor an error: an internal
		// inconsistency, not a deleted agent and not a decision.
		return conduitAuthzUnavailable, "agent lookup failed", fmt.Errorf("%w: agent lookup returned no agent", errConduitAuthzUnavailable)
	case !agent.DeletedAt.IsZero():
		return conduitAuthzTargetGone, "agent deleted", errConduitAgentGone
	case agent.ProjectID != in.ProjectID:
		return conduitAuthzDenied, "agent moved to another project", fmt.Errorf("%w: agent is no longer in the grant's project", errConduitForbidden)
	}
	if in.Identity == nil {
		// No way to resolve the principal: fail closed as unavailable, never
		// as an allow, so nothing is renewed and nothing closes early.
		return conduitAuthzUnavailable, "principal lookup failed", fmt.Errorf("%w: no principal resolver", errConduitAuthzUnavailable)
	}
	ident, err := in.Identity()
	switch {
	case errors.Is(err, errConduitPrincipalInactive):
		return conduitAuthzDenied, "principal not active", err
	case err != nil:
		return conduitAuthzUnavailable, "principal lookup failed", err
	}
	var confirm func() bool
	if in.ConfirmDeny != nil {
		confirm = func() bool { return in.ConfirmDeny(agent) }
	}
	verdict, reason, err := s.authorizeConduitUserStream(ctx, conduitUserStreamAuthz{
		Identity:    ident,
		Agent:       agent,
		Kind:        in.Kind,
		ViaTunnel:   in.ViaTunnel,
		Port:        in.Port,
		ConfirmDeny: confirm,
	})
	switch verdict {
	case conduitAuthzAllowed:
	case conduitAuthzDenied:
		if errors.Is(err, errConduitPrincipalInactive) {
			return conduitAuthzDenied, reason, err
		}
		return conduitAuthzDenied, reason, fmt.Errorf("%w: %s: %w", errConduitForbidden, reason, err)
	default:
		return conduitAuthzUnavailable, reason, fmt.Errorf("%w: %s: %w", errConduitAuthzUnavailable, reason, err)
	}
	if reason, err := s.conduitStreamPlacement(ctx, agent, in.LogAttrs); err != nil {
		return conduitAuthzUnavailable, reason, err
	}
	return conduitAuthzAllowed, "", nil
}

// conduitStreamPlacement applies mintConduitGrant's placement guard
// (checkAgentPinnedPlacement) to a stream re-check, after the
// authorization verdict, so a stream to a pinned agent whose saved
// placement is inconsistent gets no renewal, as an open gets no grant. A
// placement refusal wraps errConduitRenewalUnavailable and the
// *PlacementConflictError: no renewal is minted, and the stream is not
// closed from here; it ends at its current deadline. A failed placement
// read for a pinned agent wraps errConduitAuthzUnavailable, like the other
// lookups that fail during a re-check, and errConduitRenewalUnavailable:
// no renewal this attempt, and nothing closes. As for an open, an unpinned
// agent keeps today's path when the read fails, and a managed runtime is
// not checked.
func (s *Server) conduitStreamPlacement(ctx context.Context, agent *store.Agent, logAttrs []any) (string, error) {
	if isManagedAgentRuntime(agent.Runtime) {
		return "", nil
	}
	st := store.Store(s.store)
	if m := conduitBrokerMemoFrom(ctx); m != nil {
		st = m.over(s.store)
	}
	_, err := checkAgentPinnedPlacement(ctx, st, agent)
	if err == nil {
		return "", nil
	}
	if r, refused := placementRefusalOf(err); refused {
		slog.Warn("Conduit stream re-check: the agent's saved placement refuses the stream; it is not renewed",
			append([]any{"agent_id", agent.ID, "code", r.Code}, logAttrs...)...)
		return "agent placement inconsistent", fmt.Errorf("%w: agent placement: %w", errConduitRenewalUnavailable, err)
	}
	if agent.IsPinned() {
		return "agent placement check failed", fmt.Errorf("%w: %w: agent placement check: %w", errConduitAuthzUnavailable, errConduitRenewalUnavailable, err)
	}
	return "", nil
}

// conduitConfirmAgentPresent confirms a deny before it is acted on (design
// v2.21.40 §3.9, Deny confirmation): it re-reads the agent and reports
// whether the store answered with the agent, present and not deleted, and
// the context is still live. A failed read, a read that returns no agent
// and no error, or a deleted agent does not confirm, so the deny becomes
// unavailable for this attempt; a deleted agent is reported as gone by the
// next evaluation.
func (s *Server) conduitConfirmAgentPresent(ctx context.Context, agentID string) bool {
	agent, err := s.store.GetAgent(ctx, agentID)
	return err == nil && agent != nil && agent.DeletedAt.IsZero() && ctx.Err() == nil
}

// recordedStreamEval is the evaluation input of a stream from what this
// node recorded at open: the principal, the agent and its project, the
// kind, origin and port. A deny is acted on only while the store answers
// for the agent (conduitConfirmAgentPresent).
func (s *Server) recordedStreamEval(ctx context.Context, st *conduitUserStream) conduitStreamEval {
	return conduitStreamEval{
		AgentID:     st.AgentID,
		ProjectID:   st.ProjectID,
		Identity:    func() (Identity, error) { return st.Identity, nil },
		Kind:        st.Kind,
		ViaTunnel:   st.ViaTunnel,
		Port:        st.Port,
		ConfirmDeny: func(agent *store.Agent) bool { return s.conduitConfirmAgentPresent(ctx, agent.ID) },
		LogAttrs:    []any{"session_id", st.SessionID, "stream_id", st.StreamID},
	}
}

// checkLegacyPortRequest evaluates a user request to an agent port that
// the port-forward tunnel serves, before it is forwarded, with the same
// evaluation as the in-process re-check of a conduit port stream (st is
// a tcp stream to the exposed port). It returns the class and the cause,
// which the proxy maps to its answer (writeLegacyPortRefusal).
func (s *Server) checkLegacyPortRequest(ctx context.Context, st *conduitUserStream) (conduitAuthzVerdict, error) {
	verdict, _, err := s.evaluateConduitStream(ctx, s.recordedStreamEval(ctx, st))
	return verdict, err
}

// conduitBrokerMemo shares Runtime Broker reads among the stream checks of
// one re-check pass (a sweep, or a resync). Only the broker row is shared:
// the placement guard (requirePinnedPlacement) still decides each stream
// from that stream's own agent row, read fresh by the evaluation, so every
// verdict matches the agent read it comes with.
//
// A read is shared only when it is a definite answer: the broker row, or
// store.ErrNotFound. Checks that wait on a read that fails get that
// failure for this pass (no read of their own); nothing is stored for it,
// so a later check of the pass reads again. A waiting check gives up when
// its own context ends. The shared rows are read-only: no caller may
// modify them. A memo lives for one pass and is then dropped; notify
// re-checks, interval checks, renewals and the legacy port proxy carry
// none and read fresh.
type conduitBrokerMemo struct {
	mu    sync.Mutex
	reads map[string]*conduitBrokerRead

	// brokerMemoWaitTestHook, when set, runs in a check that has joined
	// another check's read, before it waits. Only tests set it.
	brokerMemoWaitTestHook func()
}

// conduitBrokerRead is one shared broker read. done closes when it is
// finished; then ok reports whether it gave a definite answer (broker,
// err), and failed holds the error of a read that did not.
type conduitBrokerRead struct {
	done   chan struct{}
	ok     bool
	broker *store.RuntimeBroker
	err    error
	failed error
}

type conduitBrokerMemoKey struct{}

// withConduitBrokerMemo returns ctx with a new, empty broker memo: the
// context of one re-check pass.
func withConduitBrokerMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, conduitBrokerMemoKey{}, &conduitBrokerMemo{reads: map[string]*conduitBrokerRead{}})
}

// conduitBrokerMemoFrom returns ctx's broker memo, or nil.
func conduitBrokerMemoFrom(ctx context.Context) *conduitBrokerMemo {
	m, _ := ctx.Value(conduitBrokerMemoKey{}).(*conduitBrokerMemo)
	return m
}

// getRuntimeBroker returns the broker row for id, read through read at
// most once per pass while the read gives a definite answer.
func (m *conduitBrokerMemo) getRuntimeBroker(ctx context.Context, id string, read func(context.Context, string) (*store.RuntimeBroker, error)) (*store.RuntimeBroker, error) {
	m.mu.Lock()
	if r, ok := m.reads[id]; ok {
		hook := m.brokerMemoWaitTestHook
		m.mu.Unlock()
		if hook != nil {
			hook()
		}
		select {
		case <-r.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if !r.ok {
			return nil, r.failed
		}
		return r.broker, r.err
	}
	r := &conduitBrokerRead{done: make(chan struct{})}
	m.reads[id] = r
	m.mu.Unlock()

	broker, err := read(ctx, id)
	m.mu.Lock()
	if err == nil || errors.Is(err, store.ErrNotFound) {
		r.ok, r.broker, r.err = true, broker, err
	} else {
		r.failed = err
		delete(m.reads, id)
	}
	m.mu.Unlock()
	close(r.done)
	return broker, err
}

// over returns st with GetRuntimeBroker answered through the memo.
func (m *conduitBrokerMemo) over(st store.Store) store.Store {
	return conduitBrokerMemoStore{Store: st, memo: m}
}

// conduitBrokerMemoStore is a store whose broker reads go through a pass's
// memo; every other method is the wrapped store's.
type conduitBrokerMemoStore struct {
	store.Store
	memo *conduitBrokerMemo
}

func (s conduitBrokerMemoStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	return s.memo.getRuntimeBroker(ctx, id, s.Store.GetRuntimeBroker)
}
