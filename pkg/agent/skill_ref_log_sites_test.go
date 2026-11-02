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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
)

// Log the declared skill reference in its redacted form (ptone/scion#5150).
// These tests capture the records written at each call site in this
// package that logs a declared skill reference, render them with slog's
// JSON handler (which resolves LogValuers, as the production handlers do)
// and check the reference and reason attributes. They use the default
// logger through captureSlog, which restores it on cleanup; no test in this
// package runs in parallel.

const (
	// refLogHTTPRef is a declared reference with userinfo and a query.
	refLogHTTPRef = "https://user:x@example.com/skills/s?token=x"
	// refLogHTTPRedacted is its redacted form.
	refLogHTTPRedacted = "https://(redacted URL)"
	// refLogGHRef is a gh:// reference with a query.
	refLogGHRef = "gh://owner/repo/s?token=x"
	// refLogSchemeRedacted is the redacted form of a skill-scheme reference.
	refLogSchemeRedacted = "(redacted URL)"
	// refLogWithheld replaces the reason when the reference is redacted.
	refLogWithheld = "(resolver message withheld)"
	// refLogNameWithheld replaces a skill name when the reference is redacted.
	refLogNameWithheld = "(skill name withheld)"
	// refLogPlainRef is a public reference, logged as it is.
	refLogPlainRef = "gh://owner/repo/plain-skill@v1"
)

// refLogHiddenParts are parts of the test references and messages that
// must not appear in any logged record.
var refLogHiddenParts = []string{"user:x", "token=x", "token=X", "sig=x", "example.com/skills"}

// refLogRender renders r with slog's JSON handler and returns the
// rendered line and its attributes.
func refLogRender(t *testing.T, r slog.Record) (string, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("render record: %v", err)
	}
	attrs := map[string]any{}
	if err := json.Unmarshal(buf.Bytes(), &attrs); err != nil {
		t.Fatalf("parse rendered record %q: %v", buf.String(), err)
	}
	return buf.String(), attrs
}

// refLogFind returns the rendered line and attributes of the first record
// with message msg, failing the test when there is none.
func refLogFind(t *testing.T, records []slog.Record, msg string) (string, map[string]any) {
	t.Helper()
	for _, r := range records {
		if r.Message == msg {
			return refLogRender(t, r)
		}
	}
	t.Fatalf("no record %q among %d records", msg, len(records))
	return "", nil
}

// refLogAssertNotLogged fails when any record holds a hidden part (see
// refLogHiddenParts).
func refLogAssertNotLogged(t *testing.T, records []slog.Record) {
	t.Helper()
	for _, r := range records {
		line, _ := refLogRender(t, r)
		for _, part := range refLogHiddenParts {
			if strings.Contains(line, part) {
				t.Errorf("record %q holds %q: %s", r.Message, part, line)
			}
		}
	}
}

// refLogAssertAttr fails when attrs[key] is not want.
func refLogAssertAttr(t *testing.T, attrs map[string]any, key, want string) {
	t.Helper()
	if got, _ := attrs[key].(string); got != want {
		t.Errorf("attr %s = %q, want %q", key, got, want)
	}
}

