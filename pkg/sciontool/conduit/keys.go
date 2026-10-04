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
	"slices"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/target"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
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

// appliedWelcomes is how many recent Welcomes applyWelcomeKeys remembers.
const appliedWelcomes = 4

// applyWelcomeKeys installs the grant keys of w once per Welcome: from
// OnSession, or from the first stream of the session if that arrives
// first. A Welcome applied before is not applied again, so it cannot undo
// a later refresh.
func (a *Agent) applyWelcomeKeys(w *conduitv1.Welcome) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if slices.Contains(a.applied, w) {
		return
	}
	a.applied = append(a.applied, w)
	if len(a.applied) > appliedWelcomes {
		a.applied = slices.Delete(a.applied, 0, len(a.applied)-appliedWelcomes)
	}
	if err := a.keys.SetFromWelcome(w); err != nil {
		log.Warn("Conduit: Welcome grant keys refused: %v", err)
	}
}

// refreshKeysLoop refreshes the grant keys every KeyRefreshInterval (or
// the configured shorter interval) until ctx ends. A failed fetch is
// retried with backoff (keyRetryMin doubling to keyRetryMax) instead of
// waiting a full interval.
func (a *Agent) refreshKeysLoop(ctx context.Context) {
	interval := a.opts.KeyRefreshInterval
	wait, retry := interval, min(keyRetryMin, interval)
	for {
		ch, stop := clock.After(a.clk, wait)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-ch:
		}
		err := a.RefreshKeys(ctx)
		if err == nil || ctx.Err() != nil {
			wait, retry = interval, min(keyRetryMin, interval)
			continue
		}
		wait, retry = retry, min(retry*2, keyRetryMax, interval)
		log.Warn("Conduit: grant key refresh failed, retrying in %v: %v", wait, err)
	}
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
// replaces the key set.
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
		Keys []grant.WireKey `json:"keys"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("grant keys: %w", err)
	}
	if err := a.keys.Set(grant.FromWire(out.Keys)); err != nil {
		return fmt.Errorf("grant keys: %w", err)
	}
	return nil
}

// Keys exposes the current key set (tests).
func (a *Agent) Keys() *target.KeyHolder { return &a.keys }
