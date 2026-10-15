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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for scion hub test-identity cleanup and provenance
// (ptone/scion#4240, Phase 3c). The hub is a mock.

// tiCleanupHub is a mock hub for the cleanup and provenance routes. It
// records every request; provenance answers come from a per-resource list
// (the n-th call gets the n-th answer, the last one repeats).
type tiCleanupHub struct {
	mu         sync.Mutex
	requests   []string
	bodies     []string
	calls      map[string]int
	provenance map[string][]string
	// cleanupStatus and cleanupBody answer POST cleanup.
	cleanupStatus int
	cleanupBody   string
	// status answers every route with an error status when non-zero.
	status  int
	errBody string
}

func (h *tiCleanupHub) client(t *testing.T) hubclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.RequestURI())
		h.bodies = append(h.bodies, string(body))
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if h.status != 0 {
			w.WriteHeader(h.status)
			_, _ = w.Write([]byte(h.errBody))
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/test-identities/cleanup":
			if h.cleanupStatus != 0 {
				w.WriteHeader(h.cleanupStatus)
			}
			_, _ = w.Write([]byte(h.cleanupBody))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/test-identities/provenance":
			res := r.URL.Query().Get("resource")
			h.mu.Lock()
			if h.calls == nil {
				h.calls = map[string]int{}
			}
			answers := h.provenance[res]
			n := h.calls[res]
			h.calls[res]++
			h.mu.Unlock()
			if len(answers) == 0 {
				t.Errorf("unexpected provenance for %q", res)
				w.WriteHeader(http.StatusTeapot)
				return
			}
			if n >= len(answers) {
				n = len(answers) - 1
			}
			_, _ = w.Write([]byte(answers[n]))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return c
}

func (h *tiCleanupHub) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

func tiProven(res, root, state string) string {
	return `{"resource":"` + res + `","proven":true,"fixtureRootId":"` + root + `","fixtureIssuerId":"iss-1","fixtureState":"` + state + `","reason":"fixture stamp"}`
}

func tiUnproven(res string) string {
	return `{"resource":"` + res + `","proven":false,"reason":"no fixture stamp"}`
}

func writeExpectAbsentFile(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "expect-absent.txt")
	require.NoError(t, os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return p
}

const tiCleanupRealBody = `{"runId":"run-1","dryRun":false,"scope":"issuer","complete":true,` +
	`"identities":[{"id":"fx-1","state":"expired"},{"id":"fx-2","state":"absent"}],` +
	`"targets":[{"type":"agent","id":"a-1","fixtureRootId":"fx-1","projectId":"p-1","status":"deleted"},` +
	`{"type":"project","id":"p-1","fixtureRootId":"fx-1","status":"deleted"},` +
	`{"type":"test_identity","id":"fx-1","fixtureRootId":"fx-1","status":"deleted"},` +
	`{"type":"schedule","id":"s-2","fixtureRootId":"fx-2","projectId":"p-9","status":"deleted"}],` +
	`"deleted":{"agents":1,"schedules":1,"groups":0,"projects":1},"identitiesDeleted":1,"liveSkipped":1}`

// Golden: a real run prints the counts, each identity and each target;
// it sends exactly one POST with no body and no query.
func TestHubTestIdentityCleanup_Golden(t *testing.T) {
	h := &tiCleanupHub{cleanupBody: tiCleanupRealBody}
	var out bytes.Buffer
	require.NoError(t, cleanupTestIdentities(context.Background(), &out, h.client(t).TestIdentities(), testIdentityCleanupOptions{}))
	assert.Equal(t, "Cleanup (issuer scope, run run-1): removed 1 agents, 1 schedules, 0 groups, 1 projects and 1 test identities.\n"+
		"  test identity fx-1 (expired)\n"+
		"  test identity fx-2 (absent)\n"+
		"    deleted agent a-1\n"+
		"    deleted project p-1\n"+
		"    deleted test_identity fx-1\n"+
		"    deleted schedule s-2\n"+
		"  1 live test identities were not touched\n", out.String())
	assert.Equal(t, []string{"POST /api/v1/test-identities/cleanup"}, h.seen())
	assert.Equal(t, []string{""}, h.bodies)
}

