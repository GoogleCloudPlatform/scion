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

package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Decision log tests (ptone/scion#5120).

// logCapture records the decision log as JSON lines.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// take returns the entries written since the last take, and the raw text.
func (c *logCapture) take(t *testing.T) ([]map[string]any, string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	raw := c.buf.String()
	c.buf.Reset()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out, raw
}

// captureLog sets f's decision logger to a capture at level.
func captureLog(f *fixture, level slog.Level) *logCapture {
	c := &logCapture{}
	l := slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: level}))
	f.svc.SetLogger(func() *slog.Logger { return l })
	return c
}

// one returns the only entry, failing unless there is exactly one.
func one(t *testing.T, what string, entries []map[string]any) map[string]any {
	t.Helper()
	if len(entries) != 1 {
		t.Fatalf("%s: %d entries, want 1: %v", what, len(entries), entries)
	}
	return entries[0]
}

// checkEntry checks e's level and fields. A want value of nil requires
// the field to be absent.
func checkEntry(t *testing.T, what string, e map[string]any, level string, want map[string]any) {
	t.Helper()
	if e["level"] != level {
		t.Errorf("%s: level %v, want %s", what, e["level"], level)
	}
	for k, v := range want {
		got, ok := e[k]
		switch {
		case v == nil && ok:
			t.Errorf("%s: field %s = %v, want absent", what, k, got)
		case v != nil && !ok:
			t.Errorf("%s: field %s missing (entry %v)", what, k, e)
		case v != nil && got != v:
			t.Errorf("%s: field %s = %v (%T), want %v (%T)", what, k, got, got, v, v)
		}
	}
}

func jsonDo(f *fixture, p principal, method, target, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(&p, method, target, []byte(body), map[string]string{"Content-Type": "application/json"})
}

