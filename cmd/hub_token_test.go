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

package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestHubTokenCreateHelpUsesRegistryScopes(t *testing.T) {
	help := hubTokenCreateCmd.Long
	if !strings.Contains(help, permissions.UATScopeHelp()) {
		t.Fatal("hub token create help must include registry-derived UAT scope help")
	}
	for _, required := range []string{store.UATScopeProjectUpdate, store.UATScopeAgentPortAccess} {
		if !strings.Contains(help, required) {
			t.Fatalf("hub token create help missing valid scope %q", required)
		}
	}
	for _, stale := range []string{
		store.UATScopeAgentStart,
		store.UATScopeAgentStop,
		store.UATScopeAgentDispatch,
	} {
		if strings.Contains(help, stale) {
			t.Fatalf("hub token create help exposes stale scope %q", stale)
		}
	}
}

func TestParseExpiry_Days(t *testing.T) {
	before := time.Now().UTC()
	result, err := parseExpiry("30d")
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedMin := before.Add(30 * 24 * time.Hour)
	expectedMax := after.Add(30 * 24 * time.Hour)
	if result.Before(expectedMin) || result.After(expectedMax) {
		t.Errorf("expected time around %v, got %v", expectedMin, result)
	}
}

func TestParseExpiry_Years(t *testing.T) {
	before := time.Now().UTC()
	result, err := parseExpiry("1y")
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedMin := before.AddDate(1, 0, 0)
	expectedMax := after.AddDate(1, 0, 0)
	if result.Before(expectedMin) || result.After(expectedMax) {
		t.Errorf("expected time around %v, got %v", expectedMin, result)
	}
}

func TestParseExpiry_RFC3339(t *testing.T) {
	result, err := parseExpiry("2026-12-31T00:00:00Z")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	if !result.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, result)
	}
}

func TestParseExpiry_Invalid(t *testing.T) {
	tests := []string{
		"",
		"x",
		"30",
		"abc",
		"-5d",
		"0d",
		"30h",
	}

	for _, input := range tests {
		_, err := parseExpiry(input)
		if err == nil {
			t.Errorf("expected error for input %q, got nil", input)
		}
	}
}

// TestParseLabelFlags is the F12 regression test (review-2 finding 2(b)):
// parseLabelFlags rejects a repeated --label key instead of silently
// keeping the last value.
func TestParseLabelFlags(t *testing.T) {
	cases := []struct {
		name    string
		input   []string
		want    map[string]string
		wantErr bool
	}{
		{name: "nil input yields nil", input: nil, want: nil},
		{name: "empty slice yields nil", input: []string{}, want: nil},
		{name: "single key=value", input: []string{"k=v"}, want: map[string]string{"k": "v"}},
		{name: "empty value allowed", input: []string{"k="}, want: map[string]string{"k": ""}},
		{name: "missing = is an error", input: []string{"k"}, wantErr: true},
		{name: "repeated key is an error", input: []string{"k=a", "k=b"}, wantErr: true},
		{name: "distinct keys both kept", input: []string{"k=a", "j=b"}, want: map[string]string{"k": "a", "j": "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLabelFlags(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for input %v, got labels %v", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %v: %v", tc.input, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestParseExpiry_90Days(t *testing.T) {
	result, err := parseExpiry("90d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := time.Now().UTC().Add(90 * 24 * time.Hour)
	diff := result.Sub(expected)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expected time close to %v, got %v (diff: %v)", expected, result, diff)
	}
}
