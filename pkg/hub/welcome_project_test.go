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

//go:build !no_sqlite

package hub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// Tests for the welcome project (ptone/scion#7534). Every network seam is
// faked: ref resolution (resolveRef) and archive download (fetcher). No test
// reaches the network.

const (
	welcomePreviewPath = "/api/v1/system/welcome/preview"
	welcomeInstallPath = "/api/v1/system/welcome/install"
	welcomeSkipPath    = "/api/v1/system/welcome/skip"

	wpTeamSHA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	wpSkillsSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	wpPlaywright  = "cccccccccccccccccccccccccccccccccccccccc"
	wpTeamSkillAt = "dddddddddddddddddddddddddddddddddddddddd"
)

// wpEntry is one tar entry of a fixture archive. Name is relative to the
// archive's top-level directory unless raw is set.
type wpEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	raw      bool
	size     int64 // when set, overrides len(body) in the header
}

// wpArchive builds a GitHub-style archive: a pax global header carrying
// comment (omitted when comment is ""), then the entries under top/.
func wpArchive(t *testing.T, comment, top string, entries []wpEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if comment != "" {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeXGlobalHeader,
			Name:       "pax_global_header",
			PAXRecords: map[string]string{"comment": comment},
		}))
	}
	require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top + "/", Mode: 0o755}))
	for _, e := range entries {
		name := e.name
		if !e.raw {
			name = top + "/" + e.name
		}
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{Typeflag: tf, Name: name, Mode: 0o644, Linkname: e.linkname, Format: tar.FormatPAX}
		if tf == tar.TypeReg {
			hdr.Size = int64(len(e.body))
			if e.size > 0 {
				hdr.Size = e.size
			}
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if tf == tar.TypeReg && e.body != "" {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// fakeWelcomeFetcher serves fixture archives by URL and records requests.
type fakeWelcomeFetcher struct {
	mu     sync.Mutex
	bodies map[string][]byte
	calls  []string
}

func (f *fakeWelcomeFetcher) FetchBytes(_ context.Context, rawURL string) (*remotefetch.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rawURL)
	b, ok := f.bodies[rawURL]
	if !ok {
		return nil, &remotefetch.Error{Reason: remotefetch.ReasonStatus, Detail: "not found"}
	}
	return &remotefetch.Result{Body: b}, nil
}

func (f *fakeWelcomeFetcher) fetched(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func wpArchiveURL(t *testing.T, owner, repo, sha string) string {
	t.Helper()
	u, err := config.GitHubArchiveURLBySHA(owner, repo, sha)
	require.NoError(t, err)
	return u
}

// welcomeFixture is a workstation hub with the welcome experiment on and
// faked network seams serving a default agent-team source.
type welcomeFixture struct {
	t        *testing.T
	srv      *Server
	store    store.Store
	home     string
	fetch    *fakeWelcomeFetcher
	refs     map[string]string // "owner/repo@ref" -> sha
	refMu    sync.Mutex
	refCalls []string
	creds    bool
	now      time.Time
}

// wpTeamEntries is the default agent-team repository content.
func wpTeamEntries() []wpEntry {
	return []wpEntry{
		{name: ".antigravitycli/config.json", body: "{}"},
		{name: "README.md", body: "agent team"},
		{name: "templates/README.md", body: "templates index"},
		{name: "templates/coordinator/scion-agent.yaml", body: "harness: claude\n" +
			"skills:\n" +
			"  - uri: gh://addyosmani/agent-skills/planning@v1\n" +
			"  - uri: gh://microsoft/playwright-cli/playwright-cli\n"},
		{name: "templates/coordinator/agents.md", body: "Coordinate the team.\n"},
		{name: "templates/developer/scion-agent.yaml", body: "harness: claude\n" +
			"skills:\n" +
			"  - uri: gh://scion-frontiers/agent-team/tdd@" + wpTeamSkillAt + "\n"},
		{name: "templates/developer/skills/local/SKILL.md", body: "local skill\n"},
	}
}

func newWelcomeFixture(t *testing.T) *welcomeFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv, s := testWorkstationServer(t)
	srv.SetStorage(newMockStorage("test-bucket"))
	welcomeSetExperiment(t, srv, true)

	f := &welcomeFixture{
		t: t, srv: srv, store: s, home: home,
		fetch: &fakeWelcomeFetcher{bodies: map[string][]byte{}},
		refs: map[string]string{
			"scion-frontiers/agent-team@":    wpTeamSHA,
			"addyosmani/agent-skills@v1":     wpSkillsSHA,
			"microsoft/playwright-cli@":      wpPlaywright,
			"scion-frontiers/agent-team@dev": wpTeamSHA,
		},
		now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
	}
	srv.welcome.fetcher = f.fetch
	srv.welcome.resolveRef = func(_ context.Context, owner, repo, ref string) (string, error) {
		f.refMu.Lock()
		defer f.refMu.Unlock()
		key := owner + "/" + repo + "@" + ref
		f.refCalls = append(f.refCalls, key)
		sha, ok := f.refs[key]
		if !ok {
			return "", config.ErrGitRefUnresolvable
		}
		return sha, nil
	}
	srv.welcome.now = func() time.Time { return f.now }
	srv.welcome.credentials = func(context.Context, *store.Project, string) (bool, error) { return f.creds, nil }

	f.setTeam(wpTeamEntries())
	f.fetch.bodies[wpArchiveURL(t, "addyosmani", "agent-skills", wpSkillsSHA)] = wpArchive(t, wpSkillsSHA, "agent-skills-"+wpSkillsSHA, []wpEntry{
		{name: "skills/planning/SKILL.md", body: "plan well\n"},
		{name: "skills/planning/ref/notes.md", body: "notes\n"},
		{name: "skills/other/SKILL.md", body: "not taken\n"},
	})
	f.fetch.bodies[wpArchiveURL(t, "microsoft", "playwright-cli", wpPlaywright)] = wpArchive(t, wpPlaywright, "playwright-cli-"+wpPlaywright, []wpEntry{
		{name: "skills/playwright-cli/SKILL.md", body: "browse\n"},
	})
	f.fetch.bodies[wpArchiveURL(t, "scion-frontiers", "agent-team", wpTeamSkillAt)] = wpArchive(t, wpTeamSkillAt, "agent-team-"+wpTeamSkillAt, []wpEntry{
		{name: "skills/tdd/SKILL.md", body: "red green\n"},
	})
	return f
}

// setTeam replaces the agent-team archive served at wpTeamSHA.
func (f *welcomeFixture) setTeam(entries []wpEntry) {
	f.fetch.bodies[wpArchiveURL(f.t, "scion-frontiers", "agent-team", wpTeamSHA)] =
		wpArchive(f.t, wpTeamSHA, "agent-team-"+wpTeamSHA, entries)
}

// withEmbeddedBroker registers an online auto-provide broker as the embedded
// broker, so the welcome project gets a provider at create.
func (f *welcomeFixture) withEmbeddedBroker() {
	f.t.Helper()
	b := &store.RuntimeBroker{
		ID: tid("welcome-broker"), Name: "welcome-broker", Slug: "welcome-broker",
		Status: store.BrokerStatusOnline, AutoProvide: true, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(f.t, f.store.CreateRuntimeBroker(context.Background(), b))
	f.srv.SetEmbeddedBrokerID(b.ID)
}

func welcomeSetExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.WelcomeProject {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.WelcomeProject))
}

func (f *welcomeFixture) post(path string, body interface{}) *httptest.ResponseRecorder {
	f.t.Helper()
	return doWorkstationRequest(f.t, f.srv, http.MethodPost, path, body)
}

func (f *welcomeFixture) preview() welcomePreviewResponse {
	f.t.Helper()
	rec := f.post(welcomePreviewPath, nil)
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
	var resp welcomePreviewResponse
	require.NoError(f.t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func (f *welcomeFixture) install(previewID string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.post(welcomeInstallPath, welcomeInstallRequest{PreviewID: previewID})
}

func (f *welcomeFixture) ledger() welcomeLedgerDoc {
	f.t.Helper()
	l, err := f.srv.loadWelcomeLedger(context.Background())
	require.NoError(f.t, err)
	return l.doc
}

func (f *welcomeFixture) requireNoProject(projectID string) {
	f.t.Helper()
	if projectID != "" {
		_, err := f.store.GetProject(context.Background(), projectID)
		require.ErrorIs(f.t, err, store.ErrNotFound, "the welcome project must not exist")
	}
	res, err := f.store.ListProjects(context.Background(), store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(f.t, err)
	for _, p := range res.Items {
		assert.NotEqual(f.t, welcomeProjectName, p.Name, "no project named welcome may exist")
	}
}

// stagingDirs lists the staging dirs currently on disk.
func (f *welcomeFixture) stagingDirs() []string {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.home, ".scion", welcomeStagingDirName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(f.t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func decodeWelcomeState(t *testing.T, rec *httptest.ResponseRecorder) welcomeStateResponse {
	t.Helper()
	var resp welcomeStateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp
}

// wpLogBuffer is a goroutine-safe log sink.
type wpLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *wpLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *wpLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// wpCaptureLog routes slog.Default to a buffer for the test.
func wpCaptureLog(t *testing.T) *wpLogBuffer {
	t.Helper()
	buf := &wpLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// --- 1, 4 (templates/ only): shas recorded, no gh:// left -------------------

func TestWelcome_InstallPinsShasVendorsSkillsAndTakesTemplatesOnly(t *testing.T) {
	f := newWelcomeFixture(t)
	staged := map[string]map[string]string{} // template -> rel -> content
	f.srv.welcome.importHook = func(name, dir string) error {
		files := map[string]string{}
		require.NoError(t, filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			require.NoError(t, err)
			if d.Type().IsRegular() {
				b, rerr := os.ReadFile(p)
				require.NoError(t, rerr)
				rel, _ := filepath.Rel(dir, p)
				files[filepath.ToSlash(rel)] = string(b)
			}
			return nil
		}))
		staged[name] = files
		return nil
	}

	pv := f.preview()
	assert.Equal(t, "scion-frontiers/agent-team", pv.Source.Repo)
	assert.Equal(t, "templates", pv.Source.Path)
	assert.Equal(t, wpTeamSHA, pv.Source.SHA)
	assert.Equal(t, []string{"coordinator", "developer"}, pv.Manifest.Templates)
	assert.ElementsMatch(t, []welcomeSkillPin{
		{Repo: "addyosmani/agent-skills", Ref: "v1", SHA: wpSkillsSHA},
		{Repo: "microsoft/playwright-cli", Ref: "", SHA: wpPlaywright},
		{Repo: "scion-frontiers/agent-team", Ref: wpTeamSkillAt, SHA: wpTeamSkillAt},
	}, pv.Manifest.Skills)
	assert.NotEmpty(t, pv.PreviewID)
	assert.Len(t, pv.PreviewID, 64, "256-bit preview id")
	assert.Equal(t, welcomeStatePreviewed, f.ledger().State)

	// Refs: HEAD for the source and for a skill with no ref, the skill's own
	// ref otherwise; a full sha is used as is with no resolution.
	assert.ElementsMatch(t, []string{
		"scion-frontiers/agent-team@", "addyosmani/agent-skills@v1", "microsoft/playwright-cli@",
	}, f.refCalls)
	assert.False(t, f.fetch.fetched("api.github.com"), "no GitHub API call")

	rec := f.install(pv.PreviewID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	st := decodeWelcomeState(t, rec)
	// No broker on this hub: the project is installed, no agent is created.
	assert.Equal(t, welcomeStatePendingBroker, st.State)
	assert.Empty(t, st.AgentID)

	ctx := context.Background()
	led := f.ledger()
	assert.Equal(t, welcomeStatePendingBroker, led.State)
	require.NotNil(t, led.Manifest)
	assert.Equal(t, wpTeamSHA, led.Manifest.AgentTeamSHA)
	assert.Equal(t, pv.Manifest, *led.Manifest)
	assert.Equal(t, st.ProjectID, led.ProjectID)

	project, err := f.store.GetProject(ctx, led.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, welcomeProjectName, project.Name)
	assert.Equal(t, []api.SharedDir{{Name: "scratchpad"}}, project.SharedDirs)
	var annotated welcomeManifest
	require.NoError(t, json.Unmarshal([]byte(project.Annotations[welcomeManifestAnnotation]), &annotated))
	assert.Equal(t, *led.Manifest, annotated, "the annotation matches the ledger")

	_, err = f.store.GetAgentBySlug(ctx, project.ID, welcomeCoordinatorAgent)
	assert.ErrorIs(t, err, store.ErrNotFound, "pending_broker creates no agent")

	// Staged trees: templates/ only, skills vendored, no skills[] left.
	require.Len(t, staged, 2)
	coord := staged["coordinator"]
	assert.Equal(t, "plan well\n", coord["skills/planning/SKILL.md"])
	assert.Equal(t, "notes\n", coord["skills/planning/ref/notes.md"])
	assert.Equal(t, "browse\n", coord["skills/playwright-cli/SKILL.md"])
	assert.NotContains(t, coord, "skills/other/SKILL.md")
	dev := staged["developer"]
	assert.Equal(t, "red green\n", dev["skills/tdd/SKILL.md"])
	assert.Equal(t, "local skill\n", dev["skills/local/SKILL.md"])
	for name, files := range staged {
		for rel, content := range files {
			assert.NotContains(t, content, "gh://", "%s/%s", name, rel)
			assert.NotContains(t, rel, ".antigravitycli", name)
		}
		assert.NotContains(t, files["scion-agent.yaml"], "skills:", name)
		assert.Contains(t, files["scion-agent.yaml"], "harness: claude", name)
	}

	// Stored templates are project-scoped and carry the same files.
	for _, name := range []string{"coordinator", "developer"} {
		tmpl, err := f.store.GetTemplateBySlug(ctx, name, store.TemplateScopeProject, project.ID)
		require.NoError(t, err, name)
		var paths []string
		for _, fl := range tmpl.Files {
			paths = append(paths, fl.Path)
		}
		sort.Strings(paths)
		var want []string
		for rel := range staged[name] {
			want = append(want, rel)
		}
		sort.Strings(want)
		assert.Equal(t, want, paths, name)
	}
	_, err = f.store.GetTemplateBySlug(ctx, "readme", store.TemplateScopeProject, project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	ok, err := f.srv.VerifyWelcomeProjectTemplates(ctx)
	require.NoError(t, err)
	assert.True(t, ok, "fresh install matches its manifest")
	assert.Empty(t, f.stagingDirs(), "the staging dir is removed after install")
}

// --- sec 1: drift detection ---------------------------------------------------

func TestWelcome_TemplateChangeAfterInstallIsDrift(t *testing.T) {
	f := newWelcomeFixture(t)
	pv := f.preview()
	require.Equal(t, http.StatusOK, f.install(pv.PreviewID).Code)
	ctx := context.Background()
	led := f.ledger()
	stor := f.srv.GetStorage()

	tmpl, err := f.store.GetTemplateBySlug(ctx, "developer", store.TemplateScopeProject, led.ProjectID)
	require.NoError(t, err)
	require.NotEmpty(t, tmpl.Files)
	require.True(t, isBlobLayout(tmpl))
	original := tmpl.Files[0].Hash

	// setFirstHash points the first manifest entry at hash. The manifest is a
	// content column: UpdateTemplate does not write it, so the change goes
	// through UpdateTemplateContent, and the read-back proves it was stored.
	setFirstHash := func(hash string) *store.Template {
		t.Helper()
		cur, err := f.store.GetTemplate(ctx, tmpl.ID)
		require.NoError(t, err)
		expected := store.TemplateContentPrecondition{ContentHash: cur.ContentHash, Layout: cur.Layout}
		cur.Files[0].Hash = hash
		require.NoError(t, f.store.UpdateTemplateContent(ctx, cur, expected))
		after, err := f.store.GetTemplate(ctx, tmpl.ID)
		require.NoError(t, err)
		require.Equal(t, hash, after.Files[0].Hash, "the changed manifest entry is stored")
		return after
	}

	// A manifest entry whose blob is missing is a mismatch.
	setFirstHash("sha256:" + strings.Repeat("0", 64))
	ok, err := f.srv.VerifyWelcomeProjectTemplates(ctx)
	require.NoError(t, err)
	assert.False(t, ok, "a manifest entry with no stored blob is a manifest mismatch")

	// Control: the original entry matches again.
	setFirstHash(original)
	ok, err = f.srv.VerifyWelcomeProjectTemplates(ctx)
	require.NoError(t, err)
	require.True(t, ok, "the restored manifest matches the install")

	// A consistent change after install: new bytes stored as their own blob
	// and a manifest entry that names them, so only the install's pinned
	// digest can tell.
	changed := []byte("changed after install\n")
	changedHash := transfer.HashBytes(changed)
	changedHex, isHash := templateBlobHex(changedHash)
	require.True(t, isHash)
	_, err = stor.Upload(ctx, templateBlobPath(tmpl.StoragePath, changedHex), bytes.NewReader(changed), storage.UploadOptions{})
	require.NoError(t, err)
	after := setFirstHash(changedHash)
	data, err := readTemplateObject(ctx, stor, templateObjectPath(after, after.Files[0]), defaultWelcomeLimits.MaxFile)
	require.NoError(t, err)
	require.Equal(t, changed, data, "the manifest entry and the stored bytes agree")
	ok, err = f.srv.VerifyWelcomeProjectTemplates(ctx)
	require.NoError(t, err)
	assert.False(t, ok, "a changed template file is drift from the install's pinned digest")

	require.NoError(t, f.store.DeleteTemplate(ctx, tmpl.ID))
	ok, err = f.srv.VerifyWelcomeProjectTemplates(ctx)
	require.NoError(t, err)
	assert.False(t, ok, "a removed template is a manifest mismatch")
}

// --- 2: allow-list -------------------------------------------------------------

func TestWelcome_SkillRepoOutsideAllowListFailsClosed(t *testing.T) {
	f := newWelcomeFixture(t)
	entries := wpTeamEntries()
	entries[5] = wpEntry{name: "templates/developer/scion-agent.yaml", body: "harness: claude\nskills:\n  - uri: gh://evil-org/agent-skills/planning@v1\n"}
	f.setTeam(entries)

	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "evil-org/agent-skills is not on the allow-list")
	led := f.ledger()
	assert.Equal(t, welcomeStateFailed, led.State)
	assert.Contains(t, led.Reason, "allow-list")
	f.requireNoProject(led.ProjectID)
	assert.False(t, f.fetch.fetched("evil-org"), "a refused repository is never downloaded")
	assert.Empty(t, f.stagingDirs())

	// Failed is final: no retry, no skip.
	rec = f.post(welcomePreviewPath, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	rec = f.post(welcomeSkipPath, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, welcomeStateFailed, f.ledger().State)
}

func TestWelcome_SkillRepoIdentityIsCanonical(t *testing.T) {
	f := newWelcomeFixture(t)
	entries := wpTeamEntries()
	// Case variants and a .git suffix name the same allowed repository.
	entries[5] = wpEntry{name: "templates/developer/scion-agent.yaml", body: "harness: claude\nskills:\n" +
		"  - uri: gh://AddyOsmani/Agent-Skills.git/planning@v1\n"}
	f.setTeam(entries)
	f.refs["addyosmani/agent-skills@v1"] = wpSkillsSHA

	pv := f.preview()
	for _, p := range pv.Manifest.Skills {
		assert.Equal(t, strings.ToLower(p.Repo), p.Repo)
		assert.NotContains(t, p.Repo, ".git")
	}
	assert.Contains(t, pv.Manifest.Skills, welcomeSkillPin{Repo: "addyosmani/agent-skills", Ref: "v1", SHA: wpSkillsSHA})
}

func TestWelcome_SkillURIOnAnotherHostOrWithCredentialIsRefused(t *testing.T) {
	for _, uri := range []string{
		"https://gitlab.com/addyosmani/agent-skills/planning",
		"gh://addyosmani/agent-skills/planning?token=GH_TOKEN",
		"file:///etc/skills/planning",
		// https form: userinfo, a port, plain http, a query (O3).
		"https://user@github.com/addyosmani/agent-skills/tree/v1/skills/planning",
		"https://github.com:443/addyosmani/agent-skills/tree/v1/skills/planning",
		"http://github.com/addyosmani/agent-skills/tree/v1/skills/planning",
		"https://github.com/addyosmani/agent-skills/tree/v1/skills/planning?x=1",
	} {
		t.Run(uri, func(t *testing.T) {
			f := newWelcomeFixture(t)
			entries := wpTeamEntries()
			entries[5] = wpEntry{name: "templates/developer/scion-agent.yaml", body: "harness: claude\nskills:\n  - uri: " + uri + "\n"}
			f.setTeam(entries)
			rec := f.post(welcomePreviewPath, nil)
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			f.requireNoProject(f.ledger().ProjectID)
		})
	}
}

// --- 3: refs ---------------------------------------------------------------------

func TestWelcome_UnresolvableSkillRefFails(t *testing.T) {
	f := newWelcomeFixture(t)
	entries := wpTeamEntries()
	entries[5] = wpEntry{name: "templates/developer/scion-agent.yaml", body: "harness: claude\nskills:\n  - uri: gh://addyosmani/agent-skills/planning@missing\n"}
	f.setTeam(entries)

	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "could not resolve skill planning")
	assert.Equal(t, welcomeStateFailed, f.ledger().State)
	f.requireNoProject(f.ledger().ProjectID)
}

func TestWelcome_UnresolvableSourceFails(t *testing.T) {
	f := newWelcomeFixture(t)
	delete(f.refs, "scion-frontiers/agent-team@")
	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, welcomeStateFailed, f.ledger().State)
	assert.Empty(t, f.fetch.calls, "nothing is downloaded")
}

// --- 4: pax verification ---------------------------------------------------------

func wpExtract(t *testing.T, body []byte, sha string, lim welcomeLimits, sel func(string) []string) (string, error) {
	t.Helper()
	var roots []string
	if sel == nil {
		roots = []string{"templates"}
	}
	return wpExtractRoots(t, body, sha, lim, sel, roots)
}

// wpExtractRoots is wpExtract with explicit selection roots.
func wpExtractRoots(t *testing.T, body []byte, sha string, lim welcomeLimits, sel func(string) []string, roots []string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	if sel == nil {
		sel = func(rel string) []string {
			if strings.HasPrefix(rel, "templates/") {
				return []string{strings.TrimPrefix(rel, "templates/")}
			}
			return nil
		}
	}
	return dir, extractWelcomeArchive(body, sha, root, sel, roots, &welcomeBudget{lim: lim})
}

func requireEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing may be written")
}

func TestExtractWelcomeArchive_PaxCommitVerifiedBeforeExtraction(t *testing.T) {
	entries := []wpEntry{{name: "templates/a/f.txt", body: "x"}}

	dir, err := wpExtract(t, wpArchive(t, wpTeamSHA, "r-"+wpTeamSHA, entries), wpSkillsSHA, defaultWelcomeLimits, nil)
	require.ErrorIs(t, err, errWelcomePaxMismatch)
	requireEmptyDir(t, dir)

	dir, err = wpExtract(t, wpArchive(t, "", "r-"+wpTeamSHA, entries), wpTeamSHA, defaultWelcomeLimits, nil)
	require.ErrorIs(t, err, errWelcomePaxMissing)
	requireEmptyDir(t, dir)

	dir, err = wpExtract(t, wpArchive(t, wpTeamSHA, "r-"+wpTeamSHA, entries), wpTeamSHA, defaultWelcomeLimits, nil)
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(dir, "a", "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(b))
}

func TestWelcome_PaxMismatchFailsWithNoProject(t *testing.T) {
	f := newWelcomeFixture(t)
	logs := wpCaptureLog(t)
	f.fetch.bodies[wpArchiveURL(t, "scion-frontiers", "agent-team", wpTeamSHA)] =
		wpArchive(t, wpSkillsSHA, "agent-team-x", wpTeamEntries())
	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "is not commit "+wpTeamSHA)
	f.requireNoProject(f.ledger().ProjectID)
	assert.Empty(t, f.stagingDirs())
	assertNoLocalPathOrMember(t, f, rec.Body.String()+logs.String())
}

// --- 5: refusal set and caps -----------------------------------------------------

func TestExtractWelcomeArchive_RefusalSet(t *testing.T) {
	top := "r-" + wpTeamSHA
	cases := map[string]wpEntry{
		"symlink":         {name: "templates/a/link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		"hardlink":        {name: "templates/a/hard", typeflag: tar.TypeLink, linkname: top + "/README.md"},
		"char device":     {name: "templates/a/dev", typeflag: tar.TypeChar},
		"fifo":            {name: "templates/a/fifo", typeflag: tar.TypeFifo},
		"symlinked dir":   {name: "templates/a", typeflag: tar.TypeSymlink, linkname: "/tmp"},
		"absolute name":   {name: "/etc/templates/a/x", body: "x", raw: true},
		"dot-dot name":    {name: top + "/templates/../../escape", body: "x", raw: true},
		"second top":      {name: "other/templates/a/x", body: "x", raw: true},
		"dot-dot in path": {name: "templates/a/../../../x", body: "x"},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			body := wpArchive(t, wpTeamSHA, top, []wpEntry{{name: "README.md", body: "r"}, e, {name: "templates/b/ok.txt", body: "ok"}})
			_, err := wpExtract(t, body, wpTeamSHA, defaultWelcomeLimits, nil)
			require.ErrorIs(t, err, errWelcomeRefused)
		})
	}

	// A link outside the selected tree is not taken and does not fail.
	body := wpArchive(t, wpTeamSHA, top, []wpEntry{
		{name: "CLAUDE.md", typeflag: tar.TypeSymlink, linkname: "AGENTS.md"},
		{name: "templates/b/ok.txt", body: "ok"},
	})
	_, err := wpExtract(t, body, wpTeamSHA, defaultWelcomeLimits, nil)
	require.NoError(t, err)
}

// A link standing in for a selection root (templates/, a skill's source dir)
// or for any directory on the way to one is refused, not skipped (sec S1);
// the same names as real directories pass.
func TestExtractWelcomeArchive_LinkAtOrAboveASelectionRootIsRefused(t *testing.T) {
	top := "r-" + wpSkillsSHA
	skillSel := func(rel string) []string {
		if rest, ok := strings.CutPrefix(rel, "skills/planning/"); ok && rest != "" {
			return []string{"dev/skills/planning/" + rest}
		}
		return nil
	}
	skillRoots := []string{"skills/planning"}
	file := wpEntry{name: "skills/planning/SKILL.md", body: "plan\n"}

	for name, e := range map[string]wpEntry{
		"link at the skill source dir":  {name: "skills/planning", typeflag: tar.TypeSymlink, linkname: "/tmp"},
		"link at its parent":            {name: "skills", typeflag: tar.TypeSymlink, linkname: "/tmp"},
		"fifo at its parent":            {name: "skills", typeflag: tar.TypeFifo},
		"hardlink at the skill src dir": {name: "skills/planning", typeflag: tar.TypeLink, linkname: top + "/README.md"},
	} {
		t.Run(name, func(t *testing.T) {
			body := wpArchive(t, wpSkillsSHA, top, []wpEntry{e, file})
			dir, err := wpExtractRoots(t, body, wpSkillsSHA, defaultWelcomeLimits, skillSel, skillRoots)
			require.ErrorIs(t, err, errWelcomeRefused)
			requireEmptyDir(t, dir)
		})
	}
	t.Run("regular dirs at the same names pass", func(t *testing.T) {
		body := wpArchive(t, wpSkillsSHA, top, []wpEntry{
			{name: "skills/", typeflag: tar.TypeDir}, {name: "skills/planning/", typeflag: tar.TypeDir}, file,
			{name: "other-link", typeflag: tar.TypeSymlink, linkname: "README.md"},
		})
		dir, err := wpExtractRoots(t, body, wpSkillsSHA, defaultWelcomeLimits, skillSel, skillRoots)
		require.NoError(t, err)
		b, err := os.ReadFile(filepath.Join(dir, "dev", "skills", "planning", "SKILL.md"))
		require.NoError(t, err)
		assert.Equal(t, "plan\n", string(b))
	})

	tt := "r-" + wpTeamSHA
	t.Run("link at templates/", func(t *testing.T) {
		body := wpArchive(t, wpTeamSHA, tt, []wpEntry{{name: "templates", typeflag: tar.TypeSymlink, linkname: "/tmp"}, {name: "templates/a/f.txt", body: "x"}})
		dir, err := wpExtract(t, body, wpTeamSHA, defaultWelcomeLimits, nil)
		require.ErrorIs(t, err, errWelcomeRefused)
		requireEmptyDir(t, dir)
	})
	t.Run("templates/ as a regular dir passes", func(t *testing.T) {
		body := wpArchive(t, wpTeamSHA, tt, []wpEntry{{name: "templates/", typeflag: tar.TypeDir}, {name: "templates/a/f.txt", body: "x"}})
		_, err := wpExtract(t, body, wpTeamSHA, defaultWelcomeLimits, nil)
		require.NoError(t, err)
	})
}

// The same refusal through the whole stage: a link at templates/ in the
// agent-team archive, and a link at a skill source dir's parent in a skill
// archive, each fail the preview by name with no project.
func TestWelcome_LinkAtTemplatesOrSkillParentFailsTheStage(t *testing.T) {
	t.Run("templates", func(t *testing.T) {
		f := newWelcomeFixture(t)
		f.setTeam(append([]wpEntry{{name: "templates", typeflag: tar.TypeSymlink, linkname: "elsewhere"}}, wpTeamEntries()...))
		rec := f.post(welcomePreviewPath, nil)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "the archive of scion-frontiers/agent-team at "+wpTeamSHA+" was refused")
		assert.Equal(t, welcomeStateFailed, f.ledger().State)
		assert.Empty(t, f.stagingDirs())
	})
	t.Run("skill source parent", func(t *testing.T) {
		f := newWelcomeFixture(t)
		f.fetch.bodies[wpArchiveURL(t, "addyosmani", "agent-skills", wpSkillsSHA)] = wpArchive(t, wpSkillsSHA, "agent-skills-"+wpSkillsSHA, []wpEntry{
			{name: "skills", typeflag: tar.TypeSymlink, linkname: "elsewhere"},
			{name: "skills/planning/SKILL.md", body: "plan well\n"},
		})
		rec := f.post(welcomePreviewPath, nil)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "the archive of addyosmani/agent-skills at "+wpSkillsSHA+" was refused")
		assert.Equal(t, welcomeStateFailed, f.ledger().State)
		assert.Empty(t, f.stagingDirs())
	})
	t.Run("regular dirs control", func(t *testing.T) {
		f := newWelcomeFixture(t)
		f.setTeam(append([]wpEntry{{name: "templates/", typeflag: tar.TypeDir}}, wpTeamEntries()...))
		f.fetch.bodies[wpArchiveURL(t, "addyosmani", "agent-skills", wpSkillsSHA)] = wpArchive(t, wpSkillsSHA, "agent-skills-"+wpSkillsSHA, []wpEntry{
			{name: "skills/", typeflag: tar.TypeDir},
			{name: "skills/planning/", typeflag: tar.TypeDir},
			{name: "skills/planning/SKILL.md", body: "plan well\n"},
		})
		rec := f.post(welcomePreviewPath, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

// F-3: a template whose agent config sets auth_selectedType outside the
// agent schema's enum (the no-auth sentinel "none", or any other value) fails
// the stage by name, with no project; a listed auth type passes.
func TestWelcome_TemplateWithDisallowedAuthTypeFailsByName(t *testing.T) {
	// The allowed set is a copy of the agent schema's auth_selectedType enum;
	// a value added to the schema must be added here too.
	raw, err := os.ReadFile(filepath.Join("..", "config", "schemas", "agent-v1.schema.json"))
	require.NoError(t, err)
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(raw, &schema))
	enum := schema.Properties["auth_selectedType"].Enum
	require.NotEmpty(t, enum, "the schema lists auth_selectedType values")
	assert.ElementsMatch(t, enum, slices.Collect(maps.Keys(welcomeAllowedAuthTypes)), "welcomeAllowedAuthTypes matches the schema enum")

	yamlAuth := func(line string) wpEntry {
		return wpEntry{name: "templates/developer/scion-agent.yaml", body: "harness: claude\n" +
			line + "\n" +
			"skills:\n" +
			"  - uri: gh://scion-frontiers/agent-team/tdd@" + wpTeamSkillAt + "\n"}
	}
	// jsonAuth writes the config as JSON with the given auth members, in order.
	jsonAuth := func(members string) wpEntry {
		return wpEntry{name: "templates/developer/scion-agent.json", body: `{"harness": "claude", ` + members +
			`, "skills": [{"uri": "gh://scion-frontiers/agent-team/tdd@` + wpTeamSkillAt + `"}]}`}
	}
	// The broker's decoder maps hyphenated YAML keys and case variants of JSON
	// keys to AuthSelectedType too, so those spellings are judged as well.
	// "json variant then exact" passes the first check (the decoder takes the
	// last matching key, api-key) and is refused only by the re-check after the
	// skills rewrite, which re-marshals with sorted keys so the variant is last.
	for name, tc := range map[string]struct {
		config wpEntry
		ok     bool
	}{
		"none":                    {config: yamlAuth("auth_selectedType: none")},
		"other invalid":           {config: yamlAuth("auth_selectedType: magic")},
		"control":                 {config: yamlAuth("auth_selectedType: api-key"), ok: true},
		"yaml hyphenated key":     {config: yamlAuth("auth-selectedType: none")},
		"json case-variant key":   {config: jsonAuth(`"AUTH_SELECTEDTYPE": "none"`)},
		"json exact then variant": {config: jsonAuth(`"auth_selectedType": "api-key", "AUTH_SELECTEDTYPE": "none"`)},
		"json variant then exact": {config: jsonAuth(`"auth_selectedtype": "none", "auth_selectedType": "api-key"`)},
		"json control":            {config: jsonAuth(`"auth_selectedType": "api-key"`), ok: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWelcomeFixture(t)
			entries := wpTeamEntries()
			entries[5] = tc.config
			f.setTeam(entries)
			rec := f.post(welcomePreviewPath, nil)
			if tc.ok {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				return
			}
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "template developer sets an auth type that is not allowed")
			assert.Equal(t, welcomeStateFailed, f.ledger().State)
			assert.Empty(t, f.stagingDirs())
			f.requireNoProject(f.ledger().ProjectID)
		})
	}
}

func TestExtractWelcomeArchive_CapsCountedWhileStreaming(t *testing.T) {
	top := "r-" + wpTeamSHA
	small := welcomeLimits{MaxDownload: 1 << 20, MaxUnpacked: 1 << 20, MaxTotal: 10, MaxFiles: 2, MaxFile: 6}

	t.Run("per-file", func(t *testing.T) {
		body := wpArchive(t, wpTeamSHA, top, []wpEntry{{name: "templates/a/big", body: "1234567"}})
		_, err := wpExtract(t, body, wpTeamSHA, small, nil)
		require.ErrorIs(t, err, errWelcomeTooLarge)
	})
	t.Run("file count", func(t *testing.T) {
		body := wpArchive(t, wpTeamSHA, top, []wpEntry{
			{name: "templates/a/1", body: "1"}, {name: "templates/a/2", body: "2"}, {name: "templates/a/3", body: "3"},
		})
		dir, err := wpExtract(t, body, wpTeamSHA, small, nil)
		require.ErrorIs(t, err, errWelcomeTooLarge)
		_, statErr := os.Stat(filepath.Join(dir, "a", "3"))
		assert.ErrorIs(t, statErr, os.ErrNotExist, "the cap stops extraction at the breaching entry")
	})
	t.Run("total", func(t *testing.T) {
		body := wpArchive(t, wpTeamSHA, top, []wpEntry{{name: "templates/a/1", body: "123456"}, {name: "templates/a/2", body: "123456"}})
		_, err := wpExtract(t, body, wpTeamSHA, small, nil)
		require.ErrorIs(t, err, errWelcomeTooLarge)
	})
	t.Run("decompressed stream", func(t *testing.T) {
		lim := small
		lim.MaxUnpacked = 4096
		lim.MaxFile, lim.MaxTotal = 1<<20, 1<<20
		body := wpArchive(t, wpTeamSHA, top, []wpEntry{{name: "outside/zeros", body: strings.Repeat("0", 64<<10)}})
		_, err := wpExtract(t, body, wpTeamSHA, lim, nil)
		require.ErrorIs(t, err, errWelcomeTooLarge, "entries outside the selection still count against the stream cap")
	})
}

func TestWelcome_DownloadCapFailsClosed(t *testing.T) {
	f := newWelcomeFixture(t)
	f.srv.welcome.limits = defaultWelcomeLimits
	f.srv.welcome.limits.MaxDownload = 64
	f.srv.welcome.limitsSet = true
	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "exceeds the download limit")
	f.requireNoProject(f.ledger().ProjectID)
}

func TestWelcome_ExtractRefusalFailsWithNoProject(t *testing.T) {
	f := newWelcomeFixture(t)
	logs := wpCaptureLog(t)
	entries := append(wpTeamEntries(), wpEntry{name: "templates/coordinator/escape", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"})
	f.setTeam(entries)
	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, welcomeStateFailed, f.ledger().State)
	f.requireNoProject(f.ledger().ProjectID)
	assert.Empty(t, f.stagingDirs(), "the staging dir is removed after a failure")
	assertNoLocalPathOrMember(t, f, rec.Body.String()+logs.String())
}

// --- 6: staged change after preview -------------------------------------------

func TestWelcome_StagedChangeAfterPreviewFailsInstall(t *testing.T) {
	f := newWelcomeFixture(t)
	pv := f.preview()
	var dir string
	for _, p := range f.srv.welcome.previews {
		dir = p.staged.dir
	}
	require.NotEmpty(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "coordinator", "agents.md"), []byte("tampered\n"), 0o600))

	rec := f.install(pv.PreviewID)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "changed after the preview")
	led := f.ledger()
	assert.Equal(t, welcomeStateFailed, led.State)
	f.requireNoProject(led.ProjectID)
	_, err := os.Stat(dir)
	assert.ErrorIs(t, err, os.ErrNotExist, "the staging dir is removed")
}

