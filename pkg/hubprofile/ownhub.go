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

package hubprofile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The own-hub check by hub identity (ptone/scion#4240, X5).
//
// CheckNotOwnHub compares endpoint names. A session additionally compares
// hub identities before it sends the first profile credential: the target
// hub's persistent hub id against the agent's own hub id, both read from
// the hub's public health route (GET /healthz, "hub_id", or "hub.hub_id"
// on a combined web server). Equal ids are refused with ErrOwnHub. When
// either id is unavailable the name check, which Open has already applied,
// is the only check, and this is said in debug output only. The lookups
// can only add a refusal: the name check always runs first and a failed
// lookup never allows what it refused.
//
// The target lookup carries only the profile's transport credential (the
// IAP token, for transport iap) and no Authorization; the own-hub lookup
// carries no credential at all. Neither follows redirects.

const (
	healthPath          = "/healthz"
	hubIDLookupTimeout  = 5 * time.Second
	maxHubIDLen         = 256
	healthResponseLimit = 64 << 10
)

// errNoHubID is returned when a health response carries no hub id.
var errNoHubID = errors.New("the health response has no hub id")

// healthHubID decodes the hub id of a /healthz response body: the hub
// API's top-level "hub_id", or the combined web server's "hub.hub_id".
func healthHubID(body io.Reader) (string, error) {
	var h struct {
		HubID string `json:"hub_id"`
		Hub   *struct {
			HubID string `json:"hub_id"`
		} `json:"hub"`
	}
	if err := json.NewDecoder(io.LimitReader(body, healthResponseLimit)).Decode(&h); err != nil {
		return "", errors.New("the health response is not JSON")
	}
	id := h.HubID
	if id == "" && h.Hub != nil {
		id = h.Hub.HubID
	}
	if id == "" {
		return "", errNoHubID
	}
	if len(id) > maxHubIDLen || hasControlOrSpace(id) {
		return "", errors.New("the health response has a malformed hub id")
	}
	return id, nil
}

// fetchHubID reads the hub id from GET <base>/healthz with client. Errors
// never include the response body.
func fetchHubID(ctx context.Context, client *http.Client, base string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hubIDLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+healthPath, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", healthPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s returned %d", healthPath, resp.StatusCode)
	}
	return healthHubID(resp.Body)
}

// noRedirect makes a client return a redirect response instead of
// following it.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// ownHubBases returns the base URLs (scheme, host and path; no user info,
// query or fragment) of the agent's own hub endpoints, deduplicated. Open
// has already refused unparsable values (ownHubAddrs).
func ownHubBases(getenv func(string) string) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		v := strings.TrimSpace(getenv(k))
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || u.User != nil {
			continue
		}
		base := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/")
		if !seen[base] {
			seen[base] = true
			out = append(out, base)
		}
	}
	return out
}

// CheckNotOwnHubByID compares the target hub's id with the agent's own hub
// id and returns an error wrapping ErrOwnHub when they are equal. It runs
// once per session (later calls return the first result) and is applied
// before the session sends its first profile credential. An unavailable id
// leaves the name check, already applied by Open, as the only check.
func (s *Session) CheckNotOwnHubByID(ctx context.Context) error {
	s.ownHubOnce.Do(func() { s.ownHubErr = s.compareHubIDs(ctx) })
	return s.ownHubErr
}

func (s *Session) compareHubIDs(ctx context.Context) error {
	debugf := s.deps.Debugf
	owns := ownHubBases(s.deps.Getenv)
	if len(owns) == 0 {
		debugf("hub profile %q: own-hub check by hub id skipped: this agent's own hub endpoint is not set; name check only", s.opts.Profile.Name)
		return nil
	}
	targetClient := &http.Client{Transport: &profileTransport{s: s, base: s.deps.BaseTransport}, CheckRedirect: noRedirect}
	targetID, err := fetchHubID(ctx, targetClient, s.Endpoint())
	if err != nil {
		debugf("hub profile %q: own-hub check by hub id skipped: the target hub's id is unavailable (%v); name check only", s.opts.Profile.Name, err)
		return nil
	}
	ownClient := &http.Client{Transport: s.deps.BaseTransport, CheckRedirect: noRedirect}
	compared := false
	for _, base := range owns {
		ownID, err := fetchHubID(ctx, ownClient, base)
		if err != nil {
			debugf("hub profile %q: this agent's own hub id is unavailable from one endpoint (%v)", s.opts.Profile.Name, err)
			continue
		}
		compared = true
		if ownID == targetID {
			return fmt.Errorf("hub profile %q: %w (the target reports this agent's own hub id)", s.opts.Profile.Name, ErrOwnHub)
		}
	}
	if !compared {
		debugf("hub profile %q: own-hub check by hub id skipped: this agent's own hub id is unavailable; name check only", s.opts.Profile.Name)
		return nil
	}
	debugf("hub profile %q: own-hub check by hub id passed", s.opts.Profile.Name)
	return nil
}
