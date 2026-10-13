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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile/hubprofiletest"
)

// Tests for CLI target-hub profiles (ptone/scion#4240, X3).

// profileGateEnv clears the environment the gate reads and selects a
// profile by SCION_HUB_PROFILE when name is non-empty.
func profileGateEnv(t *testing.T, name string) {
	t.Helper()
	for _, k := range []string{"SCION_HUB_PROFILE", "SCION_HUB_RUN", "SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_CLI_MODE", "SCION_PROJECT"} {
		t.Setenv(k, "")
	}
	if name != "" {
		t.Setenv(hubprofile.EnvHubProfile, name)
	}
	origFlag, origAs, origRun, origHub, origNoHub := hubProfileFlag, hubProfileAs, hubProfileRun, hubEndpoint, noHub
	t.Cleanup(func() {
		hubProfileFlag, hubProfileAs, hubProfileRun, hubEndpoint, noHub = origFlag, origAs, origRun, origHub, origNoHub
	})
	hubProfileFlag, hubProfileAs, hubProfileRun, hubEndpoint, noHub = "", "", "", "", false
}

func realCommand(t *testing.T, path string) *cobra.Command {
	t.Helper()
	c := resolveCommandPath(rootCmd, path)
	require.NotNil(t, c, "command %s must exist", path)
	return c
}

// With a profile selected, every command outside the allow-list is
// refused before it runs.
func TestHubProfileGate_UnlistedCommandsRefused(t *testing.T) {
	profileGateEnv(t, "t1")
	refused := []string{
		"whoami", "message", "resume", "suspend", "reincarnate", "attach", "keys", "schedule.create",
		"expose", "unexpose", "doctor", "hub.add", "hub.list", "hub.rename", "hub.remove",
		"hub.enable", "hub.link", "hub.secret", "hub.auth.login", "hub.token", "hub.env",
		"hub.test-identity.delegate.create", "hub.test-identity.delegate.list", "hub.test-identity.delegate.revoke",
		"artifact.share", "templates.import", "server", "clean", "sync", "config.set", "project.list",
	}
	for _, p := range refused {
		err := checkHubProfileGate(realCommand(t, p), ModeHuman)
		require.Error(t, err, p)
		assert.Contains(t, err.Error(), "is not available with a hub profile", p)
	}
	allowed := []string{
		"hub.status", "hub.profile.add", "hub.profile.list", "hub.profile.show", "hub.profile.remove",
		"hub.test-identity.issue", "hub.test-identity.token", "hub.test-identity.list",
		"hub.test-identity.delete", "hub.test-identity.teardown", "hub.test-identity.web-code",
		"hub.projects", "hub.projects.info", "hub.projects.create", "hub.projects.delete",
		"artifact.publish", "artifact.get", "artifact.versions", "artifact.delete",
		"create", "start", "list", "stop", "delete", "look", "logs",
	}
	for _, p := range allowed {
		assert.NoError(t, checkHubProfileGate(realCommand(t, p), ModeHuman), p)
	}
	// Every allow-listed path is a real command.
	for p, ok := range hubProfileAllowed {
		if ok && !strings.HasPrefix(p, "__") && p != "help" && p != "completion" {
			assert.NotNil(t, resolveCommandPath(rootCmd, p), "allow-listed %s must exist", p)
		}
	}

	hubEndpoint = "https://other.example.com"
	err := checkHubProfileGate(realCommand(t, "hub.status"), ModeHuman)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--hub and --no-hub cannot be used with a hub profile")
}

