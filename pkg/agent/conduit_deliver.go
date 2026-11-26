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

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Message delivery over the agent's conduit session (design v2.22,
// .design/conduit/v2.22-deliver-message.md). The broker stays the delivery
// orchestrator (debounce, lock, harness settings, retries, partial
// handling); only the transport of one flush changes, from runtime exec to
// the deliver-message control route, called through the hub (broker → hub
// → agent).

// LabelConduitDeliver is set to "true" on an agent launched with
// hub.conduit_deliver in its SCION_HUB_EXPERIMENTS: its sciontool registers
// deliver-message (gap G2 of the v2.22 freeze). An agent without it never
// causes a conduit delivery attempt. Cloud Run stores it under its
// GCP-sanitised key, labelConduitDeliverGCP.
const (
	LabelConduitDeliver    = "scion.conduit_deliver"
	labelConduitDeliverGCP = "scion_conduit_deliver"
)

// experimentConduitDeliver is experiments.ConduitDeliver (pkg/agent does
// not import the registry).
const experimentConduitDeliver = "hub.conduit_deliver"

// conduitDeliverRuntimes are the runtimes whose agents may deliver over
// conduit in Phase A: Cloud Run, where it removes the IAP exec dependency.
// Docker and Kubernetes keep exec (design v2.22 §7; opt-in per broker is
// later work). A variable so tests can add a runtime.
var conduitDeliverRuntimes = map[string]bool{"cloudrun": true}

// conduitNotReachedAttempts and conduitRetryBackoff bound the retries of a
// conduit delivery that provably did not reach the agent (design v2.22
// §2.4), before the exec fallback.
var (
	conduitNotReachedAttempts = 3
	conduitRetryBackoff       = 500 * time.Millisecond
)

// launchedWithConduitDeliver reports whether env (KEY=VALUE entries) lists
// hub.conduit_deliver in SCION_HUB_EXPERIMENTS.
func launchedWithConduitDeliver(env []string) bool {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k != "SCION_HUB_EXPERIMENTS" {
			continue
		}
		for _, name := range strings.Split(v, ",") {
			if strings.TrimSpace(name) == experimentConduitDeliver {
				return true
			}
		}
	}
	return false
}

// agentLaunchedWithConduitDeliver reports whether a carries
// LabelConduitDeliver.
func agentLaunchedWithConduitDeliver(a *api.AgentInfo) bool {
	return a.Labels[LabelConduitDeliver] == "true" || a.Labels[labelConduitDeliverGCP] == "true"
}

// ErrConduitNotSent wraps a ConduitDeliverer error from before the request
// was sent (no hub connection, connection refused): nothing reached the
// agent, so the delivery may be retried.
var ErrConduitNotSent = errors.New("conduit delivery: request not sent")

// ErrConduitUnavailable wraps a ConduitDeliverer error meaning conduit
// delivery cannot be used for this agent at all (for example a broker with
// several hub connections): the delivery goes over exec at once.
var ErrConduitUnavailable = errors.New("conduit delivery: unavailable")

// ConduitDeliverer asks the hub to run one deliver-message flush on an
// agent (POST /api/v1/runtime-brokers/{brokerId}/agents/{agentId}/deliver).
// It returns the hub's status and decoded answer. A transport error is
// returned as err with status 0; it wraps ErrConduitNotSent only when the
// request was provably not sent, and is otherwise ambiguous.
type ConduitDeliverer interface {
	DeliverMessage(ctx context.Context, agentID string, req *agentcontrol.DeliverRequest) (int, agentcontrol.DeliverResponse, error)
}

// SetConduitDeliverer sets the hub caller used for conduit delivery; nil
// turns conduit delivery off. The Runtime Broker sets it.
func (m *AgentManager) SetConduitDeliverer(d ConduitDeliverer) {
	m.conduitMu.Lock()
	m.conduit = d
	m.conduitMu.Unlock()
}

func (m *AgentManager) conduitDeliverer() ConduitDeliverer {
	m.conduitMu.Lock()
	defer m.conduitMu.Unlock()
	return m.conduit
}

