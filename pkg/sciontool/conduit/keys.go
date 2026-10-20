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

package conduit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/target"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/backoff"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// KeyRefreshInterval is the grant verification key refresh period, before
// jitter (design v2.21.27 §3.5, Key-set freshness: at least every 5
// minutes, ±10%). The hub activates a new signing key only after
// grant_key_activation (15m by default), so a target that refreshes this
// often already trusts a key before it signs anything, and a key the hub
// stops publishing stops verifying here within one period.
const KeyRefreshInterval = 5 * time.Minute

// keyRefreshJitter is the fraction by which each refresh period varies.
const keyRefreshJitter = 0.10

// KeyStaleAfter is how old the last successful key refresh may be before a
// target in enforce mode refuses every open and renewal grant, until a
// refresh succeeds (design v2.21.27 §3.5: three refresh periods).
const KeyStaleAfter = 15 * time.Minute

// GrantKeysHintInterval is the least time between two key fetches the
// refresh loop runs (a GrantKeysChanged hint, or the first fetch). A hint
// inside that window is served by one fetch at its end.
const GrantKeysHintInterval = 10 * time.Second

// jitterKeyRefresh returns d varied uniformly by ±keyRefreshJitter.
func jitterKeyRefresh(d time.Duration) time.Duration {
	span := float64(d) * keyRefreshJitter
	return d + time.Duration((rand.Float64()*2-1)*span)
}

// defaultKeyJitter is the refresh jitter of an agent whose options set
// none. This package's tests replace it with no jitter in TestMain, before
// any test runs.
var defaultKeyJitter = jitterKeyRefresh

// handleGrantKeysChanged is the session's OnGrantKeysChanged hook: the hub
// says its published keys changed. It only asks the refresh loop for a
// fetch; the keys still come only from the authenticated fetch.
func (a *Agent) handleGrantKeysChanged(context.Context) {
	log.Debug("Conduit: hub reports grant key change; fetching keys")
	a.kickKeyRefresh()
}

// errKeysStale refuses a grant while, in enforce mode, the last successful
// key refresh is older than KeyStaleAfter.
var errKeysStale = errors.New("grant keys not refreshed within the staleness limit")

// freshKeys is the key set renewal grants are verified against
// (target.Deadlines): the hub-direct set, except that in enforce mode a set
// whose last successful refresh is older than KeyStaleAfter holds no key.
// Every renewal is then refused as naming an unknown kid, which also
// triggers one key fetch, until a refresh succeeds. Deadlines already set
// keep running. Open grants get the same rule in verifyGrant.
type freshKeys struct{ a *Agent }

func (k freshKeys) Lookup(kid string) (grant.PublicKey, bool) {
	if k.a.keysStale() {
		return grant.PublicKey{}, false
	}
	return k.a.keys.Lookup(kid)
}

// keysStale reports whether, in enforce mode, the last successful key
// refresh is older than KeyStaleAfter.
func (a *Agent) keysStale() bool {
	if !a.enforceDeadlines.Load() {
		return false
	}
	last := a.lastKeyRefresh.Load()
	return last == 0 || a.clk.Now().Sub(time.Unix(0, last)) > KeyStaleAfter
}

// After a failed grant-key fetch the next attempt comes after
// keyRetryMin, doubling per consecutive failure up to keyRetryMax (both
// bounded by the refresh interval). A success restores the normal cadence.
const (
	keyRetryMin = 30 * time.Second
	keyRetryMax = 5 * time.Minute
)

// keyRefreshTimeout bounds one grant-key fetch.
const keyRefreshTimeout = 30 * time.Second

// maxKeyResponseBytes bounds the grant-key response body.
const maxKeyResponseBytes = 1 << 20

// UnknownKidFetchInterval is the least time between two key fetches made
// because a grant named a kid the key set lacks. It is one budget for the
// whole target, whatever the kid.
const UnknownKidFetchInterval = 10 * time.Second

// fetchForUnknownKid fetches the keys once because a grant named an
// unknown kid, unless such a fetch already started within
// UnknownKidFetchInterval (design v2.21.17 §3.5 "Target-side deadlines").
// Callers arriving during a fetch wait for it. Either way the caller
// verifies once more and refuses if the kid is still unknown.
func (a *Agent) fetchForUnknownKid(ctx context.Context) {
	a.unknownKidMu.Lock()
	defer a.unknownKidMu.Unlock()
	now := a.clk.Now()
	if !a.lastUnknownKidFetch.IsZero() && now.Sub(a.lastUnknownKidFetch) < UnknownKidFetchInterval {
		return
	}
	a.lastUnknownKidFetch = now
	if err := a.RefreshKeys(ctx); err != nil {
		log.Warn("Conduit: grant key fetch for an unknown key id failed: %v", err)
	}
}

