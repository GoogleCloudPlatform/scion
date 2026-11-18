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
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	githubtrigger "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type ghIngressLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *ghIngressLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *ghIngressLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *ghIngressLogBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

type githubIngressTestFixture struct {
	srv           *Server
	store         store.Store
	appID         int64
	webhookSecret string
	instID        int64
	auditBuf      *ghIngressLogBuffer
	warnBuf       *ghIngressLogBuffer

	clockMu sync.Mutex
	now     time.Time
}

func setupGitHubIngressFixture(t *testing.T) *githubIngressTestFixture {
	t.Helper()
	srv, s := testServer(t)

	const (
		testAppID  int64 = 778899
		testSecret       = "whsec-github-ingress-test-secret"
		testInstID int64 = 554433
	)

	baseNow := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	auditBuf := &ghIngressLogBuffer{}
	warnBuf := &ghIngressLogBuffer{}

	f := &githubIngressTestFixture{
		srv:           srv,
		store:         s,
		appID:         testAppID,
		webhookSecret: testSecret,
		instID:        testInstID,
		auditBuf:      auditBuf,
		warnBuf:       warnBuf,
		now:           baseNow,
	}

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = testAppID
	srv.config.GitHubAppConfig.WebhookSecret = testSecret
	srv.config.GitHubAppConfig.AppSlug = "scion-bot"
	srv.config.GitHubAppConfig.CommandMentionAliases = []string{"scion-alias"}
	srv.config.GitHubAppConfig.WebhooksEnabled = true
	srv.mu.Unlock()

	srv.githubWebhookIngress.mu.Lock()
	srv.githubWebhookIngress.nowFn = f.currentTime
	srv.githubWebhookIngress.auditLogger = slog.New(slog.NewJSONHandler(auditBuf, nil))
	srv.githubWebhookIngress.warnLogger = slog.New(slog.NewJSONHandler(warnBuf, nil))
	srv.githubWebhookIngress.mu.Unlock()

	setTriggerExperimentsForGitHubIngressTest(t, srv, true, true)

	require.NoError(t, s.CreateGitHubInstallation(context.Background(), &store.GitHubInstallation{
		InstallationID: testInstID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          testAppID,
		Status:         store.GitHubInstallationStatusActive,
	}))

	return f
}

func (f *githubIngressTestFixture) currentTime() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.now
}

func (f *githubIngressTestFixture) advanceClock(d time.Duration) {
	f.clockMu.Lock()
	f.now = f.now.Add(d)
	f.clockMu.Unlock()
}

func setTriggerExperimentsForGitHubIngressTest(t *testing.T, srv *Server, triggersOn, githubTriggersOn bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.Triggers:
			e.Default = triggersOn
		case experiments.GitHubTriggers:
			e.Default = githubTriggersOn
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, experiments.Default().Retired())
	require.NoError(t, err)
	srv.mu.Lock()
	srv.experiments = reg
	srv.mu.Unlock()
}

func signGitHubBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (f *githubIngressTestFixture) createCandidateProject(t *testing.T, slug, gitRemote string, instID int64) *store.Project {
	t.Helper()
	instCopy := instID
	proj := &store.Project{
		ID:                   uuid.NewString(),
		Name:                 slug,
		Slug:                 slug,
		GitRemote:            gitRemote,
		GitHubInstallationID: &instCopy,
		Created:              f.currentTime(),
		Updated:              f.currentTime(),
	}
	require.NoError(t, f.store.CreateProject(context.Background(), proj))
	return proj
}

func sampleIssueCommentPayload(t *testing.T, instID int64, repoFullName, senderLogin, senderType, commentBody string, commentCreated time.Time) []byte {
	t.Helper()
	parts := strings.SplitN(repoFullName, "/", 2)
	owner, repo := "acme", "widgets"
	if len(parts) == 2 {
		owner, repo = parts[0], parts[1]
	}
	raw, err := json.Marshal(map[string]any{
		"action": "created",
		"installation": map[string]any{
			"id": instID,
		},
		"organization": map[string]any{
			"login":      owner,
			"avatar_url": "https://avatars.example.com/org",
		},
		"repository": map[string]any{
			"id":             101,
			"name":           repo,
			"full_name":      repoFullName,
			"private":        true,
			"default_branch": "main",
			"html_url":       "https://github.com/" + repoFullName,
			"archive_url":    "https://api.github.com/repos/" + repoFullName + "/{archive_format}{/ref}",
			"owner": map[string]any{
				"login":      owner,
				"id":         10,
				"type":       "Organization",
				"avatar_url": "https://avatars.example.com/owner",
			},
		},
		"sender": map[string]any{
			"login":      senderLogin,
			"id":         202,
			"type":       senderType,
			"avatar_url": "https://avatars.example.com/sender",
		},
		"issue": map[string]any{
			"number":             42,
			"state":              "open",
			"title":              "Fix flaky webhook retry",
			"body":               "Opening issue description",
			"html_url":           "https://github.com/" + repoFullName + "/pull/42",
			"author_association": "CONTRIBUTOR",
			"created_at":         commentCreated.Add(-time.Hour).UTC().Format(time.RFC3339),
			"updated_at":         commentCreated.UTC().Format(time.RFC3339),
			"user": map[string]any{
				"login": "pr-author",
				"id":    303,
				"type":  "User",
			},
			"labels": []map[string]any{
				{"name": "bug"},
				{"name": "triggers"},
			},
			"assignees": []map[string]any{
				{"login": "octocat"},
			},
			"pull_request": map[string]any{
				"html_url": "https://github.com/" + repoFullName + "/pull/42",
			},
		},
		"comment": map[string]any{
			"id":                 9001,
			"body":               commentBody,
			"html_url":           "https://github.com/" + repoFullName + "/pull/42#issuecomment-9001",
			"author_association": "MEMBER",
			"created_at":         commentCreated.UTC().Format(time.RFC3339),
			"updated_at":         commentCreated.UTC().Format(time.RFC3339),
			"user": map[string]any{
				"login": senderLogin,
				"id":    202,
				"type":  senderType,
			},
		},
	})
	require.NoError(t, err)
	return raw
}

func (f *githubIngressTestFixture) postDelivery(t *testing.T, eventType, deliveryID string, body []byte, mutateReq func(r *http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GitHub-Hookshot/test")
	req.Header.Set("X-Hub-Signature-256", signGitHubBody(f.webhookSecret, body))
	if eventType != "" {
		req.Header.Set("X-GitHub-Event", eventType)
	}
	if deliveryID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryID)
	}
	req.Header.Set("X-GitHub-Hook-ID", "445566")
	req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", strconv.FormatInt(f.appID, 10))
	if mutateReq != nil {
		mutateReq(req)
	}
	rec := httptest.NewRecorder()
	f.srv.handleGitHubWebhook(rec, req)
	return rec
}

func parseAuditLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "audit line: %s", line)
		out = append(out, m)
	}
	return out
}

