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
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// StartClaimSettings are the timing settings of the start claim every agent
// start runs under.
type StartClaimSettings struct {
	// LeaseTTL is the claim lease; the holder renews it every LeaseTTL/3.
	LeaseTTL time.Duration
	// MaxDuration is the hard deadline on any start, including a wait for
	// another hub node to dispatch it.
	MaxDuration time.Duration
	// UnconfirmedHold bounds how long a start with an unknown outcome
	// blocks other starts, sized from the broker's whole start budget
	// (pod ready 10m, exec ready 90s, 10s, plus a margin).
	UnconfirmedHold time.Duration
	// CreateUnconfirmedHold is UnconfirmedHold for create-and-start (and
	// for a queued-stop drain's stop claim).
	CreateUnconfirmedHold time.Duration
}

// Bounds of the start-claim settings.
const (
	minStartClaimLeaseTTL = 30 * time.Second
	maxStartClaimLeaseTTL = 5 * time.Minute
	// minStartMaxDuration is the broker's pod-ready bound plus a minute.
	minStartMaxDuration = 10*time.Minute + time.Minute
	// minStartUnconfirmedHold is pod ready plus exec ready plus a minute.
	minStartUnconfirmedHold = 10*time.Minute + 100*time.Second + time.Minute
	// minStartCreateUnconfirmedHold is the hub's synchronous dispatch
	// bound (120s) plus two heartbeat intervals.
	minStartCreateUnconfirmedHold = 120*time.Second + 2*30*time.Second
)

// DefaultStartClaimSettings returns the defaults.
func DefaultStartClaimSettings() StartClaimSettings {
	return StartClaimSettings{
		LeaseTTL:              90 * time.Second,
		MaxDuration:           12 * time.Minute,
		UnconfirmedHold:       13 * time.Minute,
		CreateUnconfirmedHold: 5 * time.Minute,
	}
}

// Holds returns the unconfirmed holds by kind.
func (c StartClaimSettings) Holds() store.StartClaimHolds {
	return store.StartClaimHolds{Default: c.UnconfirmedHold, Create: c.CreateUnconfirmedHold}
}

// normalized returns c with each zero value replaced by its default and
// each out-of-range value replaced by its default, plus one warning per
// replaced non-zero value.
func (c StartClaimSettings) normalized() (StartClaimSettings, []string) {
	d := DefaultStartClaimSettings()
	var warns []string
	check := func(name string, v *time.Duration, def time.Duration, ok bool, rule string) {
		if *v == 0 {
			*v = def
			return
		}
		if !ok {
			warns = append(warns, fmt.Sprintf("%s %s is out of range (%s); using default %s", name, *v, rule, def))
			*v = def
		}
	}
	check("start_claim_lease_ttl", &c.LeaseTTL, d.LeaseTTL,
		c.LeaseTTL >= minStartClaimLeaseTTL && c.LeaseTTL <= maxStartClaimLeaseTTL, "30s to 5m")
	check("start_max_duration", &c.MaxDuration, d.MaxDuration,
		c.MaxDuration >= minStartMaxDuration, "at least 11m")
	check("start_unconfirmed_hold", &c.UnconfirmedHold, d.UnconfirmedHold,
		c.UnconfirmedHold >= minStartUnconfirmedHold, "at least 12m40s")
	check("start_create_unconfirmed_hold", &c.CreateUnconfirmedHold, d.CreateUnconfirmedHold,
		c.CreateUnconfirmedHold >= minStartCreateUnconfirmedHold && c.CreateUnconfirmedHold <= c.UnconfirmedHold,
		"3m up to start_unconfirmed_hold")
	// The default create hold must itself fit under a configured hold.
	if c.CreateUnconfirmedHold > c.UnconfirmedHold {
		c.CreateUnconfirmedHold = c.UnconfirmedHold
	}
	return c, warns
}

// parseStartClaimSettings parses the four duration strings of a settings
// snapshot. An empty string keeps the matching field of base; an
// unparseable one also keeps it, with a warning.
func parseStartClaimSettings(base StartClaimSettings, lease, maxDur, hold, createHold string) (StartClaimSettings, []string) {
	var warns []string
	parse := func(name, v string, dst *time.Duration) {
		if v == "" {
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			warns = append(warns, fmt.Sprintf("invalid %s %q, keeping %s", name, v, *dst))
			return
		}
		*dst = d
	}
	parse("start_claim_lease_ttl", lease, &base.LeaseTTL)
	parse("start_max_duration", maxDur, &base.MaxDuration)
	parse("start_unconfirmed_hold", hold, &base.UnconfirmedHold)
	parse("start_create_unconfirmed_hold", createHold, &base.CreateUnconfirmedHold)
	return base, warns
}

// startClaimSettings returns the current start-claim settings.
func (s *Server) startClaimSettings() StartClaimSettings {
	if p := s.startClaimCfg.Load(); p != nil {
		return *p
	}
	return DefaultStartClaimSettings()
}

// setStartClaimSettings normalizes and stores c, logging each warning, and
// reports whether the stored value changed.
func (s *Server) setStartClaimSettings(c StartClaimSettings) bool {
	n, warns := c.normalized()
	for _, w := range warns {
		slog.Warn("start claim setting: " + w)
	}
	old := s.startClaimCfg.Swap(&n)
	return old == nil || *old != n
}
