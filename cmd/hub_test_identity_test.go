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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for scion hub test-identity (ptone/scion#4240, Phase 2b). The hub
// is a mock; the token is a synthetic placeholder string, not a credential.

const tiCLIFakeToken = "synthetic-fixture-token-DO-NOT-PRINT-7f3a9c"

// tiCLIHub is a mock hub for the test identity routes. It records request
// bodies and query strings.
type tiCLIHub struct {
	mu      sync.Mutex
	bodies  []map[string]interface{}
	queries []string
	calls   int
	// deleteStatus and deleteBody answer DELETE; 204 when zero.
	deleteStatus int
	deleteBody   string
	// listBody answers GET.
	listBody string
}

func (h *tiCLIHub) client(t *testing.T) hubclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls++
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.bodies = append(h.bodies, body)
		h.queries = append(h.queries, r.URL.RawQuery)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		identity := `{"id":"fx-1","email":"test-identity-abc@scion-fixture.invalid","role":"member","status":"active","purpose":"e2e","issuedBy":"issuer-1","expiresAt":"2026-10-10T18:00:00Z","live":true}`
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/test-identities":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"identity":` + identity + `,"accessToken":"` + tiCLIFakeToken + `","tokenType":"Bearer","expiresIn":1800,"tokenExpiresAt":"2026-10-10T17:30:00Z"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/test-identities/fx-1/token":
			_, _ = w.Write([]byte(`{"identity":` + identity + `,"accessToken":"` + tiCLIFakeToken + `","tokenType":"Bearer","expiresIn":1800,"tokenExpiresAt":"2026-10-10T17:30:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/test-identities":
			_, _ = w.Write([]byte(h.listBody))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/test-identities/"):
			if h.deleteStatus == 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(h.deleteStatus)
			_, _ = w.Write([]byte(h.deleteBody))
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

// assertNoToken fails if the token appears in any of the outputs.
func assertNoToken(t *testing.T, outputs ...string) {
	t.Helper()
	for _, o := range outputs {
		assert.NotContains(t, o, tiCLIFakeToken)
		assert.NotContains(t, o, "DO-NOT-PRINT")
	}
}

// Golden: issue prints exactly the non-secret facts on stdout, nothing on
// stderr, and writes the token only to the 0600 file.
func TestHubTestIdentityIssue_GoldenOutputAndFile(t *testing.T) {
	hub := &tiCLIHub{}
	c := hub.client(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "fixture.token")

	var stdout, stderr bytes.Buffer
	err := issueTestIdentity(context.Background(), &stdout, &stderr, c.TestIdentities(), testIdentityIssueOptions{
		Role: "member", TTL: 30 * time.Minute, Lifetime: 2 * time.Hour, Purpose: "e2e", Out: out,
	})
	require.NoError(t, err)

	golden := "Issued test identity fx-1\n" +
		"  Email:         test-identity-abc@scion-fixture.invalid\n" +
		"  Role:          member\n" +
		"  Expires:       2026-10-10T18:00:00Z\n" +
		"  Token expires: 2026-10-10T17:30:00Z\n" +
		"  Purpose:       e2e\n" +
		"  Token file:    " + out + " (mode 0600)\n"
	assert.Equal(t, golden, stdout.String())
	assert.Empty(t, stderr.String())
	assertNoToken(t, stdout.String(), stderr.String())

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, tiCLIFakeToken+"\n", string(data))
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(out)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}

	// The request: role, purpose, token TTL and identity lifetime
	// (--lifetime, a pass-through of lifetimeSeconds) in whole seconds.
	require.Len(t, hub.bodies, 1)
	assert.Equal(t, map[string]interface{}{
		"role": "member", "purpose": "e2e", "tokenTtlSeconds": float64(1800), "lifetimeSeconds": float64(7200),
	}, hub.bodies[0])
}

// Without --ttl and --lifetime the request leaves both to the hub.
func TestHubTestIdentityIssue_DefaultsLeftToHub(t *testing.T) {
	hub := &tiCLIHub{}
	c := hub.client(t)
	var stdout, stderr bytes.Buffer
	require.NoError(t, issueTestIdentity(context.Background(), &stdout, &stderr, c.TestIdentities(), testIdentityIssueOptions{
		Role: "viewer", Out: filepath.Join(t.TempDir(), "t"),
	}))
	require.Len(t, hub.bodies, 1)
	assert.Equal(t, map[string]interface{}{"role": "viewer"}, hub.bodies[0])
}

// Golden for JSON output: no token field, and the token never appears.
func TestHubTestIdentityIssue_JSONHasNoToken(t *testing.T) {
	hub := &tiCLIHub{}
	c := hub.client(t)
	out := filepath.Join(t.TempDir(), "fixture.token")
	var stdout, stderr bytes.Buffer
	require.NoError(t, issueTestIdentity(context.Background(), &stdout, &stderr, c.TestIdentities(), testIdentityIssueOptions{Out: out, JSON: true}))
	assertNoToken(t, stdout.String(), stderr.String())
	assert.NotContains(t, stdout.String(), "accessToken")
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, out, got["tokenFile"])
	assert.Equal(t, "fx-1", got["identity"].(map[string]interface{})["id"])
	assert.ElementsMatch(t, []string{"identity", "tokenFile", "tokenExpiresAt"}, mapKeys(got))
}

// An existing file, a symlink to an existing file, and a dangling symlink
// are all refused before any request is sent; the existing file and the
// symlink target are untouched.
func TestHubTestIdentity_RefusesExistingOrSymlinkedOut(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing")
	require.NoError(t, os.WriteFile(existing, []byte("keep"), 0o644))
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("target"), 0o644))
	link := filepath.Join(dir, "link")
	dangling := filepath.Join(dir, "dangling")
	paths := map[string]string{"existing": existing}
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Symlink(target, link))
		require.NoError(t, os.Symlink(filepath.Join(dir, "missing"), dangling))
		paths["symlink"] = link
		paths["dangling symlink"] = dangling
	}

	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			hub := &tiCLIHub{}
			c := hub.client(t)
			var stdout, stderr bytes.Buffer
			err := issueTestIdentity(context.Background(), &stdout, &stderr, c.TestIdentities(), testIdentityIssueOptions{Out: path})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "refusing --out")
			err = reissueTestIdentityToken(context.Background(), &stdout, &stderr, c.TestIdentities(), testIdentityTokenOptions{ID: "fx-1", Out: path})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "refusing --out")
			assert.Zero(t, hub.calls, "no request is sent for a refused --out")
			assertNoToken(t, stdout.String(), stderr.String(), err.Error())
		})
	}
	data, err := os.ReadFile(existing)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(data))
	data, err = os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "target", string(data))
	if runtime.GOOS != "windows" {
		_, err = os.Lstat(filepath.Join(dir, "missing"))
		assert.True(t, os.IsNotExist(err), "the dangling symlink's target is not created")
	}
}

// --out is required, and a failed issuance leaves no file behind.
func TestHubTestIdentity_OutRequiredAndFailureLeavesNoFile(t *testing.T) {
	hub := &tiCLIHub{}
	c := hub.client(t)
	var stdout, stderr bytes.Buffer
	err := issueTestIdentity(context.Background(), &stdout, &stderr, c.TestIdentities(), testIdentityIssueOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--out is required")

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"quota_exceeded","message":"live test identity limit for this issuer reached"}}`))
	}))
	defer failing.Close()
	fc, err := hubclient.New(failing.URL)
	require.NoError(t, err)
	out := filepath.Join(t.TempDir(), "fixture.token")
	err = issueTestIdentity(context.Background(), &stdout, &stderr, fc.TestIdentities(), testIdentityIssueOptions{Out: out})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "limit for this issuer reached")
	_, statErr := os.Stat(out)
	assert.True(t, os.IsNotExist(statErr), "no token file is left after a failed issuance")
}

