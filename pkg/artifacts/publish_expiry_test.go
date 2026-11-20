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

package artifacts

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestParseExpiresIn(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantDur time.Duration
		never   bool
		wantErr bool
	}{
		{in: "", wantDur: 0, never: false},
		{in: "   ", wantDur: 0, never: false},
		{in: "never", never: true},
		{in: "NEVER", never: true},
		{in: " 24h ", wantDur: 24 * time.Hour},
		{in: "720h", wantDur: 720 * time.Hour},
		{in: "48h30m", wantDur: 48*time.Hour + 30*time.Minute},
		{in: "1d", wantDur: 24 * time.Hour},
		{in: "30d", wantDur: 30 * 24 * time.Hour},
		{in: "30D", wantDur: 30 * 24 * time.Hour},
		{in: "0h", wantErr: true},
		{in: "-24h", wantErr: true},
		{in: "0d", wantErr: true},
		{in: "-1d", wantErr: true},
		{in: "+1d", wantErr: true},
		{in: "1.5d", wantErr: true},
		{in: "d", wantErr: true},
		{in: "999999d", wantErr: true},
		{in: "bogus", wantErr: true},
	} {
		dur, never, err := ParseExpiresIn(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseExpiresIn(%q) = (%v, %v, nil), want error", tc.in, dur, never)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseExpiresIn(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if dur != tc.wantDur || never != tc.never {
			t.Errorf("ParseExpiresIn(%q) = (%v, %v), want (%v, %v)", tc.in, dur, never, tc.wantDur, tc.never)
		}
	}
}

