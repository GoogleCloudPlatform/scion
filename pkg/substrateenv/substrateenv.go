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

// Package substrateenv is the single source of truth for the names of the
// env vars a Substrate actor's container carries when
// V1SubstrateConfig.EgressTrustBundle is set, pointing every supported TLS
// client at the projected egress-gateway trust bundle.
//
// Two independent consumers must never drift apart:
//   - pkg/runtime's buildActorTemplate sets exactly these vars (to the
//     bundle file, except SSL_CERT_DIR which gets the bundle's mount
//     directory) on the actor's container Env;
//   - pkg/sciontool/substrate's execAsUserCmd passes exactly the subset of
//     these names that are actually set to `su -w`, so exec-invoked
//     commands don't lose the bundle to `su -`'s login-shell environment
//     reset.
//
// A name added to one list but not the other is exactly the defect class
// this package exists to prevent: an exec-invoked TLS client would keep
// silently losing the bundle even though the harness trusts it fine. This
// package has no dependencies beyond the standard library (none at all,
// in fact), so both pkg/runtime (broker-side, heavy k8s/grpc dependencies)
// and pkg/sciontool/substrate (the agent-side sciontool binary, which must
// never import pkg/config — see execuser.go's own doc comment) can import
// it without pulling in the other's dependencies or creating an import
// cycle.
package substrateenv

// TrustBundleVarNames lists every env var name buildActorTemplate points at
// the projected egress-gateway trust bundle, in the fixed order
// buildActorTemplate emits them (and su -w's whitelist, when built,
// preserves). Every name's value is the bundle file itself
// (substrateTrustBundleFile in pkg/runtime) except SSL_CERT_DIR, whose
// value is the bundle's mount directory (substrateTrustBundleMountPath) —
// both consumers already know this from their own context, so this
// package owns only the names, which is the only part that has to be
// shared.
var TrustBundleVarNames = []string{
	"NODE_EXTRA_CA_CERTS",
	"GIT_SSL_CAINFO",
	"SSL_CERT_FILE",
	"CURL_CA_BUNDLE",
	"SSL_CERT_DIR",
}