// Client-side validation: role and whole-second durations.
func TestHubTestIdentityIssue_Validation(t *testing.T) {
	hub := &tiCLIHub{}
	c := hub.client(t)
	var stdout, stderr bytes.Buffer
	for _, opts := range []testIdentityIssueOptions{
		{Role: "admin"},
		{Role: "Admin"},
		{TTL: 1500 * time.Millisecond},
		{Lifetime: 500 * time.Millisecond},
	} {
		opts.Out = filepath.Join(t.TempDir(), "t")
		err := issueTestIdentity(context.Background(), &stdout, &stderr, c.TestIdentities(), opts)
		require.Error(t, err, "%+v", opts)
		_, statErr := os.Stat(opts.Out)
		assert.True(t, os.IsNotExist(statErr))
	}
	assert.Zero(t, hub.calls)
}

// Golden: token prints the non-secret facts only and writes the file.
func TestHubTestIdentityToken_GoldenOutputAndFile(t *testing.T) {
	hub := &tiCLIHub{}
	c := hub.client(t)
	out := filepath.Join(t.TempDir(), "fixture-2.token")
	var stdout, stderr bytes.Buffer
	require.NoError(t, reissueTestIdentityToken(context.Background(), &stdout, &stderr, c.TestIdentities(), testIdentityTokenOptions{ID: "fx-1", TTL: 10 * time.Minute, Out: out}))
	golden := "New token for test identity fx-1\n" +
		"  Email:         test-identity-abc@scion-fixture.invalid\n" +
		"  Role:          member\n" +
		"  Expires:       2026-10-10T18:00:00Z\n" +
		"  Token expires: 2026-10-10T17:30:00Z\n" +
		"  Purpose:       e2e\n" +
		"  Token file:    " + out + " (mode 0600)\n"
	assert.Equal(t, golden, stdout.String())
	assert.Empty(t, stderr.String())
	assertNoToken(t, stdout.String(), stderr.String())
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, tiCLIFakeToken+"\n", string(data))
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(out)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
	require.Len(t, hub.bodies, 1)
	assert.Equal(t, map[string]interface{}{"tokenTtlSeconds": float64(600)}, hub.bodies[0])
}

