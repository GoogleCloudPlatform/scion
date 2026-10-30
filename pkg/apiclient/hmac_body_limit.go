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

package apiclient

import (
	"errors"
	"net/http"
)

// MaxSignedRequestBodyBytes is the largest request body an HMAC verifier
// accepts. The verifier hashes the body before it checks the signature, so
// the body must be bounded before that read.
//
// The value is one uniform cap for the hub verifier and both broker
// middlewares. No HMAC-verified route carries file bytes (workspace
// upload/apply pass a storage path; attachments, templates and harness
// configs are fetched with GETs). The largest legitimate bodies are agent
// creates (inline config, resolved env and secrets, each secret value at most
// 128 KiB) and broker heartbeats. A create tunnelled over the control channel
// is already bounded by its 1 MiB message limit; 2 MiB leaves headroom for a
// create sent to a broker over direct HTTP.
const MaxSignedRequestBodyBytes int64 = 2 << 20

// ErrSignedBodyTooLarge reports a signed request whose declared body length
// exceeds MaxSignedRequestBodyBytes.
var ErrSignedBodyTooLarge = errors.New("signed request body too large")

// SignedBodyTooLargeMessage is the client-facing message for a request
// refused with ErrSignedBodyTooLarge.
const SignedBodyTooLargeMessage = "signed request body exceeds the 2 MiB limit"

// LimitSignedRequestBody bounds r's body for a server-side HMAC verifier. It
// must run before anything reads the body (BuildCanonicalString hashes it).
//
// A declared Content-Length over the cap returns ErrSignedBodyTooLarge
// without reading a byte; the caller answers 413. Otherwise a non-nil body is
// wrapped in http.MaxBytesReader, which also covers an unknown length
// (ContentLength < 0) and a body longer than it declared: a read past the cap
// fails, the body hash is not computed, and the signature does not verify.
// w may be nil when the verifier has no response writer.
//
// Only the server-side verifiers call this. The client signer must not: a
// legitimate signed body is hashed in full on the sending side.
func LimitSignedRequestBody(w http.ResponseWriter, r *http.Request) error {
	if r.ContentLength > MaxSignedRequestBodyBytes {
		return ErrSignedBodyTooLarge
	}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, MaxSignedRequestBodyBytes)
	}
	return nil
}
