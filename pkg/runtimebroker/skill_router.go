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
	"github.com/GoogleCloudPlatform/scion/pkg/agent"
)

// buildSkillRouter assembles the scheme routing table the broker installs for
// agent skill provisioning.
//
// Routing policy:
//
//   - skill:// and bare names go to the Hub (the router's primary fallback).
//   - gh:// is registered with RegisterFallback, not Register. That routes
//     gh:// URIs through the Hub first, so the Hub's DB-backed cache absorbs
//     repeated resolutions of the same skill and mints credentials centrally,
//     while the local GitHub resolver stays wired as the backstop for Hub
//     transport errors and per-URI failures.
//   - gcp-skill:// goes straight to the GCP resolver; the Hub has no cache for
//     it, so there is nothing to gain from routing it through the Hub.
//
// Extracted from provisioning so the routing policy — in particular the
// Hub-first behaviour of gh:// — is assertable in isolation.
func buildSkillRouter(hub, gh, gcp agent.SkillResolver) *agent.RoutingSkillResolver {
	router := agent.NewRoutingSkillResolver(hub)
	router.RegisterFallback("gh", gh)
	router.Register("gcp-skill", gcp)
	return router
}
