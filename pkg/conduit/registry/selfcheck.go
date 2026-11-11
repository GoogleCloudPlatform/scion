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

package registry

import (
	"context"
	"fmt"
)

// ProbeFunc dials a relay's internal endpoint and returns the instance_id of
// the relay that answered. It is supplied by the caller (1d owns the
// internal relay API).
type ProbeFunc func(ctx context.Context, url string) (instanceID string, err error)

// SelfCheck verifies owner addressability at relay startup (design §3.4,
// §3.9): it dials the relay's own internalEndpoint via probe and checks the
// answering instance is selfInstanceID. It returns ErrUnaddressable for an
// empty endpoint, ErrSelfCheckMismatch (wrapped, naming both IDs) if
// another instance answered, and the wrapped probe error otherwise. In
// hosted-HA mode any error is fatal and the relay must not register.
func SelfCheck(ctx context.Context, internalEndpoint, selfInstanceID string, probe ProbeFunc) error {
	if internalEndpoint == "" {
		return ErrUnaddressable
	}
	if selfInstanceID == "" {
		return fmt.Errorf("%w: empty self instance_id", ErrInvalidInput)
	}
	if probe == nil {
		return fmt.Errorf("%w: nil probe", ErrInvalidInput)
	}
	got, err := probe(ctx, internalEndpoint)
	if err != nil {
		return fmt.Errorf("conduit registry: self-check probe of %s failed: %w", internalEndpoint, err)
	}
	if got != selfInstanceID {
		return fmt.Errorf("%w: %s answered by %q, want %q", ErrSelfCheckMismatch, internalEndpoint, got, selfInstanceID)
	}
	return nil
}
