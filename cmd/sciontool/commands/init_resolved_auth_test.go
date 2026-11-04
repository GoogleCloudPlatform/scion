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

// Placeholder values only; not a real credential or nonce.
const (
	resolvedAuthCanary = "placeholder-canary-api-key-7285"
	initTestNonce      = "init-test-nonce-0123456789abcdef"
)

// The init end of the resolved-auth path drops a reserved name again, even
// when the carry file holds one, and removes the file.
func TestTakeResolvedAuthEnv_DropsReservedNamesAtInit(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Written directly, bypassing the broker-side filter, as a planted or
	// faulty blob would arrive.
	if err := stagedsecrets.WriteResolvedAuthEnv(root, filepath.Join(root, "harness", "secrets"), initTestNonce, map[string]string{
		stagedsecrets.HubTokenStagedEnvVar: "1",
		stagedsecrets.EnvVar:               "planted",
		"SCION_HOST_UID":                   "0",
		"SCION_HARNESS_SECRETS_DIR":        "/planted",
		"EXAMPLE_API_KEY":                  resolvedAuthCanary,
	}); err != nil {
		t.Fatalf("WriteResolvedAuthEnv: %v", err)
	}
	got, err := takeResolvedAuthEnv(root, initTestNonce)
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

// A carry file this init did not stage (no marker) is never exported: it
// is removed without being read.
func TestTakeResolvedAuthEnv_UnstagedFileDiscarded(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := stagedsecrets.WriteResolvedAuthEnv(root, filepath.Join(root, "harness", "secrets"), "planted-nonce", map[string]string{
		"ANTHROPIC_BASE_URL": "https://planted.invalid",
	}); err != nil {
		t.Fatalf("WriteResolvedAuthEnv: %v", err)
	}
	got, err := takeResolvedAuthEnv(root, "")
	if err != nil {
		t.Fatalf("takeResolvedAuthEnv: %v", err)
	}
	if got != nil {
		t.Errorf("an unstaged carry file was exported (%d entries)", len(got))
	}
	if _, err := os.Lstat(filepath.Join(root, stagedsecrets.ResolvedAuthEnvFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unstaged carry file still present (lstat err=%v)", err)
	}
}

// Nothing staged: no entries, no error.
func TestTakeResolvedAuthEnv_NothingStaged(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, nonce := range []string{initTestNonce, ""} {
		got, err := takeResolvedAuthEnv(root, nonce)
		if err != nil || got != nil {
			t.Errorf("nonce set %v: takeResolvedAuthEnv with no carry file = %d entries, err %v; want none", nonce != "", len(got), err)
		}
		got, err = takeResolvedAuthEnv(filepath.Join(root, "not-mounted"), nonce)
		if err != nil || got != nil {
			t.Errorf("nonce set %v: takeResolvedAuthEnv with no in-memory volume = %d entries, err %v; want none", nonce != "", len(got), err)
		}
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

// End to end through RunInit: a resolved-auth marker inherited from the
// environment is dropped by the staging pass, so a carry file planted
// before the start (for example by an earlier run in the same pod) is
// removed unread and never reaches the harness child.
func TestRunInit_InheritedResolvedAuthMarkerDoesNotExportPlantedFile(t *testing.T) {
	f := newRunInitStagedFixture(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prevRoot := resolvedAuthEnvRoot
	resolvedAuthEnvRoot = root
	t.Cleanup(func() { resolvedAuthEnvRoot = prevRoot })
	t.Cleanup(hooks.SetHarnessDirsRootForTest(root))
	secretsDir := filepath.Join(root, "harness", "secrets")
	t.Setenv(hooks.HarnessSecretsDirEnv, secretsDir)
	t.Setenv("EXAMPLE_PLANTED", "")
	_ = os.Unsetenv("EXAMPLE_PLANTED")
	if err := stagedsecrets.WriteResolvedAuthEnv(root, secretsDir, "planted-nonce", map[string]string{"EXAMPLE_PLANTED": resolvedAuthCanary}); err != nil {
		t.Fatalf("plant the carry file: %v", err)
	}
	t.Setenv(envResolvedAuthStaged, "1")

	// Staged data without resolved-auth entries: the staging pass runs and
	// stages nothing of its own.
	data, err := json.Marshal(stagedsecrets.Staged{VariableSecrets: map[string]string{"EXAMPLE_VARIABLE": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(stagedsecrets.EnvVar, base64.StdEncoding.EncodeToString(data))
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")

	out := filepath.Join(t.TempDir(), "child-env")
	script := `printf '%s|%s' "${EXAMPLE_PLANTED:-absent}" "${SCION_RESOLVED_AUTH_STAGED:-absent}" > "$2"; sleep 0.5`
	if got := f.run(t, script, out); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if got := readFileString(t, out); got != "absent|absent" {
		t.Errorf("harness child: planted entry absent %v, marker absent %v; want true, true",
			strings.HasPrefix(got, "absent|"), strings.HasSuffix(got, "|absent"))
	}
	if os.Getenv(envResolvedAuthStaged) != "" {
		t.Error("the inherited resolved-auth marker was not cleared")
	}
	if _, err := os.Lstat(filepath.Join(root, stagedsecrets.ResolvedAuthEnvFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("planted carry file still present after RunInit (lstat err=%v)", err)
	}
}

// A carry file whose nonce is not this start's is not exported, is removed,
// and does not fail the start.
func TestTakeResolvedAuthEnv_NonceMismatchNotExported(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := stagedsecrets.WriteResolvedAuthEnv(root, filepath.Join(root, "harness", "secrets"), "stale-nonce", map[string]string{"EXAMPLE_API_KEY": resolvedAuthCanary}); err != nil {
		t.Fatal(err)
	}
	got, err := takeResolvedAuthEnv(root, initTestNonce)
	if err != nil || got != nil {
		t.Errorf("stale nonce: %d entries, err %v; want none, nil", len(got), err)
	}
	if _, err := os.Lstat(filepath.Join(root, stagedsecrets.ResolvedAuthEnvFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale carry file still present (lstat err=%v)", err)
	}
}

// Without a marker, an entry at the carry file name that cannot be
// removed (here a directory) is logged and the start goes on.
func TestTakeResolvedAuthEnv_NoMarkerOddEntryNotFatal(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, stagedsecrets.ResolvedAuthEnvFile), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := takeResolvedAuthEnv(root, "")
	if err != nil || got != nil {
		t.Errorf("odd entry, no marker: %d entries, err %v; want none, nil", len(got), err)
	}
}

// End to end through RunInit: with no marker and the in-memory volume path
// being a link (a docker or Kubernetes host where it is not a plain
// directory), the start still succeeds and the harness gets nothing from it.
func TestRunInit_NoMarkerLinkedMemRootStartSucceeds(t *testing.T) {
	f := newRunInitStagedFixture(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real-mem")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := stagedsecrets.WriteResolvedAuthEnv(real, filepath.Join(real, "harness", "secrets"), "planted-nonce", map[string]string{"EXAMPLE_PLANTED": resolvedAuthCanary}); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(base, "linked-mem")
	if err := os.Symlink(real, linked); err != nil {
		t.Fatal(err)
	}
	prevRoot := resolvedAuthEnvRoot
	resolvedAuthEnvRoot = linked
	t.Cleanup(func() { resolvedAuthEnvRoot = prevRoot })
	t.Setenv("EXAMPLE_PLANTED", "")
	_ = os.Unsetenv("EXAMPLE_PLANTED")
	t.Setenv(envResolvedAuthStaged, "")
	_ = os.Unsetenv(envResolvedAuthStaged)
	t.Setenv(stagedsecrets.EnvVar, "")
	_ = os.Unsetenv(stagedsecrets.EnvVar)
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")

	out := filepath.Join(t.TempDir(), "child-env")
	script := `printf '%s' "${EXAMPLE_PLANTED:-absent}" > "$2"; sleep 0.5`
	if got := f.run(t, script, out); got != 0 {
		t.Fatalf("RunInit() = %d, want 0 (the no-marker path must not fail the start)", got)
	}
	if got := readFileString(t, out); got != "absent" {
		t.Error("the harness child received an entry from a carry file this init did not stage")
	}
}
