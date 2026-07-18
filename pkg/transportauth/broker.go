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

// ADCSourceConstructor is a function that creates a TokenSource from ADC.
// It is injected by the caller (e.g. from the adcsource subpackage) to avoid
// linking the Google API client libraries into the lean sciontool binary.
type ADCSourceConstructor func(audience string) (TokenSource, error)

// ResolveBrokerTransport resolves transport auth config for a broker connection
// using a two-level precedence: env vars override credentials-file values.
// Returns (nil, 0, nil) when no transport config is present.
//
// Unlike FromEnv(), this function never checks SCION_TRANSPORT_TOKEN (injected
// mode) — brokers always use MetadataSource to mint their own tokens from the
// runtime identity (GKE Workload Identity / ambient SA), falling back to ADC
// when not on GCE (off-GCP brokers with SA keys).
func ResolveBrokerTransport(transportMode, transportAudience string, adcNew ADCSourceConstructor) (TokenSource, HeaderMode, error) {
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

	headerMode := ModeFromString(mode)

	// Prefer the metadata server when on GCE.
	if IsOnGCEFunc() {
		if metaMode := os.Getenv(EnvMetadataMode); metaMode == "" {
			return NewMetadataSource(audience), headerMode, nil
		}
	}

	// Fall back to ADC for off-GCP brokers with SA keys.
	if adcNew != nil {
		src, err := adcNew(audience)
		if err != nil {
			return nil, 0, fmt.Errorf("broker transport: %w", err)
		}
		return src, headerMode, nil
	}

	return nil, 0, nil
}
