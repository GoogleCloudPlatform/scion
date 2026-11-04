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
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// Placeholder values only; not a real credential or nonce.
const (
	resolvedAuthCanary    = "placeholder-canary-api-key-7285"
	initTestNonce         = "init-test-nonce-0123456789abcdef"
	resolvedAuthLeftNonce = "left-by-another-start-nonce-0123456789abcdef"
)

// resolvedAuthRunRoot points init's in-memory volume and harness
// directory checks at a fresh directory and names the harness secrets
// directory below it, for a RunInit test.
func resolvedAuthRunRoot(t *testing.T) (root, secretsDir string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prevRoot := resolvedAuthEnvRoot
	resolvedAuthEnvRoot = root
	t.Cleanup(func() { resolvedAuthEnvRoot = prevRoot })
	t.Cleanup(hooks.SetHarnessDirsRootForTest(root))
	secretsDir = filepath.Join(root, "harness", "secrets")
	t.Setenv(hooks.HarnessSecretsDirEnv, secretsDir)
	t.Setenv("EXAMPLE_RESOLVED_AUTH", "")
	_ = os.Unsetenv("EXAMPLE_RESOLVED_AUTH")
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")
	return root, secretsDir
}

// nonceHexRE is the form of a staging pass's nonce (NewResolvedAuthNonce).
var nonceHexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// carryGone reports whether the carry file is absent from root.
func carryGone(root string) bool {
	_, err := os.Lstat(filepath.Join(root, stagedsecrets.ResolvedAuthEnvFile))
	return errors.Is(err, os.ErrNotExist)
}

