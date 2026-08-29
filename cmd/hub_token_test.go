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