// --- add 3: failure after project create rolls back ---------------------------

func TestWelcome_ImportFailureRollsBackProject(t *testing.T) {
	f := newWelcomeFixture(t)
	f.srv.welcome.importHook = func(name, _ string) error {
		if name == "developer" {
			return errors.New("injected import failure")
		}
		return nil
	}
	pv := f.preview()
	led := f.ledger()
	rec := f.install(pv.PreviewID)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "could not import template developer")
	assert.NotContains(t, rec.Body.String(), "injected", "internal errors are not echoed")
	assert.Equal(t, welcomeStateFailed, f.ledger().State)
	f.requireNoProject(led.ProjectID)
	_, err := f.store.GetTemplateBySlug(context.Background(), "coordinator", store.TemplateScopeProject, led.ProjectID)
	assert.ErrorIs(t, err, store.ErrNotFound, "templates imported before the failure are gone too")
}

// --- 7: preview_id rules -----------------------------------------------------------

func TestWelcome_PreviewIDSingleUseOwnerOnlyAndExpiring(t *testing.T) {
	f := newWelcomeFixture(t)
	pv := f.preview()

	rec := f.install("unknown-preview")
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = f.install("")
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Another hub admin cannot use it, and the attempt does not burn it.
	otherID := hubConfigTokenUser(t, f.store, "welcome-other-admin", store.SystemRoleSuperAdmin)
	other, err := f.store.GetUser(context.Background(), otherID)
	require.NoError(t, err)
	rec = doRequestAsUserLoopback(t, f.srv, other, http.MethodPost, welcomeInstallPath, welcomeInstallRequest{PreviewID: pv.PreviewID})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = doRequestAsUserLoopback(t, f.srv, other, http.MethodPost, welcomePreviewPath, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, "the step belongs to its first user: %s", rec.Body.String())
	assert.Len(t, f.srv.welcome.previews, 1)

	// Expired after 30 minutes.
	f.now = f.now.Add(welcomePreviewTTL + time.Second)
	rec = f.install(pv.PreviewID)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Empty(t, f.stagingDirs(), "an expired preview's staging dir is removed")

	pv2 := f.preview()
	rec = f.install(pv2.PreviewID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.install(pv2.PreviewID)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a preview id works once: %s", rec.Body.String())
}

func TestWelcome_PreviewMapIsBounded(t *testing.T) {
	f := newWelcomeFixture(t)
	f.srv.welcome.mu.Lock()
	defer f.srv.welcome.mu.Unlock()
	var ids []string
	for i := 0; i < welcomePreviewMax+5; i++ {
		dir, err := newWelcomeStagingDir()
		require.NoError(t, err)
		f.now = f.now.Add(time.Second)
		p, err := f.srv.addWelcomePreview(DevUserID, "p", &welcomeStaged{dir: dir})
		require.NoError(t, err)
		ids = append(ids, p.id)
	}
	assert.Len(t, f.srv.welcome.previews, welcomePreviewMax)
	assert.Len(t, f.stagingDirs(), welcomePreviewMax, "evicted previews' staging dirs are removed")
	assert.Nil(t, f.srv.takeWelcomePreview(ids[0], DevUserID), "the oldest preview was evicted")

	// Expired previews are evicted first.
	f.now = f.now.Add(welcomePreviewTTL + time.Minute)
	dir, err := newWelcomeStagingDir()
	require.NoError(t, err)
	_, err = f.srv.addWelcomePreview(DevUserID, "p", &welcomeStaged{dir: dir})
	require.NoError(t, err)
	assert.Len(t, f.srv.welcome.previews, 1)
	assert.Len(t, f.stagingDirs(), 1)
}

func TestWelcome_PreviewWithoutStagedTreeIsRefused(t *testing.T) {
	f := newWelcomeFixture(t)
	f.srv.welcome.mu.Lock()
	defer f.srv.welcome.mu.Unlock()
	p, err := f.srv.addWelcomePreview(DevUserID, "p", nil)
	require.Error(t, err)
	assert.Nil(t, p)
	assert.Empty(t, f.srv.welcome.previews, "a refused preview is not stored")
}

// --- 8: staging dir mode and no local path ---------------------------------------

// assertNoLocalPathOrMember asserts text holds no local path and none of the
// fixture's archive member names.
func assertNoLocalPathOrMember(t *testing.T, f *welcomeFixture, text string) {
	t.Helper()
	assert.NotContains(t, text, f.home)
	assert.NotContains(t, text, welcomeStagingDirName)
	for _, member := range []string{"agents.md", "SKILL.md", "README.md", ".antigravitycli", "notes.md", "escape", "pax_global_header"} {
		assert.NotContains(t, text, member)
	}
}

func TestWelcome_StagingDirPrivateAndNoLocalPathInResponsesOrLogs(t *testing.T) {
	logs := wpCaptureLog(t)
	f := newWelcomeFixture(t)
	// The welcome code logs through slog.Default; the template import it calls
	// (bootstrapSingleTemplate) logs through the server's template and
	// resource subsystem loggers, so those go to the same sink here.
	sink := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.srv.templateLog, f.srv.resourceLog = sink, sink
	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	bodies := rec.Body.String()

	var dir string
	for _, p := range f.srv.welcome.previews {
		dir = p.staged.dir
	}
	fi, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm(), "the staging dir is owner-only")
	base, err := os.Stat(filepath.Dir(dir))
	require.NoError(t, err)
	assert.Zero(t, base.Mode().Perm()&0o077, "the staging base is owner-only")

	var pv welcomePreviewResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pv))
	rec = f.install(pv.PreviewID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	bodies += rec.Body.String()

	assert.NotContains(t, bodies, f.home)
	assert.NotContains(t, bodies, welcomeStagingDirName)
	assert.NotContains(t, logs.String(), dir)
	assert.NotContains(t, logs.String(), welcomeStagingDirName)
	assert.Contains(t, logs.String(), wpTeamSHA, "the hub log records the shas")
}

