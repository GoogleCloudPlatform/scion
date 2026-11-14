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
)

// reprovisionGatheredEnv carries into the start that follows a home-reset
// reprovision the names of the as_needed env keys that reprovision's
// env-gather resolved (ptone/scion#7757). It holds names only, never values.
//
// A start does not resolve as_needed env (selectDeliveryEnv skips it); only a
// create, provision or reprovision gather does. A hub-dispatched agent still
// restarts with such values (for example the Vertex AI project and location)
// because the broker restores the auth secrets its creating start staged, but
// only for the same harness-config. After a harness switch that record does
// not match, so the new harness's first start would have none of the values
// the reprovision gather just resolved for it.
//
// With a holder, the start admits the as_needed env vars and environment
// secrets stored under those names, through the same per-item delivery
// authorization as all other launch material (selectDeliveryEnvWith,
// selectDeliverySecretsWith), and fills only keys nothing else set. The
// broker's record of that start keeps the values for later restarts, as
// after a create.
type reprovisionGatheredEnv struct {
	// keys maps each stored key name the start may admit to the key the
	// broker asked for: a canonical key maps to itself, an alternative name
	// to its canonical key (as resolveAsNeededForKeys stores values).
	keys map[string]string
}

type reprovisionGatheredEnvKey struct{}

// withReprovisionGatheredEnv returns ctx with an empty holder. The reincarnate
// worker installs it for a home-reset reprovision and passes the same ctx to
// that reprovision and its start.
func withReprovisionGatheredEnv(ctx context.Context) context.Context {
	return context.WithValue(ctx, reprovisionGatheredEnvKey{}, &reprovisionGatheredEnv{})
}

func reprovisionGatheredEnvFrom(ctx context.Context) *reprovisionGatheredEnv {
	h, _ := ctx.Value(reprovisionGatheredEnvKey{}).(*reprovisionGatheredEnv)
	return h
}

// recordReprovisionGatheredEnv records in ctx's holder, if it has one, the
// names of resolved (the as_needed env a gather resolved, keyed by canonical
// key), each with the alternative names the broker accepts for it. Values are
// not kept.
func recordReprovisionGatheredEnv(ctx context.Context, resolved map[string]string, alternatives map[string][]string) {
	h := reprovisionGatheredEnvFrom(ctx)
	if h == nil || len(resolved) == 0 {
		return
	}
	h.keys = make(map[string]string, len(resolved))
	for canonical := range resolved {
		h.keys[canonical] = canonical
	}
	for canonical := range resolved {
		for _, alt := range alternatives[canonical] {
			if _, taken := h.keys[alt]; !taken {
				h.keys[alt] = canonical
			}
		}
	}
}

// canonicalKey returns the key the broker asked for when name is one of the
// recorded names. A nil holder admits nothing.
func (h *reprovisionGatheredEnv) canonicalKey(name string) (canonical string, ok bool) {
	if h == nil {
		return "", false
	}
	canonical, ok = h.keys[name]
	return canonical, ok
}
