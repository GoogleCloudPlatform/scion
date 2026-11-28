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
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

func TestTakeHandoffToken_RegularInvalidAndMissing(t *testing.T) {
	t.Run("missing .scion dir returns ErrNotExist", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))

		tok, err := TakeHandoffToken()
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want fs.ErrNotExist", err)
		}
		if tok != "" {
			t.Fatalf("tok = %q, want empty", tok)
		}
	})

	t.Run("missing token file returns ErrNotExist", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
			t.Fatal(err)
		}

		tok, err := TakeHandoffToken()
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want fs.ErrNotExist", err)
		}
		if tok != "" {
			t.Fatalf("tok = %q, want empty", tok)
		}
	})

	t.Run("valid regular file is read and unlinked", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))
		scionDir := filepath.Join(home, ".scion")
		if err := os.MkdirAll(scionDir, 0o700); err != nil {
			t.Fatal(err)
		}
		handoff := HandoffTokenFilePath()
		if err := os.WriteFile(handoff, []byte("  handoff-jwt-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		tok, err := TakeHandoffToken()
		if err != nil {
			t.Fatalf("TakeHandoffToken: %v", err)
		}
		if tok != "handoff-jwt-token" {
			t.Fatalf("tok = %q, want %q", tok, "handoff-jwt-token")
		}
		if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
			t.Fatalf("handoff file still exists after take: lstat err=%v", err)
		}
	})

	t.Run("invalid content is still unlinked", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
			t.Fatal(err)
		}
		handoff := HandoffTokenFilePath()
		if err := os.WriteFile(handoff, []byte("not-a-valid-jwt-at-all"), 0o600); err != nil {
			t.Fatal(err)
		}

		tok, err := TakeHandoffToken()
		if err != nil {
			t.Fatalf("TakeHandoffToken: %v", err)
		}
		if tok != "not-a-valid-jwt-at-all" {
			t.Fatalf("tok = %q, want %q", tok, "not-a-valid-jwt-at-all")
		}
		if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
			t.Fatalf("invalid handoff file still exists after take: lstat err=%v", err)
		}
	})
}

