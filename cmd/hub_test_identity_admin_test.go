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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for test identities with admin roles in the CLI (ptone/scion#4240,
// admin tier A4). The hub is a mock; the token is a synthetic placeholder
// string, not a credential.

const tiAdminFakeToken = "synthetic-admin-fixture-token-DO-NOT-PRINT-4c1e"

// tiAdminHub is a mock hub for the admin routes and the health summary.
// It records every request's method and path.
type tiAdminHub struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
	// status answers every route with an error status when non-zero.
	status int
	// errBody is the error body sent with status.
	errBody string
	// revokeBody answers POST revoke-all.
	revokeBody string
	// residueBody answers GET residue.
	residueBody string
	// summaryBody answers GET health summary.
	summaryBody string
}

func (h *tiAdminHub) client(t *testing.T) hubclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.EscapedPath())
		h.bodies = append(h.bodies, string(body))
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if h.status != 0 {
			w.WriteHeader(h.status)
			_, _ = w.Write([]byte(h.errBody))
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/test-identities/admin":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"identity":{"id":"ta-1","email":"test-admin-abc@scion-test-hub-admin.invalid","role":"member","status":"active",` +
				`"purpose":"admin journey","issuedBy":"issuer-1","expiresAt":"2026-10-15T11:00:00Z","live":true,` +
				`"kind":"test_hub_admin","adminRole":"hub-admin","issuanceId":"iss-1"},` +
				`"accessToken":"` + tiAdminFakeToken + `","tokenType":"Bearer","expiresIn":900,"tokenExpiresAt":"2026-10-15T10:15:00Z"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/test-identities/admin/revoke-all":
			_, _ = w.Write([]byte(h.revokeBody))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/test-identities/admin/") && strings.HasSuffix(r.URL.Path, "/residue"):
			_, _ = w.Write([]byte(h.residueBody))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/health/summary":
			_, _ = w.Write([]byte(h.summaryBody))
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

func (h *tiAdminHub) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

// assertNoAdminToken fails if the token appears in any of the outputs.
func assertNoAdminToken(t *testing.T, outputs ...string) {
	t.Helper()
	for _, o := range outputs {
		assert.NotContains(t, o, tiAdminFakeToken)
		assert.NotContains(t, o, "DO-NOT-PRINT")
	}
}

// Golden: issue --admin-role prints exactly the non-secret facts on
// stdout, nothing on stderr, and writes the token only to the 0600 file;
// the request carries the admin role, purpose and whole-second lifetimes.
func TestHubTestIdentityAdminIssue_GoldenOutputAndFile(t *testing.T) {
	hub := &tiAdminHub{}
	c := hub.client(t)
	out := filepath.Join(t.TempDir(), "hub-admin.token")

	var stdout, stderr bytes.Buffer
	err := issueTestAdminIdentity(context.Background(), &stdout, &stderr, c.TestIdentities(), testAdminIssueOptions{
		AdminRole: "hub-admin", TTL: 15 * time.Minute, Lifetime: time.Hour, Purpose: "admin journey", Out: out,
	})
	require.NoError(t, err)

	golden := "Issued test identity ta-1\n" +
		"  Admin role:    hub-admin\n" +
		"  Kind:          test_hub_admin\n" +
		"  Email:         test-admin-abc@scion-test-hub-admin.invalid\n" +
		"  Issuance ID:   iss-1\n" +
		"  Expires:       2026-10-15T11:00:00Z\n" +
		"  Token expires: 2026-10-15T10:15:00Z (no re-issue)\n" +
		"  Purpose:       admin journey\n" +
		"  Token file:    " + out + " (mode 0600)\n"
	assert.Equal(t, golden, stdout.String())
	assert.Empty(t, stderr.String())
	assertNoAdminToken(t, stdout.String(), stderr.String())

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, tiAdminFakeToken+"\n", string(data))
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(out)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}

	require.Equal(t, []string{"POST /api/v1/test-identities/admin"}, hub.seen())
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(hub.bodies[0]), &body))
	assert.Equal(t, map[string]interface{}{
		"adminRole": "hub-admin", "purpose": "admin journey", "tokenTtlSeconds": float64(900), "lifetimeSeconds": float64(3600),
	}, body)
}

// Golden for JSON output: no token field, and the token never appears.
func TestHubTestIdentityAdminIssue_JSONHasNoToken(t *testing.T) {
	hub := &tiAdminHub{}
	out := filepath.Join(t.TempDir(), "super.token")
	var stdout, stderr bytes.Buffer
	require.NoError(t, issueTestAdminIdentity(context.Background(), &stdout, &stderr, hub.client(t).TestIdentities(), testAdminIssueOptions{
		AdminRole: "super-admin", Purpose: "super-admin journey", Out: out, JSON: true,
	}))
	assertNoAdminToken(t, stdout.String(), stderr.String())
	assert.NotContains(t, stdout.String(), "accessToken")
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.ElementsMatch(t, []string{"identity", "tokenFile", "tokenExpiresAt"}, mapKeys(got))
	assert.Equal(t, out, got["tokenFile"])
	assert.Equal(t, "iss-1", got["identity"].(map[string]interface{})["issuanceId"])
}

// An existing file, a symlink, a dangling symlink and a path under a
// symlinked directory are refused before any request is sent (O_EXCL and
// the parent check), and the existing file is untouched.
func TestHubTestIdentityAdminIssue_RefusesExistingOrSymlinkedOut(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing")
	require.NoError(t, os.WriteFile(existing, []byte("keep"), 0o644))
	paths := map[string]string{"existing": existing}
	if runtime.GOOS != "windows" {
		target := filepath.Join(dir, "target")
		require.NoError(t, os.WriteFile(target, []byte("target"), 0o644))
		link := filepath.Join(dir, "link")
		require.NoError(t, os.Symlink(target, link))
		dangling := filepath.Join(dir, "dangling")
		require.NoError(t, os.Symlink(filepath.Join(dir, "missing"), dangling))
		realDir := filepath.Join(dir, "real")
		require.NoError(t, os.Mkdir(realDir, 0o700))
		linkedDir := filepath.Join(dir, "linked")
		require.NoError(t, os.Symlink(realDir, linkedDir))
		paths["symlink"] = link
		paths["dangling symlink"] = dangling
		paths["symlinked directory"] = filepath.Join(linkedDir, "new.token")
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			hub := &tiAdminHub{}
			var stdout, stderr bytes.Buffer
			err := issueTestAdminIdentity(context.Background(), &stdout, &stderr, hub.client(t).TestIdentities(), testAdminIssueOptions{
				AdminRole: "hub-admin", Purpose: "admin journey", Out: path,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "refusing --out")
			assert.Empty(t, hub.seen(), "no request is sent for a refused --out")
			assertNoAdminToken(t, stdout.String(), stderr.String(), err.Error())
		})
	}
	data, err := os.ReadFile(existing)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(data))
	if runtime.GOOS != "windows" {
		_, err := os.Stat(filepath.Join(dir, "real", "new.token"))
		assert.True(t, os.IsNotExist(err), "nothing is created through the symlinked directory")
	}
}

// Client-side validation refuses before any file or request: the admin
// role, a missing or short purpose, --out, and whole-second durations.
func TestHubTestIdentityAdminIssue_Validation(t *testing.T) {
	hub := &tiAdminHub{}
	c := hub.client(t)
	for name, opts := range map[string]testAdminIssueOptions{
		"empty role":      {AdminRole: "", Purpose: "admin journey"},
		"member role":     {AdminRole: "member", Purpose: "admin journey"},
		"case":            {AdminRole: "Hub-Admin", Purpose: "admin journey"},
		"no purpose":      {AdminRole: "hub-admin"},
		"short purpose":   {AdminRole: "hub-admin", Purpose: "  short "},
		"long purpose":    {AdminRole: "hub-admin", Purpose: strings.Repeat("p", 201)},
		"fractional ttl":  {AdminRole: "hub-admin", Purpose: "admin journey", TTL: 1500 * time.Millisecond},
		"fractional life": {AdminRole: "hub-admin", Purpose: "admin journey", Lifetime: 500 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			opts.Out = filepath.Join(t.TempDir(), "t")
			var stdout, stderr bytes.Buffer
			err := issueTestAdminIdentity(context.Background(), &stdout, &stderr, c.TestIdentities(), opts)
			require.Error(t, err)
			_, statErr := os.Stat(opts.Out)
			assert.True(t, os.IsNotExist(statErr), "no token file for a refused input")
		})
	}
	var stdout, stderr bytes.Buffer
	err := issueTestAdminIdentity(context.Background(), &stdout, &stderr, c.TestIdentities(), testAdminIssueOptions{AdminRole: "hub-admin", Purpose: "admin journey"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--out is required")
	assert.Empty(t, hub.seen())
}

// A refused issuance leaves no file and never prints the token; a 404 is
// the named tier-off error.
func TestHubTestIdentityAdminIssue_FailureLeavesNoFile(t *testing.T) {
	for name, tc := range map[string]struct {
		status  int
		body    string
		tierOff bool
	}{
		"forbidden": {http.StatusForbidden, `{"error":{"code":"forbidden","message":"issuer is not a live super-admin"}}`, false},
		"tier off":  {http.StatusNotFound, `{"error":{"code":"not_found","message":"endpoint not found"}}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			hub := &tiAdminHub{status: tc.status, errBody: tc.body}
			out := filepath.Join(t.TempDir(), "admin.token")
			var stdout, stderr bytes.Buffer
			err := issueTestAdminIdentity(context.Background(), &stdout, &stderr, hub.client(t).TestIdentities(), testAdminIssueOptions{
				AdminRole: "hub-admin", Purpose: "admin journey", Out: out,
			})
			require.Error(t, err)
			assert.Equal(t, tc.tierOff, errors.Is(err, errTestAdminTierOff), "%v", err)
			_, statErr := os.Stat(out)
			assert.True(t, os.IsNotExist(statErr), "no token file is left after a failed issuance")
			assert.Empty(t, stdout.String())
			assertNoAdminToken(t, stdout.String(), stderr.String(), err.Error())
		})
	}
}

