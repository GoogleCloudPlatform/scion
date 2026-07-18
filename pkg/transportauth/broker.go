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

package transportauth

import (
	"fmt"
	"os"
)

// ResolveBrokerTransport resolves transport auth config for a broker connection
// using a two-level precedence: env vars override credentials-file values.
// Returns (nil, 0, nil) when no transport config is present.
//
// Unlike FromEnv(), this function never checks SCION_TRANSPORT_TOKEN (injected
// mode) — brokers always use MetadataSource to mint their own tokens from the
// runtime identity (GKE Workload Identity / ambient SA).
func ResolveBrokerTransport(transportMode, transportAudience string) (TokenSource, HeaderMode, error) {
	mode := os.Getenv(EnvTransportMode)
	audience := os.Getenv(EnvTransportAudience)

	if mode == "" {
		mode = transportMode
	}
	if audience == "" {
		audience = transportAudience
	}

	if mode == "" && audience == "" {
		return nil, 0, nil
	}

	if audience == "" {
		return nil, 0, fmt.Errorf("transport audience is required when transport auth is enabled")
	}

	src := NewMetadataSource(audience)
	headerMode := ModeFromString(mode)
	return src, headerMode, nil
}