// --as and --run without a profile are refused (they never select an
// identity on the CLI's own hub).
func TestHubProfileGate_AsAndRunNeedAProfile(t *testing.T) {
	profileGateEnv(t, "")
	_, err := runProfileRoot(t, "hub", "status", "--as", "owner")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--as needs a hub profile")
	_, err = runProfileRoot(t, "hub", "status", "--run", "r1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--run needs a hub profile")

	// The flags belong to the hub and artifact trees and the agent
	// commands that run with a profile (N2, X3b).
	assert.Nil(t, rootCmd.PersistentFlags().Lookup("as"))
	assert.Nil(t, rootCmd.PersistentFlags().Lookup("run"))
	assert.NotNil(t, hubCmd.PersistentFlags().Lookup("as"))
	assert.NotNil(t, artifactCmd.PersistentFlags().Lookup("run"))
	_, err = runProfileRoot(t, "artifact", "get", "x", "--as", "owner")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--as needs a hub profile")
	_, err = runProfileRoot(t, "artifact", "versions", "x", "--run", "r1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--run needs a hub profile")
	_, err = runProfileRoot(t, "list", "--as", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--as needs a hub profile")
	_, err = runProfileRoot(t, "start", "a1", "--run", "r1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--run needs a hub profile")
	_, err = runProfileRoot(t, "whoami", "--as", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown flag: --as")
	_, err = runProfileRoot(t, "message", "a1", "hi", "--run", "r1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown flag: --run")
}

// Agent mode (ptone/scion#4240): hub status and the test-identity commands refuse
// without a target profile, and with a profile whose endpoint is the
// agent's own hub; they pass the gate with a profile for another hub.
// Delegation create is not agent-allowed at all (see
// TestHubTestIdentityCmd_ModeAvailability).
func TestHubProfileGate_AgentModeRule(t *testing.T) {
	profileGateEnv(t, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	runtime := []string{"hub.status", "hub.test-identity.issue", "hub.test-identity.token", "hub.test-identity.list", "hub.test-identity.delete", "hub.test-identity.teardown", "hub.test-identity.web-code"}

	t.Run("no profile", func(t *testing.T) {
		for _, p := range runtime {
			err := checkHubProfileGate(realCommand(t, p), ModeAgent)
			require.Error(t, err, p)
			assert.Contains(t, err.Error(), "runs only against a target-hub profile", p)
		}
		// Human mode: no profile needed.
		assert.NoError(t, checkHubProfileGate(realCommand(t, "hub.status"), ModeHuman))
		// Profile management needs no selected profile.
		assert.NoError(t, checkHubProfileGate(realCommand(t, "hub.profile.list"), ModeAgent))
	})

	require.NoError(t, hubprofile.Save(home, &hubprofile.Profile{Name: "own", Endpoint: "https://hub.example.com", Transport: hubprofile.TransportNone}, false))
	require.NoError(t, hubprofile.Save(home, &hubprofile.Profile{Name: "t1", Endpoint: "https://t1.example.com", Transport: hubprofile.TransportNone}, false))
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com/")

	t.Run("profile for the agent's own hub", func(t *testing.T) {
		t.Setenv(hubprofile.EnvHubProfile, "own")
		for _, p := range runtime {
			err := checkHubProfileGate(realCommand(t, p), ModeAgent)
			require.ErrorIs(t, err, hubprofile.ErrOwnHub, p)
		}
	})
	t.Run("profile for another hub", func(t *testing.T) {
		t.Setenv(hubprofile.EnvHubProfile, "t1")
		for _, p := range runtime {
			assert.NoError(t, checkHubProfileGate(realCommand(t, p), ModeAgent), p)
		}
	})
	t.Run("end to end: agent-mode hub status without a profile", func(t *testing.T) {
		t.Setenv(hubprofile.EnvHubProfile, "")
		t.Setenv("SCION_CLI_MODE", "agent")
		saveCLIModeState(t)
		_, err := runProfileRoot(t, "hub", "status")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "runs only against a target-hub profile")
	})
	t.Run("end to end: agent-mode hub status with an own-hub profile", func(t *testing.T) {
		t.Setenv(hubprofile.EnvHubProfile, "own")
		t.Setenv("SCION_CLI_MODE", "agent")
		saveCLIModeState(t)
		_, err := runProfileRoot(t, "hub", "status")
		require.ErrorIs(t, err, hubprofile.ErrOwnHub)
	})
	t.Run("agent-mode profile add for the own hub", func(t *testing.T) {
		t.Setenv(hubprofile.EnvHubProfile, "")
		t.Setenv("SCION_CLI_MODE", "agent")
		saveCLIModeState(t)
		_, err := runProfileRoot(t, "hub", "profile", "add", "own2", "--endpoint", "https://hub.example.com", "--transport", "none")
		require.ErrorIs(t, err, hubprofile.ErrOwnHub)
	})
}

type profileRun struct {
	stdout, stderr string
}

// runProfileRoot runs rootCmd with args, with every flag and package var a
// previous run may have set returned to its default.
func runProfileRoot(t *testing.T, args ...string) (profileRun, error) {
	t.Helper()
	restoreAllSilenceUsage(t)
	resetRootProjectFlag(t)
	saveCLIModeState(t)
	for _, name := range []string{"hub-profile", "hub", "no-hub", "format", "global"} {
		f := rootCmd.PersistentFlags().Lookup(name)
		require.NotNil(t, f, name)
		require.NoError(t, f.Value.Set(f.DefValue))
		f.Changed = false
	}
	for _, c := range []*cobra.Command{hubCmd, artifactCmd} {
		for _, name := range []string{"as", "run"} {
			f := c.PersistentFlags().Lookup(name)
			require.NotNil(t, f, name)
			require.NoError(t, f.Value.Set(f.DefValue))
			f.Changed = false
		}
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		resetCommandFlags(t, c)
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	for _, p := range []string{"hub.status", "hub.profile", "hub.test-identity", "hub.projects", "artifact",
		"create", "start", "list", "stop", "delete", "look", "logs"} {
		walk(realCommand(t, p))
	}
	hubOutputJSON, outputFormat, projectPath = false, "", ""
	t.Cleanup(func() { rootCmd.SetArgs(nil); hubOutputJSON, outputFormat = false, "" })
	rootCmd.SetArgs(args)
	var err error
	var r profileRun
	r.stdout, r.stderr = captureStdoutStderr(t, func() { err = rootCmd.Execute() })
	return r, err
}

// snapshotTree records every file and directory under root (path, mode,
// size and content hash), skipping the paths skip accepts.
func snapshotTree(t *testing.T, root string, skip func(rel string) bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		v := fmt.Sprintf("%v %d %d", fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		if fi.Mode().IsRegular() && fi.Mode().Perm()&0o400 != 0 {
			data, err := os.ReadFile(p)
			if err == nil {
				sum := sha256.Sum256(data)
				v += " " + hex.EncodeToString(sum[:])
			}
		}
		out[rel] = v
		return nil
	}))
	return out
}

// X3-1, X3-3, X3-4 (success), X3-5, X3-6 and the allow-list's no-local-state
// rule, end to end through rootCmd, against a target hub that verifies
// credentials. The agent's own hub, its scion-token, OAuth credentials for
// the target, SCION_AUTH_TOKEN, SCION_HUB_TOKEN and SCION_DEV_TOKEN are all
// present; none may ever be used, no token may reach stdout or stderr, and
// neither the local project nor ~/.scion (outside hub-profiles) may be read
// into a client or changed.
func TestHubProfile_EndToEnd_GoldenNoFallbackNoLeakNoLocalState(t *testing.T) {
	profileGateEnv(t, "")
	clk := hubprofiletest.NewClock()
	gcp := hubprofiletest.NewGCP(t, clk)
	target := hubprofiletest.NewTarget(t, clk, true)

	// The agent's own hub: any request to it fails the test.
	var ownHits atomic.Int32
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ownHits.Add(1)
		http.Error(w, "own hub must not be contacted", http.StatusTeapot)
	}))
	defer own.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte("AMBIENT-SCION-TOKEN-FILE\n"), 0o600))
	origCredPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string { return filepath.Join(home, ".scion", "credentials.json") })
	t.Cleanup(func() { credentials.SetCredentialsPath(origCredPath) })
	require.NoError(t, credentials.Store(target.Srv.URL, &credentials.TokenResponse{AccessToken: "AMBIENT-OAUTH-TOKEN", ExpiresIn: 3600e9}))
	for k, v := range gcp.Env() {
		t.Setenv(k, v)
	}
	t.Setenv("SCION_DEV_TOKEN", "AMBIENT-DEV-TOKEN")
	t.Setenv("SCION_HUB_ENDPOINT", own.URL)
	t.Setenv("SCION_CLI_MODE", "agent") // the runner case
	t.Setenv("SCION_NETWORK_MODE", "host")

	// A local project whose settings point at the agent's own hub.
	proj := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(proj, ".scion"), 0o755))
	settings, _ := json.Marshal(map[string]any{"project_id": "local-project", "hub": map[string]any{"enabled": true, "endpoint": own.URL}})
	require.NoError(t, os.WriteFile(filepath.Join(proj, ".scion", "settings.json"), settings, 0o644))
	t.Chdir(proj)

	origDeps := hubProfileDeps
	hubProfileDeps = func() hubprofile.Deps { return hubprofile.Deps{IAMCredentialsURL: gcp.IAM.URL} }
	t.Cleanup(func() { hubProfileDeps = origDeps })

	// Add the profile (no profile selected yet).
	_, err := runProfileRoot(t, "hub", "profile", "add", "t1", "--endpoint", target.Srv.URL, "--transport", "iap",
		"--iap-audience", hubprofiletest.IAPAudience, "--uat-sa", hubprofiletest.UATSA,
		"--issuer-sa", hubprofiletest.IssuerSA, "--issuer-delegation", "member="+hubprofiletest.Delegation)
	require.NoError(t, err)

	projBefore := snapshotTree(t, proj, nil)
	homeBefore := snapshotTree(t, filepath.Join(home, ".scion"), func(rel string) bool { return strings.HasPrefix(rel, "hub-profiles") })

	var outputs []string
	run := func(args ...string) (profileRun, error) {
		t.Helper()
		r, err := runProfileRoot(t, append([]string{"--hub-profile", "t1"}, args...)...)
		outputs = append(outputs, r.stdout, r.stderr)
		if err != nil {
			outputs = append(outputs, err.Error())
		}
		return r, err
	}

	r, err := run("hub", "test-identity", "issue", "--as", "owner", "--run", "r1", "--purpose", "journey")
	require.NoError(t, err, r.stderr)
	assert.Contains(t, r.stdout, `as "owner" (run r1, profile t1)`)
	assert.Contains(t, r.stdout, "(mode 0600)")
	cache := filepath.Join(home, ".scion", "hub-profiles", "t1", "run-r1", "owner.json")
	fi, err := os.Lstat(cache)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	r, err = run("hub", "status", "--as", "owner", "--run", "r1", "--json")
	require.NoError(t, err, r.stdout+r.stderr)
	var st hubprofile.StatusReport
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &st), r.stdout)
	assert.True(t, st.OK)
	assert.Equal(t, "profile:owner", st.BearerSource)
	assert.Equal(t, hubprofile.KindTestFixture, st.Identity.Kind)
	assert.Equal(t, "member", st.Identity.Tier)
	assert.Equal(t, hubprofiletest.UATSA, st.Transport.PrincipalEmail)
	assert.Equal(t, hubprofiletest.IAPAudience, st.Transport.Audience)

	r, err = run("hub", "status", "--run", "r1")
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Contains(t, r.stdout, "Bearer source:   profile:issuer")
	assert.Contains(t, r.stdout, "Result:          ok")

	r, err = run("hub", "projects", "--as", "owner", "--run", "r1")
	require.NoError(t, err, r.stderr)

	r, err = run("hub", "test-identity", "list", "--run", "r1")
	require.NoError(t, err, r.stderr)
	assert.Contains(t, r.stdout, "scion-run:r1/owner journey")

	r, err = run("hub", "test-identity", "token", "--as", "owner", "--run", "r1")
	require.NoError(t, err, r.stderr)
	assert.Contains(t, r.stdout, "New token for test identity")
	assert.Equal(t, 1, target.TokenHits())

	// An unlisted command under the profile is refused and touches nothing.
	_, err = run("resume", "a1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not available with a hub profile")
	_, err = run("whoami")
	require.Error(t, err)

	r, err = run("hub", "test-identity", "teardown", "--run", "r1")
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Contains(t, r.stdout, "torn down; its cache was removed")
	assert.Empty(t, target.LiveIdentityIDs())
	_, err = os.Stat(filepath.Dir(cache))
	assert.True(t, os.IsNotExist(err))

	// No fallback: the delegation is gone from the profile and no --as is
	// given, so the command fails without any request at all.
	require.NoError(t, hubprofile.Save(home, &hubprofile.Profile{Name: "t1", Endpoint: target.Srv.URL, Transport: hubprofile.TransportIAP, IAPAudience: hubprofiletest.IAPAudience, UATServiceAccount: hubprofiletest.UATSA}, true))
	before := len(target.Recorded())
	_, err = run("hub", "test-identity", "list")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no profile credential applies")
	_, err = run("hub", "status")
	require.Error(t, err)
	assert.Len(t, target.Recorded(), before, "no request without a profile credential")

	// Never the agent's own hub, never an ambient credential.
	assert.EqualValues(t, 0, ownHits.Load(), "the agent's own hub was contacted")
	reqs := target.Recorded()
	require.NotEmpty(t, reqs)
	for _, rq := range reqs {
		for k, vs := range rq.Header {
			for _, v := range vs {
				assert.NotContains(t, v, "AMBIENT", "%s %s header %s", rq.Method, rq.Path, k)
			}
		}
		assert.Empty(t, rq.Header.Get("X-Scion-Agent-Token"), rq.Path)
	}

	// No token in stdout, stderr or errors (golden).
	secrets := append(gcp.MintedTokens(), target.FixtureTokens()...)
	secrets = append(secrets, hubprofiletest.AccessToken)
	require.NotEmpty(t, target.FixtureTokens())
	all := strings.Join(outputs, "\n")
	for _, s := range secrets {
		assert.NotContains(t, all, s)
		if parts := strings.Split(s, "."); len(parts) == 3 {
			assert.NotContains(t, all, parts[1])
		}
	}
	assert.NotContains(t, all, "AMBIENT")
	// The environment never received a token.
	for _, kv := range os.Environ() {
		for _, s := range secrets {
			assert.NotContains(t, kv, s)
		}
	}

	// No local project state was written, and ~/.scion is unchanged
	// outside hub-profiles.
	assert.Equal(t, projBefore, snapshotTree(t, proj, nil), "the local project changed")
	assert.Equal(t, homeBefore, snapshotTree(t, filepath.Join(home, ".scion"), func(rel string) bool { return strings.HasPrefix(rel, "hub-profiles") }), "~/.scion changed")
}

