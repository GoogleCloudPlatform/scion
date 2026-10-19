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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot returns the repository root, from this file's location.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find test file location")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func readRepoFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

const goldenPath = "scripts/starter-hub/tests/testdata/scion-hub.service.golden"

const sampleTemplate = `## notes for editors, dropped
[Unit]
Description=Scion Hub API Server
After=network.target

[Service]
WorkingDirectory=@REPO_DIR@
EnvironmentFile=/home/scion/.scion/hub.env
Environment="HOME=/home/scion"
# a comment
Environment="SCION_SERVER_BASE_URL=@HUB_BASE_URL@"
ExecStart=@SCION_BIN@ --global server start --enable-hub --storage-bucket ${SCION_HUB_STORAGE_BUCKET} --auto-provide
Restart=always

[Install]
WantedBy=multi-user.target
`

func render(tmpl, repo, url, bin string) string {
	var out []string
	for _, l := range strings.Split(tmpl, "\n") {
		if strings.HasPrefix(l, "##") {
			continue
		}
		out = append(out, l)
	}
	s := strings.Join(out, "\n")
	s = strings.ReplaceAll(s, "@REPO_DIR@", repo)
	s = strings.ReplaceAll(s, "@HUB_BASE_URL@", url)
	return strings.ReplaceAll(s, "@SCION_BIN@", bin)
}

func defaultRender() string {
	return render(sampleTemplate, "/home/scion/scion", "https://hub.example.test", "/usr/local/bin/scion")
}

func compareStr(tmpl, inst string) Result {
	return Compare([]byte(tmpl), []byte(inst))
}

func wantStatus(t *testing.T, got Result, want Status) {
	t.Helper()
	if got.Status != want {
		t.Fatalf("status = %v, want %v (reason %q, diff %q)", got.Status, want, got.Reason, got.Diff)
	}
}

func TestCompare_IdenticalRenderMatches(t *testing.T) {
	wantStatus(t, compareStr(sampleTemplate, defaultRender()), Match)
}

func TestCompare_PerHostValuesMatch(t *testing.T) {
	inst := render(sampleTemplate, "/srv/other/repo", "http://10.0.0.5:8080", "/opt/bin/scion")
	wantStatus(t, compareStr(sampleTemplate, inst), Match)
}

func TestCompare_FlagAfterPlaceholderIsDrift(t *testing.T) {
	cases := map[string]string{
		"after binary": strings.Replace(defaultRender(), "/usr/local/bin/scion --global", "/usr/local/bin/scion --debug --global", 1),
		"after url":    strings.Replace(defaultRender(), `https://hub.example.test"`, `https://hub.example.test --x"`, 1),
	}
	for name, inst := range cases {
		t.Run(name, func(t *testing.T) {
			res := compareStr(sampleTemplate, inst)
			wantStatus(t, res, Drift)
		})
	}
}

func TestCompare_ChangedLinesAreDrift(t *testing.T) {
	cases := map[string]string{
		"added ExecStartPre": strings.Replace(defaultRender(), "Restart=always", "ExecStartPre=/bin/true\nRestart=always", 1),
		"removed line":       strings.Replace(defaultRender(), "Restart=always\n", "", 1),
		"flag changed":       strings.Replace(defaultRender(), "--auto-provide", "--no-auto-provide", 1),
		"env changed":        strings.Replace(defaultRender(), `HOME=/home/scion"`, `HOME=/root"`, 1),
	}
	for name, inst := range cases {
		t.Run(name, func(t *testing.T) {
			res := compareStr(sampleTemplate, inst)
			wantStatus(t, res, Drift)
			if len(res.Diff) == 0 {
				t.Fatal("drift without diff lines")
			}
		})
	}
}

func TestCompare_AddedLineDiffIsAligned(t *testing.T) {
	inst := strings.Replace(defaultRender(), "Restart=always", "ExecStartPre=/bin/true\nRestart=always", 1)
	res := compareStr(sampleTemplate, inst)
	wantStatus(t, res, Drift)
	if len(res.Diff) != 1 || res.Diff[0] != "installed: ExecStartPre=/bin/true" {
		t.Fatalf("diff = %q, want only the added line", res.Diff)
	}
}

func TestCompare_SamePlaceholderMustCaptureSameValue(t *testing.T) {
	tmpl := sampleTemplate + "Alias=@REPO_DIR@\n"
	same := defaultRender() + "Alias=/home/scion/scion\n"
	wantStatus(t, compareStr(tmpl, same), Match)
	other := defaultRender() + "Alias=/elsewhere\n"
	wantStatus(t, compareStr(tmpl, other), Drift)
}

func TestCompare_CommentsAndWhitespaceIgnored(t *testing.T) {
	inst := strings.ReplaceAll(defaultRender(), "\n", "\r\n")
	inst = strings.Replace(inst, "Restart=always", "   Restart=always  \t", 1)
	inst = strings.Replace(inst, "# a comment", "# a different comment\n; another\n\n", 1)
	wantStatus(t, compareStr(sampleTemplate, inst), Match)
}

func TestCompare_ChangedDescriptionStillReportsDrift(t *testing.T) {
	tmpl := strings.Replace(sampleTemplate, "Description=Scion Hub API Server", "Description=Scion Hub Server", 1)
	res := compareStr(tmpl, defaultRender())
	wantStatus(t, res, Drift)
}