// TestDecisionLogLifecycleEvents: each lifecycle decision writes exactly
// one INFO entry with the issue's fields.
func TestDecisionLogLifecycleEvents(t *testing.T) {
	f, _ := newLinkFixture(t)
	c := captureLog(f, slog.LevelDebug)
	user := map[string]any{logKeyPrincipalKind: PrincipalKindUser, logKeyPrincipalID: userU.ref}

	// Single-file publish.
	a := f.publish(userU, "a.md", []byte("# a"), "scope=project-1").Artifact
	id := a.ID
	entries, _ := c.take(t)
	checkEntry(t, "publish", one(t, "publish", entries), "INFO", merge(user, map[string]any{
		logKeyAction: logActionPublished, logKeyOutcome: logOutcomeAllowed, logKeyArtifactID: id,
		logKeyVersionSeq: 1.0, logKeyProjectID: "project-1", logKeyRoute: "POST /api/v1/artifacts",
		logKeyAuthority: logAuthorityOwner, logKeyOwnerKind: PrincipalKindUser, logKeyOrphaned: false,
		logKeyReason: nil,
	}))

	// Two-step publish: created, then finalized.
	files := bundle{"index.md": []byte("# b")}
	pend := f.createPending(agentA, "/api/v1/artifacts", files.manifest("index.md"))
	entries, _ = c.take(t)
	checkEntry(t, "create", one(t, "create", entries), "INFO", map[string]any{
		logKeyAction: logActionCreated, logKeyArtifactID: pend.Artifact.ID, logKeyVersionSeq: 1.0,
		logKeyPrincipalKind: PrincipalKindAgent, logKeyPrincipalID: agentA.ref, logKeyAuthority: logAuthorityOwner,
		logKeyOwnerKind: PrincipalKindAgent, logKeyProjectID: "project-1",
	})
	if rec := f.put(agentA, pend.Artifact.ID, 1, "index.md", files["index.md"]); rec.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if entries, _ = c.take(t); len(entries) != 0 {
		t.Errorf("an upload is not a decision: %v", entries)
	}
	if rec := f.finalize(agentA, pend.Artifact.ID, 1); rec.Code != http.StatusOK {
		t.Fatalf("finalize: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ = c.take(t)
	checkEntry(t, "finalize", one(t, "finalize", entries), "INFO", map[string]any{
		logKeyAction: logActionVersionFinalized, logKeyVersionSeq: 1.0, logKeyAuthority: logAuthorityOwner,
		logKeyRoute: "POST /api/v1/artifacts/{id}/versions/{seq}/finalize",
	})

	// Appended version.
	f.createPending(userU, "/api/v1/artifacts/"+id+"/versions", bundle{"a.md": []byte("# a2")}.manifest("a.md"))
	entries, _ = c.take(t)
	checkEntry(t, "append", one(t, "append", entries), "INFO", merge(user, map[string]any{
		logKeyAction: logActionVersionCreated, logKeyVersionSeq: 2.0, logKeyRoute: "POST /api/v1/artifacts/{id}/versions",
	}))

	// Grant created, then changed, then removed.
	if rec := jsonDo(f, userU, http.MethodPost, grantsPath(id), reviewBody(SubjectPrincipal, "user:user-g", GrantRead)); rec.Code != http.StatusCreated {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ = c.take(t)
	checkEntry(t, "grant created", one(t, "grant created", entries), "INFO", merge(user, map[string]any{
		logKeyAction: logActionGrantCreated, logKeyAuthority: logAuthorityOwner, logKeySubjectKind: SubjectPrincipal,
		logKeySubjectRef: "user:user-g", logKeyPermission: GrantRead, logKeyRoute: "POST /api/v1/artifacts/{id}/grants",
	}))
	rec := jsonDo(f, userU, http.MethodPost, grantsPath(id), reviewBody(SubjectPrincipal, "user:user-g", GrantWrite))
	if rec.Code != http.StatusOK {
		t.Fatalf("grant change: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ = c.take(t)
	checkEntry(t, "grant changed", one(t, "grant changed", entries), "INFO", map[string]any{
		logKeyAction: logActionGrantChanged, logKeyPermission: GrantWrite, logKeySubjectRef: "user:user-g",
	})
	grantID := decodeInto[GrantResponse](t, rec).Grant.ID
	if rec := f.do(&userU, http.MethodDelete, grantsPath(id)+"/"+grantID, nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("grant delete: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ = c.take(t)
	checkEntry(t, "grant removed", one(t, "grant removed", entries), "INFO", map[string]any{
		logKeyAction: logActionGrantRemoved, logKeyAuthority: logAuthorityOwner, logKeySubjectKind: SubjectPrincipal,
		logKeySubjectRef: "user:user-g", logKeyPermission: GrantWrite, logKeyRoute: "DELETE /api/v1/artifacts/{id}/grants/{grantId}",
	})

	// Link created and revoked: the kind only, never a ref.
	_, link := f.mustMintLink(userU, id, "")
	entries, _ = c.take(t)
	checkEntry(t, "link created", one(t, "link created", entries), "INFO", map[string]any{
		logKeyAction: logActionLinkCreated, logKeySubjectKind: SubjectLink, logKeySubjectRef: nil,
		logKeyAuthority: logAuthorityOwner, logKeyRoute: "POST /api/v1/artifacts/{id}/links",
	})
	if rec := f.do(&userU, http.MethodDelete, linksPath(id)+"/"+link.Link.ID, nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ = c.take(t)
	checkEntry(t, "link revoked", one(t, "link revoked", entries), "INFO", map[string]any{
		logKeyAction: logActionLinkRevoked, logKeySubjectKind: SubjectLink, logKeySubjectRef: nil,
		logKeyRoute: "DELETE /api/v1/artifacts/{id}/links/{linkId}",
	})

	// Expiry set and cleared.
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if rec := jsonDo(f, userU, http.MethodPatch, artifactPath(id), `{"expiresAt":"`+future+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("expiry: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ = c.take(t)
	checkEntry(t, "expiry set", one(t, "expiry set", entries), "INFO", map[string]any{
		logKeyAction: logActionExpirySet, logKeyAuthority: logAuthorityOwner, logKeyRoute: "PATCH /api/v1/artifacts/{id}",
	})
	if rec := jsonDo(f, userU, http.MethodPatch, artifactPath(id), `{"expiresAt":null}`); rec.Code != http.StatusOK {
		t.Fatalf("expiry clear: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ = c.take(t)
	checkEntry(t, "expiry cleared", one(t, "expiry cleared", entries), "INFO", map[string]any{logKeyAction: logActionExpiryCleared})

	// Move: logged in the old home, with the new home as its subject.
	f.host.crossScope = true
	f.host.allow(userU, "project-2", PermissionRead, PermissionCreate)
	if rec := jsonDo(f, userU, http.MethodPatch, artifactPath(id), `{"scopeRef":"project-2"}`); rec.Code != http.StatusOK {
		t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ = c.take(t)
	checkEntry(t, "move", one(t, "move", entries), "INFO", map[string]any{
		logKeyAction: logActionMoved, logKeyProjectID: "project-1", logKeySubjectKind: SubjectScope,
		logKeySubjectRef: "project-2", logKeyPermission: GrantRead, logKeyAuthority: logAuthorityOwner,
	})

	// Delete.
	if rec := f.do(&userU, http.MethodDelete, artifactPath(id), nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ = c.take(t)
	checkEntry(t, "delete", one(t, "delete", entries), "INFO", merge(user, map[string]any{
		logKeyAction: logActionDeleted, logKeyArtifactID: id, logKeyProjectID: "project-2",
		logKeyAuthority: logAuthorityOwner, logKeyRoute: "DELETE /api/v1/artifacts/{id}",
	}))
}

// TestDecisionLogReviewGrant: a review grant is logged with the clause that
// allowed it: steward_delegator for the owning agent's delegating user,
// steward_project_owner for an owner or administrator of the home project.
func TestDecisionLogReviewGrant(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	f.host.allowAdmin(projAdmin, "project-1")
	c := captureLog(f, slog.LevelDebug)
	memberID := PrincipalRef(member.kind, member.ref)
	for _, tc := range []struct {
		caller    principal
		subject   string
		authority string
	}{
		{delegator, reviewerID, reviewGrantDelegatorAuthority},
		{projAdmin, memberID, reviewGrantProjectAdminAuthority},
	} {
		f.host.mu.Lock()
		f.host.calls = nil
		f.host.mu.Unlock()
		if rec := f.postGrantBody(tc.caller, id, reviewBody(SubjectPrincipal, tc.subject, GrantReview)); rec.Code != http.StatusCreated {
			t.Fatalf("review grant by %s: %d %s", tc.caller.ref, rec.Code, rec.Body.String())
		}
		entries, _ := c.take(t)
		checkEntry(t, "review grant by "+tc.caller.ref, one(t, "review grant", entries), "INFO", map[string]any{
			logKeyAction: logActionGrantCreated, logKeyAuthority: tc.authority,
			logKeyPrincipalID: tc.caller.ref, logKeySubjectKind: SubjectPrincipal, logKeySubjectRef: tc.subject,
			logKeyPermission: GrantReview, logKeyOwnerKind: PrincipalKindAgent,
		})
		// The clause comes from the host's one review evaluation: no second
		// question (no AdministersScope call) is asked for the log.
		f.host.mu.Lock()
		reviews, administers := 0, 0
		for _, call := range f.host.calls {
			switch {
			case strings.HasPrefix(call, "review "):
				reviews++
			case strings.HasPrefix(call, "administers "):
				administers++
			}
		}
		f.host.mu.Unlock()
		if reviews != 1 || administers != 0 {
			t.Errorf("grant by %s asked review %d times and AdministersScope %d times, want 1 and 0", tc.caller.ref, reviews, administers)
		}
	}
}

// TestDecisionLogRefusals: refusals on management and write routes are
// logged at WARN with a reason from the enum.
func TestDecisionLogRefusals(t *testing.T) {
	f, id := newLinkFixture(t)
	c := captureLog(f, slog.LevelInfo)
	reader := principal{PrincipalKindUser, "user-reader", ""}
	f.host.allow(reader, "project-1", PermissionRead)
	// A review grantee that reads the artifact through its home project, a
	// writer that did not start the pending version, a credential that
	// does not permit publishing in project-2.
	reviewGrantee := principal{PrincipalKindUser, "user-review", ""}
	f.host.allow(reviewGrantee, "project-1", PermissionRead)
	insertGrant(f, id, SubjectPrincipal, PrincipalRef(reviewGrantee.kind, reviewGrantee.ref), GrantReview, nil)
	insertGrant(f, id, SubjectPrincipal, PrincipalRef(agentA.kind, agentA.ref), GrantWrite, nil)
	pending := f.createPending(userU, "/api/v1/artifacts/"+id+"/versions", bundle{"doc.md": []byte("# v2")}.manifest("doc.md"))
	f.host.deny(agentX, "project-2", PermissionCreate)
	c.take(t)
	manifest := `{"entry":"a.md","files":[{"path":"a.md","size":1,"sha256":"` + sha([]byte("x")) + `"}]}`
	scope64 := strings.Repeat("a", 64)
	noManage := principal{PrincipalKindUser, "user-no-manage", ""}
	f.host.allow(noManage, "project-1", PermissionRead)
	f.host.deny(noManage, "project-1", PermissionManage)
	agentC := principal{PrincipalKindAgent, "agent-c-uuid", "project-1"}
	f.host.allow(agentC, "project-1", PermissionRead)
	insertGrant(f, id, SubjectPrincipal, PrincipalRef(agentC.kind, agentC.ref), GrantWrite, nil)
	f.host.deny(agentC, "project-1", PermissionCreate)

	cases := []struct {
		name     string
		p        principal
		method   string
		target   string
		body     string
		status   int
		action   string
		reason   string
		route    string
		artifact bool
	}{
		{"agent administers", agentB, http.MethodPatch, artifactPath(id), `{"expiresAt":null}`, http.StatusForbidden,
			logActionAdminister, logReasonAgentCannotAdminister, "PATCH /api/v1/artifacts/{id}", true},
		{"reader administers", reader, http.MethodPost, linksPath(id), ``, http.StatusForbidden,
			logActionAdminister, logReasonNotAdministrator, "POST /api/v1/artifacts/{id}/links", true},
		{"reader deletes", reader, http.MethodDelete, artifactPath(id), ``, http.StatusForbidden,
			logActionDelete, logReasonNotCustodian, "DELETE /api/v1/artifacts/{id}", true},
		{"reader writes", agentB, http.MethodPost, "/api/v1/artifacts/" + id + "/versions",
			`{"entry":"a.md","files":[{"path":"a.md","size":1,"sha256":"` + sha([]byte("x")) + `"}]}`, http.StatusForbidden,
			logActionWrite, logReasonNoWriteAccess, "POST /api/v1/artifacts/{id}/versions", true},
		{"publish outside scope", agentA, http.MethodPost, "/api/v1/artifacts?name=x.md&scope=project-2", `x`, http.StatusForbidden,
			logActionCreate, logReasonNotAuthorizedInScope, "POST /api/v1/artifacts", false},
		{"cross-project share", userU, http.MethodPost, grantsPath(id), reviewBody(SubjectScope, "project-2", GrantRead), http.StatusForbidden,
			logActionShare, logReasonCrossProjectDisabled, "POST /api/v1/artifacts/{id}/grants", true},
		{"review grantee publishes", reviewGrantee, http.MethodPost, "/api/v1/artifacts/" + id + "/versions", manifest, http.StatusForbidden,
			logActionWrite, logReasonReviewOnly, "POST /api/v1/artifacts/{id}/versions", true},
		{"writer uploads to another's version", agentA, http.MethodPut, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/files/doc.md", id, pending.Version.Seq), "# v2", http.StatusForbidden,
			logActionWrite, logReasonNotVersionPublisher, "PUT /api/v1/artifacts/{id}/versions/{seq}/files/{path}", true},
		{"credential does not permit publishing", agentX, http.MethodPost, "/api/v1/artifacts?name=x.md&scope=project-2", `x`, http.StatusForbidden,
			logActionCreate, logReasonCredentialNotPermit, "POST /api/v1/artifacts", false},
		{"publish into a 64-byte scope", agentA, http.MethodPost, "/api/v1/artifacts?name=x.md&scope=" + scope64, `x`, http.StatusForbidden,
			logActionCreate, logReasonNotAuthorizedInScope, "POST /api/v1/artifacts", false},
		{"publish into a 65-byte scope", agentA, http.MethodPost, "/api/v1/artifacts?name=x.md&scope=" + scope64 + "a", `x`, http.StatusForbidden,
			logActionCreate, logReasonNotAuthorizedInScope, "POST /api/v1/artifacts", false},
		{"publish into a scope with a disallowed character", agentA, http.MethodPost, "/api/v1/artifacts?name=x.md&scope=" + url.QueryEscape("project.2"), `x`, http.StatusForbidden,
			logActionCreate, logReasonNotAuthorizedInScope, "POST /api/v1/artifacts", false},
		{"reader whose credential does not permit managing", noManage, http.MethodPatch, artifactPath(id), `{"expiresAt":null}`, http.StatusForbidden,
			logActionAdminister, logReasonCredentialNotPermit, "PATCH /api/v1/artifacts/{id}", true},
		{"agent whose credential does not permit publishing versions", agentC, http.MethodPost, "/api/v1/artifacts/" + id + "/versions", manifest, http.StatusForbidden,
			logActionWrite, logReasonCredentialNotPermit, "POST /api/v1/artifacts/{id}/versions", true},
		{"publish into a garbage scope", agentA, http.MethodPost, "/api/v1/artifacts?name=x.md&scope=" + url.QueryEscape("forged\nlabel="+strings.Repeat("x", 300)), `x`, http.StatusForbidden,
			logActionCreate, logReasonNotAuthorizedInScope, "POST /api/v1/artifacts", false},
		{"move into a project the owner cannot publish in", userU, http.MethodPatch, artifactPath(id), `{"scopeRef":"project-3"}`, http.StatusForbidden,
			logActionMove, logReasonNotAuthorizedInScope, "PATCH /api/v1/artifacts/{id}", true},
	}
	for _, tc := range cases {
		rec := jsonDo(f, tc.p, tc.method, tc.target, tc.body)
		if rec.Code != tc.status {
			t.Fatalf("%s: status %d, want %d: %s", tc.name, rec.Code, tc.status, rec.Body.String())
		}
		entries, _ := c.take(t)
		want := map[string]any{
			logKeyAction: tc.action, logKeyOutcome: logOutcomeRefused, logKeyReason: tc.reason, logKeyRoute: tc.route,
			logKeyPrincipalKind: tc.p.kind, logKeyPrincipalID: tc.p.ref,
		}
		if tc.artifact {
			want[logKeyArtifactID] = id
			want[logKeyProjectID] = "project-1"
			want[logKeyOwnerKind] = PrincipalKindUser
		} else {
			want[logKeyArtifactID] = nil
			want[logKeyProjectID] = "project-2"
			// Caller text is logged as the project only with a project id's shape.
			switch tc.name {
			case "publish into a 64-byte scope":
				want[logKeyProjectID] = scope64
			case "publish into a garbage scope", "publish into a 65-byte scope", "publish into a scope with a disallowed character":
				want[logKeyProjectID] = nil
			}
		}
		checkEntry(t, tc.name, one(t, tc.name, entries), "WARN", want)
	}
}

// TestDecisionLogReadsAtDebugOnly: a read answered with the 404 of a
// hidden or missing artifact is logged at DEBUG only, and a successful
// read is not logged at all, so reads add no INFO volume.
func TestDecisionLogReadsAtDebugOnly(t *testing.T) {
	f, id := newLinkFixture(t)
	c := captureLog(f, slog.LevelInfo)
	missing := "00000000-0000-4000-8000-000000000000"
	for _, target := range []string{artifactPath(id), artifactPath(missing)} {
		if rec := f.do(&outside, http.MethodGet, target, nil, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s: %d", target, rec.Code)
		}
	}
	if rec := f.do(&userU, http.MethodGet, artifactPath(id), nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner GET: %d", rec.Code)
	}
	if entries, raw := c.take(t); len(entries) != 0 {
		t.Fatalf("reads logged at INFO: %s", raw)
	}

	c = captureLog(f, slog.LevelDebug)
	f.do(&outside, http.MethodGet, artifactPath(id), nil, nil)
	entries, _ := c.take(t)
	checkEntry(t, "hidden", one(t, "hidden", entries), "DEBUG", map[string]any{
		logKeyAction: logActionRead, logKeyOutcome: logOutcomeRefused, logKeyReason: logReasonHidden,
		logKeyArtifactID: id, logKeyRoute: "GET /api/v1/artifacts/{id}", logKeyPrincipalID: outside.ref,
	})
	f.do(&outside, http.MethodGet, artifactPath(missing)+"/files/x.md", nil, nil)
	entries, _ = c.take(t)
	checkEntry(t, "missing", one(t, "missing", entries), "DEBUG", map[string]any{
		logKeyReason: logReasonNotFound, logKeyArtifactID: missing, logKeyRoute: "GET /api/v1/artifacts/{id}/files/{path}",
	})
	if rec := f.do(&userU, http.MethodGet, artifactPath(id), nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner GET: %d", rec.Code)
	}
	if entries, raw := c.take(t); len(entries) != 0 {
		t.Errorf("a successful read is logged: %s", raw)
	}
}

// TestDecisionLogNeverLeaks feeds known sentinel values through publish
// (title, note, content, file name), link creation (the token and its
// hash, a shared read) and grants (credentials in headers) and asserts
// none reaches any entry.
func TestDecisionLogNeverLeaks(t *testing.T) {
	f, _ := newLinkFixture(t)
	c := captureLog(f, slog.LevelDebug)
	const (
		title   = "SENTINEL-TITLE-7f3a"
		note    = "SENTINEL-NOTE-91bc"
		content = "SENTINEL-CONTENT-44de"
		cred    = "SENTINEL-CREDENTIAL-0a1b"
	)
	hdr := map[string]string{"Content-Type": "application/octet-stream", "Authorization": "Bearer " + cred, "Cookie": "session=" + cred}

	// Single-file publish with a title.
	rec := f.do(&userU, http.MethodPost, "/api/v1/artifacts?name=leak.md&scope=project-1&title="+title, []byte("# "+content), hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	id := decodeInto[ArtifactResponse](t, rec).Artifact.ID

	// Two-step publish with a title and a note.
	files := bundle{"page.md": []byte(content)}
	req := files.manifest("page.md")
	req.Title, req.Note, req.Scope = title, note, "project-1"
	f.publishBundle(userU, "/api/v1/artifacts", req, files)

	// A link, read through its token, then revoked.
	token, link := f.mustMintLink(userU, id, "")
	f.shared(RouteShared+token, "192.0.2.1:1234")
	f.shared(RouteShared+token+"/files/leak.md", "192.0.2.1:1234")
	f.shared(RouteShared+"A"+token[1:], "192.0.2.1:1234")
	f.do(&userU, http.MethodDelete, linksPath(id)+"/"+link.Link.ID, nil, hdr)

	// Grants, with the credential headers.
	b, _ := json.Marshal(GrantRequest{SubjectKind: SubjectPrincipal, SubjectRef: "user:user-g", Permission: GrantRead})
	f.do(&userU, http.MethodPost, grantsPath(id), b, map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + cred})
	// A refused grant from an outsider, and a hidden read.
	f.do(&outside, http.MethodPost, grantsPath(id), b, map[string]string{"Authorization": "Bearer " + cred})

	entries, raw := c.take(t)
	if len(entries) < 6 {
		t.Fatalf("expected the publishes, link and grant entries, got %d: %s", len(entries), raw)
	}
	for _, s := range []string{title, note, content, cred, token, LinkTokenHash(token), link.Link.ID, "leak.md", "page.md"} {
		if strings.Contains(raw, s) {
			t.Errorf("decision log contains %q:\n%s", s, raw)
		}
	}
}

// TestRouteTemplate: the route never carries a segment of the request
// path.
func TestRouteTemplate(t *testing.T) {
	id := "11111111-2222-4333-8444-555555555555"
	for target, want := range map[string]string{
		"/api/v1/artifacts":                                      "/api/v1/artifacts",
		"/api/v1/artifacts?name=a.md":                            "/api/v1/artifacts",
		"/api/v1/artifacts/" + id:                                "/api/v1/artifacts/{id}",
		"/api/v1/artifacts/" + id + "/files/secret/name.md":      "/api/v1/artifacts/{id}/files/{path}",
		"/api/v1/artifacts/" + id + "/grants":                    "/api/v1/artifacts/{id}/grants",
		"/api/v1/artifacts/" + id + "/grants/g-1":                "/api/v1/artifacts/{id}/grants/{grantId}",
		"/api/v1/artifacts/" + id + "/links":                     "/api/v1/artifacts/{id}/links",
		"/api/v1/artifacts/" + id + "/links/l-1":                 "/api/v1/artifacts/{id}/links/{linkId}",
		"/api/v1/artifacts/" + id + "/versions":                  "/api/v1/artifacts/{id}/versions",
		"/api/v1/artifacts/" + id + "/versions/3":                "/api/v1/artifacts/{id}/versions/{seq}",
		"/api/v1/artifacts/" + id + "/versions/3/finalize":       "/api/v1/artifacts/{id}/versions/{seq}/finalize",
		"/api/v1/artifacts/" + id + "/versions/3/view":           "/api/v1/artifacts/{id}/versions/{seq}/view",
		"/api/v1/artifacts/" + id + "/versions/3/files/a/b.png":  "/api/v1/artifacts/{id}/versions/{seq}/files/{path}",
		"/api/v1/artifacts/" + id + "/versions/3/secret-segment": "/api/v1/artifacts/{id}/unknown",
		"/api/v1/artifacts/shared/TOKEN-SECRET/files/x.md":       "/api/v1/artifacts/shared/{token}",
		"/api/v1/artifacts/view/CAP-SECRET/x.html":               "/api/v1/artifacts/view/{capability}",
		"/elsewhere/x": "unknown",
	} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if got := routeTemplate(r); got != want {
			t.Errorf("routeTemplate(%s) = %q, want %q", target, got, want)
		}
	}
}

// TestMaintenanceLog: the owner liveness pass and the expiry sweep write
// one entry per artifact they change and a per-run summary.
func TestMaintenanceLog(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	c := &logCapture{}
	l := slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug}))

	gone1 := seedOwned(t, f.store, PrincipalKindAgent, "agent-gone")
	gone2 := seedOwned(t, f.store, PrincipalKindAgent, "agent-gone")
	checker := newFakeChecker()
	checker.gone["agent:agent-gone"] = true
	if _, err := ReconcileOwners(ctx, f.store, checker, time.Now(), l); err != nil {
		t.Fatal(err)
	}
	entries, _ := c.take(t)
	if len(entries) != 3 {
		t.Fatalf("owner pass: %d entries, want 2 marks and a summary: %v", len(entries), entries)
	}
	marked := map[any]bool{}
	for _, e := range entries[:2] {
		checkEntry(t, "orphaned", e, "INFO", map[string]any{
			logKeyAction: logActionOrphaned, logKeyOutcome: logOutcomeAllowed, logKeyOwnerKind: PrincipalKindAgent,
			logKeyOrphaned: true, logKeyPrincipalKind: ActorKindSystem, logKeyAuthority: AuthorityReconciler, logKeyRoute: nil, logKeyProjectID: "project-1",
		})
		marked[e[logKeyArtifactID]] = true
	}
	if !marked[gone1.ID] || !marked[gone2.ID] {
		t.Errorf("marked %v, want %s and %s", marked, gone1.ID, gone2.ID)
	}
	checkEntry(t, "owner pass", entries[2], "INFO", map[string]any{
		logKeyAction: logActionOwnerPass, logKeyOutcome: logOutcomeAllowed, logKeyOrphanedCount: 2.0,
		logKeyUnorphanedCount: 0.0, logKeyOwnersChecked: 1.0,
	})

	// Back again: unorphaned.
	delete(checker.gone, "agent:agent-gone")
	checker.live["agent:agent-gone"] = true
	if _, err := ReconcileOwners(ctx, f.store, checker, time.Now(), l); err != nil {
		t.Fatal(err)
	}
	entries, _ = c.take(t)
	if len(entries) != 3 || entries[0][logKeyAction] != logActionUnorphaned || entries[0][logKeyOrphaned] != false {
		t.Errorf("unorphan pass entries: %v", entries)
	}

	// A pass that changes no mark writes nothing.
	if _, err := ReconcileOwners(ctx, f.store, checker, time.Now(), l); err != nil {
		t.Fatal(err)
	}
	if entries, raw := c.take(t); len(entries) != 0 {
		t.Errorf("a pass that changed nothing logged: %s", raw)
	}

	// A failing checker: a WARN summary with the reason.
	checker.failOn = len(checker.calls)
	if _, err := ReconcileOwners(ctx, f.store, checker, time.Now(), l); err == nil {
		t.Fatal("want a checker error")
	}
	entries, _ = c.take(t)
	checkEntry(t, "failed pass", one(t, "failed pass", entries), "WARN", map[string]any{
		logKeyAction: logActionOwnerPass, logKeyOutcome: logOutcomeError, logKeyReason: logReasonCheckerError,
	})

	// Expiry sweep.
	past := time.Now().Add(-time.Hour)
	if _, err := f.store.SetExpiry(ctx, gone1.ID, &past, testActor); err != nil {
		t.Fatal(err)
	}
	c.take(t)
	n, err := SweepExpired(ctx, f.store, time.Now(), 10, l)
	if err != nil || n != 1 {
		t.Fatalf("SweepExpired = %d, %v", n, err)
	}
	entries, _ = c.take(t)
	if len(entries) != 2 {
		t.Fatalf("sweep: %d entries, want 1 and a summary: %v", len(entries), entries)
	}
	checkEntry(t, "retention delete", entries[0], "INFO", map[string]any{
		logKeyAction: logActionDeleted, logKeyArtifactID: gone1.ID, logKeyPrincipalKind: ActorKindSystem, logKeyProjectID: "project-1",
		logKeyPrincipalID: AuthorityRetention, logKeyAuthority: AuthorityRetention,
	})
	checkEntry(t, "retention pass", entries[1], "INFO", map[string]any{logKeyAction: logActionRetentionPass, logKeyCount: 1.0})
	if n, _ := SweepExpired(ctx, f.store, time.Now(), 10, l); n != 0 {
		t.Fatalf("second sweep = %d", n)
	}
	if entries, raw := c.take(t); len(entries) != 0 {
		t.Errorf("an empty sweep logs nothing: %s", raw)
	}
}

// TestDecisionLogDefaultsToDiscard: a service without a logger, or with
// one that answers nil, still serves.
func TestDecisionLogDefaultsToDiscard(t *testing.T) {
	f := newFixture(t, false)
	f.publish(userU, "a.md", []byte("a"), "scope=project-1")
	f.svc.SetLogger(func() *slog.Logger { return nil })
	f.publish(userU, "b.md", []byte("b"), "scope=project-1")
}

// TestDecisionLogKeysMatchLabels pins that the keys pkg/util/logging
// promotes to labels are the keys the service writes.
func TestDecisionLogKeysMatchLabels(t *testing.T) {
	for got, want := range map[string]string{
		logKeyArtifactID:    logging.AttrArtifactID,
		logKeyProjectID:     logging.AttrProjectID,
		logKeyAction:        logging.AttrArtifactAction,
		logKeyOutcome:       logging.AttrArtifactResult,
		logKeyReason:        logging.AttrArtifactReason,
		logKeyPrincipalKind: logging.AttrPrincipalKind,
		logKeyPrincipalID:   logging.AttrPrincipalID,
	} {
		if got != want {
			t.Errorf("artifacts key %q, logging key %q", got, want)
		}
	}
}

func merge(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// failingStore fails every request-driven version write, as a store outage
// would.
type failingStore struct{ Store }

var errStoreDown = errors.New("store down")

func (failingStore) CreatePublished(context.Context, *Artifact, *Version, []File, []Grant) error {
	return errStoreDown
}
func (failingStore) CreatePending(context.Context, *Artifact, *Version, []File, []Grant) error {
	return errStoreDown
}
func (failingStore) CreateVersion(context.Context, *Version, []File, int) error { return errStoreDown }
func (failingStore) FinalizeVersion(context.Context, string, int, time.Time, []File, int) (*Artifact, error) {
	return nil, errStoreDown
}

// TestDecisionLogVersionFailed: a version that could not be recorded or
// finalized is logged as version_failed with outcome error and reason
// store_error at each write site; a discarded review with outcome refused
// and its discard reason.
func TestDecisionLogVersionFailed(t *testing.T) {
	t.Run("store errors", func(t *testing.T) {
		f := newFixture(t, false)
		files := bundle{"a.md": []byte("# a")}
		existing := f.publish(userU, "e.md", []byte("# e"), "scope=project-1").Artifact.ID
		manifest := files.manifest("a.md")
		manifest.Scope = "project-1"
		pend := f.createPending(userU, "/api/v1/artifacts", manifest)
		if rec := f.put(userU, pend.Artifact.ID, 1, "a.md", files["a.md"]); rec.Code != http.StatusNoContent {
			t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
		}
		c := captureLog(f, slog.LevelDebug)
		f.svc.SetStore(failingStore{f.store})
		for _, tc := range []struct {
			name  string
			do    func() *httptest.ResponseRecorder
			id    string
			seq   any
			route string
		}{
			{"single-file publish", func() *httptest.ResponseRecorder {
				return f.do(&userU, http.MethodPost, "/api/v1/artifacts?name=b.md&scope=project-1", []byte("# b"), nil)
			}, "", 1.0, "POST /api/v1/artifacts"},
			{"manifest create", func() *httptest.ResponseRecorder { return f.postJSON(&userU, "/api/v1/artifacts", manifest) },
				"", 1.0, "POST /api/v1/artifacts"},
			{"appended version", func() *httptest.ResponseRecorder {
				return f.postJSON(&userU, "/api/v1/artifacts/"+existing+"/versions", files.manifest("a.md"))
			}, existing, nil, "POST /api/v1/artifacts/{id}/versions"},
			{"finalize", func() *httptest.ResponseRecorder { return f.finalize(userU, pend.Artifact.ID, 1) },
				pend.Artifact.ID, 1.0, "POST /api/v1/artifacts/{id}/versions/{seq}/finalize"},
		} {
			if rec := tc.do(); rec.Code != http.StatusInternalServerError {
				t.Fatalf("%s: status %d, want 500: %s", tc.name, rec.Code, rec.Body.String())
			}
			entries, _ := c.take(t)
			e := one(t, tc.name, entries)
			want := map[string]any{
				logKeyAction: logActionVersionFailed, logKeyOutcome: logOutcomeError, logKeyReason: logReasonStoreError,
				logKeyRoute: tc.route, logKeyProjectID: "project-1", logKeyPrincipalID: userU.ref, logKeyVersionSeq: tc.seq,
			}
			if tc.id != "" {
				want[logKeyArtifactID] = tc.id
			} else if id, _ := e[logKeyArtifactID].(string); id == "" {
				t.Errorf("%s: no artifact_id", tc.name)
			}
			checkEntry(t, tc.name, e, "INFO", want)
		}
	})
	t.Run("discarded review", func(t *testing.T) {
		f := newReviewFixture(t)
		seq := f.startReview(userU, with(f.v1, "plan.md", reviewMarked), "plan.md")
		v3 := with(f.v1, "plan.md", reviewParent+"More.\n")
		f.publishBundle(agentA, "/api/v1/artifacts/"+f.id+"/versions", v3.manifest("plan.md"), v3)
		c := captureLog(f.fixture, slog.LevelDebug)
		if rec := f.finalizeReview(userU, seq, 1); rec.Code != http.StatusConflict {
			t.Fatalf("stale review: %d %s", rec.Code, rec.Body.String())
		}
		entries, _ := c.take(t)
		checkEntry(t, "discard", one(t, "discard", entries), "INFO", map[string]any{
			logKeyAction: logActionVersionFailed, logKeyOutcome: logOutcomeRefused, logKeyReason: FailReasonStaleReview,
			logKeyArtifactID: f.id, logKeyVersionSeq: float64(seq), logKeyProjectID: "project-1",
			logKeyOwnerKind: PrincipalKindAgent, logKeyPrincipalID: userU.ref,
			logKeyRoute: "POST /api/v1/artifacts/{id}/versions/{seq}/finalize",
		})
	})
}

// TestDecisionLogLinkSubjectRefNeverWritten: a share link's subject ref (the
// token's hash) is dropped even when an entry carries it.
func TestDecisionLogLinkSubjectRefNeverWritten(t *testing.T) {
	c := &logCapture{}
	l := slog.New(slog.NewJSONHandler(c, nil))
	writeDecision(context.Background(), l, slog.LevelInfo, "", "user", "u1",
		logEntry{action: logActionLinkCreated, subjectKind: SubjectLink, subjectRef: "token-hash-sentinel"})
	entries, raw := c.take(t)
	checkEntry(t, "link", one(t, "link", entries), "INFO", map[string]any{logKeySubjectKind: SubjectLink, logKeySubjectRef: nil})
	if strings.Contains(raw, "token-hash-sentinel") {
		t.Errorf("the link subject ref reached the entry: %s", raw)
	}
}

// noClauseHost is the fake host without ReviewGrantClauser: it allows review
// grants through ReviewGrantAuthority alone, which does not name the clause.
type noClauseHost struct {
	Host
	ReviewGrantAuthority
	ReviewSubjectAuthority
}

// TestDecisionLogReviewGrantUnnamedClause: on a host that does not name the
// clause, a review grant is logged (and recorded) with the authority
// reviewGrantUnnamedAuthority, and the
// host is asked once (MayGrantReview), never a second question.
func TestDecisionLogReviewGrantUnnamedClause(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	f.svc.host = noClauseHost{f.host, f.host, f.host}
	c := captureLog(f, slog.LevelDebug)
	f.host.mu.Lock()
	f.host.calls = nil
	f.host.mu.Unlock()
	if rec := f.postGrantBody(delegator, id, reviewBody(SubjectPrincipal, reviewerID, GrantReview)); rec.Code != http.StatusCreated {
		t.Fatalf("review grant: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ := c.take(t)
	checkEntry(t, "unnamed clause", one(t, "unnamed clause", entries), "INFO", map[string]any{
		logKeyAction: logActionGrantCreated, logKeyAuthority: reviewGrantUnnamedAuthority, logKeyPrincipalID: delegator.ref,
	})
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	reviews := 0
	for _, call := range f.host.calls {
		if strings.HasPrefix(call, "review ") {
			reviews++
		}
	}
	if reviews != 1 {
		t.Errorf("the host was asked about review %d times, want 1: %v", reviews, f.host.calls)
	}
}

// TestDecisionLogProjectAdminDelete: a delete by an administrator of the
// home project is logged with the event authority custodian_project_admin.
func TestDecisionLogProjectAdminDelete(t *testing.T) {
	f, id := newLinkFixture(t)
	admin := principal{PrincipalKindUser, "user-home-admin", ""}
	f.host.allow(admin, "project-1", PermissionRead)
	f.host.allowAdmin(admin, "project-1")
	c := captureLog(f, slog.LevelDebug)
	if rec := f.do(&admin, http.MethodDelete, artifactPath(id), nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ := c.take(t)
	checkEntry(t, "project admin delete", one(t, "project admin delete", entries), "INFO", map[string]any{
		logKeyAction: logActionDeleted, logKeyAuthority: logAuthorityCustodianProjectAdmin, logKeyPrincipalID: admin.ref,
		logKeyArtifactID: id, logKeyProjectID: "project-1", logKeyOwnerKind: PrincipalKindUser,
	})
}

// TestProjectIDShaped pins the shape a caller-supplied scope must have to be
// logged as project_id: 1 to 64 ASCII letters, digits, '-' or '_'.
func TestProjectIDShaped(t *testing.T) {
	for s, want := range map[string]bool{
		"":                                     false,
		strings.Repeat("a", 64):                true,
		strings.Repeat("a", 65):                false,
		"0b4f1d2e-9c3a-4e5f-8a7b-6c5d4e3f2a1b": true,
		"project_2-B9":                         true,
		"project.2":                            false,
		"project 2":                            false,
		"project\n2":                           false,
		"projé":                                false,
		"project/2":                            false,
	} {
		if got := projectIDShaped(s); got != want {
			t.Errorf("projectIDShaped(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestDecisionLogRefusalReasonsOnlyWhenWritten: the reason of a refusal is
// worked out (with extra host questions) only when the entry is written.
// With the logger above WARN, each refusal asks the host strictly less than
// with it at WARN.
func TestDecisionLogRefusalReasonsOnlyWhenWritten(t *testing.T) {
	f, id := newLinkFixture(t)
	reader := principal{PrincipalKindUser, "user-reader", ""}
	f.host.allow(reader, "project-1", PermissionRead)
	manifest := `{"entry":"a.md","files":[{"path":"a.md","size":1,"sha256":"` + sha([]byte("x")) + `"}]}`
	calls := func(level slog.Level, p principal, method, target, body string) int {
		t.Helper()
		captureLog(f, level)
		f.host.mu.Lock()
		f.host.calls = nil
		f.host.mu.Unlock()
		if rec := jsonDo(f, p, method, target, body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: %d %s", method, target, rec.Code, rec.Body.String())
		}
		f.host.mu.Lock()
		defer f.host.mu.Unlock()
		return len(f.host.calls)
	}
	for _, tc := range []struct {
		name         string
		p            principal
		method, path string
		body         string
	}{
		{"administer", reader, http.MethodPatch, artifactPath(id), `{"expiresAt":null}`},
		{"delete", reader, http.MethodDelete, artifactPath(id), ``},
		{"write", agentB, http.MethodPost, "/api/v1/artifacts/" + id + "/versions", manifest},
		{"create", agentA, http.MethodPost, "/api/v1/artifacts?name=x.md&scope=project-2", `x`},
	} {
		off := calls(slog.LevelError, tc.p, tc.method, tc.path, tc.body)
		on := calls(slog.LevelWarn, tc.p, tc.method, tc.path, tc.body)
		if off >= on {
			t.Errorf("%s: %d host calls with the refusal not logged, %d logged; want fewer when not logged", tc.name, off, on)
		}
	}
}

// grantCountStore counts ListGrants calls.
type grantCountStore struct {
	Store
	mu sync.Mutex
	n  int
}

func (s *grantCountStore) ListGrants(ctx context.Context, id string) ([]Grant, error) {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return s.Store.ListGrants(ctx, id)
}

// TestDecisionLogGrantRemovalReadOnlyWhenWritten: the grant read that gives
// a removal entry its subject happens only when the entry is written.
func TestDecisionLogGrantRemovalReadOnlyWhenWritten(t *testing.T) {
	f, id := newLinkFixture(t)
	st := &grantCountStore{Store: f.store}
	f.svc.SetStore(st)
	remove := func(level slog.Level) int {
		t.Helper()
		rec := jsonDo(f, userU, http.MethodPost, grantsPath(id), reviewBody(SubjectPrincipal, "user:user-g", GrantRead))
		if rec.Code != http.StatusCreated {
			t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
		}
		grantID := decodeInto[GrantResponse](t, rec).Grant.ID
		captureLog(f, level)
		st.mu.Lock()
		st.n = 0
		st.mu.Unlock()
		if rec := f.do(&userU, http.MethodDelete, grantsPath(id)+"/"+grantID, nil, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("delete grant: %d %s", rec.Code, rec.Body.String())
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.n
	}
	if off, on := remove(slog.LevelWarn), remove(slog.LevelInfo); off >= on {
		t.Errorf("grant reads: %d with the removal not logged, %d logged; want fewer when not logged", off, on)
	}
}

// sweepFailStore deletes one artifact, then fails, as a sweep that broke
// partway would.
type sweepFailStore struct{ Store }

func (sweepFailStore) SweepExpired(context.Context, time.Time, int) ([]ArtifactRef, error) {
	return []ArtifactRef{{ID: "a-swept", ScopeRef: "project-1"}}, errStoreDown
}

// TestMaintenanceLogSweepFailure: a sweep that fails partway logs what it
// deleted and a WARN summary with reason store_error.
func TestMaintenanceLogSweepFailure(t *testing.T) {
	f := newFixture(t, false)
	c := &logCapture{}
	l := slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug}))
	n, err := SweepExpired(context.Background(), sweepFailStore{f.store}, time.Now(), 10, l)
	if n != 1 || err == nil {
		t.Fatalf("SweepExpired = %d, %v; want 1 and the error", n, err)
	}
	entries, _ := c.take(t)
	if len(entries) != 2 {
		t.Fatalf("%d entries, want the deletion and a summary: %v", len(entries), entries)
	}
	checkEntry(t, "deletion", entries[0], "INFO", map[string]any{
		logKeyAction: logActionDeleted, logKeyArtifactID: "a-swept", logKeyProjectID: "project-1"})
	checkEntry(t, "summary", entries[1], "WARN", map[string]any{
		logKeyAction: logActionRetentionPass, logKeyOutcome: logOutcomeError, logKeyReason: logReasonStoreError, logKeyCount: 1.0})
}

// TestDecisionLogVersionsThroughGrants: a version written through a grant is
// logged with the grant's permission and no authority: write for a write
// grantee, review for a review grantee.
func TestDecisionLogVersionsThroughGrants(t *testing.T) {
	f := newReviewLevelFixture(t)
	c := captureLog(f.fixture, slog.LevelDebug)
	f.startReview(userU, with(f.v1, "plan.md", reviewMarked), "plan.md")
	entries, _ := c.take(t)
	checkEntry(t, "write grantee", one(t, "write grantee", entries), "INFO", map[string]any{
		logKeyAction: logActionVersionCreated, logKeyPermission: GrantWrite, logKeyAuthority: nil, logKeyPrincipalID: userU.ref})
	f.startReview(rvOnly, with(f.v1, "plan.md", reviewMarked), "plan.md")
	entries, _ = c.take(t)
	checkEntry(t, "review grantee", one(t, "review grantee", entries), "INFO", map[string]any{
		logKeyAction: logActionVersionCreated, logKeyPermission: GrantReview, logKeyAuthority: nil, logKeyPrincipalID: rvOnly.ref})
}

// sweepFailFirstStore fails before deleting anything.
type sweepFailFirstStore struct{ Store }

func (sweepFailFirstStore) SweepExpired(context.Context, time.Time, int) ([]ArtifactRef, error) {
	return nil, errStoreDown
}

// TestMaintenanceLogSweepFailsFirst: a sweep that fails before deleting
// anything still logs its WARN summary.
func TestMaintenanceLogSweepFailsFirst(t *testing.T) {
	f := newFixture(t, false)
	c := &logCapture{}
	l := slog.New(slog.NewJSONHandler(c, nil))
	if n, err := SweepExpired(context.Background(), sweepFailFirstStore{f.store}, time.Now(), 10, l); n != 0 || err == nil {
		t.Fatalf("SweepExpired = %d, %v; want 0 and the error", n, err)
	}
	entries, _ := c.take(t)
	checkEntry(t, "summary", one(t, "summary", entries), "WARN", map[string]any{
		logKeyAction: logActionRetentionPass, logKeyOutcome: logOutcomeError, logKeyReason: logReasonStoreError})
}
