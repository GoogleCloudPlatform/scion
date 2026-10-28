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

package runtimebroker

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// Requests the hub sends on the broker's conduit session are served only
// when they are one of the broker RPCs the session lists in its Hello,
// carry a valid hub signature that names the receiving session, and have
// not been served before. Every other request is refused before any
// handler runs.

// conduitRPCRoute is one served broker RPC: its method and its path, split
// into segments, where "*" stands for exactly one non-empty segment, and
// the broker RPC capability name (conduit.BrokerRPCs) it serves.
type conduitRPCRoute struct {
	method   string
	segments []string
	name     string
}

// conduitRPCRoutes are the broker RPCs of conduit.BrokerRPCs, by method
// and path.
var conduitRPCRoutes = []conduitRPCRoute{
	{http.MethodGet, []string{"api", "v1", "agents", "*", "logs"}, conduit.RPCAgentLogs},
	{http.MethodPost, []string{"api", "v1", "agents", "*", "exec"}, conduit.RPCAgentExec},
	{http.MethodGet, []string{"api", "v1", "images", "status"}, conduit.RPCImageStatus},
	{http.MethodPost, []string{"api", "v1", "images", "pull"}, conduit.RPCImagePull},
	{http.MethodDelete, []string{"api", "v1", "images", "local"}, conduit.RPCImageDelete},
	{http.MethodDelete, []string{"api", "v1", "projects", "*"}, conduit.RPCProjectCleanup},
}

