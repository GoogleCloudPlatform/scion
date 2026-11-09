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
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateFlatCloudRunHubEndpoint checks the hub URL a flat cloudrun
// instance gives its agents (amendment P5-CR, CR3 activation step 1): an
// absolute https URL with a host that is neither loopback nor link-local,
// no user information, and no percent sign in the host (an IPv6 zone or a
// percent-encoded name). Agents send their hub token to it. The flat host
// checks it at activation (precondition P5) and the broker checks it again
// at every start, with this one function.
func ValidateFlatCloudRunHubEndpoint(raw string) error {
	if raw == "" {
		return errors.New("no hub endpoint is configured for Cloud Run agents")
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return errors.New("the hub endpoint for Cloud Run agents must be an absolute URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("the hub endpoint for Cloud Run agents must use https, not %q", u.Scheme)
	}
	if u.User != nil {
		return errors.New("the hub endpoint for Cloud Run agents must not carry user information")
	}
	if strings.Contains(u.Host, "%") {
		return errors.New("the hub endpoint for Cloud Run agents must not contain a percent sign in its host (an IPv6 zone or a percent-encoded name)")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return errors.New("the hub endpoint for Cloud Run agents has no host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errors.New("the hub endpoint for Cloud Run agents is a loopback host")
	}
	if ip := net.ParseIP(host); ip != nil {
		switch {
		case ip.IsLoopback(), ip.IsUnspecified():
			return errors.New("the hub endpoint for Cloud Run agents is a loopback address")
		case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
			return errors.New("the hub endpoint for Cloud Run agents is a link-local address")
		}
	}
	return nil
}
