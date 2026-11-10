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

package hub

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/google/uuid"
)

// HMAC capability URLs for local-storage skill file downloads (#1792).
//
// Cloud storage hands out presigned object-store URLs that carry their own
// credential. Local storage has no equivalent: its download URLs point back at
// the Hub's /api/v1/skills/{id}/files/{path} route, which requires a principal.
// The runtime broker downloads skill files at dispatch without one (and the
// authorization kernel denies broker principals anyway), so every local-storage
// skill download failed 401.
//
// The Hub therefore signs the local-storage file URLs it issues from
// resolveRegistrySkillRef, after the creator's read access has been checked.
// The signature is a short-lived capability for exactly one file of exactly one
// skill version; the /files/ handler accepts it in place of a principal.
//
//	/api/v1/skills/<id>/files/<path>?raw=1&version=<v>&exp=<unix>&sig=<b64url(HMAC-SHA256(key, msg))>
//	msg = "skill-file\n" + id + "\n" + version + "\n" + path + "\n" + exp
//
// With a versioned signing ring (ptone/scion#1530 Phase 4, emit versioned)
// the URL also names the signing generation, and the message binds it:
//
//	...&kv=2&kid=<generation ID>&exp=<unix>&sig=<b64url(HMAC-SHA256(key, msg2))>
//	msg2 = "skill-file-v2\n" + kid + "\n" + id + "\n" + version + "\n" + path + "\n" + exp
//
// A URL without kv and kid is checked with the ring's legacy readers; one
// with both (each exactly once, kv=2) with generation kid only; any other
// combination is refused.

const (
	// SecretKeyDownloadSigningKey is the secret key name for the dedicated
	// download-URL signing key. It is deliberately separate from the agent
	// token, user token and OIDC keys.
	SecretKeyDownloadSigningKey = "download_signing_key"

	// skillFileURLTTL is how long a signed skill file URL stays valid. Matches
	// the GCS presigned-URL lifetime.
	skillFileURLTTL = 15 * time.Minute

	// skillFileURLMaxClockSkew bounds how far in the future an exp may lie
	// beyond the TTL. The Hub never issues such a URL, so anything further out
	// is rejected outright.
	skillFileURLMaxClockSkew = time.Minute

	// skillFileSigDomain is the domain-separation prefix of the signed message.
	skillFileSigDomain = "skill-file"

	// skillFileSigDomainV2 is the domain-separation prefix of the
	// versioned message.
	skillFileSigDomainV2 = "skill-file-v2"

	skillFileSigParam = "sig"
	skillFileExpParam = "exp"
	// skillFileVersionParam and skillFileKeyIDParam carry the versioned
	// form's version marker ("2") and signing generation ID.
	skillFileVersionParam = "kv"
	skillFileKeyIDParam   = "kid"
	skillFileVersion2     = "2"
)

// skillURLIssuanceClosedWarned makes the issuance-closed warning appear once
// per process.
var skillURLIssuanceClosedWarned atomic.Bool

// initDownloadSigningKey loads or creates the download signing key through the
// same persistence path as the Hub's other signing keys.
//
// Replicas must agree on the key or signed URLs fail intermittently with 401,
// so this follows the agent/user key policy: when the deployment requires
// stable keys (a GCP secret backend or RequireStableSigningKey) a failure is
// returned and startup fails. Otherwise — local development and single-node
// Hubs — it falls back to an in-memory key, since losing it only invalidates
// URLs that expire within skillFileURLTTL.
//
// In keyring mode the key comes from the signing ring instead (see
// capability_keys.go) and nothing is read from the secret backend.
func (s *Server) initDownloadSigningKey(ctx context.Context) error {
	if s.keyringManaged() {
		keys, err := s.managedCapabilityKeys(SecretKeyDownloadSigningKey)
		if err != nil {
			return err
		}
		s.downloadKeys = keys
		return nil
	}
	key, err := s.ensureSigningKey(ctx, SecretKeyDownloadSigningKey, nil)
	if err == nil && len(key) == 0 {
		err = fmt.Errorf("download signing key resolved to an empty value")
	}
	if err != nil {
		_, isGCPBackend := s.secretBackend.(*secret.GCPBackend)
		if isGCPBackend || s.config.RequireStableSigningKey {
			return fmt.Errorf("download signing key: %w", err)
		}
		slog.Warn("Download signing key could not be loaded or persisted; using an ephemeral in-memory key "+
			"(signed skill file URLs will not validate on other replicas or after restart)",
			"error", err)
		key = make([]byte, 32)
		if _, rerr := rand.Read(key); rerr != nil {
			return fmt.Errorf("generate ephemeral download signing key: %w", rerr)
		}
		s.recordSigningKeySource(SecretKeyDownloadSigningKey, signingKeySourceEphemeral)
	}
	s.downloadKeys = fixedCapabilityKeys(key)
	return nil
}