func TestGitHubWebhookIngress_ExperimentGateBothFlagsRequired(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	proj := f.createCandidateProject(t, "gate-proj", "https://github.com/acme/widgets.git", f.instID)
	owner := createUserForGitHubTriggerTest(t, f.store, "gate-owner", store.UserRoleMember)
	bindProjectRoleForGitHubTriggerTest(t, f.store, proj.ID, owner.ID, store.ProjectRoleOwner)

	body := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "alice", "User", "@scion-bot /review", f.currentTime())

	offCombos := []struct {
		name             string
		triggersOn       bool
		githubTriggersOn bool
	}{
		{"both_off", false, false},
		{"only_triggers_on", true, false},
		{"only_github_triggers_on", false, true},
	}

	for _, tc := range offCombos {
		t.Run(tc.name, func(t *testing.T) {
			setTriggerExperimentsForGitHubIngressTest(t, f.srv, tc.triggersOn, tc.githubTriggersOn)
			assert.False(t, f.srv.githubTriggersEnabled())

			// Even without target headers, off flags preserve 200 {"status":"ignored","event":...} unchanged.
			rec := f.postDelivery(t, "issue_comment", uuid.NewString(), body, func(r *http.Request) {
				r.Header.Del("X-GitHub-Hook-Installation-Target-Type")
				r.Header.Del("X-GitHub-Hook-Installation-Target-ID")
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.JSONEq(t, `{"event":"issue_comment","status":"ignored"}`, rec.Body.String())

			// requireGitHubTriggers routes also answer 404 unless BOTH flags are enabled.
			settingsRec := doRequestAsUser(t, f.srv, owner, http.MethodGet, "/api/v1/projects/"+proj.ID+"/trigger-sources/github", nil)
			assert.Equal(t, http.StatusNotFound, settingsRec.Code)
		})
	}

	setTriggerExperimentsForGitHubIngressTest(t, f.srv, true, true)
	assert.True(t, f.srv.githubTriggersEnabled())

	deliv := uuid.NewString()
	rec := f.postDelivery(t, "issue_comment", deliv, body, nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var accepted GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &accepted))
	assert.Equal(t, GitHubWebhookStatusQueued, accepted.Status)
	require.Len(t, accepted.Events, 1)
	assert.Equal(t, proj.ID, accepted.Events[0].ProjectID)
}

func TestGitHubWebhookIngress_WrongTargetAndRateLimitedAudit(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	proj := f.createCandidateProject(t, "target-proj", "https://github.com/acme/widgets.git", f.instID)
	body := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "alice", "User", "hello", f.currentTime())

	cases := []struct {
		name   string
		mutate func(r *http.Request)
	}{
		{
			name: "missing_target_type",
			mutate: func(r *http.Request) {
				r.Header.Del("X-GitHub-Hook-Installation-Target-Type")
			},
		},
		{
			name: "repository_target_type",
			mutate: func(r *http.Request) {
				r.Header.Set("X-GitHub-Hook-Installation-Target-Type", "repository")
			},
		},
		{
			name: "wrong_app_id",
			mutate: func(r *http.Request) {
				r.Header.Set("X-GitHub-Hook-Installation-Target-ID", "99999999")
			},
		},
		{
			name: "duplicate_target_id_header",
			mutate: func(r *http.Request) {
				r.Header.Add("X-GitHub-Hook-Installation-Target-ID", strconv.FormatInt(f.appID, 10))
			},
		},
	}

	for _, tc := range cases {
		rec := f.postDelivery(t, "issue_comment", uuid.NewString(), body, tc.mutate)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", tc.name, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "wrong target")
	}

	// All 4 mismatches occurred in the same minute: 1 audit record emitted, 3 suppressed.
	lines := parseAuditLines(t, f.auditBuf.String())
	require.Len(t, lines, 1)
	assert.Equal(t, GitHubAuditEventRejected, lines[0]["event_type"])
	assert.Equal(t, GitHubAuditRejectReasonTargetMismatch, lines[0]["reason"])
	assert.NotContains(t, f.auditBuf.String(), "99999999")

	// Advance past 1 minute: next mismatch emits audit with suppressed_count=3.
	f.advanceClock(61 * time.Second)
	rec := f.postDelivery(t, "issue_comment", uuid.NewString(), body, func(r *http.Request) {
		r.Header.Set("X-GitHub-Hook-Installation-Target-ID", "111111")
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	lines = parseAuditLines(t, f.auditBuf.String())
	require.Len(t, lines, 2)
	assert.Equal(t, float64(3), lines[1]["suppressed_count"])

	evs, err := f.store.ListTriggerEvents(context.Background(), store.TriggerEventFilter{ProjectID: proj.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, evs.Items)
}

func TestGitHubWebhookIngress_UnknownEventFamilyAndBadDeliveryID(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	proj := f.createCandidateProject(t, "family-proj", "https://github.com/acme/widgets.git", f.instID)
	body := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "alice", "User", "hi", f.currentTime())

	for _, family := range []string{"check_run", "deployment", "workflow_run"} {
		rec := f.postDelivery(t, family, "not-checked-for-ignored-family", body, nil)
		require.Equal(t, http.StatusOK, rec.Code, family)
		assert.JSONEq(t, fmt.Sprintf(`{"status":"ignored","event":%q}`, family), rec.Body.String())
	}

	badDeliveries := []struct {
		name   string
		mutate func(r *http.Request)
	}{
		{
			name: "missing_delivery_header",
			mutate: func(r *http.Request) {
				r.Header.Del("X-GitHub-Delivery")
			},
		},
		{
			name: "non_guid",
			mutate: func(r *http.Request) {
				r.Header.Set("X-GitHub-Delivery", "deliv-12345")
			},
		},
		{
			name: "over_64_bytes",
			mutate: func(r *http.Request) {
				r.Header.Set("X-GitHub-Delivery", strings.Repeat("a", 65))
			},
		},
		{
			name: "wrong_hyphen_positions",
			mutate: func(r *http.Request) {
				r.Header.Set("X-GitHub-Delivery", "72d3162ecc78-11e3-81ab-4c9367dc09580")
			},
		},
		{
			name: "duplicate_delivery_header",
			mutate: func(r *http.Request) {
				r.Header.Add("X-GitHub-Delivery", uuid.NewString())
			},
		},
	}

	for _, tc := range badDeliveries {
		rec := f.postDelivery(t, "issue_comment", uuid.NewString(), body, tc.mutate)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", tc.name, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "invalid delivery id")
	}

	evs, err := f.store.ListTriggerEvents(context.Background(), store.TriggerEventFilter{ProjectID: proj.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, evs.Items)
}

func TestGitHubWebhookIngress_HubVerifiedBucketExhausted(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	proj := f.createCandidateProject(t, "bucket-proj", "https://github.com/acme/widgets.git", f.instID)
	body := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "alice", "User", "hi", f.currentTime())

	f.srv.githubWebhookIngress.mu.Lock()
	f.srv.githubWebhookIngress.bucketInit = false
	f.srv.githubWebhookIngress.bucketRate = 1.0
	f.srv.githubWebhookIngress.bucketBurst = 2
	f.srv.githubWebhookIngress.mu.Unlock()

	for i := 0; i < 2; i++ {
		rec := f.postDelivery(t, "issue_comment", uuid.NewString(), body, func(r *http.Request) {
			r.RemoteAddr = fmt.Sprintf("192.0.2.%d:443", i+1)
		})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	}

	// Third delivery from a brand-new IP still hits the hub-wide verified bucket (no IP keying).
	rec := f.postDelivery(t, "issue_comment", uuid.NewString(), body, func(r *http.Request) {
		r.RemoteAddr = "198.51.100.99:443"
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, "5", rec.Header().Get("Retry-After"))

	evs, err := f.store.ListTriggerEvents(context.Background(), store.TriggerEventFilter{ProjectID: proj.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, evs.Items, 2, "503 rate-limited delivery must write no row")

	// Advancing clock refills 1 token and admits the next delivery.
	f.advanceClock(1100 * time.Millisecond)
	rec = f.postDelivery(t, "issue_comment", uuid.NewString(), body, nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
}

func TestGitHubWebhookIngress_MalformedPayloadAndNormaliserErrors(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	proj := f.createCandidateProject(t, "badpayload-proj", "https://github.com/acme/widgets.git", f.instID)

	badBodies := []struct {
		name string
		body []byte
	}{
		{"empty_body", nil},
		{"invalid_json", []byte(`{"action":`)},
		{"json_array", []byte(`[1,2,3]`)},
		{"contains_nul", []byte("{\"installation\":{\"id\":554433},\"x\":\"\x00\"}")},
		{"missing_installation", []byte(`{"action":"created"}`)},
		{"zero_installation_id", []byte(`{"action":"created","installation":{"id":0}}`)},
		{"negative_installation_id", []byte(`{"action":"created","installation":{"id":-5}}`)},
		{"float_installation_id", []byte(`{"action":"created","installation":{"id":12.5}}`)},
		{
			"missing_action",
			[]byte(fmt.Sprintf(`{"installation":{"id":%d},"repository":{"full_name":"acme/widgets"},"sender":{"login":"alice","type":"User"}}`, f.instID)),
		},
		{
			"missing_sender_login",
			[]byte(fmt.Sprintf(`{"action":"created","installation":{"id":%d},"repository":{"full_name":"acme/widgets"},"sender":{}}`, f.instID)),
		},
		{
			"invalid_repo_full_name",
			[]byte(fmt.Sprintf(`{"action":"created","installation":{"id":%d},"repository":{"full_name":"not-slash"},"sender":{"login":"alice","type":"User"}}`, f.instID)),
		},
		{
			"normaliser_missing_comment_object",
			[]byte(fmt.Sprintf(`{"action":"created","installation":{"id":%d},"repository":{"full_name":"acme/widgets"},"sender":{"login":"alice","type":"User"},"issue":{"number":1,"state":"open","title":"t"}}`, f.instID)),
		},
	}

	for _, tc := range badBodies {
		rec := f.postDelivery(t, "issue_comment", uuid.NewString(), tc.body, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", tc.name, rec.Body.String())
	}

	evs, err := f.store.ListTriggerEvents(context.Background(), store.TriggerEventFilter{ProjectID: proj.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, evs.Items)
}

func TestGitHubWebhookIngress_UnknownAndInactiveInstallation(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	ctx := context.Background()

	const (
		unknownInstID   int64 = 888888
		suspendedInstID int64 = 777777
		deletedInstID   int64 = 666666
	)
	require.NoError(t, f.store.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: suspendedInstID,
		AccountLogin:   "acme-susp",
		AccountType:    "Organization",
		AppID:          f.appID,
		Status:         store.GitHubInstallationStatusSuspended,
	}))
	require.NoError(t, f.store.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: deletedInstID,
		AccountLogin:   "acme-del",
		AccountType:    "Organization",
		AppID:          f.appID,
		Status:         store.GitHubInstallationStatusDeleted,
	}))

	projSusp := f.createCandidateProject(t, "susp-proj", "https://github.com/acme-susp/widgets.git", suspendedInstID)
	projDel := f.createCandidateProject(t, "del-proj", "https://github.com/acme-del/widgets.git", deletedInstID)

	for _, tc := range []struct {
		name   string
		instID int64
		repo   string
	}{
		{"unknown_installation", unknownInstID, "acme/widgets"},
		{"suspended_installation", suspendedInstID, "acme-susp/widgets"},
		{"deleted_installation", deletedInstID, "acme-del/widgets"},
	} {
		f.auditBuf.Reset()
		deliv := uuid.NewString()
		body := sampleIssueCommentPayload(t, tc.instID, tc.repo, "alice", "User", "hello", f.currentTime())
		rec := f.postDelivery(t, "issue_comment", deliv, body, nil)
		require.Equal(t, http.StatusAccepted, rec.Code, "%s: %s", tc.name, rec.Body.String())

		var resp GitHubWebhookAcceptedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, GitHubWebhookStatusUnknownInstallation, resp.Status)
		assert.Equal(t, deliv, resp.Delivery)
		assert.Empty(t, resp.Events)

		audits := parseAuditLines(t, f.auditBuf.String())
		require.Len(t, audits, 1, tc.name)
		assert.Equal(t, trigger.AuditEventReceived, audits[0]["event_type"])
		assert.Equal(t, GitHubWebhookStatusUnknownInstallation, audits[0]["result"])
	}

	for _, pid := range []string{projSusp.ID, projDel.ID} {
		evs, err := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: pid}, store.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, evs.Items)
	}
}

