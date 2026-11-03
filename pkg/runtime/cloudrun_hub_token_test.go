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

//go:build unix

package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// crHubTokenCanary is an obviously fake credential. It must reach the
// instance only inside the decoded staged blob.
const crHubTokenCanary = "fake-hub-token-canary-7239-not-a-credential"

// writeCRHubToken writes the token file where pkg/agent writes it, below a
// fresh broker-side agent home, and returns the home.
func writeCRHubToken(t *testing.T, token string) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "agent-home")
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, cloudRunHubTokenRel), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// crCreatedContainer returns the single created instance's container.
func crCreatedContainer(t *testing.T, fake *fakeInstancesClient) *runpb.Container {
	t.Helper()
	if len(fake.createReqs) != 1 {
		t.Fatalf("CreateInstance called %d times, want 1", len(fake.createReqs))
	}
	return fake.createReqs[0].Instance.Containers[0]
}

// assertCanaryOnlyInStagedBlob checks that the canary is in the staged
// blob's HubToken field and in no env value (raw), arg or command.
func assertCanaryOnlyInStagedBlob(t *testing.T, c *runpb.Container) {
	t.Helper()
	var blob string
	for _, ev := range c.Env {
		v, _ := ev.GetValues().(*runpb.EnvVar_Value)
		if v == nil {
			continue
		}
		if strings.Contains(v.Value, crHubTokenCanary) {
			t.Errorf("env var %s carries the raw hub credential", ev.Name)
		}
		if ev.Name == stagedsecrets.EnvVar {
			blob = v.Value
		}
	}
	for i, a := range append(append([]string{}, c.Command...), c.Args...) {
		if strings.Contains(a, crHubTokenCanary) {
			t.Errorf("command/arg %d carries the hub credential", i)
		}
	}
	if blob == "" {
		t.Fatalf("%s not set; the hub credential was not staged", stagedsecrets.EnvVar)
	}
	staged, err := stagedsecrets.Decode(blob)
	if err != nil {
		t.Fatalf("decode %s: %v", stagedsecrets.EnvVar, err)
	}
	if staged.HubToken != crHubTokenCanary {
		t.Errorf("staged HubToken present=%v, want the agent's hub credential", staged.HubToken != "")
	}
	for _, fs := range staged.FileSecrets {
		if strings.HasSuffix(fs.Target, cloudRunHubTokenRel) {
			t.Errorf("hub credential staged as a file secret (target %s)", fs.Target)
		}
	}
}

// With a broker-local home, the hub credential is carried in the staged
// blob's dedicated field, never in an env value of its own or in argv.
func TestCloudRunRun_LocalHome_StagesHubTokenInBlobOnly(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	logs := captureRuntimeLog(t)

	cfg := runConfigForTest()
	cfg.UnixUsername = "scion"
	cfg.HomeDir = writeCRHubToken(t, crHubTokenCanary)
	cfg.CommandArgs = []string{"echo", "hello"}

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertCanaryOnlyInStagedBlob(t, crCreatedContainer(t, fake))
	if strings.Contains(logs.String(), crHubTokenCanary) {
		t.Error("runtime log carries the hub credential")
	}
}

// The token is staged alongside other secrets without disturbing them.
func TestCloudRunRun_LocalHome_StagesHubTokenWithSecrets(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)

	cfg := runConfigForTest()
	cfg.UnixUsername = "scion"
	cfg.Env = []string{"PRESET_KEY=from-env"}
	cfg.ResolvedSecrets = crTestResolvedSecrets()
	cfg.HomeDir = writeCRHubToken(t, crHubTokenCanary)

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertResolvedSecretsInEnv(t, crInstanceEnv(t, fake))
	assertCanaryOnlyInStagedBlob(t, crCreatedContainer(t, fake))
}