// kickKeyRefresh asks the refresh loop to fetch the keys now. Kicks
// arriving while one is pending collapse into it.
func (a *Agent) kickKeyRefresh() {
	select {
	case a.keyKick <- struct{}{}:
	default:
	}
}

// refreshKeysLoop refreshes the grant keys every KeyRefreshInterval (or
// the configured shorter interval), varied by ±10%, until ctx ends, and on
// each kick (kickKeyRefresh): a GrantKeysChanged hint or the first fetch.
// A kick is served at once unless the last fetch started less than
// GrantKeysHintInterval ago; then it is served when that interval ends.
// Kicks arriving before a fetch starts are served by it; one arriving
// during a fetch gets one more fetch, so a kick is never dropped. A failed
// fetch is retried with backoff (keyRetryMin doubling to keyRetryMax)
// instead of waiting a full interval.
func (a *Agent) refreshKeysLoop(ctx context.Context) {
	interval := a.opts.KeyRefreshInterval
	jitter := a.opts.keyJitter
	if jitter == nil {
		jitter = defaultKeyJitter
	}
	retry := newKeyRetryBackoff(interval)
	wait := jitter(interval)
	var lastFetch time.Time
	for {
		kicked := false
		select {
		case <-a.keyKick:
			// A kick already pending (Run kicks before starting the
			// loop) is served before any timer is armed.
			kicked = true
		default:
			ch, stop := clock.After(a.clk, wait)
			select {
			case <-ctx.Done():
				stop()
				return
			case <-ch:
			case <-a.keyKick:
				stop()
				kicked = true
			}
		}
		if kicked && !lastFetch.IsZero() {
			if d := GrantKeysHintInterval - a.clk.Now().Sub(lastFetch); d > 0 {
				ch, stop := clock.After(a.clk, d)
				select {
				case <-ctx.Done():
					stop()
					return
				case <-ch:
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		select { // this fetch serves every kick that arrived before it
		case <-a.keyKick:
		default:
		}
		lastFetch = a.clk.Now()
		err := a.RefreshKeys(ctx)
		if err == nil || ctx.Err() != nil {
			wait = jitter(interval)
			retry.Reset()
			continue
		}
		wait = retry.Failure()
		log.Warn("Conduit: grant key refresh failed, retrying in %v: %v", wait, err)
	}
}

// newKeyRetryBackoff returns the grant-key retry policy for a refresh
// interval: keyRetryMin doubling to keyRetryMax, both bounded by the
// interval, without jitter.
func newKeyRetryBackoff(interval time.Duration) *backoff.Backoff {
	return &backoff.Backoff{Base: min(keyRetryMin, interval), Max: min(keyRetryMax, interval)}
}

// errKeyRedirect refuses a redirect on the grant-key route.
var errKeyRedirect = errors.New("grant keys: redirect not followed")

// keyClient returns a copy of c (http.DefaultClient if nil) that does not
// follow redirects; c itself is left unchanged.
func keyClient(c *http.Client) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	kc := *c
	kc.CheckRedirect = func(*http.Request, []*http.Request) error { return errKeyRedirect }
	return &kc
}

// RefreshKeys fetches the hub's current grant verification keys over the
// authenticated hub API (agent token, plus the transport credential) and
// replaces the key set. The same response sets whether stream deadlines
// are enforced (target_stream_deadlines: "enforce"; absent or "off" turns
// enforcement off). A response that fails to parse changes neither.
//
// Fetches run one at a time. A successful one is the refresh KeyStaleAfter
// counts from.
func (a *Agent) RefreshKeys(ctx context.Context) error {
	a.fetchMu.Lock()
	defer a.fetchMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, keyRefreshTimeout)
	defer cancel()
	endpoint := strings.TrimSuffix(a.opts.HubURL, "/") + "/api/v1/conduit/grant-keys"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	h, _ := a.header(ctx)
	req.Header = h
	resp, err := a.opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKeyResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("grant keys: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Keys                  []grant.WireKey `json:"keys"`
		TargetStreamDeadlines string          `json:"target_stream_deadlines"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("grant keys: %w", err)
	}
	var enforce bool
	switch out.TargetStreamDeadlines {
	case "", deadlinesOff:
	case deadlinesEnforce:
		enforce = true
	default:
		return fmt.Errorf("grant keys: unknown target_stream_deadlines %q", out.TargetStreamDeadlines)
	}
	if err := a.keys.Set(grant.FromWire(out.Keys)); err != nil {
		return fmt.Errorf("grant keys: %w", err)
	}
	a.enforceDeadlines.Store(enforce)
	a.lastKeyRefresh.Store(a.clk.Now().UnixNano())
	return nil
}

// Values of target_stream_deadlines in the grant-key response.
const (
	deadlinesOff     = "off"
	deadlinesEnforce = "enforce"
)

// Keys exposes the current key set (tests).
func (a *Agent) Keys() *target.KeyHolder { return &a.keys }