// --dry-run sends only POST ?dryRun=true (the hub writes nothing) and
// says nothing was changed.
func TestHubTestIdentityCleanup_DryRun(t *testing.T) {
	h := &tiCleanupHub{cleanupBody: `{"dryRun":true,"scope":"delegation","complete":false,` +
		`"identities":[{"id":"fx-1","state":"expired"}],` +
		`"targets":[{"type":"agent","id":"a-1","fixtureRootId":"fx-1","status":"target"}],"deleted":{"agents":0,"schedules":0,"groups":0,"projects":0}}`}
	var out bytes.Buffer
	require.NoError(t, cleanupTestIdentities(context.Background(), &out, h.client(t).TestIdentities(), testIdentityCleanupOptions{DryRun: true}))
	assert.Equal(t, "Cleanup dry run (delegation scope): 1 test identities, 1 targets; nothing was changed.\n"+
		"  test identity fx-1 (expired)\n"+
		"    target agent a-1\n", out.String())
	assert.Equal(t, []string{"POST /api/v1/test-identities/cleanup?dryRun=true"}, h.seen())
}

// --expect-absent with proven and unproven IDs: provenance is checked
// before the run; a proven ID the run deleted is cleaned; a proven ID the
// run did not select and that is still proven afterwards is NOT cleaned;
// one proven before and gone after is absent; each unproven ID is printed
// with proven:false, the reason and "real-admin action required", is never
// reported as cleaned, and makes the command fail.
func TestHubTestIdentityCleanup_ExpectAbsentMixed(t *testing.T) {
	h := &tiCleanupHub{
		cleanupBody: tiCleanupRealBody,
		provenance: map[string][]string{
			"agent:a-1":    {tiProven("agent:a-1", "fx-1", "expired")},
			"schedule:s-2": {tiProven("schedule:s-2", "fx-2", "absent")},
			"agent:a-live": {tiProven("agent:a-live", "fx-3", "live")},
			"group:g-gone": {tiProven("group:g-gone", "fx-4", "expired"), tiUnproven("group:g-gone")},
			"agent:porter": {tiUnproven("agent:porter")},
			"project:p-x":  {tiUnproven("project:p-x")},
		},
	}
	file := writeExpectAbsentFile(t, "# run 42", "agent:a-1", "", "schedule:s-2", "agent:a-live", "group:g-gone", "agent:porter", "project:p-x", "agent:a-1")
	expected, err := readExpectAbsent(file)
	require.NoError(t, err)
	assert.Equal(t, []string{"agent:a-1", "schedule:s-2", "agent:a-live", "group:g-gone", "agent:porter", "project:p-x"}, expected, "comments, blanks and duplicates dropped")

	var out bytes.Buffer
	err = cleanupTestIdentities(context.Background(), &out, h.client(t).TestIdentities(), testIdentityCleanupOptions{ExpectAbsent: expected})
	require.ErrorIs(t, err, errExpectAbsentResiduals)
	assert.Contains(t, err.Error(), "3 of 6")
	got := out.String()
	exp := got[strings.Index(got, "Expected absent:\n"):]
	assert.Equal(t, "Expected absent:\n"+
		"  agent:a-1: cleaned (proven, test identity fx-1)\n"+
		"  schedule:s-2: cleaned (proven, test identity fx-2)\n"+
		"  agent:a-live: NOT cleaned (proven, test identity fx-3): still present; its test identity is live (cleanup removes only an expired or deleted identity's resources, and only for its issuer)\n"+
		"  group:g-gone: absent (proven before the run, gone after it)\n"+
		"  agent:porter: proven:false, reason: no fixture stamp; real-admin action required (never removed by cleanup)\n"+
		"  project:p-x: proven:false, reason: no fixture stamp; real-admin action required (never removed by cleanup)\n", exp)
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "porter") || strings.Contains(line, "p-x") {
			assert.NotContains(t, line, "cleaned (", "an unproven ID is never reported as cleaned: %s", line)
			assert.NotContains(t, line, "absent (", line)
		}
	}
	// Provenance first (one per ID), then one cleanup, then a second
	// provenance only for the proven IDs the run did not select.
	seen := h.seen()
	require.Len(t, seen, 6+1+2)
	for i, res := range expected {
		assert.Equal(t, "GET /api/v1/test-identities/provenance?resource="+strings.ReplaceAll(res, ":", "%3A"), seen[i])
	}
	assert.Equal(t, "POST /api/v1/test-identities/cleanup", seen[6])
	assert.ElementsMatch(t, []string{
		"GET /api/v1/test-identities/provenance?resource=agent%3Aa-live",
		"GET /api/v1/test-identities/provenance?resource=group%3Ag-gone",
	}, seen[7:])

	// JSON: the same outcomes, machine-readable.
	h2 := &tiCleanupHub{cleanupBody: tiCleanupRealBody, provenance: map[string][]string{
		"agent:a-1": {tiProven("agent:a-1", "fx-1", "expired")}, "agent:porter": {tiUnproven("agent:porter")},
	}}
	out.Reset()
	err = cleanupTestIdentities(context.Background(), &out, h2.client(t).TestIdentities(), testIdentityCleanupOptions{ExpectAbsent: []string{"agent:a-1", "agent:porter"}, JSON: true})
	require.ErrorIs(t, err, errExpectAbsentResiduals)
	var doc struct {
		Report       hubclient.TestIdentityCleanupReport `json:"report"`
		ExpectAbsent []expectAbsentResult                `json:"expectAbsent"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &doc), out.String())
	assert.Equal(t, "run-1", doc.Report.RunID)
	assert.Equal(t, []expectAbsentResult{
		{Resource: "agent:a-1", Proven: true, Outcome: expectAbsentCleaned, FixtureRootID: "fx-1", FixtureState: "expired"},
		{Resource: "agent:porter", Proven: false, Outcome: expectAbsentUnproven, Reason: "no fixture stamp"},
	}, doc.ExpectAbsent)
}

// Every expected ID proven and cleaned: the command succeeds. A dry run
// reports "would be cleaned" and, for a proven ID it would not select,
// NOT cleaned without a second provenance call.
func TestHubTestIdentityCleanup_ExpectAbsentAllCleanedAndDryRun(t *testing.T) {
	h := &tiCleanupHub{cleanupBody: tiCleanupRealBody, provenance: map[string][]string{
		"agent:a-1":   {tiProven("agent:a-1", "fx-1", "expired")},
		"project:p-1": {tiProven("project:p-1", "fx-1", "expired")},
	}}
	var out bytes.Buffer
	require.NoError(t, cleanupTestIdentities(context.Background(), &out, h.client(t).TestIdentities(), testIdentityCleanupOptions{ExpectAbsent: []string{"agent:a-1", "project:p-1"}}))
	assert.Contains(t, out.String(), "  agent:a-1: cleaned (proven, test identity fx-1)\n  project:p-1: cleaned (proven, test identity fx-1)\n")

	dry := &tiCleanupHub{cleanupBody: `{"dryRun":true,"scope":"issuer","identities":[{"id":"fx-1","state":"expired"}],` +
		`"targets":[{"type":"agent","id":"a-1","fixtureRootId":"fx-1","status":"target"}],"deleted":{"agents":0,"schedules":0,"groups":0,"projects":0}}`,
		provenance: map[string][]string{
			"agent:a-1":    {tiProven("agent:a-1", "fx-1", "expired")},
			"agent:a-live": {tiProven("agent:a-live", "fx-3", "live")},
		}}
	out.Reset()
	err := cleanupTestIdentities(context.Background(), &out, dry.client(t).TestIdentities(), testIdentityCleanupOptions{DryRun: true, ExpectAbsent: []string{"agent:a-1", "agent:a-live"}})
	require.ErrorIs(t, err, errExpectAbsentResiduals)
	assert.Contains(t, out.String(), "  agent:a-1: would be cleaned (proven, test identity fx-1)\n")
	assert.Contains(t, out.String(), "  agent:a-live: NOT cleaned (proven, test identity fx-3): not selected by this cleanup (its test identity is live, or another issuer's)\n")
	assert.Len(t, dry.seen(), 3, "two provenance checks and one dry-run cleanup; no write")
	assert.Equal(t, "POST /api/v1/test-identities/cleanup?dryRun=true", dry.seen()[2])
}

// An invalid --expect-absent line refuses the whole file before anything
// is sent.
func TestHubTestIdentityCleanup_ExpectAbsentRejectsBadLines(t *testing.T) {
	for _, line := range []string{"agent", "agent:", "user:u-1", "agent:a/1", "agent:a b", ":a-1", "agent:a:1"} {
		_, err := readExpectAbsent(writeExpectAbsentFile(t, "agent:a-1", line))
		require.Error(t, err, line)
		assert.Contains(t, err.Error(), "line 2", line)
	}
	_, err := readExpectAbsent(filepath.Join(t.TempDir(), "missing.txt"))
	require.Error(t, err)
}

// An incomplete run (409 purge_incomplete) prints what is left and fails;
// expected IDs it did not remove are NOT cleaned.
func TestHubTestIdentityCleanup_Incomplete(t *testing.T) {
	h := &tiCleanupHub{
		cleanupStatus: http.StatusConflict,
		cleanupBody: `{"error":{"code":"conflict","message":"did not complete","details":{"reason":"purge_incomplete","report":` +
			`{"runId":"run-3","scope":"issuer","complete":false,"identities":[{"id":"fx-1","state":"expired"}],` +
			`"targets":[{"type":"agent","id":"a-1","fixtureRootId":"fx-1","status":"not_done","reason":"in_doubt: in_doubt"}],` +
			`"deleted":{"agents":0,"schedules":0,"groups":0,"projects":0}}}}}`,
		provenance: map[string][]string{"agent:a-1": {tiProven("agent:a-1", "fx-1", "expired")}},
	}
	var out bytes.Buffer
	err := cleanupTestIdentities(context.Background(), &out, h.client(t).TestIdentities(), testIdentityCleanupOptions{ExpectAbsent: []string{"agent:a-1"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not complete; run the command again")
	assert.Contains(t, out.String(), "    not_done agent a-1 (in_doubt: in_doubt)\n")
	assert.Contains(t, out.String(), "  agent:a-1: NOT cleaned (proven, test identity fx-1): not_done: in_doubt: in_doubt\n")
}

// The hub's refusals are named: 403 (no test_identity.issue, or a test
// identity of any kind, the admin roles included) and 404 (test identities
// off), for cleanup and provenance alike.
func TestHubTestIdentityCleanup_NamedRefusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusForbidden, `{"error":{"code":"forbidden","message":"a test identity cannot issue test identities","details":{"reason":"test_identity_fixture_call"}}}`, errTestIdentityCleanupForbidden},
		{http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"authentication required"}}`, errTestIdentityCleanupForbidden},
		{http.StatusNotFound, `{"error":{"code":"not_found","message":"endpoint not found"}}`, errTestIdentitiesOff},
	} {
		h := &tiCleanupHub{status: tc.status, errBody: tc.body}
		var out bytes.Buffer
		err := cleanupTestIdentities(context.Background(), &out, h.client(t).TestIdentities(), testIdentityCleanupOptions{})
		require.ErrorIs(t, err, tc.want, tc.status)
		assert.Empty(t, out.String())
		err = showTestIdentityProvenance(context.Background(), &out, h.client(t).TestIdentities(), "agent:a-1", false)
		require.ErrorIs(t, err, tc.want, tc.status)
		err = cleanupTestIdentities(context.Background(), &out, h.client(t).TestIdentities(), testIdentityCleanupOptions{ExpectAbsent: []string{"agent:a-1"}})
		require.ErrorIs(t, err, tc.want, tc.status)
		assert.Len(t, h.seen(), 3, "an expect-absent check that is refused never runs the cleanup")
	}
}

