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
	gen, serving := r.generation()
	if !serving {
		return nil, conduit.Reject(conduit.CloseRelayRestart, "relay not serving")
	}
	kind, err := conduit.PrincipalKindFromProto(hello.GetPrincipalKind())
	if err != nil {
		return nil, conduit.Reject(conduit.CloseProtocolError, "unknown principal kind")
	}
	switch {
	case string(kind) == registry.PrincipalRelayPeer || p.Kind == registry.PrincipalRelayPeer:
		// relay-peer is an internal-API identity only; it never holds a
		// target session (design §3.10).
		return nil, conduit.Reject(conduit.CloseForbidden, "relay-peer principals may not open target sessions")
	case string(kind) != p.Kind || hello.GetPrincipalId() != p.ID:
		return nil, conduit.Reject(conduit.CloseForbidden, "hello principal does not match the authenticated principal")
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
	if s := caps.GetExecScope(); s != "" && s != p.ExecScope {
		return nil, conduit.Reject(conduit.CloseForbidden, "hello exec_scope does not match the authoritative exec scope")
	}
	transport, err := registryTransport(a.transport)
	if err != nil {
		return nil, conduit.Reject(conduit.CloseForbidden, err.Error())
	}
	// Keys before the insert: a target that cannot verify grants is
	// useless, and failing here consumes no epoch.
	keys, err := r.cfg.GrantKeys(ctx)
	if err != nil {
		r.log.Warn("Conduit admission refused: grant keys unavailable", "error", err)
		return nil, conduit.Reject(conduit.CloseRelayRestart, "grant keys unavailable")
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
		return nil, conduit.Reject(conduit.CloseRelayRestart, "relay superseded")
	case errors.Is(err, registry.ErrInvalidInput):
		return nil, conduit.Reject(conduit.CloseForbidden, "session not admissible")
	case err != nil:
		r.log.Warn("Conduit admission failed: registry insert", "error", err)
		return nil, conduit.Reject(conduit.CloseRelayRestart, "registry unavailable")
	}
	rec.ConnectionEpoch = epoch
	a.mu.Lock()
	a.rec, a.source, a.ok = rec, inc.Source, true
	a.mu.Unlock()
	if p.Kind != registry.PrincipalUser {
		r.log.Info("Conduit session admitted", "session_id", rec.SessionID, "principal_kind", p.Kind,
			"principal_id", p.ID, "connection_epoch", epoch, "incarnation_source", inc.Source)
	}

	w := &conduitv1.Welcome{
		SessionId:       rec.SessionID,
		RelayInstanceId: r.cfg.InstanceID,
		ConnectionEpoch: epoch,
		GrantKeys:       keys,
	}
	if r.cfg.LifetimeHint > 0 {
		w.LifetimeHintS = uint32(r.cfg.LifetimeHint.Seconds())
	}
	return w, nil
}

// AbandonAdmission undoes an admission whose Welcome was discarded.
func (a *admitter) AbandonAdmission(_ context.Context, _ *conduitv1.Hello, w *conduitv1.Welcome) {
	a.mu.Lock()
	rec, ok := a.rec, a.ok && a.rec.SessionID == w.GetSessionId()
	a.ok = false
	a.mu.Unlock()
	if ok {
		a.r.deleteRow(rec.SessionID, rec.RelayGeneration)
	}
}

func (a *admitter) Refresh(ctx context.Context, ar *conduitv1.AuthRefresh) error {
	if a.r.cfg.Refresh == nil {
		return conduit.Reject(conduit.CloseUnauthenticated, "auth refresh not supported")
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
