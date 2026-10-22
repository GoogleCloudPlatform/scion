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

package relayrole

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
)

// TestSessionRecheckNotifyEventClosesUserSession: a revocation event alone
// (no sweep, no direct call) re-checks the held user sessions with their
// admission tokens, and a user session the hub's re-check closes ends with
// 4401 before any sweep. The session re-check is its own hub call; the
// stream re-check does not stand in for it.
func TestSessionRecheckNotifyEventClosesUserSession(t *testing.T) {
	f := newRoleFixture(t)
	events := newFakeEvents()
	f.role.cfg.Events = events
	f.admitAs(registry.PrincipalUser, "tok-user", sessionRecheckAnswer(true, conduit.CloseUnauthenticated, ""))
	ls, _, err := f.dial(t, relaytest.UserHello("user-1"), conduit.Config{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.role.Start(ctx)
	if n := f.hub.count("sessions/recheck"); n != 0 {
		t.Fatalf("%d session re-checks before the event, want 0", n)
	}

	events.ch <- "conduit.authz.changed"

	if code := closeCode(waitEnded(t, ls)); code != conduit.CloseUnauthenticated {
		t.Fatalf("session closed with %d, want 4401", code)
	}
	var req relay.HubSessionRecheckRequest
	f.hub.last("sessions/recheck", &req)
	if len(req.Items) != 1 || req.Items[0].AdmissionToken != "tok-user" {
		t.Fatalf("re-check items %+v", req.Items)
	}
}
