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

package artifacts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/google/uuid"
)

// Route patterns the service serves, in net/http ServeMux syntax.
const (
	// RouteCollection serves the artifact collection.
	RouteCollection = "/api/v1/artifacts"
	// RouteByID serves everything under an artifact.
	RouteByID = "/api/v1/artifacts/"
	// RouteShared serves share links. Its requests authenticate by link
	// token only, so the host must mount it without requiring a session.
	RouteShared = "/api/v1/artifacts/shared/"
)

// RoutePatterns returns every pattern RegisterRoutes mounts.
func RoutePatterns() []string {
	return []string{RouteCollection, RouteByID, RouteShared}
}

// Mux is the subset of *http.ServeMux that RegisterRoutes needs.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Guard wraps the handler for pattern with the host's authentication and
// feature gating before it is mounted. The host decides what the guard does
// for each pattern; RouteShared must not require a session.
type Guard func(pattern string, handler http.Handler) http.Handler

// Service is the artifact service.
//
// A Service is built with NewService and configured either with a backend
// provider (SetBackendProvider), which it asks on each request, or with
// SetStore and SetBlobStorage; limits come from SetLimits. Either may run
// after the routes are mounted (the hub opens its database after it builds
// its router). Until a store and blob storage are available, data routes
// answer 503.
type Service struct {
	host Host

	mu       sync.RWMutex
	store    Store
	blobs    storage.Storage
	hubID    string
	limits   func(context.Context) Limits
	provider func() Backend

	fetcherFactory func(RemoteImageLimits) ImageFetcher
	// fetchFloor overrides the fetch floor in tests; zero means the
	// fetcher's connect timeout.
	fetchFloor time.Duration
}

// Backend is what a request needs from the service's environment: the
// metadata store, the blob storage and the hub id that namespaces blobs.
type Backend struct {
	Store Store
	Blobs storage.Storage
	HubID string
}

// Limits are the size limits the service enforces.
type Limits struct {
	// MaxFileBytes caps the size of one file.
	MaxFileBytes int64
	// MaxBundleBytes caps the total size of the files of one version.
	MaxBundleBytes int64
	// MaxFiles caps the number of files of one version.
	MaxFiles int
	// RemoteImages bound the remote images fetched at publish time. A host
	// that sets a limits getter must fill them in: incomplete or invalid
	// values turn remote images off.
	RemoteImages RemoteImageLimits
}

// Default limits, used when no limits getter is set or it yields a
// non-positive value (design D19).
const (
	DefaultMaxFileBytes   int64 = 32 << 20
	DefaultMaxBundleBytes int64 = 256 << 20
	DefaultMaxFiles             = 200
)

// NewService returns a service that identifies and authorizes callers
// through host.
func NewService(host Host) *Service {
	return &Service{host: host}
}

// Host returns the host the service was built with.
func (s *Service) Host() Host { return s.host }

// SetBackendProvider makes the service ask fn for its store, blob storage
// and hub id on each request, instead of the values given to SetStore and
// SetBlobStorage. fn must be safe for concurrent use and cheap.
func (s *Service) SetBackendProvider(fn func() Backend) {
	s.mu.Lock()
	s.provider = fn
	s.mu.Unlock()
}

// SetStore sets the store that holds artifact metadata.
func (s *Service) SetStore(st Store) {
	s.mu.Lock()
	s.store = st
	s.mu.Unlock()
}

// SetBlobStorage sets the object storage that holds file bytes. Blobs are
// written under hubs/{hubID}/artifacts/, a prefix only this service writes.
func (s *Service) SetBlobStorage(blobs storage.Storage, hubID string) {
	s.mu.Lock()
	s.blobs, s.hubID = blobs, hubID
	s.mu.Unlock()
}

// SetLimits sets the function that yields the current limits. It is called
// on every write, so limits follow the host's settings without a restart.
func (s *Service) SetLimits(fn func(context.Context) Limits) {
	s.mu.Lock()
	s.limits = fn
	s.mu.Unlock()
}

// backend is a consistent snapshot of the service's configuration.
type backend struct {
	store  Store
	blobs  storage.Storage
	hubID  string
	limits func(context.Context) Limits
}

func (s *Service) backend() (backend, bool) {
	s.mu.RLock()
	b := backend{store: s.store, blobs: s.blobs, hubID: s.hubID, limits: s.limits}
	provider := s.provider
	s.mu.RUnlock()
	if provider != nil {
		p := provider()
		b.store, b.blobs, b.hubID = p.Store, p.Blobs, p.HubID
	}
	return b, b.store != nil && b.blobs != nil && b.hubID != ""
}

func (b backend) maxFileBytes(ctx context.Context) int64 {
	return b.currentLimits(ctx).MaxFileBytes
}

// currentLimits reads the limits once, defaulting each unset or
// non-positive one.
func (b backend) currentLimits(ctx context.Context) Limits {
	var l Limits
	if b.limits != nil {
		l = b.limits(ctx)
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultMaxFileBytes
	}
	if l.MaxBundleBytes <= 0 {
		l.MaxBundleBytes = DefaultMaxBundleBytes
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultMaxFiles
	}
	return l
}

