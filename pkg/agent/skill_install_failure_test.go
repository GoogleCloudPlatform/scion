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

package agent

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// Tests for #5020: a skill that resolves but fails to install is skipped
// when optional, and fails the create with a *SkillResolutionError naming
// the skill when required.

const (
	installFailGoodURI    = "skill://scion/core/good-skill@1.0"
	installFailMissingURI = "gh://owner/repo/missing-skill@main"
	// installFail404Reason is the user-facing reason for a SKILL.md
	// download that got a 404: download failures are shown verbatim.
	installFail404Reason = "failed to download SKILL.md: download failed with status 404"
)

// newInstallFailServer serves good content at /good/SKILL.md and a 404 for
// every other path, and points http.DefaultTransport at it for the test.
func newInstallFailServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/good/SKILL.md" {
			_, _ = w.Write(content)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = origTransport })
	return srv
}

// installFailSkills returns a skill that installs and a skill whose SKILL.md
// download 404s, with missingOptional as the latter's Optional flag.
func installFailSkills(srvURL string, content []byte, missingOptional bool) []ResolvedSkill {
	contentHash := transfer.HashBytes(content)
	return []ResolvedSkill{
		{
			Name:    "good-skill",
			URI:     installFailGoodURI,
			Version: "1.0.0",
			Files: []ResolvedFile{
				{Path: "SKILL.md", URL: srvURL + "/good/SKILL.md", Hash: contentHash},
			},
		},
		{
			Name:     "missing-skill",
			URI:      installFailMissingURI,
			Version:  "main",
			Optional: missingOptional,
			Files: []ResolvedFile{
				{Path: "SKILL.md", URL: srvURL + "/missing/SKILL.md"},
			},
		},
	}
}

func TestInstallResolvedSkills_OptionalInstallFailureSkipped(t *testing.T) {
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)
	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	record, skipped, err := installResolvedSkills(context.Background(),
		installFailSkills(srv.URL, content, true), skillsDest, agentHome)
	if err != nil {
		t.Fatalf("an optional skill install failure must not fail the install, got: %v", err)
	}

	if len(record.Skills) != 1 || record.Skills[0].URI != installFailGoodURI {
		t.Errorf("record should hold only the good skill, got %+v", record.Skills)
	}
	if _, err := os.Stat(filepath.Join(skillsDest, "good-skill", "SKILL.md")); err != nil {
		t.Errorf("good skill should still be installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillsDest, "missing-skill")); !os.IsNotExist(err) {
		t.Errorf("skipped skill must not be installed, stat err = %v", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skipped skill, got %+v", skipped)
	}
	if skipped[0].URI != installFailMissingURI {
		t.Errorf("skipped URI = %q, want %q", skipped[0].URI, installFailMissingURI)
	}
	if skipped[0].Reason != installFail404Reason {
		t.Errorf("skipped reason = %q, want %q", skipped[0].Reason, installFail404Reason)
	}
}

func TestInstallResolvedSkills_RequiredInstallFailureTypedError(t *testing.T) {
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)
	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	_, _, err := installResolvedSkills(context.Background(),
		installFailSkills(srv.URL, content, false), skillsDest, agentHome)
	if err == nil {
		t.Fatal("a required skill's install failure must fail the install")
	}
	var skillErr *SkillResolutionError
	if !errors.As(err, &skillErr) {
		t.Fatalf("required install failure: expected a *SkillResolutionError, got %T: %v", err, err)
	}
	if skillErr.URI != installFailMissingURI {
		t.Errorf("error URI = %q, want %q", skillErr.URI, installFailMissingURI)
	}
	if skillErr.Code != SkillErrCodeInstallFailed {
		t.Errorf("error code = %q, want %q", skillErr.Code, SkillErrCodeInstallFailed)
	}
	if skillErr.Message != installFail404Reason {
		t.Errorf("error message = %q, want %q", skillErr.Message, installFail404Reason)
	}
	want := `required skill "` + installFailMissingURI + `" could not be installed: ` + installFail404Reason
	if err.Error() != want {
		t.Errorf("err.Error() = %q, want %q", err.Error(), want)
	}
}

