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

package hub

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// conditionCanary is an obviously fake credential.
const conditionCanary = "fake-hub-token-canary-7239-not-a-credential"

// fakeIATToken builds a three-part token with the given iat. The parts are
// encoded at run time, so no token-shaped literal is in the source.
func fakeIATToken(iat int64) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		enc([]byte(fmt.Sprintf(`{"sub":"agent-1","iat":%d}`, iat))) + "." +
		enc([]byte("fake-signature-not-real-0123456789"))
}

func writeTokenAt(t *testing.T, home, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, TokenFile)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckTokenFile_Conditions(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, home string)
		want  TokenFileCondition
	}{
		{"absent", func(t *testing.T, home string) {}, TokenFileAbsent},
		{"present", func(t *testing.T, home string) { writeTokenAt(t, home, conditionCanary+"\n") }, TokenFilePresent},
		{"empty", func(t *testing.T, home string) { writeTokenAt(t, home, " \n") }, TokenFileMalformed},
		{"interior whitespace", func(t *testing.T, home string) { writeTokenAt(t, home, "two words") }, TokenFileMalformed},
		{"too large", func(t *testing.T, home string) { writeTokenAt(t, home, strings.Repeat("x", tokenFileMaxBytes+1)) }, TokenFileMalformed},
		{"symlink", func(t *testing.T, home string) {
			other := filepath.Join(t.TempDir(), "other")
			if err := os.WriteFile(other, []byte(conditionCanary), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, filepath.Join(home, ".scion", TokenFile)); err != nil {
				t.Fatal(err)
			}
		}, TokenFileNotRegular},
		{"unreadable directory", func(t *testing.T, home string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a mode-000 directory")
			}
			writeTokenAt(t, home, conditionCanary)
			dir := filepath.Join(home, ".scion")
			if err := os.Chmod(dir, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		}, TokenFileUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Cleanup(SetTokenHome(home))
			tc.setup(t, home)
			if got := CheckTokenFile(); got != tc.want {
				t.Errorf("CheckTokenFile() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The owner check's refusal is classified by its sentinel, not its text.
func TestClassifyTokenFileErr_WrongOwner(t *testing.T) {
	err := fmt.Errorf("refusing to read %s: %w", "/x/.scion/scion-token", errTokenFileOwner)
	if got := classifyTokenFileErr(err); got != TokenFileWrongOwner {
		t.Errorf("classify = %q, want %q", got, TokenFileWrongOwner)
	}
	if !strings.Contains(err.Error(), "unexpected owner") {
		t.Errorf("owner refusal text changed: %q", err.Error())
	}
}

func TestTokenIssuedAt(t *testing.T) {
	if iat, ok := TokenIssuedAt(fakeIATToken(1700000000)); !ok || !iat.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("TokenIssuedAt(well-formed) = %v, %v", iat, ok)
	}
	enc := base64.RawURLEncoding.EncodeToString
	for name, tok := range map[string]string{
		"dev token":        conditionCanary,
		"two parts":        "a.b",
		"bad base64":       "a.!!!.c",
		"claims not json":  "a." + enc([]byte("not json")) + ".c",
		"no iat":           "a." + enc([]byte(`{"sub":"x"}`)) + ".c",
		"zero iat":         "a." + enc([]byte(`{"iat":0}`)) + ".c",
		"non-numeric iat":  "a." + enc([]byte(`{"iat":"soon"}`)) + ".c",
		"fractional iat":   "a." + enc([]byte(`{"iat":1.5}`)) + ".c",
		"empty first part": "." + enc([]byte(`{"iat":5}`)) + ".c",
	} {
		if _, ok := TokenIssuedAt(tok); ok {
			t.Errorf("TokenIssuedAt(%s) ok, want not ok", name)
		}
	}
}

func TestHubClientUnconfiguredReason_NamesConditionsOnly(t *testing.T) {
	scrubHubEnv(t)
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))

	// Agent ID set, token file absent, no bootstrap credential.
	t.Setenv(EnvAgentID, "agent-1")
	got := HubClientUnconfiguredReason()
	if !strings.Contains(got, "token file absent") || strings.Contains(got, EnvAgentID+") is unset") {
		t.Errorf("reason = %q, want only the absent token file", got)
	}

	// A malformed file: named, its content never quoted.
	writeTokenAt(t, home, conditionCanary+" "+conditionCanary)
	got = HubClientUnconfiguredReason()
	if !strings.Contains(got, "token file malformed") || strings.Contains(got, conditionCanary) {
		t.Errorf("reason does not name the malformed file, or quotes content (length %d)", len(got))
	}

	// Agent ID unset as well.
	t.Setenv(EnvAgentID, "")
	if got := HubClientUnconfiguredReason(); !strings.Contains(got, EnvAgentID+") is unset") {
		t.Errorf("reason = %q, want the unset agent ID", got)
	}

	// Nothing required missing.
	t.Setenv(EnvAgentID, "agent-1")
	writeTokenAt(t, home, conditionCanary)
	if got := HubClientUnconfiguredReason(); got != "" {
		t.Errorf("reason = %q, want empty with a present token and agent ID", got)
	}
}

func TestRemoveTokenFile(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	if err := RemoveTokenFile(); err != nil {
		t.Fatalf("RemoveTokenFile with no file: %v", err)
	}
	p := writeTokenAt(t, home, conditionCanary)
	if err := RemoveTokenFile(); err != nil {
		t.Fatalf("RemoveTokenFile: %v", err)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Errorf("token file still present after RemoveTokenFile (err %v)", err)
	}

	// A symlink at the leaf is removed, never followed.
	other := filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(other, []byte(conditionCanary), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, p); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTokenFile(); err != nil {
		t.Fatalf("RemoveTokenFile on a symlink: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("symlink target removed or changed: %v", err)
	}
}
