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
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/googleapi"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// GET /api/v1/gcs/object implements the gs:// link fetch-and-render endpoint
// (steps 1-14 below). The client never names an
// agent or SA: the sending agent's current assigned service account is
// derived server-side from a message the viewer is authorized to read, and
// only for a URI that literally appears in that message's body.

const (
	// gcsLinkMaxBytes is the object size cap ("Size > 10 MiB -> 413"),
	// checked from Attrs before the reader ever opens.
	gcsLinkMaxBytes = 10 * 1024 * 1024

	// gcsLinkRequestDeadline bounds every request from the moment its
	// concurrency slot is acquired, including the token mint (step 10) and
	// the response write (step 13): see handleGCSObject's use of
	// context.WithTimeout and http.NewResponseController.SetWriteDeadline.
	// There is no separate mint-specific timeout: the mint uses this same
	// request-scoped ctx directly.
	gcsLinkRequestDeadline = 60 * time.Second

	// gcsLinkGlobalConcurrency is the process-wide in-flight fetch limit.
	gcsLinkGlobalConcurrency = 16

	// gcsLinkRateLimitPerMinute is the per-viewer rate limit.
	gcsLinkRateLimitPerMinute = 60

	// gcsLinkSniffBytes is how much of the object is inspected to decide
	// text vs. octet-stream, mirroring the conventional content-sniffing
	// prefix size.
	gcsLinkSniffBytes = 512

	// Error codes not already defined among the shared ErrCode* constants.
	gcsErrCodeTooLarge    = "too_large"
	gcsErrCodeUpstream    = "upstream_error"
	gcsObjectNotFoundBody = "Object not found"

	// gcsLinksExperiment is the registered experiment (pkg/experiments) that
	// gates both the web linkifier (LayerWeb) and this endpoint (LayerServer;
	// ptone/scion#2545). The hub decides hub behaviour (AGENTS.md "Experimental
	// features"): this constant is what handleGCSObject's step 1 actually
	// checks, and the web gate only mirrors it.
	gcsLinksExperiment = "web.gcs_links"
)

// gcsBucketPattern and gcsObjectPattern mirror the web client's
// GCS_URI_PATTERN bucket/object groups exactly (chat-file-links.ts), so a
// value that could never have come from a rendered link is rejected before
// any lookup. Go's RE2 engine has no lookaround, but none is needed here:
// both are plain anchored character classes.
var (
	gcsBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)
	gcsObjectPattern = regexp.MustCompile(`^[A-Za-z0-9._~+=,@%:!$*()/-]*[A-Za-z0-9_~+=@%]$`)
)

// parseGCSObjectQuery extracts message/bucket/object from the query string,
// requiring exactly those three keys, each present exactly once. Any extra
// key (including a client-supplied agent/sa/email) or a duplicated key fails
// closed with ok=false — this is what keeps the endpoint from ever accepting
// a client-asserted identity.
func parseGCSObjectQuery(r *http.Request) (messageID, bucket, object string, ok bool) {
	q := r.URL.Query()
	if len(q) != 3 {
		return "", "", "", false
	}
	for key, vals := range q {
		switch key {
		case "message", "bucket", "object":
			if len(vals) != 1 {
				return "", "", "", false
			}
		default:
			// Given the len(q) != 3 check above already ran, a request that
			// reaches this branch has exactly 3 distinct keys with at least
			// one not named message/bucket/object — which means at least one
			// of message/bucket/object is absent, so q.Get on it returns "".
			// validateGCSObjectParams already rejects an empty messageID
			// (fails uuid.Parse), an empty bucket (fails the length-3
			// minimum) and an empty object (the pattern requires >= 1 char)
			// alike, so this explicit reject is unreachable-in-practice
			// given that caller; it is kept because parseGCSObjectQuery must
			// stay correct on its own, independent of what its one caller
			// happens to check afterward.
			return "", "", "", false
		}
	}
	return q.Get("message"), q.Get("bucket"), q.Get("object"), true
}

