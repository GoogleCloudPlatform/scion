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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
)

// Fingerprint identifies a credential value without holding it: the
// SHA-256 of the value. It is safe to compare and, shortened, to log.
type Fingerprint [sha256.Size]byte

// TokenFingerprint returns the fingerprint of a credential value.
func TokenFingerprint(token string) Fingerprint {
	return sha256.Sum256([]byte(token))
}

// Short returns the first 8 hex digits of the fingerprint, for logs.
func (f Fingerprint) Short() string {
	return hex.EncodeToString(f[:4])
}

// TokenInfo describes an agent credential without holding its value.
type TokenInfo struct {
	// ID is the credential's jti claim, or "" if it has none.
	ID string
	// IssuedAt is the iat claim; zero if absent.
	IssuedAt time.Time
	// Expiry is the exp claim; zero if absent.
	Expiry time.Time
	// Fingerprint is the SHA-256 of the value.
	Fingerprint Fingerprint
}

// LogID names the credential in a log line: its jti, or a short
// fingerprint when it has no jti. Never part of the value itself.
func (i TokenInfo) LogID() string {
	if i.ID != "" {
		return "jti=" + i.ID
	}
	return "sha256:" + i.Fingerprint.Short()
}

// agentTokenSignatureLen is the length of an HS256 signature, the only
// algorithm the hub signs agent credentials with.
const agentTokenSignatureLen = sha256.Size

// Fixed reasons a candidate credential is refused. They never quote the
// value or a decoder's error text, which can contain parts of it.
var (
	ErrCandidateEmpty     = errors.New("credential file is empty")
	ErrCandidateFormat    = errors.New("credential is not a three-part token")
	ErrCandidateHeader    = errors.New("credential header is not readable")
	ErrCandidateAlgorithm = errors.New("credential algorithm is not HS256")
	ErrCandidateSignature = errors.New("credential signature has the wrong length")
	ErrCandidateClaims    = errors.New("credential claims are not readable")
	ErrCandidateNoExpiry  = errors.New("credential has no expiry")
	ErrCandidateExpired   = errors.New("credential has expired")
	ErrCandidateSubject   = errors.New("credential subject does not match this agent")
	ErrCandidateOlder     = errors.New("credential is not newer than the last accepted one")
	// ErrCandidateRefused: the hub answered 401 or 403 for the credential.
	ErrCandidateRefused = errors.New("the hub did not accept the credential")
	// ErrCandidateUnchecked: no answer was obtained, or the answer was not a
	// 2xx with this agent's record, so the credential was neither accepted
	// nor refused.
	ErrCandidateUnchecked = errors.New("the hub could not confirm the credential")
)