// list passes limit and includeExpired through, sends nothing that would
// widen the result past the caller's own identities (the hub scopes the
// list to the caller unless it is an admin session; see
// pkg/hub TestTestIdentity_ListIsolation), and prints exactly what the hub
// returned.
func TestHubTestIdentityList_PassThroughAndOutput(t *testing.T) {
	hub := &tiCLIHub{listBody: `{"items":[{"id":"fx-1","email":"a@scion-fixture.invalid","role":"member","purpose":"e2e","issuedBy":"issuer-1","expiresAt":"2026-10-10T18:00:00Z","live":true},{"id":"fx-2","email":"b@scion-fixture.invalid","role":"viewer","issuedBy":"issuer-1","expiresAt":"2026-10-10T16:00:00Z","live":false}],"truncated":true}`}
	c := hub.client(t)
	var stdout bytes.Buffer
	require.NoError(t, listTestIdentities(context.Background(), &stdout, c.TestIdentities(), &hubclient.ListTestIdentitiesOptions{Limit: 2, IncludeExpired: true}, false))
	require.Len(t, hub.queries, 1)
	assert.Equal(t, "includeExpired=true&limit=2", hub.queries[0])
	golden := "ID    EMAIL                    ROLE    LIVE  EXPIRES               PURPOSE\n" +
		"fx-1  a@scion-fixture.invalid  member  yes   2026-10-10T18:00:00Z  e2e\n" +
		"fx-2  b@scion-fixture.invalid  viewer  no    2026-10-10T16:00:00Z  \n" +
		"(more identities match; raise --limit to see them)\n"
	assert.Equal(t, golden, stdout.String())

	// Defaults send no query at all.
	stdout.Reset()
	require.NoError(t, listTestIdentities(context.Background(), &stdout, c.TestIdentities(), &hubclient.ListTestIdentitiesOptions{}, false))
	assert.Equal(t, "", hub.queries[1])

	// Empty list.
	empty := &tiCLIHub{listBody: `{"items":[],"truncated":false}`}
	stdout.Reset()
	require.NoError(t, listTestIdentities(context.Background(), &stdout, empty.client(t).TestIdentities(), nil, false))
	assert.Equal(t, "No test identities.\n", stdout.String())
}