// --admin-role is refused in agent mode and with a hub profile, and
// --role cannot be combined with it; all before any hub call.
func TestHubTestIdentityAdminIssue_RefusedContexts(t *testing.T) {
	newIssue := func(t *testing.T, setRole bool) *cobra.Command {
		c := &cobra.Command{Use: "issue"}
		c.Flags().String("role", "member", "")
		c.Flags().String("admin-role", "", "")
		require.NoError(t, c.Flags().Set("admin-role", "hub-admin"))
		if setRole {
			require.NoError(t, c.Flags().Set("role", "viewer"))
		}
		return c
	}

	t.Run("agent mode", func(t *testing.T) {
		profileGateEnv(t, "")
		t.Setenv("SCION_CLI_MODE", "agent")
		err := runHubTestIdentityAdminIssue(newIssue(t, false))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not available in agent mode")
	})
	t.Run("hub profile", func(t *testing.T) {
		profileGateEnv(t, "t1")
		t.Setenv("SCION_CLI_MODE", "human")
		err := runHubTestIdentityAdminIssue(newIssue(t, false))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not available with a hub profile")
	})
	t.Run("role combined", func(t *testing.T) {
		profileGateEnv(t, "")
		t.Setenv("SCION_CLI_MODE", "human")
		err := runHubTestIdentityAdminIssue(newIssue(t, true))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--role and --admin-role cannot be combined")
	})
	for _, what := range []string{"revoke-all", "residue"} {
		t.Run(what+" agent mode", func(t *testing.T) {
			profileGateEnv(t, "")
			t.Setenv("SCION_CLI_MODE", "agent")
			require.Error(t, refuseTestAdminContext(what))
		})
		t.Run(what+" hub profile", func(t *testing.T) {
			profileGateEnv(t, "t1")
			t.Setenv("SCION_CLI_MODE", "human")
			require.Error(t, refuseTestAdminContext(what))
		})
	}
	t.Run("human without profile", func(t *testing.T) {
		profileGateEnv(t, "")
		t.Setenv("SCION_CLI_MODE", "human")
		assert.NoError(t, refuseTestAdminContext("revoke-all"))
	})
}

