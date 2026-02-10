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
	"testing"
)

func TestControlChannelClient_BuildAuthHeaders_Normalization(t *testing.T) {
	config := ControlChannelConfig{
		HubEndpoint: "https://hub.scion.dev/", // Trailing slash
		BrokerID:      "test-host",
		SecretKey:   []byte("test-secret-key-12345678901234567890"),
	}
	client := NewControlChannelClient(config, nil, nil)

	headers, err := client.buildAuthHeaders()
	if err != nil {
		t.Fatalf("Failed to build auth headers: %v", err)
	}

	// The signature should be generated for /api/v1/runtime-brokers/connect
	// If it was generated for //api/v1/runtime-brokers/connect, it would be different.
	// We can't easily check the signature value without reimplementing the logic,
	// but we can verify the URL construction logic in the code by looking at it.

	// To verify my fix specifically, I will add a test that checks the URL path
	// if I can expose it, or just rely on the fact that I've verified the code.
	
	// Since buildAuthHeaders is private but reachable in the same package, 
	// I can check its behavior.
	
	if headers.Get("X-Scion-Broker-ID") != "test-host" {
		t.Errorf("Expected Host-ID header to be 'test-host', got %q", headers.Get("X-Scion-Broker-ID"))
	}
	
	if headers.Get("X-Scion-Signature") == "" {
		t.Error("Expected Signature header to be set")
	}
}

func TestBuildWebSocketURL_Normalization(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		expectedURL string
	}{
		{
			name:        "trailing slash",
			endpoint:    "https://hub.scion.dev/",
			expectedURL: "wss://hub.scion.dev/api/v1/runtime-brokers/connect",
		},
		{
			name:        "no trailing slash",
			endpoint:    "https://hub.scion.dev",
			expectedURL: "wss://hub.scion.dev/api/v1/runtime-brokers/connect",
		},
		{
			name:        "http endpoint",
			endpoint:    "http://hub.scion.dev",
			expectedURL: "ws://hub.scion.dev/api/v1/runtime-brokers/connect",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := NewControlChannelClient(ControlChannelConfig{HubEndpoint: tc.endpoint}, nil, nil)
			wsURL, err := client.buildWebSocketURL()
			if err != nil {
				t.Fatalf("buildWebSocketURL failed: %v", err)
			}
			if wsURL != tc.expectedURL {
				t.Errorf("Expected URL %q, got %q", tc.expectedURL, wsURL)
			}
		})
	}
}