// writeRefLogTemplate writes a template that declares the good skill
// (required) and ref, optional or not.
func writeRefLogTemplate(t *testing.T, ref string, optional bool) (projectScionDir string) {
	t.Helper()
	tmpDir := t.TempDir()

	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	tplDir := filepath.Join(globalScionDir, "templates", "ref-log-tpl")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	cfg := map[string]any{
		"default_harness_config": "claude",
		"skills": []map[string]any{
			{"uri": installFailGoodURI},
			{"uri": ref, "optional": optional},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	projectScionDir = filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

func TestSkillRefLog_ProvisionOptionalResolveFailure(t *testing.T) {
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)
	projectScionDir := writeRefLogTemplate(t, refLogHTTPRef, true)

	resolver := &mockResolver{
		resolved: installFailSkills(srv.URL, content, false)[:1],
		errors: []ResolveError{{
			URI: refLogHTTPRef, Code: SkillErrCodeResolveFailed,
			Message: "fetch " + refLogHTTPRef + " failed",
		}},
	}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	records := captureSlog(t)

	if _, _, _, err := ProvisionAgent(ctx, "ref-log-agent", "ref-log-tpl", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent must succeed when only an optional skill fails to resolve, got: %v", err)
	}
	_, attrs := refLogFind(t, *records, "optional skill could not be resolved; skipped")
	refLogAssertAttr(t, attrs, "skill_uri", refLogHTTPRedacted)
	refLogAssertAttr(t, attrs, "reason", refLogWithheld)
	refLogAssertNotLogged(t, *records)
}

// TestSkillRefLog_ProvisionOptionalResolveFailurePlainRef checks that a
// public reference is logged as it is, and that a URL in the resolver's
// reason text (as a wrapped *url.Error quotes it) is redacted.
func TestSkillRefLog_ProvisionOptionalResolveFailurePlainRef(t *testing.T) {
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)
	projectScionDir := writeRefLogTemplate(t, refLogPlainRef, true)

	resolver := &mockResolver{
		resolved: installFailSkills(srv.URL, content, false)[:1],
		errors: []ResolveError{{
			URI: refLogPlainRef, Code: SkillErrCodeResolveFailed,
			Message: `Get "https://example.com/skills/r?sig=x": EOF`,
		}},
	}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	records := captureSlog(t)

	if _, _, _, err := ProvisionAgent(ctx, "ref-log-agent", "ref-log-tpl", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent must succeed when only an optional skill fails to resolve, got: %v", err)
	}
	_, attrs := refLogFind(t, *records, "optional skill could not be resolved; skipped")
	refLogAssertAttr(t, attrs, "skill_uri", refLogPlainRef)
	refLogAssertAttr(t, attrs, "reason", `Get "https://(redacted URL) EOF`)
	refLogAssertNotLogged(t, *records)
}

func TestSkillRefLog_ProvisionOptionalMissingFromResponse(t *testing.T) {
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)
	projectScionDir := writeRefLogTemplate(t, refLogHTTPRef, true)

	resolver := &mockResolver{resolved: installFailSkills(srv.URL, content, false)[:1]}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	records := captureSlog(t)

	if _, _, _, err := ProvisionAgent(ctx, "ref-log-agent", "ref-log-tpl", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent must succeed when an optional skill is missing from the response, got: %v", err)
	}
	_, attrs := refLogFind(t, *records, "optional skill missing from resolver response; skipped")
	refLogAssertAttr(t, attrs, "skill_uri", refLogHTTPRedacted)
	refLogAssertNotLogged(t, *records)
}

// TestSkillRefLog_ProvisionRequiredMissingFromResponse checks the error for
// a required skill the resolver neither resolved nor reported, which the
// agent runtime logs as it is.
func TestSkillRefLog_ProvisionRequiredMissingFromResponse(t *testing.T) {
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)
	projectScionDir := writeRefLogTemplate(t, refLogHTTPRef, false)

	resolver := &mockResolver{resolved: installFailSkills(srv.URL, content, false)[:1]}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	records := captureSlog(t)

	_, _, _, err := ProvisionAgent(ctx, "ref-log-agent", "ref-log-tpl", "", "", projectScionDir, "", "", "", "")
	if err == nil {
		t.Fatal("ProvisionAgent must fail when a required skill is missing from the response")
	}
	if want := `required skill "` + refLogHTTPRedacted + `" missing from resolver response`; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not hold %q", err.Error(), want)
	}
	for _, part := range append([]string{refLogHTTPRef}, refLogHiddenParts...) {
		if strings.Contains(err.Error(), part) {
			t.Errorf("error %q holds %q", err.Error(), part)
		}
	}
	refLogAssertNotLogged(t, *records)
}

