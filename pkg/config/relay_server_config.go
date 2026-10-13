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

package config

import (
	"errors"
	"fmt"
	"net/url"
)

// RelayServerConfig holds the settings of a separate relay process, scion
// server --role relay (server.relay; design §3.9 "Separate relay"). The
// settings the relay shares with the hub's own relay (internal listener,
// peer auth, lifetime cap, ...) stay under server.hub.conduit. Read at
// startup.
type RelayServerConfig struct {
	// HubInternalURL is the base URL of the hub internal API the relay
	// calls for admission, RPCs and re-checks. Any hub replica serves it,
	// so it may be a load-balanced internal address.
	HubInternalURL string `json:"hubInternalUrl,omitempty" yaml:"hubInternalUrl,omitempty" koanf:"hubInternalUrl"`
	// PublicURL is the relay's own public URL (its
	// relay_instances.public_endpoint).
	PublicURL string `json:"publicUrl,omitempty" yaml:"publicUrl,omitempty" koanf:"publicUrl"`
}

// IsZero reports whether nothing is configured.
func (c RelayServerConfig) IsZero() bool {
	return c.HubInternalURL == "" && c.PublicURL == ""
}

// Validate checks the configured values. Whether a value is required
// depends on the server role and is checked at startup.
func (c RelayServerConfig) Validate() error {
	var errs []error
	for _, f := range []struct{ key, v string }{
		{"server.relay.hub_internal_url", c.HubInternalURL},
		{"server.relay.public_url", c.PublicURL},
	} {
		if f.v == "" {
			continue
		}
		u, err := url.Parse(f.v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			errs = append(errs, fmt.Errorf("invalid %s %q: want an http(s) URL without query, fragment or credentials", f.key, f.v))
		}
	}
	return errors.Join(errs...)
}
