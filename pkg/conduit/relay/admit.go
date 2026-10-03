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

package relay

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// admitter is the per-connection conduit.Admitter. Admission order matters
// for fencing: every refusal that does not need the registry (state,
// principal identity, incarnation policy, exec scope, grant keys) happens
// before InsertSessionWithNextEpoch, so a refused Hello writes no row and
// consumes no epoch.
type admitter struct {
	r         *Relay
	p         Principal
	transport string

	mu     sync.Mutex
	rec    registry.SessionRecord
	source string
	ok     bool
}

var (
	_ conduit.Admitter       = (*admitter)(nil)
	_ conduit.AdmitAbandoner = (*admitter)(nil)
)

func (a *admitter) Admit(ctx context.Context, hello *conduitv1.Hello) (*conduitv1.Welcome, error) {
	r, p := a.r, a.p
	gen, state := r.admissionState()
	switch state {
	case stateServing:
	case stateDraining:
		return nil, reject(conduit.CloseRelayRestart, ReasonDraining, "")
	default:
		return nil, reject(conduit.CloseRelayRestart, ReasonNotServing, "")
	}
	kind, err := conduit.PrincipalKindFromProto(hello.GetPrincipalKind())
	if err != nil {
		return nil, reject(conduit.CloseProtocolError, ReasonBadHello, "unknown principal kind")
	}
	switch {
	case string(kind) == registry.PrincipalRelayPeer || p.Kind == registry.PrincipalRelayPeer:
		// relay-peer is an internal-API identity only; it never holds a
		// target session (design §3.10).
		return nil, reject(conduit.CloseForbidden, ReasonForbidden, "relay-peer principals may not open target sessions")
	case string(kind) != p.Kind || hello.GetPrincipalId() != p.ID:
		return nil, reject(conduit.CloseForbidden, ReasonForbidden, "hello principal does not match the authenticated principal")
	}
	caps := hello.GetCapabilities()
	inc, err := admitIncarnation(p, caps.GetEndpointIncarnation())
	if err != nil {
		if IsSupersededIncarnation(err) {
			r.log.Warn("Conduit admission refused: superseded incarnation",
				"principal_kind", p.Kind, "principal_id", p.ID, "presented", caps.GetEndpointIncarnation())
		}
		return nil, err
	}
	fallback := p.Kind == registry.PrincipalAgent && inc.Source == IncarnationSourceGeneration
	if fallback {
		if err := CheckFallbackAgainstLaunchID(ctx, r.cfg.Store, p.ID, p.Agent, inc, ""); err != nil {
			return nil, a.fallbackRefused(err)
		}
	}
	if s := caps.GetExecScope(); s != "" && s != p.ExecScope {
		return nil, reject(conduit.CloseForbidden, ReasonForbidden, "hello exec_scope does not match the authoritative exec scope")
	}
	transport, err := registryTransport(a.transport)
	if err != nil {
		return nil, reject(conduit.CloseForbidden, ReasonForbidden, err.Error())
	}
	// Keys before the insert: a target that cannot verify grants is
	// useless, and failing here consumes no epoch.
	keys, err := r.cfg.GrantKeys(ctx)
	if err != nil {
		r.log.Warn("Conduit admission refused: grant keys unavailable", "error", err)
		return nil, reject(conduit.CloseRelayTimeout, ReasonGrantKeysUnavailable, "")
	}

	rec := registry.SessionRecord{
		SessionID:           r.cfg.NewSessionID(),
		PrincipalKind:       p.Kind,
		PrincipalID:         p.ID,
		ProjectID:           p.ProjectID,
		RelayInstanceID:     r.cfg.InstanceID,
		RelayGeneration:     gen,
		Transport:           transport,
		EndpointIncarnation: inc.Value,
		ExecScope:           p.ExecScope,
		Capabilities:        capabilitiesFromProto(caps, inc, p.ExecScope),
	}
	epoch, err := r.cfg.Registry.InsertSessionWithNextEpoch(ctx, rec)
	switch {
	case errors.Is(err, registry.ErrRelaySuperseded):
		r.supersede()
		return nil, reject(conduit.CloseRelayRestart, ReasonRelayRestart, "relay superseded")
	case errors.Is(err, registry.ErrInvalidInput):
		return nil, reject(conduit.CloseForbidden, ReasonForbidden, "session not admissible")
	case err != nil:
		r.log.Warn("Conduit admission failed: registry insert", "error", err)
		return nil, reject(conduit.CloseRelayTimeout, ReasonRegistryUnavailable, "")
	}
	rec.ConnectionEpoch = epoch
	if fallback {
		if err := a.recheckFallback(ctx, rec, inc); err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	a.rec, a.source, a.ok = rec, inc.Source, true
	a.mu.Unlock()
	if p.Kind != registry.PrincipalUser {
		r.log.Info("Conduit session admitted", "session_id", rec.SessionID, "principal_kind", p.Kind,
			"principal_id", p.ID, "connection_epoch", epoch, "incarnation_source", inc.Source)
	}

	// endpoint_incarnation is the ADMITTED value (a launch id, or gen-N
	// under the interim fallback), never the presented one: grants are
	// minted against it, so the target must verify against it (§3.2).
	w := &conduitv1.Welcome{
		SessionId:           rec.SessionID,
		RelayInstanceId:     r.cfg.InstanceID,
		ConnectionEpoch:     epoch,
		GrantKeys:           keys,
		EndpointIncarnation: inc.Value,
	}
	if r.cfg.LifetimeHint > 0 {
		w.LifetimeHintS = uint32(r.cfg.LifetimeHint.Seconds())
	}
	return w, nil
}

// fallbackRefused logs and maps a CheckFallbackAgainstLaunchID error: the
// 4409 refusal as is, a store read error as 4504 (fail closed; an
// infrastructure fault is never 4409).
func (a *admitter) fallbackRefused(err error) error {
	r, p := a.r, a.p
	if IsSupersededIncarnation(err) {
		r.log.Warn("Conduit admission refused: hello without launch id while a session of the current launch exists",
			"reason", ReasonLegacyHelloSuperseded, "principal_id", p.ID, "project_id", p.ProjectID,
			"current_launch_id", p.Agent.LaunchID, "generation", p.Agent.Generation)
		return err
	}
	r.log.Warn("Conduit admission refused: registry unavailable", "principal_id", p.ID, "error", err)
	return reject(conduit.CloseRelayTimeout, ReasonRegistryUnavailable, "")
}

// recheckFallback closes the check-then-insert race of the fallback fence:
// a launch-id Hello admitted concurrently may have inserted its row after
// our pre-check. If a launch_id row now exists (or the re-check cannot be
// read), our row is deleted and the Hello refused (4409, or 4504 on a read
// error).
//
// Our insert bumped the agent's epoch. A launch session that inserted
// before us is therefore no longer epoch-current, and once our row is gone
// nothing would be routable while that session stays connected (it gets
// no signal). Its row is deleted as well, so its relay's next touch finds
// the row gone and sends GoAway 4503 with a jittered reconnect hint; the
// launch container redials, bumps the epoch and is routable again within
// about one ping interval. A launch session that inserted after us is
// already current and is left alone.
func (a *admitter) recheckFallback(ctx context.Context, rec registry.SessionRecord, inc Incarnation) error {
	r, p := a.r, a.p
	err := CheckFallbackAgainstLaunchID(ctx, r.cfg.Store, p.ID, p.Agent, inc, rec.SessionID)
	if err == nil {
		return nil
	}
	r.deleteRow(ctx, rec.SessionID, rec.RelayGeneration)
	if IsSupersededIncarnation(err) {
		r.evictObsoleteLaunchRows(ctx, p)
	}
	return a.fallbackRefused(err)
}

// AbandonAdmission undoes an admission whose Welcome was discarded.
func (a *admitter) AbandonAdmission(ctx context.Context, _ *conduitv1.Hello, w *conduitv1.Welcome) {
	a.mu.Lock()
	rec, ok := a.rec, a.ok && a.rec.SessionID == w.GetSessionId()
	a.ok = false
	a.mu.Unlock()
	if ok {
		a.r.deleteRow(ctx, rec.SessionID, rec.RelayGeneration)
	}
}

func (a *admitter) Refresh(ctx context.Context, ar *conduitv1.AuthRefresh) error {
	if a.r.cfg.Refresh == nil {
		return reject(conduit.CloseUnauthenticated, ReasonUnauthenticated, "auth refresh not supported")
	}
	return a.r.cfg.Refresh(ctx, a.p, ar)
}

func (a *admitter) admitted() (registry.SessionRecord, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rec, a.source, a.ok
}

func registryTransport(name string) (string, error) {
	switch name {
	case registry.TransportWS, registry.TransportGRPC, registry.TransportH1Pair:
		return name, nil
	}
	return "", fmt.Errorf("transport %q is not routable", name)
}

// capabilitiesFromProto records the Hello capabilities with the
// authoritative incarnation and exec scope (never the presented ones) and
// the incarnation source, so operators can see sessions admitted with the
// interim gen-N value.
func capabilitiesFromProto(c *conduitv1.Capabilities, inc Incarnation, execScope string) registry.Capabilities {
	return registry.Capabilities{
		StreamKinds:         append([]string(nil), c.GetStreamKinds()...),
		RPC:                 append([]string(nil), c.GetRpc()...),
		EndpointIncarnation: inc.Value,
		ExecScope:           execScope,
		IncarnationSource:   inc.Source,
		TransportLimits: registry.TransportLimits{
			MaxFrame:     int64(c.GetTransportLimits().GetMaxFrame()),
			IdleTimeoutS: int64(c.GetTransportLimits().GetIdleTimeoutS()),
		},
	}
}