func TestSkillRefLog_DestNameCollision(t *testing.T) {
	for _, tc := range []struct {
		name          string
		winnerRef     string
		wantWinner    string
		loserOptional bool
		msg           string
	}{
		{"required loser", refLogGHRef, refLogSchemeRedacted, false, "skill destination-name collision resolved"},
		{"optional loser", refLogGHRef, refLogSchemeRedacted, true, "skill destination-name collision resolved (optional skill dropped)"},
		{"redacted loser only", refLogPlainRef, refLogPlainRef, false, "skill destination-name collision resolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := captureSlog(t)
			skills := []ResolvedSkill{
				{Name: "dup", URI: refLogHTTPRef, Scope: "hub", Optional: tc.loserOptional},
				{Name: "dup", URI: tc.winnerRef, Scope: "template"},
			}
			deduplicateByDestName(context.Background(), skills)

			_, attrs := refLogFind(t, *records, tc.msg)
			refLogAssertAttr(t, attrs, "winner_uri", tc.wantWinner)
			refLogAssertAttr(t, attrs, "dropped_uri", refLogHTTPRedacted)
			refLogAssertAttr(t, attrs, "dest_name", refLogNameWithheld)
			refLogAssertNotLogged(t, *records)
		})
	}
}

func TestSkillRefLog_InstallFailure(t *testing.T) {
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)

	for _, tc := range []struct {
		name     string
		optional bool
		msg      string
	}{
		{"optional", true, "optional skill could not be installed; skipped"},
		{"required", false, "required skill could not be installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentHome := t.TempDir()
			skillsDest := filepath.Join(agentHome, ".claude", "skills")
			records := captureSlog(t)
			skills := []ResolvedSkill{{
				Name: "missing-skill", URI: refLogHTTPRef, Version: "main", Optional: tc.optional,
				Files: []ResolvedFile{{Path: "SKILL.md", URL: srv.URL + "/missing/SKILL.md"}},
			}}
			_, _, _ = installResolvedSkills(context.Background(), skills, skillsDest, agentHome)

			_, attrs := refLogFind(t, *records, tc.msg)
			refLogAssertAttr(t, attrs, "skill_uri", refLogHTTPRedacted)
			refLogAssertAttr(t, attrs, "reason", refLogWithheld)
			refLogAssertAttr(t, attrs, "error", refLogWithheld)
			refLogAssertAttr(t, attrs, "skill_name", refLogNameWithheld)
			refLogAssertNotLogged(t, *records)
		})
	}
}

// TestSkillRefLog_InstallFailureQuotingRef checks an install failure whose
// text quotes the reference (a hash mismatch).
func TestSkillRefLog_InstallFailureQuotingRef(t *testing.T) {
	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")
	records := captureSlog(t)
	skills := []ResolvedSkill{{
		Name: "hash-skill", URI: refLogHTTPRef, Version: "1.0.0",
		Files: []ResolvedFile{{Path: "SKILL.md", Content: []byte("# Hash Skill"), Hash: "sha256:00"}},
	}}
	if _, _, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome); err == nil {
		t.Fatal("a hash mismatch must fail the install")
	}
	_, attrs := refLogFind(t, *records, "required skill could not be installed")
	refLogAssertAttr(t, attrs, "skill_uri", refLogHTTPRedacted)
	refLogAssertAttr(t, attrs, "reason", refLogWithheld)
	refLogAssertAttr(t, attrs, "error", refLogWithheld)
	refLogAssertAttr(t, attrs, "skill_name", refLogNameWithheld)
	refLogAssertNotLogged(t, *records)
}

// TestSkillRefLog_InstallFailurePublicRefURLInText checks an install
// failure of a public reference whose text holds an http(s) URL: the
// reason and error are logged with the URL redacted.
func TestSkillRefLog_InstallFailurePublicRefURLInText(t *testing.T) {
	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")
	records := captureSlog(t)
	skills := []ResolvedSkill{{
		Name: "pub-skill", As: "https://example.com/n?sig=x", URI: refLogPlainRef, Version: "1.0.0", Optional: true,
		Files: []ResolvedFile{{Path: "SKILL.md", Content: []byte("# Skill")}},
	}}
	if _, _, err := installResolvedSkills(context.Background(), skills, skillsDest, agentHome); err != nil {
		t.Fatalf("an optional skill must be skipped, got: %v", err)
	}
	_, attrs := refLogFind(t, *records, "optional skill could not be installed; skipped")
	refLogAssertAttr(t, attrs, "skill_uri", refLogPlainRef)
	refLogAssertAttr(t, attrs, "skill_name", "pub-skill")
	const wantPrefix = `invalid skill destination name "https://(redacted URL) `
	for _, key := range []string{"reason", "error"} {
		if got, _ := attrs[key].(string); !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("attr %s = %q, want prefix %q", key, got, wantPrefix)
		}
	}
	refLogAssertNotLogged(t, *records)
}

