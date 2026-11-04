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

package commands

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// Placeholder value only; not a real credential.
const resolvedAuthCanary = "placeholder-canary-api-key-7285"

// The init end of the resolved-auth path drops a reserved name again, even
// when the carry file holds one, and removes the file.
func TestTakeResolvedAuthEnv_DropsReservedNamesAtInit(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Written directly, bypassing the broker-side filter, as a planted or
	// faulty blob would arrive.
	if err := stagedsecrets.WriteResolvedAuthEnv(root, filepath.Join(root, "harness", "secrets"), map[string]string{
		stagedsecrets.HubTokenStagedEnvVar: "1",
		stagedsecrets.EnvVar:               "planted",
		"SCION_HOST_UID":                   "0",
		"SCION_HARNESS_SECRETS_DIR":        "/planted",
		"EXAMPLE_API_KEY":                  resolvedAuthCanary,
	}); err != nil {
		t.Fatalf("WriteResolvedAuthEnv: %v", err)
	}
	got, err := takeResolvedAuthEnv(root)
	if err != nil {
		t.Fatalf("takeResolvedAuthEnv: %v", err)
	}
	for _, name := range []string{stagedsecrets.HubTokenStagedEnvVar, stagedsecrets.EnvVar, "SCION_HOST_UID", "SCION_HARNESS_SECRETS_DIR"} {
		if _, ok := got[name]; ok {
			t.Errorf("reserved name %s reached the harness env entries", name)
		}
	}
	if got["EXAMPLE_API_KEY"] != resolvedAuthCanary {
		t.Error("control: the canary entry did not reach the harness env entries")
	}
	if _, err := os.Lstat(filepath.Join(root, stagedsecrets.ResolvedAuthEnvFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("carry file still present after init read it (lstat err=%v)", err)
	}
}

// Nothing staged: no entries, no error.
func TestTakeResolvedAuthEnv_NothingStaged(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := takeResolvedAuthEnv(root)
	if err != nil || got != nil {
		t.Errorf("takeResolvedAuthEnv with no carry file = %d entries, err %v; want none", len(got), err)
	}
	got, err = takeResolvedAuthEnv(filepath.Join(root, "not-mounted"))
	if err != nil || got != nil {
		t.Errorf("takeResolvedAuthEnv with no in-memory volume = %d entries, err %v; want none", len(got), err)
	}
}

// End to end through RunInit (embedded, no re-exec): a staged
// resolved-auth entry reaches the harness child's environment and not
// init's own; a reserved name in the same staged data reaches neither;
// the carry file is gone after the run; no value is logged.
func TestRunInit_StagedResolvedAuthEnvReachesHarnessOnly(t *testing.T) {
	f := newRunInitStagedFixture(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prevRoot := resolvedAuthEnvRoot
	resolvedAuthEnvRoot = root
	t.Cleanup(func() { resolvedAuthEnvRoot = prevRoot })
	t.Cleanup(hooks.SetHarnessDirsRootForTest(root))
	t.Setenv(hooks.HarnessSecretsDirEnv, filepath.Join(root, "harness", "secrets"))
	t.Setenv("EXAMPLE_RESOLVED_AUTH", "")
	_ = os.Unsetenv("EXAMPLE_RESOLVED_AUTH")

	data, err := json.Marshal(stagedsecrets.Staged{ResolvedAuthEnv: map[string]string{
		"EXAMPLE_RESOLVED_AUTH":            resolvedAuthCanary,
		stagedsecrets.HubTokenStagedEnvVar: "1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(stagedsecrets.EnvVar, base64.StdEncoding.EncodeToString(data))
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")

	out := filepath.Join(t.TempDir(), "child-env")
	script := `printf '%s|%s' "${EXAMPLE_RESOLVED_AUTH:-absent}" "${SCION_HUB_TOKEN_STAGED:-absent}" > "$2"; sleep 0.5`
	if got := f.run(t, script, out); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	got := readFileString(t, out)
	if got != resolvedAuthCanary+"|absent" {
		t.Errorf("harness child: resolved-auth entry present %v, reserved marker absent %v; want true, true",
			strings.HasPrefix(got, resolvedAuthCanary+"|"), strings.HasSuffix(got, "|absent"))
	}
	if os.Getenv("EXAMPLE_RESOLVED_AUTH") != "" {
		t.Error("the resolved-auth entry is in init's own environment")
	}
	if _, err := os.Lstat(filepath.Join(root, stagedsecrets.ResolvedAuthEnvFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("carry file still present after RunInit (lstat err=%v)", err)
	}
	if strings.Contains(f.readLog(), resolvedAuthCanary) {
		t.Error("the sciontool log carries the resolved-auth value")
	}
}