// validateGCSObjectParams checks the syntax of already-extracted params
// (step 3: input validation, before any lookup). It performs no store or
// network lookup.
func validateGCSObjectParams(messageID, bucket, object string) bool {
	if _, err := uuid.Parse(messageID); err != nil {
		return false
	}
	if !gcsBucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") {
		return false
	}
	if len(object) > 1024 || !gcsObjectPattern.MatchString(object) {
		return false
	}
	return true
}

// isGCSWordByte matches JavaScript's ASCII \w: [A-Za-z0-9_].
func isGCSWordByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

// isGCSObjectClassByte is the object character class from GCS_URI_PATTERN /
// the object validation regex: [A-Za-z0-9._~+=,@%:!$*()/-].
func isGCSObjectClassByte(b byte) bool {
	if isGCSWordByte(b) {
		return true
	}
	switch b {
	case '.', '~', '+', '=', ',', '@', '%', ':', '!', '$', '*', '(', ')', '/', '-':
		return true
	}
	return false
}

// isGCSObjectFinalByte is GCS_URI_PATTERN's required final-character class:
// [A-Za-z0-9_~+=@%]. A valid extraction's last byte is always one of these.
func isGCSObjectFinalByte(b byte) bool {
	if isGCSWordByte(b) {
		return true
	}
	switch b {
	case '~', '+', '=', '@', '%':
		return true
	}
	return false
}

// isGCSObjectTrimByte is the sentence-punctuation subset of the object class
// that isGCSObjectFinalByte excludes but that the middle of an object still
// accepts: trailing occurrences of these are trimmed one at a time by
// extractGCSObjectAt, exactly as the client pattern's own required-final-char
// backtracking does. '/' is deliberately not a member — a trailing '/' means
// "directory-like" and is handled as an outright reject, never trimmed down
// to a shorter object (isGCSObjectClassByte \ (isGCSObjectFinalByte ∪ '/')
// is exactly this set).
func isGCSObjectTrimByte(b byte) bool {
	switch b {
	case '.', ',', ':', '!', '$', '*', '(', ')', '-':
		return true
	}
	return false
}

// isGCSWhitespaceByte is ASCII whitespace, used only by the continuation
// rule below. A non-ASCII byte is treated as non-whitespace, which is the
// conservative (reject-continuation) direction.
func isGCSWhitespaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// extractGCSObjectAt extracts the object GCS_URI_PATTERN would capture at a
// `gs://<bucket>/` occurrence: a maximal run of object-class characters,
// trimmed from the right of any trailing sentence punctuation
// (isGCSObjectTrimByte) down to a character in the required final class,
// full rejection — never a shorter fallback — if the run ends in '/' (a
// directory reference, never an object, even after trimming would otherwise
// reach a valid final character further back), and full rejection if what
// remains is immediately followed by '#', '?' or '&' with a further
// non-whitespace character after that (a fragment-, query- or param-like
// continuation, never a legitimate end of a posted name), or immediately
// followed by '\' at all (a markdown escape character: raw text can carry
// an escape the client's rendered, already-escaped-past text never sees the
// same way, so the server treats a trailing backslash as always voiding the
// candidate, with no separate whitespace exception).
//
// The server and the client run the identical trim/reject algorithm, but
// not the identical input: the client works on marked's rendered,
// HTML-escaped text, split at every tag; the server works on the raw
// markdown body. The two can therefore diverge for markdown-syntax-bearing
// text (emphasis/strike wrappers, escapes, entities) even though neither
// side's algorithm itself is wrong — a link the client renders can still be
// denied by the server, and a raw token the server would allow is not
// always the one the client extracts and shows as a link.
//
// rawEnd is always returned, regardless of ok: the end of the maximal
// object-class run before any trimming. A caller scanning left to right for
// multiple occurrences must skip to rawEnd either way, mirroring the client
// regex's own lastIndex, which advances past the whole raw run even when
// the extraction is void — this is what stops a further `gs://` nested
// inside that run from ever being a candidate on its own.
func extractGCSObjectAt(body string, start int) (object string, ok bool, rawEnd int) {
	end := start
	for end < len(body) && isGCSObjectClassByte(body[end]) {
		end++
	}
	rawEnd = end
	if end == start {
		return "", false, rawEnd
	}
	for end > start && isGCSObjectTrimByte(body[end-1]) {
		end--
	}
	if end == start {
		return "", false, rawEnd
	}
	if body[end-1] == '/' {
		return "", false, rawEnd
	}
	// Invariant: isGCSObjectClassByte minus (isGCSObjectTrimByte union '/')
	// is exactly isGCSObjectFinalByte, so the trim loop above always leaves
	// end-1 pointing at a final-class byte once it hasn't returned above.
	// Asserted directly rather than left implicit, since a class definition
	// changing out from under this relationship would otherwise fail silent.
	if !isGCSObjectFinalByte(body[end-1]) {
		return "", false, rawEnd
	}
	if end < len(body) {
		switch body[end] {
		case '#', '?', '&':
			if end+1 < len(body) && !isGCSWhitespaceByte(body[end+1]) {
				return "", false, rawEnd
			}
		case '\\':
			return "", false, rawEnd
		}
	}
	return body[start:end], true, rawEnd
}