// Golden: provenance prints the stamp of a proven resource and only the
// reason for an unproven one (never an owner or creator); --json prints
// the hub's answer; an invalid reference is refused before sending.
func TestHubTestIdentityProvenance_Golden(t *testing.T) {
	h := &tiCleanupHub{provenance: map[string][]string{
		"agent:a-1":   {tiProven("agent:a-1", "fx-1", "expired")},
		"project:p-x": {tiUnproven("project:p-x")},
	}}
	svc := h.client(t).TestIdentities()
	var out bytes.Buffer
	require.NoError(t, showTestIdentityProvenance(context.Background(), &out, svc, "agent:a-1", false))
	assert.Equal(t, "agent:a-1: proven:true, test identity fx-1 (expired), issuer iss-1\n", out.String())
	out.Reset()
	require.NoError(t, showTestIdentityProvenance(context.Background(), &out, svc, "project:p-x", false))
	assert.Equal(t, "project:p-x: proven:false, reason: no fixture stamp\nCleanup never removes it; a real administrator must decide.\n", out.String())
	out.Reset()
	require.NoError(t, showTestIdentityProvenance(context.Background(), &out, svc, "project:p-x", true))
	var p hubclient.TestIdentityProvenance
	require.NoError(t, json.Unmarshal(out.Bytes(), &p))
	assert.Equal(t, hubclient.TestIdentityProvenance{Resource: "project:p-x", Reason: "no fixture stamp"}, p)

	for _, bad := range []string{"agent", "agent:", "user:u-1", "agent:a/b", "group:a:b"} {
		_, err := parseProvenanceResource(bad)
		require.Error(t, err, bad)
	}
	for _, good := range []string{"agent:a-1", " schedule:s-1 ", "group:g", "project:p"} {
		got, err := parseProvenanceResource(good)
		require.NoError(t, err, good)
		assert.Equal(t, strings.TrimSpace(good), got)
	}
	assert.Len(t, h.seen(), 3)
}