// usesConduitTransport reports whether deliveries to target are tried over
// conduit: a deliverer is set, the runtime is one conduitDeliverRuntimes
// lists, and the agent was launched with the flag. The hub decides the
// rest (experiment on, route advertised) per call.
func (m *AgentManager) usesConduitTransport(target *api.AgentInfo) bool {
	if m.conduitDeliverer() == nil || m.Runtime == nil || !conduitDeliverRuntimes[m.Runtime.Name()] {
		return false
	}
	return target.ID != "" && agentLaunchedWithConduitDeliver(target)
}

// nextDeliveryID returns a delivery id for one flush: unique across
// processes (the per-process nonce) and within one (the counter). Every
// attempt of the flush reuses it.
func nextDeliveryID() string {
	return fmt.Sprintf("%s-%d", msgBufferNonce(), msgBufferSeq.Add(1))
}

// ErrAwaitingSession reports that a conduit delivery failed after its bounded
// retries because the hub answered 503 not_reached with reason=no_session on
// the final attempt and runtime exec is unavailable (ptone/scion#7881).
var ErrAwaitingSession = errors.New("agent conduit session not connected")

// conduitOutcome is how a conduit attempt ended for the caller.
type conduitOutcome int

const (
	conduitDone       conduitOutcome = iota // delivered, or an error to report as is
	conduitUseExec                          // nothing pasted: deliver over exec instead
	conduitNotReached                       // nothing pasted: may retry
)

// deliverOverConduit runs one flush over conduit with the bounded
// not-reached retries. It returns conduitUseExec (with the reason) when the
// flush must go over exec instead; otherwise the delivery's final error
// (nil when delivered), already typed for the message buffer: a
// PartialDeliveryError when something may have been pasted, a
// finalDeliveryError for a failure that must not be retried.
func (m *AgentManager) deliverOverConduit(ctx context.Context, target *api.AgentInfo, req *agentcontrol.DeliverRequest) (conduitOutcome, error) {
	d := m.conduitDeliverer()
	if d == nil {
		// usesConduitTransport checked for a deliverer, but under its own
		// lock: SetConduitDeliverer(nil) (conduit delivery turned off) can
		// land in between. Nothing was sent; deliver over exec instead.
		return conduitUseExec, fmt.Errorf("%w: no deliverer", ErrConduitUnavailable)
	}
	var lastErr error
	for attempt := 1; attempt <= conduitNotReachedAttempts; attempt++ {
		dctx, cancel := context.WithTimeout(ctx, agentcontrol.BrokerDeliverTimeout)
		status, resp, err := d.DeliverMessage(dctx, target.ID, req)
		cancel()
		outcome, oerr := classifyConduitAnswer(status, resp, err)
		slog.Info("message delivery over conduit",
			"agent_id", target.ID, "delivery_id", req.DeliveryID, "op", req.Op,
			"messages", len(req.MessageIDs), "attempt", attempt, "status", status,
			"outcome", resp.Outcome, "transport_error", err != nil)
		switch outcome {
		case conduitDone, conduitUseExec:
			return outcome, oerr
		}
		lastErr = oerr
		if attempt < conduitNotReachedAttempts {
			t := time.NewTimer(conduitRetryBackoff)
			select {
			case <-ctx.Done():
				t.Stop()
				return conduitUseExec, ctx.Err()
			case <-t.C:
			}
		}
	}
	return conduitUseExec, lastErr
}