// The instance's home is the agent's NFS home leaf, mounted at
// /home/<user>, in both homes modes: the runtime does not read the homes
// setting, so sciontool init writes the credential to
// /home/<user>/.scion/scion-token on that leaf either way.
func TestCloudRunRun_HomeIsNFSLeafInBothHomesModes(t *testing.T) {
	for _, mode := range []string{"", "local", HomeStorageNFS} {
		t.Run("homes="+mode, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt := newFakeCloudRunRuntime(t, fake)

			cfg := runConfigForTest()
			cfg.UnixUsername = "scion"
			cfg.HomeStorageBackend = mode
			cfg.HomeDir = writeCRHubToken(t, crHubTokenCanary)

			if _, err := rt.Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			inst := fake.createReqs[0].Instance
			var homeVol *runpb.Volume
			for _, v := range inst.Volumes {
				if v.Name == "home" {
					homeVol = v
				}
			}
			if homeVol == nil || homeVol.GetNfs() == nil {
				t.Fatalf("instance has no NFS home volume")
			}
			if got, want := homeVol.GetNfs().GetPath(), "/ws/projects/proj/agents/agent-1/home"; got != want {
				t.Errorf("home volume path = %q, want the agent's home leaf %q", got, want)
			}
			mounted := false
			for _, m := range inst.Containers[0].VolumeMounts {
				if m.Name == "home" && m.MountPath == "/home/"+rt.ExecUser() {
					mounted = true
				}
			}
			if !mounted {
				t.Errorf("home leaf not mounted at /home/%s", rt.ExecUser())
			}
			assertCanaryOnlyInStagedBlob(t, inst.Containers[0])
		})
	}
}

// No broker-side home or no token file: nothing is staged and nothing is
// reported (the start simply has no hub credential).
func TestCloudRunRun_NoHubTokenStagesNothing(t *testing.T) {
	for name, home := range map[string]func(t *testing.T) string{
		"no home":       func(t *testing.T) string { return "" },
		"no token file": func(t *testing.T) string { return t.TempDir() },
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt := newFakeCloudRunRuntime(t, fake)
			logs := captureRuntimeLog(t)
			cfg := runConfigForTest()
			cfg.HomeDir = home(t)
			if _, err := rt.Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if _, ok := crInstanceEnv(t, fake)[stagedsecrets.EnvVar]; ok {
				t.Errorf("%s set with no secrets and no hub credential", stagedsecrets.EnvVar)
			}
			if strings.Contains(logs.String(), "hub credential is not staged") {
				t.Error("a start with no token file reported a refused credential")
			}
		})
	}
}

// A symlinked or hard-linked token file, or a linked .scion directory, is
// refused at the broker read: nothing is staged, the start continues, and
// the warning names the condition without any file content.
func TestCloudRunRun_LinkedHubTokenRefused(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, home, other string){
		"symlinked token file": func(t *testing.T, home, other string) {
			if err := os.Symlink(other, filepath.Join(home, cloudRunHubTokenRel)); err != nil {
				t.Fatal(err)
			}
		},
		"hard-linked token file": func(t *testing.T, home, other string) {
			if err := os.Link(other, filepath.Join(home, cloudRunHubTokenRel)); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked .scion directory": func(t *testing.T, home, other string) {
			if err := os.Remove(filepath.Join(home, ".scion")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Dir(other), filepath.Join(home, ".scion")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt := newFakeCloudRunRuntime(t, fake)
			logs := captureRuntimeLog(t)

			home := filepath.Join(t.TempDir(), "agent-home")
			if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
				t.Fatal(err)
			}
			otherDir := t.TempDir()
			other := filepath.Join(otherDir, "scion-token")
			if err := os.WriteFile(other, []byte(crHubTokenCanary), 0o600); err != nil {
				t.Fatal(err)
			}
			plant(t, home, other)

			cfg := runConfigForTest()
			cfg.HomeDir = home
			if _, err := rt.Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			c := crCreatedContainer(t, fake)
			for _, ev := range c.Env {
				if ev.Name == stagedsecrets.EnvVar {
					t.Errorf("%s set: a linked token file was staged", stagedsecrets.EnvVar)
				}
			}
			out := logs.String()
			if !strings.Contains(out, "hub credential is not staged") || !strings.Contains(out, hubTokenCondLinked) {
				t.Errorf("no warning naming the linked-file condition (log length %d)", len(out))
			}
			if strings.Contains(out, crHubTokenCanary) {
				t.Error("warning carries file content")
			}
		})
	}
}

