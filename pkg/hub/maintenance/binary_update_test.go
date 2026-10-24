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

package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRepo = "GoogleCloudPlatform/scion"

// fakeGitHub serves the GitHub API, release downloads (github.com redirecting
// to objects.githubusercontent.com) and LATEST.json, through an
// http.RoundTripper, so requests keep their real https URLs and the
// executor's URL pinning and redirect policy are exercised unchanged.
type fakeGitHub struct {
	t *testing.T

	mu       sync.Mutex
	requests []string

	releases map[string]string // tag → release JSON
	files    map[string][]byte // github.com download path → body
	latest   string            // LATEST.json body
	// redirectHost is where github.com download requests are redirected.
	redirectHost string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	return &fakeGitHub{
		t:            t,
		releases:     map[string]string{},
		files:        map[string][]byte{},
		redirectHost: "objects.githubusercontent.com",
	}
}

func (f *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.String())
	f.mu.Unlock()
	rec := httptest.NewRecorder()
	f.serve(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Host == "api.github.com" && strings.HasPrefix(r.URL.Path, "/repos/"+testRepo+"/releases/tags/"):
		tag := strings.TrimPrefix(r.URL.Path, "/repos/"+testRepo+"/releases/tags/")
		body, ok := f.releases[tag]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	case r.URL.Host == "github.com":
		if _, ok := f.files[r.URL.Path]; !ok {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "https://"+f.redirectHost+r.URL.Path, http.StatusFound)
	case strings.HasSuffix(r.URL.Host, "githubusercontent.com") && r.URL.Host != "raw.githubusercontent.com",
		r.URL.Host == "evil.example.com":
		body, ok := f.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	case r.URL.Host == "raw.githubusercontent.com" && r.URL.Path == "/"+testRepo+"/main/LATEST.json":
		_, _ = w.Write([]byte(f.latest))
	default:
		f.t.Errorf("unexpected request %s", r.URL)
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeGitHub) requested(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func downloadPath(tag, name string) string {
	return fmt.Sprintf("/%s/releases/download/%s/%s", testRepo, tag, name)
}

// addRelease publishes tag with a tarball whose `scion version` reports
// reportVersion, and a SHA256SUMS listing it (plus a bogus self-entry).
// It returns the tarball.
func (f *fakeGitHub) addRelease(tag, published, reportVersion string) []byte {
	f.t.Helper()
	script := fmt.Sprintf("#!/bin/sh\necho '{\"version\":\"%s\"}'\n", reportVersion)
	tarball := createTestTarball(f.t, map[string][]byte{
		"scion":   []byte(script),
		"LICENSE": []byte("license"),
	})
	sum := sha256.Sum256(tarball)
	asset := BinaryAssetName()
	sums := fmt.Sprintf("%s  SHA256SUMS\n%s  %s\n", emptySHA256, hex.EncodeToString(sum[:]), asset)
	f.files[downloadPath(tag, asset)] = tarball
	f.files[downloadPath(tag, ChecksumsAssetName)] = []byte(sums)
	f.setRelease(tag, published,
		"https://github.com"+downloadPath(tag, asset),
		"https://github.com"+downloadPath(tag, ChecksumsAssetName))
	return tarball
}

func (f *fakeGitHub) setRelease(tag, published, binURL, sumsURL string) {
	f.releases[tag] = fmt.Sprintf(`{"tag_name":%q,"draft":false,"published_at":%q,"assets":[
		{"name":%q,"browser_download_url":%q},
		{"name":"SHA256SUMS","browser_download_url":%q}]}`,
		tag, published, BinaryAssetName(), binURL, sumsURL)
}

// fakeInstaller records the executor's calls.
type fakeInstaller struct {
	preflightErr error
	calls        []string
	stagedBin    []byte
}

func (f *fakeInstaller) Name() string { return "fake" }
func (f *fakeInstaller) Preflight(context.Context) error {
	f.calls = append(f.calls, "preflight")
	return f.preflightErr
}
func (f *fakeInstaller) Stage(_ context.Context, tag, bin string) (string, error) {
	f.calls = append(f.calls, "stage:"+tag)
	b, err := os.ReadFile(bin)
	f.stagedBin = b
	return "staged-" + tag, err
}
func (f *fakeInstaller) Activate(_ context.Context, staged string) error {
	f.calls = append(f.calls, "activate:"+staged)
	return nil
}
func (f *fakeInstaller) Restart(context.Context) error {
	f.calls = append(f.calls, "restart")
	return nil
}
func (f *fakeInstaller) Retained() []InstalledRelease { return nil }
func (f *fakeInstaller) Status() InstallerStatus {
	return InstallerStatus{Strategy: "fake", Supported: true}
}

func newTestExecutor(gh *fakeGitHub, inst Installer, current string) *BinaryUpdateExecutor {
	client := &http.Client{Transport: gh}
	return NewBinaryUpdateExecutor(BinaryUpdateOptions{
		GitHubRepo:     testRepo,
		Installer:      inst,
		Releases:       &GitHubReleases{Repo: testRepo, Client: client},
		HTTPClient:     client,
		CurrentVersion: current,
	})
}

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("update-binary runs on Linux only")
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	re, ok := IsRequestError(err)
	if !ok || re.Code != code {
		t.Fatalf("err = %v, want request error %q", err, code)
	}
}