func TestGitHubWebhookIngress_OwnAppSenderIgnoredAndThirdPartyBotAccepted(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	proj := f.createCandidateProject(t, "botguard-proj", "https://github.com/acme/widgets.git", f.instID)

	// Own-App sender (<slug>[bot] with type Bot, case-insensitive) -> 202 ignored/self, no row, metrics only.
	selfDeliv := uuid.NewString()
	selfBody := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "SCION-BOT[bot]", "Bot", "@scion-bot /review", f.currentTime())
	rec := f.postDelivery(t, "issue_comment", selfDeliv, selfBody, nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var selfResp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &selfResp))
	assert.Equal(t, GitHubWebhookStatusIgnored, selfResp.Status)
	assert.Equal(t, "self", selfResp.Reason)
	assert.Equal(t, int64(1), f.srv.githubWebhookIngress.selfIgnoredCount.Load())

	evs, err := f.store.ListTriggerEvents(context.Background(), store.TriggerEventFilter{ProjectID: proj.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, evs.Items)

	// Third-party bot (dependabot[bot]) is NOT dropped at ingress; sender_type == "Bot" is recorded.
	depDeliv := uuid.NewString()
	depBody := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "dependabot[bot]", "Bot", "Bump deps", f.currentTime())
	depRec := f.postDelivery(t, "issue_comment", depDeliv, depBody, nil)
	require.Equal(t, http.StatusAccepted, depRec.Code, depRec.Body.String())

	var depResp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(depRec.Body.Bytes(), &depResp))
	assert.Equal(t, GitHubWebhookStatusQueued, depResp.Status)
	require.Len(t, depResp.Events, 1)

	stored, err := f.store.GetTriggerEvent(context.Background(), depResp.Events[0].EventID)
	require.NoError(t, err)
	assert.Equal(t, "Bot", stored.Attributes["sender_type"])
	assert.Equal(t, "dependabot[bot]", stored.Attributes["sender"])
}

func TestGitHubWebhookIngress_NoMatchingProject(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	// Bound to installation, but different repository and different git host.
	_ = f.createCandidateProject(t, "other-repo", "https://github.com/acme/other-repo.git", f.instID)
	_ = f.createCandidateProject(t, "other-host", "https://gitlab.example.com/acme/widgets.git", f.instID)

	deliv := uuid.NewString()
	body := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "alice", "User", "hello", f.currentTime())
	rec := f.postDelivery(t, "issue_comment", deliv, body, nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var resp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, GitHubWebhookStatusNoProject, resp.Status)
	assert.Equal(t, deliv, resp.Delivery)
	assert.Empty(t, resp.Events)

	audits := parseAuditLines(t, f.auditBuf.String())
	require.Len(t, audits, 1)
	assert.Equal(t, trigger.AuditEventReceived, audits[0]["event_type"])
	assert.Equal(t, GitHubWebhookStatusNoProject, audits[0]["result"])
	assert.Equal(t, "acme/widgets", audits[0]["repo"])
}

