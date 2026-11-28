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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// noPrincipalMessageHost implements Host + MessageScopeHost where Principal
// always returns ok=false (simulating an agent without project:artifact:read)
// while PermitsMessageRead and MayReadMessage allow the caller.
type noPrincipalMessageHost struct {
	*messageHost
	allowPermits bool
	allowRead    bool
}

func (h *noPrincipalMessageHost) Principal(context.Context) (string, string, string, bool) {
	return "", "", "", false
}

func (h *noPrincipalMessageHost) PermitsMessageRead(context.Context) bool {
	return h.allowPermits
}

func (h *noPrincipalMessageHost) MayReadMessage(context.Context, string, *string) bool {
	return h.allowRead
}

func TestServeMessageAttachment(t *testing.T) {
	f := newAttachmentFixture(t, false)
	h := &noPrincipalMessageHost{
		messageHost:  f.mh,
		allowPermits: true,
		allowRead:    true,
	}
	svc := f.svc

	const payload = "hello from message attachment"
	attID := f.upload(userU, "note.txt", "text/plain", []byte(payload))
	msgID := uuid.NewString()
	if err := svc.BindAttachment(context.Background(), attID, userU.kind, userU.ref, msgID, "proj-1"); err != nil {
		t.Fatalf("BindAttachment: %v", err)
	}

	// Swap host to one whose Principal always returns false, proving
	// ServeMessageAttachment does not call host.Principal.
	svc.host = h

	t.Run("serves bound attachment without Principal", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test?stream=1", nil)
		rec := httptest.NewRecorder()
		if got := svc.ServeMessageAttachment(rec, req, attID, msgID); got != ServeWritten {
			t.Fatalf("ServeMessageAttachment = %v, want ServeWritten", got)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Body.String() != payload {
			t.Fatalf("body = %q, want %q", rec.Body.String(), payload)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("missing nosniff header")
		}
		if got := rec.Header().Get("Content-Disposition"); got != "inline; filename=note.txt" {
			t.Errorf("Content-Disposition = %q, want %q", got, "inline; filename=note.txt")
		}
	})

	t.Run("honors ?download=1 for forced attachment", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test?download=1", nil)
		rec := httptest.NewRecorder()
		if got := svc.ServeMessageAttachment(rec, req, attID, msgID); got != ServeWritten {
			t.Fatalf("ServeMessageAttachment = %v, want ServeWritten", got)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Content-Disposition"); got != "attachment; filename=note.txt" {
			t.Errorf("Content-Disposition = %q, want %q", got, "attachment; filename=note.txt")
		}
	})

	t.Run("wrong messageID returns ServeNotFound without writing", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		rec := httptest.NewRecorder()
		if got := svc.ServeMessageAttachment(rec, req, attID, uuid.NewString()); got != ServeNotFound {
			t.Fatalf("ServeMessageAttachment = %v, want ServeNotFound", got)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("expected empty body on ServeNotFound, got %q", rec.Body.String())
		}
	})

	t.Run("non-canonical id returns ServeNotFound", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		rec := httptest.NewRecorder()
		if got := svc.ServeMessageAttachment(rec, req, "not-a-uuid", msgID); got != ServeNotFound {
			t.Fatalf("ServeMessageAttachment = %v, want ServeNotFound", got)
		}
	})

	t.Run("MayReadAttachments false returns ServeRefused without writing", func(t *testing.T) {
		h.allowRead = false
		defer func() { h.allowRead = true }()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		rec := httptest.NewRecorder()
		if got := svc.ServeMessageAttachment(rec, req, attID, msgID); got != ServeRefused {
			t.Fatalf("ServeMessageAttachment = %v, want ServeRefused", got)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("expected empty body on ServeRefused, got %q", rec.Body.String())
		}
	})
}