func TestBinaryUpdate_ValidateParams(t *testing.T) {
	e := NewBinaryUpdateExecutor(BinaryUpdateOptions{CurrentVersion: "nightly-20261022"})
	tests := []struct {
		name   string
		params map[string]string
		code   string // "" = valid
	}{
		{"tag", map[string]string{"target_version": "nightly-20261023"}, ""},
		{"tag with channel and pin", map[string]string{"target_version": "nightly-20261023", "channel": "nightly", "pin": "latest"}, ""},
		{"cross-channel tag (order checked at run time)", map[string]string{"target_version": "v0.3.0-preview.4"}, ""},
		{"latest in current channel", map[string]string{"target_version": "latest:nightly"}, ""},
		{"download_url", map[string]string{"target_version": "nightly-20261023", "download_url": "https://evil/x"}, CodeUnknownParam},
		{"checksums_url", map[string]string{"target_version": "nightly-20261023", "checksums_url": "x"}, CodeUnknownParam},
		{"current_version", map[string]string{"target_version": "nightly-20261023", "current_version": "dev"}, CodeUnknownParam},
		{"confirm_downgrade (P3)", map[string]string{"target_version": "nightly-20261023", "confirm_downgrade": "x"}, CodeUnknownParam},
		{"dryRun", map[string]string{"target_version": "nightly-20261023", "dryRun": "true"}, CodeUnknownParam},
		{"missing target", map[string]string{}, CodeInvalidParam},
		{"not a tag", map[string]string{"target_version": "latest"}, CodeInvalidParam},
		{"path in tag", map[string]string{"target_version": "nightly-../../x"}, CodeInvalidParam},
		{"channel mismatch", map[string]string{"target_version": "nightly-20261023", "channel": "preview"}, CodeInvalidParam},
		{"latest other channel", map[string]string{"target_version": "latest:preview"}, CodeInvalidParam},
		{"latest unknown channel", map[string]string{"target_version": "latest:beta"}, CodeInvalidParam},
		{"latest channel mismatch", map[string]string{"target_version": "latest:nightly", "channel": "preview"}, CodeInvalidParam},
		{"same", map[string]string{"target_version": "nightly-20261022"}, CodeAlreadyRunning},
		{"older", map[string]string{"target_version": "nightly-20261021"}, CodeDowngradeNotYetSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := e.ValidateParams(tt.params)
			if tt.code == "" {
				if err != nil {
					t.Fatalf("ValidateParams: %v", err)
				}
				return
			}
			wantCode(t, err, tt.code)
		})
	}

	t.Run("configured channel defines latest", func(t *testing.T) {
		e := NewBinaryUpdateExecutor(BinaryUpdateOptions{CurrentVersion: "nightly-20261022", Channel: "preview"})
		if err := e.ValidateParams(map[string]string{"target_version": "latest:preview"}); err != nil {
			t.Fatal(err)
		}
		wantCode(t, e.ValidateParams(map[string]string{"target_version": "latest:nightly"}), CodeInvalidParam)
	})

	t.Run("status codes", func(t *testing.T) {
		if (&RequestError{Code: CodeUnknownParam}).HTTPStatus() != http.StatusBadRequest ||
			(&RequestError{Code: CodeInvalidParam}).HTTPStatus() != http.StatusBadRequest ||
			(&RequestError{Code: CodeAlreadyRunning}).HTTPStatus() != http.StatusConflict ||
			(&RequestError{Code: CodeDowngradeNotYetSupported}).HTTPStatus() != http.StatusConflict {
			t.Fatal("unexpected status mapping")
		}
	})
}