func TestGitHubWebhookIngress_MultiProjectFanOutListsAndNormaliserContract(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	ctx := context.Background()

	// Two projects sharing acme/widgets with different git remote formats and letter case.
	projA := f.createCandidateProject(t, "fanout-a", "https://github.com/Acme/Widgets.git", f.instID)
	projB := f.createCandidateProject(t, "fanout-b", "git@github.com:acme/widgets.git", f.instID)
	_ = f.createCandidateProject(t, "unrelated", "https://github.com/acme/unrelated.git", f.instID)

	ownerA := createUserForGitHubTriggerTest(t, f.store, "fanout-owner-a", store.UserRoleMember)
	bindProjectRoleForGitHubTriggerTest(t, f.store, projA.ID, ownerA.ID, store.ProjectRoleOwner)

	// Comment created 25h before ReceivedAt exercises StaleThreshold (>24h -> stale="true")
	// and @scion-alias exercises MentionAliases from GitHubAppMentionIdentities.
	commentTime := f.currentTime().Add(-25 * time.Hour)
	deliv := "72d3162e-cc78-11e3-81ab-4c9367dc0958"
	body := sampleIssueCommentPayload(t, f.instID, "Acme/Widgets", "alice", "User", "Please check: @scion-alias /review focus-auth", commentTime)

	rec := f.postDelivery(t, "issue_comment", deliv, body, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer should-never-be-stored")
	})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var resp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, GitHubWebhookStatusQueued, resp.Status)
	assert.Equal(t, deliv, resp.Delivery)
	require.Len(t, resp.Events, 2)
	assert.Empty(t, resp.SkippedProjects)

	byProj := make(map[string]GitHubWebhookProjectEvent, 2)
	for _, e := range resp.Events {
		assert.Equal(t, GitHubWebhookStatusQueued, e.Status)
		byProj[e.ProjectID] = e
	}
	require.Contains(t, byProj, projA.ID)
	require.Contains(t, byProj, projB.ID)
	assert.NotEqual(t, byProj[projA.ID].EventID, byProj[projB.ID].EventID)

	dqStore, ok := f.store.(store.DurableQueueStore)
	require.True(t, ok)

	for _, pid := range []string{projA.ID, projB.ID} {
		ev, err := f.store.GetTriggerEvent(ctx, byProj[pid].EventID)
		require.NoError(t, err)
		assert.Equal(t, store.TriggerSourceKindGitHub, ev.SourceKind)
		assert.Equal(t, strconv.FormatInt(f.instID, 10), ev.SourceID)
		assert.Equal(t, "github.issue_comment.created", ev.Type)
		assert.Equal(t, "github:Acme/Widgets#42", ev.Subject)
		assert.Equal(t, deliv+"/"+pid, ev.DedupKey)
		assert.Equal(t, commentTime.UTC(), ev.OccurredAt.UTC())
		assert.Equal(t, f.currentTime().UTC(), ev.ReceivedAt.UTC())
		assert.Equal(t, "review", ev.Attributes["command"])
		assert.Equal(t, "focus-auth", ev.Attributes["command_args"])
		assert.Equal(t, "true", ev.Attributes["mention"])
		assert.Equal(t, "true", ev.Attributes["stale"], "25h old comment must carry stale=true")
		assert.Equal(t, "true", ev.Attributes["is_pull_request"])
		assert.Equal(t, map[string][]string{
			"labels":    {"bug", "triggers"},
			"assignees": {"octocat"},
		}, ev.Lists)

		// Header allow-list check: signature and Authorization headers are never stored.
		assert.Equal(t, "issue_comment", ev.Headers["X-GitHub-Event"])
		assert.Equal(t, deliv, ev.Headers["X-GitHub-Delivery"])
		assert.Equal(t, "445566", ev.Headers["X-GitHub-Hook-ID"])
		assert.Equal(t, strconv.FormatInt(f.appID, 10), ev.Headers["X-GitHub-Hook-Installation-Target-ID"])
		assert.Equal(t, "GitHub-Hookshot/test", ev.Headers["User-Agent"])
		assert.NotContains(t, ev.Headers, "X-Hub-Signature-256")
		assert.NotContains(t, ev.Headers, "Authorization")

		// Payload is projected (organization and archive_url stripped).
		assert.NotContains(t, ev.Payload, "archive_url")
		assert.NotContains(t, ev.Payload, "avatars.example.com")

		stats, err := dqStore.DurableQueueStats(ctx, trigger.EventQueueKind, pid, f.currentTime())
		require.NoError(t, err)
		assert.Equal(t, int64(1), stats.Pending)
	}

	// Verify GET /trigger-events and GET /trigger-events/{id} return persisted lists.
	detailRec := doRequestAsUser(t, f.srv, ownerA, http.MethodGet,
		"/api/v1/projects/"+projA.ID+"/trigger-events/"+byProj[projA.ID].EventID, nil)
	require.Equal(t, http.StatusOK, detailRec.Code, detailRec.Body.String())
	var detail TriggerEventDetailResponse
	require.NoError(t, json.Unmarshal(detailRec.Body.Bytes(), &detail))
	assert.Equal(t, map[string][]string{"labels": {"bug", "triggers"}, "assignees": {"octocat"}}, detail.Lists)

	// When AppSlug is unknown (cleared override and no private key to fetch GET /app),
	// mention_detection=unavailable is recorded on the normalised event.
	f.srv.mu.Lock()
	f.srv.config.GitHubAppConfig.AppSlug = ""
	f.srv.mu.Unlock()
	f.srv.githubAppIdentity.reset()

	noSlugDeliv := uuid.NewString()
	noSlugRec := f.postDelivery(t, "issue_comment", noSlugDeliv, body, nil)
	require.Equal(t, http.StatusAccepted, noSlugRec.Code, noSlugRec.Body.String())
	var noSlugResp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(noSlugRec.Body.Bytes(), &noSlugResp))
	require.Len(t, noSlugResp.Events, 2)
	noSlugEv, err := f.store.GetTriggerEvent(ctx, noSlugResp.Events[0].EventID)
	require.NoError(t, err)
	assert.Equal(t, "unavailable", noSlugEv.Attributes["mention_detection"])
	assert.NotContains(t, noSlugEv.Attributes, "command")
}

