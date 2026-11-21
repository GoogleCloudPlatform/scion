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

package github_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	triggithub "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
)

func signTestWebhook(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWireVerifier_VerifyAndDedupKey(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	v := &triggithub.WireVerifier{
		ExpectedAppID: 12345,
		Now:           func() time.Time { return now },
	}
	secret := "top-secret"
	body := []byte(`{"action":"created"}`)
	deliveryID := "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", nil)
	req.Header.Set("X-Hub-Signature-256", signTestWebhook(body, secret))
	req.Header.Set("X-GitHub-Event", triggithub.EventIssueComment)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "12345")

	secrets := source.SecretFunc(func(context.Context) (string, error) { return secret, nil })
	d, err := v.Verify(req, body, secrets)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if d.ID != strings.ToLower(deliveryID) {
		t.Errorf("Delivery.ID = %q, want %q", d.ID, strings.ToLower(deliveryID))
	}
	if d.TargetID != "12345" || d.TargetType != "integration" {
		t.Errorf("unexpected target (%q, %q)", d.TargetType, d.TargetID)
	}
	if d.Event != triggithub.EventIssueComment {
		t.Errorf("Delivery.Event = %q, want %q", d.Event, triggithub.EventIssueComment)
	}
	if !d.ReceivedAt.Equal(now) {
		t.Errorf("Delivery.ReceivedAt = %v, want %v", d.ReceivedAt, now)
	}

	k1 := v.DedupKey(d, body)
	k2 := v.DedupKey(d, body)
	if k1 == "" || k1 != k2 {
		t.Fatalf("expected deterministic non-empty DedupKey, got %q / %q", k1, k2)
	}
	if !strings.HasPrefix(k1, strings.ToLower(deliveryID)+":") {
		t.Errorf("DedupKey %q missing delivery ID prefix", k1)
	}
	if k3 := v.DedupKey(d, []byte(`{"action":"edited"}`)); k3 == k1 {
		t.Errorf("DedupKey must change when body changes")
	}

	// Bad signature rejected.
	badSigReq := req.Clone(context.Background())
	badSigReq.Header.Set("X-Hub-Signature-256", "sha256=0000")
	if _, err := v.Verify(badSigReq, body, secrets); !errors.Is(err, triggithub.ErrInvalidWebhookSignature) {
		t.Errorf("expected ErrInvalidWebhookSignature, got %v", err)
	}

	// Target mismatch rejected.
	wrongTarget := req.Clone(context.Background())
	wrongTarget.Header.Set("X-GitHub-Hook-Installation-Target-ID", "99999")
	if _, err := v.Verify(wrongTarget, body, secrets); !errors.Is(err, triggithub.ErrWebhookTargetMismatch) {
		t.Errorf("expected ErrWebhookTargetMismatch, got %v", err)
	}
}

func TestInstallationResolver(t *testing.T) {
	var lifecycleCalled bool
	resolver := &triggithub.InstallationResolver{
		LookupInstallation: func(_ context.Context, installationID int64) (source.Installation, error) {
			if installationID != 77 {
				t.Fatalf("unexpected installationID %d", installationID)
			}
			return source.Installation{ID: "77", Account: "acme", Active: true}, nil
		},
		ResolveTargets: func(_ context.Context, inst source.Installation, repo string) ([]source.Target, error) {
			return []source.Target{{ProjectID: "proj-1", InstallationID: inst.ID, Resource: repo, RoutingStatus: "ok"}}, nil
		},
		HandleLifecycle: func(_ context.Context, d source.Delivery, _ []byte) (bool, error) {
			lifecycleCalled = true
			return true, nil
		},
	}

	ctx := context.Background()
	payload := []byte(`{"installation":{"id":77},"repository":{"full_name":"acme/widgets"}}`)
	inst, err := resolver.Installation(ctx, source.Delivery{Event: triggithub.EventIssueComment}, payload)
	if err != nil {
		t.Fatalf("Installation failed: %v", err)
	}
	if inst.ID != "77" || !inst.Active {
		t.Fatalf("unexpected installation: %+v", inst)
	}

	targets, err := resolver.Targets(ctx, inst, payload)
	if err != nil {
		t.Fatalf("Targets failed: %v", err)
	}
	if len(targets) != 1 || targets[0].ProjectID != "proj-1" || targets[0].Resource != "acme/widgets" {
		t.Fatalf("unexpected targets: %+v", targets)
	}

	// Content event is not a lifecycle event.
	handled, err := resolver.OnLifecycle(ctx, source.Delivery{Event: triggithub.EventIssueComment}, payload)
	if err != nil || handled || lifecycleCalled {
		t.Fatalf("content event should not be handled by OnLifecycle: handled=%v err=%v", handled, err)
	}

	// Lifecycle event is handled.
	handled, err = resolver.OnLifecycle(ctx, source.Delivery{Event: triggithub.EventInstallation}, payload)
	if err != nil || !handled || !lifecycleCalled {
		t.Fatalf("installation event should be handled by OnLifecycle: handled=%v err=%v", handled, err)
	}
}