// CLI mode (DEC-7, X3b gate): cleanup and provenance are agent-allowed
// and hub-profile-allowed, and in agent mode they run only against a
// target-hub profile for another hub: refused without a profile and with
// a profile for the agent's own hub, admitted with a profile for another
// hub; human mode needs no profile.
func TestHubTestIdentityCleanup_ModeAndProfileGate(t *testing.T) {
	paths := []string{"hub.test-identity.cleanup", "hub.test-identity.provenance"}
	for _, p := range paths {
		require.NotNil(t, resolveCommandPath(rootCmd, p), p)
		assert.True(t, agentAllowed[p], p)
		assert.True(t, hubProfileAllowed[p], p)
		assert.True(t, hubProfileAgentRuntime[p], p)
		assert.False(t, hubProfileNoSession[p], p)
	}
	profileGateEnv(t, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, p := range paths {
		err := checkHubProfileGate(realCommand(t, p), ModeAgent)
		require.Error(t, err, p)
		assert.Contains(t, err.Error(), "runs only against a target-hub profile", p)
		assert.NoError(t, checkHubProfileGate(realCommand(t, p), ModeHuman), p)
	}
	require.NoError(t, hubprofile.Save(home, &hubprofile.Profile{Name: "own", Endpoint: "https://hub.example.com", Transport: hubprofile.TransportNone}, false))
	require.NoError(t, hubprofile.Save(home, &hubprofile.Profile{Name: "t1", Endpoint: "https://t1.example.com", Transport: hubprofile.TransportNone}, false))
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com/")
	t.Setenv(hubprofile.EnvHubProfile, "own")
	for _, p := range paths {
		require.ErrorIs(t, checkHubProfileGate(realCommand(t, p), ModeAgent), hubprofile.ErrOwnHub, p)
	}
	t.Setenv(hubprofile.EnvHubProfile, "t1")
	for _, p := range paths {
		assert.NoError(t, checkHubProfileGate(realCommand(t, p), ModeAgent), p)
		assert.NoError(t, checkHubProfileGate(realCommand(t, p), ModeHuman), p)
	}
}

// The wire types match the hub's (drift guard).
func TestHubTestIdentityCleanup_WireTypesMatchHub(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hub, client interface{}
	}{
		{"report", hub.TestIdentityCleanupReport{}, hubclient.TestIdentityCleanupReport{}},
		{"identity", hub.TestIdentityCleanupIdentity{}, hubclient.TestIdentityCleanupIdentity{}},
		{"target", hub.TestIdentityCleanupTarget{}, hubclient.TestIdentityCleanupTarget{}},
		{"provenance", hub.TestIdentityProvenance{}, hubclient.TestIdentityProvenance{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, jsonFieldNames(reflect.TypeOf(tc.hub)), jsonFieldNames(reflect.TypeOf(tc.client)))
		})
	}
}
