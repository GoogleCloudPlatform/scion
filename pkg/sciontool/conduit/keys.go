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
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/target"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/backoff"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// KeyRefreshInterval is the longest a target goes without refreshing its
// grant verification keys. The hub activates a new signing key only after
// grant_key_activation (15m by default), so a target that refreshes at
// least this often already trusts a key before it signs anything.
const KeyRefreshInterval = 10 * time.Minute

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
// the configured shorter interval) until ctx ends, and at once on each
// kick (kickKeyRefresh). A failed fetch is retried with backoff
// (keyRetryMin doubling to keyRetryMax) instead of waiting a full
// interval.
func (a *Agent) refreshKeysLoop(ctx context.Context) {
	interval := a.opts.KeyRefreshInterval
	retry := newKeyRetryBackoff(interval)
	wait := interval
	for {
		select {
		case <-a.keyKick:
			// A kick already pending (Run kicks before starting the
			// loop) is served before any timer is armed.
		default:
			ch, stop := clock.After(a.clk, wait)
			select {
			case <-ctx.Done():
				stop()
				return
			case <-ch:
			case <-a.keyKick:
				stop()
			}
		}
		if ctx.Err() != nil {
			return
		}
		err := a.RefreshKeys(ctx)
		if err == nil || ctx.Err() != nil {
			wait = interval
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
func (a *Agent) RefreshKeys(ctx context.Context) error {
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
	return nil
}

// Values of target_stream_deadlines in the grant-key response.
const (
	deadlinesOff     = "off"
	deadlinesEnforce = "enforce"
)

// Keys exposes the current key set (tests).
func (a *Agent) Keys() *target.KeyHolder { return &a.keys }
