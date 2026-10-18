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

package unitdrift

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupRepo writes the sample template into a fake checkout and the given
// installed unit next to it, returning the repo path and a locator for it.
func setupRepo(t *testing.T, template, installed string) (string, Locator) {
	t.Helper()
	repo := t.TempDir()
	if template != "" {
		p := filepath.Join(repo, TemplatePath)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(template), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unit := filepath.Join(t.TempDir(), "scion-hub.service")
	if err := os.WriteFile(unit, []byte(installed), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, func(context.Context, string) (UnitInfo, error) {
		return UnitInfo{FragmentPath: unit}, nil
	}
}

func fixedRevision(context.Context, string) (string, error) { return "abc1234", nil }

const wantDriftReport = `WARNING: installed unit differs from template: re-provision needed
The systemd unit for scion-hub was installed from an older template. Rebuild from git updates the binary only; it does not change the unit.
To apply the template from commit abc1234 (branch main):
  1. On the workstation and checkout used to provision this hub (not on the hub VM), check out that commit.
  2. Run scripts/starter-hub/gce-start-hub.sh --full with the same flags and environment used at provisioning (for example --no-tls, HUB_BASE_URL and any hub-config.sh overrides). Running it without the original flags can reconfigure the hub (for example enable TLS on a --no-tls hub).
  3. Note that --full also re-applies settings.yaml, hub.env (if present on the workstation), the Caddyfile and the sudoers rules from that workstation, then pulls, builds and restarts the hub.
Drop-ins under scion-hub.service.d can still override template keys after re-provisioning.
Differences (values of Environment and credential-like keys elided):
  installed: ExecStartPre=/bin/true
`

func TestCheck_DriftReportIsExact(t *testing.T) {
	inst := strings.Replace(defaultRender(), "Restart=always", "ExecStartPre=/bin/true\nRestart=always", 1)
	repo, loc := setupRepo(t, sampleTemplate, inst)
	got := Check(context.Background(), CheckOptions{
		RepoPath: repo, Branch: "main", Service: "scion-hub", Locate: loc, Revision: fixedRevision,
	})
	if got != wantDriftReport {
		t.Fatalf("report:\n%s\nwant:\n%s", got, wantDriftReport)
	}
}

func TestCheck_DriftWithoutRevision(t *testing.T) {
	inst := strings.Replace(defaultRender(), "Restart=always", "Restart=no", 1)
	repo, loc := setupRepo(t, sampleTemplate, inst)
	got := Check(context.Background(), CheckOptions{
		RepoPath: repo, Service: "scion-hub", Locate: loc,
		Revision: func(context.Context, string) (string, error) { return "", errors.New("no git") },
	})
	if !strings.Contains(got, "To apply the template from the checked-out commit:\n") {
		t.Fatalf("report without a commit should name the checked-out commit:\n%s", got)
	}
}

func TestCheck_Match(t *testing.T) {
	repo, loc := setupRepo(t, sampleTemplate, defaultRender())
	got := Check(context.Background(), CheckOptions{RepoPath: repo, Service: "scion-hub", Locate: loc})
	if got != "Systemd unit matches template.\n" {
		t.Fatalf("report = %q", got)
	}
}

func TestCheck_NotesForDropInsAndReload(t *testing.T) {
	repo, loc := setupRepo(t, sampleTemplate, defaultRender())
	withNotes := func(ctx context.Context, s string) (UnitInfo, error) {
		info, err := loc(ctx, s)
		info.DropInPaths = []string{"/etc/systemd/system/scion-hub.service.d/10-shared.conf"}
		info.NeedDaemonReload = true
		return info, err
	}
	got := Check(context.Background(), CheckOptions{RepoPath: repo, Service: "scion-hub", Locate: withNotes})
	if !strings.HasPrefix(got, "Systemd unit matches template.\n") {
		t.Fatalf("drop-ins must not cause drift:\n%s", got)
	}
	if !strings.Contains(got, "10-shared.conf") || !strings.Contains(got, "daemon-reload needed") {
		t.Fatalf("missing notes:\n%s", got)
	}
}

func TestCheck_SkippedCases(t *testing.T) {
	cases := map[string]func(t *testing.T) CheckOptions{
		"no template": func(t *testing.T) CheckOptions {
			repo, loc := setupRepo(t, "", defaultRender())
			return CheckOptions{RepoPath: repo, Locate: loc}
		},
		"locator error": func(t *testing.T) CheckOptions {
			repo, _ := setupRepo(t, sampleTemplate, defaultRender())
			return CheckOptions{RepoPath: repo, Locate: func(context.Context, string) (UnitInfo, error) {
				return UnitInfo{}, errors.New("no systemd")
			}}
		},
		"no fragment": func(t *testing.T) CheckOptions {
			repo, _ := setupRepo(t, sampleTemplate, defaultRender())
			return CheckOptions{RepoPath: repo, Locate: func(context.Context, string) (UnitInfo, error) {
				return UnitInfo{}, nil
			}}
		},
		"unreadable fragment": func(t *testing.T) CheckOptions {
			repo, _ := setupRepo(t, sampleTemplate, defaultRender())
			return CheckOptions{RepoPath: repo, Locate: func(context.Context, string) (UnitInfo, error) {
				return UnitInfo{FragmentPath: filepath.Join(t.TempDir(), "missing.service")}, nil
			}}
		},
		"nil locator": func(t *testing.T) CheckOptions {
			repo, _ := setupRepo(t, sampleTemplate, defaultRender())
			return CheckOptions{RepoPath: repo}
		},
		"other unit": func(t *testing.T) CheckOptions {
			repo, loc := setupRepo(t, sampleTemplate, "[Unit]\nDescription=Something else\n")
			return CheckOptions{RepoPath: repo, Locate: loc}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			opts := mk(t)
			opts.Service = "scion-hub"
			got := Check(context.Background(), opts)
			if !strings.HasPrefix(got, "Systemd unit check skipped: ") || strings.Contains(got, "WARNING") {
				t.Fatalf("report = %q, want a skipped line", got)
			}
		})
	}
}

func TestCheck_BlockingLocatorTimesOut(t *testing.T) {
	repo, _ := setupRepo(t, sampleTemplate, defaultRender())
	// The locator ignores ctx on purpose and only returns after 3 s, so
	// Check must give up at its own 50 ms deadline.
	release := make(chan struct{})
	time.AfterFunc(3*time.Second, func() { close(release) })
	start := time.Now()
	got := Check(context.Background(), CheckOptions{
		RepoPath: repo, Service: "scion-hub", Timeout: 50 * time.Millisecond,
		Locate: func(context.Context, string) (UnitInfo, error) {
			<-release
			return UnitInfo{}, nil
		},
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Check did not honour its timeout (took %v)", elapsed)
	}
	if !strings.HasPrefix(got, "Systemd unit check skipped: ") || !strings.Contains(got, "timed out") {
		t.Fatalf("report = %q", got)
	}
}

func TestCheck_PanicsBecomeSkipped(t *testing.T) {
	repo, loc := setupRepo(t, sampleTemplate, strings.Replace(defaultRender(), "Restart=always", "Restart=no", 1))
	for name, opts := range map[string]CheckOptions{
		"locator panics": {RepoPath: repo, Service: "scion-hub", Locate: func(context.Context, string) (UnitInfo, error) {
			panic("boom")
		}},
		"revision panics": {RepoPath: repo, Service: "scion-hub", Locate: loc, Revision: func(context.Context, string) (string, error) {
			panic("boom")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			got := Check(context.Background(), opts)
			if !strings.HasPrefix(got, "Systemd unit check skipped: ") || !strings.Contains(got, "internal error: boom") {
				t.Fatalf("report = %q", got)
			}
		})
	}
}

func TestParseShow(t *testing.T) {
	info := parseShow("FragmentPath=/etc/systemd/system/scion-hub.service\nDropInPaths=/a.conf /b.conf\nNeedDaemonReload=yes\n")
	if info.FragmentPath != "/etc/systemd/system/scion-hub.service" || len(info.DropInPaths) != 2 || !info.NeedDaemonReload {
		t.Fatalf("parseShow = %+v", info)
	}
	info = parseShow("FragmentPath=\nDropInPaths=\nNeedDaemonReload=no\n")
	if info.FragmentPath != "" || len(info.DropInPaths) != 0 || info.NeedDaemonReload {
		t.Fatalf("parseShow empty = %+v", info)
	}
}
