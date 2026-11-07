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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// initHubTokenCanary is an obviously fake credential.
const initHubTokenCanary = "fake-hub-token-canary-7239-not-a-credential"

// iatToken builds a three-part token with the given iat, encoded at run
// time so no token-shaped literal is in the source.
func iatToken(iat int64) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256"}`)) + "." + enc([]byte(fmt.Sprintf(`{"iat":%d}`, iat))) + "." + enc([]byte("fake-signature"))
}

func isolatedTokenHome(t *testing.T) (home, tokenPath string) {
	t.Helper()
	home = t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	return home, filepath.Join(home, ".scion", hub.TokenFile)
}

func readFileString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// The staged credential lands at <home>/.scion/scion-token, mode 0600,
// owned by the agent user (here the test's own uid). On Cloud Run <home>
// is the agent's NFS home leaf in both homes modes.
func TestStageHubToken_WritesCanonicalPath0600AgentOwned(t *testing.T) {
	_, tokenPath := isolatedTokenHome(t)
	kept, err := stageHubToken(initHubTokenCanary, os.Getuid(), os.Getgid())
	if err != nil || kept {
		t.Fatalf("stageHubToken: kept=%v err=%v", kept, err)
	}
	if got := readFileString(t, tokenPath); got != initHubTokenCanary {
		t.Errorf("token file does not hold the staged credential (length %d)", len(got))
	}
	fi, err := os.Lstat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Errorf("token file mode = %v, want a regular file at 0600", fi.Mode())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		t.Errorf("token file owner uid = %d, want %d", st.Uid, os.Getuid())
	}
	if got := hub.TokenFilePath(); got != tokenPath {
		t.Errorf("TokenFilePath() = %q, want %q", got, tokenPath)
	}
}

// A symlink planted at the token path is never written through: the link
// target keeps its content.
func TestStageHubToken_RefusesPlantedSymlink(t *testing.T) {
	_, tokenPath := isolatedTokenHome(t)
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, tokenPath); err != nil {
		t.Fatal(err)
	}
	_, err := stageHubToken(initHubTokenCanary, os.Getuid(), os.Getgid())
	if got := readFileString(t, target); got != "original" {
		t.Fatalf("symlink target was written through (now %d bytes)", len(got))
	}
	if err == nil {
		fi, lerr := os.Lstat(tokenPath)
		if lerr != nil || !fi.Mode().IsRegular() {
			t.Errorf("stageHubToken succeeded but the token path is not a regular file (err %v)", lerr)
		}
	}
}

// A symlinked .scion directory is refused, and nothing is written into the
// directory it points to.
func TestStageHubToken_RefusesSymlinkedScionDir(t *testing.T) {
	home, _ := isolatedTokenHome(t)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(home, ".scion")); err != nil {
		t.Fatal(err)
	}
	if _, err := stageHubToken(initHubTokenCanary, os.Getuid(), os.Getgid()); err == nil {
		t.Error("stageHubToken through a symlinked .scion succeeded")
	}
	if _, err := os.Lstat(filepath.Join(elsewhere, hub.TokenFile)); !os.IsNotExist(err) {
		t.Errorf("a token file was written through the symlinked directory (err %v)", err)
	}
}

// Same-run restart rule: an existing well-formed credential with a newer
// iat is kept; anything else is replaced by the staged value.
func TestStageHubToken_IATRule(t *testing.T) {
	cases := []struct {
		name     string
		existing string // "" = no file
		staged   string
		wantKept bool
	}{
		{"no existing file", "", iatToken(100), false},
		{"existing newer is kept", iatToken(200), iatToken(100), true},
		{"existing older is replaced", iatToken(100), iatToken(200), false},
		{"equal iat is replaced", iatToken(100), iatToken(100), false},
		{"malformed existing is replaced", "not.a.token", iatToken(100), false},
		{"existing without iat is replaced", initHubTokenCanary, iatToken(100), false},
		{"staged without iat replaces", iatToken(200), initHubTokenCanary, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, tokenPath := isolatedTokenHome(t)
			if tc.existing != "" {
				if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(tokenPath, []byte(tc.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			kept, err := stageHubToken(tc.staged, os.Getuid(), os.Getgid())
			if err != nil {
				t.Fatalf("stageHubToken: %v", err)
			}
			if kept != tc.wantKept {
				t.Errorf("kept = %v, want %v", kept, tc.wantKept)
			}
			want := tc.staged
			if tc.wantKept {
				want = tc.existing
			}
			if got := readFileString(t, tokenPath); got != want {
				t.Error("token file holds the wrong credential")
			}
		})
	}
}

// An unreadable existing file never fails the start: the staged value is
// written.
func TestStageHubToken_UnreadableExistingIsReplaced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	_, tokenPath := isolatedTokenHome(t)
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(iatToken(999)), 0); err != nil {
		t.Fatal(err)
	}
	kept, err := stageHubToken(iatToken(1), os.Getuid(), os.Getgid())
	if err != nil || kept {
		t.Fatalf("stageHubToken: kept=%v err=%v", kept, err)
	}
	if got := readFileString(t, tokenPath); got != iatToken(1) {
		t.Error("staged credential not written over an unreadable file")
	}
}

