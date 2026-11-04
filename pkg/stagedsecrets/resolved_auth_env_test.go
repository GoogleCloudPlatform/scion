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

package stagedsecrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testNonce stands in for a staging pass's nonce.
const testNonce = "test-nonce-0123456789abcdef"

// realTempDir is t.TempDir with symlinks resolved, so the no-follow walk
// accepts it where the temp root is reached through a link (macOS /var).
func realTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestReservedResolvedAuthEnvName(t *testing.T) {
	for _, name := range []string{EnvVar, HubTokenStagedEnvVar, ResolvedAuthStagedEnvVar, "SCION_HOST_UID", "SCION_HOST_GID",
		"SCION_STARTUP_LISTENER_PORT", "SCION_HARNESS_SECRETS_DIR", "SCION_HARNESS_OUTPUTS_DIR",
		"PORT", "SCION_TRANSPORT_TOKEN", "SCION_TRANSPORT_TOKEN_EXPIRY", "SCION_EXTRA_PATH", "SCION_NATIVE_TELEMETRY_POLICY"} {
		if !ReservedResolvedAuthEnvName(name) {
			t.Errorf("%s is not reserved", name)
		}
	}
	if ReservedResolvedAuthEnvName("ANTHROPIC_API_KEY") {
		t.Error("control: ANTHROPIC_API_KEY is reserved")
	}
}

// Every name init removes from the harness environment is reserved, and
// the returned list is a copy.
func TestHarnessStrippedEnvNamesReserved(t *testing.T) {
	names := HarnessStrippedEnvNames()
	if len(names) == 0 {
		t.Fatal("control: HarnessStrippedEnvNames is empty")
	}
	for _, name := range names {
		if !ReservedResolvedAuthEnvName(name) {
			t.Errorf("%s is removed from the harness environment but not reserved", name)
		}
	}
	names[0] = "CHANGED_BY_CALLER"
	if HarnessStrippedEnvNames()[0] == "CHANGED_BY_CALLER" {
		t.Error("HarnessStrippedEnvNames returned the package slice, not a copy")
	}
}

func TestExportableResolvedAuthEnv(t *testing.T) {
	got, dropped := ExportableResolvedAuthEnv(map[string]string{
		HubTokenStagedEnvVar: "1",
		EnvVar:               "x",
		"SCION_HOST_UID":     "0",
		"BAD=NAME":           "x",
		"1LEADING_DIGIT":     "x",
		"":                   "x",
		"EXAMPLE_API_KEY":    "placeholder-api-key",
		"_UNDERSCORE_OK1":    "y",
	})
	if dropped != 6 {
		t.Errorf("dropped = %d, want 6", dropped)
	}
	want := map[string]string{"EXAMPLE_API_KEY": "placeholder-api-key", "_UNDERSCORE_OK1": "y"}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s not kept", k)
		}
	}
}

// memRoot returns an in-memory-volume stand-in and the harness secrets
// directory below it that the runtime would name.
func memRoot(t *testing.T) (root, secretsDir string) {
	t.Helper()
	root = realTempDir(t)
	return root, filepath.Join(root, "harness", "secrets")
}

func carryAbsent(t *testing.T, root, when string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, ResolvedAuthEnvFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("carry file present %s (lstat err=%v)", when, err)
	}
}