func TestBinaryUpdate_Success(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
	inst := &fakeInstaller{}
	e := newTestExecutor(gh, inst, "nightly-20261022")

	var hooked *PendingRestart
	var log bytes.Buffer
	err := e.RunWithBeforeRestart(context.Background(), &log, map[string]string{"target_version": "nightly-20261023"},
		func(_ context.Context, p PendingRestart) error {
			inst.calls = append(inst.calls, "hook")
			hooked = &p
			return nil
		})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	want := []string{"preflight", "stage:nightly-20261023", "activate:staged-nightly-20261023", "hook", "restart"}
	if strings.Join(inst.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", inst.calls, want)
	}
	if hooked == nil || *hooked != (PendingRestart{From: "nightly-20261022", To: "nightly-20261023", Strategy: "fake"}) {
		t.Fatalf("hook got %+v", hooked)
	}
	if !strings.Contains(string(inst.stagedBin), `"version":"nightly-20261023"`) {
		t.Fatalf("staged the wrong file: %q", inst.stagedBin)
	}
	if !gh.requested("objects.githubusercontent.com") {
		t.Fatal("download did not follow the githubusercontent redirect")
	}
	if !strings.Contains(log.String(), "Checksum verified") {
		t.Fatalf("log lacks checksum verification:\n%s", log.String())
	}
}

func TestBinaryUpdate_PreflightFailureMakesNoRequest(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
	gh.latest = `{"channels":{"nightly":{"version":"nightly-20261023"}}}`

	for _, target := range []string{"nightly-20261023", "latest:nightly"} {
		inst := NewUnsupportedInstaller("binary not in /opt/scion", MigrateLayoutHint)
		e := newTestExecutor(gh, inst, "nightly-20261022")
		err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": target})
		if !errors.Is(err, ErrInstallerUnsupported) {
			t.Fatalf("%s: err = %v, want unsupported", target, err)
		}
	}
	if n := gh.requestCount(); n != 0 {
		t.Fatalf("%d HTTP requests made despite failed preflight: %v", n, gh.requests)
	}
}

func TestBinaryUpdate_HookFailureSkipsRestart(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
	inst := &fakeInstaller{}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error { return errors.New("db down") })
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range inst.calls {
		if c == "restart" {
			t.Fatal("restarted although the hook failed")
		}
	}
}

func TestBinaryUpdate_RefusedDownloads(t *testing.T) {
	requireLinux(t)
	const tag = "nightly-20261023"
	asset := BinaryAssetName()

	tests := []struct {
		name    string
		setup   func(gh *fakeGitHub)
		wantErr string
	}{
		{
			name: "asset on a foreign host",
			setup: func(gh *fakeGitHub) {
				gh.setRelease(tag, "2026-10-23T04:00:00Z",
					"https://evil.example.com"+downloadPath(tag, asset),
					"https://github.com"+downloadPath(tag, ChecksumsAssetName))
			},
			wantErr: "is not under https://github.com/",
		},
		{
			name: "asset under another tag",
			setup: func(gh *fakeGitHub) {
				gh.addRelease("nightly-20261001", "2026-10-01T04:00:00Z", tag)
				gh.setRelease(tag, "2026-10-23T04:00:00Z",
					"https://github.com"+downloadPath("nightly-20261001", asset),
					"https://github.com"+downloadPath(tag, ChecksumsAssetName))
			},
			wantErr: "is not under",
		},
		{
			name:    "redirect to a non-githubusercontent host",
			setup:   func(gh *fakeGitHub) { gh.redirectHost = "evil.example.com" },
			wantErr: "refusing redirect",
		},
		{
			name: "empty-file hash for the asset",
			setup: func(gh *fakeGitHub) {
				gh.files[downloadPath(tag, ChecksumsAssetName)] = []byte(emptySHA256 + "  " + asset + "\n")
			},
			wantErr: "empty-file hash",
		},
		{
			name: "checksum mismatch",
			setup: func(gh *fakeGitHub) {
				gh.files[downloadPath(tag, ChecksumsAssetName)] = []byte(strings.Repeat("ab", 32) + "  " + asset + "\n")
			},
			wantErr: "checksum mismatch",
		},
		{
			name: "no SHA256SUMS asset",
			setup: func(gh *fakeGitHub) {
				gh.releases[tag] = fmt.Sprintf(`{"tag_name":%q,"published_at":"2026-10-23T04:00:00Z","assets":[{"name":%q,"browser_download_url":%q}]}`,
					tag, asset, "https://github.com"+downloadPath(tag, asset))
			},
			wantErr: "no SHA256SUMS asset",
		},
		{
			name: "draft release",
			setup: func(gh *fakeGitHub) {
				gh.releases[tag] = strings.Replace(gh.releases[tag], `"draft":false`, `"draft":true`, 1)
			},
			wantErr: "not a published release",
		},
		{
			name: "binary reports another version",
			setup: func(gh *fakeGitHub) {
				gh.addRelease(tag, "2026-10-23T04:00:00Z", "nightly-20261022")
			},
			wantErr: "version mismatch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			gh.addRelease(tag, "2026-10-23T04:00:00Z", tag)
			tt.setup(gh)
			inst := &fakeInstaller{}
			e := newTestExecutor(gh, inst, "nightly-20261022")
			err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": tag})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if len(inst.calls) != 1 || inst.calls[0] != "preflight" {
				t.Fatalf("installer calls = %v, want only preflight", inst.calls)
			}
			if gh.requested("evil.example.com") {
				t.Fatal("request reached evil.example.com")
			}
		})
	}
}