func TestRemoveWelcomeStagingDir_ConfinedToHubStaging(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("k"), 0o600))

	// A path outside the staging base is never removed.
	require.Error(t, removeWelcomeStagingDirChecked(outside))
	// A link standing in for a staging dir is not followed.
	base, err := welcomeStagingBase()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(base, 0o700))
	link := filepath.Join(base, strings.Repeat("a", 32))
	require.NoError(t, os.Symlink(outside, link))
	require.Error(t, removeWelcomeStagingDirChecked(link))
	// A link inside a staging dir is removed as a link, not followed.
	dir, err := newWelcomeStagingDir()
	require.NoError(t, err)
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "inner")))
	require.NoError(t, removeWelcomeStagingDirChecked(dir))
	_, err = os.Stat(dir)
	assert.ErrorIs(t, err, os.ErrNotExist)

	b, err := os.ReadFile(keep)
	require.NoError(t, err)
	assert.Equal(t, "k", string(b), "nothing outside the staging dir is touched")
}

// --- 9: experiment off --------------------------------------------------------------

func TestWelcome_ExperimentOffRefusesAllRoutes(t *testing.T) {
	f := newWelcomeFixture(t)
	welcomeSetExperiment(t, f.srv, false)
	for _, path := range []string{welcomePreviewPath, welcomeInstallPath, welcomeSkipPath} {
		rec := f.post(path, welcomeInstallRequest{PreviewID: "x"})
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", path, rec.Body.String())
	}
	l, err := f.srv.loadWelcomeLedger(context.Background())
	require.NoError(t, err)
	assert.False(t, l.exists(), "nothing is recorded")
	assert.Empty(t, f.fetch.calls, "nothing is downloaded")
}