// A home path that ValidateAgentHomeSource refuses (here a relative path)
// stages nothing, and Run warns with the home condition only.
func TestCloudRunRun_RefusedHomePathStagesNothing(t *testing.T) {
	if tok, cond := readCloudRunHubToken("relative/agent-home"); tok != "" || cond != hubTokenCondHome {
		t.Errorf("readCloudRunHubToken(relative) = token present %v, condition %q; want none and %q", tok != "", cond, hubTokenCondHome)
	}
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	logs := captureRuntimeLog(t)
	cfg := runConfigForTest()
	cfg.HomeDir = "relative/agent-home"
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := crInstanceEnv(t, fake)[stagedsecrets.EnvVar]; ok {
		t.Errorf("%s set although the home path was refused", stagedsecrets.EnvVar)
	}
	out := logs.String()
	if !strings.Contains(out, "hub credential is not staged") || !strings.Contains(out, hubTokenCondHome) {
		t.Errorf("no warning naming the home condition (log length %d)", len(out))
	}
}

// An oversized token file is refused by condition.
func TestReadCloudRunHubToken_TooLarge(t *testing.T) {
	home := writeCRHubToken(t, strings.Repeat("x", cloudRunHubTokenMaxBytes+1))
	tok, cond := readCloudRunHubToken(home)
	if tok != "" || cond != hubTokenCondTooLarge {
		t.Errorf("got token present=%v condition %q, want none and %q", tok != "", cond, hubTokenCondTooLarge)
	}
}

// The other runtimes' staged payloads are unchanged. The decoded blob the
// shared helpers build without a token is exactly the JSON the format had
// before HubToken existed (written out here, not derived from the code
// under test), and it has no hub_token key at all.
func TestStagedPayloadUnchangedWithoutHubToken(t *testing.T) {
	want := `{"file_secrets":[{"name":"creds","target":"` + crTestFileTarget + `","value":"` +
		base64.StdEncoding.EncodeToString([]byte(crTestFileSecretValue)) +
		`"}],"variable_secrets":{"EXAMPLE_REGION":"` + crTestVarSecretValue + `"}}`

	decodeJSON := func(t *testing.T, blob string) []byte {
		t.Helper()
		raw, err := base64.StdEncoding.DecodeString(blob)
		if err != nil {
			t.Fatalf("decode blob: %v", err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("blob JSON: %v", err)
		}
		if _, ok := keys["hub_token"]; ok {
			t.Error("hub_token key present in a payload built without a token")
		}
		return raw
	}

	// docker, podman, apple: prepareContainerSecretEnv.
	dockerCfg := RunConfig{UnixUsername: "scion", ResolvedSecrets: crTestResolvedSecrets()}
	if err := prepareContainerSecretEnv(&dockerCfg); err != nil {
		t.Fatal(err)
	}
	// cloudrun-sandbox (and cloudrun before this change): applyResolvedSecretsToEnv.
	flatCfg := RunConfig{UnixUsername: "scion", ResolvedSecrets: crTestResolvedSecrets()}
	if _, err := applyResolvedSecretsToEnv(&flatCfg, cloudRunEnvLimit); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string][]string{"prepareContainerSecretEnv": dockerCfg.Env, "applyResolvedSecretsToEnv": flatCfg.Env} {
		found := false
		for _, e := range env {
			if k, v, _ := strings.Cut(e, "="); k == stagedsecrets.EnvVar {
				found = true
				if got := string(decodeJSON(t, v)); got != want {
					t.Errorf("%s: staged JSON changed:\n got %s\nwant %s", name, got, want)
				}
			}
		}
		if !found {
			t.Errorf("%s: no %s entry", name, stagedsecrets.EnvVar)
		}
	}

	// No secrets and no token: nothing is added.
	empty := RunConfig{UnixUsername: "scion"}
	if err := prepareContainerSecretEnv(&empty); err != nil || len(empty.Env) != 0 {
		t.Errorf("prepareContainerSecretEnv with no secrets changed env (%d entries, err %v)", len(empty.Env), err)
	}
}