func TestWriteTakeResolvedAuthEnv(t *testing.T) {
	root, secretsDir := memRoot(t)
	env := map[string]string{"EXAMPLE_API_KEY": "placeholder-api-key"}
	if err := WriteResolvedAuthEnv(root, secretsDir, testNonce, env); err != nil {
		t.Fatalf("WriteResolvedAuthEnv: %v", err)
	}
	st, err := os.Lstat(filepath.Join(root, ResolvedAuthEnvFile))
	if err != nil {
		t.Fatalf("carry file missing: %v", err)
	}
	if st.Mode().Perm() != 0o600 || !st.Mode().IsRegular() {
		t.Errorf("carry file mode = %v, want a regular 0600 file", st.Mode())
	}
	if _, err := os.Lstat(secretsDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("the write created the harness secrets directory; the file belongs in the mem root")
	}
	got, err := TakeResolvedAuthEnv(root, testNonce)
	if err != nil {
		t.Fatalf("TakeResolvedAuthEnv: %v", err)
	}
	if got["EXAMPLE_API_KEY"] != "placeholder-api-key" || len(got) != 1 {
		t.Errorf("TakeResolvedAuthEnv returned %d entries, want the one written", len(got))
	}
	carryAbsent(t, root, "after a successful take")
	again, err := TakeResolvedAuthEnv(root, testNonce)
	if err != nil || again != nil {
		t.Errorf("second Take = %d entries, err %v; want none", len(again), err)
	}
}