// --- 10: skip, and never re-created -----------------------------------------------

func TestWelcome_SkipIsIdempotentAndFinal(t *testing.T) {
	f := newWelcomeFixture(t)
	f.preview()
	rec := f.post(welcomeSkipPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, welcomeStateSkipped, decodeWelcomeState(t, rec).State)
	assert.Empty(t, f.stagingDirs(), "skip removes outstanding previews")

	rec = f.post(welcomeSkipPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, "a repeat skip answers the same: %s", rec.Body.String())
	assert.Equal(t, welcomeStateSkipped, decodeWelcomeState(t, rec).State)

	rec = f.post(welcomePreviewPath, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	rec = f.install("anything")
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, welcomeStateSkipped, f.ledger().State)
	f.requireNoProject("")
}

func TestWelcome_DeletedProjectIsNeverRecreated(t *testing.T) {
	f := newWelcomeFixture(t)
	pv := f.preview()
	require.Equal(t, http.StatusOK, f.install(pv.PreviewID).Code)
	led := f.ledger()

	rec := doWorkstationRequest(t, f.srv, http.MethodDelete, "/api/v1/projects/"+led.ProjectID+"?force=true", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())

	rec = f.post(welcomePreviewPath, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	rec = f.post(welcomeSkipPath, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, "skip refuses after the project was created: %s", rec.Body.String())
	rec = f.install(pv.PreviewID)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	f.requireNoProject(led.ProjectID)
	assert.Equal(t, led.State, f.ledger().State, "a delete changes nothing in the ledger")
}

// --- 11: no credential ------------------------------------------------------------

func TestWelcome_NoCredentialGivesPendingCredentialsAndNoAgent(t *testing.T) {
	f := newWelcomeFixture(t)
	f.withEmbeddedBroker()
	f.creds = false
	pv := f.preview()
	rec := f.install(pv.PreviewID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	st := decodeWelcomeState(t, rec)
	assert.Equal(t, welcomeStatePendingCredentials, st.State)
	assert.Empty(t, st.AgentID)

	ctx := context.Background()
	_, err := f.store.GetProject(ctx, st.ProjectID)
	require.NoError(t, err, "the project exists")
	agents, err := f.store.ListAgents(ctx, store.AgentFilter{ProjectID: st.ProjectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, agents.Items, "no agent, so no NoAuth agent")
	assert.Equal(t, welcomeStatePendingCredentials, f.ledger().State)
}

func TestWelcomeCoordinatorHasCredentials_UnresolvedHarnessConfigIsNoCredential(t *testing.T) {
	f := newWelcomeFixture(t)
	f.withEmbeddedBroker()
	f.srv.welcome.credentials = nil // the production check
	pv := f.preview()
	rec := f.install(pv.PreviewID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, welcomeStatePendingCredentials, decodeWelcomeState(t, rec).State,
		"a coordinator whose harness config does not resolve is not started")
}

// --- 12, add 2: coordinator role and task --------------------------------------------

func TestWelcome_CoordinatorRoleIsProjectScopedWithNoHubWideRole(t *testing.T) {
	f := newWelcomeFixture(t)
	f.withEmbeddedBroker()
	f.creds = true
	pv := f.preview()
	rec := f.install(pv.PreviewID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	st := decodeWelcomeState(t, rec)
	require.Equal(t, welcomeStateStarted, st.State, rec.Body.String())
	require.NotEmpty(t, st.AgentID)
	ctx := context.Background()

	agent, err := f.store.GetAgent(ctx, st.AgentID)
	require.NoError(t, err)
	assert.Equal(t, st.ProjectID, agent.ProjectID)
	require.NotNil(t, agent.AppliedConfig)
	// (1) the tier.
	assert.Equal(t, string(AgentRoleFull), agent.AppliedConfig.AgentRole)
	assert.False(t, agent.AppliedConfig.NoAuth)
	// add 2: the task text.
	assert.True(t, strings.HasPrefix(agent.AppliedConfig.Task, "Greet "), agent.AppliedConfig.Task)
	assert.True(t, strings.HasSuffix(agent.AppliedConfig.Task,
		" (user:"+DevUserID+") once with scion message; ask what project they want to start; then wait."), agent.AppliedConfig.Task)

	// (2) scopes: project:agent:create, and every scope is project-relative.
	scopes := ScopesForRole(AgentRole(agent.AppliedConfig.AgentRole))
	assert.Contains(t, scopes, ScopeAgentCreate)
	for _, sc := range scopes {
		assert.True(t, strings.HasPrefix(string(sc), "project:") || strings.HasPrefix(string(sc), "agent:"),
			"scope %s is project-relative", sc)
	}
	// (3) the token is bound to the welcome project.
	tok, err := f.srv.GenerateAgentToken(agent.ID, agent.ProjectID, nil, AgentRole(agent.AppliedConfig.AgentRole), nil)
	require.NoError(t, err)
	claims, err := f.srv.agentTokenService.ValidateAgentToken(tok)
	require.NoError(t, err)
	assert.Equal(t, st.ProjectID, claims.ProjectID)

	// (4) no system-scope binding and none on another project.
	bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalAgent, agent.ID)
	require.NoError(t, err)
	for _, b := range bindings {
		assert.NotEqual(t, store.RoleScopeSystem, b.ScopeType, "no hub-wide role")
		assert.Equal(t, st.ProjectID, b.ScopeID, "no role on another project")
	}

	// (5) creating an agent in another project is denied.
	other := &store.Project{ID: tid("welcome-other-project"), Name: "Other", Slug: "welcome-other", OwnerID: DevUserID}
	require.NoError(t, f.store.CreateProject(ctx, other))
	rec = doLoopbackSystemRequest(f.srv, http.MethodPost, "/api/v1/projects/"+other.ID+"/agents",
		`{"name":"intruder"}`, map[string]string{"X-Scion-Agent-Token": tok})
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
	_, err = f.store.GetAgentBySlug(ctx, other.ID, "intruder")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Q4: the coordinator resolves --type <role> against the welcome
	// project's templates.
	rec = doLoopbackSystemRequest(f.srv, http.MethodGet,
		"/api/v1/templates?scope=project&projectId="+st.ProjectID+"&name=developer", "",
		map[string]string{"X-Scion-Agent-Token": tok})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list ListTemplatesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.NotEmpty(t, list.Templates)
	assert.Equal(t, "developer", list.Templates[0].Name)

	// Skip refuses after started.
	rec = f.post(welcomeSkipPath, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

// --- sec 4: hosts ----------------------------------------------------------------------

func TestWelcomeArchiveFetcher_OnlyGitHubArchiveHosts(t *testing.T) {
	assert.Equal(t, []string{"github.com", "codeload.github.com"}, welcomeArchiveHosts)
	fetcher := newWelcomeArchiveFetcher(defaultWelcomeLimits, slog.Default())
	// Every hop, the first and each redirect target, passes the same host
	// check (remotefetch TestFetchAllowedHosts covers the redirect hop); a
	// refused host is refused before any connection.
	for _, u := range []string{
		"https://api.github.com/repos/o/r/tarball/" + wpTeamSHA,
		"https://raw.githubusercontent.com/o/r/" + wpTeamSHA + "/x",
		"https://codeload.github.com.example.invalid/o/r/tar.gz/" + wpTeamSHA,
		"http://github.com/o/r/archive/" + wpTeamSHA + ".tar.gz",
	} {
		_, err := fetcher.FetchBytes(context.Background(), u)
		var fe *remotefetch.Error
		require.True(t, errors.As(err, &fe), u)
		assert.Contains(t, []remotefetch.Reason{remotefetch.ReasonHostNotAllowed, remotefetch.ReasonScheme}, fe.Reason, u)
	}
}

// --- ledger CAS ---------------------------------------------------------------------------

func TestWelcomeLedger_CASRefusesStaleTransition(t *testing.T) {
	f := newWelcomeFixture(t)
	ctx := context.Background()
	_, err := f.srv.transitionWelcomeLedger(ctx, welcomeLedgerDoc{State: welcomeStateClaimed, OwnerUserID: "u"}, "")
	require.NoError(t, err)
	_, err = f.srv.transitionWelcomeLedger(ctx, welcomeLedgerDoc{State: welcomeStateClaimed, OwnerUserID: "v"}, "")
	require.ErrorIs(t, err, errWelcomeLedgerConflict, "a second claim loses")
	stale, err := f.srv.loadWelcomeLedger(ctx)
	require.NoError(t, err)
	_, err = f.srv.transitionWelcomeLedger(ctx, welcomeLedgerDoc{State: welcomeStateSkipped}, welcomeStateClaimed)
	require.NoError(t, err)
	err = f.srv.saveWelcomeLedger(ctx, stale, welcomeLedgerDoc{State: welcomeStateStarted})
	require.ErrorIs(t, err, errWelcomeLedgerConflict, "a write from a stale revision loses")
	assert.Equal(t, welcomeStateSkipped, f.ledger().State)
	assert.Equal(t, fmt.Sprint(welcomeLedgerSchemaVersion), fmt.Sprint(f.ledger().SchemaVersion))
}

// --- round 1: R1 detached context ------------------------------------------

// wpServeWithContext sends a workstation request with ctx through the full
// handler, as the dev super-admin.
func wpServeWithContext(f *welcomeFixture, ctx context.Context, path string, body interface{}) *httptest.ResponseRecorder {
	f.t.Helper()
	var b []byte
	if body != nil {
		var err error
		b, err = json.Marshal(body)
		require.NoError(f.t, err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWelcome_ClientGoneMidInstallStillRollsBackAndRecordsFailed(t *testing.T) {
	f := newWelcomeFixture(t)
	pv := f.preview()
	led := f.ledger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.srv.welcome.importHook = func(name, _ string) error {
		if name == "developer" {
			cancel() // the client goes away between create and this import
			return errors.New("injected import failure")
		}
		return nil
	}
	rec := wpServeWithContext(f, ctx, welcomeInstallPath, welcomeInstallRequest{PreviewID: pv.PreviewID})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, welcomeStateFailed, f.ledger().State, "the failure is recorded on the detached context")
	f.requireNoProject(led.ProjectID)
}

func TestWelcome_ClientGoneMidInstallDoesNotAbortIt(t *testing.T) {
	f := newWelcomeFixture(t)
	pv := f.preview()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.srv.welcome.importHook = func(name, _ string) error {
		if name == "coordinator" {
			cancel()
		}
		return nil
	}
	rec := wpServeWithContext(f, ctx, welcomeInstallPath, welcomeInstallRequest{PreviewID: pv.PreviewID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	led := f.ledger()
	assert.Equal(t, welcomeStatePendingBroker, led.State)
	_, err := f.store.GetProject(context.Background(), led.ProjectID)
	require.NoError(t, err, "the install completed after the client went away")
}

// O-1 (release-manager 3fa0dc4e): a client that cancels mid-preview leaves
// the ledger at claimed with no staging dir, and the same owner's next
// preview proceeds from claimed.
func TestWelcome_ClientCancelMidPreviewKeepsClaimedThenPreviewSucceeds(t *testing.T) {
	f := newWelcomeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := f.srv.welcome.resolveRef
	f.srv.welcome.resolveRef = func(c context.Context, owner, repo, ref string) (string, error) {
		cancel()
		if c.Err() != nil {
			return "", c.Err()
		}
		return inner(c, owner, repo, ref)
	}
	rec := wpServeWithContext(f, ctx, welcomePreviewPath, nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	led := f.ledger()
	assert.Equal(t, welcomeStateClaimed, led.State, "a client cancel records nothing final")
	assert.Empty(t, led.Reason)
	assert.Empty(t, f.stagingDirs(), "the staging dir is removed")
	f.requireNoProject(led.ProjectID)

	f.srv.welcome.resolveRef = inner
	pv := f.preview()
	require.NotEmpty(t, pv.PreviewID)
	again := f.ledger()
	assert.Equal(t, welcomeStatePreviewed, again.State)
	assert.Equal(t, led.ProjectID, again.ProjectID, "the claimed project id is kept")
}

// wpCancelFetcher cancels the request inside the archive fetch, after the
// staging dir exists, and records how many staging dirs were on disk then.
type wpCancelFetcher struct {
	cancel   context.CancelFunc
	base     string
	dirsSeen int
	canceled bool // ctx.Err() is context.Canceled after the cancel
}

func (c *wpCancelFetcher) FetchBytes(ctx context.Context, _ string) (*remotefetch.Result, error) {
	entries, _ := os.ReadDir(c.base)
	c.dirsSeen = len(entries)
	c.cancel()
	c.canceled = errors.Is(ctx.Err(), context.Canceled)
	return nil, ctx.Err()
}

// L-2: a client cancel AFTER the staging dir exists (inside the fetch)
// removes that dir and keeps the claim. Control: the dir existed when the
// cancel fired, so the empty assertion is not vacuous.
func TestWelcome_ClientCancelAfterStagingRemovesTheStagingDir(t *testing.T) {
	f := newWelcomeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cf := &wpCancelFetcher{cancel: cancel, base: filepath.Join(f.home, ".scion", welcomeStagingDirName)}
	f.srv.welcome.fetcher = cf
	rec := wpServeWithContext(f, ctx, welcomePreviewPath, nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, 1, cf.dirsSeen, "the staging dir existed when the client cancelled")
	require.True(t, cf.canceled, "the fetcher saw the request context cancelled")
	assert.Empty(t, f.stagingDirs(), "the staging dir is removed")
	led := f.ledger()
	assert.Equal(t, welcomeStateClaimed, led.State, "a client cancel records nothing final")
	assert.Empty(t, led.Reason)
	f.requireNoProject(led.ProjectID)
}

// wpGetProjectFailOnce fails the first successful GetProject of one id, once.
// The create route's own existence check reads a missing project, so the
// failure lands on the welcome flow's read after the create.
type wpGetProjectFailOnce struct {
	store.Store
	id    string
	fired bool
}

func (w *wpGetProjectFailOnce) GetProject(ctx context.Context, id string) (*store.Project, error) {
	p, err := w.Store.GetProject(ctx, id)
	if err == nil && id == w.id && !w.fired {
		w.fired = true
		return nil, errors.New("injected read failure")
	}
	return p, err
}

// N-c: a project read that fails right after a successful create rolls the
// project back, as the later error paths do.
func TestWelcome_ProjectReadFailureAfterCreateRollsBack(t *testing.T) {
	f := newWelcomeFixture(t)
	pv := f.preview()
	fs := &wpGetProjectFailOnce{Store: f.srv.store, id: f.ledger().ProjectID}
	f.srv.store = fs
	rec := f.install(pv.PreviewID)
	f.srv.store = fs.Store
	require.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, fs.fired, "the injected read failure fired after the create")
	f.requireNoProject(f.ledger().ProjectID)
}

// O-1 condition (a): a deadline (or any error other than a client cancel)
// still records failed.
func TestWelcome_DeadlineMidPreviewStillRecordsFailed(t *testing.T) {
	f := newWelcomeFixture(t)
	f.srv.welcome.resolveRef = func(context.Context, string, string, string) (string, error) {
		return "", context.DeadlineExceeded
	}
	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, welcomeStateFailed, f.ledger().State)
	assert.Empty(t, f.stagingDirs())
	f.requireNoProject(f.ledger().ProjectID)
}

// O-2: only one preview is outstanding. A new preview replaces the earlier
// one: the earlier preview_id no longer installs and only one staging dir is
// left.
func TestWelcome_NewPreviewReplacesTheEarlierOne(t *testing.T) {
	f := newWelcomeFixture(t)
	first := f.preview()
	require.Len(t, f.stagingDirs(), 1)
	second := f.preview()
	require.NotEqual(t, first.PreviewID, second.PreviewID)
	assert.Len(t, f.stagingDirs(), 1, "the earlier preview's staging dir is removed")

	rec := f.install(first.PreviewID)
	require.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), errCodeWelcomePreviewInvalid)

	require.Equal(t, http.StatusOK, f.install(second.PreviewID).Code)
}

// L-1: a failed re-preview ends the step, so the earlier preview is
// forgotten: its staging dir is removed and its preview_id is gone.
func TestWelcome_FailedRePreviewDropsTheEarlierPreview(t *testing.T) {
	f := newWelcomeFixture(t)
	first := f.preview()
	require.Len(t, f.stagingDirs(), 1)
	require.Len(t, f.srv.welcome.previews, 1)

	delete(f.fetch.bodies, wpArchiveURL(t, "scion-frontiers", "agent-team", wpTeamSHA))
	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, welcomeStateFailed, f.ledger().State)
	assert.Empty(t, f.stagingDirs(), "the earlier preview's staging dir is removed")
	assert.Empty(t, f.srv.welcome.previews, "the earlier preview is forgotten")

	require.NotEqual(t, http.StatusOK, f.install(first.PreviewID).Code)
	f.requireNoProject(f.ledger().ProjectID)
}

// L-1: the first preview call of a process sweeps the staging dirs an
// earlier process left behind. Only <32-hex> real directories under the
// staging base go; other names and links stay, and a link's target is
// untouched. L4-1: the sweep runs before the final-state early return, so a
// final-state ledger still clears a stale dir (the call itself answers 409).
func TestWelcome_FirstPreviewSweepsStaleStagingDirs(t *testing.T) {
	t.Run("first preview", func(t *testing.T) {
		f := newWelcomeFixture(t)
		base := filepath.Join(f.home, ".scion", welcomeStagingDirName)
		stale := filepath.Join(base, strings.Repeat("a", 32))
		require.NoError(t, os.MkdirAll(filepath.Join(stale, "t"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(stale, "t", "f"), []byte("x"), 0o600))
		other := filepath.Join(base, "not-a-staging-dir")
		require.NoError(t, os.Mkdir(other, 0o700))
		// Creating a symlink needs a privilege on Windows, so the link case runs
		// on the other platforms only.
		withLink := runtime.GOOS != "windows"
		outside := t.TempDir()
		link := filepath.Join(base, strings.Repeat("b", 32))
		if withLink {
			require.NoError(t, os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0o600))
			require.NoError(t, os.Symlink(outside, link))
		}

		f.preview()

		_, err := os.Lstat(stale)
		assert.ErrorIs(t, err, os.ErrNotExist, "a stale staging dir is removed")
		_, err = os.Lstat(other)
		assert.NoError(t, err, "a name that is not a staging dir stays")
		want := 2 // the new preview's dir and the other name
		if withLink {
			fi, err := os.Lstat(link)
			require.NoError(t, err, "a link is not removed")
			assert.NotZero(t, fi.Mode()&os.ModeSymlink)
			b, err := os.ReadFile(filepath.Join(outside, "keep"))
			require.NoError(t, err)
			assert.Equal(t, "keep", string(b), "a link's target is untouched")
			want = 3 // and the link
		}
		assert.Len(t, f.stagingDirs(), want)
	})
	t.Run("final-state ledger", func(t *testing.T) {
		f := newWelcomeFixture(t)
		require.Equal(t, http.StatusOK, f.post(welcomeSkipPath, nil).Code)
		require.True(t, welcomeStateFinal(f.ledger().State), "skip leaves a final state")
		require.False(t, f.srv.welcome.stagingSwept, "skip does not sweep")
		stale := filepath.Join(f.home, ".scion", welcomeStagingDirName, strings.Repeat("c", 32))
		require.NoError(t, os.MkdirAll(stale, 0o700))
		require.Len(t, f.stagingDirs(), 1, "control: the stale dir exists before the call")

		rec := f.post(welcomePreviewPath, nil)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		_, err := os.Lstat(stale)
		assert.ErrorIs(t, err, os.ErrNotExist, "the stale dir is removed even though the step is done")
		assert.Empty(t, f.stagingDirs())
	})
}

// R2-1: CRLF content in a template file and in a vendored skill file is
// normalized before the manifest hash, so the manifest matches the stored
// (normalized) bytes and a fresh install reports no drift. Control: the raw
// CRLF bytes would not match, so the test cannot pass vacuously.
func TestWelcome_CRLFFilesHashAsStoredAndFreshInstallHasNoDrift(t *testing.T) {
	f := newWelcomeFixture(t)
	const tplCRLF = "Coordinate\r\nthe team.\r\n"
	const skillCRLF = "plan\r\nwell\r\n"
	entries := wpTeamEntries()
	entries[4] = wpEntry{name: "templates/coordinator/agents.md", body: tplCRLF}
	f.setTeam(entries)
	f.fetch.bodies[wpArchiveURL(t, "addyosmani", "agent-skills", wpSkillsSHA)] = wpArchive(t, wpSkillsSHA, "agent-skills-"+wpSkillsSHA, []wpEntry{
		{name: "skills/planning/SKILL.md", body: skillCRLF},
		{name: "skills/planning/ref/notes.md", body: "notes\n"},
	})

	pv := f.preview()
	require.Equal(t, http.StatusOK, f.install(pv.PreviewID).Code)
	ctx := context.Background()
	ok, err := f.srv.VerifyWelcomeProjectTemplates(ctx)
	require.NoError(t, err)
	require.True(t, ok, "a fresh install holding CRLF files reports no drift")

	led := f.ledger()
	tmpl, err := f.store.GetTemplateBySlug(ctx, "coordinator", store.TemplateScopeProject, led.ProjectID)
	require.NoError(t, err)
	stored := map[string][]byte{}
	files := map[string]string{}
	for _, name := range led.Manifest.Templates {
		tt, err := f.store.GetTemplateBySlug(ctx, api.Slugify(name), store.TemplateScopeProject, led.ProjectID)
		require.NoError(t, err)
		for _, fe := range tt.Files {
			data, err := readTemplateObject(ctx, f.srv.GetStorage(), templateObjectPath(tt, fe), defaultWelcomeLimits.MaxFile)
			require.NoError(t, err)
			stored[name+"/"+fe.Path] = data
			files[name+"/"+fe.Path] = transfer.HashBytes(data)
		}
	}
	require.NotNil(t, tmpl)
	assert.Equal(t, "Coordinate\nthe team.\n", string(stored["coordinator/agents.md"]))
	assert.Equal(t, "plan\nwell\n", string(stored["coordinator/skills/planning/SKILL.md"]))
	assert.Equal(t, led.Manifest.FilesSHA256, welcomeFilesDigest(files), "the stored bytes hash to the manifest")

	raw := maps.Clone(files)
	raw["coordinator/agents.md"] = transfer.HashBytes([]byte(tplCRLF))
	raw["coordinator/skills/planning/SKILL.md"] = transfer.HashBytes([]byte(skillCRLF))
	assert.NotEqual(t, led.Manifest.FilesSHA256, welcomeFilesDigest(raw), "the raw CRLF bytes do not match the manifest")
}

// --- round 1: O1 drift re-hashes stored bytes --------------------------------

func TestWelcome_StoredByteChangeIsDriftEvenWithUnchangedHashField(t *testing.T) {
	f := newWelcomeFixture(t)
	pv := f.preview()
	require.Equal(t, http.StatusOK, f.install(pv.PreviewID).Code)
	ctx := context.Background()
	ok, err := f.srv.VerifyWelcomeProjectTemplates(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	tmpl, err := f.store.GetTemplateBySlug(ctx, "coordinator", store.TemplateScopeProject, f.ledger().ProjectID)
	require.NoError(t, err)
	require.NotEmpty(t, tmpl.Files)
	obj := templateObjectPath(tmpl, tmpl.Files[0])
	_, err = f.srv.GetStorage().Upload(ctx, obj, bytes.NewReader([]byte("tampered bytes\n")), storage.UploadOptions{})
	require.NoError(t, err)

	after, err := f.store.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, tmpl.Files, after.Files, "the stored hash metadata is untouched")
	ok, err = f.srv.VerifyWelcomeProjectTemplates(ctx)
	require.NoError(t, err)
	assert.False(t, ok, "changed stored bytes are a mismatch")
}

// --- round 1: O2 no-auth fallback forbidden ------------------------------------

func TestWelcome_NoAuthFallbackIsRefusedAndNoAgentSurvives(t *testing.T) {
	f := newWelcomeFixture(t)
	f.withEmbeddedBroker()
	f.creds = true // the welcome pre-check passes; the create path's own check finds no credential
	ctx := context.Background()
	require.NoError(t, f.store.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("wp-dts-hc"), Name: "wp-dts", Slug: "wp-dts", Harness: "claude",
		ContentHash: "wp-dts-hash", Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{
			NoAuthBehavior: "drop-to-shell",
			AuthMeta: &api.HarnessAuthMetadata{Types: map[string]api.HarnessAuthTypeMetadata{
				"api-key": {RequiredEnv: []api.HarnessAuthEnvRequirement{{AnyOf: []string{"WP_ABSENT_API_KEY"}}}},
			}},
		},
	}))
	entries := wpTeamEntries()
	entries[3] = wpEntry{name: "templates/coordinator/scion-agent.yaml", body: "harness: claude\ndefault_harness_config: wp-dts\n"}
	f.setTeam(entries)

	pv := f.preview()
	led := f.ledger()
	rec := f.install(pv.PreviewID)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "no credentials")
	assert.Equal(t, welcomeStateFailed, f.ledger().State)
	agents, err := f.store.ListAgents(ctx, store.AgentFilter{ProjectID: led.ProjectID, IncludeDeleted: true}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, agents.Items, "no agent was ever created without credentials")
	f.requireNoProject(led.ProjectID)
}

// --- round 1: N1, N2 -----------------------------------------------------------

func TestWelcome_DuplicateSkillNameInATemplateFails(t *testing.T) {
	f := newWelcomeFixture(t)
	entries := wpTeamEntries()
	entries[5] = wpEntry{name: "templates/developer/scion-agent.yaml", body: "harness: claude\nskills:\n" +
		"  - uri: gh://addyosmani/agent-skills/planning@v1\n" +
		"  - uri: gh://microsoft/playwright-cli/playwright-cli\n    as: planning\n"}
	f.setTeam(entries)
	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "two skills named planning")
	f.requireNoProject(f.ledger().ProjectID)
}

func TestWelcome_TemplateWithTwoAgentConfigsFails(t *testing.T) {
	f := newWelcomeFixture(t)
	entries := append(wpTeamEntries(), wpEntry{name: "templates/developer/scion-agent.json", body: "{}"})
	f.setTeam(entries)
	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "more than one agent config")
	f.requireNoProject(f.ledger().ProjectID)
}