// gcsBucketScanPattern matches GCS_URI_PATTERN's bucket group anchored only
// at the start of the given substring, for scanning a bucket embedded
// within a larger body — unlike gcsBucketPattern, which validates a
// complete, already-extracted bucket string anchored at both ends.
var gcsBucketScanPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]`)

// gcsOccurrenceAt attempts to match one gs://<bucket>/<object> candidate
// starting at pos, which must already point at "gs://" with a valid left
// boundary — the caller checks that. next is where a left-to-right scan
// must continue afterward: if no bucket matches here at all (or the bucket
// isn't followed by '/'), next is pos+1, exactly like a failed regex
// attempt trying the next position; otherwise next is extractGCSObjectAt's
// rawEnd, regardless of whether the object extraction itself is ok — see
// extractGCSObjectAt's own doc comment for why.
func gcsOccurrenceAt(body string, pos int) (bucket, object string, ok bool, next int) {
	start := pos + len("gs://")
	loc := gcsBucketScanPattern.FindStringIndex(body[start:])
	if loc == nil {
		return "", "", false, pos + 1
	}
	bucketEnd := start + loc[1]
	if bucketEnd >= len(body) || body[bucketEnd] != '/' {
		return "", "", false, pos + 1
	}
	object, ok, rawEnd := extractGCSObjectAt(body, bucketEnd+1)
	return body[start:bucketEnd], object, ok, rawEnd
}

// bodyReferencesGCSURI reports whether body contains a `gs://<bucket>/`
// occurrence whose (bucket, extractGCSObjectAt-result) equals
// (bucket, object) exactly — exact extraction equality, not a
// boundary-scan on the requested object's own literal text, and scanned
// left to right, non-overlapping, across ALL buckets, exactly mirroring the
// client regex's own global scan. This is what stops a viewer from turning
// a posted `gs://b/secret~` into a read of `gs://b/secret` (the extraction
// from the posted body is always "secret~", which a request for "secret"
// can never equal) and from turning a posted `gs://pub/x=gs://sec/key`
// into a read of `sec/key` (the whole "x=gs://sec/key" run belongs to the
// outer, pub-bucket candidate; the scan never re-starts partway through an
// already-consumed run, so the nested sec occurrence is never a candidate
// on its own — see gcsOccurrenceAt/extractGCSObjectAt).
func bodyReferencesGCSURI(body, bucket, object string) bool {
	i := 0
	for i < len(body) {
		idx := strings.Index(body[i:], "gs://")
		if idx == -1 {
			return false
		}
		pos := i + idx

		leftOK := true
		if pos > 0 {
			lb := body[pos-1]
			leftOK = !isGCSWordByte(lb) && lb != '/'
		}
		if !leftOK {
			i = pos + 1
			continue
		}

		candBucket, candObject, ok, next := gcsOccurrenceAt(body, pos)
		if ok && candBucket == bucket && candObject == object {
			return true
		}
		if next <= pos {
			next = pos + 1
		}
		i = next
	}
	return false
}