type tokenClaims struct {
	Sub string `json:"sub"`
	Jti string `json:"jti"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

func decodeSegment(seg string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
}

// parseTokenClaims decodes the claims segment of a three-part token
// without checking its signature.
func parseTokenClaims(token string) (tokenClaims, []string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return tokenClaims{}, nil, ErrCandidateFormat
	}
	payload, err := decodeSegment(parts[1])
	if err != nil {
		return tokenClaims{}, nil, ErrCandidateClaims
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return tokenClaims{}, nil, ErrCandidateClaims
	}
	return claims, parts, nil
}

func infoFromClaims(token string, claims tokenClaims) TokenInfo {
	info := TokenInfo{ID: claims.Jti, Fingerprint: TokenFingerprint(token)}
	if claims.Iat > 0 {
		info.IssuedAt = time.Unix(claims.Iat, 0)
	}
	if claims.Exp > 0 {
		info.Expiry = time.Unix(claims.Exp, 0)
	}
	return info
}

// InspectToken describes token as far as it can be read, without
// validating it. A value that is not a readable three-part token gets
// only its fingerprint.
func InspectToken(token string) TokenInfo {
	claims, _, err := parseTokenClaims(token)
	if err != nil {
		return TokenInfo{Fingerprint: TokenFingerprint(token)}
	}
	return infoFromClaims(token, claims)
}

// ValidateCandidateToken checks that token, read from the credential file,
// is a complete agent credential this agent can adopt: three base64url
// parts, an HS256 header with a full-length signature (a partly written
// file fails here), an expiry after now, and a subject claim equal to
// agentID (a credential without a subject is refused). It does not check
// the signature itself; the hub does (see CheckCredentialAccepted). Errors
// are the fixed ErrCandidate* values.
func ValidateCandidateToken(token, agentID string, now time.Time) (TokenInfo, error) {
	if token == "" {
		return TokenInfo{}, ErrCandidateEmpty
	}
	claims, parts, err := parseTokenClaims(token)
	if err != nil {
		return TokenInfo{}, err
	}
	headerJSON, err := decodeSegment(parts[0])
	if err != nil {
		return TokenInfo{}, ErrCandidateHeader
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return TokenInfo{}, ErrCandidateHeader
	}
	if header.Alg != "HS256" {
		return TokenInfo{}, ErrCandidateAlgorithm
	}
	sig, err := decodeSegment(parts[2])
	if err != nil || len(sig) != agentTokenSignatureLen {
		return TokenInfo{}, ErrCandidateSignature
	}
	if claims.Exp == 0 {
		return TokenInfo{}, ErrCandidateNoExpiry
	}
	info := infoFromClaims(token, claims)
	if !now.Before(info.Expiry) {
		return TokenInfo{}, ErrCandidateExpired
	}
	if claims.Sub == "" || claims.Sub != agentID {
		return TokenInfo{}, ErrCandidateSubject
	}
	return info, nil
}

// noteVerified raises the verified issue time to token's iat, if later.
// Call it only for a credential the hub has accepted. With onlyIfCurrent,
// it changes nothing unless token is the in-memory credential.
func (c *Client) noteVerified(token string, onlyIfCurrent bool) {
	iat := InspectToken(token).IssuedAt
	if iat.IsZero() {
		return
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if onlyIfCurrent && c.token != token {
		return
	}
	if iat.After(c.verifiedIAT) {
		c.verifiedIAT = iat
	}
}

// VerifiedIssuedAt returns the newest issue time among agent credentials
// the hub has accepted from this client: a credential returned by a token
// refresh the hub URL itself answered, one CheckCredentialAccepted
// confirmed, or the in-memory credential once CheckCurrentCredentialAccepted
// confirmed it (if still in memory then). It is zero until one is accepted
// and never decreases. A credential read from the credential file must be
// newer than it to be adopted from the file.
func (c *Client) VerifiedIssuedAt() time.Time {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.verifiedIAT
}

// agentRecordMaxBytes bounds how much of the agent record
// CheckCredentialAccepted reads; a longer answer is not confirmation.
const agentRecordMaxBytes = 1 << 20

// directClient returns a copy of the client's HTTP client, built now so it
// has the client's current transport (including any transport credential),
// that returns the response to the request it was given as is: the caller
// sees the status the hub URL answered with.
func (c *Client) directClient() *http.Client {
	dc := *c.client
	dc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &dc
}

// CheckCredentialAccepted asks the hub whether it accepts token as this
// agent's credential, by reading this agent's own record (GET
// /api/v1/agents/{id}) with token as the agent credential, sent the same
// way as on any other request. The request changes nothing on the hub.
//
// It returns nil only for a 2xx from the hub URL itself (see directClient)
// whose body is this agent's record. That shows the hub's handler
// answered for this agent; it does not show that the credential belongs to
// this agent (a credential allowed to read another agent's record gets the
// same answer), which is why callers must also check the credential's
// subject (ValidateCandidateToken). A 401 or 403 returns
// ErrCandidateRefused; any other status, or no answer, returns
// ErrCandidateUnchecked. On success the verified issue time is raised to
// the credential's. Neither the credential nor the response body appears
// in an error.
func (c *Client) CheckCredentialAccepted(ctx context.Context, token string) error {
	if err := c.checkCredential(ctx, token); err != nil {
		return err
	}
	c.noteVerified(token, false)
	return nil
}

// CheckCurrentCredentialAccepted is CheckCredentialAccepted for the
// client's own credential: the verified issue time is raised only if token
// is still the in-memory credential when the hub's answer arrives. The
// result is recorded in the client's credential state for token (a check
// of a candidate, CheckCredentialAccepted, is not recorded).
func (c *Client) CheckCurrentCredentialAccepted(ctx context.Context, token string) error {
	err := c.checkCredential(ctx, token)
	if c != nil && token != "" {
		c.recordCheck(token, err)
	}
	if err != nil {
		return err
	}
	c.noteVerified(token, true)
	return nil
}

// checkCredential is CheckCredentialAccepted without recording the result.
func (c *Client) checkCredential(ctx context.Context, token string) error {
	if c == nil || c.hubURL == "" || c.agentID == "" || token == "" {
		return ErrCandidateUnchecked
	}
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s", strings.TrimSuffix(c.hubURL, "/"), c.agentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ErrCandidateUnchecked
	}
	c.setAgentAuth(req.Header, token)
	resp, err := c.directClient().Do(req)
	if err != nil {
		return ErrCandidateUnchecked
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrCandidateRefused
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return ErrCandidateUnchecked
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, agentRecordMaxBytes+1))
	if err != nil || len(body) > agentRecordMaxBytes {
		return ErrCandidateUnchecked
	}
	var record struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &record); err != nil || record.ID != c.agentID {
		return ErrCandidateUnchecked
	}
	return nil
}

// CredentialInfo describes the client's in-memory agent credential.
func (c *Client) CredentialInfo() TokenInfo {
	return InspectToken(c.GetToken())
}

// noteTransportFile records value as the transport token file's content
// as known to this client (written or adopted here).
func (c *Client) noteTransportFile(value string) {
	c.tokenMu.Lock()
	c.transportFP = TokenFingerprint(value)
	c.transportFPSet = true
	c.tokenMu.Unlock()
}

// NoteTransportTokenFile records the transport token file's current
// content as known to this client, so TransportTokenFileChanged reports
// only later writes by another process. A missing or empty file records
// nothing.
func (c *Client) NoteTransportTokenFile() {
	if v := readTransportTokenForWatch(); v != "" {
		c.noteTransportFile(v)
	}
}

// TransportTokenFileChanged reports whether the transport token file holds
// a value other than the last one this client wrote or adopted, that is,
// one written by another process (reset-auth). It is false when the
// client does not use a hub-provided transport token, or the file is
// missing or empty.
func (c *Client) TransportTokenFileChanged() bool {
	if !c.hasHubProvidedTransport() {
		return false
	}
	v := readTransportTokenForWatch()
	if v == "" {
		return false
	}
	fp := TokenFingerprint(v)
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return !c.transportFPSet || fp != c.transportFP
}

// readTokenFileQuiet reads the agent credential file like ReadTokenFile,
// through the same guarded reader, but without warning about a refused
// file: the watcher reads every few seconds, and a refused file is reported
// by the reset path when it reads the file.
func readTokenFileQuiet() string {
	v, err := readTokenFileGuarded(TokenFilePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// readTransportTokenForWatch reads the transport token file through the
// guarded reader, returning "" when it is missing, refused or empty.
func readTransportTokenForWatch() string {
	v, err := ReadTransportTokenFileGuarded(TransportTokenFilePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// Defaults for TokenFileWatcher.
const (
	DefaultTokenWatchInterval    = 5 * time.Second
	DefaultTokenWatchStableDelay = 250 * time.Millisecond
)

// TokenWatchConfig configures a TokenFileWatcher.
type TokenWatchConfig struct {
	// Interval between checks. Zero uses DefaultTokenWatchInterval.
	Interval time.Duration
	// StableDelay is how long after seeing a change the files are read
	// again; the change is reported only if both reads match, so a file
	// still being written is not reported. Zero uses
	// DefaultTokenWatchStableDelay.
	StableDelay time.Duration
	// Notify receives a value, without blocking, when a change is seen.
	// Give it a buffer of 1: pending notifications coalesce.
	Notify chan<- struct{}
	// Clock drives the interval and the stability delay; nil uses the
	// real clock.
	Clock clock.Clock

	// readAgent and readTransport read the two files (tests); nil uses
	// ReadTokenFile and the guarded transport token reader.
	readAgent     func() string
	readTransport func() string
}

// TokenFileWatcher polls the agent credential file and the transport token
// file and reports when either changes, so a credential written there
// without a reset signal is still picked up. It only reports changes; the
// receiver reads, validates and adopts. It reads through the same guarded,
// no-follow readers as every other credential read.
type TokenFileWatcher struct {
	cfg TokenWatchConfig

	mu            sync.Mutex
	lastAgent     Fingerprint
	lastAgentSet  bool
	lastTransport Fingerprint
	lastTransSet  bool
	// retryFP and retryAt: report retryFP again once retryAt has passed,
	// if the agent credential file still holds it (RetryAfter).
	retryFP  Fingerprint
	retryAt  time.Time
	retrySet bool
}

// NewTokenFileWatcher returns a watcher seeded with the files' current
// contents, so only later changes are reported.
func NewTokenFileWatcher(cfg TokenWatchConfig) *TokenFileWatcher {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultTokenWatchInterval
	}
	if cfg.StableDelay <= 0 {
		cfg.StableDelay = DefaultTokenWatchStableDelay
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	if cfg.readAgent == nil {
		cfg.readAgent = readTokenFileQuiet
	}
	if cfg.readTransport == nil {
		cfg.readTransport = readTransportTokenForWatch
	}
	w := &TokenFileWatcher{cfg: cfg}
	a, aok, t, tok := w.read()
	w.lastAgent, w.lastAgentSet = a, aok
	w.lastTransport, w.lastTransSet = t, tok
	return w
}

// read returns the fingerprints of the two files; ok is false for a
// missing or empty file.
func (w *TokenFileWatcher) read() (agent Fingerprint, agentOK bool, transport Fingerprint, transportOK bool) {
	if v := w.cfg.readAgent(); v != "" {
		agent, agentOK = TokenFingerprint(v), true
	}
	if v := w.cfg.readTransport(); v != "" {
		transport, transportOK = TokenFingerprint(v), true
	}
	return
}

// changed reports whether a read differs from the last recorded one. A
// file that became missing or empty is not a change: there is nothing to
// adopt.
func (w *TokenFileWatcher) changed(a Fingerprint, aok bool, t Fingerprint, tok bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return (aok && (!w.lastAgentSet || a != w.lastAgent)) ||
		(tok && (!w.lastTransSet || t != w.lastTransport))
}

// Refused tells the watcher that the receiver refused a credential with
// fingerprint refused. If the watcher last reported a different agent
// credential (the file changed between the report and the read), it
// forgets that one, so it is reported again if the file goes back to it.
// A refusal of the reported value itself changes nothing, so a refused
// file is reported once, not on every check.
func (w *TokenFileWatcher) Refused(refused Fingerprint) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastAgentSet && w.lastAgent != refused {
		w.lastAgentSet = false
	}
}

// RecheckAgent makes the watcher report the agent credential file's
// current value at its next check, even if it reported that value before.
func (w *TokenFileWatcher) RecheckAgent() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastAgentSet = false
}

// RetryAfter asks the watcher to report the agent credential with
// fingerprint fp again once d has passed on its clock, if the file still
// holds it then. It is for a credential whose check could not complete. A
// different value in the file is reported as usual and drops the request.
func (w *TokenFileWatcher) RetryAfter(fp Fingerprint, d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.retryFP, w.retryAt, w.retrySet = fp, w.cfg.Clock.Now().Add(d), true
}

// retryDue reports whether a RetryAfter request for the agent credential
// a is due, and clears a request that is due or no longer applies.
func (w *TokenFileWatcher) retryDue(a Fingerprint, aok bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.retrySet {
		return false
	}
	if !aok || a != w.retryFP {
		w.retrySet = false
		return false
	}
	if w.cfg.Clock.Now().Before(w.retryAt) {
		return false
	}
	w.retrySet = false
	return true
}

// Run checks the files every Interval until ctx is cancelled. It returns
// a channel that is closed when it stops.
func (w *TokenFileWatcher) Run(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if !w.sleep(ctx, w.cfg.Interval) {
				return
			}
			a, aok, t, tok := w.read()
			retry := w.retryDue(a, aok)
			if !retry && !w.changed(a, aok, t, tok) {
				continue
			}
			if !w.sleep(ctx, w.cfg.StableDelay) {
				return
			}
			a2, aok2, t2, tok2 := w.read()
			if a2 != a || aok2 != aok || t2 != t || tok2 != tok {
				// Still being written; check again next time.
				continue
			}
			w.mu.Lock()
			if aok {
				w.lastAgent, w.lastAgentSet = a, true
			}
			if tok {
				w.lastTransport, w.lastTransSet = t, true
			}
			w.mu.Unlock()
			select {
			case w.cfg.Notify <- struct{}{}:
			default:
			}
		}
	}()
	return done
}

// sleep waits d on the watcher's clock; it returns false if ctx ended.
func (w *TokenFileWatcher) sleep(ctx context.Context, d time.Duration) bool {
	timer, stop := clock.After(w.cfg.Clock, d)
	select {
	case <-ctx.Done():
		stop()
		return false
	case <-timer:
		return true
	}
}