// CLI mode: revoke-all and residue are human mode only (absent from
// agentAllowed, removed in agent mode) and never run with a hub profile.
// issue stays agent-allowed for the profile path, and refuses --admin-role
// at run time (TestHubTestIdentityAdminIssue_RefusedContexts).
func TestHubTestIdentityAdminCmd_ModeAvailability(t *testing.T) {
	humanOnly := []string{"hub.test-identity.revoke-all", "hub.test-identity.residue"}
	for _, p := range humanOnly {
		require.NotNil(t, resolveCommandPath(rootCmd, p), "%s must exist", p)
		assert.False(t, agentAllowed[p], "%s must not be allowed in agent mode", p)
		assert.False(t, hubProfileAllowed[p], "%s must not run with a hub profile", p)
	}
	issue := resolveCommandPath(rootCmd, "hub.test-identity.issue")
	require.NotNil(t, issue)
	require.NotNil(t, issue.Flags().Lookup("admin-role"))
	assert.Equal(t, "", issue.Flags().Lookup("admin-role").DefValue)

	for _, mode := range []string{"human", "agent"} {
		t.Run(mode, func(t *testing.T) {
			root := &cobra.Command{Use: "scion"}
			hubReal := resolveCommandPath(rootCmd, "hub")
			require.NotNil(t, hubReal)
			root.AddCommand(cloneCommandShape(hubReal))
			t.Setenv("SCION_CLI_MODE", mode)
			applyModeRestrictions(root, resolveMode())
			for _, p := range humanOnly {
				if mode == "human" {
					assert.NotNil(t, resolveCommandPath(root, p), "human mode keeps %s", p)
				} else {
					assert.Nil(t, resolveCommandPath(root, p), "agent mode removes %s", p)
				}
			}
		})
	}

	profileGateEnv(t, "t1")
	for _, p := range humanOnly {
		err := checkHubProfileGate(realCommand(t, p), ModeHuman)
		require.Error(t, err, p)
		assert.Contains(t, err.Error(), "is not available with a hub profile")
	}
}

