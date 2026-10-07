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

package chatapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const sharedDirTestProjectID = "abcd1234-ef56-7890-abcd-ef1234567890"

// stubSharedDirSettings replaces the global settings loader with gs.
func stubSharedDirSettings(t *testing.T, gs *config.VersionedSettings) {
	t.Helper()
	orig := loadSharedDirSettings
	loadSharedDirSettings = func() (*config.VersionedSettings, error) { return gs, nil }
	t.Cleanup(func() { loadSharedDirSettings = orig })
}

// useLocalSharedDirs points HOME at a temp dir and selects the local
// shared-dir backend, so tests never read the host's real settings.
func useLocalSharedDirs(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubSharedDirSettings(t, &config.VersionedSettings{})
	return home
}

// nfsSettings returns fake settings selecting the nfs backend with share
// "share1" under mountRoot.
func nfsSettings(mountRoot string) *config.VersionedSettings {
	return &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			SharedDirStorage: &config.V1SharedDirStorageConfig{
				Backend: "nfs",
				NFS: &config.V1NFSConfig{
					MountRoot: mountRoot,
					Shares:    []config.V1NFSShare{{ID: "share1"}},
				},
			},
		},
	}
}

// useNFSSharedDirs points HOME at a temp dir, selects the nfs backend with
// a mounted share, and returns the home dir and the expected
// (symlink-resolved) host path of the project's shared dir name.
func useNFSSharedDirs(t *testing.T, name string) (home, sharedDir string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	mountRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(mountRoot)
	if err != nil {
		t.Fatal(err)
	}
	stubSharedDirSettings(t, nfsSettings(mountRoot))
	return home, filepath.Join(resolved, "share1", "projects", sharedDirTestProjectID, "shared-dirs", name)
}

// useUnavailableNFSSharedDirs selects the nfs backend with a mount root
// that does not exist, and returns the home dir.
func useUnavailableNFSSharedDirs(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubSharedDirSettings(t, nfsSettings(filepath.Join(t.TempDir(), "not-mounted")))
	return home
}

func localSharedDir(home, name string) string {
	return filepath.Join(home, ".scion", "project-configs", "my-project__abcd1234", "shared-dirs", name)
}

func assertNotExist(t *testing.T, path, msg string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s: %s exists (stat err = %v)", msg, path, err)
	}
}

func TestResolveSharedDirHostPath_SettingsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	orig := loadSharedDirSettings
	loadSharedDirSettings = func() (*config.VersionedSettings, error) {
		return nil, scionruntime.ErrSharedDirStorageUnavailable
	}
	t.Cleanup(func() { loadSharedDirSettings = orig })

	_, err := resolveSharedDirHostPath(os.Getenv("HOME"), "my-project", sharedDirTestProjectID, "scratchpad")
	if !errors.Is(err, scionruntime.ErrSharedDirStorageUnavailable) {
		t.Fatalf("err = %v, want ErrSharedDirStorageUnavailable", err)
	}
}

// --- Entry point: inbound Google Chat attachments (downloadInboundAttachment) ---

type fakeDownloader struct{ body string }