func TestEventNormalizer(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	norm := &triggithub.EventNormalizer{
		Options: triggithub.Options{
			AppSlug:    "scion-bot",
			ReceivedAt: now,
		},
	}

	body := []byte(`{
		"action": "created",
		"installation": {"id": 42},
		"repository": {
			"id": 1,
			"full_name": "acme/widgets",
			"name": "widgets",
			"private": false,
			"default_branch": "main",
			"owner": {"login": "acme", "id": 2, "type": "Organization"}
		},
		"sender": {"login": "alice", "id": 10, "type": "User"},
		"issue": {
			"number": 7,
			"title": "Fix bug",
			"state": "open",
			"html_url": "https://github.com/acme/widgets/issues/7",
			"user": {"login": "bob", "id": 11, "type": "User"}
		},
		"comment": {
			"id": 99,
			"body": "@scion-bot /review please",
			"author_association": "MEMBER",
			"html_url": "https://github.com/acme/widgets/issues/7#issuecomment-99",
			"created_at": "2026-11-21T11:55:00Z",
			"user": {"login": "alice", "id": 10, "type": "User"}
		}
	}`)

	ev, err := norm.Normalize(
		context.Background(),
		source.Delivery{ID: "deliv-1", Event: triggithub.EventIssueComment, ReceivedAt: now},
		body,
		source.Target{ProjectID: "proj-a"},
	)
	if err != nil {
		t.Fatalf("Normalize failed: %v", err)
	}
	if !ev.Handled || ev.SourceKind != triggithub.SourceKind || ev.Type != "github.issue_comment.created" {
		t.Fatalf("unexpected normalised event: %+v", ev)
	}
	if ev.DedupKey != "deliv-1/proj-a" {
		t.Errorf("DedupKey = %q, want %q", ev.DedupKey, "deliv-1/proj-a")
	}
	if ev.Attributes["command"] != "review" {
		t.Errorf("command = %q, want review", ev.Attributes["command"])
	}
	if ev.Sender.Login != "alice" || ev.Sender.Type != "User" {
		t.Errorf("unexpected Sender: %+v", ev.Sender)
	}
}

func TestAppActorIdentity(t *testing.T) {
	id := &triggithub.AppActorIdentity{
		AppSlug: "scion-app",
		AppID:   4242,
	}
	ctx := context.Background()

	if !id.IsSelf(ctx, source.Principal{Type: "Bot", Login: "scion-app[bot]"}) {
		t.Error("expected own bot login to match IsSelf")
	}
	if !id.IsSelf(ctx, source.Principal{Type: "User", Login: "someone", PerformedViaAppID: "4242"}) {
		t.Error("expected matching performed_via_github_app.id to match IsSelf")
	}
	if id.IsSelf(ctx, source.Principal{Type: "Bot", Login: "dependabot[bot]"}) {
		t.Error("third-party bot must not match IsSelf")
	}
}