func TestTakeHandoffToken_EdgeCases(t *testing.T) {
	t.Run("symlink leaf refused and unlinked without touching target", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(victim, []byte("do-not-read"), 0o600); err != nil {
			t.Fatal(err)
		}
		handoff := HandoffTokenFilePath()
		if err := os.Symlink(victim, handoff); err != nil {
			t.Fatal(err)
		}

		tok, err := TakeHandoffToken()
		if err == nil {
			t.Fatalf("expected error on symlink, got tok=%q", tok)
		}
		if errors.Is(err, ErrHandoffUnlinkFailed) {
			t.Fatalf("symlink unlink should have succeeded, got %v", err)
		}
		if _, statErr := os.Lstat(handoff); !os.IsNotExist(statErr) {
			t.Fatalf("symlink still present after take: err=%v", statErr)
		}
		if got, rerr := os.ReadFile(victim); rerr != nil || string(got) != "do-not-read" {
			t.Fatalf("victim modified or removed: data=%q err=%v", got, rerr)
		}
	})

	t.Run("FIFO leaf refused without blocking and unlinked", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
			t.Fatal(err)
		}
		handoff := HandoffTokenFilePath()
		if err := mkfifoForTest(handoff, 0o600); err != nil {
			t.Fatalf("mkfifoForTest: %v", err)
		}

		tok, err := TakeHandoffToken()
		if err == nil {
			t.Fatalf("expected error on FIFO, got tok=%q", tok)
		}
		if !errors.Is(err, dirfd.ErrNotSingleLinkRegular) {
			t.Fatalf("err = %v, want dirfd.ErrNotSingleLinkRegular", err)
		}
		if _, statErr := os.Lstat(handoff); !os.IsNotExist(statErr) {
			t.Fatalf("FIFO still present after take: err=%v", statErr)
		}
	})

	t.Run("oversize file refused and unlinked", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
			t.Fatal(err)
		}
		handoff := HandoffTokenFilePath()
		if err := os.WriteFile(handoff, []byte(strings.Repeat("a", tokenFileMaxBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}

		tok, err := TakeHandoffToken()
		if !errors.Is(err, dirfd.ErrTooLarge) {
			t.Fatalf("err = %v (tok=%q), want dirfd.ErrTooLarge", err, tok)
		}
		if _, statErr := os.Lstat(handoff); !os.IsNotExist(statErr) {
			t.Fatalf("oversize file still present after take: err=%v", statErr)
		}
	})

	t.Run("hardlink refused and handoff entry unlinked without touching original", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
			t.Fatal(err)
		}
		orig := filepath.Join(home, "orig")
		if err := os.WriteFile(orig, []byte("original-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		handoff := HandoffTokenFilePath()
		if err := os.Link(orig, handoff); err != nil {
			t.Fatal(err)
		}

		tok, err := TakeHandoffToken()
		if !errors.Is(err, dirfd.ErrNotSingleLinkRegular) {
			t.Fatalf("err = %v (tok=%q), want dirfd.ErrNotSingleLinkRegular", err, tok)
		}
		if _, statErr := os.Lstat(handoff); !os.IsNotExist(statErr) {
			t.Fatalf("hardlink entry still present after take: err=%v", statErr)
		}
		if got, rerr := os.ReadFile(orig); rerr != nil || string(got) != "original-secret" {
			t.Fatalf("original file damaged: data=%q err=%v", got, rerr)
		}
	})

	t.Run("directory at leaf fails closed with ErrHandoffUnlinkFailed", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))
		handoff := HandoffTokenFilePath()
		if err := os.MkdirAll(handoff, 0o700); err != nil {
			t.Fatal(err)
		}

		tok, err := TakeHandoffToken()
		if !errors.Is(err, ErrHandoffUnlinkFailed) {
			t.Fatalf("err = %v (tok=%q), want ErrHandoffUnlinkFailed", err, tok)
		}
	})

	t.Run("symlinked .scion parent fails closed with ErrHandoffUnlinkFailed", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))
		victimDir := t.TempDir()
		victimFile := filepath.Join(victimDir, TokenFile)
		if err := os.WriteFile(victimFile, []byte("keep-me"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victimDir, filepath.Join(home, ".scion")); err != nil {
			t.Fatal(err)
		}

		tok, err := TakeHandoffToken()
		if !errors.Is(err, ErrHandoffUnlinkFailed) {
			t.Fatalf("err = %v (tok=%q), want ErrHandoffUnlinkFailed", err, tok)
		}
		if got, rerr := os.ReadFile(victimFile); rerr != nil || string(got) != "keep-me" {
			t.Fatalf("victim file touched through symlinked .scion: data=%q err=%v", got, rerr)
		}
	})

	t.Run("unwritable .scion dir fails closed with ErrHandoffUnlinkFailed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses DAC write permission on 0500 directory")
		}
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))
		scionDir := filepath.Join(home, ".scion")
		if err := os.MkdirAll(scionDir, 0o700); err != nil {
			t.Fatal(err)
		}
		handoff := HandoffTokenFilePath()
		if err := os.WriteFile(handoff, []byte("tok"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(scionDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(scionDir, 0o700) })

		tok, err := TakeHandoffToken()
		if !errors.Is(err, ErrHandoffUnlinkFailed) {
			t.Fatalf("err = %v (tok=%q), want ErrHandoffUnlinkFailed", err, tok)
		}
	})
}