// With a profile, no local project settings are read: a project whose
// settings file is unreadable (mode 000) does not affect an allow-listed
// command. (The end-to-end test also shows the local project and ~/.scion
// are unchanged and the agent's own hub, named by those settings, is never
// contacted.)
func TestHubProfile_ReadsNoLocalProjectSettings(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 file")
	}
	profileGateEnv(t, "")
	clk := hubprofiletest.NewClock()
	gcp := hubprofiletest.NewGCP(t, clk)
	target := hubprofiletest.NewTarget(t, clk, false)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for k, v := range gcp.Env() {
		t.Setenv(k, v)
	}
	t.Setenv("SCION_NETWORK_MODE", "host")
	origDeps := hubProfileDeps
	hubProfileDeps = func() hubprofile.Deps { return hubprofile.Deps{IAMCredentialsURL: gcp.IAM.URL} }
	t.Cleanup(func() { hubProfileDeps = origDeps })
	require.NoError(t, hubprofile.Save(home, &hubprofile.Profile{Name: "t1", Endpoint: target.Srv.URL, Transport: hubprofile.TransportNone, IssuerServiceAccount: hubprofiletest.IssuerSA, IssuerDelegations: map[string]string{"member": hubprofiletest.Delegation}}, false))

	proj := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(proj, ".scion"), 0o755))
	sp := filepath.Join(proj, ".scion", "settings.json")
	require.NoError(t, os.WriteFile(sp, []byte(`{"hub":{"enabled":true,"endpoint":"http://127.0.0.1:1"}}`), 0o000))
	t.Chdir(proj)

	r, err := runProfileRoot(t, "--hub-profile", "t1", "hub", "test-identity", "list")
	require.NoError(t, err, "with a profile the unreadable local settings are never read: %s", r.stderr)
	assert.Contains(t, r.stdout, "No test identities.")

	// hub projects reaches the target through loadHubClient (the shared
	// helper of the general hub commands): it must not read the settings
	// either.
	r, err = runProfileRoot(t, "--hub-profile", "t1", "hub", "test-identity", "issue", "--as", "owner", "--run", "r1")
	require.NoError(t, err, r.stderr)
	r, err = runProfileRoot(t, "--hub-profile", "t1", "hub", "projects", "--as", "owner", "--run", "r1")
	require.NoError(t, err, "hub projects with a profile must not read the unreadable local settings: %s", r.stderr)
	assert.Contains(t, r.stdout, "No projects found")
}