// delete: 204 prints a confirmation; 404 succeeds with a notice on stderr
// (idempotent); 409 lists the blocking agents or projects and fails.
func TestHubTestIdentityDelete(t *testing.T) {
	ctx := context.Background()

	hub := &tiCLIHub{}
	var stdout, stderr bytes.Buffer
	require.NoError(t, deleteTestIdentity(ctx, &stdout, &stderr, hub.client(t).TestIdentities(), "fx-1"))
	assert.Equal(t, "Deleted test identity fx-1.\n", stdout.String())
	assert.Empty(t, stderr.String())
	require.Len(t, hub.bodies, 1)
	assert.Nil(t, hub.bodies[0], "delete sends no body")
	assert.Equal(t, "", hub.queries[0], "delete sends no query (no purge)")

	gone := &tiCLIHub{deleteStatus: http.StatusNotFound, deleteBody: `{"error":{"code":"not_found","message":"test identity not found"}}`}
	stdout.Reset()
	stderr.Reset()
	require.NoError(t, deleteTestIdentity(ctx, &stdout, &stderr, gone.client(t).TestIdentities(), "fx-1"))
	assert.Empty(t, stdout.String())
	assert.Equal(t, "Notice: test identity fx-1 was not found (already deleted, or not one you may delete); nothing to do.\n", stderr.String())

	agents := &tiCLIHub{deleteStatus: http.StatusConflict, deleteBody: `{"error":{"code":"conflict","message":"owns agents","details":{"agents":[{"id":"a-1","slug":"worker","projectId":"p-1"}]}}}`}
	err := deleteTestIdentity(ctx, &stdout, &stderr, agents.client(t).TestIdentities(), "fx-1")
	require.Error(t, err)
	assert.Equal(t, "test identity fx-1 still owns agents; delete them first:\n  agent worker (a-1) in project p-1", err.Error())

	owner := &tiCLIHub{deleteStatus: http.StatusConflict, deleteBody: `{"error":{"code":"last_owner","message":"last owner","details":{"projects":[{"id":"p-1","name":"e2e-project"}]}}}`}
	err = deleteTestIdentity(ctx, &stdout, &stderr, owner.client(t).TestIdentities(), "fx-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "test identity fx-1 is the last owner of these projects; delete them first")
	assert.Contains(t, err.Error(), "  project e2e-project (p-1)")

	forbidden := &tiCLIHub{deleteStatus: http.StatusForbidden, deleteBody: `{"error":{"code":"forbidden","message":"requires test_identity.issue permission"}}`}
	err = deleteTestIdentity(ctx, &stdout, &stderr, forbidden.client(t).TestIdentities(), "fx-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to delete test identity fx-1")
}

// The delete command has no --purge flag (Phase 3 adds it).
func TestHubTestIdentityDelete_NoPurgeFlag(t *testing.T) {
	cmd := resolveCommandPath(rootCmd, "hub.test-identity.delete")
	require.NotNil(t, cmd)
	assert.Nil(t, cmd.Flags().Lookup("purge"))
}

// CLI mode: scion hub test-identity and its subcommands are available in
// human mode and removed in agent mode (not in agentAllowed).
func TestHubTestIdentityCmd_ModeAvailability(t *testing.T) {
	paths := []string{
		"hub.test-identity",
		"hub.test-identity.issue",
		"hub.test-identity.token",
		"hub.test-identity.list",
		"hub.test-identity.delete",
	}
	for _, p := range paths {
		require.NotNil(t, resolveCommandPath(rootCmd, p), "%s must exist in the real command tree", p)
		assert.False(t, agentAllowed[p], "%s must not be allowed in agent mode", p)
	}
	for _, tc := range []struct {
		mode    string
		present bool
	}{
		{"human", true},
		{"agent", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			root := &cobra.Command{Use: "scion"}
			hubReal := resolveCommandPath(rootCmd, "hub")
			require.NotNil(t, hubReal)
			root.AddCommand(cloneCommandShape(hubReal))

			t.Setenv("SCION_CLI_MODE", tc.mode)
			applyModeRestrictions(root, resolveMode())
			for _, p := range paths {
				if tc.present {
					assert.NotNil(t, resolveCommandPath(root, p), "%s mode keeps %s", tc.mode, p)
				} else {
					assert.Nil(t, resolveCommandPath(root, p), "%s mode removes %s", tc.mode, p)
				}
			}
		})
	}
}
