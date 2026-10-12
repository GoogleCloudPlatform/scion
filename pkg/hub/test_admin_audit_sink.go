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
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// testAdminAuditTag prefixes every line the test admin audit sink writes, so
// the lines can be selected from the process output (journald on systemd
// test hubs) without parsing anything else.
const testAdminAuditTag = "[scion-test-admin-audit]"

// Test admin audit event names.
const (
	testAdminAuditEventGateStart = "test_admin_gate_start"
	testAdminAuditEventBrokerSet = "test_admin_broker_set"
	// testAdminAuditEventRequest is written for every request an admin-kind
	// test identity makes, before the handler runs. A write failure answers
	// 503 and the handler does not run.
	testAdminAuditEventRequest = "test_admin_request"
	// testAdminAuditEventResponse is written when that request completes,
	// with the response status and the decision.
	testAdminAuditEventResponse = "test_admin_response"
	// testAdminAuditEventPolicyDenial is written for every test-admin
	// policy refusal.
	testAdminAuditEventPolicyDenial = "test_admin_policy_denial"
)

// testAdminAuditChainSeed is the "prev" value of the first line of a chain.
// Each process start opens a new chain (seq 0) with its first line.
const testAdminAuditChainSeed = "0000000000000000000000000000000000000000000000000000000000000000"

// testAdminAuditOutput is where the sink writes: the process stdout. A
// package variable only so tests can capture the output.
var testAdminAuditOutput io.Writer = os.Stdout

// testAdminAuditSink is the synchronous, level-independent, hash-chained
// writer for test admin tier events (admin tier design §3.6.2). It
// deliberately does not use slog (whose level server.log_level controls at
// runtime), the async decision-audit writer, or any experiment: each event
// is one JSON line, written under a fixed tag directly to the output before
// emit returns.
//
// Every line carries "seq" (0 for the first line this process writes) and
// "prev", the SHA-256 (hex) of the previous line as written (the full line
// without its newline); the first line's prev is testAdminAuditChainSeed. A
// deleted, inserted or edited line therefore breaks the chain
// (verifyTestAdminAuditChain). A failed write does not advance the chain,
// so the next line links to the last line actually written.
//
// Events: the gate-start and test-mode-start lines, the broker set line,
// issuance, every request by an admin-kind test identity (before the
// handler, and its completion), every test-admin policy refusal, and
// revoke-all. Callers fail closed on a write error: issuance and admin-kind
// requests answer 503.
//
// No line ever carries a token or other credential: callers pass
// identifiers only (user, issuance, credential record IDs).
type testAdminAuditSink struct {
	mu   sync.Mutex
	out  io.Writer
	now  func() time.Time
	seq  uint64
	prev string
}

func newTestAdminAuditSink(out io.Writer) *testAdminAuditSink {
	return &testAdminAuditSink{out: out, now: time.Now, prev: testAdminAuditChainSeed}
}

// testAdminAuditReservedFields are set by the sink; a caller's field of the
// same name is replaced.
var testAdminAuditReservedFields = []string{"event", "ts", "seq", "prev"}

