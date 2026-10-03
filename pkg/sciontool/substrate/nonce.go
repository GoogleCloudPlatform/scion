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

package substrate

import "crypto/subtle"

// NonceVerifier authenticates the bearer token presented to
// POST /scion/v1/bootstrap. substrate-runtime.md §5.2 documents two options:
//
//   - the actor's identity, minted by the broker via ateapi's MintActorJWT
//     (if a verifiable identity is available through the systemInfo volume);
//   - the fallback, where the server accepts only the first
//     bootstrap request and correctness instead relies on a NetworkPolicy
//     restricting router ingress to the broker namespace.
//
// This interface lets either plug in without changing the HTTP handler.
// Server currently defaults to FirstBootstrapWinsVerifier (the
// fallback).
type NonceVerifier interface {
	// VerifyNonce reports whether token is accepted as the bootstrap nonce.
	// It is called once, before the single-shot bootstrap check.
	VerifyNonce(token string) bool
}

// FirstBootstrapWinsVerifier implements the substrate-runtime.md §5.2 fallback: it
// accepts any bearer token, including an empty one. The server's own
// single-shot bootstrap enforcement (accept only the first call) is the
// only thing standing between an in-cluster caller and bootstrap, so this
// verifier must only be used behind a NetworkPolicy that restricts router
// ingress to the broker namespace — it is not a substitute for one.
type FirstBootstrapWinsVerifier struct{}

// VerifyNonce always returns true. See the type doc comment for the
// security assumption this depends on.
func (FirstBootstrapWinsVerifier) VerifyNonce(string) bool { return true }

// StaticNonceVerifier compares the presented bearer token against a fixed
// expected value in constant time. It stands in for the MintActorJWT-derived
// nonce (substrate-runtime.md §5.2) — once that mechanism is wired up, its
// caller resolves the actor's expected nonce and constructs this verifier
// (or a JWT-verifying one) instead of FirstBootstrapWinsVerifier.
type StaticNonceVerifier struct {
	Expected string
}

// VerifyNonce reports whether token matches the expected nonce. An empty
// Expected or token never matches.
func (v StaticNonceVerifier) VerifyNonce(token string) bool {
	if token == "" || v.Expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(v.Expected)) == 1
}
