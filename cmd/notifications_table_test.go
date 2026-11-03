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

package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	tableTestArtifactID = "5f1c2d3e-0000-4000-8000-000000000001"
	tableTestNilUUID    = "00000000-0000-0000-0000-000000000000"
)

// tableLine formats one table row the way writeNotificationsTable does
// (padRight is the cmd test helper in hub_registry_test.go).
func tableLine(id, agent, status, timeStr, msg string) string {
	return padRight(id, 12) + "  " + padRight(agent, 14) + "  " + padRight(status, 20) + "  " + padRight(timeStr, 22) + "  " + msg + "\n"
}

func renderNotificationsTable(notifs ...hubclient.Notification) string {
	var b bytes.Buffer
	writeNotificationsTable(&b, notifs)
	return b.String()
}

// TestWriteNotificationsTable_ArtifactRowKeepsRef pins that a notification
// about an artifact prints its whole message and a reference line built
// from ResourceID, however long the message is, and "-" for its nil agent.
func TestWriteNotificationsTable_ArtifactRowKeepsRef(t *testing.T) {
	label := strings.Repeat("Very Long Display Name ", 6) + "(someone@example.com)"
	msg := label + " asked you to review an artifact (Can review): scion://artifact/" + tableTestArtifactID
	out := renderNotificationsTable(hubclient.Notification{
		ID:           "0123456789abcdef",
		AgentID:      tableTestNilUUID,
		Status:       store.NotificationStatusArtifactReviewRequested,
		Message:      msg,
		Kind:         store.NotificationKindArtifactGrant,
		ResourceType: store.NotificationResourceArtifact,
		ResourceID:   tableTestArtifactID,
	})

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want header, rule, row and reference:\n%s", len(lines), out)
	}
	wantRow := tableLine("0123456789ab", "-", "ARTIFACT_REVIEW_R...", "-", msg)
	if lines[2]+"\n" != wantRow {
		t.Errorf("row:\n got %q\nwant %q", lines[2], strings.TrimSuffix(wantRow, "\n"))
	}
	wantRef := tableLine("", "", "", "", "scion://artifact/"+tableTestArtifactID)
	if lines[3]+"\n" != wantRef {
		t.Errorf("reference line:\n got %q\nwant %q", lines[3], strings.TrimSuffix(wantRef, "\n"))
	}
}

// TestWriteNotificationsTable_NoRefForOtherRows pins that the reference line
// appears only for a classified notification about an artifact with a
// canonical id.
func TestWriteNotificationsTable_NoRefForOtherRows(t *testing.T) {
	for name, n := range map[string]hubclient.Notification{
		"no kind":         {ID: "n1", AgentID: "a1", Status: "COMPLETED", Message: "done", ResourceType: store.NotificationResourceArtifact, ResourceID: tableTestArtifactID},
		"other resource":  {ID: "n2", AgentID: "a1", Status: "X", Message: "m", Kind: "other", ResourceType: "agent", ResourceID: tableTestArtifactID},
		"non-canonical":   {ID: "n3", AgentID: "a1", Status: "X", Message: "m", Kind: store.NotificationKindArtifactGrant, ResourceType: store.NotificationResourceArtifact, ResourceID: strings.ToUpper(tableTestArtifactID)},
		"not a uuid":      {ID: "n4", AgentID: "a1", Status: "X", Message: "m", Kind: store.NotificationKindArtifactGrant, ResourceType: store.NotificationResourceArtifact, ResourceID: "x\x1b[2J"},
		"empty resource":  {ID: "n5", AgentID: "a1", Status: "X", Message: "m", Kind: store.NotificationKindArtifactGrant},
		"empty agent id":  {ID: "n6", AgentID: "", Status: "X", Message: "m"},
		"nil agent no kd": {ID: "n7", AgentID: tableTestNilUUID, Status: "X", Message: "m"},
	} {
		out := renderNotificationsTable(n)
		if strings.Contains(out, "scion://artifact/") {
			t.Errorf("%s: unexpected reference line:\n%s", name, out)
		}
		if got := strings.Count(out, "\n"); got != 3 {
			t.Errorf("%s: got %d lines, want 3:\n%s", name, got, out)
		}
	}
}

