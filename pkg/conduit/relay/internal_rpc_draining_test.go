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

//go:build !no_sqlite

package relay_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestServeRPCDrainingTargetIsStaleRoute: an internal RPC to a target
// session that is draining (the target sent GoAway) is refused as a stale
// route for that session, so the caller re-resolves. It is never a 502,
// which the caller would have to treat as an unknown outcome.
func TestServeRPCDrainingTargetIsStaleRoute(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	cfg := echoConfig()
	echo := cfg.RPCHandler
	cfg.RPCHandler = conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		if req.GetPath() == "/hold" {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return &conduitv1.RpcResponse{Status: 200}
		}
		return echo.HandleRPC(ctx, req)
	})
	p := newPair(t, cfg)
	ls, _, ok := p.a.Relay.Local(context.Background(), p.rec.SessionID)
	if !ok {
		t.Fatal("owner relay does not hold the target session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// An RPC in flight keeps the target's drain open.
	held := make(chan error, 1)
	go func() {
		_, err := p.remote().Call(ctx, &conduitv1.RpcRequest{RequestId: "hold", Method: "GET", Path: "/hold"})
		held <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("held RPC did not reach the target")
	}
	if err := p.target.GoAway(conduit.GoAwayOptions{DrainDeadline: time.Minute}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ls.GoAwayReceived():
	case <-time.After(10 * time.Second):
		t.Fatal("owner relay did not see the target's GoAway")
	}

	_, err := p.remote().Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "GET", Path: "/x"})
	var stale *relay.StaleRouteError
	if !errors.As(err, &stale) {
		t.Fatalf("Call = %v, want a stale route", err)
	}
	if stale.Reason != string(registry.ReasonDraining) {
		t.Fatalf("stale reason = %q, want %q", stale.Reason, registry.ReasonDraining)
	}

	close(release)
	if err := <-held; err != nil {
		t.Fatalf("held RPC = %v, want it to complete during the drain", err)
	}
}