func TestFinishStagedHubToken_RemovesOnlyOnCleanExit(t *testing.T) {
	_, tokenPath := isolatedTokenHome(t)
	if _, err := stageHubToken(initHubTokenCanary, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	finishStagedHubToken(false)
	if _, err := os.Lstat(tokenPath); err != nil {
		t.Fatalf("staged token file removed after an exit that is not clean (err %v)", err)
	}
	finishStagedHubToken(true)
	if _, err := os.Lstat(tokenPath); !os.IsNotExist(err) {
		t.Errorf("staged token file not removed after a clean exit (err %v)", err)
	}
}

// An unreadable existing file is unlinked before the write only when it is
// a regular file with one link: a hard-linked unreadable file is refused,
// nothing is removed, and the start fails closed.
func TestStageHubToken_UnreadableHardLinkedIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	_, tokenPath := isolatedTokenHome(t)
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(iatToken(5)), 0); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(tokenPath), "other-link")
	if err := os.Link(tokenPath, other); err != nil {
		t.Fatal(err)
	}
	if _, err := stageHubToken(iatToken(9), os.Getuid(), os.Getgid()); err == nil {
		t.Error("stageHubToken replaced a hard-linked unreadable token file")
	}
	for _, p := range []string{tokenPath, other} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed (err %v)", filepath.Base(p), err)
		}
	}
}

// An UNREADABLE non-regular entry (a mode-000 FIFO, which the guarded read
// classes as unreadable rather than non-regular) reaches the unlink path
// and is refused there by the regular-file check: nothing is removed and
// the start fails closed.
func TestStageHubToken_UnreadableFIFOIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode-000 FIFO")
	}
	_, tokenPath := isolatedTokenHome(t)
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(tokenPath, 0); err != nil {
		t.Fatal(err)
	}
	if got := hub.CheckTokenFile(); got != hub.TokenFileUnreadable {
		t.Fatalf("precondition: CheckTokenFile() = %q, want %q", got, hub.TokenFileUnreadable)
	}
	if _, err := stageHubToken(initHubTokenCanary, os.Getuid(), os.Getgid()); err == nil {
		t.Error("stageHubToken succeeded over an unreadable FIFO")
	}
	fi, err := os.Lstat(tokenPath)
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the unreadable FIFO was removed or replaced (err %v)", err)
	}
}

// A non-regular entry (a FIFO) at the token path is never removed or
// written through, and the start fails closed.
func TestStageHubToken_FIFOIsRefused(t *testing.T) {
	_, tokenPath := isolatedTokenHome(t)
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(tokenPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stageHubToken(initHubTokenCanary, os.Getuid(), os.Getgid()); err == nil {
		t.Error("stageHubToken succeeded over a FIFO")
	}
	fi, err := os.Lstat(tokenPath)
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the FIFO was removed or replaced (err %v)", err)
	}
}

// "hub-connected" (or "hosted") is claimed only for a configured client.
// With an endpoint set and no configured client the line is an error that
// names what is missing and never carries credential material.
func TestHubModeLogLine(t *testing.T) {
	reason := func() string { return "agent credential: token file absent and no bootstrap credential" }

	if isErr, line := hubModeLogLine(hub.ModeLocal, false, "", reason); isErr || !strings.Contains(line, "local") {
		t.Errorf("local: isErr=%v line=%q", isErr, line)
	}
	if isErr, line := hubModeLogLine(hub.ModeHubConnected, true, "https://hub.example", reason); isErr || line != "Operating mode: hub-connected (endpoint: https://hub.example)" {
		t.Errorf("configured: isErr=%v line=%q", isErr, line)
	}
	for _, mode := range []hub.Mode{hub.ModeHubConnected, hub.ModeHosted} {
		isErr, line := hubModeLogLine(mode, false, "https://hub.example", reason)
		if !isErr {
			t.Errorf("%v unconfigured: not an error", mode)
		}
		if strings.Contains(line, "hub-connected") || strings.Contains(line, "Operating mode: hosted") {
			t.Errorf("%v unconfigured: line claims a connected mode: %q", mode, line)
		}
		if !strings.Contains(line, "token file absent") || !strings.Contains(line, "conduit") {
			t.Errorf("%v unconfigured: line does not name what is missing: %q", mode, line)
		}
	}
	if _, line := hubModeLogLine(hub.ModeHubConnected, false, "https://hub.example", func() string { return "" }); !strings.Contains(line, "could not be built") {
		t.Errorf("empty reason: line = %q", line)
	}
}

// End to end through the real reason: a malformed token file holding the
// canary is named as malformed, and the canary is not in the line.
func TestHubModeLogLine_RealReasonCarriesNoCredential(t *testing.T) {
	_, tokenPath := isolatedTokenHome(t)
	t.Setenv(hub.EnvHubToken, "")
	t.Setenv(hub.EnvAgentID, "agent-1")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(initHubTokenCanary+" trailing-part"), 0o600); err != nil {
		t.Fatal(err)
	}
	isErr, line := hubModeLogLine(hub.ModeHubConnected, false, "https://hub.example", hub.HubClientUnconfiguredReason)
	if !isErr || !strings.Contains(line, "malformed") {
		t.Errorf("line does not report the malformed file (isErr=%v)", isErr)
	}
	if strings.Contains(line, initHubTokenCanary) {
		t.Error("log line carries the credential")
	}
}