// TestInstallResolvedSkills_FilesystemErrorDoesNotLeakPath checks that a
// local filesystem failure, whose error names a broker path, reaches the
// user as the fixed reason rather than the path.
func TestInstallResolvedSkills_FilesystemErrorDoesNotLeakPath(t *testing.T) {
	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	// "dir" is written as a file, so creating "dir/SKILL.md" fails with
	// ENOTDIR on a path under skillsDest (independent of permissions, so
	// it also fails as root).
	skills := []ResolvedSkill{{
		Name:    "fs-skill",
		URI:     "skill://scion/core/fs-skill@1.0",
		Version: "1.0.0",
		Files: []ResolvedFile{
			{Path: "dir", Content: []byte("a file")},
			{Path: "dir/SKILL.md", Content: []byte("# FS Skill")},
		},
	}}

	_, _, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	var skillErr *SkillResolutionError
	if !errors.As(err, &skillErr) {
		t.Fatalf("filesystem install failure: expected a *SkillResolutionError, got %T: %v", err, err)
	}
	if skillErr.Message != skillInstallFailedOnBroker {
		t.Errorf("error message = %q, want the fixed %q", skillErr.Message, skillInstallFailedOnBroker)
	}
	if strings.Contains(err.Error(), agentHome) {
		t.Errorf("user-facing error leaks a broker path: %q", err.Error())
	}
}

// writeInstallFailTemplate writes a template that references the good skill
// (required) and the missing skill, optional or not.
func writeInstallFailTemplate(t *testing.T, missingOptional bool) (projectScionDir string) {
	t.Helper()
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	tplDir := filepath.Join(globalScionDir, "templates", "install-fail-tpl")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	optional := "false"
	if missingOptional {
		optional = "true"
	}
	tplConfig := `{
		"default_harness_config": "claude",
		"skills": [
			{"uri": "` + installFailGoodURI + `"},
			{"uri": "` + installFailMissingURI + `", "optional": ` + optional + `}
		]
	}`
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644); err != nil {
		t.Fatal(err)
	}

	projectScionDir = filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

func TestProvisionAgent_OptionalSkillInstallFailure_CreateSucceeds(t *testing.T) {
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)
	projectScionDir := writeInstallFailTemplate(t, true)

	// The resolver leaves Optional unset on the resolved skill, as a
	// resolver that does not copy it would; the template's optional flag
	// must still apply.
	resolver := &mockResolver{resolved: installFailSkills(srv.URL, content, false)}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	records := captureSlog(t)

	agentHome, _, _, err := ProvisionAgent(ctx, "install-fail-agent", "install-fail-tpl", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent must succeed when only an optional skill fails to install, got: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(agentHome, ".scion", "resolved-skills.json"))
	if err != nil {
		t.Fatalf("expected resolved-skills.json: %v", err)
	}
	if !strings.Contains(string(data), installFailGoodURI) {
		t.Errorf("resolution record should list the good skill, got: %s", data)
	}
	if strings.Contains(string(data), installFailMissingURI) {
		t.Errorf("resolution record must not list the skipped skill, got: %s", data)
	}
	if !hasSkipWarning(*records, "optional skill could not be installed; skipped", installFailMissingURI, installFail404Reason) {
		t.Errorf("expected a Warn naming %s and reason %q, got %d records", installFailMissingURI, installFail404Reason, len(*records))
	}
}

func TestProvisionAgent_RequiredSkillInstallFailure_YieldsSkillResolutionError(t *testing.T) {
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)
	projectScionDir := writeInstallFailTemplate(t, false)

	resolver := &mockResolver{resolved: installFailSkills(srv.URL, content, false)}
	ctx := ContextWithSkillResolver(context.Background(), resolver)

	_, _, _, err := ProvisionAgent(ctx, "install-fail-agent", "install-fail-tpl", "", "", projectScionDir, "", "", "", "")
	if err == nil {
		t.Fatal("ProvisionAgent must fail when a required skill fails to install")
	}
	var skillErr *SkillResolutionError
	if !errors.As(err, &skillErr) {
		t.Fatalf("provision required install failure: expected a *SkillResolutionError, got %T: %v", err, err)
	}
	if skillErr.URI != installFailMissingURI {
		t.Errorf("provision error URI = %q, want %q", skillErr.URI, installFailMissingURI)
	}
	if skillErr.Code != SkillErrCodeInstallFailed {
		t.Errorf("provision error code = %q, want %q", skillErr.Code, SkillErrCodeInstallFailed)
	}
}

// captureSlog routes the default slog logger to a collector for the test.
func captureSlog(t *testing.T) *[]slog.Record {
	t.Helper()
	var records []slog.Record
	old := slog.Default()
	slog.SetDefault(slog.New(&collectHandler{records: &records}))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &records
}