func (f fakeDownloader) DownloadAttachment(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func downloadTestInbound(t *testing.T) (string, error) {
	t.Helper()
	r := &CommandRouter{downloader: fakeDownloader{body: "attachment-bytes"}, log: slog.New(slog.DiscardHandler)}
	att := EventAttachment{Name: "note.txt", ContentType: "text/plain", DownloadURI: "https://example.invalid/note.txt"}
	return r.downloadInboundAttachment(context.Background(), att, "spaces/AAA", "my-project", sharedDirTestProjectID)
}

func TestDownloadInboundAttachment_LocalBackend(t *testing.T) {
	home := useLocalSharedDirs(t)

	agentPath, err := downloadTestInbound(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(agentPath, "/scion-volumes/scratchpad/.attachments/_gchat/AAA/") {
		t.Errorf("agentPath = %q", agentPath)
	}
	entries, err := os.ReadDir(filepath.Join(localSharedDir(home, "scratchpad"), ".attachments", "_gchat", "AAA"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("local attachment dir: entries=%d err=%v", len(entries), err)
	}
}

func TestDownloadInboundAttachment_NFSBackend(t *testing.T) {
	home, nfsDir := useNFSSharedDirs(t, "scratchpad")

	if _, err := downloadTestInbound(t); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(nfsDir, ".attachments", "_gchat", "AAA"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("attachment must be staged under the nfs shared dir: entries=%d err=%v", len(entries), err)
	}
	assertNotExist(t, localSharedDir(home, "scratchpad"), "local shared dir must not be written for nfs")
}

func TestDownloadInboundAttachment_NFSUnavailable(t *testing.T) {
	home := useUnavailableNFSSharedDirs(t)

	_, err := downloadTestInbound(t)
	if !errors.Is(err, scionruntime.ErrSharedDirStorageUnavailable) {
		t.Fatalf("err = %v, want ErrSharedDirStorageUnavailable", err)
	}
	assertNotExist(t, localSharedDir(home, "scratchpad"), "must not fall back to the local shared dir")
}

// --- Entry point: outbound agent attachments (ResolveOutboundAttachments) ---

func writeTestFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveOutboundAttachments_Backends(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	paths := []string{
		"/scion-volumes/scratchpad/out/report.md",
		"/workspace/.scion-volumes/scratchpad/out/report.md",
	}

	t.Run("local", func(t *testing.T) {
		home := useLocalSharedDirs(t)
		want := filepath.Join(localSharedDir(home, "scratchpad"), "out", "report.md")
		writeTestFile(t, want, 10)

		got := ResolveOutboundAttachments(log, paths, "my-project", sharedDirTestProjectID)
		if len(got) != 2 || got[0].Path != want || got[1].Path != want {
			t.Fatalf("got %+v, want both paths resolved to %q", got, want)
		}
	})

	t.Run("nfs", func(t *testing.T) {
		_, nfsDir := useNFSSharedDirs(t, "scratchpad")
		want := filepath.Join(nfsDir, "out", "report.md")
		writeTestFile(t, want, 10)

		got := ResolveOutboundAttachments(log, paths, "my-project", sharedDirTestProjectID)
		if len(got) != 2 || got[0].Path != want || got[1].Path != want {
			t.Fatalf("got %+v, want both paths resolved to %q", got, want)
		}
	})

	t.Run("nfs unavailable", func(t *testing.T) {
		home := useUnavailableNFSSharedDirs(t)
		// A file at the local layout must not be picked up as a fallback.
		writeTestFile(t, filepath.Join(localSharedDir(home, "scratchpad"), "out", "report.md"), 10)

		if got := ResolveOutboundAttachments(log, paths, "my-project", sharedDirTestProjectID); len(got) != 0 {
			t.Fatalf("got %+v, want no attachments", got)
		}
		_, err := resolveAgentPath(paths[0], "my-project", sharedDirTestProjectID)
		if !errors.Is(err, scionruntime.ErrSharedDirStorageUnavailable) {
			t.Fatalf("resolveAgentPath err = %v, want ErrSharedDirStorageUnavailable", err)
		}
	})
}

// --- Entry point: oversize checks on notification attachments (sendOversizeErrorCards) ---

func sendOversizeTestCards(t *testing.T) *fakeMessenger {
	t.Helper()
	fm := &fakeMessenger{}
	relay := NewNotificationRelay(nil, fm, slog.New(slog.DiscardHandler))
	links := []state.SpaceLink{{SpaceID: "spaces/AAA", ProjectID: sharedDirTestProjectID, Platform: "googlechat"}}
	relay.sendOversizeErrorCards(context.Background(), []string{"/scion-volumes/scratchpad/big.bin"},
		"my-project", sharedDirTestProjectID, "googlechat", links)
	return fm
}

func TestSendOversizeErrorCards_Backends(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		home := useLocalSharedDirs(t)
		writeTestFile(t, filepath.Join(localSharedDir(home, "scratchpad"), "big.bin"), MaxAttachmentSize+1)

		if fm := sendOversizeTestCards(t); len(fm.messages) != 1 {
			t.Fatalf("sent %d cards, want 1", len(fm.messages))
		}
	})

	t.Run("nfs", func(t *testing.T) {
		_, nfsDir := useNFSSharedDirs(t, "scratchpad")
		writeTestFile(t, filepath.Join(nfsDir, "big.bin"), MaxAttachmentSize+1)

		if fm := sendOversizeTestCards(t); len(fm.messages) != 1 {
			t.Fatalf("sent %d cards, want 1", len(fm.messages))
		}
	})

	t.Run("nfs unavailable", func(t *testing.T) {
		home := useUnavailableNFSSharedDirs(t)
		// A file at the local layout must not be picked up as a fallback.
		writeTestFile(t, filepath.Join(localSharedDir(home, "scratchpad"), "big.bin"), MaxAttachmentSize+1)

		if fm := sendOversizeTestCards(t); len(fm.messages) != 0 {
			t.Fatalf("sent %d cards, want 0", len(fm.messages))
		}
	})
}