// resolveSenderAgent requires the message's Sender/SenderID to name an
// existing agent whose slug matches Sender exactly. A broker-inbound row
// claiming "agent:x" with an empty or user SenderID fails the prefix/lookup
// check; a SenderID that resolves to a different agent (slug mismatch)
// fails the final comparison.
//
// The two halves of the guard below are each individually redundant with
// that final comparison, given a store lookup that succeeds: "agent:" +
// agent.Slug != msg.Sender already requires msg.Sender to both start with
// "agent:" and match this exact agent, so a msg.Sender missing the prefix
// (or a msg.SenderID resolving to some agent) fails the final check on its
// own regardless of whether it is also caught here first. This early guard
// exists to skip the store round-trip for a message that plainly cannot be
// agent-sent, not to add an independent security boundary.
func (s *Server) resolveSenderAgent(ctx context.Context, msg *store.Message) *store.Agent {
	if !strings.HasPrefix(msg.Sender, "agent:") || msg.SenderID == "" {
		return nil
	}
	agent, err := s.store.GetAgent(ctx, msg.SenderID)
	if err != nil || agent == nil {
		return nil
	}
	if "agent:"+agent.Slug != msg.Sender {
		return nil
	}
	return agent
}

// resolveCurrentSA returns agent's current assigned service account, or nil
// if the agent has no usable one right now: the current assignment is
// resolved at request time, with no send-time snapshot. Unassigned,
// passthrough/block mode, a deleted SA record, or an
// SA record whose email no longer matches the assignment all return nil.
func (s *Server) resolveCurrentSA(ctx context.Context, agent *store.Agent) *store.GCPServiceAccount {
	if agent.AppliedConfig == nil || agent.AppliedConfig.GCPIdentity == nil {
		return nil
	}
	gcpID := agent.AppliedConfig.GCPIdentity
	if gcpID.MetadataMode != store.GCPMetadataModeAssign || gcpID.ServiceAccountID == "" || gcpID.ServiceAccountEmail == "" {
		return nil
	}
	sa, err := s.store.GetGCPServiceAccount(ctx, gcpID.ServiceAccountID)
	if err != nil || sa == nil {
		return nil
	}
	if sa.Email != gcpID.ServiceAccountEmail {
		return nil
	}
	return sa
}

// classifyGCSError maps a GCS Attrs/read error to an audit reason. Only
// 404/403/401 get a specific reason; everything else (5xx, timeouts,
// network errors) is an upstream error.
func classifyGCSError(err error) GCSLinkFetchReason {
	if errors.Is(err, storage.ErrObjectNotExist) {
		return GCSLinkReasonGCSNotFound
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		switch gerr.Code {
		case http.StatusNotFound:
			return GCSLinkReasonGCSNotFound
		case http.StatusForbidden, http.StatusUnauthorized:
			return GCSLinkReasonGCSDenied
		}
	}
	return GCSLinkReasonUpstreamError
}

// gcsObjectBasename returns the last '/'-separated segment of object, used
// only for the Content-Disposition filename — never for authorization.
func gcsObjectBasename(object string) string {
	if idx := strings.LastIndexByte(object, '/'); idx >= 0 {
		return object[idx+1:]
	}
	return object
}

// isRFC5987AttrChar is the RFC 5987 attr-char set used by the filename*
// extended parameter. Everything else, including every byte outside ASCII,
// is percent-encoded.
func isRFC5987AttrChar(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// gcsContentDisposition always returns the RFC 5987 extended-parameter form
// (filename*=UTF-8, two single quotes, then the percent-encoded name), never
// a plain quoted filename — unlike project_workspace_handlers.go's
// contentDisposition, which lets mime.FormatMediaType choose the plain form
// when it can.
func gcsContentDisposition(filename string) string {
	var b strings.Builder
	b.WriteString("attachment; filename*=UTF-8''")
	for i := 0; i < len(filename); i++ {
		c := filename[i]
		if isRFC5987AttrChar(c) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			const hex = "0123456789ABCDEF"
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xF])
		}
	}
	return b.String()
}

