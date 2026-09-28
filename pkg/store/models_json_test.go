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

package store

import (
	"encoding/json"
	"sort"
	"testing"
)

// legacyGroveKeys are grove-named JSON keys that no store model may emit or
// recognize.
var legacyGroveKeys = []string{"groveId", "groveName", "grove", "groves"}

// assertNoLegacyKeys fails the test if any legacy grove-named key appears in
// the given JSON-encoded object. Included alongside assertExactKeySet below:
// it is redundant with the exact key-set check, but it names the legacy
// grove keys explicitly, which makes a failure easier to read.
func assertNoLegacyKeys(t *testing.T, label string, data []byte) {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: unmarshal to map failed: %v", label, err)
	}
	for _, k := range legacyGroveKeys {
		if v, ok := m[k]; ok {
			t.Errorf("%s: unexpected legacy key %q in JSON output (value %v): %s", label, k, v, data)
		}
	}
}

// assertExactKeySet fails the test unless the JSON-encoded object's key set
// is exactly want (order-independent). This is a "golden" check: unlike a
// legacy-key denylist, it also catches any other unexpected key a
// regression might introduce (a re-added alias under a different name, a
// wrapper-struct leak, a typo'd tag, etc.), not just the four grove-named
// ones.
func assertExactKeySet(t *testing.T, label string, data []byte, want []string) {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: unmarshal to map failed: %v", label, err)
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)

	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)

	if len(got) != len(wantSorted) {
		t.Fatalf("%s: key set = %v (%d keys), want %v (%d keys)", label, got, len(got), wantSorted, len(wantSorted))
	}
	for i := range got {
		if got[i] != wantSorted[i] {
			t.Fatalf("%s: key set = %v, want %v", label, got, wantSorted)
		}
	}
}