// The init end of the resolved-auth path drops a reserved name again, even
// when the carry file holds one, and removes the file.
func TestTakeResolvedAuthEnv_DropsReservedNamesAtInit(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Written directly, bypassing the broker-side filter, as a left-over or
	// faulty blob would arrive.
	if err := stagedsecrets.WriteResolvedAuthEnv(root, filepath.Join(root, "harness", "secrets"), initTestNonce, map[string]string{
		stagedsecrets.HubTokenStagedEnvVar: "1",
		stagedsecrets.EnvVar:               "left-over",
		"SCION_HOST_UID":                   "0",
		"SCION_HARNESS_SECRETS_DIR":        "/left-over",
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
	if err := stagedsecrets.WriteResolvedAuthEnv(root, filepath.Join(root, "harness", "secrets"), "other-start-nonce", map[string]string{
		"ANTHROPIC_BASE_URL": "https://left.invalid",
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
	root, _ := resolvedAuthRunRoot(t)

	data, err := json.Marshal(stagedsecrets.Staged{ResolvedAuthEnv: map[string]string{
		"EXAMPLE_RESOLVED_AUTH":            resolvedAuthCanary,
		stagedsecrets.HubTokenStagedEnvVar: "1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(stagedsecrets.EnvVar, base64.StdEncoding.EncodeToString(data))

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
// environment is dropped by the staging pass, so a carry file left
// before the start (for example by an earlier run in the same pod) is
// removed unread and never reaches the harness child.
func TestRunInit_InheritedResolvedAuthMarkerDoesNotExportLeftFile(t *testing.T) {
	f := newRunInitStagedFixture(t)
	root, secretsDir := resolvedAuthRunRoot(t)
	t.Setenv("EXAMPLE_LEFT", "")
	_ = os.Unsetenv("EXAMPLE_LEFT")
	// The inherited marker carries the SAME value as the file's nonce, so
	// only the staging pass dropping the marker keeps the file unread.
	if err := stagedsecrets.WriteResolvedAuthEnv(root, secretsDir, resolvedAuthLeftNonce, map[string]string{"EXAMPLE_LEFT": resolvedAuthCanary}); err != nil {
		t.Fatalf("write the left carry file: %v", err)
	}
	t.Setenv(envResolvedAuthStaged, resolvedAuthLeftNonce)

	// Staged data without resolved-auth entries: the staging pass runs and
	// stages nothing of its own.
	data, err := json.Marshal(stagedsecrets.Staged{VariableSecrets: map[string]string{"EXAMPLE_VARIABLE": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(stagedsecrets.EnvVar, base64.StdEncoding.EncodeToString(data))

	out := filepath.Join(t.TempDir(), "child-env")
	script := `printf '%s|%s' "${EXAMPLE_LEFT:-absent}" "${SCION_RESOLVED_AUTH_STAGED:-absent}" > "$2"; sleep 0.5`
	if got := f.run(t, script, out); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if got := readFileString(t, out); got != "absent|absent" {
		t.Errorf("harness child: left entry absent %v, marker absent %v; want true, true",
			strings.HasPrefix(got, "absent|"), strings.HasSuffix(got, "|absent"))
	}
	if os.Getenv(envResolvedAuthStaged) != "" {
		t.Error("the inherited resolved-auth marker was not cleared")
	}
	if _, err := os.Lstat(filepath.Join(root, stagedsecrets.ResolvedAuthEnvFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("left carry file still present after RunInit (lstat err=%v)", err)
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
	if err := stagedsecrets.WriteResolvedAuthEnv(real, filepath.Join(real, "harness", "secrets"), "other-start-nonce", map[string]string{"EXAMPLE_LEFT": resolvedAuthCanary}); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(base, "linked-mem")
	if err := os.Symlink(real, linked); err != nil {
		t.Fatal(err)
	}
	prevRoot := resolvedAuthEnvRoot
	resolvedAuthEnvRoot = linked
	t.Cleanup(func() { resolvedAuthEnvRoot = prevRoot })
	t.Setenv("EXAMPLE_LEFT", "")
	_ = os.Unsetenv("EXAMPLE_LEFT")
	t.Setenv(envResolvedAuthStaged, "")
	_ = os.Unsetenv(envResolvedAuthStaged)
	t.Setenv(stagedsecrets.EnvVar, "")
	_ = os.Unsetenv(stagedsecrets.EnvVar)
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")

	out := filepath.Join(t.TempDir(), "child-env")
	script := `printf '%s' "${EXAMPLE_LEFT:-absent}" > "$2"; sleep 0.5`
	if got := f.run(t, script, out); got != 0 {
		t.Fatalf("RunInit() = %d, want 0 (the no-marker path must not fail the start)", got)
	}
	if got := readFileString(t, out); got != "absent" {
		t.Error("the harness child received an entry from a carry file this init did not stage")
	}
}

// The staging pass hands its nonce to the next pass through the marker:
// at the re-exec the marker equals the nonce in the carry file it wrote.
// The re-exec is observed through its seam (which returns without
// exec'ing, so RunInit goes on in this process and launches the harness).
// Neither the nonce nor the entry's value is logged.
func TestRunInit_ResolvedAuthMarkerHandoffAcrossReExec(t *testing.T) {
	f := newRunInitStagedFixture(t)
	root, _ := resolvedAuthRunRoot(t)
	t.Setenv(envResolvedAuthStaged, "")
	_ = os.Unsetenv(envResolvedAuthStaged)
	data, err := json.Marshal(stagedsecrets.Staged{ResolvedAuthEnv: map[string]string{"EXAMPLE_RESOLVED_AUTH": resolvedAuthCanary}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(stagedsecrets.EnvVar, base64.StdEncoding.EncodeToString(data))

	calls := 0
	var markerAtReExec, fileNonce string
	orig := reExecWithCleanEnv
	reExecWithCleanEnv = func() error {
		calls++
		markerAtReExec = os.Getenv(envResolvedAuthStaged)
		if b, err := os.ReadFile(filepath.Join(root, stagedsecrets.ResolvedAuthEnvFile)); err == nil {
			var c struct {
				Nonce string `json:"nonce"`
			}
			_ = json.Unmarshal(b, &c)
			fileNonce = c.Nonce
		}
		return nil
	}
	t.Cleanup(func() { reExecWithCleanEnv = orig })

	out := filepath.Join(t.TempDir(), "child-env")
	script := `printf '%s' "${EXAMPLE_RESOLVED_AUTH:-absent}" > "$2"; sleep 0.5`
	if got := RunInit([]string{"sh", "-c", script, "sh", f.tokenPath, out}, InitRunOptions{DisableTermSignalForwarding: true}); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if calls != 1 {
		t.Fatalf("re-exec reached %d times, want 1", calls)
	}
	if !nonceHexRE.MatchString(markerAtReExec) || markerAtReExec != fileNonce {
		t.Errorf("at the re-exec: marker is 64 lowercase hex %v, equals the carry file's nonce %v; want true, true", nonceHexRE.MatchString(markerAtReExec), markerAtReExec == fileNonce)
	}
	if readFileString(t, out) != resolvedAuthCanary {
		t.Error("the staged resolved-auth entry did not reach the harness child")
	}
	if os.Getenv(envResolvedAuthStaged) != "" {
		t.Error("the marker is still set after the harness launch")
	}
	if !carryGone(root) {
		t.Error("carry file still present after RunInit")
	}
	logText := f.readLog()
	if markerAtReExec != "" && strings.Contains(logText, markerAtReExec) {
		t.Error("the sciontool log carries the nonce")
	}
	if strings.Contains(logText, resolvedAuthCanary) {
		t.Error("the sciontool log carries the resolved-auth value")
	}
}

// The pass after the re-exec (no staged data, the marker set to the carry
// file's nonce) reads that file: its entry reaches the harness child.
func TestRunInit_ResolvedAuthSecondPassTakesMarkedFile(t *testing.T) {
	f := newRunInitStagedFixture(t)
	root, secretsDir := resolvedAuthRunRoot(t)
	t.Setenv(stagedsecrets.EnvVar, "")
	_ = os.Unsetenv(stagedsecrets.EnvVar)
	if err := stagedsecrets.WriteResolvedAuthEnv(root, secretsDir, initTestNonce, map[string]string{"EXAMPLE_RESOLVED_AUTH": resolvedAuthCanary}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envResolvedAuthStaged, initTestNonce)

	out := filepath.Join(t.TempDir(), "child-env")
	script := `printf '%s|%s' "${EXAMPLE_RESOLVED_AUTH:-absent}" "${SCION_RESOLVED_AUTH_STAGED:-absent}" > "$2"; sleep 0.5`
	if got := f.run(t, script, out); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if readFileString(t, out) != resolvedAuthCanary+"|absent" {
		t.Error("second pass: the marked carry file's entry did not reach the harness child, or the marker did")
	}
	if !carryGone(root) {
		t.Error("carry file still present after the second pass")
	}
}

// The transport-token re-exec path (no staged data) drops an inherited
// marker too, even one equal to a carry file's nonce: the file is removed
// unread and its entry never reaches the harness child.
func TestRunInit_TransportReexecDropsInheritedResolvedAuthMarker(t *testing.T) {
	f := newRunInitStagedFixture(t)
	root, secretsDir := resolvedAuthRunRoot(t)
	t.Setenv(stagedsecrets.EnvVar, "")
	_ = os.Unsetenv(stagedsecrets.EnvVar)
	if err := stagedsecrets.WriteResolvedAuthEnv(root, secretsDir, resolvedAuthLeftNonce, map[string]string{"EXAMPLE_RESOLVED_AUTH": resolvedAuthCanary}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envResolvedAuthStaged, resolvedAuthLeftNonce)
	t.Setenv(transportauth.EnvTransportToken, "fake-transport-token-canary-7285")
	t.Setenv(transportauth.EnvTransportTokenFile, "")

	out := filepath.Join(t.TempDir(), "child-env")
	script := `printf '%s' "${EXAMPLE_RESOLVED_AUTH:-absent}" > "$2"; sleep 0.5`
	if got := f.run(t, script, out); got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if readFileString(t, out) != "absent" {
		t.Error("the transport re-exec path kept an inherited marker and exported a carry file left by another start")
	}
	if !carryGone(root) {
		t.Error("carry file still present after the transport re-exec path")
	}
}
