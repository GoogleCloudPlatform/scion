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

package hub

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// liveInventoryKey identifies one HTTP entry point declared in
// authzop.Catalog, by the operation that declares it and the declared
// method/pattern.
type liveInventoryKey struct {
	OperationID string
	Method      string
	Pattern     string
}

// liveInventoryExclusions lists HTTP catalog entry points that
// TestCatalogHTTPEntryPoints_LiveMethodCheck deliberately does not send a
// live request for, each with a reviewed reason. Keep this list small: an
// entry belongs here only when actually dispatching the request would risk
// a real external side effect (minting a credential, restarting the host
// process, calling out to a release channel) rather than merely exercising
// routing. TestLiveInventoryExclusionsNotStale asserts every key here still
// names a real, currently-declared catalog HTTP entry point.
var liveInventoryExclusions = map[liveInventoryKey]string{
	{OperationID: "gcp.identity.mint", Method: "POST", Pattern: "/api/v1/agent/gcp-token"}:                       "would proceed to call the live GCP token-minting API once past agent-JWT authentication; no store lookup short-circuits it first",
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/restart"}:       "invokes a real systemd restart subprocess (handleAdminRestart); no entity lookup precedes it to short-circuit safely",
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/check-updates"}: "may call out to the configured release channel (GitHub) to check for updates (handleCheckForUpdates)",
}

// substituteLiveInventoryParams replaces every "{name}" placeholder in an
// authzop EntryPoint pattern with a fixed, syntactically valid placeholder
// value, or with a caller-supplied override for specific placeholder names,
// so the resulting path can be dispatched through the real server mux.
func substituteLiveInventoryParams(pattern string, overrides map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); {
		if pattern[i] == '{' {
			end := strings.IndexByte(pattern[i:], '}')
			if end < 0 {
				b.WriteString(pattern[i:])
				break
			}
			name := pattern[i+1 : i+end]
			if v, ok := overrides[name]; ok {
				b.WriteString(v)
			} else {
				b.WriteString("live-inventory-placeholder")
			}
			i += end + 1
			continue
		}
		b.WriteByte(pattern[i])
		i++
	}
	return b.String()
}

// catalogHTTPEntryPoints returns every (operationID, EntryPoint) pair in
// authzop.Catalog whose Kind is EntryPointHTTPRoute. Non-HTTP kinds
// (WebSocket, SSE, BrokerCall, SchedulerJob, CLICommand, BackgroundJob,
// InternalDispatch) are out of scope for a live HTTP method check by
// construction — they either have no HTTP surface at all, or (SSE/WebSocket)
// are already pinned by dedicated tests in authzop/drift_test.go.
func catalogHTTPEntryPoints() []struct {
	OperationID string
	EntryPoint  authzop.EntryPoint
} {
	var out []struct {
		OperationID string
		EntryPoint  authzop.EntryPoint
	}
	for _, spec := range authzop.Catalog {
		for _, ep := range spec.EntryPoints {
			if ep.Kind != authzop.EntryPointHTTPRoute {
				continue
			}
			out = append(out, struct {
				OperationID string
				EntryPoint  authzop.EntryPoint
			}{OperationID: string(spec.ID), EntryPoint: ep})
		}
	}
	return out
}

// TestCatalogHTTPEntryPoints_LiveMethodCheck probes the real server mux for
// every declared HTTP entry point in authzop.Catalog (skipping the small,
// reviewed liveInventoryExclusions) and asserts the declared method is
// accepted there — i.e. the response is never 405 Method Not Allowed.
//
// Every handler in this package that dispatches by HTTP method returns 405
// via MethodNotAllowed(w) as the first thing it does on an unrecognized
// method, before authentication, entity lookup, or business logic (see
// e.g. handleSecrets, handleGroupRoutes, handleSchedules,
// handleAdminMaintenanceOps). A placeholder path segment therefore still
// reaches the real method dispatch for the overwhelming majority of routes,
// which makes "not 405" a reliable, low-fixture signal that the catalog's
// declared method is the one the live route actually accepts.
//
// This is #2227's "probe the real hub server mux... so EVERY catalog HTTP
// entry is checked for method and path" — a deterministic walk driven by
// authzop.Catalog itself, verified against the live mux rather than a
// second hand-typed list of routes.
func TestCatalogHTTPEntryPoints_LiveMethodCheck(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// group.member.remove (DELETE /groups/{id}/members/{memberType}/{memberId})
	// is the one entry point where handleGroupMemberByID validates the
	// member type and looks up the group by ID *before* it reaches the
	// method switch (handlers_groups.go:806-828) — a placeholder group ID
	// would 404 there regardless of method, never reaching the switch this
	// test means to exercise. Give it one real group and one real member so
	// the method dispatch is actually reached.
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("live-inv-group"), Slug: "live-inv-group", Name: "Live Inventory Group",
	}))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("live-inv-member"), Email: "live-inv-member@test.com", DisplayName: "Live Inv Member", Role: "member", Status: "active",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    tid("live-inv-group"),
		MemberID:   tid("live-inv-member"),
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))
	fixtureOverrides := map[string]map[string]string{
		"group.member.remove": {"id": tid("live-inv-group"), "memberType": "user", "memberId": tid("live-inv-member")},
	}

	tested := 0
	for _, entry := range catalogHTTPEntryPoints() {
		ep := entry.EntryPoint
		key := liveInventoryKey{OperationID: entry.OperationID, Method: ep.Method, Pattern: ep.Pattern}
		if reason, excluded := liveInventoryExclusions[key]; excluded {
			t.Logf("skipping %s %s (operation %s): %s", ep.Method, ep.Pattern, entry.OperationID, reason)
			continue
		}

		path := substituteLiveInventoryParams(ep.Pattern, fixtureOverrides[entry.OperationID])

		var body interface{}
		switch ep.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			body = map[string]interface{}{}
		}

		rec := doRequest(t, srv, ep.Method, path, body)
		tested++
		if rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s (operation %s): got 405 Method Not Allowed — the catalog declares a method the live route does not accept",
				ep.Method, path, entry.OperationID)
		}
	}

	if tested == 0 {
		t.Fatal("no HTTP catalog entry points were exercised — this test is broken")
	}
	t.Logf("live method check: %d HTTP entry points probed, %d excluded", tested, len(liveInventoryExclusions))
}

// TestLiveInventoryExclusionsNotStale asserts every entry in
// liveInventoryExclusions still names a real, currently declared catalog
// HTTP entry point. A stale exclusion — left behind after a catalog
// correction changes or removes the entry point it names — would silently
// stop meaning anything and hide the entry from live-method coverage for no
// reason.
func TestLiveInventoryExclusionsNotStale(t *testing.T) {
	live := make(map[liveInventoryKey]bool)
	for _, entry := range catalogHTTPEntryPoints() {
		live[liveInventoryKey{OperationID: entry.OperationID, Method: entry.EntryPoint.Method, Pattern: entry.EntryPoint.Pattern}] = true
	}
	for key := range liveInventoryExclusions {
		if !live[key] {
			t.Errorf("stale live-inventory exclusion: operation %q method %q pattern %q does not match any current catalog HTTP entry point",
				key.OperationID, key.Method, key.Pattern)
		}
	}
}
