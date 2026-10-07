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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"hash/maphash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTML bundles (design D8). The web page shows an HTML entry in an iframe
// with sandbox="allow-scripts" and no allow-same-origin, so its scripts run
// in an opaque origin. Requests from such a frame carry no session, so the
// frame loads the bundle through a view URL:
//
//	/api/v1/artifacts/view/<capability>/<path>
//
// The capability is minted for a caller who may read the version (POST
// /api/v1/artifacts/{id}/versions/{seq}/view), names exactly one version of
// one artifact and expires after ViewTTL. It is a path segment, so the
// bundle's relative links (img/a.png, css/site.css) resolve under it. The
// view route accepts the capability alone and serves only files of that
// version; it never uses or grants anything based on a session.
//
// Every view response carries the policy viewCSP builds, whose sandbox
// directive puts the document in an opaque origin even when the URL is
// opened directly, and whose source lists let it load only files under its
// own view path.

const (
	// ViewTTL is how long a view capability is valid.
	ViewTTL = 30 * time.Minute

	// viewMaxSkew bounds how far beyond ViewTTL an expiry may lie. The
	// service never mints one; anything further out is refused.
	viewMaxSkew = time.Minute

	// viewSigDomain separates view capability signatures from any other
	// use of the key.
	viewSigDomain = "artifact-view"
)

// viewCSP returns the Content-Security-Policy of a view response served
// for host under capability. Scripts and styles of the bundle run, in an
// opaque origin (sandbox without allow-same-origin); scripts, styles,
// images, fonts and media load only from this view's own path on the hub
// (inline scripts and styles and data: images and fonts are allowed); the
// document cannot connect, frame, embed, submit forms, open popups or
// navigate the page that frames it, and only the hub may frame it.
//
// The source is a host-source with a path: no scheme, so it takes the
// response's own, and a trailing '/', so it matches every file under the
// view. A host that is not a plain host[:port] yields no source at all,
// which blocks every load.
func viewCSP(host, capability string) string {
	src := "'none'"
	if validCSPHost(host) {
		src = host + RouteView + capability + "/"
	}
	return "sandbox allow-scripts; default-src 'none'; script-src " + src + " 'unsafe-inline'; " +
		"style-src " + src + " 'unsafe-inline'; img-src " + src + " data:; font-src " + src + " data:; media-src " + src + "; " +
		"connect-src 'none'; frame-src 'none'; worker-src 'none'; object-src 'none'; form-action 'none'; " +
		"base-uri 'none'; frame-ancestors 'self'"
}

// validCSPHost accepts host[:port] made of letters, digits, '.' and '-',
// with a numeric port.
func validCSPHost(h string) bool {
	host, port, hasPort := strings.Cut(h, ":")
	if host == "" || len(h) > 260 {
		return false
	}
	for i := 0; i < len(host); i++ {
		if c := host[i]; !isASCIIAlnum(c) && c != '.' && c != '-' {
			return false
		}
	}
	if hasPort {
		if port == "" || len(port) > 5 {
			return false
		}
		for i := 0; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				return false
			}
		}
	}
	return true
}

// WarnHTMLRemoteImages is the publish warning, and the viewer notice, for
// an HTML entry that references absolute http(s) images.
const WarnHTMLRemoteImages = "Remote images are not loaded in HTML artifacts; include them in the bundle."

// ViewResponse answers a view capability request.
type ViewResponse struct {
	// URL is the path of the entry under the view capability.
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
	// RemoteImages is true when the entry references absolute http(s)
	// images, which the frame does not load.
	RemoteImages bool `json:"remoteImages,omitempty"`
}

func viewMAC(key []byte, id string, seq int, exp int64) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(viewSigDomain + "\n" + id + "\n" + strconv.Itoa(seq) + "\n" + strconv.FormatInt(exp, 10)))
	return mac.Sum(nil)
}

// mintViewCapability returns the capability for version seq of artifact id
// until exp: "<id>.<seq>.<exp>.<signature>".
func mintViewCapability(key []byte, id string, seq int, exp time.Time) string {
	e := exp.Unix()
	return id + "." + strconv.Itoa(seq) + "." + strconv.FormatInt(e, 10) + "." +
		base64.RawURLEncoding.EncodeToString(viewMAC(key, id, seq, e))
}

var errBadCapability = errors.New("invalid view capability")