func TestBinaryUpdate_SizeCap(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	tarball := gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
	inst := &fakeInstaller{}
	client := &http.Client{Transport: gh}
	e := NewBinaryUpdateExecutor(BinaryUpdateOptions{
		GitHubRepo: testRepo, Installer: inst, HTTPClient: client, CurrentVersion: "nightly-20261022",
		Releases:         &GitHubReleases{Repo: testRepo, Client: client},
		MaxDownloadBytes: int64(len(tarball) - 1),
	})
	err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"})
	if !errors.Is(err, ErrDownloadTooLarge) {
		t.Fatalf("err = %v, want ErrDownloadTooLarge", err)
	}
	if len(inst.calls) != 1 {
		t.Fatalf("installer calls = %v", inst.calls)
	}
}

func TestBinaryUpdate_LatestChannel(t *testing.T) {
	requireLinux(t)

	t.Run("resolves through LATEST.json", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.latest = `{"channels":{"nightly":{"version":"nightly-20261023"}}}`
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		inst := &fakeInstaller{}
		e := newTestExecutor(gh, inst, "nightly-20261022")
		if err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "latest:nightly"}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if inst.calls[len(inst.calls)-1] != "restart" {
			t.Fatalf("calls = %v", inst.calls)
		}
	})

	t.Run("already on latest", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.latest = `{"channels":{"nightly":{"version":"nightly-20261023"}}}`
		e := newTestExecutor(gh, &fakeInstaller{}, "nightly-20261023")
		err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "latest:nightly"})
		wantCode(t, err, CodeAlreadyRunning)
		if gh.requested("api.github.com") {
			t.Fatal("release looked up although already on latest")
		}
	})

	t.Run("manifest lists a bad tag", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.latest = `{"channels":{"nightly":{"version":"v1.0.0"}}}`
		e := newTestExecutor(gh, &fakeInstaller{}, "nightly-20261022")
		err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "latest:nightly"})
		if err == nil || !strings.Contains(err.Error(), "not a nightly release tag") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestBinaryUpdate_CrossChannelOrder(t *testing.T) {
	requireLinux(t)

	t.Run("switch to an older preview is refused before download", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("v0.3.0-preview.4", "2026-10-22T18:00:00Z", "v0.3.0-preview.4")
		inst := &fakeInstaller{}
		e := newTestExecutor(gh, inst, "nightly-20261023")
		err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "v0.3.0-preview.4"})
		wantCode(t, err, CodeDowngradeNotYetSupported)
		if gh.requested("/releases/download/") {
			t.Fatal("downloaded a refused target")
		}
	})

	t.Run("switch to a newer preview uses publish dates", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("v0.3.0-preview.4", "2026-10-22T18:00:00Z", "v0.3.0-preview.4")
		inst := &fakeInstaller{}
		e := newTestExecutor(gh, inst, "nightly-20261020")
		if err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "v0.3.0-preview.4"}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})

	t.Run("preview to nightly orders by the running release's publish date", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("v0.3.0-preview.4", "2026-10-22T18:00:00Z", "v0.3.0-preview.4")
		gh.addRelease("nightly-20261022", "2026-10-22T04:00:00Z", "nightly-20261022")
		e := newTestExecutor(gh, &fakeInstaller{}, "v0.3.0-preview.4")
		err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261022"})
		wantCode(t, err, CodeDowngradeNotYetSupported)
	})

	t.Run("git build without a build time is refused", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		e := newTestExecutor(gh, &fakeInstaller{}, "")
		e.current, e.buildTime = "", time.Time{}
		err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"})
		wantCode(t, err, CodeDowngradeNotYetSupported)
	})
}

