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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRepo = "GoogleCloudPlatform/scion"

// fakeGitHub serves the GitHub API (single-tag lookups and the release list
// the catalog reads) and release downloads (github.com redirecting to
// objects.githubusercontent.com), through an
// http.RoundTripper, so requests keep their real https URLs and the
// executor's URL pinning and redirect policy are exercised unchanged.
type fakeGitHub struct {
	t *testing.T

	mu       sync.Mutex
	requests []string

	releases map[string]string // tag → release JSON
	files    map[string][]byte // github.com download path → body
	// unlisted tags are served by single-tag lookups but left out of the
	// release list (an old release past the list's pages).
	unlisted map[string]bool
	// listStatus, when set, is the status of every release list request.
	listStatus int
	// redirectHost is where github.com download requests are redirected.
	redirectHost string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	return &fakeGitHub{
		t:            t,
		releases:     map[string]string{},
		files:        map[string][]byte{},
		unlisted:     map[string]bool{},
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
	case r.URL.Host == "api.github.com" && r.URL.Path == "/repos/"+testRepo+"/releases":
		// One page with every listed release; the catalog orders them itself.
		if f.listStatus != 0 {
			http.Error(w, "list failed", f.listStatus)
			return
		}
		tags := make([]string, 0, len(f.releases))
		for tag := range f.releases {
			if !f.unlisted[tag] {
				tags = append(tags, tag)
			}
		}
		sort.Strings(tags)
		bodies := make([]string, 0, len(tags))
		for _, tag := range tags {
			bodies = append(bodies, f.releases[tag])
		}
		_, _ = w.Write([]byte("[" + strings.Join(bodies, ",") + "]"))
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
	e := NewBinaryUpdateExecutor(BinaryUpdateOptions{CurrentVersion: "nightly-20261022", Snapshotter: &fakeDBSnapshotter{}})
	tests := []struct {
		name   string
		params map[string]string
		code   string // "" = valid
	}{
		{"tag", map[string]string{"target_version": "nightly-20261023"}, ""},
		{"tag with channel and pin", map[string]string{"target_version": "nightly-20261023", "channel": "nightly", "pin": "latest"}, ""},
		{"cross-channel tag (order checked at run time)", map[string]string{"target_version": "v0.3.0-preview.4"}, ""},
		{"latest in current channel", map[string]string{"target_version": "latest:nightly"}, ""},
		{"latest in another channel", map[string]string{"target_version": "latest:preview", "channel": "preview"}, ""},
		{"latest pinned to latest", map[string]string{"target_version": "latest:stable", "pin": "latest"}, ""},
		{"tag pinned to itself", map[string]string{"target_version": "nightly-20261023", "pin": "nightly-20261023"}, ""},
		{"download_url", map[string]string{"target_version": "nightly-20261023", "download_url": "https://evil/x"}, CodeUnknownParam},
		{"checksums_url", map[string]string{"target_version": "nightly-20261023", "checksums_url": "x"}, CodeUnknownParam},
		{"current_version", map[string]string{"target_version": "nightly-20261023", "current_version": "dev"}, CodeUnknownParam},
		{"confirm_downgrade on an upgrade is ignored", map[string]string{"target_version": "nightly-20261023", "confirm_downgrade": "nightly-20261023"}, ""},
		{"confirm_downgrade not a tag", map[string]string{"target_version": "nightly-20261023", "confirm_downgrade": "yes"}, CodeInvalidParam},
		{"dryRun", map[string]string{"target_version": "nightly-20261023", "dryRun": "true"}, CodeUnknownParam},
		{"missing target", map[string]string{}, CodeInvalidParam},
		{"not a tag", map[string]string{"target_version": "latest"}, CodeInvalidParam},
		{"path in tag", map[string]string{"target_version": "nightly-../../x"}, CodeInvalidParam},
		{"channel mismatch", map[string]string{"target_version": "nightly-20261023", "channel": "preview"}, CodeInvalidParam},
		{"latest pinned to a tag", map[string]string{"target_version": "latest:nightly", "pin": "nightly-20261023"}, CodeInvalidParam},
		{"tag pinned to another tag", map[string]string{"target_version": "nightly-20261023", "pin": "nightly-20261024"}, CodeInvalidParam},
		{"tag pinned to garbage", map[string]string{"target_version": "nightly-20261023", "pin": "newest"}, CodeInvalidParam},
		{"latest unknown channel", map[string]string{"target_version": "latest:beta"}, CodeInvalidParam},
		{"latest channel mismatch", map[string]string{"target_version": "latest:nightly", "channel": "preview"}, CodeInvalidParam},
		{"same", map[string]string{"target_version": "nightly-20261022"}, CodeAlreadyRunning},
		{"older", map[string]string{"target_version": "nightly-20261021"}, CodeDowngradeRequiresConfirmation},
		{"older, confirmed", map[string]string{"target_version": "nightly-20261021", "confirm_downgrade": "nightly-20261021"}, ""},
		{"older, confirmation names another tag", map[string]string{"target_version": "nightly-20261021", "confirm_downgrade": "nightly-20261020"}, CodeDowngradeRequiresConfirmation},
		{"below the floor", map[string]string{"target_version": "nightly-20261008"}, CodeBelowDowngradeFloor},
		{"below the floor, confirmed", map[string]string{"target_version": "nightly-20261008", "confirm_downgrade": "nightly-20261008"}, CodeBelowDowngradeFloor},
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

	t.Run("status codes", func(t *testing.T) {
		if (&RequestError{Code: CodeUnknownParam}).HTTPStatus() != http.StatusBadRequest ||
			(&RequestError{Code: CodeInvalidParam}).HTTPStatus() != http.StatusBadRequest ||
			(&RequestError{Code: CodeAlreadyRunning}).HTTPStatus() != http.StatusConflict ||
			(&RequestError{Code: CodeDowngradeRequiresConfirmation}).HTTPStatus() != http.StatusConflict ||
			(&RequestError{Code: CodeBelowDowngradeFloor}).HTTPStatus() != http.StatusConflict ||
			(&RequestError{Code: CodeDowngradeUnsupportedDriver}).HTTPStatus() != http.StatusConflict {
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
	if hooked == nil || *hooked != (PendingRestart{From: "nightly-20261022", To: "nightly-20261023", Strategy: "fake",
		Channel: "nightly", Pin: PinLatest}) {
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

	run := func(e *BinaryUpdateExecutor, params map[string]string) (*PendingRestart, error) {
		var hooked *PendingRestart
		err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, params,
			func(_ context.Context, p PendingRestart) error {
				hooked = &p
				return nil
			})
		return hooked, err
	}

	t.Run("resolves through the release catalog", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261022", "2026-10-22T04:00:00Z", "nightly-20261022")
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		inst := &fakeInstaller{}
		e := newTestExecutor(gh, inst, "nightly-20261021")
		hooked, err := run(e, map[string]string{"target_version": "latest:nightly"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if inst.calls[len(inst.calls)-1] != "restart" || hooked == nil || hooked.To != "nightly-20261023" {
			t.Fatalf("calls = %v, hook = %+v", inst.calls, hooked)
		}
		if hooked.Channel != "nightly" || hooked.Pin != PinLatest {
			t.Fatalf("hook channel/pin = %q/%q", hooked.Channel, hooked.Pin)
		}
		if !gh.requested("/repos/" + testRepo + "/releases?") {
			t.Fatal("latest was not resolved through the release list")
		}
	})

	t.Run("any channel, not only the running one", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261020", "2026-10-20T04:00:00Z", "nightly-20261020")
		gh.addRelease("v0.3.0-preview.4", "2026-10-22T18:00:00Z", "v0.3.0-preview.4")
		inst := &fakeInstaller{}
		e := newTestExecutor(gh, inst, "nightly-20261020")
		hooked, err := run(e, map[string]string{"target_version": "latest:preview", "channel": "preview"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if hooked == nil || hooked.To != "v0.3.0-preview.4" || hooked.Channel != "preview" {
			t.Fatalf("hook = %+v", hooked)
		}
	})

	t.Run("skips a newer release that is not installable", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		// nightly-20261024 has no SHA256SUMS line for the tarball.
		gh.addRelease("nightly-20261024", "2026-10-24T04:00:00Z", "nightly-20261024")
		gh.files[downloadPath("nightly-20261024", ChecksumsAssetName)] = []byte(emptySHA256 + "  SHA256SUMS\n")
		e := newTestExecutor(gh, &fakeInstaller{}, "nightly-20261022")
		hooked, err := run(e, map[string]string{"target_version": "latest:nightly"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if hooked == nil || hooked.To != "nightly-20261023" {
			t.Fatalf("hook = %+v, want nightly-20261023", hooked)
		}
	})

	t.Run("latest of another channel that is older needs confirmation before download", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		gh.addRelease("v0.3.0-preview.4", "2026-10-22T18:00:00Z", "v0.3.0-preview.4")
		inst := &fakeInstaller{}
		e := newTestExecutor(gh, inst, "nightly-20261023")
		e.snapshots = &fakeDBSnapshotter{}
		hooked, err := run(e, map[string]string{"target_version": "latest:preview", "channel": "preview"})
		wantCode(t, err, CodeDowngradeRequiresConfirmation)
		if hooked != nil || len(inst.calls) != 1 {
			t.Fatalf("installer calls = %v, hook = %+v; want preflight only", inst.calls, hooked)
		}
		if gh.requested("/releases/download/v0.3.0-preview.4/scion-") {
			t.Fatal("the older release was downloaded")
		}
	})

	t.Run("already on latest", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		e := newTestExecutor(gh, &fakeInstaller{}, "nightly-20261023")
		_, err := run(e, map[string]string{"target_version": "latest:nightly"})
		wantCode(t, err, CodeAlreadyRunning)
		if gh.requested("/releases/tags/") || gh.requested("/releases/download/nightly-20261023/scion-") {
			t.Fatal("release looked up or downloaded although already on latest")
		}
	})

	t.Run("no installable release in the channel", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		inst := &fakeInstaller{}
		e := newTestExecutor(gh, inst, "nightly-20261022")
		_, err := run(e, map[string]string{"target_version": "latest:stable"})
		if err == nil || !strings.Contains(err.Error(), "no installable stable release") {
			t.Fatalf("err = %v", err)
		}
		if len(inst.calls) != 1 {
			t.Fatalf("installer calls = %v, want preflight only", inst.calls)
		}
	})

	t.Run("pin is passed to the hook", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		e := newTestExecutor(gh, &fakeInstaller{}, "nightly-20261022")
		hooked, err := run(e, map[string]string{"target_version": "nightly-20261023", "pin": "nightly-20261023"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if hooked == nil || hooked.Pin != "nightly-20261023" {
			t.Fatalf("hook = %+v", hooked)
		}
	})
}

func TestBinaryUpdate_CrossChannelOrder(t *testing.T) {
	requireLinux(t)

	t.Run("switch to an older preview needs confirmation before download", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("v0.3.0-preview.4", "2026-10-22T18:00:00Z", "v0.3.0-preview.4")
		inst := &fakeInstaller{}
		e := newTestExecutor(gh, inst, "nightly-20261023")
		e.snapshots = &fakeDBSnapshotter{}
		err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "v0.3.0-preview.4"})
		wantCode(t, err, CodeDowngradeRequiresConfirmation)
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
		e.snapshots = &fakeDBSnapshotter{}
		err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261022"})
		wantCode(t, err, CodeDowngradeRequiresConfirmation)
	})

	t.Run("git build without a build time needs confirmation", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		e := newTestExecutor(gh, &fakeInstaller{}, "")
		e.current, e.buildTime = "", time.Time{}
		e.snapshots = &fakeDBSnapshotter{}
		err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"})
		wantCode(t, err, CodeDowngradeRequiresConfirmation)
		if re, _ := IsRequestError(err); re.Details["class"] != "switch-older" {
			t.Fatalf("details = %v, want class switch-older for an undecidable order", re.Details)
		}
	})
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

// ctxAwareDeactivator is a fakeInstaller that can roll back, and whose
// Deactivate fails like a real ctx-honouring rollback (a sudo command, a
// store write) would when its context is already done.
type ctxAwareDeactivator struct {
	fakeInstaller
	deactivated bool
	sawDeadline time.Duration // time left on the rollback ctx
	sawValue    any
	restartErr  error
}

type testCtxKey struct{}

func (f *ctxAwareDeactivator) Restart(context.Context) error {
	f.calls = append(f.calls, "restart")
	return f.restartErr
}

func (f *ctxAwareDeactivator) Deactivate(ctx context.Context) error {
	f.calls = append(f.calls, "deactivate")
	if err := ctx.Err(); err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		f.sawDeadline = time.Until(dl)
	}
	f.sawValue = ctx.Value(testCtxKey{})
	f.deactivated = true
	return nil
}

func TestBinaryUpdate_RollbackRunsWhenRunCtxIsCancelled(t *testing.T) {
	requireLinux(t)

	t.Run("undoActivation with an already-cancelled parent", func(t *testing.T) {
		inst := &ctxAwareDeactivator{}
		e := newTestExecutor(newFakeGitHub(t), inst, "nightly-20261022")
		parent, cancel := context.WithCancel(context.WithValue(context.Background(), testCtxKey{}, "trace-1"))
		cancel()
		err := e.undoActivation(parent, &bytes.Buffer{}, "nightly-20261023", errors.New("hook failed"))
		if err == nil || !strings.Contains(err.Error(), "rolled back, nothing changed") {
			t.Fatalf("err = %v, want a completed rollback", err)
		}
		if !inst.deactivated {
			t.Fatal("rollback did not run on a cancelled parent ctx")
		}
		if inst.sawDeadline <= 0 || inst.sawDeadline > e.installTimeout {
			t.Fatalf("rollback ctx deadline in %v, want bounded by installTimeout %v", inst.sawDeadline, e.installTimeout)
		}
		if inst.sawValue != "trace-1" {
			t.Fatalf("rollback ctx lost the parent's values: %v", inst.sawValue)
		}
	})

	t.Run("hook fails after the run ctx is cancelled", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		inst := &ctxAwareDeactivator{}
		e := newTestExecutor(gh, inst, "nightly-20261022")
		runCtx, cancelRun := context.WithCancel(context.Background())
		defer cancelRun()
		err := e.RunWithBeforeRestart(runCtx, &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
			func(ctx context.Context, _ PendingRestart) error {
				cancelRun() // the run is cancelled (shutdown, run deadline) during the hook
				return ctx.Err()
			})
		if err == nil || !strings.Contains(err.Error(), "rolled back, nothing changed") || !inst.deactivated {
			t.Fatalf("err = %v, deactivated = %v; want the rollback to complete", err, inst.deactivated)
		}
		for _, c := range inst.calls {
			if c == "restart" {
				t.Fatal("restarted after the hook failed")
			}
		}
	})

	t.Run("restart fails with the run ctx past its deadline", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
		inst := &ctxAwareDeactivator{}
		e := newTestExecutor(gh, inst, "nightly-20261022")
		runCtx, cancelRun := context.WithCancel(context.Background())
		defer cancelRun()
		inst.restartErr = errors.New("restart timed out")
		err := e.RunWithBeforeRestart(runCtx, &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
			func(context.Context, PendingRestart) error {
				cancelRun() // expires before Restart runs
				return nil
			})
		if err == nil || !strings.Contains(err.Error(), "rolled back, nothing changed") || !inst.deactivated {
			t.Fatalf("err = %v, deactivated = %v; want the rollback to complete", err, inst.deactivated)
		}
	})
}