// conduitRPCAllowed reports whether method and the decoded path p are one
// of conduitRPCRoutes. A wildcard segment is never empty, "." or "..".
func conduitRPCAllowed(method, p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	segs := strings.Split(p[1:], "/")
	for _, r := range conduitRPCRoutes {
		if r.method != method || len(r.segments) != len(segs) {
			continue
		}
		ok := true
		for i, want := range r.segments {
			got := segs[i]
			if want == "*" {
				if got == "" || got == "." || got == ".." {
					ok = false
					break
				}
				continue
			}
			if got != want {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// conduitRPCGuard decides whether a request received on a broker conduit
// session is served.
type conduitRPCGuard struct {
	brokerID string
	// key returns the hub connection's current shared key.
	key     func() []byte
	maxSkew time.Duration
	seen    *seenOnce
	now     func() time.Time
}

// conduitSeenRequests remembers served requests for the whole broker
// process, across sessions and hub connections. A guard with an injected
// clock must use its own set (newSeenOnce): add moves the set's sweep time
// to the latest now it sees.
var conduitSeenRequests = newSeenOnce()

// NewConduitBrokerRPCHandler returns the RPC handler for the broker
// conduit sessions of the hub connection connectionName: requests reach
// handlers only when the guard for brokerID, checking signatures with key,
// accepts them.
func NewConduitBrokerRPCHandler(handlers http.Handler, connectionName, brokerID string, key func() []byte, log *slog.Logger) conduit.RPCHandler {
	return newGuardedConduitRPCHandler(handlers, connectionName, 0, log, newConduitRPCGuard(brokerID, key))
}

// newConduitRPCGuard returns the guard for brokerID, checking signatures
// with key.
func newConduitRPCGuard(brokerID string, key func() []byte) *conduitRPCGuard {
	return &conduitRPCGuard{brokerID: brokerID, key: key, maxSkew: 5 * time.Minute, seen: conduitSeenRequests, now: time.Now}
}

// conduitRPCRefusal is a refused request: its status and a fixed message.
type conduitRPCRefusal struct {
	status  int
	message string
}

// conduitRPCChecker decides whether a request received on a broker
// conduit session is served: nil to serve it, the refusal otherwise.
type conduitRPCChecker interface {
	check(ctx context.Context, req *wsprotocol.RequestEnvelope) *conduitRPCRefusal
}

var _ conduitRPCChecker = (*conduitRPCGuard)(nil)

// canonicalRequestPath reports whether p is a request path in the exact
// form the handlers route on: it starts with a slash, holds no question
// mark, hash, percent sign or control character, and parses back to
// itself under the broker's request host, with no user info and no
// fragment. It returns the parsed URL (with query) when it is.
func canonicalRequestPath(p, query string) (*url.URL, bool) {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "?#%") {
		return nil, false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x20 || c == 0x7f {
			return nil, false
		}
	}
	target := "http://runtime-broker" + p
	if query != "" {
		target += "?" + query
	}
	u, err := url.Parse(target)
	if err != nil || u.Host != "runtime-broker" || u.User != nil || u.Fragment != "" || u.RawPath != "" || u.Path != p {
		return nil, false
	}
	return u, true
}

// check returns nil when req, received on the session of ctx, may be
// served, and the refusal otherwise.
func (g *conduitRPCGuard) check(ctx context.Context, req *wsprotocol.RequestEnvelope) *conduitRPCRefusal {
	// Only the exact canonical form is served, so the path checked is the
	// path the handlers route on.
	u, ok := canonicalRequestPath(req.Path, req.Query)
	if !ok || !conduitRPCAllowed(req.Method, u.Path) {
		return &conduitRPCRefusal{http.StatusNotFound, "not found"}
	}
	unauth := func(msg string) *conduitRPCRefusal { return &conduitRPCRefusal{http.StatusUnauthorized, msg} }

	h := http.Header{}
	for k, v := range req.Headers {
		h.Set(k, v)
	}
	brokerID := h.Get(apiclient.HeaderBrokerID)
	timestamp := h.Get(apiclient.HeaderTimestamp)
	nonce := h.Get(apiclient.HeaderNonce)
	signature := h.Get(apiclient.HeaderSignature)
	if brokerID == "" || timestamp == "" || nonce == "" || signature == "" {
		return unauth("missing authentication headers")
	}
	if brokerID != g.brokerID {
		return unauth("wrong broker")
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return unauth("invalid timestamp format")
	}
	now := g.now()
	requestTime := time.Unix(ts, 0)
	skew := now.Sub(requestTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > g.maxSkew {
		return unauth("timestamp outside acceptable range")
	}
	sigBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return unauth("invalid signature encoding")
	}
	welcome := conduit.WelcomeFromContext(ctx)
	sessionID := welcome.GetSessionId()
	if welcome == nil || sessionID == "" || h.Get(conduit.BrokerRPCSessionHeader) != sessionID ||
		!signedHeaderListed(h.Get(apiclient.HeaderSignedHeaders), conduit.BrokerRPCSessionHeader) {
		return unauth("request not issued for this session")
	}
	key := g.key()
	if len(key) == 0 {
		return unauth("no key")
	}
	r := &http.Request{Method: req.Method, URL: u, Header: h}
	if len(req.Body) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(req.Body))
		r.ContentLength = int64(len(req.Body))
	}
	if !apiclient.VerifyHMAC(key, apiclient.BuildCanonicalString(r, timestamp, nonce), sigBytes) {
		return unauth("invalid signature")
	}
	// Recorded only after the signature verifies, so only a key holder can
	// add entries. The key is the verified signature bytes: they depend only
	// on the canonical signed input, so every header encoding of one signed
	// request (signature text spellings, which the base64 decoder accepts
	// with other unused bits or CR/LF; a nonce that takes in a signed header
	// line) has the same key.
	if !g.seen.add(string(sigBytes), requestTime.Add(g.maxSkew), now) {
		return unauth("request already served")
	}
	return nil
}

// signedHeaderListed reports whether name is in the semicolon-separated
// signed headers list, matching case-insensitively.
func signedHeaderListed(list, name string) bool {
	for _, n := range strings.Split(list, ";") {
		if strings.EqualFold(strings.TrimSpace(n), name) {
			return true
		}
	}
	return false
}

// seenOnce remembers keys until their expiry: a key is refused while it is
// held. Expired keys are dropped on each add, so it holds at most the keys
// of one acceptance window.
//
// Callers pass the now they checked the request against, which can be
// older than another caller's now (a request body read after the check,
// or scheduling). So add also refuses any key whose expiry is before the
// latest now it has swept with: such a key may already have been dropped,
// and accepting it could serve the same request twice. Only a request at
// the very edge of its window is refused this way, which is the safe side.
type seenOnce struct {
	mu   sync.Mutex
	keys map[string]time.Time
	// sweptTo is the latest now any add has swept with.
	sweptTo time.Time
}

func newSeenOnce() *seenOnce { return &seenOnce{keys: map[string]time.Time{}} }

// add records key until expiry and reports whether it was new.
func (s *seenOnce) add(key string, expiry, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.After(s.sweptTo) {
		s.sweptTo = now
	}
	for k, exp := range s.keys {
		if s.sweptTo.After(exp) {
			delete(s.keys, k)
		}
	}
	if s.sweptTo.After(expiry) {
		return false
	}
	if _, ok := s.keys[key]; ok {
		return false
	}
	s.keys[key] = expiry
	return true
}