func TestCompare_OtherUnitsAreSkipped(t *testing.T) {
	single := readRepoFile(t, "scripts/single-node-vm/config-templates/scion-hub.service")
	res := Compare([]byte(sampleTemplate), single)
	wantStatus(t, res, Skipped)
	if !strings.Contains(res.Reason, `Description="Scion Hub"`) {
		t.Errorf("reason %q should name the installed Description", res.Reason)
	}
	res = compareStr(sampleTemplate, "[Service]\nExecStart=/bin/true\n")
	wantStatus(t, res, Skipped)
}

func TestCompare_UnusableInputsAreSkipped(t *testing.T) {
	wantStatus(t, compareStr("", defaultRender()), Skipped)
	wantStatus(t, compareStr(sampleTemplate, ""), Skipped)
	wantStatus(t, compareStr("# only a comment\n", defaultRender()), Skipped)
	noDesc := strings.Replace(sampleTemplate, "Description=Scion Hub API Server\n", "", 1)
	wantStatus(t, compareStr(noDesc, defaultRender()), Skipped)
}

func TestCompare_UnknownPlaceholderIsWildcard(t *testing.T) {
	tmpl := strings.Replace(sampleTemplate, "Restart=always", "Restart=@RESTART_POLICY@", 1)
	wantStatus(t, compareStr(tmpl, defaultRender()), Match)
}

func TestCompare_RegexpMetaIsLiteral(t *testing.T) {
	// "${...}", quotes and "." in the template must not act as patterns.
	inst := strings.Replace(defaultRender(), "${SCION_HUB_STORAGE_BUCKET}", "$XSCION_HUB_STORAGE_BUCKET}", 1)
	wantStatus(t, compareStr(sampleTemplate, inst), Drift)
	inst = strings.Replace(defaultRender(), "multi-user.target", "multi-userXtarget", 1)
	wantStatus(t, compareStr(sampleTemplate, inst), Drift)
}

func TestCompare_DiffElidesValues(t *testing.T) {
	inst := strings.Replace(defaultRender(), `Environment="HOME=/home/scion"`,
		"Environment=\"HOME=/home/scion\"\nEnvironment=\"API_TOKEN=s3cr3t-value\"\nEnvironmentFile=/root/private.env\nApiKey=k3y-value\nEnvironment = \"SPACED_TOKEN=sp4ced-value\"", 1)
	res := compareStr(sampleTemplate, inst)
	wantStatus(t, res, Drift)
	joined := strings.Join(res.Diff, "\n")
	for _, secret := range []string{"s3cr3t-value", "/root/private.env", "k3y-value", "sp4ced-value"} {
		if strings.Contains(joined, secret) {
			t.Errorf("diff shows value %q:\n%s", secret, joined)
		}
	}
	for _, want := range []string{`installed: Environment="API_TOKEN=…"`, "installed: EnvironmentFile=…", "installed: ApiKey=…", `installed: Environment="SPACED_TOKEN=…"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("diff missing %q:\n%s", want, joined)
		}
	}
}

func TestCompare_ElidedValueDifferenceIsLabelled(t *testing.T) {
	inst := strings.Replace(defaultRender(), `HOME=/home/scion"`, `HOME=/root"`, 1)
	res := compareStr(sampleTemplate, inst)
	wantStatus(t, res, Drift)
	want := []string{
		`template:  Environment="HOME=…" (value differs)`,
		`installed: Environment="HOME=…" (value differs)`,
	}
	if strings.Join(res.Diff, "\n") != strings.Join(want, "\n") {
		t.Fatalf("diff = %q, want %q", res.Diff, want)
	}
}

func TestCompare_DiffIsCapped(t *testing.T) {
	var extra strings.Builder
	for i := 0; i < 20; i++ {
		extra.WriteString("X-Extra=line\n")
	}
	res := compareStr(sampleTemplate, defaultRender()+extra.String())
	wantStatus(t, res, Drift)
	if len(res.Diff) != maxDiffLines+1 || !strings.Contains(res.Diff[maxDiffLines], "10 more") {
		t.Fatalf("diff not capped: %q", res.Diff)
	}
}

// The committed golden file is what gce-start-hub.sh renders at the test
// harness defaults (checked by scripts/starter-hub/tests/run.sh). It must
// match the real template, tying the shell renderer to this matcher.
func TestCompare_GoldenMatchesRealTemplate(t *testing.T) {
	res := Compare(readRepoFile(t, TemplatePath), readRepoFile(t, goldenPath))
	wantStatus(t, res, Match)
}

// TestTemplateDescriptionIsKnown keeps knownStarterHubDescriptions in step
// with the template: when the template's Description changes, the new value
// must be appended (and the old ones kept) so no older hub is skipped.
func TestTemplateDescriptionIsKnown(t *testing.T) {
	desc, ok := description(normalize(readRepoFile(t, TemplatePath)))
	if !ok {
		t.Fatal("template has no Description= line")
	}
	for _, d := range knownStarterHubDescriptions {
		if d == desc {
			return
		}
	}
	t.Fatalf("template Description %q is not in knownStarterHubDescriptions; append it (keep the old values)", desc)
}

func TestTemplatePlaceholdersAreKnown(t *testing.T) {
	got := placeholderRE.FindAllString(strings.Join(normalize(readRepoFile(t, TemplatePath)), "\n"), -1)
	want := map[string]bool{"@REPO_DIR@": true, "@SCION_BIN@": true, "@HUB_BASE_URL@": true}
	if len(got) != len(want) {
		t.Fatalf("placeholders = %q, want each of %v once (gce-start-hub.sh renders exactly these)", got, want)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("placeholder %s is not rendered by gce-start-hub.sh", p)
		}
	}
}