// parseViewCapability checks a capability and returns the artifact and
// version it names.
func parseViewCapability(key []byte, capability string, now time.Time) (string, int, error) {
	if len(key) == 0 {
		return "", 0, errBadCapability
	}
	parts := strings.Split(capability, ".")
	if len(parts) != 4 || !canonicalID(parts[0]) {
		return "", 0, errBadCapability
	}
	seq, ok := parseSeq(parts[1])
	if !ok {
		return "", 0, errBadCapability
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || strconv.FormatInt(exp, 10) != parts[2] {
		return "", 0, errBadCapability
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || !hmac.Equal(sig, viewMAC(key, parts[0], seq, exp)) {
		return "", 0, errBadCapability
	}
	t := time.Unix(exp, 0)
	if !now.Before(t) || t.After(now.Add(ViewTTL+viewMaxSkew)) {
		return "", 0, errBadCapability
	}
	return parts[0], seq, nil
}

// handleMintView implements POST /{id}/versions/{seq}/view: a view
// capability for a ready version whose entry is HTML, for a caller who may
// read it.
func (s *Service) handleMintView(w http.ResponseWriter, r *http.Request, id string, seq int) {
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return
	}
	v, ok := readyVersion(w, r, b, a, seq)
	if !ok {
		return
	}
	if len(b.viewKey) == 0 {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "artifact views are not configured")
		return
	}
	entry, err := b.store.GetFile(r.Context(), v.ID, v.EntryPath)
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: get entry failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the version")
		return
	}
	if entry.MediaType != mediaTypeHTML {
		writeError(w, http.StatusBadRequest, "bad_request", "only HTML entries are shown through a view")
		return
	}
	exp := time.Now().Add(ViewTTL).Truncate(time.Second)
	capability := mintViewCapability(b.viewKey, a.ID, v.Seq, exp)
	resp := ViewResponse{
		URL:       RouteView + capability + "/" + escapePath(v.EntryPath),
		ExpiresAt: exp.UTC(),
	}
	if window, _, err := s.entryWindow(r, b, entry); err == nil {
		resp.RemoteImages = htmlHasRemoteImages(window)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleView implements GET /api/v1/artifacts/view/<capability>/<path>.
// Every failure answers the same 404.
func (s *Service) handleView(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) < 2 {
		writeNotFound(w)
		return
	}
	b, ok := s.backend()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "artifact storage is not configured")
		return
	}
	id, seq, err := parseViewCapability(b.viewKey, segs[0], time.Now())
	if err != nil {
		writeNotFound(w)
		return
	}
	filePath := strings.Join(segs[1:], "/")
	if _, err := cleanFilePath(filePath); err != nil {
		writeNotFound(w)
		return
	}
	ctx := r.Context()
	a, err := b.store.GetArtifact(ctx, id)
	if err != nil || (a.ExpiresAt != nil && !time.Now().Before(*a.ExpiresAt)) {
		writeNotFound(w)
		return
	}
	v, err := b.store.GetVersion(ctx, a.ID, seq)
	if err != nil || v.State != VersionStateReady {
		writeNotFound(w)
		return
	}
	f, err := b.store.GetFile(ctx, v.ID, filePath)
	if err != nil || f.SHA256 == "" || f.Pending || (f.Origin == FileOriginRemote && f.FetchStatus != FetchStatusOK) {
		writeNotFound(w)
		return
	}
	h := w.Header()
	h.Set("Content-Type", responseContentType(f.MediaType))
	h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	h.Set("Content-Security-Policy", viewCSP(r.Host, segs[0]))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Disposition", "inline")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	rc, _, err := b.blobs.Download(ctx, BlobPath(b.hubID, f.SHA256))
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: blob read failed", "error", err)
		h.Del("Content-Length")
		writeError(w, http.StatusInternalServerError, "internal", "could not read the file")
		return
	}
	defer func() { _ = rc.Close() }()
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		slog.WarnContext(ctx, "artifacts: blob stream interrupted", "error", err)
	}
}

// entryWindow reads the first imageScanWindow bytes of a version's entry.
func (s *Service) entryWindow(r *http.Request, b backend, entry *File) (string, bool, error) {
	rc, _, err := b.blobs.Download(r.Context(), BlobPath(b.hubID, entry.SHA256))
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rc.Close() }()
	window, err := readWindow(rc, entry.Size)
	return window, entry.Size > imageScanWindow, err
}

// htmlHasRemoteImages reports whether an HTML document (cut to the scan
// window) has an <img> tag whose src is an absolute http(s) URL. It reads
// only <img> tags, each up to its first '>' and never past the next '<', so
// it is linear in the window.
func htmlHasRemoteImages(doc string) bool {
	// limit 1: the scan stops at the first remote image. Labels are never
	// hashed on this path; the seed is set anyway.
	s := &imageScan{doc: doc, limit: 1, seen: map[string]struct{}{}, seed: maphash.MakeSeed()}
	for i := 0; i < len(doc) && !s.full; {
		k := strings.IndexByte(doc[i:], '<')
		if k < 0 {
			break
		}
		i += k
		if end, ok := s.imgTag(i); ok {
			i = end
		}
		i++
	}
	return s.full
}

// escapePath escapes each segment of a slash-separated path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}
