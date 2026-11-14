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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

// conduitSettings are the resolved harness delivery and readiness settings
// the route bounds are checked against before a conduit delivery (design
// v2.22 §1.7): no value is clamped; a setting outside the bounds sends the
// delivery over exec.
func conduitSettings(h api.Harness, ds api.DeliverySpec, timings harnessReadyTimings) tmuxdeliver.Settings {
	interrupt := h.GetInterruptSequence()
	if len(interrupt) == 0 {
		interrupt = []string{h.GetInterruptKey()}
	}
	t := timings.withDefaults()
	return tmuxdeliver.Settings{
		SubmitKeys:       ds.SubmitKeys,
		InterruptKeys:    interrupt,
		ExtraSubmits:     ds.ExtraSubmits,
		ExtraSubmitDelay: ds.ExtraSubmitDelay,
		ReadyStrategy:    "hook",
		ReadySettle:      t.Settle,
		ReadyTimeout:     t.Deadline,
	}
}

// conduitReadySpec is the readiness block of a conduit paste: the broker's
// own wait timings, which sciontool uses when the image writes no ready
// spec of its own (the image's spec replaces them, as on the exec path).
func conduitReadySpec(timings harnessReadyTimings) *agentcontrol.ReadySpec {
	t := timings.withDefaults()
	return &agentcontrol.ReadySpec{
		Strategy:  "hook",
		SettleMS:  int(t.Settle.Milliseconds()),
		TimeoutMS: int(t.Deadline.Milliseconds()),
	}
}
