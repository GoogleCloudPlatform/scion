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
	"context"
	"strings"
	"testing"
)

const flatCRHubURL = "https://hub.example.com"

// TestFlatCloudRunHubEndpoint_NotAgentInfluenced: for a flat cloudrun
// instance, the request-level HubEndpoint, the connection endpoint, the
// resolved env and project settings cannot change the agent's hub URL: it
// is always the endpoint the host validated at activation, and trusted.
func TestFlatCloudRunHubEndpoint_NotAgentInfluenced(t *testing.T) {
	for _, op := range []startOperation{opCreate, opHTTPStart, opHTTPRestart} {
		got, trusted, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
			Op:                    op,
			ReqHubEndpoint:        "https://other.example.net",
			ConnectionHubEndpoint: "https://conn.example.net",
			BrokerHubEndpoint:     "http://localhost:8080",
			ResolvedEnv:           map[string]string{"SCION_HUB_ENDPOINT": "https://env.example.net", "SCION_HUB_URL": "https://env2.example.net"},
			ProjectPath:           t.TempDir(),
			ContainerHubEndpoint:  flatCRHubURL,
			RuntimeName:           "cloudrun",
			FlatCloudRun:          true,
		})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if got != flatCRHubURL || !trusted {
			t.Errorf("%s: endpoint = %q (trusted %v), want %q, trusted", op, got, trusted, flatCRHubURL)
		}
	}
	// Control: without the flat flag the request-level endpoint wins, so the
	// assertion above is about the flat branch, not the inputs.
	got, _, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
		Op: opCreate, ReqHubEndpoint: "https://other.example.net", RuntimeName: "docker",
	})
	if err != nil || got != "https://other.example.net" {
		t.Fatalf("non-flat control = %q, %v", got, err)
	}
}

// TestHubEnv_FlatCloudRunNeverDerivesKService: a flat cloudrun instance
// whose endpoint is loopback (or missing, or not https) is refused at
// start; the K_SERVICE derivation of the legacy runtime never runs.
func TestHubEnv_FlatCloudRunNeverDerivesKService(t *testing.T) {
	t.Setenv("K_SERVICE", "hub-service")
	for _, ep := range []string{"", "http://localhost:8080", "https://127.0.0.1", "http://hub.example.com"} {
		got, _, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
			Op:                   opCreate,
			BrokerHubEndpoint:    "http://localhost:8080",
			ContainerHubEndpoint: ep,
			RuntimeName:          "cloudrun",
			FlatCloudRun:         true,
		})
		if err == nil {
			t.Errorf("endpoint %q: got %q, want a refusal", ep, got)
		}
	}
	if _, _, err := resolveEffectiveHubEndpoint(context.Background(), hubEndpointInputs{
		Op: "bogus", ContainerHubEndpoint: flatCRHubURL, RuntimeName: "cloudrun", FlatCloudRun: true,
	}); err == nil {
		t.Error("an unknown operation must be refused on the flat branch too")
	}
}

// TestValidateFlatCloudRunHubEndpoint: the activation rules (P5) for the
// agent-facing hub URL.
func TestValidateFlatCloudRunHubEndpoint(t *testing.T) {
	for _, ok := range []string{flatCRHubURL, "https://hub.example.com:8443/base", "HTTPS://Hub.Example.com", "https://203.0.113.10"} {
		if err := ValidateFlatCloudRunHubEndpoint(ok); err != nil {
			t.Errorf("%q: %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"hub.example.com",                   // not absolute
		"/hub",                              // not absolute
		"http://hub.example.com",            // plain http
		"ftp://hub.example.com",             // other scheme
		"https://localhost",                 // loopback name
		"https://api.localhost",             // loopback name
		"https://127.0.0.1",                 // loopback IPv4
		"https://[::1]:443",                 // loopback IPv6
		"https://0.0.0.0",                   // unspecified
		"https://169.254.169.254",           // link-local IPv4
		"https://[fe80::1]",                 // link-local IPv6
		"https://user:pw@hub.example.com",   // user information
		"https://",                          // no host
		"https://[fe80::1%25eth0]",          // link-local IPv6 with a zone
		"https://[2001:db8::1%25eth0]:8443", // any zone
	} {
		if err := ValidateFlatCloudRunHubEndpoint(bad); err == nil {
			t.Errorf("%q accepted, want refused", bad)
		}
	}
}

// TestBuildStartContext_FlatCloudRunTargetGetsActivationEndpoint: on a
// server whose flat instance targets cloudrun (flatCloudRunTarget), the
// start context gives the agent the endpoint the host validated at
// activation (ContainerHubEndpoint), never the request's, and an endpoint
// that fails the activation rules refuses the start.
func TestBuildStartContext_FlatCloudRunTargetGetsActivationEndpoint(t *testing.T) {
	t.Run("validated endpoint", func(t *testing.T) {
		e := newE10Server(t)
		e.seedLiveRecord(t)
		e.srv.config.ContainerHubEndpoint = flatCRHubURL
		in := e10Inputs(opHTTPStart, nil, map[string]string{}, false)
		in.HubEndpoint = "https://other.example.net"
		sc, err := e.srv.buildStartContext(context.Background(), in)
		if err != nil {
			t.Fatalf("buildStartContext: %v", err)
		}
		if got := sc.Opts.Env["SCION_HUB_ENDPOINT"]; got != flatCRHubURL {
			t.Errorf("SCION_HUB_ENDPOINT = %q, want %q", got, flatCRHubURL)
		}
		if sc.Opts.TrustedHubEndpoint != flatCRHubURL {
			t.Errorf("TrustedHubEndpoint = %q, want %q", sc.Opts.TrustedHubEndpoint, flatCRHubURL)
		}
	})
	t.Run("endpoint failing the activation rules", func(t *testing.T) {
		e := newE10Server(t)
		e.seedLiveRecord(t)
		e.srv.config.ContainerHubEndpoint = "http://127.0.0.1:8080"
		in := e10Inputs(opHTTPStart, nil, map[string]string{}, false)
		in.HubEndpoint = flatCRHubURL
		_, err := e.srv.buildStartContext(context.Background(), in)
		if err == nil || !strings.Contains(err.Error(), "hub endpoint") {
			t.Fatalf("buildStartContext error = %v, want the hub endpoint refusal", err)
		}
		if n := *e.runs; n != 0 {
			t.Errorf("runs = %d, want 0", n)
		}
	})
}