// computeSkillFileHMAC computes the raw HMAC-SHA256 bytes over the canonical
// message binding skill ID, version, file path and expiry. The fields are
// newline-separated; none of them can contain a newline (IDs are UUIDs,
// versions are semver, and file paths are validated), so the encoding is
// unambiguous.
func computeSkillFileHMAC(key []byte, skillID, version, filePath string, exp int64) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(skillFileSigDomain + "\n" + skillID + "\n" + version + "\n" + filePath + "\n" + strconv.FormatInt(exp, 10)))
	return mac.Sum(nil)
}

// computeSkillFileHMACv2 computes the raw HMAC-SHA256 bytes of the
// versioned message: the v2 domain and the signing generation ID kid, then
// the same fields in the same order as computeSkillFileHMAC.
func computeSkillFileHMACv2(key []byte, kid, skillID, version, filePath string, exp int64) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(skillFileSigDomainV2 + "\n" + kid + "\n" + skillID + "\n" + version + "\n" + filePath + "\n" + strconv.FormatInt(exp, 10)))
	return mac.Sum(nil)
}

// skillFileSignature computes the base64url (unpadded) encoding of the
// canonical HMAC-SHA256 for skillID@version/filePath and exp. See
// computeSkillFileHMAC for the message format.
func skillFileSignature(key []byte, skillID, version, filePath string, exp int64) string {
	return base64.RawURLEncoding.EncodeToString(computeSkillFileHMAC(key, skillID, version, filePath, exp))
}

// signDownloadURL returns the query-string suffix ("exp=...&sig=...", or
// "kv=2&kid=...&exp=...&sig=..." when the signing ring emits versioned
// values) that authorizes a download of filePath in skillID@version until
// expiry. It returns "" when no signing key is configured, or when no key
// may sign now (the active generation reached its sign_until), in which
// case the URL is issued unsigned and the request falls back to normal
// authorization.
func (s *Server) signDownloadURL(skillID, version, filePath string, expiry time.Time) string {
	if !s.downloadKeys.configured() || skillID == "" || version == "" || filePath == "" {
		return ""
	}
	key, versioned, err := s.downloadKeys.issue()
	if err != nil {
		if skillURLIssuanceClosedWarned.CompareAndSwap(false, true) {
			slog.Warn("Skill file URLs are issued unsigned: the active signing generation may no longer sign (sign_until reached)")
		}
		return ""
	}
	exp := expiry.Unix()
	expPart := skillFileExpParam + "=" + strconv.FormatInt(exp, 10) + "&"
	if versioned {
		sig := base64.RawURLEncoding.EncodeToString(computeSkillFileHMACv2(key.Bytes(), key.ID, skillID, version, filePath, exp))
		return skillFileVersionParam + "=" + skillFileVersion2 + "&" + skillFileKeyIDParam + "=" + url.QueryEscape(key.ID) + "&" +
			expPart + skillFileSigParam + "=" + url.QueryEscape(sig)
	}
	sig := skillFileSignature(key.Bytes(), skillID, version, filePath, exp)
	return expPart + skillFileSigParam + "=" + url.QueryEscape(sig)
}

// signSkillFileDownloadURLs appends a capability signature to every Hub
// skill-file URL (absolute or Hub-relative) in urls that points at this
// skill. URLs that do not route through the Hub's files endpoint — such as
// cloud-storage presigned URLs — are left untouched.
func (s *Server) signSkillFileDownloadURLs(urls []DownloadURLInfo, skillID, version string, now time.Time) []DownloadURLInfo {
	marker := "/api/v1/skills/" + skillID + "/files/"
	expiry := now.Add(skillFileURLTTL)
	for i := range urls {
		if !strings.Contains(urls[i].URL, marker) {
			continue
		}
		suffix := s.signDownloadURL(skillID, version, urls[i].Path, expiry)
		if suffix == "" {
			continue
		}
		sep := "?"
		if strings.Contains(urls[i].URL, "?") {
			sep = "&"
		}
		urls[i].URL += sep + suffix
	}
	return urls
}