// revoke-all requires --reason: nothing is sent without one, in either
// mode.
func TestHubTestIdentityRevokeAll_ReasonRequired(t *testing.T) {
	for _, dry := range []bool{false, true} {
		for _, reason := range []string{"", "   "} {
			hub := &tiAdminHub{}
			var stdout bytes.Buffer
			err := revokeAllTestAdminIdentities(context.Background(), &stdout, hub.client(t), testAdminRevokeAllOptions{Reason: reason, DryRun: dry})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--reason is required")
			assert.Empty(t, hub.seen(), "no request without a reason")
			assert.Empty(t, stdout.String())
		}
	}
	cmd := resolveCommandPath(rootCmd, "hub.test-identity.revoke-all")
	require.NotNil(t, cmd)
	require.NotNil(t, cmd.Flags().Lookup("reason"))
	require.NotNil(t, cmd.Flags().Lookup("dry-run"))
	assert.Equal(t, "false", cmd.Flags().Lookup("dry-run").DefValue)
}

// --dry-run changes nothing: it sends exactly one GET (the health summary)
// and never the revoke-all POST, and prints what it read.
func TestHubTestIdentityRevokeAll_DryRunChangesNothing(t *testing.T) {
	hub := &tiAdminHub{summaryBody: `{"hub":{"test_admin_identities_enabled":true,"test_super_admin_identities_enabled":false,"live_test_admin_identities":2}}`}
	var stdout bytes.Buffer
	require.NoError(t, revokeAllTestAdminIdentities(context.Background(), &stdout, hub.client(t), testAdminRevokeAllOptions{Reason: "INC-1 stop", DryRun: true}))
	assert.Equal(t, []string{"GET /api/v1/admin/health/summary"}, hub.seen())
	golden := "Dry run: nothing was changed. Your super-admin authority was not checked.\n" +
		"  Admin tier enabled:       yes\n" +
		"  Super-admin kind enabled: no\n" +
		"  Live identities:          2\n" +
		"  Reason:                   INC-1 stop (not stored by the hub)\n" +
		"Without --dry-run, revoke-all deletes (or suspends) every live test identity with an admin role.\n"
	assert.Equal(t, golden, stdout.String())

	// JSON, tier off, count unknown: still only the GET.
	off := &tiAdminHub{summaryBody: `{"hub":{"test_admin_identities_enabled":false,"live_test_admin_identities":null}}`}
	stdout.Reset()
	require.NoError(t, revokeAllTestAdminIdentities(context.Background(), &stdout, off.client(t), testAdminRevokeAllOptions{Reason: "check", DryRun: true, JSON: true}))
	assert.Equal(t, []string{"GET /api/v1/admin/health/summary"}, off.seen())
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, true, got["dryRun"])
	assert.Equal(t, false, got["adminTierEnabled"])
	assert.NotContains(t, got, "liveIdentities")
	assert.NotContains(t, got, "revoked")

	// A failed read is an error, and still no POST.
	denied := &tiAdminHub{status: http.StatusForbidden, errBody: `{"error":{"code":"forbidden","message":"no"}}`}
	stdout.Reset()
	err := revokeAllTestAdminIdentities(context.Background(), &stdout, denied.client(t), testAdminRevokeAllOptions{Reason: "check", DryRun: true})
	require.Error(t, err)
	assert.Equal(t, []string{"GET /api/v1/admin/health/summary"}, denied.seen())
}