// TestBinaryUpdate_SymlinkEndToEnd runs the executor with the real symlink
// installer on a temporary /opt/scion layout.
func TestBinaryUpdate_SymlinkEndToEnd(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")

	root := installLayout(t, "nightly-20261022")
	opts := symlinkOpts(root)
	signalled := false
	opts.Signal = func() error { signalled = true; return nil }
	inst := Detect(context.Background(), opts)
	if inst.Name() != StrategySymlink {
		t.Fatalf("Detect = %s (%s)", inst.Name(), inst.Status().Reason)
	}

	e := newTestExecutor(gh, inst, "nightly-20261022")
	var hookSawCurrent string
	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error {
			hookSawCurrent = readLink(t, root, currentLinkName)
			if signalled {
				t.Error("signalled before the hook")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hookSawCurrent != "releases/nightly-20261023" {
		t.Fatalf("current at hook time = %q", hookSawCurrent)
	}
	if prev := readLink(t, root, prevLinkName); prev != "releases/nightly-20261022" {
		t.Fatalf("previous = %q", prev)
	}
	if !signalled {
		t.Fatal("hub was not signalled")
	}
	out, err := os.ReadFile(filepath.Join(root, currentLinkName, binaryName))
	if err != nil || !strings.Contains(string(out), "nightly-20261023") {
		t.Fatalf("current/scion = %q, %v", out, err)
	}
}

func TestCheckAllowedParams(t *testing.T) {
	if err := CheckAllowedParams(map[string]string{"branch": "main"}, []string{"branch"}); err != nil {
		t.Fatal(err)
	}
	err := CheckAllowedParams(map[string]string{"branch": "x", "z": "", "a": ""}, []string{"branch"})
	wantCode(t, err, CodeUnknownParam)
	if !strings.Contains(err.Error(), "a, z") {
		t.Fatalf("message = %q, want sorted unknown names", err)
	}
}

func TestBinaryUpdate_HookFailureRollsBackSymlink(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")

	root := installLayout(t, "nightly-20261022", "nightly-20261021")
	if err := os.Symlink("releases/nightly-20261021", filepath.Join(root, prevLinkName)); err != nil {
		t.Fatal(err)
	}
	opts := symlinkOpts(root)
	signals := 0
	opts.Signal = func() error { signals++; return nil }
	inst := Detect(context.Background(), opts)
	if inst.Name() != StrategySymlink {
		t.Fatalf("Detect = %s (%s)", inst.Name(), inst.Status().Reason)
	}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	params := map[string]string{"target_version": "nightly-20261023"}

	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, params,
		func(context.Context, PendingRestart) error { return errors.New("db down") })
	if err == nil || !strings.Contains(err.Error(), "rolled back, nothing changed") {
		t.Fatalf("err = %v, want rollback", err)
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
		t.Fatalf("current = %q after rollback", cur)
	}
	if prev := readLink(t, root, prevLinkName); prev != "releases/nightly-20261021" {
		t.Fatalf("previous = %q after rollback", prev)
	}
	if signals != 0 {
		t.Fatal("signalled although the hook failed")
	}

	// A retry of the same target now goes through.
	if err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, params,
		func(context.Context, PendingRestart) error { return nil }); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261023" {
		t.Fatalf("current after retry = %q", cur)
	}
	if signals != 1 {
		t.Fatalf("signals = %d, want 1", signals)
	}
}