// TestWriteNotificationsTable_ExistingRowUnchanged pins the table output of
// a notification without a kind: the message is still cut at 60 runes and
// the agent id still shown.
func TestWriteNotificationsTable_ExistingRowUnchanged(t *testing.T) {
	msg := "watched-agent has reached a state of COMPLETED: " + strings.Repeat("x", 40)
	out := renderNotificationsTable(hubclient.Notification{
		ID:      "11111111-1111-1111-1111-111111111111",
		AgentID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		Status:  "COMPLETED",
		Message: msg,
	})
	want := tableLine("ID", "AGENT", "STATUS", "TIME", "MESSAGE") +
		tableLine("------------", "--------------", "--------------------", "----------------------", "-------") +
		tableLine("11111111-111", "bbbbbbbb-bb...", "COMPLETED", "-", msg[:57]+"...")
	if out != want {
		t.Errorf("table:\n got %q\nwant %q", out, want)
	}
}

// TestWriteNotificationsTable_RuneSafeCut pins that the 60-rune cut never
// splits a multi-byte character.
func TestWriteNotificationsTable_RuneSafeCut(t *testing.T) {
	msg := strings.Repeat("é", 70)
	out := renderNotificationsTable(hubclient.Notification{ID: "n1", AgentID: "a1", Status: "S", Message: msg})
	want := strings.Repeat("é", 57) + "..."
	if !strings.HasSuffix(strings.TrimSuffix(out, "\n"), want) {
		t.Errorf("table does not end with the rune-safe cut:\n%q", out)
	}
	if !strings.Contains(out, "é") || strings.ContainsRune(out, '\uFFFD') {
		t.Errorf("table split a character:\n%q", out)
	}
}

// TestWriteNotificationsTable_SanitisesOtherPrincipalsText pins that message
// and status text never reach the terminal with control or bidirectional
// format characters: a notification message can carry another user's
// display name.
func TestWriteNotificationsTable_SanitisesOtherPrincipalsText(t *testing.T) {
	hostile := "Mal\x1b[2J\rlory\n\u202eweiver ot uoy deksa (Can review): scion://artifact/" + tableTestArtifactID
	for name, n := range map[string]hubclient.Notification{
		"with kind": {ID: "n1", AgentID: tableTestNilUUID, Status: "ARTIFACT\x1b[31m", Message: hostile,
			Kind: store.NotificationKindArtifactGrant, ResourceType: store.NotificationResourceArtifact, ResourceID: tableTestArtifactID},
		"without kind": {ID: "n2", AgentID: "a1", Status: "COMPLETED", Message: hostile},
	} {
		out := renderNotificationsTable(n)
		for _, bad := range []string{"\x1b", "\r", "\u202e"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: output contains %q:\n%q", name, bad, out)
			}
		}
		rows := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		wantLines := 3
		if n.Kind != "" {
			wantLines = 4
		}
		if len(rows) != wantLines {
			t.Errorf("%s: got %d lines, want %d (a newline in the text must not start a line):\n%q", name, len(rows), wantLines, out)
		}
	}
}

// TestNotificationJSON_ExistingRowUnchanged pins that the JSON of a
// notification without kind or resource fields is the same as before those
// fields existed, and that a notification with them carries them.
func TestNotificationJSON_ExistingRowUnchanged(t *testing.T) {
	var existing hubclient.Notification
	in := `{"id":"n1","subscriptionId":"s1","agentId":"a1","projectId":"p1","subscriberType":"user",` +
		`"subscriberId":"u1","status":"COMPLETED","message":"done","dispatched":true,"acknowledged":false,` +
		`"createdAt":"2026-03-01T12:00:00Z","dispatchAttempts":1,"dispatchOutcome":"delivered"}`
	if err := json.Unmarshal([]byte(in), &existing); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Errorf("JSON changed:\n got %s\nwant %s", out, in)
	}

	existing.Kind = store.NotificationKindArtifactGrant
	existing.ResourceType = store.NotificationResourceArtifact
	existing.ResourceID = tableTestArtifactID
	out, err = json.Marshal(existing)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSuffix(in, "}") + `,"kind":"artifact_grant","resourceType":"artifact","resourceId":"` + tableTestArtifactID + `"}`
	if string(out) != want {
		t.Errorf("JSON with kind:\n got %s\nwant %s", out, want)
	}
}
