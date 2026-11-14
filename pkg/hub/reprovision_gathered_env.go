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
	"maps"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// reprovisionGatheredEnv carries the as_needed env a home-reset reprovision's
// env-gather resolved into the start that follows it, in memory only
// (ptone/scion#7757).
//
// A start does not resolve as_needed env (selectDeliveryEnv skips it); only a
// create, provision or reprovision gather does. A hub-dispatched agent still
// restarts with such values (for example the Vertex AI project and location)
// because the broker restores the auth secrets its creating start staged, but
// only for the same harness-config. After a harness switch that record does
// not match, so the new harness's first start would have none of the values
// the reprovision gather just resolved for it.
//
// The values are never logged or persisted: the start passes them to the
// broker as secret-fetchable env, and the broker's own record of that start
// keeps them for later restarts, as after a create.
type reprovisionGatheredEnv struct {
	env map[string]string
}

type reprovisionGatheredEnvKey struct{}

// withReprovisionGatheredEnv returns ctx with an empty holder. The reincarnate
// worker installs it for a home-reset reprovision and passes the same ctx to
// the start, so that start receives what the reprovision gather resolved.
func withReprovisionGatheredEnv(ctx context.Context) context.Context {
	return context.WithValue(ctx, reprovisionGatheredEnvKey{}, &reprovisionGatheredEnv{})
}

func reprovisionGatheredEnvFrom(ctx context.Context) *reprovisionGatheredEnv {
	h, _ := ctx.Value(reprovisionGatheredEnvKey{}).(*reprovisionGatheredEnv)
	return h
}

// recordReprovisionGatheredEnv stores env in ctx's holder, if it has one.
func recordReprovisionGatheredEnv(ctx context.Context, env map[string]string) {
	if h := reprovisionGatheredEnvFrom(ctx); h != nil && len(env) > 0 {
		h.env = maps.Clone(env)
	}
}

// mergeReprovisionGatheredEnv fills keys of resolvedEnv that are absent or
// empty from ctx's holder, classified secret-fetchable like any as_needed
// value. It never replaces a value the start resolved itself.
func mergeReprovisionGatheredEnv(ctx context.Context, resolvedEnv map[string]string, classifications *map[string]api.EnvKind) {
	h := reprovisionGatheredEnvFrom(ctx)
	if h == nil {
		return
	}
	for k, v := range h.env {
		if v == "" {
			continue
		}
		if existing, ok := resolvedEnv[k]; ok && existing != "" {
			continue
		}
		resolvedEnv[k] = v
		classifyEnv(classifications, k, api.EnvKindSecretFetchable)
	}
}