// An environment-type secret can never supply the staged-token marker that
// sciontool init reads, on any runtime path.
func TestReservedSecretEnvTarget_HubTokenStagedMarker(t *testing.T) {
	secret := api.ResolvedSecret{Name: "m", Type: "environment", Target: stagedsecrets.HubTokenStagedEnvVar, Value: "1"}

	flatCfg := RunConfig{UnixUsername: "scion", ResolvedSecrets: []api.ResolvedSecret{secret}}
	if _, err := applyResolvedSecretsToEnv(&flatCfg, cloudRunEnvLimit); err != nil {
		t.Fatal(err)
	}
	for _, e := range flatCfg.Env {
		if strings.HasPrefix(e, stagedsecrets.HubTokenStagedEnvVar+"=") {
			t.Error("applyResolvedSecretsToEnv let a secret set the staged-token marker")
		}
	}

	ordinary := api.ResolvedSecret{Name: "k", Type: "environment", Target: "EXAMPLE_API_KEY", Value: "placeholder"}
	dockerCfg := RunConfig{
		Harness:         &harness.Generic{},
		Name:            "test-agent",
		UnixUsername:    "scion",
		Image:           "scion-agent:latest",
		ResolvedSecrets: []api.ResolvedSecret{secret, ordinary},
	}
	args, err := buildCommonRunArgs(dockerCfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	sawOrdinary := false
	for _, a := range args {
		if strings.HasPrefix(a, stagedsecrets.HubTokenStagedEnvVar+"=") {
			t.Error("buildCommonRunArgs let a secret set the staged-token marker")
		}
		if a == "EXAMPLE_API_KEY=placeholder" {
			sawOrdinary = true
		}
	}
	if !sawOrdinary {
		t.Error("control: an ordinary environment secret was not emitted")
	}
	if !reservedSecretEnvTarget(stagedsecrets.HubTokenStagedEnvVar) || reservedSecretEnvTarget("EXAMPLE_API_KEY") {
		t.Error("reservedSecretEnvTarget does not single out the marker")
	}
}

// The existing-instance path (no update API) keeps the instance's
// creation-time payload; it says so instead of dropping the credential
// silently, and the credential never reaches the log or the error.
func TestCloudRunRun_ExistingInstanceWarnsCredentialNotRedelivered(t *testing.T) {
	wantName := "projects/test-project/locations/us-central1/instances/" + cloudRunInstanceID("agent-1")
	fake := &fakeInstancesClient{getInstance: &runpb.Instance{Name: wantName}}
	rt := newFakeCloudRunRuntime(t, fake)
	logs := captureRuntimeLog(t)

	cfg := runConfigForTest()
	cfg.HomeDir = writeCRHubToken(t, crHubTokenCanary)
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		if strings.Contains(err.Error(), crHubTokenCanary) {
			t.Error("error carries the credential")
		}
		t.Fatalf("Run on an existing instance failed (error length %d)", len(err.Error()))
	}
	if len(fake.startReqs) != 1 || len(fake.createReqs) != 0 {
		t.Fatalf("start/create calls = %d/%d, want 1/0", len(fake.startReqs), len(fake.createReqs))
	}
	out := logs.String()
	if !strings.Contains(out, "keeps its creation-time staged secrets, including the hub credential") {
		t.Errorf("no warning on the reuse path (log length %d)", len(out))
	}
	if strings.Contains(out, crHubTokenCanary) {
		t.Error("log carries the credential")
	}
}

// When the blob is over the limit and carries the hub credential, the
// error says so (without the value).
func TestCloudRunRun_OversizedBlobMentionsHubCredential(t *testing.T) {
	cfg := RunConfig{UnixUsername: "scion", ResolvedSecrets: []api.ResolvedSecret{
		{Name: "big", Type: "variable", Target: "BIG", Value: strings.Repeat("v", cloudRunMaxEnvValueBytes)},
	}}
	_, err := applyStagedSecretsToEnv(&cfg, cloudRunEnvLimit, crHubTokenCanary)
	if err == nil {
		t.Fatal("no error for an oversized blob")
	}
	if !strings.Contains(err.Error(), "also carries the agent's hub credential") || strings.Contains(err.Error(), crHubTokenCanary) {
		t.Errorf("error does not mention the credential, or carries it (length %d)", len(err.Error()))
	}
}