// hasSkipWarning reports whether records hold a Warn with message msg whose
// skill_uri is uri and whose reason is reason.
func hasSkipWarning(records []slog.Record, msg, uri, reason string) bool {
	for _, r := range records {
		if r.Level != slog.LevelWarn || r.Message != msg {
			continue
		}
		var gotURI, gotReason string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "skill_uri":
				gotURI = a.Value.String()
			case "reason":
				gotReason = a.Value.String()
			}
			return true
		})
		if gotURI == uri && gotReason == reason {
			return true
		}
	}
	return false
}

// TestProvisionAgent_OptionalSkillResolveFailure_WarnsAndSucceeds checks that
// an optional skill whose resolution fails is skipped with a Warn naming the
// skill and the reason, not only a debug line.
func TestProvisionAgent_OptionalSkillResolveFailure_WarnsAndSucceeds(t *testing.T) {
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)
	projectScionDir := writeInstallFailTemplate(t, true)

	const reason = "expected gh://owner/repo/skill-name"
	resolver := &mockResolver{
		resolved: installFailSkills(srv.URL, content, false)[:1],
		errors:   []ResolveError{{URI: installFailMissingURI, Code: "invalid_uri", Message: reason}},
	}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	records := captureSlog(t)

	_, _, _, err := ProvisionAgent(ctx, "resolve-fail-agent", "install-fail-tpl", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent must succeed when only an optional skill fails to resolve, got: %v", err)
	}
	if !hasSkipWarning(*records, "optional skill could not be resolved; skipped", installFailMissingURI, reason) {
		t.Errorf("resolve skip: expected a Warn naming %s and reason %q, got %d records", installFailMissingURI, reason, len(*records))
	}
}

// TestProvisionAgent_OptionalSkillMissingFromResponse_Warns checks that an
// optional skill the resolver neither resolved nor reported an error for is
// skipped with a Warn naming the skill, not silently.
func TestProvisionAgent_OptionalSkillMissingFromResponse_Warns(t *testing.T) {
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)
	projectScionDir := writeInstallFailTemplate(t, true)

	resolver := &mockResolver{resolved: installFailSkills(srv.URL, content, false)[:1]}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	records := captureSlog(t)

	_, _, _, err := ProvisionAgent(ctx, "missing-skill-agent", "install-fail-tpl", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent must succeed when an optional skill is missing from the response, got: %v", err)
	}
	if !hasSkipWarning(*records, "optional skill missing from resolver response; skipped", installFailMissingURI, "missing from resolver response") {
		t.Errorf("missing skip: expected a Warn naming %s, got %d records", installFailMissingURI, len(*records))
	}
}