func TestGitHubWebhookIngress_PrimaryRoutingAndStalePrimaryWarning(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	ctx := context.Background()

	projA := f.createCandidateProject(t, "primary-a", "https://github.com/acme/widgets.git", f.instID)
	projB := f.createCandidateProject(t, "primary-b", "https://github.com/acme/widgets.git", f.instID)

	t.Run("routed_to_primary_delivers_only_to_primary_and_audits_bypassed", func(t *testing.T) {
		require.NoError(t, f.store.SetGitHubInstallationTriggerRoute(ctx, f.instID, "acme/widgets", store.GitHubTriggerRoute{
			PrimaryProjectID: projB.ID,
			SetBy:            "admin",
			SetAt:            f.currentTime(),
		}))

		f.auditBuf.Reset()
		deliv := uuid.NewString()
		body := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "alice", "User", "hello", f.currentTime())
		rec := f.postDelivery(t, "issue_comment", deliv, body, nil)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		var resp GitHubWebhookAcceptedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, GitHubWebhookStatusQueued, resp.Status)
		require.Len(t, resp.Events, 1)
		assert.Equal(t, projB.ID, resp.Events[0].ProjectID)

		evsA, err := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: projA.ID}, store.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, evsA.Items, "bypassed project must receive no event")

		audits := parseAuditLines(t, f.auditBuf.String())
		require.Len(t, audits, 1)
		assert.Equal(t, GitHubWebhookRoutingRoutedToPrimary, audits[0]["routing"])
		assert.Equal(t, []any{projA.ID}, audits[0]["bypassed_project_ids"])
	})

	t.Run("stale_primary_fans_out_to_all_candidates_and_warns_once_per_hour", func(t *testing.T) {
		require.NoError(t, f.store.SetGitHubInstallationTriggerRoute(ctx, f.instID, "acme/widgets", store.GitHubTriggerRoute{
			PrimaryProjectID: uuid.NewString(), // no longer a candidate
			SetBy:            "admin",
			SetAt:            f.currentTime(),
		}))

		f.auditBuf.Reset()
		f.warnBuf.Reset()

		body := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "alice", "User", "hello", f.currentTime())
		rec1 := f.postDelivery(t, "issue_comment", uuid.NewString(), body, nil)
		require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())

		var resp1 GitHubWebhookAcceptedResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		require.Len(t, resp1.Events, 2, "stale primary must fail open to all candidates")

		audits := parseAuditLines(t, f.auditBuf.String())
		require.Len(t, audits, 1)
		assert.Equal(t, GitHubWebhookRoutingStalePrimary, audits[0]["routing"])

		warns := parseAuditLines(t, f.warnBuf.String())
		require.Len(t, warns, 1, "first stale_primary delivery must emit slog.Warn")

		// Second delivery 10 minutes later suppresses duplicate stale_primary warning.
		f.advanceClock(10 * time.Minute)
		rec2 := f.postDelivery(t, "issue_comment", uuid.NewString(), body, nil)
		require.Equal(t, http.StatusAccepted, rec2.Code)
		assert.Len(t, parseAuditLines(t, f.warnBuf.String()), 1, "stale_primary slog.Warn is rate-limited to 1/hour")

		// After >1 hour, warning is emitted again.
		f.advanceClock(55 * time.Minute)
		rec3 := f.postDelivery(t, "issue_comment", uuid.NewString(), body, nil)
		require.Equal(t, http.StatusAccepted, rec3.Code)
		assert.Len(t, parseAuditLines(t, f.warnBuf.String()), 2)
	})
}

func TestGitHubWebhookIngress_RedeliveryReturnsDuplicate(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	ctx := context.Background()

	projA := f.createCandidateProject(t, "dup-a", "https://github.com/acme/widgets.git", f.instID)
	projB := f.createCandidateProject(t, "dup-b", "https://github.com/acme/widgets.git", f.instID)

	deliv := "72d3162e-cc78-11e3-81ab-4c9367dc0958"
	body := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "alice", "User", "hello", f.currentTime())

	rec1 := f.postDelivery(t, "issue_comment", deliv, body, nil)
	require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())
	var first GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &first))
	require.Len(t, first.Events, 2)

	// Manual redelivery with uppercase GUID hex deduplicates to the exact original event IDs.
	f.auditBuf.Reset()
	rec2 := f.postDelivery(t, "issue_comment", strings.ToUpper(deliv), body, nil)
	require.Equal(t, http.StatusAccepted, rec2.Code, rec2.Body.String())
	var second GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &second))
	assert.Equal(t, GitHubWebhookStatusDuplicate, second.Status)
	require.Len(t, second.Events, 2)
	for i := range second.Events {
		assert.Equal(t, GitHubWebhookStatusDuplicate, second.Events[i].Status)
		assert.Equal(t, first.Events[i].ProjectID, second.Events[i].ProjectID)
		assert.Equal(t, first.Events[i].EventID, second.Events[i].EventID)
	}

	dqStore := f.store.(store.DurableQueueStore)
	for _, pid := range []string{projA.ID, projB.ID} {
		evs, err := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: pid}, store.ListOptions{})
		require.NoError(t, err)
		assert.Len(t, evs.Items, 1)
		stats, err := dqStore.DurableQueueStats(ctx, trigger.EventQueueKind, pid, f.currentTime())
		require.NoError(t, err)
		assert.Equal(t, int64(1), stats.Pending)
	}

	audits := parseAuditLines(t, f.auditBuf.String())
	require.Len(t, audits, 1)
	assert.Equal(t, GitHubWebhookStatusDuplicate, audits[0]["result"])
}

func TestGitHubWebhookIngress_PendingCapSkipsOverCapProjectWithAudit(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	ctx := context.Background()

	projOver := f.createCandidateProject(t, "cap-over", "https://github.com/acme/widgets.git", f.instID)
	projOK := f.createCandidateProject(t, "cap-ok", "https://github.com/acme/widgets.git", f.instID)

	// First accept one delivery so projOver has 1 real event we can redeliver at the cap.
	seedDeliv := "11111111-2222-3333-4444-555555555555"
	body := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "alice", "User", "hello", f.currentTime())
	seedRec := f.postDelivery(t, "issue_comment", seedDeliv, body, nil)
	require.Equal(t, http.StatusAccepted, seedRec.Code)

	// Fill projOver's pending queue up to MaxPendingEventsPerProject (5000).
	queue := f.srv.TriggerEventQueue()
	require.NotNil(t, queue)
	require.NoError(t, f.store.WithTx(ctx, func(tx store.Store) error {
		for i := 1; i < trigger.MaxPendingEventsPerProject; i++ {
			_, err := queue.Enqueue(ctx, tx, durablequeue.Item{
				Kind:      trigger.EventQueueKind,
				DedupKey:  fmt.Sprintf("cap-fill-%d", i),
				ProjectID: projOver.ID,
				Payload:   []byte(`{"v":1,"event_id":"synthetic"}`),
			})
			if err != nil {
				return err
			}
		}
		return nil
	}))

	// New delivery: projOver is skipped with queue_full, while healthy sibling projOK is queued!
	f.auditBuf.Reset()
	newDeliv := uuid.NewString()
	rec := f.postDelivery(t, "issue_comment", newDeliv, body, nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var resp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, GitHubWebhookStatusQueued, resp.Status)
	require.Len(t, resp.Events, 1)
	assert.Equal(t, projOK.ID, resp.Events[0].ProjectID)
	assert.Equal(t, GitHubWebhookStatusQueued, resp.Events[0].Status)
	require.Len(t, resp.SkippedProjects, 1)
	assert.Equal(t, projOver.ID, resp.SkippedProjects[0].ProjectID)
	assert.Equal(t, GitHubWebhookSkipReasonQueueFull, resp.SkippedProjects[0].Reason)

	// Verify no trigger_events row was written for projOver on newDeliv.
	_, err := f.store.GetTriggerEventByDedupKey(ctx, store.TriggerSourceKindGitHub, strconv.FormatInt(f.instID, 10), newDeliv+"/"+projOver.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	audits := parseAuditLines(t, f.auditBuf.String())
	require.Len(t, audits, 1)
	assert.Equal(t, GitHubWebhookStatusQueued, audits[0]["result"])
	assert.Contains(t, f.auditBuf.String(), GitHubWebhookSkipReasonQueueFull)

	// Redelivery of seedDeliv while projOver is at 5000 pending still answers duplicate (dedup runs before cap check).
	dupRec := f.postDelivery(t, "issue_comment", seedDeliv, body, nil)
	require.Equal(t, http.StatusAccepted, dupRec.Code, dupRec.Body.String())
	var dupResp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(dupRec.Body.Bytes(), &dupResp))
	assert.Equal(t, GitHubWebhookStatusDuplicate, dupResp.Status)
	assert.Empty(t, dupResp.SkippedProjects)
	require.Len(t, dupResp.Events, 2)
}