// emit writes one tagged, chained JSON line for event with the given
// fields. The "event", "ts", "seq" and "prev" fields are set by the sink.
// It returns the write error, if any; callers fail closed on it.
func (k *testAdminAuditSink) emit(event string, fields map[string]any) error {
	if k == nil || k.out == nil {
		return errors.New("test admin audit sink is not configured")
	}
	rec := make(map[string]any, len(fields)+len(testAdminAuditReservedFields))
	for key, v := range fields {
		rec[key] = v
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	prev := k.prev
	if prev == "" {
		prev = testAdminAuditChainSeed
	}
	rec["event"] = event
	rec["ts"] = k.now().UTC().Format(time.RFC3339Nano)
	rec["seq"] = k.seq
	rec["prev"] = prev
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line := make([]byte, 0, len(testAdminAuditTag)+1+len(body)+1)
	line = append(line, testAdminAuditTag...)
	line = append(line, ' ')
	line = append(line, body...)
	sum := sha256.Sum256(line)
	line = append(line, '\n')
	if _, err := k.out.Write(line); err != nil {
		return err
	}
	k.seq++
	k.prev = hex.EncodeToString(sum[:])
	return nil
}

// errTestAdminAuditChainBroken is returned by verifyTestAdminAuditChain.
var errTestAdminAuditChainBroken = errors.New("test admin audit chain is broken")

// verifyTestAdminAuditChain checks the test admin audit lines in r. Lines
// without the sink tag (other process output interleaved in the journal)
// are ignored. Each tagged line must parse, carry the next seq and link to
// the previous tagged line's hash; a line with seq 0 and the seed as prev
// opens a new chain (a process start). The first tagged line must open a
// chain. It returns the number of tagged lines checked, and an error
// naming the first break: a deleted, inserted, reordered or edited line.
func verifyTestAdminAuditChain(r io.Reader) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	tag := []byte(testAdminAuditTag + " ")
	var (
		n       int
		lineNo  int
		prevSum string
		nextSeq uint64
		started bool
	)
	for sc.Scan() {
		lineNo++
		raw := sc.Bytes()
		if !bytes.HasPrefix(raw, tag) {
			continue
		}
		n++
		var hdr struct {
			Seq  *uint64 `json:"seq"`
			Prev string  `json:"prev"`
		}
		if err := json.Unmarshal(raw[len(tag):], &hdr); err != nil || hdr.Seq == nil {
			return n, fmt.Errorf("%w: line %d is not a chained sink line", errTestAdminAuditChainBroken, lineNo)
		}
		switch {
		case *hdr.Seq == 0 && hdr.Prev == testAdminAuditChainSeed:
			// A new chain: a process start.
		case !started:
			return n, fmt.Errorf("%w: line %d does not open a chain (seq %d)", errTestAdminAuditChainBroken, lineNo, *hdr.Seq)
		case *hdr.Seq != nextSeq:
			return n, fmt.Errorf("%w: line %d has seq %d, want %d (a line is missing or out of order)", errTestAdminAuditChainBroken, lineNo, *hdr.Seq, nextSeq)
		case hdr.Prev != prevSum:
			return n, fmt.Errorf("%w: line %d does not link to the previous line (a line was edited, removed or inserted)", errTestAdminAuditChainBroken, lineNo)
		}
		started = true
		sum := sha256.Sum256(raw)
		prevSum = hex.EncodeToString(sum[:])
		nextSeq = *hdr.Seq + 1
	}
	if err := sc.Err(); err != nil {
		return n, err
	}
	return n, nil
}

// testAdminRequestAudit is the per-request record of an admin-kind test
// identity's request, carried in the request context by
// testAdminRequestAuditMiddleware.
type testAdminRequestAudit struct {
	mu      sync.Mutex
	denials map[string]bool
	order   []string
}

type testAdminRequestAuditKey struct{}

func testAdminRequestAuditFromContext(ctx context.Context) *testAdminRequestAudit {
	a, _ := ctx.Value(testAdminRequestAuditKey{}).(*testAdminRequestAudit)
	return a
}

// noteDenial records subject as refused by the test-admin policy in this
// request. It reports whether the subject is new for the request, so a
// subject evaluated many times (a capability projection) is written to the
// sink once per request.
func (a *testAdminRequestAudit) noteDenial(subject string) bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.denials == nil {
		a.denials = map[string]bool{}
	}
	if a.denials[subject] {
		return false
	}
	a.denials[subject] = true
	a.order = append(a.order, subject)
	return true
}

func (a *testAdminRequestAudit) deniedSubjects() []string {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.order...)
}