// classifyConduitAnswer maps one /deliver answer (design v2.22 §1.3, §2.4).
func classifyConduitAnswer(status int, resp agentcontrol.DeliverResponse, err error) (conduitOutcome, error) {
	if err != nil {
		if errors.Is(err, ErrConduitUnavailable) {
			return conduitUseExec, err
		}
		if errors.Is(err, ErrConduitNotSent) {
			return conduitNotReached, err
		}
		// A timeout or reset after send is ambiguous: never retried.
		return conduitDone, &PartialDeliveryError{Err: fmt.Errorf("conduit delivery outcome unknown: %w", err)}
	}
	switch {
	case status == http.StatusOK && resp.Outcome == agentcontrol.OutcomeDelivered:
		return conduitDone, nil
	case status == http.StatusOK && resp.Outcome == agentcontrol.OutcomePartial:
		return conduitDone, &PartialDeliveryError{Err: fmt.Errorf("partial delivery over conduit (stage %s)", resp.Stage)}
	case status == http.StatusConflict && resp.Outcome == agentcontrol.OutcomeInFlight:
		// The hub refused a duplicate of a delivery another call owns. That
		// owner pastes (or not) and reports the outcome; this call must not
		// fall back to exec, retry, or report a failure of the owner's
		// rows (review FV1-N1). deliver turns this into success.
		return conduitDone, errDeliveryInFlight
	case status == http.StatusUnprocessableEntity && resp.Outcome == agentcontrol.OutcomeOutOfBounds:
		// The agent image's own readiness spec is outside the route
		// bounds; nothing was done. Exec applies the spec in full
		// (review P2-R1).
		return conduitUseExec, fmt.Errorf("agent readiness spec outside conduit route bounds: %s", resp.Field)
	case status == http.StatusConflict && resp.Outcome == agentcontrol.OutcomeInProgress:
		return conduitDone, &PartialDeliveryError{Err: errors.New("delivery over conduit still in progress on the agent")}
	case status == http.StatusConflict && resp.Outcome == agentcontrol.OutcomeBusy:
		return conduitNotReached, errors.New("agent busy with another delivery")
	case status == http.StatusConflict && resp.Outcome == agentcontrol.OutcomeBound:
		return conduitDone, &finalDeliveryError{Err: errors.New("delivery refused by the hub: messages bound to another delivery")}
	case status == http.StatusRequestTimeout:
		return conduitDone, &finalDeliveryError{Err: errors.New("expired before paste")}
	case status == http.StatusBadRequest:
		return conduitDone, &finalDeliveryError{Err: fmt.Errorf("delivery refused by the agent: invalid %s", resp.Field)}
	case status == http.StatusNotFound, status == http.StatusRequestEntityTooLarge:
		return conduitUseExec, fmt.Errorf("conduit delivery not available (status %d)", status)
	case status == http.StatusServiceUnavailable && resp.Outcome == "not_reached" && resp.Reason == agentcontrol.ReasonNoSession:
		return conduitNotReached, fmt.Errorf("conduit delivery not reached (status %d, reason %s): %w", status, resp.Reason, ErrAwaitingSession)
	case status == http.StatusUnprocessableEntity, status == http.StatusTooManyRequests,
		status == http.StatusNotImplemented, status == http.StatusServiceUnavailable:
		return conduitNotReached, fmt.Errorf("conduit delivery not reached (status %d)", status)
	default:
		// Anything else (a hub timeout after send, an unexpected status)
		// may have pasted: never retried.
		return conduitDone, &PartialDeliveryError{Err: fmt.Errorf("conduit delivery outcome unknown (status %d)", status)}
	}
}

// conduitDeliverRequest builds the route request for one flush from the
// shared steps' request. ready, when not nil, asks sciontool to wait for
// readiness (op=paste only).
func conduitDeliverRequest(deliveryID string, messageIDs []string, req tmuxdeliver.Request, ready *agentcontrol.ReadySpec) *agentcontrol.DeliverRequest {
	out := &agentcontrol.DeliverRequest{
		V:             agentcontrol.DeliverVersion,
		DeliveryID:    deliveryID,
		MessageIDs:    messageIDs,
		Op:            string(req.Op),
		InterruptKeys: req.InterruptKeys,
		DeadlineMS:    agentcontrol.MaxDeliverDeadline.Milliseconds(),
		SubmitKeys:    req.SubmitKeys,
	}
	if req.Text != "" {
		text := req.Text
		out.Text = &text
		out.ExtraSubmits = &agentcontrol.ExtraSubmits{Count: req.ExtraSubmits, DelayMS: int(req.ExtraSubmitDelay.Milliseconds())}
	}
	if req.Op == tmuxdeliver.OpPaste {
		out.Ready = ready
	}
	return out
}

type deliveryMessageIDKey struct{}

// WithDeliveryMessageID returns a context carrying the hub message id of the
// message passed to AgentManager.Message. It lets the flush that carries the
// message go over conduit, where the hub binds the delivery to its rows.
func WithDeliveryMessageID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, deliveryMessageIDKey{}, id)
}

func deliveryMessageIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(deliveryMessageIDKey{}).(string)
	return id
}

type resumeSuffixKey struct{}