// TestInstallResolvedSkills_OptionalCacheFallthroughLeavesNoPartialDir checks
// that an optional skill whose cache copy fails partway and whose download
// then fails is skipped without leaving a half-copied file in skills/<name>,
// and without removing what was already there.
func TestInstallResolvedSkills_OptionalCacheFallthroughLeavesNoPartialDir(t *testing.T) {
	content := []byte("# Cached Skill")
	srv := newInstallFailServer(t, content)
	fileHash := transfer.HashBytes(content)
	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{{Path: "SKILL.md", Hash: fileHash}})

	// A cache entry whose copy fails after SKILL.md: copyDir copies entries
	// in name order, and "zz-broken" is a dangling symlink that cannot be
	// opened (independent of permissions, so it also fails as root).
	cacheDir := t.TempDir()
	cache, err := templatecache.New(cacheDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	cachedPath, err := cache.Put(bundleHash, map[string][]byte{"SKILL.md": content})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cacheDir, "does-not-exist"), filepath.Join(cachedPath, "zz-broken")); err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithSkillCache(context.Background(), cache)

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")
	// A skill already at the destination (e.g. from the template).
	existing := filepath.Join(skillsDest, "cached-skill")
	if err := os.MkdirAll(existing, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "LOCAL.md"), []byte("local"), 0644); err != nil {
		t.Fatal(err)
	}

	skills := []ResolvedSkill{{
		Name:     "cached-skill",
		URI:      "skill://scion/core/cached-skill@1.0",
		Version:  "1.0.0",
		Hash:     bundleHash,
		Optional: true,
		Files:    []ResolvedFile{{Path: "SKILL.md", URL: srv.URL + "/missing/SKILL.md", Hash: fileHash}},
	}}
	_, skipped, err := installResolvedSkills(ctx, skills, skillsDest, agentHome)
	if err != nil {
		t.Fatalf("cache fallthrough: optional skill failure must not fail the install, got: %v", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("cache fallthrough: expected 1 skipped skill, got %+v", skipped)
	}
	if _, err := os.Stat(filepath.Join(existing, "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("cache fallthrough: half-copied SKILL.md left in skills/cached-skill (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(existing, "LOCAL.md")); err != nil {
		t.Errorf("cache fallthrough: pre-existing skill content was removed: %v", err)
	}
}

// TestInstallResolvedSkills_OptionalCollisionWinnerKeepsRequiredLoserRequired
// checks that when an optional skill wins a dest-name collision against a
// required one and then fails to install, the install fails as required
// rather than silently dropping the required declaration.
func TestInstallResolvedSkills_OptionalCollisionWinnerKeepsRequiredLoserRequired(t *testing.T) {
	content := []byte("# Foo")
	srv := newInstallFailServer(t, content)
	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	const requiredURI = "gh://a/repo/foo@main"
	const optionalURI = "gh://b/repo/foo@main"
	skills := []ResolvedSkill{
		{
			Name: "foo", URI: requiredURI, Version: "main", Scope: "hub",
			Files: []ResolvedFile{{Path: "SKILL.md", URL: srv.URL + "/good/SKILL.md", Hash: transfer.HashBytes(content)}},
		},
		{
			Name: "foo", URI: optionalURI, Version: "main", Scope: "project", Optional: true,
			Files: []ResolvedFile{{Path: "SKILL.md", URL: srv.URL + "/missing/SKILL.md"}},
		},
	}
	_, _, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome)
	var skillErr *SkillResolutionError
	if !errors.As(err, &skillErr) {
		t.Fatalf("collision: expected a *SkillResolutionError, got %T: %v", err, err)
	}
	if skillErr.URI != optionalURI || skillErr.Code != SkillErrCodeInstallFailed {
		t.Errorf("collision: got URI %q code %q, want %q %q", skillErr.URI, skillErr.Code, optionalURI, SkillErrCodeInstallFailed)
	}
}

// TestInstallResolvedSkills_TransportErrorScrubsURL checks that a download
// transport failure names neither the URL's path nor its query (a signed
// URL's token) in the user-facing text or in the logged error.
func TestInstallResolvedSkills_TransportErrorScrubsURL(t *testing.T) {
	const secret = "SECRETSIG"
	// Port 1 on loopback: the connection is refused.
	fileURL := "https://127.0.0.1:1/private/path/SKILL.md?sig=" + secret
	newSkills := func(optional bool) []ResolvedSkill {
		return []ResolvedSkill{{
			Name: "scrub-skill", URI: "skill://scion/core/scrub-skill@1.0", Version: "1.0.0", Optional: optional,
			Files: []ResolvedFile{{Path: "SKILL.md", URL: fileURL}},
		}}
	}
	leaks := func(s string) bool { return strings.Contains(s, secret) || strings.Contains(s, "/private/path") }

	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	// Required: the user-facing error.
	_, _, err := installResolvedSkills(context.Background(), newSkills(false), skillsDest, agentHome)
	var skillErr *SkillResolutionError
	if !errors.As(err, &skillErr) {
		t.Fatalf("transport scrub: expected a *SkillResolutionError, got %T: %v", err, err)
	}
	if !strings.HasPrefix(skillErr.Message, "failed to download SKILL.md: download failed: ") {
		t.Errorf("transport scrub: message %q should give the transport cause", skillErr.Message)
	}
	if leaks(skillErr.Message) || leaks(err.Error()) {
		t.Errorf("transport scrub: user-facing error leaks the URL: %q", err.Error())
	}

	// Optional: the logged error.
	records := captureSlog(t)
	if _, _, err := installResolvedSkills(context.Background(), newSkills(true), skillsDest, agentHome); err != nil {
		t.Fatalf("transport scrub: optional skill must be skipped, got: %v", err)
	}
	var logged string
	for _, r := range *records {
		if r.Level != slog.LevelWarn || r.Message != "optional skill could not be installed; skipped" {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "error" {
				logged = a.Value.String()
			}
			return true
		})
	}
	if logged == "" {
		t.Fatal("transport scrub: no skip Warn with an error attr")
	}
	if leaks(logged) {
		t.Errorf("transport scrub: logged error leaks the URL: %q", logged)
	}
}