// TestSkillRefLog_GitHubStaleServeWarning checks the GitHub resolver's
// stderr line for a stale entry served during a cooldown names the
// reference in its redacted form.
func TestSkillRefLog_GitHubStaleServeWarning(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	serveTestCommit(mux, "owner", "repo", &calls)
	serveTestSkill(mux, "owner", "repo", "s", &calls)
	clock := newFakeClock()
	r := newCooldownTestResolver(t, mux, clock)
	const token = "test-token-x"
	r.provisionCredentials = map[string]string{"X": token}

	const uri = "gh://owner/repo/s@main?token=X"
	ghRef, err := ParseGitHubSkillURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	cacheKey := resolutionCacheKey(ghRef, token)
	now := time.Now()
	r.resolutionCache.mu.Lock()
	r.resolutionCache.entries[cacheKey] = &resolutionCacheEntry{
		Skill: ResolvedSkill{Name: "s", URI: uri, Version: "stale",
			Files: []ResolvedFile{{Path: "SKILL.md", Mode: "0644", Content: []byte("hello")}}},
		CachedAt:    now.Add(-2 * time.Hour),
		ExpiresAt:   now.Add(-time.Minute),
		IsBranchRef: true,
	}
	r.resolutionCache.mu.Unlock()
	r.cooldown.record(GitHubCooldownIdentity(token), clock.Now().Add(time.Minute))

	stderr := refLogCaptureStderr(t, func() {
		res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: uri}}, ResolveOpts{})
		if err != nil {
			t.Errorf("Resolve: %v", err)
			return
		}
		if len(res.Resolved) != 1 || res.Resolved[0].Version != "stale" {
			t.Errorf("expected the stale entry, got %+v", res)
		}
	})
	if want := "serving stale entry for " + refLogSchemeRedacted + " (named)"; !strings.Contains(stderr, want) {
		t.Errorf("stderr %q does not hold %q", stderr, want)
	}
	for _, part := range append([]string{uri}, refLogHiddenParts...) {
		if strings.Contains(stderr, part) {
			t.Errorf("stderr %q holds %q", stderr, part)
		}
	}
}

// TestSkillRefLog_GitHubModelessFallbackWarning checks the GitHub resolver's
// stderr line for a cache entry from before file modes were recorded,
// served because re-resolving it failed (ptone/scion#5781): it names the
// reference in its redacted form and says the files may lack exec bits.
func TestSkillRefLog_GitHubModelessFallbackWarning(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	serveTestCommit(mux, "owner", "repo", &calls)
	serveTestSkill(mux, "owner", "repo", "s", &calls)
	clock := newFakeClock()
	r := newCooldownTestResolver(t, mux, clock)
	const token = "test-token-x"
	r.provisionCredentials = map[string]string{"X": token}

	const uri = "gh://owner/repo/s@main?token=X"
	ghRef, err := ParseGitHubSkillURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	cacheKey := resolutionCacheKey(ghRef, token)
	now := time.Now()
	r.resolutionCache.mu.Lock()
	r.resolutionCache.entries[cacheKey] = &resolutionCacheEntry{
		Skill: ResolvedSkill{Name: "s", URI: uri, Version: "legacy",
			Files: []ResolvedFile{{Path: "SKILL.md", Content: []byte("hello")}}},
		CachedAt:    now.Add(-time.Minute),
		ExpiresAt:   now.Add(time.Hour),
		IsBranchRef: true,
	}
	r.resolutionCache.mu.Unlock()
	r.cooldown.record(GitHubCooldownIdentity(token), clock.Now().Add(time.Minute))

	stderr := refLogCaptureStderr(t, func() {
		res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: uri}}, ResolveOpts{})
		if err != nil {
			t.Errorf("Resolve: %v", err)
			return
		}
		if len(res.Resolved) != 1 || res.Resolved[0].Version != "legacy" {
			t.Errorf("expected the legacy entry, got %+v", res)
		}
	})
	if want := "re-resolving " + refLogSchemeRedacted + " (named) failed"; !strings.Contains(stderr, want) {
		t.Errorf("stderr %q does not hold %q", stderr, want)
	}
	if want := "may lack executable bits"; !strings.Contains(stderr, want) {
		t.Errorf("stderr %q does not hold %q", stderr, want)
	}
	for _, part := range append([]string{uri}, refLogHiddenParts...) {
		if strings.Contains(stderr, part) {
			t.Errorf("stderr %q holds %q", stderr, part)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d GitHub requests during the cooldown, want 0", n)
	}
}