// gcsSniffLooksLikeText reports whether sniffedPrefix should be served as
// text/plain. When truncated is true, sniffedPrefix is not the object's
// entire content — the sniff window can then cut a multi-byte UTF-8 rune
// exactly in half at the boundary, which a bare utf8.Valid would wrongly
// read as invalid encoding rather than "more bytes needed." Successively
// shorter trims (at most utf8.UTFMax-1 bytes, the longest possible
// incomplete lead sequence) are tried before concluding the object is not
// valid UTF-8 text.
func gcsSniffLooksLikeText(sniffedPrefix []byte, truncated bool) bool {
	if utf8.Valid(sniffedPrefix) {
		return true
	}
	if !truncated {
		return false
	}
	for trim := 1; trim < utf8.UTFMax && trim < len(sniffedPrefix); trim++ {
		if utf8.Valid(sniffedPrefix[:len(sniffedPrefix)-trim]) {
			return true
		}
	}
	return false
}

// gcsSniffedImageType returns one of the four supported raster image
// Content-Types if http.DetectContentType recognizes sniffedPrefix as such,
// or "" otherwise. http.DetectContentType never reports image/svg+xml (SVG
// is plain-text XML, not a sniffed binary signature) and its text/html
// result is not read here at all, so an object whose bytes are HTML -
// regardless of its stored metadata or file extension - never takes this
// branch; it falls through to the caller's own text/octet-stream decision.
func gcsSniffedImageType(sniffedPrefix []byte) string {
	switch ct := http.DetectContentType(sniffedPrefix); ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return ct
	default:
		return ""
	}
}