func TestGitHubWebhookIngress_AttributesCeilingExceededMappedTo202Skipped(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	ctx := context.Background()

	projA := f.createCandidateProject(t, "ceil-a", "https://github.com/acme/widgets.git", f.instID)
	projB := f.createCandidateProject(t, "ceil-b", "https://github.com/acme/widgets.git", f.instID)

	// Construct a pull_request_review_comment payload whose protected identifier
	// attributes (5 KiB head_ref + 4 KiB path) exceed the 8 KiB attributes ceiling.
	hugeHeadRef := "feat/" + strings.Repeat("r", 5000)
	hugePath := "dir/" + strings.Repeat("p", 4000) + ".go"
	raw, err := json.Marshal(map[string]any{
		"action":       "created",
		"installation": map[string]any{"id": f.instID},
		"repository": map[string]any{
			"id": 1, "name": "widgets", "full_name": "acme/widgets", "private": false,
			"owner": map[string]any{"login": "acme", "id": 1, "type": "Organization"},
		},
		"sender": map[string]any{"login": "alice", "id": 2, "type": "User"},
		"pull_request": map[string]any{
			"number": 7, "state": "open", "title": "some title", "body": "some body",
			"html_url": "https://github.com/acme/widgets/pull/7", "author_association": "MEMBER",
			"base": map[string]any{"ref": "main", "sha": "111", "repo": map[string]any{"id": 1, "full_name": "acme/widgets", "name": "widgets", "private": false}},
			"head": map[string]any{"ref": hugeHeadRef, "sha": "222", "repo": map[string]any{"id": 1, "full_name": "acme/widgets", "name": "widgets", "private": false}},
		},
		"comment": map[string]any{
			"id": 55, "body": "inline note", "path": hugePath, "line": 12, "side": "RIGHT",
			"html_url":   "https://github.com/acme/widgets/pull/7#discussion_r55",
			"created_at": "2026-11-18T12:00:00Z", "updated_at": "2026-11-18T12:00:00Z",
		},
	})
	require.NoError(t, err)

	f.auditBuf.Reset()
	deliv := uuid.NewString()
	rec := f.postDelivery(t, githubtrigger.EventPullRequestReviewComment, deliv, raw, nil)
	require.Equal(t, http.StatusAccepted, rec.Code, "ErrAttributesCeilingExceeded must return 202 skipped, not 400: %s", rec.Body.String())

	var resp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, GitHubWebhookStatusSkipped, resp.Status)
	assert.Equal(t, deliv, resp.Delivery)
	assert.Empty(t, resp.Events)
	require.Len(t, resp.SkippedProjects, 2)
	skippedIDs := []string{resp.SkippedProjects[0].ProjectID, resp.SkippedProjects[1].ProjectID}
	assert.ElementsMatch(t, []string{projA.ID, projB.ID}, skippedIDs)
	for _, sp := range resp.SkippedProjects {
		assert.Equal(t, GitHubWebhookSkipReasonAttributesCeiling, sp.Reason)
	}

	for _, pid := range []string{projA.ID, projB.ID} {
		evs, lErr := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: pid}, store.ListOptions{})
		require.NoError(t, lErr)
		assert.Empty(t, evs.Items)
	}

	audits := parseAuditLines(t, f.auditBuf.String())
	require.Len(t, audits, 1)
	assert.Equal(t, GitHubWebhookStatusSkipped, audits[0]["result"])
	assert.Contains(t, f.auditBuf.String(), GitHubWebhookSkipReasonAttributesCeiling)
}

func TestGitHubWebhookIngress_InstallationAndPingEventsUntouched(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	ctx := context.Background()

	// Ping without target headers succeeds byte-for-byte unchanged even when both flags are enabled.
	pingBody := []byte(`{"zen":"Speak like a human."}`)
	pingRec := f.postDelivery(t, "ping", uuid.NewString(), pingBody, func(r *http.Request) {
		r.Header.Del("X-GitHub-Hook-Installation-Target-Type")
		r.Header.Del("X-GitHub-Hook-Installation-Target-ID")
	})
	require.Equal(t, http.StatusOK, pingRec.Code, pingRec.Body.String())
	assert.JSONEq(t, `{"status":"pong"}`, pingRec.Body.String())

	// Installation lifecycle event without target headers still creates installation record and no trigger_events.
	instPayload, err := json.Marshal(map[string]any{
		"action": "created",
		"installation": map[string]any{
			"id":                   int64(991122),
			"app_id":               f.appID,
			"repository_selection": "selected",
			"account": map[string]any{
				"login": "new-org",
				"type":  "Organization",
			},
		},
		"repositories": []map[string]any{
			{"id": 1, "full_name": "new-org/service", "name": "service"},
		},
	})
	require.NoError(t, err)

	instRec := f.postDelivery(t, "installation", uuid.NewString(), instPayload, func(r *http.Request) {
		r.Header.Del("X-GitHub-Hook-Installation-Target-Type")
		r.Header.Del("X-GitHub-Hook-Installation-Target-ID")
	})
	require.Equal(t, http.StatusOK, instRec.Code, instRec.Body.String())
	gotInst, err := f.store.GetGitHubInstallation(ctx, 991122)
	require.NoError(t, err)
	assert.Equal(t, store.GitHubInstallationStatusActive, gotInst.Status)

	evs, err := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, evs.Items)
}

func TestGitHubWebhookIngress_ReceiptAuditContentFree(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	_ = f.createCandidateProject(t, "audit-proj", "https://github.com/acme/widgets.git", f.instID)

	const (
		sentinelComment = "SUPER_SECRET_PROMPT_INJECTION_COMMENT_9911"
		sentinelTitle   = "CONFIDENTIAL_PR_TITLE_8822"
		sentinelBody    = "CONFIDENTIAL_PR_BODY_7733"
	)

	raw, err := json.Marshal(map[string]any{
		"action":       "created",
		"installation": map[string]any{"id": f.instID},
		"repository": map[string]any{
			"id": 101, "name": "widgets", "full_name": "acme/widgets", "private": true,
			"owner": map[string]any{"login": "acme", "id": 10, "type": "Organization"},
		},
		"sender": map[string]any{"login": "alice", "id": 202, "type": "User"},
		"issue": map[string]any{
			"number": 99, "state": "open", "title": sentinelTitle, "body": sentinelBody,
			"html_url":   "https://github.com/acme/widgets/issues/99",
			"created_at": "2026-11-18T11:00:00Z", "updated_at": "2026-11-18T11:30:00Z",
		},
		"comment": map[string]any{
			"id": 500, "body": sentinelComment,
			"html_url":   "https://github.com/acme/widgets/issues/99#issuecomment-500",
			"created_at": "2026-11-18T11:30:00Z", "updated_at": "2026-11-18T11:30:00Z",
		},
	})
	require.NoError(t, err)

	deliv := uuid.NewString()
	// Exercise accepted, duplicate, and target-mismatch audit paths.
	rec1 := f.postDelivery(t, "issue_comment", deliv, raw, nil)
	require.Equal(t, http.StatusAccepted, rec1.Code)
	rec2 := f.postDelivery(t, "issue_comment", deliv, raw, nil)
	require.Equal(t, http.StatusAccepted, rec2.Code)
	rec3 := f.postDelivery(t, "issue_comment", uuid.NewString(), raw, func(r *http.Request) {
		r.Header.Set("X-GitHub-Hook-Installation-Target-ID", "000000")
	})
	require.Equal(t, http.StatusBadRequest, rec3.Code)

	auditOut := f.auditBuf.String()
	require.NotEmpty(t, auditOut)
	for _, forbidden := range []string{
		sentinelComment,
		sentinelTitle,
		sentinelBody,
		f.webhookSecret,
	} {
		assert.NotContains(t, auditOut, forbidden, "audit output must be strictly content-free")
	}

	// O4: payload_bytes in trigger.event.received must always report the raw body length.
	for _, entry := range parseAuditLines(t, auditOut) {
		if entry["event_type"] == trigger.AuditEventReceived {
			assert.Equal(t, float64(len(raw)), entry["payload_bytes"], "payload_bytes must equal raw request body length")
		}
	}
}

