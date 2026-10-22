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

package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// tgRequest is one request a thread group test server saw.
type tgRequest struct {
	Method string
	Path   string // escaped path
	Query  string
	Body   string
}

// newThreadGroupTestServer returns a server that records every request and
// answers it with handle.
func newThreadGroupTestServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() []tgRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []tgRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, tgRequest{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Body: string(body)})
		mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []tgRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]tgRequest(nil), reqs...)
	}
}

func writeTGJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeTGError(w http.ResponseWriter, status int, code, message string) {
	writeTGJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

const tgGroupJSON = `{"id":"g1","projectId":"p1","name":"Release","createdBy":"u1","createdByKind":"user",
	"createdAt":"2026-10-01T10:00:00Z","updatedAt":"2026-10-02T10:00:00Z",
	"threads":[{"conversationId":"c1","threadId":"t1","name":"release-notes","addedBy":"u1","addedAt":"2026-10-01T11:00:00Z"}]}`

func newTGClient(t *testing.T, url string) Client {
	t.Helper()
	c, err := New(url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestThreadGroupService_RequestsAndDecoding(t *testing.T) {
	srv, seen := newThreadGroupTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/thread-groups":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"threadGroups":[` + tgGroupJSON + `]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/thread-groups":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			// A hub that sent threads:null must still decode to [].
			_, _ = w.Write([]byte(`{"id":"g2","projectId":"p1","name":"New","threads":null}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tgGroupJSON))
		}
	})
	c := newTGClient(t, srv.URL)
	ctx := context.Background()
	tg := c.ThreadGroups()

	groups, err := tg.List(ctx, "p1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(groups) != 1 || groups[0].Name != "Release" || groups[0].CreatedByKind != "user" ||
		len(groups[0].Threads) != 1 || groups[0].Threads[0].ThreadID != "t1" || groups[0].Threads[0].Name != "release-notes" {
		t.Fatalf("List decoded %+v", groups)
	}
	if groups[0].CreatedAt.IsZero() || groups[0].Threads[0].AddedAt.IsZero() {
		t.Errorf("timestamps not decoded: %+v", groups[0])
	}

	created, err := tg.Create(ctx, &CreateThreadGroupRequest{ProjectID: "p1", Name: "New"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != "g2" || created.Threads == nil || len(created.Threads) != 0 {
		t.Errorf("Create decoded %+v, want g2 with non-nil empty threads", created)
	}
	if _, err := tg.Get(ctx, "g 1/x"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := tg.Rename(ctx, "g1", "Renamed"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := tg.AddThread(ctx, "g1", &AddThreadGroupThreadRequest{ConversationID: "c9"}); err != nil {
		t.Fatalf("AddThread: %v", err)
	}
	if _, err := tg.AddThread(ctx, "g1", &AddThreadGroupThreadRequest{ThreadID: "t9"}); err != nil {
		t.Fatalf("AddThread by thread: %v", err)
	}
	if err := tg.RemoveThread(ctx, "g1", "c9"); err != nil {
		t.Fatalf("RemoveThread: %v", err)
	}
	if err := tg.Delete(ctx, "g1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want := []tgRequest{
		{Method: "GET", Path: "/api/v1/thread-groups", Query: "projectId=p1"},
		{Method: "POST", Path: "/api/v1/thread-groups", Body: `{"projectId":"p1","name":"New"}`},
		{Method: "GET", Path: "/api/v1/thread-groups/g%201%2Fx"},
		{Method: "PATCH", Path: "/api/v1/thread-groups/g1", Body: `{"name":"Renamed"}`},
		{Method: "POST", Path: "/api/v1/thread-groups/g1/threads", Body: `{"conversationId":"c9"}`},
		{Method: "POST", Path: "/api/v1/thread-groups/g1/threads", Body: `{"threadId":"t9"}`},
		{Method: "DELETE", Path: "/api/v1/thread-groups/g1/threads/c9"},
		{Method: "DELETE", Path: "/api/v1/thread-groups/g1"},
	}
	got := seen()
	if len(got) != len(want) {
		t.Fatalf("requests = %+v, want %d", got, len(want))
	}
	for i := range want {
		g := got[i]
		g.Body = strings.TrimSpace(g.Body)
		if g != want[i] {
			t.Errorf("request %d = %+v, want %+v", i, g, want[i])
		}
	}
}

// An empty projectId is not sent, so the hub defaults it for an agent, and
// a list with no groups (or a null list) is [], never nil.
func TestThreadGroupService_ListEmptyProjectAndNull(t *testing.T) {
	srv, seen := newThreadGroupTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"threadGroups":null}`))
	})
	groups, err := newTGClient(t, srv.URL).ThreadGroups().List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if groups == nil || len(groups) != 0 {
		t.Errorf("groups = %#v, want empty non-nil", groups)
	}
	if q := seen()[0].Query; q != "" {
		t.Errorf("query = %q, want none", q)
	}
}

func TestCreateConversationRequest_ThreadGroupIDJSON(t *testing.T) {
	b, _ := json.Marshal(CreateConversationRequest{DisplayName: "x", Kind: "group", ThreadGroupID: "g1"})
	if !strings.Contains(string(b), `"threadGroupId":"g1"`) {
		t.Errorf("marshal = %s, want threadGroupId", b)
	}
	b, _ = json.Marshal(CreateConversationRequest{DisplayName: "x"})
	if strings.Contains(string(b), "threadGroupId") {
		t.Errorf("marshal = %s, want threadGroupId omitted when empty", b)
	}
}

// olderHubHandler answers every thread group route 404, as an older hub's
// experiment gate does with thread groups off (ptone/scion#5546).
func olderHubHandler(w http.ResponseWriter, r *http.Request) {
	writeTGError(w, http.StatusNotFound, "not_found", "route not found")
}

func TestThreadGroupService_OlderHubIsNotEnabledError(t *testing.T) {
	srv, _ := newThreadGroupTestServer(t, olderHubHandler)
	tg := newTGClient(t, srv.URL).ThreadGroups()
	ctx := context.Background()

	calls := map[string]func() error{
		"List":   func() error { _, err := tg.List(ctx, "p1"); return err },
		"Create": func() error { _, err := tg.Create(ctx, &CreateThreadGroupRequest{Name: "x"}); return err },
		"Get":    func() error { _, err := tg.Get(ctx, "g1"); return err },
		"Rename": func() error { _, err := tg.Rename(ctx, "g1", "y"); return err },
		"Delete": func() error { return tg.Delete(ctx, "g1") },
		"AddThread": func() error {
			_, err := tg.AddThread(ctx, "g1", &AddThreadGroupThreadRequest{ConversationID: "c1"})
			return err
		},
		"RemoveThread": func() error { return tg.RemoveThread(ctx, "g1", "c1") },
	}
	for name, call := range calls {
		err := call()
		if !IsThreadGroupsNotEnabled(err) {
			t.Errorf("%s: err = %v, want ThreadGroupsNotEnabledError", name, err)
			continue
		}
		if err.Error() != ThreadGroupsNotEnabledMessage {
			t.Errorf("%s: message = %q", name, err.Error())
		}
		var apiErr *apiclient.APIError
		if !errors.As(err, &apiErr) || !apiErr.IsNotFound() {
			t.Errorf("%s: original 404 not wrapped: %v", name, err)
		}
	}
}

// On a hub that serves thread groups, a 404 for a missing group or thread
// stays a plain not-found: the collection probe answers 400 (a user without
// projectId) or 403/200 (an agent), not 404.
func TestThreadGroupService_ItemNotFoundStaysNotFound(t *testing.T) {
	for _, probe := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusOK} {
		srv, seen := newThreadGroupTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/thread-groups" {
				if probe == http.StatusOK {
					writeTGJSON(w, probe, map[string]any{"threadGroups": []any{}})
					return
				}
				writeTGError(w, probe, "invalid_request", "projectId is required")
				return
			}
			writeTGError(w, http.StatusNotFound, "not_found", "Thread group not found")
		})
		tg := newTGClient(t, srv.URL).ThreadGroups()
		_, err := tg.Get(context.Background(), "g-missing")
		if err == nil || IsThreadGroupsNotEnabled(err) {
			t.Fatalf("probe %d: err = %v, want a plain not-found", probe, err)
		}
		if !strings.Contains(err.Error(), "Thread group not found") {
			t.Errorf("probe %d: err = %v", probe, err)
		}
		reqs := seen()
		if len(reqs) != 2 || reqs[1].Path != "/api/v1/thread-groups" || reqs[1].Query != "" {
			t.Errorf("probe %d: requests = %+v, want the item call then a bare collection probe", probe, reqs)
		}
	}
}

// A non-404 error is returned unchanged, with no probe.
func TestThreadGroupService_OtherErrorsUnchanged(t *testing.T) {
	srv, seen := newThreadGroupTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeTGError(w, http.StatusConflict, "conflict", "a thread group with this name already exists in the project")
	})
	tg := newTGClient(t, srv.URL).ThreadGroups()
	_, err := tg.Rename(context.Background(), "g1", "dup")
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || !apiErr.IsConflict() || IsThreadGroupsNotEnabled(err) {
		t.Fatalf("err = %v, want the 409", err)
	}
	if n := len(seen()); n != 1 {
		t.Errorf("requests = %d, want 1 (no probe for a non-404)", n)
	}
}
