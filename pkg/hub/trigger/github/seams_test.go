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

func TestVerifyWebhookSignature(t *testing.T) {
	body := []byte(`{"action":"created"}`)
	secret := "top-secret"
	valid := signTestWebhook(body, secret)

	if !triggithub.VerifyWebhookSignature(body, valid, secret) {
		t.Fatal("expected valid lowercase signature to verify")
	}
	// Uppercase hex must be rejected identically to production.
	upper := "sha256=" + strings.ToUpper(strings.TrimPrefix(valid, "sha256="))
	if triggithub.VerifyWebhookSignature(body, upper, secret) {
		t.Fatal("expected uppercase hex signature to be rejected")
	}
	if triggithub.VerifyWebhookSignature(body, strings.TrimPrefix(valid, "sha256="), secret) {
		t.Fatal("expected signature without sha256= prefix to be rejected")
	}
	if triggithub.VerifyWebhookSignature(body, valid, "") {
		t.Fatal("expected empty secret to be rejected")
	}
	if triggithub.VerifyWebhookSignature(body, "", secret) {
		t.Fatal("expected empty signature to be rejected")
	}
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

	// Pin provider-level DedupKey format: "<lowercased-delivery-id>:<sha256-hex(body)>".
	bodySum := sha256.Sum256(body)
	wantKey := strings.ToLower(deliveryID) + ":" + hex.EncodeToString(bodySum[:])
	k1 := v.DedupKey(d, body)
	if k1 != wantKey {
		t.Fatalf("DedupKey = %q, want %q", k1, wantKey)
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

	// Uppercase hex signature rejected.
	upperSigReq := req.Clone(context.Background())
	upperSigReq.Header.Set("X-Hub-Signature-256", "sha256="+strings.ToUpper(strings.TrimPrefix(signTestWebhook(body, secret), "sha256=")))
	if _, err := v.Verify(upperSigReq, body, secrets); !errors.Is(err, triggithub.ErrInvalidWebhookSignature) {
		t.Errorf("expected ErrInvalidWebhookSignature for uppercase hex, got %v", err)
	}

	// Target mismatch rejected.
	wrongTarget := req.Clone(context.Background())
	wrongTarget.Header.Set("X-GitHub-Hook-Installation-Target-ID", "99999")
	if _, err := v.Verify(wrongTarget, body, secrets); !errors.Is(err, triggithub.ErrWebhookTargetMismatch) {
		t.Errorf("expected ErrWebhookTargetMismatch, got %v", err)
	}

	// ExpectedAppID == 0 must fail closed, never skip target check.
	zeroAppVerifier := &triggithub.WireVerifier{ExpectedAppID: 0}
	if _, err := zeroAppVerifier.Verify(req, body, secrets); !errors.Is(err, triggithub.ErrWebhookTargetMismatch) {
		t.Errorf("expected ErrWebhookTargetMismatch when ExpectedAppID == 0, got %v", err)
	}

	// ExpectedAppIDFn observes live AppID updates.
	liveAppID := int64(12345)
	liveVerifier := &triggithub.WireVerifier{
		ExpectedAppIDFn: func() int64 { return liveAppID },
		Now:             func() time.Time { return now },
	}
	if _, err := liveVerifier.Verify(req, body, secrets); err != nil {
		t.Fatalf("expected liveVerifier to accept initial AppID 12345, got %v", err)
	}
	liveAppID = 54321
	if _, err := liveVerifier.Verify(req, body, secrets); !errors.Is(err, triggithub.ErrWebhookTargetMismatch) {
		t.Errorf("expected ErrWebhookTargetMismatch after live AppID change, got %v", err)
	}

	// Single empty X-GitHub-Event header matches production step 2 (passes with
	// empty Event so IsSupportedEvent("") == false -> 200 ignored).
	emptyEventReq := req.Clone(context.Background())
	emptyEventReq.Header.Set("X-GitHub-Event", "")
	emptyEventReq.Header.Del("X-GitHub-Delivery")
	dEmpty, err := v.Verify(emptyEventReq, body, secrets)
	if err != nil {
		t.Fatalf("expected single empty X-GitHub-Event to pass Verify like production, got %v", err)
	}
	if dEmpty.Event != "" || triggithub.IsSupportedEvent(dEmpty.Event) {
		t.Errorf("unexpected empty-event delivery: %+v", dEmpty)
	}

	// Duplicate X-GitHub-Event header rejected.
	dupEventReq := req.Clone(context.Background())
	dupEventReq.Header.Add("X-GitHub-Event", triggithub.EventIssues)
	if _, err := v.Verify(dupEventReq, body, secrets); !errors.Is(err, triggithub.ErrInvalidWebhookHeaders) {
		t.Errorf("expected ErrInvalidWebhookHeaders for duplicate X-GitHub-Event, got %v", err)
	}

	// Exported helpers WebhookTargetMatches and IsDeliveryGUID.
	if !triggithub.WebhookTargetMatches(req.Header, 12345) {
		t.Error("expected WebhookTargetMatches to accept matching headers")
	}
	if triggithub.WebhookTargetMatches(req.Header, 0) || triggithub.WebhookTargetMatches(nil, 12345) {
		t.Error("expected WebhookTargetMatches to fail closed on 0 appID or nil headers")
	}
	if !triggithub.IsDeliveryGUID("11111111-2222-3333-4444-555555555555") || triggithub.IsDeliveryGUID("not-a-guid") {
		t.Error("unexpected IsDeliveryGUID result")
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
	if !ev.ReceivedAt.Equal(now) {
		t.Errorf("ReceivedAt = %v, want %v", ev.ReceivedAt, now)
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

func TestTokenMinter(t *testing.T) {
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

	// Nil callback returns ErrNotConfigured.
	empty := &triggithub.TokenMinter{}
	if _, err := empty.Mint(context.Background(), source.Target{ProjectID: "p1"}, source.CredentialScope{}); !errors.Is(err, triggithub.ErrNotConfigured) {
		t.Errorf("expected ErrNotConfigured on Mint, got %v", err)
	}
}

func TestReadinessChecker(t *testing.T) {
	checked := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	checker := &triggithub.ReadinessChecker{
		CheckFunc: func(context.Context) (source.Report, error) {
			return source.Report{
				Ready:              true,
				WebhooksEnabled:    true,
				HasWebhookSecret:   true,
				AppSlug:            "scion-app",
				MissingEvents:      []string{},
				MissingPermissions: []string{},
				CheckedAt:          &checked,
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

	var nilChecker triggithub.ReadinessChecker
	if _, err := nilChecker.Check(context.Background()); !errors.Is(err, triggithub.ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
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