// hasSkillFileSignature reports whether the request carries capability
// signature parameters (sig, exp, or the versioned form's kv or kid).
// Presence of any of them commits the request to signature validation: a
// malformed or partial signature is rejected rather than silently falling
// back to principal authorization.
func hasSkillFileSignature(r *http.Request) bool {
	raw := r.URL.RawQuery
	if !strings.Contains(raw, "sig=") && !strings.Contains(raw, "exp=") &&
		!strings.Contains(raw, "kv=") && !strings.Contains(raw, "kid=") {
		return false
	}
	q := r.URL.Query()
	return q.Has(skillFileSigParam) || q.Has(skillFileExpParam) ||
		q.Has(skillFileVersionParam) || q.Has(skillFileKeyIDParam)
}

// verifySkillFileSignature validates the request's capability signature for
// skillID@version/filePath. It checks expiry first, then recomputes the HMAC
// with each candidate key and compares in constant time. Without kv and kid
// the candidates are the signing ring's legacy readers; with both (each
// exactly once, kv=2 and a well-formed kid) only generation kid, which
// verifies only before its verify_until. Any missing, malformed, partial,
// repeated, expired or mismatched parameter, or an unknown kid, fails.
func (s *Server) verifySkillFileSignature(r *http.Request, skillID, version, filePath string, now time.Time) bool {
	if !s.downloadKeys.configured() {
		return false
	}
	q := r.URL.Query()
	// Exactly one value of each parameter: repeated parameters are ambiguous.
	if len(q[skillFileSigParam]) != 1 || len(q[skillFileExpParam]) != 1 {
		return false
	}
	kv, hasKV := q[skillFileVersionParam]
	kids, hasKid := q[skillFileKeyIDParam]
	kid := ""
	switch {
	case !hasKV && !hasKid:
		// The legacy (ID-less) form.
	case len(kv) == 1 && len(kids) == 1 && kv[0] == skillFileVersion2 && capabilityKeyIDPattern.MatchString(kids[0]):
		kid = kids[0]
	default:
		return false
	}
	expStr := q.Get(skillFileExpParam)
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || strconv.FormatInt(exp, 10) != expStr {
		return false
	}
	nowUnix := now.Unix()
	if nowUnix > exp {
		return false
	}
	if exp > nowUnix+int64((skillFileURLTTL+skillFileURLMaxClockSkew)/time.Second) {
		return false
	}
	// Strict decoding: exactly one encoding of a given MAC is accepted.
	got, err := base64.RawURLEncoding.Strict().DecodeString(q.Get(skillFileSigParam))
	if err != nil {
		return false
	}
	for _, k := range s.downloadKeys.candidates(kid) {
		var want []byte
		if kid == "" {
			want = computeSkillFileHMAC(k.Bytes(), skillID, version, filePath, exp)
		} else {
			want = computeSkillFileHMACv2(k.Bytes(), kid, skillID, version, filePath, exp)
		}
		if hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

// isSignedSkillFileRequest reports whether r has the shape of a capability-URL
// skill file download: a GET of /api/v1/skills/{id}/files/{path} carrying
// signature parameters. UnifiedAuthMiddleware lets such requests through
// without a principal; it checks only the shape, because the signing key lives
// on the Server. handleSkillFileRead then MUST verify the signature and
// rejects the request when it does not validate.
func isSignedSkillFileRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/skills/")
	if !ok {
		return false
	}
	// Only a clean path is admitted: no dot-segments, empty segments or
	// trailing slash, which the mux would otherwise redirect or reinterpret.
	if path.Clean(r.URL.Path) != r.URL.Path {
		return false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || parts[1] != "files" || !validSkillFilePath(parts[2]) {
		return false
	}
	// The ID must be a canonical UUID (skill IDs always are). This keeps
	// reserved segments such as "resolve", which handleSkillByID dispatches
	// elsewhere, from being reached anonymously.
	if !isCanonicalUUID(parts[0]) {
		return false
	}
	if !strings.Contains(r.URL.RawQuery, "sig=") || !strings.Contains(r.URL.RawQuery, "exp=") {
		return false
	}
	q := r.URL.Query()
	return q.Get(skillFileSigParam) != "" && q.Get(skillFileExpParam) != ""
}

// isCanonicalUUID reports whether id is a UUID in canonical lowercase
// 8-4-4-4-12 form, as produced by api.NewUUID. uuid.Parse alone also accepts
// braced, urn:uuid: and uppercase forms.
func isCanonicalUUID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}