// Handler returns the service's HTTP handler for every route pattern.
func (s *Service) Handler() http.Handler { return s }

// RegisterRoutes mounts the service on mux at every pattern in
// RoutePatterns, each wrapped by guard. A nil guard mounts the handler
// unwrapped.
func (s *Service) RegisterRoutes(mux Mux, guard Guard) {
	for _, pattern := range RoutePatterns() {
		var h http.Handler = s
		if guard != nil {
			h = guard(pattern, h)
		}
		mux.Handle(pattern, h)
	}
}

// ServeHTTP routes a request:
//
//	POST /api/v1/artifacts?name=<file>[&title=][&scope=]    single-file publish
//	POST /api/v1/artifacts                                  create a pending version (JSON manifest)
//	GET  /api/v1/artifacts/{id}                             metadata of the current version
//	GET  /api/v1/artifacts/{id}/files/{path}                a file of the current version
//	GET  /api/v1/artifacts/{id}/versions                    the ready versions
//	POST /api/v1/artifacts/{id}/versions                    append a pending version
//	GET  /api/v1/artifacts/{id}/versions/{seq}              one ready version
//	POST /api/v1/artifacts/{id}/versions/{seq}/finalize     make a pending version ready
//	GET  /api/v1/artifacts/{id}/versions/{seq}/files/{path} a file of version seq
//	PUT  /api/v1/artifacts/{id}/versions/{seq}/files/{path} upload a file of a pending version
//
// Everything else, including share links (a later phase), answers 404.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), RouteCollection)
	if !ok || (rest != "" && rest[0] != '/') {
		writeNotFound(w)
		return
	}
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, http.MethodPost)
			return
		}
		if r.URL.Query().Has(paramName) {
			s.handlePublish(w, r)
			return
		}
		s.handleCreate(w, r)
		return
	}
	segs, ok := splitEscapedPath(rest)
	if !ok || segs[0] == "shared" {
		writeNotFound(w)
		return
	}
	id := segs[0]
	if !canonicalID(id) {
		writeNotFound(w)
		return
	}
	switch {
	case len(segs) == 1:
		if !isRead(r.Method) {
			writeMethodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		s.handleGetArtifact(w, r, id)
	case len(segs) >= 3 && segs[1] == "files":
		if !isRead(r.Method) {
			writeMethodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		s.handleGetFile(w, r, id, 0, strings.Join(segs[2:], "/"))
	case len(segs) == 2 && segs[1] == "versions":
		switch {
		case isRead(r.Method):
			s.handleListVersions(w, r, id)
		case r.Method == http.MethodPost:
			s.handleCreateVersion(w, r, id)
		default:
			writeMethodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
		}
	case len(segs) >= 3 && segs[1] == "versions":
		seq, ok := parseSeq(segs[2])
		if !ok {
			writeNotFound(w)
			return
		}
		switch {
		case len(segs) == 3:
			if !isRead(r.Method) {
				writeMethodNotAllowed(w, http.MethodGet, http.MethodHead)
				return
			}
			s.handleGetVersion(w, r, id, seq)
		case len(segs) == 4 && segs[3] == "finalize":
			if r.Method != http.MethodPost {
				writeMethodNotAllowed(w, http.MethodPost)
				return
			}
			s.handleFinalize(w, r, id, seq)
		case len(segs) >= 5 && segs[3] == "files":
			filePath := strings.Join(segs[4:], "/")
			switch {
			case isRead(r.Method):
				s.handleGetFile(w, r, id, seq, filePath)
			case r.Method == http.MethodPut:
				s.handlePutFile(w, r, id, seq, filePath)
			default:
				writeMethodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPut)
			}
		default:
			writeNotFound(w)
		}
	default:
		writeNotFound(w)
	}
}

func isRead(method string) bool { return method == http.MethodGet || method == http.MethodHead }

// splitEscapedPath splits an escaped path into unescaped segments. A
// segment that fails to unescape, or that unescapes to contain a slash, is
// rejected so that every file has one canonical URL.
func splitEscapedPath(p string) ([]string, bool) {
	raw := strings.Split(p, "/")
	segs := make([]string, len(raw))
	for i, r := range raw {
		u, err := url.PathUnescape(r)
		if err != nil || strings.Contains(u, "/") {
			return nil, false
		}
		segs[i] = u
	}
	return segs, true
}

// canonicalID reports whether id has the form the service assigns: a
// lowercase 36-character UUID. Anything else is answered 404 before any
// store lookup, so a malformed id (a NUL byte, invalid UTF-8) can never
// reach the database driver.
func canonicalID(id string) bool {
	if len(id) != 36 || strings.ToLower(id) != id {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}

// parseSeq parses a version number: a positive decimal integer without sign
// or leading zeros.
func parseSeq(v string) (int, bool) {
	if v == "" || v[0] < '1' || v[0] > '9' || len(v) > 9 {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n > 0
}

// errorResponse matches the hub's JSON error envelope so clients see one
// shape whichever side answered.
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

// writeNotFound answers 404. It is also the answer when the caller may not
// read an artifact, so a response never tells whether an artifact exists.
func writeNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "not found")
}

func writeMethodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}