func TestBinaryUpdate_HookFailureSudoInstallSaysNoRollback(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
	opts, r := sudoOpts(t, ruleInstall, ruleNoBlock)
	inst := Detect(context.Background(), opts)
	if inst.Name() != StrategySudoInstall {
		t.Fatalf("Detect = %s", inst.Name())
	}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error { return errors.New("db down") })
	if err == nil || !strings.Contains(err.Error(), "cannot be rolled back") || !strings.Contains(err.Error(), "next restart") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range r.runs {
		if strings.Contains(c, "systemctl restart") && !strings.Contains(c, " -l ") {
			t.Fatalf("restarted although the hook failed: %q", c)
		}
	}
}

func TestBinaryUpdate_StepDeadlines(t *testing.T) {
	requireLinux(t)

	t.Run("hung sudo install fails the run", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		opts, r := sudoOpts(t, ruleInstall, ruleNoBlock)
		inst := Detect(context.Background(), opts)
		r.block = map[string]bool{
			"sudo -n install -m 755 " + filepath.Join(opts.RepoPath, SudoStagingName) + " /usr/local/bin/scion": true,
		}
		e := newTestExecutor(gh, inst, "nightly-20261022")
		e.installTimeout = 50 * time.Millisecond
		hooked := false
		err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
			func(context.Context, PendingRestart) error { hooked = true; return nil })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want deadline exceeded", err)
		}
		if hooked {
			t.Fatal("hook ran after a failed install")
		}
	})

	t.Run("hung preflight fails the run without network", func(t *testing.T) {
		gh := newFakeGitHub(t)
		opts, r := sudoOpts(t, ruleInstall, ruleNoBlock)
		inst := Detect(context.Background(), opts)
		r.block = map[string]bool{"sudo -n -l " + strings.ReplaceAll(ruleInstall, "$REPO", opts.RepoPath): true}
		e := newTestExecutor(gh, inst, "nightly-20261022")
		e.preflightTimeout = 50 * time.Millisecond
		err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"})
		if err == nil || !strings.Contains(err.Error(), "preflight failed") {
			t.Fatalf("err = %v", err)
		}
		if gh.requestCount() != 0 {
			t.Fatal("network used after a failed preflight")
		}
	})

	t.Run("hook gets a bounded context", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		inst := &fakeInstaller{}
		e := newTestExecutor(gh, inst, "nightly-20261022")
		e.hookTimeout = 50 * time.Millisecond
		err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
			func(ctx context.Context, _ PendingRestart) error { <-ctx.Done(); return ctx.Err() })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		for _, c := range inst.calls {
			if c == "restart" {
				t.Fatal("restarted after the hook timed out")
			}
		}
	})
}

func TestBinaryUpdate_RestartFailureRollsBackSymlink(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")

	root := installLayout(t, "nightly-20261022", "nightly-20261021")
	if err := os.Symlink("releases/nightly-20261021", filepath.Join(root, prevLinkName)); err != nil {
		t.Fatal(err)
	}
	opts := symlinkOpts(root)
	opts.Signal = func() error { return errors.New("kill: operation not permitted") }
	inst := Detect(context.Background(), opts)
	if inst.Name() != StrategySymlink {
		t.Fatalf("Detect = %s (%s)", inst.Name(), inst.Status().Reason)
	}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	hooked := false
	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error { hooked = true; return nil })
	if !hooked {
		t.Fatal("hook not called before the restart")
	}
	if err == nil || !strings.Contains(err.Error(), "restarting the hub failed") || !strings.Contains(err.Error(), "rolled back, nothing changed") {
		t.Fatalf("err = %v", err)
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
		t.Fatalf("current = %q after rollback", cur)
	}
	if prev := readLink(t, root, prevLinkName); prev != "releases/nightly-20261021" {
		t.Fatalf("previous = %q after rollback", prev)
	}
}

func TestBinaryUpdate_RestartFailureSudoInstallSaysNoRollback(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
	opts, r := sudoOpts(t, ruleInstall, ruleNoBlock)
	inst := Detect(context.Background(), opts)
	r.failRun = map[string]error{ruleNoBlockTk: errors.New("exit status 1")}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "restarting the hub failed") || !strings.Contains(err.Error(), "cannot be rolled back") {
		t.Fatalf("err = %v", err)
	}
}