// revoke-all posts once with no body and prints every outcome and the
// reason; JSON carries the same.
func TestHubTestIdentityRevokeAll_Golden(t *testing.T) {
	hub := &tiAdminHub{revokeBody: `{"revoked":[{"id":"ta-1","kind":"test_hub_admin","outcome":"deleted"},{"id":"ta-2","kind":"test_super_admin","outcome":"suspended"}],"count":2}`}
	var stdout bytes.Buffer
	require.NoError(t, revokeAllTestAdminIdentities(context.Background(), &stdout, hub.client(t), testAdminRevokeAllOptions{Reason: " INC-1 stop "}))
	assert.Equal(t, []string{"POST /api/v1/test-identities/admin/revoke-all"}, hub.seen())
	assert.Equal(t, "", hub.bodies[0], "revoke-all sends no body")
	golden := "Revoked 2 test identities with admin roles.\n" +
		"  Reason: INC-1 stop (not stored by the hub)\n" +
		"  test_hub_admin ta-1: deleted\n" +
		"  test_super_admin ta-2: suspended\n" +
		"Next: restart the hub without --test-mode=admin or super-admin.\n"
	assert.Equal(t, golden, stdout.String())

	empty := &tiAdminHub{revokeBody: `{"revoked":[],"count":0}`}
	stdout.Reset()
	require.NoError(t, revokeAllTestAdminIdentities(context.Background(), &stdout, empty.client(t), testAdminRevokeAllOptions{Reason: "drill", JSON: true}))
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, false, got["dryRun"])
	assert.Equal(t, "drill", got["reason"])
	assert.Equal(t, float64(0), got["count"])
}