// artifact commands under a profile build the client from the profile and
// the target project from --project, with no local settings.
func TestRequireArtifactProfileClient(t *testing.T) {
	profileGateEnv(t, "t1")
	clk := hubprofiletest.NewClock()
	gcp := hubprofiletest.NewGCP(t, clk)
	target := hubprofiletest.NewTarget(t, clk, false)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for k, v := range gcp.Env() {
		t.Setenv(k, v)
	}
	origDeps := hubProfileDeps
	hubProfileDeps = func() hubprofile.Deps { return hubprofile.Deps{IAMCredentialsURL: gcp.IAM.URL} }
	t.Cleanup(func() { hubProfileDeps = origDeps })
	require.NoError(t, hubprofile.Save(home, &hubprofile.Profile{Name: "t1", Endpoint: target.Srv.URL, Transport: hubprofile.TransportNone, IssuerServiceAccount: hubprofiletest.IssuerSA, IssuerDelegations: map[string]string{"member": hubprofiletest.Delegation}}, false))

	// Without --as: refused (the issuer credential is for test identity
	// routes only).
	_, _, err := requireArtifactHubClient()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--as <alias>")

	// Issue an alias, then build the artifact client as it.
	hubProfileRun = "r1"
	s, err := openHubProfileIssuerSession()
	require.NoError(t, err)
	_, err = hubprofile.IssueToCache(t.Context(), s, hubprofile.IssueRequest{Alias: "owner"})
	require.NoError(t, err)
	hubProfileAs = "owner"
	settings, client, err := requireArtifactHubClient()
	require.NoError(t, err)
	assert.Equal(t, target.Srv.URL, settings.GetHubEndpoint(), "artifact refs name the target hub")
	assert.Empty(t, settings.GetHubProjectID())
	err = checkArtifactPublishScope(settings)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--project")
	me, err := client.Auth().Me(t.Context())
	require.NoError(t, err)
	assert.Contains(t, me.Email, "@scion-fixture.invalid")
}

// bearerSourceLabel names the bearer source without a profile.
func TestBearerSourceLabel(t *testing.T) {
	for _, tc := range []struct {
		info authInfo
		want string
	}{
		{authInfo{MethodType: "oauth"}, "OAuth"},
		{authInfo{MethodType: "agent_token", Source: "scion-token file"}, "agent-token"},
		{authInfo{MethodType: "agent_token", Source: "SCION_AUTH_TOKEN env"}, "env"},
		{authInfo{MethodType: "bearer", Source: "SCION_HUB_TOKEN env"}, "env"},
		{authInfo{MethodType: "devauth"}, "dev"},
		{authInfo{MethodType: "proxy-auth"}, "transport"},
		{authInfo{MethodType: "none"}, "none"},
	} {
		assert.Equal(t, tc.want, bearerSourceLabel(tc.info))
	}
}