func TestConfigureIsolatedTokenStore_FileMode(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	t.Cleanup(SetRuntimeTokenStoreForTest())

	runtimeDir := filepath.Join(t.TempDir(), "run-scion")
	runtimePath := filepath.Join(runtimeDir, TokenFile)

	if err := ConfigureIsolatedTokenStore(TokenStoreRuntimeFile, runtimePath); err != nil {
		t.Fatalf("ConfigureIsolatedTokenStore: %v", err)
	}
	if got := CurrentTokenStoreMode(); got != TokenStoreRuntimeFile {
		t.Fatalf("CurrentTokenStoreMode() = %v, want TokenStoreRuntimeFile", got)
	}
	if got := TokenFilePath(); got != runtimePath {
		t.Fatalf("TokenFilePath() = %q, want %q", got, runtimePath)
	}
	if want := filepath.Join(home, ".scion", TokenFile); HandoffTokenFilePath() != want {
		t.Fatalf("HandoffTokenFilePath() = %q, want %q", HandoffTokenFilePath(), want)
	}

	dirInfo, err := os.Stat(runtimeDir)
	if err != nil {
		t.Fatalf("stat runtime dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("runtime dir mode = %04o, want 0700", perm)
	}
	if st, ok := dirInfo.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		if st.Uid != 0 || st.Gid != 0 {
			t.Fatalf("runtime dir owner = %d:%d, want 0:0", st.Uid, st.Gid)
		}
	}

	if err := WriteTokenFile("runtime-secret-token", 0, 0); err != nil {
		t.Fatalf("WriteTokenFile: %v", err)
	}
	if got := ReadTokenFile(); got != "runtime-secret-token" {
		t.Fatalf("ReadTokenFile() = %q, want %q", got, "runtime-secret-token")
	}
	if cond := CheckTokenFile(); cond != TokenFilePresent {
		t.Fatalf("CheckTokenFile() = %q, want %q", cond, TokenFilePresent)
	}

	fileInfo, err := os.Stat(runtimePath)
	if err != nil {
		t.Fatalf("stat runtime token file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("runtime token file mode = %04o, want 0600", perm)
	}
	if st, ok := fileInfo.Sys().(*syscall.Stat_t); ok {
		if int(st.Uid) != os.Geteuid() {
			t.Fatalf("runtime token file uid = %d, want %d", st.Uid, os.Geteuid())
		}
		if os.Geteuid() == 0 && (st.Uid != 0 || st.Gid != 0) {
			t.Fatalf("runtime token file owner = %d:%d, want 0:0", st.Uid, st.Gid)
		}
	}

	// Home path must remain untouched.
	if _, err := os.Lstat(HandoffTokenFilePath()); !os.IsNotExist(err) {
		t.Fatalf("home handoff path should not exist in file mode: err=%v", err)
	}

	if err := RemoveTokenFile(); err != nil {
		t.Fatalf("RemoveTokenFile: %v", err)
	}
	if _, err := os.Lstat(runtimePath); !os.IsNotExist(err) {
		t.Fatalf("runtime token file still exists after RemoveTokenFile: err=%v", err)
	}
}

func TestConfigureIsolatedTokenStore_MemoryMode(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	t.Cleanup(SetRuntimeTokenStoreForTest())

	if err := ConfigureIsolatedTokenStore(TokenStoreMemory, ""); err != nil {
		t.Fatalf("ConfigureIsolatedTokenStore(memory): %v", err)
	}
	if got := TokenFilePath(); got != "" {
		t.Fatalf("TokenFilePath() = %q, want empty in memory mode", got)
	}
	if want := filepath.Join(home, ".scion", TokenFile); HandoffTokenFilePath() != want {
		t.Fatalf("HandoffTokenFilePath() = %q, want %q", HandoffTokenFilePath(), want)
	}

	if err := WriteTokenFile("mem-only-token", 0, 0); err != nil {
		t.Fatalf("WriteTokenFile in memory mode: %v", err)
	}
	if got := ReadTokenFile(); got != "mem-only-token" {
		t.Fatalf("ReadTokenFile() = %q, want %q", got, "mem-only-token")
	}
	if cond := CheckTokenFile(); cond != TokenFilePresent {
		t.Fatalf("CheckTokenFile() = %q, want %q", cond, TokenFilePresent)
	}
	if err := ChownTokenFile(1000, 1000); err != nil {
		t.Fatalf("ChownTokenFile in memory mode: %v", err)
	}

	// Nothing written to $HOME/.scion.
	if _, err := os.Lstat(filepath.Join(home, ".scion")); !os.IsNotExist(err) {
		t.Fatalf("memory mode should not create $HOME/.scion: err=%v", err)
	}

	// NewClient picks up the in-memory token, and RefreshToken updates memory without writing to disk.
	refreshedExpiry := time.Now().Add(10 * time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/agent-mem-1/token/refresh" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"mem-refreshed-token","expires_at":"` + refreshedExpiry + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	t.Setenv(EnvHubEndpoint, srv.URL)
	t.Setenv(EnvHubURL, "")
	t.Setenv(EnvHubToken, "")
	t.Setenv(EnvAgentID, "agent-mem-1")

	client := NewClient()
	if client == nil || !client.IsConfigured() {
		t.Fatalf("NewClient() returned unconfigured client in memory mode")
	}
	if got := client.AuthToken(); got != "mem-only-token" {
		t.Fatalf("client.AuthToken() = %q, want %q", got, "mem-only-token")
	}
	newTok, _, err := client.RefreshToken(context.Background())
	if err != nil {
		t.Fatalf("RefreshToken in memory mode: %v", err)
	}
	if newTok != "mem-refreshed-token" || client.AuthToken() != "mem-refreshed-token" {
		t.Fatalf("refreshed token mismatch: returned=%q client=%q", newTok, client.AuthToken())
	}
	if got := ReadTokenFile(); got != "mem-refreshed-token" {
		t.Fatalf("ReadTokenFile() after refresh = %q, want %q", got, "mem-refreshed-token")
	}
	if _, err := os.Lstat(filepath.Join(home, ".scion")); !os.IsNotExist(err) {
		t.Fatalf("refresh in memory mode must not write to $HOME/.scion: err=%v", err)
	}
}

func TestTokenStore_NonShellHomeModeRegression(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))

	if got := CurrentTokenStoreMode(); got != TokenStoreHome {
		t.Fatalf("default mode = %v, want TokenStoreHome", got)
	}
	if TokenFilePath() != HandoffTokenFilePath() {
		t.Fatalf("home mode: TokenFilePath()=%q != HandoffTokenFilePath()=%q", TokenFilePath(), HandoffTokenFilePath())
	}
	if err := WriteTokenFile("home-token", 0, 0); err != nil {
		t.Fatalf("WriteTokenFile: %v", err)
	}
	if got := ReadTokenFile(); got != "home-token" {
		t.Fatalf("ReadTokenFile() = %q, want %q", got, "home-token")
	}
	data, err := os.ReadFile(filepath.Join(home, ".scion", TokenFile))
	if err != nil || string(data) != "home-token" {
		t.Fatalf("home token file = %q, err=%v", data, err)
	}
}

func TestTokenFileWatcher_IsolatedWatchesHandoffPath(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	t.Cleanup(SetRuntimeTokenStoreForTest())

	runtimePath := filepath.Join(t.TempDir(), "run-scion", TokenFile)
	if err := ConfigureIsolatedTokenStore(TokenStoreRuntimeFile, runtimePath); err != nil {
		t.Fatalf("ConfigureIsolatedTokenStore: %v", err)
	}
	if err := WriteTokenFile("initial-runtime-tok", 0, 0); err != nil {
		t.Fatalf("WriteTokenFile: %v", err)
	}

	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	notify := make(chan struct{}, 1)
	w := NewTokenFileWatcher(TokenWatchConfig{
		Interval:    time.Second,
		StableDelay: 100 * time.Millisecond,
		Notify:      notify,
		Clock:       fc,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := w.Run(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Writing to the runtime store must NOT trigger the watcher.
	if err := WriteTokenFile("updated-runtime-tok", 0, 0); err != nil {
		t.Fatalf("WriteTokenFile: %v", err)
	}
	fc.Advance(2 * time.Second)
	select {
	case <-notify:
		t.Fatal("watcher fired on runtime store write; it must watch only the handoff path")
	case <-time.After(50 * time.Millisecond):
	}

	// Writing to the handoff path MUST trigger the watcher.
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HandoffTokenFilePath(), []byte("new-handoff-tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	fc.Advance(time.Second)
	time.Sleep(10 * time.Millisecond)
	fc.Advance(150 * time.Millisecond)

	select {
	case <-notify:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not fire when handoff file was written")
	}
}

func TestRemoveStaleHandoffTmpAndVerifyAbsent(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))

	if err := RemoveStaleHandoffTmpFile(); err != nil {
		t.Fatalf("RemoveStaleHandoffTmpFile (missing): %v", err)
	}
	if err := VerifyHandoffTokenAbsent(); err != nil {
		t.Fatalf("VerifyHandoffTokenAbsent (missing): %v", err)
	}

	scionDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(scionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tmpPath := HandoffTokenTmpFilePath()
	if err := os.WriteFile(tmpPath, []byte("stale-tmp"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStaleHandoffTmpFile(); err != nil {
		t.Fatalf("RemoveStaleHandoffTmpFile: %v", err)
	}
	if _, err := os.Lstat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("stale tmp file still exists: err=%v", err)
	}

	handoff := HandoffTokenFilePath()
	if err := os.WriteFile(handoff, []byte("present"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyHandoffTokenAbsent(); !errors.Is(err, ErrHandoffUnlinkFailed) {
		t.Fatalf("VerifyHandoffTokenAbsent (present) = %v, want ErrHandoffUnlinkFailed", err)
	}
}

func TestRuntimeStoreGuard_PanicsOnDefaultPathWithoutOverride(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when configuring DefaultRuntimeTokenPath without SetRuntimeTokenStoreForTest()")
		}
	}()
	_ = ConfigureIsolatedTokenStore(TokenStoreRuntimeFile, DefaultRuntimeTokenPath)
}