// gcsContentType decides the response Content-Type from the object's own
// bytes, never from its stored metadata. An object with a Content-Encoding
// is always application/octet-stream - a compressed object is never
// transcoded and then mislabeled, and the stored bytes are compressed data
// that would not sniff as a meaningful image or text type anyway. Otherwise,
// one of the four supported raster image types is used if
// http.DetectContentType recognizes the sniffed prefix as one of them;
// otherwise a sniffed prefix that is valid UTF-8 gets text/plain; everything
// else gets application/octet-stream.
func gcsContentType(contentEncoding string, sniffedPrefix []byte, truncated bool) string {
	if contentEncoding != "" {
		return "application/octet-stream"
	}
	if ct := gcsSniffedImageType(sniffedPrefix); ct != "" {
		return ct
	}
	if gcsSniffLooksLikeText(sniffedPrefix, truncated) {
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// gcsWriteDeny writes the fixed body for a deny status. The 404 branch is
// the enumeration-resistant one: every 404 deny path (missing message,
// unreadable message, URI not in body, non-agent sender, no SA, GCS
// 404/403/401) calls this exact function with this exact literal, so the
// bytes on the wire never vary by reason.
func gcsWriteDeny(w http.ResponseWriter, status int) {
	switch status {
	case http.StatusNotFound:
		writeError(w, http.StatusNotFound, ErrCodeNotFound, gcsObjectNotFoundBody, nil)
	case http.StatusBadRequest:
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid gs:// object request", nil)
	case http.StatusTooManyRequests:
		writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "rate limit exceeded", nil)
	case http.StatusBadGateway:
		writeError(w, http.StatusBadGateway, gcsErrCodeUpstream, "could not fetch the object", nil)
	default:
		InternalError(w)
	}
}

// handleGCSObject handles GET /api/v1/gcs/object. See the package-level
// doc comment above for the step numbering this function follows.
func (s *Server) handleGCSObject(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqCtx := r.Context()

	event := &GCSLinkFetchEvent{Timestamp: start}
	status := http.StatusOK

	emitAudit := func() {
		event.Status = status
		event.DurationMS = time.Since(start).Milliseconds()
		if s.auditLogger != nil {
			_ = s.auditLogger.LogGCSLinkFetchEvent(reqCtx, event)
		}
	}
	deny := func(reason GCSLinkFetchReason, code int) {
		event.Decision = "deny"
		event.Reason = reason
		status = code
		gcsWriteDeny(w, code)
		emitAudit()
	}

	// A non-GET method has no reason enum value of its own and is not an
	// enumeration-sensitive case (unlike every gcsWriteDeny path below, it
	// carries no information about any particular message/bucket/object), so
	// it writes the ordinary MethodNotAllowed response directly rather than
	// through gcsWriteDeny — but exactly one audit event is still emitted for
	// every request, so it is recorded here rather than skipped.
	if r.Method != http.MethodGet {
		event.Decision = "deny"
		event.Reason = GCSLinkReasonBadRequest
		status = http.StatusMethodNotAllowed
		MethodNotAllowed(w, http.MethodGet)
		emitAudit()
		return
	}

	// Step 1: feature availability. Both the registered experiment and a
	// configured token generator are required; either missing denies with
	// the same uniform not-available response, so a caller cannot tell which
	// one is off.
	if !s.experimentEnabled(gcsLinksExperiment) || s.gcpTokenGenerator == nil {
		deny(GCSLinkReasonFeatureOff, http.StatusNotFound)
		return
	}

	// Step 2: caller must be a user identity — never an agent token, and
	// never unauthenticated.
	user := GetUserIdentityFromContext(reqCtx)
	if user == nil {
		deny(GCSLinkReasonNotUser, http.StatusNotFound)
		return
	}
	event.ViewerUserID = user.ID()

	// Step 3: syntax validation, no lookups yet. Any extra, missing or
	// duplicated query key — including a client-supplied agent/sa/email —
	// is rejected here.
	messageID, bucket, object, ok := parseGCSObjectQuery(r)
	if !ok || !validateGCSObjectParams(messageID, bucket, object) {
		deny(GCSLinkReasonBadRequest, http.StatusBadRequest)
		return
	}
	event.MessageID = messageID
	event.Bucket = bucket
	event.Object = object

	// Step 4: per-user rate limit, then the global concurrency slot.
	if s.gcsLinkRateLimiter != nil && !s.gcsLinkRateLimiter.Allow(user.ID()) {
		deny(GCSLinkReasonRateLimited, http.StatusTooManyRequests)
		return
	}
	if s.gcsLinkSem != nil {
		select {
		case s.gcsLinkSem <- struct{}{}:
			defer func() { <-s.gcsLinkSem }()
		default:
			deny(GCSLinkReasonRateLimited, http.StatusTooManyRequests)
			return
		}
	}

	ctx, cancel := context.WithTimeout(reqCtx, gcsLinkEffectiveRequestDeadline(s))
	defer cancel()

	// Step 5: message lookup.
	msg, err := s.store.GetMessage(ctx, messageID)
	if err != nil {
		deny(GCSLinkReasonMessageNotFound, http.StatusNotFound)
		return
	}
	event.ConversationID = msg.ConversationID

	// Step 6: message visibility, including the web-chat soft-delete flag.
	if !s.canUserReadMessage(ctx, user, msg) {
		deny(GCSLinkReasonMessageNotReadable, http.StatusNotFound)
		return
	}

	// Step 7: the URI must appear in the message's current stored body.
	if !bodyReferencesGCSURI(msg.Msg, bucket, object) {
		deny(GCSLinkReasonURINotInBody, http.StatusNotFound)
		return
	}

	// Step 8: sender must be a resolvable agent.
	sender := s.resolveSenderAgent(ctx, msg)
	if sender == nil {
		deny(GCSLinkReasonSenderNotAgent, http.StatusNotFound)
		return
	}
	event.SenderAgentID = sender.ID

	// Step 9: the sender's current assigned SA.
	sa := s.resolveCurrentSA(ctx, sender)
	if sa == nil {
		deny(GCSLinkReasonNoSA, http.StatusNotFound)
		return
	}
	event.SAEmail = sa.Email

	// Step 10: mint / client for sa. This is the first point that talks to
	// GCP at all — every check above it is a store lookup or pure function.
	factory := s.gcsLinkSourceFactory
	if factory == nil {
		factory = s.gcsObjectSourceFor
	}
	source, releaseSource, err := factory(ctx, sa.Email)
	if err != nil {
		deny(GCSLinkReasonMintFailed, http.StatusBadGateway)
		return
	}
	defer releaseSource()

	// Step 11: Attrs. GCS 404/403/401 all fall through to the same 404*
	// body; only the audit reason distinguishes them.
	attrs, err := source.Attrs(ctx, bucket, object)
	if err != nil {
		reason := classifyGCSError(err)
		if reason == GCSLinkReasonGCSNotFound || reason == GCSLinkReasonGCSDenied {
			deny(reason, http.StatusNotFound)
		} else {
			deny(GCSLinkReasonUpstreamError, http.StatusBadGateway)
		}
		return
	}

	// Step 12: size cap, before the reader ever opens.
	if attrs.Size > gcsLinkMaxBytes {
		event.Generation = attrs.Generation
		event.Decision = "deny"
		event.Reason = GCSLinkReasonTooLarge
		status = http.StatusRequestEntityTooLarge
		writeError(w, http.StatusRequestEntityTooLarge, gcsErrCodeTooLarge, "object exceeds the size limit", map[string]interface{}{
			"size":  attrs.Size,
			"limit": int64(gcsLinkMaxBytes),
		})
		emitAudit()
		return
	}

	// Step 13: stream with safe headers, generation pinned. A precondition
	// failure from a concurrent overwrite (or any other read error) is a 502,
	// never a partial or substituted body — see gcsClientSource.Open.
	reader, err := source.Open(ctx, bucket, object, attrs.Generation)
	if err != nil {
		deny(GCSLinkReasonUpstreamError, http.StatusBadGateway)
		return
	}
	defer func() { _ = reader.Close() }()

	limited := io.LimitReader(reader, attrs.Size)
	peek := make([]byte, gcsLinkSniffBytes)
	n, readErr := io.ReadFull(limited, peek)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		deny(GCSLinkReasonUpstreamError, http.StatusBadGateway)
		return
	}
	peek = peek[:n]
	// truncated: the peek stopped only because the sniff window filled up,
	// not because the object ended — there is more content after it, so a
	// multi-byte rune may have been cut exactly at the boundary.
	truncated := n == gcsLinkSniffBytes && attrs.Size > int64(n)

	w.Header().Set("Content-Type", gcsContentType(attrs.ContentEncoding, peek, truncated))
	w.Header().Set("Content-Length", strconv.FormatInt(attrs.Size, 10))
	w.Header().Set("Content-Disposition", gcsContentDisposition(gcsObjectBasename(object)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", untrustedContentSandboxCSP)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Scion-Gcs-Generation", strconv.FormatInt(attrs.Generation, 10))

	// Bound the response write by the same deadline as the rest of the
	// request: a client that stops reading would otherwise hold this
	// goroutine, and its concurrency slot, in an in-progress Write for as
	// long as the hub's server-wide WriteTimeout allows — operator-
	// configurable, including to 0 (unbounded). Per-request is strictly
	// tighter than that, and never looser. A ResponseWriter that does not
	// support it (http.ErrNotSupported) is logged and otherwise ignored:
	// this is a defense in depth, not a correctness requirement the
	// request's own success depends on.
	if deadline, ok := ctx.Deadline(); ok {
		if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil {
			slog.DebugContext(ctx, "gcs link: SetWriteDeadline not applied", "error", err)
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(peek)
	nc, copyErr := io.Copy(w, limited)

	// Step 14: exactly one audit event, allow this time. The status stays
	// 200 regardless of copyErr: the header line and status are already on
	// the wire by the time io.Copy could fail, so there is no HTTP-level
	// error left to report — only the audit record can still distinguish a
	// stream that failed partway through from one that completed cleanly.
	event.Generation = attrs.Generation
	event.Bytes = int64(len(peek)) + nc
	event.Decision = "allow"
	event.Reason = GCSLinkReasonOK
	if copyErr != nil {
		event.Reason = GCSLinkReasonUpstreamError
	}
	status = http.StatusOK
	emitAudit()
}