func TestPublishSingleFileExpiresIn(t *testing.T) {
	ctx := context.Background()

	t.Run("default retention 0 allows agent duration and never", func(t *testing.T) {
		f := newFixture(t, false)
		f.svc.SetLimits(func(context.Context) Limits { return Limits{DefaultRetention: 0} })

		before := time.Now().UTC()
		resp := f.publish(agentA, "demo.md", []byte("hi"), "expires_in=720h")
		after := time.Now().UTC()
		if resp.Artifact.ExpiresAt == nil {
			t.Fatal("expected ExpiresAt to be set")
		}
		if resp.Artifact.ExpiresAt.Before(before.Add(720*time.Hour-time.Second)) || resp.Artifact.ExpiresAt.After(after.Add(720*time.Hour+time.Second)) {
			t.Errorf("ExpiresAt = %v, want ~now+720h", resp.Artifact.ExpiresAt)
		}
		events, err := f.store.ListEvents(ctx, resp.Artifact.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Action != EventExpirySet || events[0].ActorKind != PrincipalKindAgent || events[0].ActorRef != agentA.ref || events[0].Authority != AuthorityOwner {
			t.Errorf("events = %+v, want single %s by agent:%s owner", events, EventExpirySet, agentA.ref)
		}

		// Agent can set "never" when DefaultRetention == 0.
		respNever := f.publish(agentA, "forever.md", []byte("hi"), "expires_in=never")
		if respNever.Artifact.ExpiresAt != nil {
			t.Errorf("ExpiresAt = %v, want nil for never", respNever.Artifact.ExpiresAt)
		}
		events, err = f.store.ListEvents(ctx, respNever.Artifact.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Action != EventExpiryCleared || events[0].ActorKind != PrincipalKindAgent || events[0].Authority != AuthorityOwner {
			t.Errorf("events = %+v, want single %s by agent owner", events, EventExpiryCleared)
		}

		// Agent at 24h floor succeeds; below 24h is refused.
		resp24 := f.publish(agentA, "day.md", []byte("hi"), "expires_in=24h")
		if resp24.Artifact.ExpiresAt == nil {
			t.Error("expected ExpiresAt for 24h")
		}
		if rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts?name=short.md&expires_in=23h59m", []byte("hi"), nil); rec.Code != http.StatusBadRequest {
			t.Errorf("below 24h status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("positive default retention caps agent and forbids never", func(t *testing.T) {
		f := newFixture(t, false)
		f.svc.SetLimits(func(context.Context) Limits { return Limits{DefaultRetention: 7 * 24 * time.Hour} })

		// Shortening within [24h, 7d] succeeds.
		before := time.Now().UTC()
		resp := f.publish(agentA, "short.md", []byte("hi"), "expires_in=48h")
		if resp.Artifact.ExpiresAt == nil || resp.Artifact.ExpiresAt.Before(before.Add(48*time.Hour-time.Second)) {
			t.Errorf("ExpiresAt = %v, want ~now+48h", resp.Artifact.ExpiresAt)
		}
		events, err := f.store.ListEvents(ctx, resp.Artifact.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Action != EventExpirySet || events[0].ActorKind != PrincipalKindAgent || events[0].Authority != AuthorityOwner {
			t.Errorf("events = %+v, want single %s by agent owner", events, EventExpirySet)
		}

		// Exact default retention succeeds.
		respEq := f.publish(agentA, "exact.md", []byte("hi"), "expires_in=7d")
		if respEq.Artifact.ExpiresAt == nil {
			t.Error("expected ExpiresAt for 7d")
		}

		// Exceeding default retention or "never" is refused for agent.
		if rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts?name=long.md&expires_in=169h", []byte("hi"), nil); rec.Code != http.StatusBadRequest {
			t.Errorf("above default retention status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
		if rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts?name=never.md&expires_in=never", []byte("hi"), nil); rec.Code != http.StatusBadRequest {
			t.Errorf("never with positive default retention status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}

		// Without expires_in, behaves as today: default retention applied, no expiry event.
		respDefault := f.publish(agentA, "def.md", []byte("hi"), "")
		if respDefault.Artifact.ExpiresAt == nil {
			t.Fatal("expected default retention ExpiresAt")
		}
		events, err = f.store.ListEvents(ctx, respDefault.Artifact.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 0 {
			t.Errorf("default publish events = %+v, want none", events)
		}

		// User can set <24h, >default, and never.
		respUserShort := f.publish(userU, "u1.md", []byte("hi"), "scope=project-1&expires_in=1h")
		if respUserShort.Artifact.ExpiresAt == nil {
			t.Error("expected ExpiresAt for user 1h")
		}
		respUserNever := f.publish(userU, "u2.md", []byte("hi"), "scope=project-1&expires_in=never")
		if respUserNever.Artifact.ExpiresAt != nil {
			t.Errorf("user never ExpiresAt = %v, want nil", respUserNever.Artifact.ExpiresAt)
		}
		events, err = f.store.ListEvents(ctx, respUserNever.Artifact.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Action != EventExpiryCleared || events[0].ActorKind != PrincipalKindUser || events[0].Authority != AuthorityOwner {
			t.Errorf("user never events = %+v", events)
		}
	})

	t.Run("invalid expires_in rejected", func(t *testing.T) {
		f := newFixture(t, false)
		for _, bad := range []string{"0h", "-1h", "bogus", "999999d"} {
			if rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts?name=a.md&expires_in="+bad, []byte("hi"), nil); rec.Code != http.StatusBadRequest {
				t.Errorf("expires_in=%q status = %d, want 400", bad, rec.Code)
			}
		}
	})
}

func TestPublishManifestAndKeyedExpiresIn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, false)
	f.svc.SetLimits(func(context.Context) Limits { return Limits{DefaultRetention: 30 * 24 * time.Hour} })

	body1 := []byte("v1")
	manifest := func(key, expiresIn string, body []byte) []byte {
		req := CreateVersionRequest{
			Title:     "Demo",
			Key:       key,
			Entry:     "demo.md",
			ExpiresIn: expiresIn,
			Files:     []ManifestFile{{Path: "demo.md", Size: int64(len(body)), SHA256: sha(body)}},
		}
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Create keyed artifact with expiresIn=720h (30d == default retention).
	rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts", manifest("demo-key", "720h", body1), map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create v1 status = %d: %s", rec.Code, rec.Body.String())
	}
	var pend1 PendingVersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pend1); err != nil {
		t.Fatal(err)
	}
	if pend1.Artifact.ExpiresAt == nil {
		t.Fatal("v1 ExpiresAt is nil")
	}
	v1Exp := *pend1.Artifact.ExpiresAt

	// Upload and finalize v1.
	rec = f.do(&agentA, http.MethodPut, "/api/v1/artifacts/"+pend1.Artifact.ID+"/versions/1/files/demo.md", body1, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("upload v1 status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = f.do(&agentA, http.MethodPost, "/api/v1/artifacts/"+pend1.Artifact.ID+"/versions/1/finalize", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("finalize v1 status = %d: %s", rec.Code, rec.Body.String())
	}

	// Re-publish same key with shorter expiresIn=48h -> updates artifact expiry and records event.
	body2 := []byte("v2")
	before := time.Now().UTC()
	rec = f.do(&agentA, http.MethodPost, "/api/v1/artifacts", manifest("demo-key", "48h", body2), map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create v2 status = %d: %s", rec.Code, rec.Body.String())
	}
	var pend2 PendingVersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pend2); err != nil {
		t.Fatal(err)
	}
	if pend2.Artifact.ID != pend1.Artifact.ID {
		t.Fatalf("v2 created new artifact %s, want %s", pend2.Artifact.ID, pend1.Artifact.ID)
	}
	if pend2.Artifact.ExpiresAt == nil || !pend2.Artifact.ExpiresAt.Before(v1Exp) || pend2.Artifact.ExpiresAt.Before(before.Add(48*time.Hour-time.Second)) {
		t.Errorf("v2 ExpiresAt = %v, want ~now+48h (< %v)", pend2.Artifact.ExpiresAt, v1Exp)
	}
	events, err := f.store.ListEvents(ctx, pend1.Artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Action != EventExpirySet || events[1].ActorKind != PrincipalKindAgent || events[1].Authority != AuthorityOwner {
		t.Errorf("events after v2 = %+v, want 2 %s events by agent owner", events, EventExpirySet)
	}

	// Re-publish same key with disallowed expiresIn (<24h or >30d or never) -> 400, no new version.
	for _, bad := range []string{"12h", "721h", "never"} {
		rec = f.do(&agentA, http.MethodPost, "/api/v1/artifacts", manifest("demo-key", bad, []byte("v3")), map[string]string{"Content-Type": "application/json"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("keyed expiresIn=%q status = %d, want 400 (%s)", bad, rec.Code, rec.Body.String())
		}
	}

	// Re-publish same key WITHOUT expiresIn -> keeps v2 expiry, no new expiry event.
	rec = f.do(&agentA, http.MethodPost, "/api/v1/artifacts", manifest("demo-key", "", []byte("v3")), map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create v3 status = %d: %s", rec.Code, rec.Body.String())
	}
	var pend3 PendingVersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pend3); err != nil {
		t.Fatal(err)
	}
	if pend3.Artifact.ExpiresAt == nil || !pend3.Artifact.ExpiresAt.Equal(*pend2.Artifact.ExpiresAt) {
		t.Errorf("v3 ExpiresAt = %v, want unchanged %v", pend3.Artifact.ExpiresAt, pend2.Artifact.ExpiresAt)
	}
	events, err = f.store.ListEvents(ctx, pend1.Artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Errorf("events after v3 without expiresIn = %d, want 2", len(events))
	}

	// POST /{id}/versions refuses expiresIn.
	vReq, _ := json.Marshal(CreateVersionRequest{
		Entry:     "demo.md",
		ExpiresIn: "48h",
		Files:     []ManifestFile{{Path: "demo.md", Size: 2, SHA256: sha([]byte("v4"))}},
	})
	rec = f.do(&agentA, http.MethodPost, "/api/v1/artifacts/"+pend1.Artifact.ID+"/versions", vReq, map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /{id}/versions with expiresIn status = %d, want 400", rec.Code)
	}
}