// revoke-all and residue map the hub's refusal of a non-super-admin
// caller (403, or 401) onto errTestAdminSuperAdminRequired; revoke-all
// maps 404 onto errTestAdminTierOff.
func TestHubTestIdentityAdmin_NamedRefusals(t *testing.T) {
	forbidden := `{"error":{"code":"forbidden","message":"revoke-all requires a human super-admin session","details":{"reason":"test_identity_cannot_issue"}}}`
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		hub := &tiAdminHub{status: status, errBody: forbidden}
		var stdout bytes.Buffer
		err := revokeAllTestAdminIdentities(context.Background(), &stdout, hub.client(t), testAdminRevokeAllOptions{Reason: "INC-1"})
		require.Error(t, err)
		assert.True(t, errors.Is(err, errTestAdminSuperAdminRequired), "revoke-all %d: %v", status, err)
		assert.Empty(t, stdout.String())

		res := &tiAdminHub{status: status, errBody: forbidden}
		err = printTestAdminResidue(context.Background(), &stdout, res.client(t).TestIdentities(), "ta-1", false)
		require.Error(t, err)
		assert.True(t, errors.Is(err, errTestAdminSuperAdminRequired), "residue %d: %v", status, err)
		assert.Empty(t, stdout.String())
	}

	off := &tiAdminHub{status: http.StatusNotFound, errBody: `{"error":{"code":"not_found","message":"endpoint not found"}}`}
	var stdout bytes.Buffer
	err := revokeAllTestAdminIdentities(context.Background(), &stdout, off.client(t), testAdminRevokeAllOptions{Reason: "INC-1"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errTestAdminTierOff), "%v", err)

	missing := &tiAdminHub{status: http.StatusNotFound, errBody: `{"error":{"code":"not_found","message":"test admin identity not found"}}`}
	err = printTestAdminResidue(context.Background(), &stdout, missing.client(t).TestIdentities(), "nope", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a test identity with an admin role")
	assert.False(t, errors.Is(err, errTestAdminSuperAdminRequired))
}

// Golden: residue prints the stamped grants and resources, the unstamped
// changes, kept roles, truncation and the limit; one ID per call.
func TestHubTestIdentityResidue_Golden(t *testing.T) {
	hub := &tiAdminHub{residueBody: `{"identityId":"ta-1","issuanceId":"iss-1",` +
		`"stampedGrants":[{"kind":"role_binding","id":"rb-1","roleDefinitionId":"project-admin","principalType":"user","principalId":"u-2","scopeType":"project","scopeId":"p-1","expiresAt":"2026-10-15T11:00:00Z"},` +
		`{"kind":"group_membership","id":"gm-1","principalType":"user","principalId":"u-3","groupId":"g-1"}],` +
		`"stampedResources":{"agents":["ag-1"],"schedules":[],"groups":["g-2"],"projects":["p-1"]},` +
		`"unstampedChanges":[{"mutationType":"user_suspend","targetType":"user","targetId":"u-4","actorKind":"user","actorId":"ta-1","timestamp":"2026-10-15T10:05:00Z"}],` +
		`"keptRoles":[{"kind":"role_definition","id":"rd-1","name":"ops","boundOutsideIssuance":true}],"truncated":true,"limit":"complete only as far as the mutation audit"}`}
	var stdout bytes.Buffer
	require.NoError(t, printTestAdminResidue(context.Background(), &stdout, hub.client(t).TestIdentities(), "ta-1", false))
	assert.Equal(t, []string{"GET /api/v1/test-identities/admin/ta-1/residue"}, hub.seen())
	golden := "Residue of test identity ta-1 (issuance iss-1)\n" +
		"Stamped grants (removed by a purge): 2\n" +
		"  role_binding rb-1 role=project-admin principal=user:u-2 scope=project:p-1 expires=2026-10-15T11:00:00Z\n" +
		"  group_membership gm-1 principal=user:u-3 group=g-1\n" +
		"Stamped resources (removed by a purge): 1 agents, 0 schedules, 1 groups, 1 projects\n" +
		"  agent ag-1\n" +
		"  group g-2\n" +
		"  project p-1\n" +
		"Changes to rows without its stamp (reported, never reverted): 1\n" +
		"  user_suspend user u-4 by user ta-1 at 2026-10-15T10:05:00Z\n" +
		"Custom roles kept by a purge (bound outside the issuance): 1\n" +
		"  role_definition rd-1 name=ops\n" +
		"(a list hit its bound; more residue exists than is shown)\n" +
		"Limit: complete only as far as the mutation audit\n"
	assert.Equal(t, golden, stdout.String())

	stdout.Reset()
	require.NoError(t, printTestAdminResidue(context.Background(), &stdout, hub.client(t).TestIdentities(), "ta-1", true))
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, "iss-1", got["issuanceId"])

	require.Error(t, printTestAdminResidue(context.Background(), &stdout, hub.client(t).TestIdentities(), " ", false))
	cmd := resolveCommandPath(rootCmd, "hub.test-identity.residue")
	require.NotNil(t, cmd)
	assert.Error(t, cmd.Args(cmd, []string{}))
	assert.Error(t, cmd.Args(cmd, []string{"a", "b"}), "one identity per call")
	assert.NoError(t, cmd.Args(cmd, []string{"a"}))
}