// WithResumeSuffix returns a context carrying a wake-on-message resume
// instruction to append after the waking message(s) in the same delivery.
func WithResumeSuffix(ctx context.Context, suffix string) context.Context {
	if suffix == "" {
		return ctx
	}
	return context.WithValue(ctx, resumeSuffixKey{}, suffix)
}

// ResumeSuffixFromContext returns the wake resume suffix attached with
// WithResumeSuffix, or "".
func ResumeSuffixFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(resumeSuffixKey{}).(string)
	return s
}

// conduitSettings are the resolved harness delivery and readiness settings
// the route bounds are checked against before a conduit delivery (design
// v2.22 §1.7): no value is clamped; a setting outside the bounds sends the
// delivery over exec.
func conduitSettings(h api.Harness, ds api.DeliverySpec) tmuxdeliver.Settings {
	interrupt := h.GetInterruptSequence()
	if len(interrupt) == 0 {
		interrupt = []string{h.GetInterruptKey()}
	}
	rs := effectiveReadySpec(h)
	return tmuxdeliver.Settings{
		SubmitKeys:       ds.SubmitKeys,
		InterruptKeys:    interrupt,
		ExtraSubmits:     ds.ExtraSubmits,
		ExtraSubmitDelay: ds.ExtraSubmitDelay,
		ReadyStrategy:    rs.Strategy,
		ReadySettle:      rs.Settle,
		ReadyTimeout:     rs.Timeout,
	}
}

// effectiveReadySpec is the harness's resolved readiness spec, the one the
// agent was launched with (it reaches the container as the init-run spec):
// what the route bounds are checked against (review P2-R1). A harness
// without one gets the hook defaults.
func effectiveReadySpec(h api.Harness) api.ReadySpec {
	rs := h.ReadySpec()
	if rs.Strategy == "" {
		rs = state.DefaultReadySpec(state.ReadyStrategyHook)
	}
	return rs
}

// conduitReadySpec is the readiness block of a conduit paste: the
// harness's resolved spec, which sciontool uses when the image writes no
// ready spec of its own. When the image does, sciontool checks that one
// against the bounds too and refuses rather than shortens it.
func conduitReadySpec(h api.Harness) *agentcontrol.ReadySpec {
	rs := effectiveReadySpec(h)
	return &agentcontrol.ReadySpec{
		Strategy:  rs.Strategy,
		SettleMS:  int(rs.Settle.Milliseconds()),
		TimeoutMS: int(rs.Timeout.Milliseconds()),
	}
}

// execAvailable reports whether the runtime can exec at all, from its
// static capability (runtime.ExecCapability), without running anything
// (review FV2-N1). A missing or starting tmux session is not "no exec":
// that is left to the readiness wait and the pre-paste retries, and a
// transient exec error is an ordinary delivery error.
func (m *AgentManager) execAvailable() bool {
	if c, ok := m.Runtime.(runtime.ExecCapability); ok {
		return c.ExecAvailable()
	}
	return true
}

// errDeliveryInFlight: the hub refused a duplicate of a delivery another
// call owns (409 in_flight). deliver turns it into success; a split flush
// stops at it.
var errDeliveryInFlight = errors.New("delivery in flight under another call")

// splitIDPlaceholder stands in for a delivery id when sizing a request: as
// long as the longest id, so a request that fits with it fits with any.
var splitIDPlaceholder = strings.Repeat("x", 64)

// fitsOneConduitDelivery reports whether the whole delivery fits one
// conduit request (text, encoded size, id count). Only a flush whose every
// message has a hub id is split; others keep their path.
func (m *AgentManager) fitsOneConduitDelivery(h api.Harness, d deliveryRequest, req tmuxdeliver.Request) bool {
	if d.partialIDs || len(d.messageIDs) == 0 {
		return true
	}
	return conduitDeliverRequest(splitIDPlaceholder, d.messageIDs, req, m.readyBlock(h, d.readyWanted)).FitsRoute() == nil
}

// readyBlock is the request's readiness block when the delivery wants one.
func (m *AgentManager) readyBlock(h api.Harness, wanted bool) *agentcontrol.ReadySpec {
	if !wanted {
		return nil
	}
	return conduitReadySpec(h)
}