type ghInvalidInputTxStore struct {
	store.Store
	fault         *storeFaultSwitch
	failProjectID string // empty fails every project when armed
}

func (w *ghInvalidInputTxStore) CountDurableQueuePending(ctx context.Context, kind, projectID string) (int64, error) {
	if c, ok := w.Store.(store.DurableQueuePendingCounter); ok {
		return c.CountDurableQueuePending(ctx, kind, projectID)
	}
	return 0, errors.New("inner store missing DurableQueuePendingCounter")
}

func (w *ghInvalidInputTxStore) CreateTriggerEvent(ctx context.Context, evt *store.TriggerEvent) error {
	if w.fault.Active() && (w.failProjectID == "" || (evt != nil && evt.ProjectID == w.failProjectID)) {
		return fmt.Errorf("%w: injected invalid trigger event", store.ErrInvalidInput)
	}
	return w.Store.CreateTriggerEvent(ctx, evt)
}

func (w *ghInvalidInputTxStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return w.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&ghInvalidInputTxStore{
			Store:         tx,
			fault:         w.fault,
			failProjectID: w.failProjectID,
		})
	})
}

func TestGitHubWebhookIngress_AmpersandLabelsCeilingAndInvalidInputSkip(t *testing.T) {
	t.Run("ampersand_heavy_labels_within_unescaped_8kib_persist_without_500", func(t *testing.T) {
		f := setupGitHubIngressFixture(t)
		ctx := context.Background()
		proj := f.createCandidateProject(t, "amp-labels-proj", "https://github.com/acme/widgets.git", f.instID)

		// 40 labels of 50 characters (mostly '&') fit under 8 KiB with
		// SetEscapeHTML(false) (~2.1 KiB) even though standard json.Marshal
		// expands each '&' to '\u0026' (> 11 KiB).
		labelObjs := make([]map[string]any, 40)
		wantLabels := make([]string, 40)
		for i := range labelObjs {
			prefix := fmt.Sprintf("L%02d", i)
			name := prefix + strings.Repeat("&", 50-len(prefix))
			wantLabels[i] = name
			labelObjs[i] = map[string]any{"id": i + 1, "name": name}
		}

		raw, err := json.Marshal(map[string]any{
			"action":       "created",
			"installation": map[string]any{"id": f.instID},
			"repository": map[string]any{
				"id": 101, "name": "widgets", "full_name": "acme/widgets", "private": false,
				"owner": map[string]any{"login": "acme", "id": 10, "type": "Organization"},
			},
			"sender": map[string]any{"login": "alice", "id": 202, "type": "User"},
			"issue": map[string]any{
				"number": 42, "state": "open", "title": "R&D labels test", "body": "body",
				"html_url":   "https://github.com/acme/widgets/issues/42",
				"created_at": "2026-11-18T11:00:00Z", "updated_at": "2026-11-18T11:30:00Z",
				"labels": labelObjs,
			},
			"comment": map[string]any{
				"id": 701, "body": "@scion-bot /ping",
				"html_url":   "https://github.com/acme/widgets/issues/42#issuecomment-701",
				"created_at": "2026-11-18T11:30:00Z", "updated_at": "2026-11-18T11:30:00Z",
			},
		})
		require.NoError(t, err)

		deliv := uuid.NewString()
		rec := f.postDelivery(t, "issue_comment", deliv, raw, nil)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		var resp GitHubWebhookAcceptedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, GitHubWebhookStatusQueued, resp.Status)
		require.Len(t, resp.Events, 1)
		assert.Empty(t, resp.SkippedProjects)

		stored, err := f.store.GetTriggerEvent(ctx, resp.Events[0].EventID)
		require.NoError(t, err)
		assert.Equal(t, proj.ID, stored.ProjectID)
		assert.Empty(t, stored.Attributes["lists_truncated"])
		assert.Equal(t, wantLabels, stored.Lists["labels"])
	})

	t.Run("store_err_invalid_input_becomes_skipped_project_with_audit_not_500", func(t *testing.T) {
		srv, rawStore, wrapper, fault := testServerWithStoreFault(t, func(inner store.Store, sw *storeFaultSwitch) *ghInvalidInputTxStore {
			return &ghInvalidInputTxStore{Store: inner, fault: sw}
		})

		const (
			testAppID  int64 = 778899
			testSecret       = "whsec-github-invalid-input-test"
			testInstID int64 = 554433
		)
		baseNow := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
		auditBuf := &ghIngressLogBuffer{}
		f := &githubIngressTestFixture{
			srv:           srv,
			store:         rawStore,
			appID:         testAppID,
			webhookSecret: testSecret,
			instID:        testInstID,
			auditBuf:      auditBuf,
			warnBuf:       &ghIngressLogBuffer{},
			now:           baseNow,
		}

		srv.mu.Lock()
		srv.config.GitHubAppConfig.AppID = testAppID
		srv.config.GitHubAppConfig.WebhookSecret = testSecret
		srv.config.GitHubAppConfig.AppSlug = "scion-bot"
		srv.config.GitHubAppConfig.WebhooksEnabled = true
		srv.mu.Unlock()

		srv.githubWebhookIngress.mu.Lock()
		srv.githubWebhookIngress.nowFn = f.currentTime
		srv.githubWebhookIngress.auditLogger = slog.New(slog.NewJSONHandler(auditBuf, nil))
		srv.githubWebhookIngress.mu.Unlock()

		setTriggerExperimentsForGitHubIngressTest(t, srv, true, true)
		require.NoError(t, rawStore.CreateGitHubInstallation(context.Background(), &store.GitHubInstallation{
			InstallationID: testInstID,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			AppID:          testAppID,
			Status:         store.GitHubInstallationStatusActive,
		}))

		projA := f.createCandidateProject(t, "inv-proj-a", "https://github.com/acme/widgets.git", testInstID)
		projB := f.createCandidateProject(t, "inv-proj-b", "https://github.com/acme/widgets.git", testInstID)

		// 1. Fail only projA: projA is skipped with invalid_event, projB still queues.
		wrapper.failProjectID = projA.ID
		fault.Arm()
		auditBuf.Reset()

		deliv1 := uuid.NewString()
		body1 := sampleIssueCommentPayload(t, testInstID, "acme/widgets", "alice", "User", "hello", baseNow)
		rec1 := f.postDelivery(t, "issue_comment", deliv1, body1, nil)
		require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())

		var resp1 GitHubWebhookAcceptedResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		assert.Equal(t, GitHubWebhookStatusQueued, resp1.Status)
		require.Len(t, resp1.Events, 1)
		assert.Equal(t, projB.ID, resp1.Events[0].ProjectID)
		require.Len(t, resp1.SkippedProjects, 1)
		assert.Equal(t, GitHubWebhookSkippedProject{
			ProjectID: projA.ID,
			Reason:    GitHubWebhookSkipReasonInvalidEvent,
		}, resp1.SkippedProjects[0])
		assert.Contains(t, auditBuf.String(), GitHubWebhookSkipReasonInvalidEvent)

		// 2. Fail all projects: top-level status is 202 skipped (not 500) + audit.
		wrapper.failProjectID = ""
		auditBuf.Reset()

		deliv2 := uuid.NewString()
		rec2 := f.postDelivery(t, "issue_comment", deliv2, body1, nil)
		require.Equal(t, http.StatusAccepted, rec2.Code, rec2.Body.String())

		var resp2 GitHubWebhookAcceptedResponse
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
		assert.Equal(t, GitHubWebhookStatusSkipped, resp2.Status)
		assert.Empty(t, resp2.Events)
		require.Len(t, resp2.SkippedProjects, 2)
		audits := parseAuditLines(t, auditBuf.String())
		require.Len(t, audits, 1)
		assert.Equal(t, GitHubWebhookStatusSkipped, audits[0]["result"])
	})
}