// The hubclient wire types match the hub's JSON field names, so a field
// added on either side fails here (drift guard).
func TestHubTestIdentityAdmin_WireTypesMatchHub(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hub, client interface{}
	}{
		{"issue request", hub.CreateTestAdminIdentityRequest{}, hubclient.IssueTestAdminIdentityRequest{}},
		{"identity", hub.TestAdminIdentityView{}, hubclient.TestAdminIdentity{}},
		{"token response", hub.TestAdminIdentityTokenResponse{}, hubclient.TestAdminIdentityTokenResponse{}},
		{"revoked", hub.RevokedTestAdminIdentity{}, hubclient.RevokedTestAdminIdentity{}},
		{"revoke-all response", hub.RevokeAllTestAdminIdentitiesResponse{}, hubclient.RevokeAllTestAdminIdentitiesResponse{}},
		{"residue report", hub.TestAdminResidueReport{}, hubclient.TestAdminResidueReport{}},
		{"residue grant", store.IssuanceStampedGrant{}, hubclient.TestAdminResidueGrant{}},
		{"residue resources", hub.TestAdminResidueResources{}, hubclient.TestAdminResidueResources{}},
		{"residue change", hub.TestAdminResidueChange{}, hubclient.TestAdminResidueChange{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, jsonFieldNames(reflect.TypeOf(tc.hub)), jsonFieldNames(reflect.TypeOf(tc.client)))
		})
	}
}

