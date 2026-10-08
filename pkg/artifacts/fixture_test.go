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
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// --- test host ---

type principal struct{ kind, ref, home string }

type principalKey struct{}

func withPrincipal(r *http.Request, p principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}

// fakeHost authorizes from a table of principal ref → scope → permissions.
type fakeHost struct {
	mu    sync.Mutex
	perms map[string]map[string]map[string]bool
	calls []string
	// denied lists "principalRef scope permission" triples the credential
	// does not permit (Permits); everything else is permitted.
	denied map[string]bool
	// memberErr, when set, makes MemberScopes fail.
	memberErr error
	// crossScope is the answer of CrossScopeSharingAllowed.
	crossScope bool
}

func (h *fakeHost) CrossScopeSharingAllowed(context.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.crossScope
}

func newFakeHost() *fakeHost {
	return &fakeHost{perms: map[string]map[string]map[string]bool{}, denied: map[string]bool{}}
}

// deny makes Permits refuse perm in scope for p.
func (h *fakeHost) deny(p principal, scope, perm string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.denied[PrincipalRef(p.kind, p.ref)+" "+scope+" "+perm] = true
}

func (h *fakeHost) Permits(ctx context.Context, scope, perm string) bool {
	kind, ref, _, ok := h.Principal(ctx)
	if !ok || scope == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "permits "+scope+" "+perm)
	return !h.denied[PrincipalRef(kind, ref)+" "+scope+" "+perm]
}

func (h *fakeHost) allow(p principal, scope string, perms ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := PrincipalRef(p.kind, p.ref)
	if h.perms[key] == nil {
		h.perms[key] = map[string]map[string]bool{}
	}
	if h.perms[key][scope] == nil {
		h.perms[key][scope] = map[string]bool{}
	}
	for _, perm := range perms {
		h.perms[key][scope][perm] = true
	}
}

func (h *fakeHost) Principal(ctx context.Context) (string, string, string, bool) {
	p, ok := ctx.Value(principalKey{}).(principal)
	if !ok {
		return "", "", "", false
	}
	return p.kind, p.ref, p.home, true
}

func (h *fakeHost) Authorize(ctx context.Context, scope, perm string) bool {
	kind, ref, _, ok := h.Principal(ctx)
	if !ok {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, scope+" "+perm)
	return h.perms[PrincipalRef(kind, ref)][scope][perm]
}

// MemberScopes reports the scopes the principal has any permission in,
// plus its home scope.
func (h *fakeHost) MemberScopes(ctx context.Context) ([]string, error) {
	kind, ref, home, ok := h.Principal(ctx)
	if !ok {
		return nil, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.memberErr != nil {
		return nil, h.memberErr
	}
	var out []string
	if home != "" {
		out = append(out, home)
	}
	for scope := range h.perms[PrincipalRef(kind, ref)] {
		out = append(out, scope)
	}
	return out, nil
}

// SealCursor "seals" by base64-encoding the principal, binding and
// position; OpenCursor refuses any mismatch. Enough to test binding.
func (h *fakeHost) SealCursor(ctx context.Context, position, binding string) (string, error) {
	kind, ref, _, _ := h.Principal(ctx)
	return "fake." + base64.RawURLEncoding.EncodeToString([]byte(PrincipalRef(kind, ref)+"\n"+binding+"\n"+position)), nil
}

func (h *fakeHost) OpenCursor(ctx context.Context, cursor, binding string) (string, error) {
	kind, ref, _, _ := h.Principal(ctx)
	body, ok := strings.CutPrefix(cursor, "fake.")
	if !ok {
		return "", errors.New("bad cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(string(raw), "\n", 3)
	if len(parts) != 3 || parts[0] != PrincipalRef(kind, ref) || parts[1] != binding {
		return "", errors.New("bad cursor")
	}
	return parts[2], nil
}

// --- object storage double that behaves like GCS for signed URLs ---

// redirectingStorage is local storage that claims to be GCS, so the service
// redirects instead of streaming, and records the signed URL options.
type redirectingStorage struct {
	*storage.LocalStorage
	mu   sync.Mutex
	last storage.SignedURLOptions
}

func (s *redirectingStorage) Provider() storage.Provider { return storage.ProviderGCS }

func (s *redirectingStorage) GenerateSignedURL(_ context.Context, p string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	s.mu.Lock()
	s.last = opts
	s.mu.Unlock()
	return &storage.SignedURL{URL: "https://objects.example/" + p + "?sig=x", Method: opts.Method}, nil
}

// --- fixture ---

var (
	agentA  = principal{PrincipalKindAgent, "agent-a-uuid", "project-1"}
	agentB  = principal{PrincipalKindAgent, "agent-b-uuid", "project-1"}
	agentX  = principal{PrincipalKindAgent, "agent-x-uuid", "project-2"}
	userU   = principal{PrincipalKindUser, "user-1", ""}
	outside = principal{PrincipalKindUser, "user-2", ""}
)

type fixture struct {
	t     *testing.T
	svc   *Service
	host  *fakeHost
	db    *sql.DB
	store Store
	blobs storage.Storage
	local *storage.LocalStorage
}

func newFixture(t *testing.T, redirect bool) *fixture {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "a.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db, "sqlite")
	if err := st.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var blobs storage.Storage = local
	if redirect {
		blobs = &redirectingStorage{LocalStorage: local}
	}
	host := newFakeHost()
	// Project members may read and publish in their project.
	for _, p := range []principal{agentA, agentB} {
		host.allow(p, "project-1", PermissionRead, PermissionCreate)
	}
	host.allow(agentX, "project-2", PermissionRead, PermissionCreate)
	host.allow(userU, "project-1", PermissionRead, PermissionCreate)

	svc := NewService(host)
	svc.SetStore(st)
	svc.SetBlobStorage(blobs, "hub-1")
	return &fixture{t: t, svc: svc, host: host, db: db, store: st, blobs: blobs, local: local}
}

func (f *fixture) do(p *principal, method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if p != nil {
		r = withPrincipal(r, *p)
	}
	rec := httptest.NewRecorder()
	f.svc.ServeHTTP(rec, r)
	return rec
}

func (f *fixture) publish(p principal, name string, body []byte, query string) ArtifactResponse {
	f.t.Helper()
	target := "/api/v1/artifacts?name=" + url.QueryEscape(name)
	if query != "" {
		target += "&" + query
	}
	rec := f.do(&p, http.MethodPost, target, body, map[string]string{"Content-Type": "application/octet-stream"})
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("publish: status %d: %s", rec.Code, rec.Body.String())
	}
	var resp ArtifactResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not a JSON error (%d): %q", rec.Code, rec.Body.String())
	}
	return e.Error.Code
}