func TestGitHubWebhookIngress_OwnAppUnknownIdentityPerformedViaAppAndRetryBackoff(t *testing.T) {
	f := setupGitHubIngressFixture(t)
	ctx := context.Background()
	proj := f.createCandidateProject(t, "r1-ownapp-proj", "https://github.com/acme/widgets.git", f.instID)

	// Start with GET /app failing (502) and AppSlug unset so the bot login is unknown.
	mockGH := newMockAppEndpoint(t, http.StatusBadGateway, `{"message":"bad gateway"}`)
	f.srv.githubAppIdentity.reset()
	f.srv.githubAppIdentity.mu.Lock()
	f.srv.githubAppIdentity.nowFn = f.currentTime
	f.srv.githubAppIdentity.mu.Unlock()

	f.srv.mu.Lock()
	f.srv.config.GitHubAppConfig.AppSlug = ""
	f.srv.config.GitHubAppConfig.PrivateKey = testGitHubAppPrivateKeyPEM(t)
	f.srv.config.GitHubAppConfig.APIBaseURL = mockGH.server.URL
	f.srv.mu.Unlock()

	// 1. App-authored comment carrying comment.performed_via_github_app.id == AppID
	// while GET /app fails: dropped as 202 ignored/self with zero DB rows, and a
	// rate-limited warning is logged once.
	f.warnBuf.Reset()
	ownAppCommentBody, err := json.Marshal(map[string]any{
		"action":       "created",
		"installation": map[string]any{"id": f.instID},
		"repository": map[string]any{
			"id": 101, "name": "widgets", "full_name": "acme/widgets", "private": false,
			"owner": map[string]any{"login": "acme", "id": 10, "type": "Organization"},
		},
		"sender": map[string]any{"login": "unfetched-scion[bot]", "id": 9001, "type": "Bot"},
		"issue": map[string]any{
			"number": 42, "state": "open", "title": "Issue", "body": "body",
			"html_url":   "https://github.com/acme/widgets/issues/42",
			"created_at": "2026-11-18T11:00:00Z", "updated_at": "2026-11-18T11:30:00Z",
		},
		"comment": map[string]any{
			"id": 801, "body": "App acknowledgement comment",
			"html_url":                 "https://github.com/acme/widgets/issues/42#issuecomment-801",
			"created_at":               "2026-11-18T11:30:00Z",
			"updated_at":               "2026-11-18T11:30:00Z",
			"performed_via_github_app": map[string]any{"id": f.appID, "slug": "unfetched-scion"},
		},
	})
	require.NoError(t, err)

	recOwn := f.postDelivery(t, "issue_comment", uuid.NewString(), ownAppCommentBody, nil)
	require.Equal(t, http.StatusAccepted, recOwn.Code, recOwn.Body.String())

	var ownResp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(recOwn.Body.Bytes(), &ownResp))
	assert.Equal(t, GitHubWebhookStatusIgnored, ownResp.Status)
	assert.Equal(t, "self", ownResp.Reason)
	assert.Equal(t, int64(1), f.srv.githubWebhookIngress.selfIgnoredCount.Load())
	assert.Equal(t, int64(1), mockGH.hits.Load(), "initial lazy GET /app should run once")

	warnLines := parseAuditLines(t, f.warnBuf.String())
	require.Len(t, warnLines, 1, "expected 1 rate-limited warning while identity is unknown")

	evs, err := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: proj.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, evs.Items, "own-App comment must not create any trigger_events row")

	// 2. Third-party bot (renovate[bot], different performed_via_github_app.id)
	// while identity is still unknown (within 5m backoff): accepted 202 queued,
	// no second GET /app call, and warning stays rate-limited.
	f.advanceClock(time.Minute)
	otherBotBody, err := json.Marshal(map[string]any{
		"action":       "created",
		"installation": map[string]any{"id": f.instID},
		"repository": map[string]any{
			"id": 101, "name": "widgets", "full_name": "acme/widgets", "private": false,
			"owner": map[string]any{"login": "acme", "id": 10, "type": "Organization"},
		},
		"sender": map[string]any{"login": "renovate[bot]", "id": 9002, "type": "Bot"},
		"issue": map[string]any{
			"number": 42, "state": "open", "title": "Issue", "body": "body",
			"html_url":   "https://github.com/acme/widgets/issues/42",
			"created_at": "2026-11-18T11:00:00Z", "updated_at": "2026-11-18T11:30:00Z",
		},
		"comment": map[string]any{
			"id": 802, "body": "Dependency update status",
			"html_url":                 "https://github.com/acme/widgets/issues/42#issuecomment-802",
			"created_at":               "2026-11-18T11:31:00Z",
			"updated_at":               "2026-11-18T11:31:00Z",
			"performed_via_github_app": map[string]any{"id": int64(999111), "slug": "renovate"},
		},
	})
	require.NoError(t, err)

	recOther := f.postDelivery(t, "issue_comment", uuid.NewString(), otherBotBody, nil)
	require.Equal(t, http.StatusAccepted, recOther.Code, recOther.Body.String())

	var otherResp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(recOther.Body.Bytes(), &otherResp))
	assert.Equal(t, GitHubWebhookStatusQueued, otherResp.Status, "third-party bots must not be dropped while identity is unknown")
	require.Len(t, otherResp.Events, 1)
	assert.Equal(t, int64(1), mockGH.hits.Load(), "failed GET /app must remain cached within the 5m backoff")
	assert.Len(t, parseAuditLines(t, f.warnBuf.String()), 1, "unknown-identity warning must stay rate-limited within 1h")

	// 3. Upstream GET /app recovers and clock advances past githubAppIdentityFailureRetryBackoff:
	// identity fetch retries, populates BotLogin, and drops own-App by sender.login alone.
	mockGH.setPayload(t, http.StatusOK, map[string]any{
		"id":     f.appID,
		"slug":   "unfetched-scion",
		"events": []string{"issue_comment"},
	})
	f.advanceClock(githubAppIdentityFailureRetryBackoff)

	ownByLoginOnly := sampleIssueCommentPayload(t, f.instID, "acme/widgets", "unfetched-scion[bot]", "Bot", "post-recovery comment", f.currentTime())
	recRecovered := f.postDelivery(t, "issue_comment", uuid.NewString(), ownByLoginOnly, nil)
	require.Equal(t, http.StatusAccepted, recRecovered.Code, recRecovered.Body.String())

	var recovResp GitHubWebhookAcceptedResponse
	require.NoError(t, json.Unmarshal(recRecovered.Body.Bytes(), &recovResp))
	assert.Equal(t, GitHubWebhookStatusIgnored, recovResp.Status)
	assert.Equal(t, "self", recovResp.Reason)
	assert.GreaterOrEqual(t, mockGH.hits.Load(), int64(2), "GET /app must retry once the failure backoff expires")

	evsAfter, err := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: proj.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, evsAfter.Items, 1, "only the third-party bot event should exist in store")
}
