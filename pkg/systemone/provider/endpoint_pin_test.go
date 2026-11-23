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

package provider_test

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubNoDefaultAdapter struct{}

func (stubNoDefaultAdapter) Name() string    { return "no-default" }
func (stubNoDefaultAdapter) Version() string { return "0.1.0" }
func (stubNoDefaultAdapter) Capabilities(provider.InstanceView) (provider.Capabilities, error) {
	return provider.Capabilities{SideEffectFree: true, AuthScheme: provider.AuthBearer}, nil
}
func (stubNoDefaultAdapter) Validate(provider.InstanceView, provider.RuleSpec) error { return nil }
func (stubNoDefaultAdapter) Invoke(context.Context, provider.Transport, provider.InstanceView, provider.ModelRequest) (provider.ModelResult, error) {
	return provider.ModelResult{}, nil
}

type stubDefaultEndpointAdapter struct {
	stubNoDefaultAdapter
	def string
}

func (a stubDefaultEndpointAdapter) DefaultEndpoint() string { return a.def }

func TestCanonicalOrigin_AndMatchesAdapterDefaultEndpoint_F9Table(t *testing.T) {
	const wantOrigin = "https://generativelanguage.googleapis.com"
	gemAdapter := stubDefaultEndpointAdapter{def: wantOrigin}

	accepted := []struct {
		name string
		in   string
	}{
		{name: "omitted_empty", in: ""},
		{name: "omitted_whitespace", in: "   "},
		{name: "canonical_exact", in: "https://generativelanguage.googleapis.com"},
		{name: "trailing_slash", in: "https://generativelanguage.googleapis.com/"},
		{name: "explicit_port_443", in: "https://generativelanguage.googleapis.com:443"},
		{name: "mixed_case_scheme_host_port_443_slash", in: "HTTPS://Generativelanguage.googleapis.com:443/"},
		{name: "surrounding_spaces", in: "  https://generativelanguage.googleapis.com:443/  "},
	}
	for _, tc := range accepted {
		t.Run("accept_"+tc.name, func(t *testing.T) {
			got, ok := provider.MatchesAdapterDefaultEndpoint(gemAdapter, tc.in)
			require.True(t, ok, "expected %q to match canonical default", tc.in)
			assert.Equal(t, wantOrigin, got)
		})
	}

	rejected := []struct {
		name string
		in   string
	}{
		{name: "http_scheme", in: "http://generativelanguage.googleapis.com"},
		{name: "ws_scheme", in: "wss://generativelanguage.googleapis.com"},
		{name: "custom_port_8443", in: "https://generativelanguage.googleapis.com:8443"},
		{name: "leading_zero_port_0443", in: "https://generativelanguage.googleapis.com:0443"},
		{name: "empty_port_colon", in: "https://generativelanguage.googleapis.com:"},
		{name: "path_override_v1beta", in: "https://generativelanguage.googleapis.com/v1beta"},
		{name: "double_slash_path", in: "https://generativelanguage.googleapis.com//"},
		{name: "query_param", in: "https://generativelanguage.googleapis.com?key=x"},
		{name: "bare_question_mark", in: "https://generativelanguage.googleapis.com?"},
		{name: "fragment", in: "https://generativelanguage.googleapis.com#sec"},
		{name: "bare_hash", in: "https://generativelanguage.googleapis.com#"},
		{name: "userinfo", in: "https://user:pw@generativelanguage.googleapis.com"},
		{name: "trailing_dot", in: "https://generativelanguage.googleapis.com."},
		{name: "leading_dot", in: "https://.generativelanguage.googleapis.com"},
		{name: "ipv4_literal", in: "https://142.250.80.42"},
		{name: "loopback_ipv4_port", in: "https://127.0.0.1:443"},
		{name: "ipv6_literal", in: "https://[::1]"},
		{name: "decimal_ip_literal", in: "https://2130706433"},
		{name: "punycode_xn_root", in: "https://xn--generativelanguage-123.googleapis.com"},
		{name: "punycode_xn_sublabel", in: "https://generativelanguage.xn--googleapis-99a.com"},
		{name: "unicode_host", in: "https://gënerativelanguage.googleapis.com"},
		{name: "percent_encoded_host", in: "https://generativelanguage%2egoogleapis.com"},
		{name: "attacker_suffix_spoof", in: "https://generativelanguage.googleapis.com.attacker.example"},
		{name: "attacker_prefix_spoof", in: "https://generativelanguage.googleapis.com-attacker.example"},
		{name: "subdomain_spoof", in: "https://sub.generativelanguage.googleapis.com"},
		{name: "different_host", in: "https://attacker.example"},
		{name: "backslash_url", in: `https://generativelanguage.googleapis.com\@attacker.example`},
	}
	for _, tc := range rejected {
		t.Run("reject_"+tc.name, func(t *testing.T) {
			got, ok := provider.MatchesAdapterDefaultEndpoint(gemAdapter, tc.in)
			assert.False(t, ok, "expected %q to be rejected, got %q", tc.in, got)
			assert.Empty(t, got)
		})
	}

	t.Run("adapter_without_EndpointDefaulter_or_empty_default_always_rejected", func(t *testing.T) {
		_, ok1 := provider.MatchesAdapterDefaultEndpoint(stubNoDefaultAdapter{}, "")
		assert.False(t, ok1)
		_, ok2 := provider.MatchesAdapterDefaultEndpoint(stubNoDefaultAdapter{}, wantOrigin)
		assert.False(t, ok2)
		_, ok3 := provider.MatchesAdapterDefaultEndpoint(stubDefaultEndpointAdapter{def: ""}, "")
		assert.False(t, ok3)
		_, ok4 := provider.MatchesAdapterDefaultEndpoint(nil, "")
		assert.False(t, ok4)
	})
}

type stubPolicyAdapter struct {
	stubNoDefaultAdapter
	names []string
}

func (a stubPolicyAdapter) AllowedSecretNames() []string { return a.names }

func TestAllowedSecretNamesFor_ExactPolicy(t *testing.T) {
	assert.Nil(t, provider.AllowedSecretNamesFor(nil))
	assert.Nil(t, provider.AllowedSecretNamesFor(stubNoDefaultAdapter{}))
	assert.Nil(t, provider.AllowedSecretNamesFor(stubPolicyAdapter{names: nil}))
	assert.Nil(t, provider.AllowedSecretNamesFor(stubPolicyAdapter{names: []string{"1invalid", "systemone.instance.x"}}))
	assert.Equal(t, []string{"GEMINI_API_KEY"}, provider.AllowedSecretNamesFor(stubPolicyAdapter{
		names: []string{"GEMINI_API_KEY", "GEMINI_API_KEY", "1invalid"},
	}))
}