// TestSkillRefLog_ContentCacheRecord checks the content-cache record of an
// install: the skill name is withheld for a redacted reference and logged
// as it is for a public one.
func TestSkillRefLog_ContentCacheRecord(t *testing.T) {
	for _, tc := range []struct {
		name, ref, want string
	}{
		{"redacted ref", refLogHTTPRef, refLogNameWithheld},
		{"public ref", refLogPlainRef, "cache-skill"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, err := templatecache.New(t.TempDir(), 0)
			if err != nil {
				t.Fatalf("templatecache.New: %v", err)
			}
			ctx := ContextWithSkillCache(context.Background(), cache)
			agentHome := t.TempDir()
			skillsDest := filepath.Join(agentHome, ".claude", "skills")
			records := captureSlog(t)
			skills := []ResolvedSkill{{
				Name: "cache-skill", URI: tc.ref, Version: "1.0.0", Hash: "sha256:00", Optional: true,
				Files: []ResolvedFile{{Path: "SKILL.md", Content: []byte("# Cache Skill")}},
			}}
			_, _, _ = installResolvedSkills(ctx, skills, skillsDest, agentHome)

			_, attrs := refLogFind(t, *records, "skill_content_cache: cache miss, will download")
			refLogAssertAttr(t, attrs, "skill", tc.want)
			refLogAssertNotLogged(t, *records)
		})
	}
}

// TestSkillRefLog_GitHubNoTokenWarning checks the GitHub resolver's stderr
// line for an unauthenticated request names the reference in its redacted
// form.
func TestSkillRefLog_GitHubNoTokenWarning(t *testing.T) {
	server, _ := newTestGitHubServer(t)
	r := newTestGitHubResolver(server)
	r.token = ""

	const uri = "HTTPS://github.com/owner/repo/tree/main/skills/s"
	stderr := refLogCaptureStderr(t, func() {
		_, _ = r.Resolve(context.Background(), []api.SkillReference{{URI: uri}}, ResolveOpts{})
	})
	if want := "no GITHUB_TOKEN configured for https://github.com/owner/repo/tree/main/skills/s;"; !strings.Contains(stderr, want) {
		t.Errorf("stderr %q does not hold %q", stderr, want)
	}
	if strings.Contains(stderr, uri) {
		t.Errorf("stderr %q holds the declared reference", stderr)
	}
}

// refLogCaptureStderr returns what fn writes to os.Stderr. It swaps
// os.Stderr for the call; no test in this package runs in parallel.
func refLogCaptureStderr(t *testing.T, fn func()) string {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = pw
	done := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(pr)
		done <- data
	}()
	func() {
		defer func() { os.Stderr = orig }()
		fn()
	}()
	_ = pw.Close()
	data := <-done
	_ = pr.Close()
	return string(data)
}

func TestSkillRefLog_GitHubPanicOutcome(t *testing.T) {
	for _, tc := range []struct {
		ref, want string
	}{
		{refLogGHRef, refLogSchemeRedacted},
		{refLogPlainRef, refLogPlainRef},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			records := captureSlog(t)
			panicOutcome(api.SkillReference{URI: tc.ref}, "boom")

			_, attrs := refLogFind(t, *records, "github: panic during skill resolution")
			refLogAssertAttr(t, attrs, "uri", tc.want)
			refLogAssertNotLogged(t, *records)
		})
	}
}
