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

package trigger

import (
	"fmt"
	"net/http"
	"time"
)

// ScionV1Verifier verifies scion_v1 HMAC-SHA256 webhook deliveries
// (Stage 1 §8.1a, Stage 2 §7.2, §7.3).
type ScionV1Verifier struct {
	Now          time.Time
	ReplayWindow time.Duration
}

// Verify checks X-Scion-Signature against each candidate secret in secrets,
// then validates the signed X-Scion-* routing header bounds so unsigned
// callers always receive ErrInvalidSignature (401) rather than 400.
func (v *ScionV1Verifier) Verify(r *http.Request, body []byte, secrets []string) (VerifiedHeaders, error) {
	if r == nil || len(secrets) == 0 {
		return VerifiedHeaders{}, ErrInvalidSignature
	}
	sigVals := r.Header.Values(HeaderSignature)
	if len(sigVals) != 1 {
		return VerifiedHeaders{}, ErrInvalidSignature
	}
	delivery := r.Header.Get(HeaderDelivery)
	eventHdr := r.Header.Get(HeaderEvent)
	subjectHdr := r.Header.Get(HeaderSubject)

	now := v.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	matched := false
	for _, secret := range secrets {
		if VerifyScionV1SignatureWithWindow(secret, now, v.ReplayWindow, sigVals[0], delivery, eventHdr, subjectHdr, body) {
			matched = true
			break
		}
	}
	if !matched {
		return VerifiedHeaders{}, ErrInvalidSignature
	}

	if err := validateSignedRoutingHeaders(r.Header, delivery, eventHdr, subjectHdr); err != nil {
		return VerifiedHeaders{}, fmt.Errorf("%w: %s", ErrInvalidHeader, err.Error())
	}

	return VerifiedHeaders{
		Delivery:       delivery,
		Event:          eventHdr,
		Subject:        subjectHdr,
		SignedDelivery: delivery != "",
		SignedEvent:    eventHdr != "",
		SignedSubject:  subjectHdr != "",
	}, nil
}