// jsonFieldNames lists the JSON names of a struct's fields, flattening
// embedded structs, sorted.
func jsonFieldNames(typ reflect.Type) []string {
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if f.Anonymous && tag == "" && f.Type.Kind() == reflect.Struct {
			names = append(names, jsonFieldNames(f.Type)...)
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// A plain delete of a test identity with an admin role gets the hub's 404
// (only a purge removes one). On the plain human path the command still
// succeeds, so a teardown can run twice, and its notice names
// "delete <id> --purge" (never revoke-all, a hub-wide action); it sends no
// purge itself. The shared deleteTestIdentity (also the profile alias path)
// prints no admin hint.
func TestHubTestIdentityDelete_AdminKind404PointsToPurge(t *testing.T) {
	notFound := `{"error":{"code":"not_found","message":"test identity not found"}}`
	hub := &tiCLIHub{deleteStatus: http.StatusNotFound, deleteBody: notFound}
	var stdout, stderr bytes.Buffer
	require.NoError(t, deleteTestIdentityWithHint(context.Background(), &stdout, &stderr, hub.client(t).TestIdentities(), "ta-1", true))
	assert.Empty(t, stdout.String())
	assert.Equal(t, "Notice: test identity ta-1 was not found as a member or viewer identity (already deleted, or not one you may delete); nothing was deleted.\n"+
		"A test identity with an admin role is not removed by a plain delete: use \"scion hub test-identity delete ta-1 --purge\".\n", stderr.String())
	assert.NotContains(t, stderr.String(), "revoke-all")
	require.Len(t, hub.queries, 1)
	assert.Equal(t, "", hub.queries[0], "a plain delete sends no purge")

	// The shared helper, as the profile alias path calls it: no hint.
	shared := &tiCLIHub{deleteStatus: http.StatusNotFound, deleteBody: notFound}
	stderr.Reset()
	require.NoError(t, deleteTestIdentity(context.Background(), &stdout, &stderr, shared.client(t).TestIdentities(), "fx-1"))
	assert.NotContains(t, stderr.String(), "admin role")
	assert.NotContains(t, stderr.String(), "--purge")
}

// The hint applies only on the human path without a hub profile.
func TestHubTestIdentityDelete_AdminHintContexts(t *testing.T) {
	t.Run("human without profile", func(t *testing.T) {
		profileGateEnv(t, "")
		t.Setenv("SCION_CLI_MODE", "human")
		assert.True(t, testAdminDeleteHintApplies())
	})
	t.Run("agent mode", func(t *testing.T) {
		profileGateEnv(t, "")
		t.Setenv("SCION_CLI_MODE", "agent")
		assert.False(t, testAdminDeleteHintApplies())
	})
	t.Run("hub profile", func(t *testing.T) {
		profileGateEnv(t, "t1")
		t.Setenv("SCION_CLI_MODE", "human")
		assert.False(t, testAdminDeleteHintApplies())
	})
}

// O1: issue dispatches --admin-role before the profile path, so with a hub
// profile selected issue --admin-role is refused, never silently issued as
// a member identity through the profile.
func TestHubTestIdentityIssue_AdminRoleDispatchedBeforeProfile(t *testing.T) {
	profileGateEnv(t, "t1")
	t.Setenv("SCION_CLI_MODE", "human")
	c := &cobra.Command{Use: "issue"}
	c.Flags().String("role", "member", "")
	c.Flags().String("admin-role", "", "")
	require.NoError(t, c.Flags().Set("admin-role", "hub-admin"))
	err := runHubTestIdentityIssue(c, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issue --admin-role is not available with a hub profile")
}

// O2: when the token cannot be written after issuance, the file is
// removed, stderr names the identity and the purge path (no re-issue
// exists), and the token appears nowhere.
func TestHubTestIdentityAdminIssue_WriteFailureAfterIssue(t *testing.T) {
	orig := testAdminTokenWrite
	t.Cleanup(func() { testAdminTokenWrite = orig })
	testAdminTokenWrite = func(f *tokenFile, token string) error {
		_ = f.f.Close() // the real write then fails on the closed file
		return f.write(token)
	}
	hub := &tiAdminHub{}
	out := filepath.Join(t.TempDir(), "admin.token")
	var stdout, stderr bytes.Buffer
	err := issueTestAdminIdentity(context.Background(), &stdout, &stderr, hub.client(t).TestIdentities(), testAdminIssueOptions{
		AdminRole: "hub-admin", Purpose: "admin journey", Out: out,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to write --out")
	assert.Equal(t, "Test identity ta-1 was issued, but its token could not be written. Its token cannot be re-issued; remove it with \"scion hub test-identity delete ta-1 --purge\".\n", stderr.String())
	assert.Empty(t, stdout.String())
	assertNoAdminToken(t, stdout.String(), stderr.String(), err.Error())
	_, statErr := os.Stat(out)
	assert.True(t, os.IsNotExist(statErr), "no partial token file is left")
}

// O3: a revoke-all that fails partway (500 or 503) says that some
// identities may already be revoked and that running it again is safe; it
// is not mistaken for a refusal.
func TestHubTestIdentityRevokeAll_PartialFailureHint(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusInternalServerError} {
		hub := &tiAdminHub{status: status, errBody: `{"error":{"code":"unavailable","message":"test admin audit is unavailable"}}`}
		var stdout bytes.Buffer
		err := revokeAllTestAdminIdentities(context.Background(), &stdout, hub.client(t), testAdminRevokeAllOptions{Reason: "INC-1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "some identities may already be revoked")
		assert.Contains(t, err.Error(), "--dry-run")
		assert.False(t, errors.Is(err, errTestAdminSuperAdminRequired))
		assert.False(t, errors.Is(err, errTestAdminTierOff))
		assert.Equal(t, []string{"POST /api/v1/test-identities/admin/revoke-all"}, hub.seen(), "sent once, not retried")
		assert.Empty(t, stdout.String())
	}
}
