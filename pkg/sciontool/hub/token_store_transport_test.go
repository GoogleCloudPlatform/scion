//go:build unix

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

package hub

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

func makeTestTransportJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	enc := base64.RawURLEncoding
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"sub": "sa@example.iam", "exp": exp.Unix()})
	return enc.EncodeToString(hdr) + "." + enc.EncodeToString(claims) + "." + enc.EncodeToString(make([]byte, 32))
}

func TestTransportTokenIsolation_FileMode(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	t.Cleanup(SetRuntimeTokenStoreForTest())

	runtimeDir := filepath.Join(t.TempDir(), "run-scion")
	runtimePath := filepath.Join(runtimeDir, TokenFile)
	if err := ConfigureIsolatedTokenStore(TokenStoreRuntimeFile, runtimePath); err != nil {
		t.Fatalf("ConfigureIsolatedTokenStore: %v", err)
	}

	wantTransportPath := filepath.Join(runtimeDir, transportauth.TransportTokenFileName)
	if got := TransportTokenFilePath(); got != wantTransportPath {
		t.Fatalf("TransportTokenFilePath() = %q, want %q", got, wantTransportPath)
	}
	if want := filepath.Join(home, ".scion", transportauth.TransportTokenFileName); HandoffTransportTokenFilePath() != want {
		t.Fatalf("HandoffTransportTokenFilePath() = %q, want %q", HandoffTransportTokenFilePath(), want)
	}

	oldTok := makeTestTransportJWT(t, time.Now().Add(time.Hour))
	newTok := makeTestTransportJWT(t, time.Now().Add(2*time.Hour))

	if err := WriteTransportTokenFile(oldTok, 0, 0); err != nil {
		t.Fatalf("WriteTransportTokenFile: %v", err)
	}
	info, err := os.Stat(wantTransportPath)
	if err != nil {
		t.Fatalf("stat runtime transport token: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("runtime transport token mode = %04o, want 0600", perm)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		if st.Uid != 0 || st.Gid != 0 {
			t.Fatalf("runtime transport token owner = %d:%d, want 0:0", st.Uid, st.Gid)
		}
	}

	// Build a client using the runtime transport store and verify AdoptTransportTokenFile
	// takes + unlinks the handoff file and updates the runtime store.
	client := NewClientWithConfig("http://127.0.0.1:1", "agent-tok", "agent-1")
	client.SetTransportAuth(NewTransportTokenFileSource(), transportauth.HeaderProxyAuthorization)

	handoff := HandoffTransportTokenFilePath()
	if err := os.MkdirAll(filepath.Dir(handoff), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoff, []byte(newTok), 0o600); err != nil {
		t.Fatal(err)
	}

	if !client.TransportTokenFileChanged() {
		t.Fatal("TransportTokenFileChanged() = false, want true when handoff file is present")
	}

	adopted, err := client.AdoptTransportTokenFile(1000, 1000)
	if err != nil || !adopted {
		t.Fatalf("AdoptTransportTokenFile() = (%v, %v), want (true, nil)", adopted, err)
	}
	if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
		t.Fatalf("handoff transport token still exists after adopt: err=%v", err)
	}
	gotRuntime, err := ReadTransportTokenFileGuarded(wantTransportPath)
	if err != nil || gotRuntime != newTok {
		t.Fatalf("runtime transport token = %q (err=%v), want newTok", gotRuntime, err)
	}
	gotSrc, err := client.oidcSource.Token()
	if err != nil || gotSrc != newTok {
		t.Fatalf("oidcSource.Token() = %q (err=%v), want newTok", gotSrc, err)
	}

	// Unparseable handoff must be unlinked and must NOT clobber the runtime store or restore to $HOME.
	if err := os.WriteFile(handoff, []byte("not-a-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	adopted, err = client.AdoptTransportTokenFile(1000, 1000)
	if err == nil || adopted {
		t.Fatalf("AdoptTransportTokenFile(unparseable) = (%v, %v), want (false, err)", adopted, err)
	}
	if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
		t.Fatalf("unparseable handoff transport token must be unlinked and not restored: err=%v", err)
	}
	gotRuntime, err = ReadTransportTokenFileGuarded(wantTransportPath)
	if err != nil || gotRuntime != newTok {
		t.Fatalf("runtime transport token clobbered by unparseable handoff: %q (err=%v)", gotRuntime, err)
	}
}

func TestTransportTokenIsolation_MemoryMode(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	t.Cleanup(SetRuntimeTokenStoreForTest())

	if err := ConfigureIsolatedTokenStore(TokenStoreMemory, ""); err != nil {
		t.Fatalf("ConfigureIsolatedTokenStore(memory): %v", err)
	}
	if got := TransportTokenFilePath(); got != "" {
		t.Fatalf("TransportTokenFilePath() = %q, want empty in memory mode", got)
	}

	tok1 := makeTestTransportJWT(t, time.Now().Add(time.Hour))
	tok2 := makeTestTransportJWT(t, time.Now().Add(2*time.Hour))

	if err := WriteTransportTokenFile(tok1, 0, 0); err != nil {
		t.Fatalf("WriteTransportTokenFile(memory): %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".scion")); !os.IsNotExist(err) {
		t.Fatalf("memory mode must not create $HOME/.scion: err=%v", err)
	}

	client := NewClientWithConfig("http://127.0.0.1:1", "agent-tok", "agent-1")
	client.SetTransportAuth(NewTransportTokenFileSource(), transportauth.HeaderProxyAuthorization)

	got, err := client.oidcSource.Token()
	if err != nil || got != tok1 {
		t.Fatalf("oidcSource.Token() = %q (err=%v), want tok1", got, err)
	}

	// Adopt a fresh handoff transport token in memory mode.
	handoff := HandoffTransportTokenFilePath()
	if err := os.MkdirAll(filepath.Dir(handoff), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoff, []byte(tok2), 0o600); err != nil {
		t.Fatal(err)
	}

	adopted, err := client.AdoptTransportTokenFile(1000, 1000)
	if err != nil || !adopted {
		t.Fatalf("AdoptTransportTokenFile(memory) = (%v, %v), want (true, nil)", adopted, err)
	}
	if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
		t.Fatalf("handoff transport token still exists after adopt in memory mode: err=%v", err)
	}
	got, err = client.oidcSource.Token()
	if err != nil || got != tok2 {
		t.Fatalf("oidcSource.Token() after adopt = %q (err=%v), want tok2", got, err)
	}
}

func TestTakeHandoffTransportToken_SymlinkAndDirectory(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
		t.Fatal(err)
	}

	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	handoff := HandoffTransportTokenFilePath()
	if err := os.Symlink(victim, handoff); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeHandoffTransportToken(); err == nil {
		t.Fatal("expected error taking symlinked transport token")
	}
	if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
		t.Fatalf("symlinked transport handoff must be unlinked: err=%v", err)
	}

	if err := os.MkdirAll(handoff, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeHandoffTransportToken(); !errors.Is(err, ErrHandoffUnlinkFailed) {
		t.Fatalf("TakeHandoffTransportToken(dir) = %v, want ErrHandoffUnlinkFailed", err)
	}
}
