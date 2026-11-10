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

package opsettings

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// notification_channels has an entry in the canonical section-key mapping,
// in both directions (ptone/scion#4167).
func TestNotificationsCanonicalMapping(t *testing.T) {
	if got := KoanfPathFromSectionKey("notifications", "notification_channels"); got != "server.notification_channels" {
		t.Errorf("KoanfPathFromSectionKey = %q, want server.notification_channels", got)
	}
	if got := SectionKeyFromKoanfPath("server.notification_channels"); got != "notification_channels" {
		t.Errorf("SectionKeyFromKoanfPath = %q, want notification_channels", got)
	}
	if got := KoanfPathFromSectionKey("notifications", "other"); got != "" {
		t.Errorf("KoanfPathFromSectionKey(notifications, other) = %q, want \"\"", got)
	}
}

// Seeding extracts the notifications document from the settings keyspace,
// and loading it back yields the same channels (ptone/scion#4167).
func TestNotificationsSeedingRoundTrip(t *testing.T) {
	channels := []interface{}{
		map[string]interface{}{"type": "slack", "params": map[string]interface{}{"channel": "#ops"}},
		map[string]interface{}{"type": "webhook", "params": map[string]interface{}{"url": "https://hook.example.com"}},
	}
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(map[string]interface{}{
		"server.notification_channels": channels,
		"server.hub.hub_name":          "not-in-this-section",
	}, "."), nil); err != nil {
		t.Fatal(err)
	}

	doc, err := ExtractSectionFromKoanf(k, "notifications")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("seeded document keys = %v, want only notification_channels", got)
	}
	wantJSON, _ := json.Marshal(channels)
	gotJSON, _ := json.Marshal(got["notification_channels"])
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("seeded notification_channels = %s, want %s", gotJSON, wantJSON)
	}

	back, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{"notifications": doc})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Get("server.notification_channels"), k.Get("server.notification_channels")) {
		t.Errorf("loaded channels = %v, want %v", back.Get("server.notification_channels"), k.Get("server.notification_channels"))
	}
	if keys := back.Keys(); len(keys) == 0 {
		t.Error("loaded keyspace is empty")
	}
	for _, key := range back.Keys() {
		if len(key) < len("server.notification_channels") || key[:len("server.notification_channels")] != "server.notification_channels" {
			t.Errorf("loaded unexpected key %q", key)
		}
	}

	// No channels configured: the seeded document is empty and loads no keys.
	empty, err := ExtractSectionFromKoanf(koanf.New("."), "notifications")
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != "{}" {
		t.Errorf("seeded document without channels = %s, want {}", empty)
	}
	// An empty list is kept, so a stored clear stays a clear.
	cleared, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{"notifications": json.RawMessage(`{"notification_channels":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !cleared.Exists("server.notification_channels") {
		t.Error("an empty notification_channels list is dropped on load")
	}
}
