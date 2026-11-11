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

package hubsync

import (
	"errors"
	"strings"
	"testing"
)

// The prompts name the hub endpoint without its user info.
func TestShowProjectLinkPrompt_RedactsEndpoint(t *testing.T) {
	out := withPromptIO(t, strings.NewReader(""), false)
	ShowProjectLinkPrompt("demo", "https://tok3n@hub.example.com", true)
	if got := out.String(); !strings.Contains(got, "Hub endpoint: https://xxxxx@hub.example.com\n") || strings.Contains(got, "tok3n") {
		t.Errorf("project link prompt = %q, want the endpoint without user info", got)
	}
}

func TestShowBrokerRegistrationPrompt_RedactsEndpoint(t *testing.T) {
	out := withPromptIO(t, strings.NewReader(""), false)
	ShowBrokerRegistrationPrompt("https://alice:s3cret@hub.example.com", true)
	if got := out.String(); !strings.Contains(got, "Hub endpoint: https://alice:xxxxx@hub.example.com\n") || strings.Contains(got, "s3cret") {
		t.Errorf("broker registration prompt = %q, want the endpoint without user info", got)
	}
}

// A warning quoting a hub error shows the URL without its user info.
func TestWarnf_RedactsURLUserInfo(t *testing.T) {
	buf := captureWarnings(t)
	warnf("could not check for agents to import: %v", errors.New(`Get "https://tok3n@hub.example.com/api/v1/agents": connection refused`))
	got := buf.String()
	want := "Warning: could not check for agents to import: Get \"https://xxxxx@hub.example.com/api/v1/agents\": connection refused\n"
	if got != want {
		t.Errorf("warnf wrote %q, want %q", got, want)
	}
}

// The hub-not-responding error names the endpoint without user info.
func TestHubUnreachableError_RedactsEndpoint(t *testing.T) {
	got := hubUnreachableError("https://alice:pass word@hub.example.com", errors.New("connection refused")).Error()
	if !strings.Contains(got, "hub at https://xxxxx@hub.example.com is not responding") || strings.Contains(got, "pass") || strings.Contains(got, "word") {
		t.Errorf("error = %q, want the endpoint without user info", got)
	}
}