func TestAgent_JSON_CanonicalKeysOnly(t *testing.T) {
	a := Agent{
		ID:           "a-1",
		Slug:         "a-1-slug",
		Name:         "agent-name",
		Template:     "claude",
		ProjectID:    "p-1",
		Detached:     false,
		MessageMode:  "none",
		StateVersion: 1,
	}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "Agent", data)
	// Note: encoding/json's "omitempty" has no effect on non-pointer
	// time.Time fields (it only recognizes false/0/nil/empty-length kinds,
	// and a zero time.Time struct is none of those), so StartedAt,
	// LastSeen, LastActivityEvent and DeletedAt are always present
	// alongside Created/Updated despite carrying an "omitempty" tag.
	assertExactKeySet(t, "Agent", data, []string{
		"id", "slug", "name", "template", "projectId", "detached",
		"created", "updated", "startedAt", "lastSeen", "lastActivityEvent",
		"deletedAt", "messageMode", "stateVersion", "generation",
	})

	var m map[string]interface{}
	_ = json.Unmarshal(data, &m)
	if m["projectId"] != "p-1" {
		t.Errorf("projectId = %v, want %q", m["projectId"], "p-1")
	}

	// Canonical round trip: marshal then unmarshal back into the same type.
	var rt Agent
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != a.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, a.ProjectID)
	}

	// The legacy decode fallback is gone: a bare groveId is no longer
	// recognized, and ProjectID stays empty.
	var decoded Agent
	if err := json.Unmarshal([]byte(`{"id":"a-2","groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}

func TestProject_JSON_CanonicalKeysOnly(t *testing.T) {
	p := Project{ID: "p-1", Name: "proj", Slug: "proj-slug"}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "Project", data)
	assertExactKeySet(t, "Project", data, []string{
		"id", "name", "slug", "created", "updated",
	})

	var m map[string]interface{}
	_ = json.Unmarshal(data, &m)
	if m["id"] != "p-1" || m["name"] != "proj" || m["slug"] != "proj-slug" {
		t.Errorf("canonical fields = %+v, want id=p-1 name=proj slug=proj-slug", m)
	}

	var rt Project
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ID != p.ID || rt.Name != p.Name || rt.Slug != p.Slug {
		t.Errorf("round-trip = %+v, want id=%s name=%s slug=%s", rt, p.ID, p.Name, p.Slug)
	}

	var decoded Project
	if err := json.Unmarshal([]byte(`{"groveId":"legacy-id","groveName":"legacy-name","grove":"legacy-slug"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ID != "" || decoded.Name != "" || decoded.Slug != "" {
		t.Errorf("decoded = %+v, want zero value (legacy grove fields must not be honored)", decoded)
	}
}

func TestProjectProvider_JSON_CanonicalKeysOnly(t *testing.T) {
	pp := ProjectProvider{ProjectID: "p-1", BrokerID: "b-1", BrokerName: "broker", Status: "online"}
	data, err := json.Marshal(pp)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "ProjectProvider", data)
	// LastSeen/LinkedAt are non-pointer time.Time with "omitempty", which
	// encoding/json does not honor for struct-valued fields — see the note
	// in TestAgent_JSON_CanonicalKeysOnly.
	assertExactKeySet(t, "ProjectProvider", data, []string{
		"projectId", "brokerId", "brokerName", "status", "lastSeen", "linkedAt",
	})

	var rt ProjectProvider
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != pp.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, pp.ProjectID)
	}

	var decoded ProjectProvider
	if err := json.Unmarshal([]byte(`{"groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}

func TestTemplate_JSON_CanonicalKeysOnly(t *testing.T) {
	tmpl := Template{
		ID:        "t-1",
		Name:      "tmpl",
		Slug:      "tmpl-slug",
		Harness:   "claude",
		Image:     "img",
		ProjectID: "p-1",
		Scope:     "project",
		Status:    "active",
	}
	data, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "Template", data)
	assertExactKeySet(t, "Template", data, []string{
		"id", "name", "slug", "harness", "image", "scope", "projectId",
		"status", "created", "updated",
	})

	var m map[string]interface{}
	_ = json.Unmarshal(data, &m)
	if m["projectId"] != "p-1" {
		t.Errorf("projectId = %v, want %q", m["projectId"], "p-1")
	}

	// A global template (no ProjectID) must omit "projectId" entirely
	// (omitempty). Nothing else in this file exercises a Template with an
	// empty ProjectID.
	global := Template{
		ID:      "t-global",
		Name:    "global-tmpl",
		Slug:    "global-tmpl-slug",
		Harness: "claude",
		Image:   "img",
		Scope:   "global",
		Status:  "active",
	}
	globalData, err := json.Marshal(global)
	if err != nil {
		t.Fatalf("Marshal (global) failed: %v", err)
	}
	assertNoLegacyKeys(t, "Template (global)", globalData)
	assertExactKeySet(t, "Template (global)", globalData, []string{
		"id", "name", "slug", "harness", "image", "scope", "status", "created", "updated",
	})

	var rt Template
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != tmpl.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, tmpl.ProjectID)
	}

	var decoded Template
	if err := json.Unmarshal([]byte(`{"id":"t-2","groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}

func TestNotificationSubscription_JSON_CanonicalKeysOnly(t *testing.T) {
	s := NotificationSubscription{
		ID: "s-1", Scope: "project", SubscriberType: "user", SubscriberID: "u-1",
		ProjectID: "p-1", TriggerActivities: []string{"COMPLETED"}, CreatedBy: "creator",
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "NotificationSubscription", data)
	assertExactKeySet(t, "NotificationSubscription", data, []string{
		"id", "scope", "subscriberType", "subscriberId", "projectId",
		"triggerActivities", "createdAt", "createdBy",
	})

	var rt NotificationSubscription
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != s.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, s.ProjectID)
	}

	var decoded NotificationSubscription
	if err := json.Unmarshal([]byte(`{"id":"s-2","groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}

func TestSubscriptionTemplate_JSON_CanonicalKeysOnly(t *testing.T) {
	st := SubscriptionTemplate{
		ID: "st-1", Name: "test-sub-template", Scope: "project",
		TriggerActivities: []string{"COMPLETED"}, ProjectID: "p-1", CreatedBy: "creator",
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "SubscriptionTemplate", data)
	assertExactKeySet(t, "SubscriptionTemplate", data, []string{
		"id", "name", "scope", "triggerActivities", "projectId", "createdBy",
	})

	var m map[string]interface{}
	_ = json.Unmarshal(data, &m)
	if m["projectId"] != "p-1" {
		t.Errorf("projectId = %v, want %q", m["projectId"], "p-1")
	}

	var rt SubscriptionTemplate
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != st.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, st.ProjectID)
	}

	var decoded SubscriptionTemplate
	if err := json.Unmarshal([]byte(`{"id":"st-2","name":"test-sub","groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}

func TestNotification_JSON_CanonicalKeysOnly(t *testing.T) {
	n := Notification{
		ID: "n-1", SubscriptionID: "sub-1", AgentID: "agent-1", ProjectID: "p-1",
		SubscriberType: "user", SubscriberID: "u-1", Status: "PENDING",
		Message: "hi", Dispatched: true, Acknowledged: false,
	}
	data, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "Notification", data)
	assertExactKeySet(t, "Notification", data, []string{
		"id", "subscriptionId", "agentId", "projectId", "subscriberType",
		"subscriberId", "status", "message", "dispatched", "acknowledged", "createdAt",
	})

	var rt Notification
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != n.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, n.ProjectID)
	}

	var decoded Notification
	if err := json.Unmarshal([]byte(`{"id":"n-2","groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}

func TestUserAccessToken_JSON_CanonicalKeysOnly(t *testing.T) {
	tok := UserAccessToken{
		ID: "uat-1", UserID: "u-1", Name: "tok", Prefix: "scion_pat_ab",
		ProjectID: "p-1", Scopes: []string{"agent:read"}, Revoked: false,
	}
	data, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "UserAccessToken", data)
	assertExactKeySet(t, "UserAccessToken", data, []string{
		"id", "userId", "name", "prefix", "projectId", "scopes", "revoked", "created",
	})

	var rt UserAccessToken
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != tok.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, tok.ProjectID)
	}

	var decoded UserAccessToken
	if err := json.Unmarshal([]byte(`{"id":"uat-2","groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}

func TestMessage_JSON_CanonicalKeysOnly(t *testing.T) {
	m := Message{
		ID: "m-1", ProjectID: "p-1", Sender: "user:alice", SenderID: "alice",
		Recipient: "agent:bob", RecipientID: "bob", Msg: "hi", Type: "instruction",
		Read: false, AgentID: "agent-1",
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "Message", data)
	assertExactKeySet(t, "Message", data, []string{
		"id", "projectId", "sender", "senderId", "recipient", "recipientId",
		"msg", "type", "read", "agentId", "createdAt",
	})

	var rt Message
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != m.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, m.ProjectID)
	}

	var decoded Message
	if err := json.Unmarshal([]byte(`{"id":"m-2","groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}

func TestScheduledEvent_JSON_CanonicalKeysOnly(t *testing.T) {
	e := ScheduledEvent{
		ID: "e-1", ProjectID: "p-1", EventType: "message", Payload: "{}",
		Status: "pending", CreatedBy: "creator",
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "ScheduledEvent", data)
	assertExactKeySet(t, "ScheduledEvent", data, []string{
		"id", "projectId", "eventType", "fireAt", "payload", "status", "createdAt", "createdBy",
	})

	var rt ScheduledEvent
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != e.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, e.ProjectID)
	}

	var decoded ScheduledEvent
	if err := json.Unmarshal([]byte(`{"id":"e-2","groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}

func TestSchedule_JSON_CanonicalKeysOnly(t *testing.T) {
	s := Schedule{
		ID: "sc-1", ProjectID: "p-1", Name: "sched", CronExpr: "* * * * *",
		EventType: "message", Payload: "{}", Status: "active",
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "Schedule", data)
	assertExactKeySet(t, "Schedule", data, []string{
		"id", "projectId", "name", "cronExpr", "eventType", "payload", "status",
		"runCount", "errorCount", "createdAt", "updatedAt",
	})

	var rt Schedule
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != s.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, s.ProjectID)
	}

	var decoded Schedule
	if err := json.Unmarshal([]byte(`{"id":"sc-2","groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}

func TestProjectSyncState_JSON_CanonicalKeysOnly(t *testing.T) {
	s := ProjectSyncState{ProjectID: "p-1", FileCount: 5, TotalBytes: 1024}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "ProjectSyncState", data)
	assertExactKeySet(t, "ProjectSyncState", data, []string{
		"projectId", "fileCount", "totalBytes",
	})

	var rt ProjectSyncState
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != s.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, s.ProjectID)
	}

	var decoded ProjectSyncState
	if err := json.Unmarshal([]byte(`{"groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}

func TestAgentSessionMetrics_JSON_CanonicalKeysOnly(t *testing.T) {
	m := AgentSessionMetrics{ID: "asm-1", AgentID: "agent-1", ProjectID: "p-1", SessionID: "sess-1"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	assertNoLegacyKeys(t, "AgentSessionMetrics", data)
	assertExactKeySet(t, "AgentSessionMetrics", data, []string{
		"id", "agentId", "projectId", "sessionId", "startedAt", "createdAt",
	})

	var rt AgentSessionMetrics
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}
	if rt.ProjectID != m.ProjectID {
		t.Errorf("round-trip ProjectID = %q, want %q", rt.ProjectID, m.ProjectID)
	}

	var decoded AgentSessionMetrics
	if err := json.Unmarshal([]byte(`{"id":"asm-2","groveId":"legacy"}`), &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if decoded.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", decoded.ProjectID)
	}
}