func TestTokenMinterAndCommentAction(t *testing.T) {
	exp := time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC)
	minter := &triggithub.TokenMinter{
		MintFunc: func(_ context.Context, tgt source.Target, scope source.CredentialScope) (source.Credential, error) {
			return source.Credential{
				Token:     "ghs_test",
				ExpiresAt: exp,
				Scope:     scope.Permissions,
			}, nil
		},
	}

	cred, err := minter.Mint(context.Background(), source.Target{ProjectID: "p1"}, source.CredentialScope{
		Permissions: map[string]string{"issues": "write"},
	})
	if err != nil {
		t.Fatalf("Mint failed: %v", err)
	}
	if cred.Token != "ghs_test" || !cred.ExpiresAt.Equal(exp) {
		t.Fatalf("unexpected credential: %+v", cred)
	}

	rateLimited := false
	action := &triggithub.CommentAction{
		DefaultMinter: minter,
		ValidateFunc: func(_ context.Context, req source.ActionValidateRequest) (string, error) {
			return req.Payload, nil
		},
		AllowRate: func(string) error {
			if rateLimited {
				return errors.New("comment rate limited")
			}
			return nil
		},
		FireFunc: func(ctx context.Context, req source.ActionFireRequest) (source.ActionFireResult, error) {
			if req.Minter == nil {
				t.Fatal("expected DefaultMinter to be injected")
			}
			c, merr := req.Minter.Mint(ctx, req.Target, source.CredentialScope{Permissions: map[string]string{"issues": "write"}})
			if merr != nil || c.Token != "ghs_test" {
				t.Fatalf("mint failed: %v", merr)
			}
			return source.ActionFireResult{Status: "fired", ResultRef: "555"}, nil
		},
	}

	if action.Kind() != triggithub.ActionKindGitHubComment {
		t.Fatalf("Kind = %q, want %q", action.Kind(), triggithub.ActionKindGitHubComment)
	}
	if norm, err := action.Validate(context.Background(), source.ActionValidateRequest{ProjectID: "p1", Payload: `{"body":"hi"}`}); err != nil || norm != `{"body":"hi"}` {
		t.Fatalf("Validate = (%q, %v)", norm, err)
	}
	res, err := action.Fire(context.Background(), source.ActionFireRequest{ProjectID: "p1", FireID: "f1"})
	if err != nil || res.Status != "fired" || res.ResultRef != "555" {
		t.Fatalf("Fire = (%+v, %v)", res, err)
	}

	rateLimited = true
	res, err = action.Fire(context.Background(), source.ActionFireRequest{ProjectID: "p1", FireID: "f2"})
	if err != nil || res.Status != "failed" || res.Error != "comment rate limited" {
		t.Fatalf("expected rate-limited failure, got (%+v, %v)", res, err)
	}
}

func TestReadinessChecker(t *testing.T) {
	checked := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	checker := &triggithub.ReadinessChecker{
		Snapshot: func(context.Context) (triggithub.ReadinessSnapshot, error) {
			return triggithub.ReadinessSnapshot{
				Configured:       true,
				WebhooksEnabled:  true,
				HasWebhookSecret: true,
				AppSlug:          "scion-app",
				Events:           triggithub.RequiredEvents,
				Permissions: map[string]string{
					"contents":      "write",
					"issues":        "write",
					"metadata":      "read",
					"pull_requests": "write",
				},
				CheckedAt: &checked,
			}, nil
		},
	}

	rep, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if !rep.Ready || len(rep.MissingEvents) != 0 || len(rep.MissingPermissions) != 0 {
		t.Fatalf("expected ready report, got %+v", rep)
	}

	// Missing event + downgraded permission -> not ready.
	partial := triggithub.EvaluateReadiness(triggithub.ReadinessSnapshot{
		Configured:       true,
		WebhooksEnabled:  true,
		HasWebhookSecret: true,
		Events:           []string{triggithub.EventPullRequest},
		Permissions:      map[string]string{"issues": "read", "metadata": "read"},
	})
	if partial.Ready {
		t.Fatalf("expected not ready, got %+v", partial)
	}
	if len(partial.MissingEvents) != 4 {
		t.Errorf("MissingEvents = %v, want 4 missing", partial.MissingEvents)
	}
	if len(partial.MissingPermissions) != 3 {
		t.Errorf("MissingPermissions = %v, want 3 missing", partial.MissingPermissions)
	}
}

func TestPresetCatalog(t *testing.T) {
	cat := &triggithub.PresetCatalog{}
	presets := cat.List()
	if len(presets) == 0 {
		t.Fatal("expected non-empty preset list")
	}

	var cmdPreset source.Preset
	for _, p := range presets {
		if p.ID == triggithub.PresetCommand {
			cmdPreset = p
			break
		}
	}
	if cmdPreset.ID == "" {
		t.Fatalf("preset %q not found in catalog", triggithub.PresetCommand)
	}

	match, suggested, err := cat.Compile(cmdPreset, map[string]any{"command": "review"})
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	if len(match.Types) == 0 || len(match.Rules) == 0 {
		t.Fatalf("expected compiled match types and rules, got %+v", match)
	}
	if suggested.Type != triggithub.ActionKindDispatchAgent {
		t.Errorf("SuggestedAction.Type = %q, want %q", suggested.Type, triggithub.ActionKindDispatchAgent)
	}
}
