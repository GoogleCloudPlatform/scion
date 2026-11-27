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
	"fmt"
	"slices"
	"strings"
)

// defaultConduitDeliverRuntimes are the runtime names whose agents may deliver
// messages over Conduit when server.broker.conduit_deliver_runtimes is unset.
var defaultConduitDeliverRuntimes = []string{
	"cloudrun",
	"kubernetes",
	"docker",
}

// knownConduitDeliverRuntimes lists the valid Runtime.Name() values accepted by
// server.broker.conduit_deliver_runtimes.
var knownConduitDeliverRuntimes = []string{
	"cloudrun",
	"kubernetes",
	"docker",
	"podman",
	"container",
	"substrate",
	"cloudrun-sandbox",
}

// DefaultConduitDeliverRuntimes returns a fresh copy of the default runtime set
// used when server.broker.conduit_deliver_runtimes is unset.
func DefaultConduitDeliverRuntimes() []string {
	return append([]string(nil), defaultConduitDeliverRuntimes...)
}

// NormalizeConduitDeliverRuntimes validates and deduplicates a configured
// server.broker.conduit_deliver_runtimes list.
//
// A nil slice represents "unset" and is returned as nil, nil. A non-nil empty
// slice represents an explicit empty list (disable Conduit delivery on all
// runtimes) and is returned as a non-nil empty slice. Any entry that is empty
// after trimming or not a known runtime name returns an error naming the
// offending entry and the known runtime names.
func NormalizeConduitDeliverRuntimes(runtimes []string) ([]string, error) {
	if runtimes == nil {
		return nil, nil
	}
	seen := make(map[string]bool, len(runtimes))
	out := make([]string, 0, len(runtimes))
	for _, raw := range runtimes {
		name := strings.TrimSpace(raw)
		if !slices.Contains(knownConduitDeliverRuntimes, name) {
			return nil, fmt.Errorf("unknown runtime %q (valid values: %s)", raw, strings.Join(knownConduitDeliverRuntimes, ", "))
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// ResolveConduitDeliverRuntimes returns the effective runtime list for a
// broker: DefaultConduitDeliverRuntimes when runtimes is nil (unset), or the
// validated and deduplicated slice otherwise (preserving an explicit empty
// slice).
func ResolveConduitDeliverRuntimes(runtimes []string) ([]string, error) {
	if runtimes == nil {
		return DefaultConduitDeliverRuntimes(), nil
	}
	return NormalizeConduitDeliverRuntimes(runtimes)
}