// splitParts groups parts, in order, into sub-flushes that each fit one
// conduit request (ptone Q3, amends design v2.22 §1.6). A message is never
// split: one too large on its own is a sub-flush by itself, which then
// takes the usual oversize path (exec where the runtime has it, otherwise
// a named failure). It returns each group's first part index.
func splitParts(parts []deliveryPart, fits func(text string, ids []string) bool) [][2]int {
	var groups [][2]int // [start, end)
	start := 0
	for i := range parts {
		if i == start {
			continue
		}
		text, ids := joinParts(parts[start : i+1])
		if fits(text, ids) {
			continue
		}
		groups = append(groups, [2]int{start, i})
		start = i
	}
	if start < len(parts) {
		groups = append(groups, [2]int{start, len(parts)})
	}
	return groups
}

// joinParts is the coalesced text and the ids of parts, joined as the
// buffer joins a flush.
func joinParts(parts []deliveryPart) (string, []string) {
	texts := make([]string, len(parts))
	ids := make([]string, len(parts))
	for i, p := range parts {
		texts[i], ids[i] = p.text, p.id
	}
	return strings.Join(texts, "\n\n"), ids
}

// deliverSplit delivers an oversize buffered flush as ordered sub-flushes,
// each with its own delivery id and its own message rows, under the lock
// the caller holds. It stops at the first sub-flush that is not delivered
// (a failure, a partial, or one in flight under another call): no later
// sub-flush is sent, and the error reports the messages from that
// sub-flush on as failed (the in-flight one excluded; its owner reports
// it). Each sub-flush keeps the usual at-most-once rules.
func (m *AgentManager) deliverSplit(ctx context.Context, agent *api.AgentInfo, h api.Harness, ds api.DeliverySpec, d deliveryRequest) error {
	base := tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, SubmitKeys: ds.SubmitKeys,
		ExtraSubmits: ds.ExtraSubmits, ExtraSubmitDelay: ds.ExtraSubmitDelay}
	fits := func(text string, ids []string) bool {
		r := base
		r.Text = text
		return conduitDeliverRequest(splitIDPlaceholder, ids, r, m.readyBlock(h, d.readyWanted)).FitsRoute() == nil
	}
	groups := splitParts(d.parts, fits)
	slog.Info("message delivery split for conduit", "agent_id", agent.ID, "messages", len(d.parts), "sub_flushes", len(groups))
	for i, g := range groups {
		text, ids := joinParts(d.parts[g[0]:g[1]])
		sub := d
		sub.message, sub.messageIDs, sub.parts = text, ids, nil
		sub.onSuccess = make([]DeliverySuccessHandler, 0, g[1]-g[0])
		for _, p := range d.parts[g[0]:g[1]] {
			if p.onSuccess != nil {
				sub.onSuccess = append(sub.onSuccess, p.onSuccess)
			}
		}
		sub.readyWanted = d.readyWanted && i == 0
		err := m.deliverResolved(ctx, agent, h, ds, sub)
		switch {
		case err == nil:
			continue
		case errors.Is(err, errDeliveryInFlight):
			return &splitDeliveryError{failedFrom: g[1], err: fmt.Errorf("split delivery stopped: sub-flush %d of %d in flight under another call", i+1, len(groups))}
		case i == 0 && len(groups) == 1:
			return err // single sub-flush: the flush's own error
		case i == 0 && errors.Is(err, ErrAwaitingSession):
			return &splitDeliveryError{
				failedFrom: 0,
				err:        err,
				tailFrom:   g[1],
				tailErr:    fmt.Errorf("split delivery stopped before sub-flush 2 of %d: earlier sub-flush awaiting session", len(groups)),
			}
		case i == 0:
			return err // nothing delivered yet: the flush's own error
		default:
			se := &splitDeliveryError{
				failedFrom: g[0],
				err:        fmt.Errorf("split delivery stopped at sub-flush %d of %d: %w", i+1, len(groups), err),
			}
			if i+1 < len(groups) && errors.Is(err, ErrAwaitingSession) {
				se.tailFrom = g[1]
				se.tailErr = fmt.Errorf("split delivery stopped before sub-flush %d of %d: earlier sub-flush awaiting session", i+2, len(groups))
			}
			return se
		}
	}
	return nil
}
