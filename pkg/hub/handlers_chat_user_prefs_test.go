// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !no_sqlite

package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
)

// A prefs GET has to answer in the same field names the PUT accepts. The rail
// writes spaceSortMode / spaceOrder and then reads its own preferences back on
// the next load; if the response were PascalCase the read would silently miss
// every field and the custom space order would look unsaved (#1031).
func TestChatV2_UserPrefs_ResponseUsesRequestFieldNames(t *testing.T) {
	srv, _ := testServer(t)

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	order := `["proj-b","proj-a"]`
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/chat/user-prefs", map[string]string{
		"spaceSortMode":  "custom",
		"spaceOrder":     order,
		"threadSortMode": "alpha",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/user-prefs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{
		"spaceSortMode":  "custom",
		"spaceOrder":     order,
		"threadSortMode": "alpha",
	}
	for key, value := range want {
		got, ok := payload[key]
		if !ok {
			t.Errorf("response is missing %q: %s", key, rec.Body.String())
			continue
		}
		if got != value {
			t.Errorf("%s = %v, want %q", key, got, value)
		}
	}
}