func TestWriteResolvedAuthEnv_ReplacesStaleFile(t *testing.T) {
	root, secretsDir := memRoot(t)
	if err := os.WriteFile(filepath.Join(root, ResolvedAuthEnvFile), []byte(`{"STALE":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteResolvedAuthEnv(root, secretsDir, testNonce, map[string]string{"EXAMPLE_API_KEY": "placeholder-api-key"}); err != nil {
		t.Fatalf("WriteResolvedAuthEnv over a stale file: %v", err)
	}
	got, err := TakeResolvedAuthEnv(root, testNonce)
	if err != nil {
		t.Fatalf("TakeResolvedAuthEnv: %v", err)
	}
	if _, stale := got["STALE"]; stale || got["EXAMPLE_API_KEY"] != "placeholder-api-key" {
		t.Errorf("take after replacing a stale file returned the stale content")
	}
}

func TestWriteResolvedAuthEnv_EmptyWritesNothing(t *testing.T) {
	root, _ := memRoot(t)
	// No entries: nothing is written, and no in-memory directory is needed.
	if err := WriteResolvedAuthEnv(root, "", testNonce, nil); err != nil {
		t.Fatalf("WriteResolvedAuthEnv(nil): %v", err)
	}
	carryAbsent(t, root, "for no entries")
}

// Without an in-memory harness directory named by the runtime, the write
// fails and nothing is written anywhere.
func TestWriteResolvedAuthEnv_NoFallbackWithoutMemDirs(t *testing.T) {
	root, _ := memRoot(t)
	env := map[string]string{"EXAMPLE_API_KEY": "placeholder-api-key"}
	if err := WriteResolvedAuthEnv(root, "", testNonce, env); !errors.Is(err, ErrNoResolvedAuthEnvRoot) {
		t.Errorf("no secrets dir: err = %v, want ErrNoResolvedAuthEnvRoot", err)
	}
	carryAbsent(t, root, "without a named in-memory directory")
	if err := WriteResolvedAuthEnv(root, filepath.Join(realTempDir(t), "secrets"), testNonce, env); err == nil {
		t.Error("a secrets dir outside the mem root was accepted")
	}
	carryAbsent(t, root, "with a secrets dir outside the mem root")
	missing := filepath.Join(root, "not-mounted")
	if err := WriteResolvedAuthEnv(missing, filepath.Join(missing, "secrets"), testNonce, env); err == nil {
		t.Error("a missing mem root was accepted")
	}
	if got, err := TakeResolvedAuthEnv(missing, testNonce); err != nil || got != nil {
		t.Errorf("Take on a missing root = %d entries, err %v; want none", len(got), err)
	}
}

func TestResolvedAuthEnv_RefusesLinks(t *testing.T) {
	root, secretsDir := memRoot(t)
	target := filepath.Join(root, "elsewhere")
	if err := os.WriteFile(target, []byte(`{"EXAMPLE":"planted"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ResolvedAuthEnvFile)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeResolvedAuthEnv(root, testNonce); err == nil {
		t.Error("read through a link at the carry file succeeded")
	}
	carryAbsent(t, root, "after a refused read of a link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// The write unlinks the planted link (never following it) and creates
	// its own file.
	if err := WriteResolvedAuthEnv(root, secretsDir, testNonce, map[string]string{"EXAMPLE_API_KEY": "placeholder-api-key"}); err != nil {
		t.Fatalf("write over a planted link: %v", err)
	}
	if st, err := os.Lstat(link); err != nil || !st.Mode().IsRegular() {
		t.Errorf("carry file is not a regular file after the write (err=%v)", err)
	}
	if data, _ := os.ReadFile(target); string(data) != `{"EXAMPLE":"planted"}` {
		t.Error("the link target was written")
	}

	linkedRoot := filepath.Join(realTempDir(t), "linked-root")
	if err := os.Symlink(root, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if err := WriteResolvedAuthEnv(linkedRoot, filepath.Join(linkedRoot, "secrets"), testNonce, map[string]string{"EXAMPLE_API_KEY": "placeholder-api-key"}); err == nil {
		t.Error("write through a linked in-memory volume path succeeded")
	}
}

func TestTakeResolvedAuthEnv_RefusesHardLink(t *testing.T) {
	root, secretsDir := memRoot(t)
	if err := WriteResolvedAuthEnv(root, secretsDir, testNonce, map[string]string{"EXAMPLE_API_KEY": "placeholder-api-key"}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(realTempDir(t), "second-link")
	if err := os.Link(filepath.Join(root, ResolvedAuthEnvFile), other); err != nil {
		t.Skipf("hard link not possible here: %v", err)
	}
	if _, err := TakeResolvedAuthEnv(root, testNonce); err == nil {
		t.Error("a carry file with two links was accepted")
	}
	carryAbsent(t, root, "after a refused read of a file with two links")
}

func TestTakeResolvedAuthEnv_RefusesWrongOwner(t *testing.T) {
	root, secretsDir := memRoot(t)
	if err := WriteResolvedAuthEnv(root, secretsDir, testNonce, map[string]string{"EXAMPLE_API_KEY": "placeholder-api-key"}); err != nil {
		t.Fatal(err)
	}
	prev := resolvedAuthEnvOwner
	resolvedAuthEnvOwner = func() (int, int) { return os.Getuid() + 1, os.Getgid() }
	defer func() { resolvedAuthEnvOwner = prev }()
	if _, err := TakeResolvedAuthEnv(root, testNonce); err == nil {
		t.Error("a carry file owned by another uid was accepted")
	}
	carryAbsent(t, root, "after a refused read of a file with the wrong owner")
}

func TestTakeResolvedAuthEnv_RefusesOversizeAndBadJSON(t *testing.T) {
	root, _ := memRoot(t)
	big := make([]byte, maxResolvedAuthEnvFileBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(root, ResolvedAuthEnvFile), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeResolvedAuthEnv(root, testNonce); err == nil {
		t.Error("an oversize carry file was accepted")
	}
	carryAbsent(t, root, "after a refused oversize read")
	if err := os.WriteFile(filepath.Join(root, ResolvedAuthEnvFile), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeResolvedAuthEnv(root, testNonce); err == nil {
		t.Error("an undecodable carry file was accepted")
	}
	carryAbsent(t, root, "after a failed decode")
}

// Formatting a Staged never prints a resolved-auth value.
func TestStaged_FormattingWithholdsResolvedAuthEnv(t *testing.T) {
	const canary = "placeholder-canary-resolved-auth-7285"
	s := Staged{ResolvedAuthEnv: map[string]string{"EXAMPLE_API_KEY": canary}}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(verb, s); strings.Contains(out, canary) {
			t.Errorf("%s of Staged prints a resolved-auth value", verb)
		}
	}
}

// DiscardResolvedAuthEnv removes the carry file, or a link planted at its
// name, without following it; nothing there is not an error.
func TestDiscardResolvedAuthEnv(t *testing.T) {
	root, secretsDir := memRoot(t)
	if removed, err := DiscardResolvedAuthEnv(root); err != nil || removed {
		t.Errorf("no file: removed %v, err %v; want false, nil", removed, err)
	}
	if removed, err := DiscardResolvedAuthEnv(filepath.Join(root, "not-mounted")); err != nil || removed {
		t.Errorf("no root: removed %v, err %v; want false, nil", removed, err)
	}
	if err := WriteResolvedAuthEnv(root, secretsDir, testNonce, map[string]string{"EXAMPLE_API_KEY": "placeholder-api-key"}); err != nil {
		t.Fatal(err)
	}
	if removed, err := DiscardResolvedAuthEnv(root); err != nil || !removed {
		t.Errorf("file: removed %v, err %v; want true, nil", removed, err)
	}
	carryAbsent(t, root, "after a discard")
	target := filepath.Join(root, "elsewhere")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, ResolvedAuthEnvFile)); err != nil {
		t.Fatal(err)
	}
	if removed, err := DiscardResolvedAuthEnv(root); err != nil || !removed {
		t.Errorf("link: removed %v, err %v; want true, nil", removed, err)
	}
	carryAbsent(t, root, "after discarding a link")
	if data, err := os.ReadFile(target); err != nil || string(data) != "keep" {
		t.Error("discarding a link touched its target")
	}
}

// A carry file whose nonce is not the caller's (a stale or planted file)
// is refused with ErrResolvedAuthEnvNonceMismatch and removed; an empty
// nonce is refused outright.
func TestTakeResolvedAuthEnv_NonceMismatch(t *testing.T) {
	root, secretsDir := memRoot(t)
	if err := WriteResolvedAuthEnv(root, secretsDir, "stale-nonce", map[string]string{"EXAMPLE_API_KEY": "placeholder-api-key"}); err != nil {
		t.Fatal(err)
	}
	got, err := TakeResolvedAuthEnv(root, testNonce)
	if !errors.Is(err, ErrResolvedAuthEnvNonceMismatch) || got != nil {
		t.Errorf("stale nonce: %d entries, err %v; want none, ErrResolvedAuthEnvNonceMismatch", len(got), err)
	}
	carryAbsent(t, root, "after a nonce mismatch")
	if err := WriteResolvedAuthEnv(root, secretsDir, testNonce, map[string]string{"EXAMPLE_API_KEY": "placeholder-api-key"}); err != nil {
		t.Fatal(err)
	}
	if got, err := TakeResolvedAuthEnv(root, ""); err == nil || got != nil {
		t.Errorf("empty nonce: %d entries, err %v; want a refusal", len(got), err)
	}
	if err := WriteResolvedAuthEnv(root, secretsDir, "", map[string]string{"EXAMPLE_API_KEY": "x"}); err == nil {
		t.Error("a carry file was written without a nonce")
	}
	// A carry file with no nonce field, or an empty one, is a mismatch for
	// any non-empty marker, and it is removed.
	_, _ = TakeResolvedAuthEnv(root, testNonce) // consume the file written above
	for _, body := range []string{`{"env":{"EXAMPLE_API_KEY":"x"}}`, `{"nonce":"","env":{"EXAMPLE_API_KEY":"x"}}`} {
		if err := os.WriteFile(filepath.Join(root, ResolvedAuthEnvFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := TakeResolvedAuthEnv(root, testNonce)
		if !errors.Is(err, ErrResolvedAuthEnvNonceMismatch) || got != nil {
			t.Errorf("carry file without a nonce: %d entries, err %v; want none, ErrResolvedAuthEnvNonceMismatch", len(got), err)
		}
		carryAbsent(t, root, "after a carry file without a nonce")
	}
	a, err1 := NewResolvedAuthNonce()
	b, err2 := NewResolvedAuthNonce()
	if err1 != nil || err2 != nil || len(a) != 64 || a == b {
		t.Errorf("NewResolvedAuthNonce: lengths %d/%d, equal %v, errs %v/%v", len(a), len(b), a == b, err1, err2)
	}
}