// --- round 1: SEC-2 narrowing ----------------------------------------------------

func TestWelcome_SpecialEntryOutsideSelectionIsSkippedAndCounted(t *testing.T) {
	f := newWelcomeFixture(t)
	entries := append(wpTeamEntries(),
		wpEntry{name: "CLAUDE.md", typeflag: tar.TypeSymlink, linkname: "AGENTS.md"},
		wpEntry{name: "docs/dev", typeflag: tar.TypeChar})
	f.setTeam(entries)
	f.fetch.bodies[wpArchiveURL(t, "addyosmani", "agent-skills", wpSkillsSHA)] = wpArchive(t, wpSkillsSHA, "agent-skills-"+wpSkillsSHA, []wpEntry{
		{name: "skills/planning/SKILL.md", body: "plan well\n"},
		{name: "skills/planning/ref/notes.md", body: "notes\n"},
		{name: "skills/other/link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	pv := f.preview()
	assert.Equal(t, 3, pv.Manifest.SkippedEntries, "the 3 special entries outside the selected subtrees are counted")
	var dir string
	for _, p := range f.srv.welcome.previews {
		dir = p.staged.dir
	}
	for _, rel := range []string{"CLAUDE.md", "docs", "coordinator/skills/other"} {
		_, err := os.Lstat(filepath.Join(dir, rel))
		assert.ErrorIs(t, err, os.ErrNotExist, "%s is never extracted", rel)
	}
	assert.Equal(t, 3, f.ledger().Manifest.SkippedEntries, "the count is in the ledger")
}

func TestWelcome_SpecialEntryInsideASkillSubtreeRefusesTheInstall(t *testing.T) {
	f := newWelcomeFixture(t)
	f.fetch.bodies[wpArchiveURL(t, "addyosmani", "agent-skills", wpSkillsSHA)] = wpArchive(t, wpSkillsSHA, "agent-skills-"+wpSkillsSHA, []wpEntry{
		{name: "skills/planning/SKILL.md", body: "plan well\n"},
		{name: "skills/planning/dev", typeflag: tar.TypeChar},
	})
	rec := f.post(welcomePreviewPath, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, welcomeStateFailed, f.ledger().State)
	f.requireNoProject(f.ledger().ProjectID)
}

// --- round 1: SEC-1, SEC-3 ---------------------------------------------------------

// wpCallAs calls a welcome handler directly with identity u in the context,
// bypassing the route guard, to show what the in-process handler calls
// enforce on their own.
func wpCallAs(f *welcomeFixture, u *store.User, h func(http.ResponseWriter, *http.Request), path string, body interface{}) *httptest.ResponseRecorder {
	f.t.Helper()
	var b []byte
	if body != nil {
		var err error
		b, err = json.Marshal(body)
		require.NoError(f.t, err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	ident := authUser(u)
	ctx := contextWithIdentity(req.Context(), ident)
	ctx = context.WithValue(ctx, userContextKey{}, UserIdentity(ident))
	rec := httptest.NewRecorder()
	h(rec, req.WithContext(ctx))
	return rec
}

func TestWelcome_CallerWithoutProjectCreateGetsAFailedInstall(t *testing.T) {
	f := newWelcomeFixture(t)
	// A hub viewer, not in the hub-members group, holds no project.create
	// (hub members do).
	viewerID := tid("welcome-viewer")
	createTestUserWithRole(t, f.store, viewerID, viewerID+"@test.com", "member", store.SystemRoleHubViewer)
	member, err := f.store.GetUser(context.Background(), viewerID)
	require.NoError(t, err)
	rec := wpCallAs(f, member, f.srv.handleWelcomePreview, welcomePreviewPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var pv welcomePreviewResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pv))
	led := f.ledger()

	rec = wpCallAs(f, member, f.srv.handleWelcomeInstall, welcomeInstallPath, welcomeInstallRequest{PreviewID: pv.PreviewID})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "could not create the welcome project")
	assert.Equal(t, welcomeStateFailed, f.ledger().State)
	f.requireNoProject(led.ProjectID)
}

func TestWelcome_AnotherAdminCannotSkipAClaimedStep(t *testing.T) {
	f := newWelcomeFixture(t)
	f.preview()
	otherID := hubConfigTokenUser(t, f.store, "welcome-skip-other", store.SystemRoleSuperAdmin)
	other, err := f.store.GetUser(context.Background(), otherID)
	require.NoError(t, err)
	rec := doRequestAsUserLoopback(t, f.srv, other, http.MethodPost, welcomeSkipPath, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, welcomeStatePreviewed, f.ledger().State, "another admin's skip changes nothing")
	rec = f.post(welcomeSkipPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, "the owner can still skip: %s", rec.Body.String())
}

// --- round 1: FYI existing "welcome" slug ---------------------------------------

// An existing project with slug "welcome" does not fail the install: project
// create picks the next free slug, as it does for any create.
func TestWelcome_ExistingWelcomeSlugGetsTheNextFreeSlug(t *testing.T) {
	f := newWelcomeFixture(t)
	ctx := context.Background()
	require.NoError(t, f.store.CreateProject(ctx, &store.Project{ID: tid("welcome-taken"), Name: "welcome", Slug: "welcome", OwnerID: DevUserID}))
	pv := f.preview()
	rec := f.install(pv.PreviewID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	project, err := f.store.GetProject(ctx, f.ledger().ProjectID)
	require.NoError(t, err)
	assert.NotEqual(t, "welcome", project.Slug)
	assert.True(t, strings.HasPrefix(project.Slug, "welcome"), project.Slug)
	st := decodeWelcomeState(t, rec)
	assert.Equal(t, project.Slug, st.ProjectSlug, "the install result names the project as created")
	assert.Equal(t, project.Name, st.ProjectName)
	assert.Equal(t, project.Slug, f.ledger().ProjectSlug, "the ledger records the actual slug")
}