// recordTestAdminPolicyDenial writes a test_admin_policy_denial line for a
// refusal of subject (of class: permission, legacy_route, role_gate,
// settings) to the admin-kind identity userID. A write failure is logged;
// the refusal stands either way.
func (s *Server) recordTestAdminPolicyDenial(ctx context.Context, userID, class, subject string) {
	if !testAdminRequestAuditFromContext(ctx).noteDenial(class + ":" + subject) {
		return
	}
	fields := map[string]any{
		"user_id": userID,
		"class":   class,
		"subject": subject,
	}
	if ta, ok := testAdminActorFromContext(ctx); ok {
		fields["issuance_id"] = ta.IssuanceID
		fields["kind"] = ta.Kind
	}
	if route := routeFromContext(ctx); route != "" {
		fields["route_id"] = route
	}
	if err := s.testAdminAudit.emit(testAdminAuditEventPolicyDenial, fields); err != nil {
		slog.Error("test admin audit: policy denial line could not be written", "error", err)
	}
}

// testAdminAuditStatusWriter records the response status. It passes
// Flush, Hijack and the http.ResponseController methods through to the
// wrapped writer (Unwrap).
type testAdminAuditStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *testAdminAuditStatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *testAdminAuditStatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *testAdminAuditStatusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *testAdminAuditStatusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("the response writer does not support hijacking")
	}
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return h.Hijack()
}

func (w *testAdminAuditStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// testAdminResponseDecision names the outcome of an admin-kind request for
// the response line.
func testAdminResponseDecision(status int, policyDenied bool) string {
	switch {
	case policyDenied && (status == http.StatusForbidden || status == http.StatusNotFound):
		return "policy_denied"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "denied"
	case status >= 100 && status < 400:
		return "allowed"
	}
	return "failed"
}

// testAdminRequestAuditMiddleware writes the sink lines of every request
// an admin-kind test identity makes. It runs right after authentication:
// the JWT row block marks an admitted admin-kind identity in the request
// context (contextWithTestAdminActor). Before the handler runs it writes a
// test_admin_request line (method, route template, user, kind,
// issuance_id); if that write fails the request answers 503 and the
// handler does not run. When the handler returns it writes a
// test_admin_response line with the status and the decision. Requests of
// every other caller pass through untouched.
func (s *Server) testAdminRequestAuditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ta, ok := testAdminActorFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		userID := ""
		if u := GetUserIdentityFromContext(r.Context()); !isNilIdentity(u) {
			userID = u.ID()
		}
		route := ""
		if s.mux != nil {
			_, route = s.mux.Handler(r)
		}
		base := map[string]any{
			"user_id":     userID,
			"kind":        ta.Kind,
			"issuance_id": ta.IssuanceID,
			"method":      r.Method,
			"route":       route,
		}
		if err := s.testAdminAudit.emit(testAdminAuditEventRequest, base); err != nil {
			slog.Error("test admin audit: request line could not be written; refusing the request", "error", err)
			writeError(w, http.StatusServiceUnavailable, ErrCodeTestAdminAuditUnavailable,
				"the test admin audit sink is unavailable", nil)
			return
		}
		audit := &testAdminRequestAudit{}
		sw := &testAdminAuditStatusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), testAdminRequestAuditKey{}, audit)))
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		denied := audit.deniedSubjects()
		done := make(map[string]any, len(base)+3)
		for k, v := range base {
			done[k] = v
		}
		done["status"] = status
		done["decision"] = testAdminResponseDecision(status, len(denied) > 0)
		if len(denied) > 0 {
			done["policy_denials"] = denied
		}
		if err := s.testAdminAudit.emit(testAdminAuditEventResponse, done); err != nil {
			slog.Error("test admin audit: response line could not be written", "error", err)
		}
	})
}

// ErrCodeTestAdminAuditUnavailable is the API error code of a 503 answered
// because the test admin audit sink could not be written.
const ErrCodeTestAdminAuditUnavailable = "test_admin_audit_unavailable"
