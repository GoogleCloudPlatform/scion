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

package messagepolicy_test

import (
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestPolicyPromptText_GoldenAndExclusions(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := messagepolicy.PolicyPromptText(nil, nil, nil); got != "" {
			t.Fatalf("PolicyPromptText(nil, nil, nil) = %q, want empty", got)
		}
	})

	t.Run("decisions_golden_and_exclusions", func(t *testing.T) {
		decisions := []messages.Decision{
			{
				Key:             "EXCLUDED_DECISION_KEY_1",
				Question:        "Which region should we deploy to?",
				DefaultChoiceID: "EXCLUDED_DEFAULT_CHOICE_ID",
				Choices: []messages.Choice{
					{
						ID:          "EXCLUDED_CHOICE_ID_1",
						Label:       "us-central1",
						Description: "Primary US region",
					},
					{
						ID:    "EXCLUDED_CHOICE_ID_2",
						Label: "europe-west1",
					},
				},
				Other: &messages.OtherOption{
					Label:       "Custom region",
					Description: "Enter any GCP region",
					Placeholder: "asia-east1",
				},
			},
			{
				Key:      "EXCLUDED_DECISION_KEY_2",
				Question: "Proceed now?",
				Choices: []messages.Choice{
					{ID: "EXCLUDED_YES_ID", Label: "Yes"},
				},
			},
		}

		want := strings.Join([]string{
			"Decision question: Which region should we deploy to?",
			"Choice: us-central1",
			"Choice description: Primary US region",
			"Choice: europe-west1",
			"Other option: Custom region",
			"Other description: Enter any GCP region",
			"Other placeholder: asia-east1",
			"Decision question: Proceed now?",
			"Choice: Yes",
		}, "\n")

		got := messagepolicy.PolicyPromptText(decisions, nil, nil)
		if got != want {
			t.Fatalf("PolicyPromptText mismatch:\ngot:\n%s\n\nwant:\n%s", got, want)
		}
		// Determinism check.
		if got2 := messagepolicy.PolicyPromptText(decisions, nil, nil); got2 != got {
			t.Fatalf("PolicyPromptText not deterministic: %q != %q", got2, got)
		}
		for _, excluded := range []string{
			"EXCLUDED_DECISION_KEY_1",
			"EXCLUDED_DECISION_KEY_2",
			"EXCLUDED_DEFAULT_CHOICE_ID",
			"EXCLUDED_CHOICE_ID_1",
			"EXCLUDED_CHOICE_ID_2",
			"EXCLUDED_YES_ID",
		} {
			if strings.Contains(got, excluded) {
				t.Errorf("PolicyPromptText leaked excluded token %q in:\n%s", excluded, got)
			}
		}
	})

	t.Run("secret_requests_golden_and_exclusions", func(t *testing.T) {
		reqs := []messages.SecretRequest{
			{
				Key:         "STRIPE_API_KEY",
				KeyNorm:     "STRIPE_API_KEY",
				Description: "Needed to run billing integration tests",
				SecretType:  messages.SecretRequestTypeFile,
				Target:      "~/.config/EXCLUDED_SECRET_TARGET",
			},
			{
				Key:        "GITHUB_TOKEN",
				KeyNorm:    "GITHUB_TOKEN",
				SecretType: messages.SecretRequestTypeEnvironment,
				Target:     "EXCLUDED_ENV_TARGET",
			},
		}

		want := strings.Join([]string{
			"Secret request key: STRIPE_API_KEY",
			"Secret request description: Needed to run billing integration tests",
			"Secret request key: GITHUB_TOKEN",
		}, "\n")

		got := messagepolicy.PolicyPromptText(nil, reqs, nil)
		if got != want {
			t.Fatalf("PolicyPromptText mismatch:\ngot:\n%s\n\nwant:\n%s", got, want)
		}
		for _, excluded := range []string{
			"EXCLUDED_SECRET_TARGET",
			"EXCLUDED_ENV_TARGET",
			messages.SecretRequestTypeFile,
			messages.SecretRequestTypeEnvironment,
		} {
			if strings.Contains(got, excluded) {
				t.Errorf("PolicyPromptText leaked excluded token %q in:\n%s", excluded, got)
			}
		}
	})

	t.Run("command_request_golden_and_exclusions", func(t *testing.T) {
		cmdReq := &messages.CommandRequest{
			Key:                  "EXCLUDED_CMD_KEY",
			KeyNorm:              "excluded_cmd_key",
			Description:          "Run database migration dry-run",
			Workdir:              "/workspace/db",
			Script:               "set -e\n./migrate --dry-run",
			TimeoutS:             987,
			ShareOutputRequested: true,
			CommandSHA256:        "EXCLUDED_CMD_SHA256",
		}

		want := strings.Join([]string{
			"Command description: Run database migration dry-run",
			"Command workdir: /workspace/db",
			"Command script:\nset -e\n./migrate --dry-run",
		}, "\n")

		got := messagepolicy.PolicyPromptText(nil, nil, cmdReq)
		if got != want {
			t.Fatalf("PolicyPromptText mismatch:\ngot:\n%s\n\nwant:\n%s", got, want)
		}
		for _, excluded := range []string{
			"EXCLUDED_CMD_KEY",
			"excluded_cmd_key",
			"987",
			"EXCLUDED_CMD_SHA256",
			"share_output",
		} {
			if strings.Contains(got, excluded) {
				t.Errorf("PolicyPromptText leaked excluded token %q in:\n%s", excluded, got)
			}
		}
	})

	t.Run("command_request_without_optional_fields", func(t *testing.T) {
		cmdReq := &messages.CommandRequest{
			Key:      "cmd-1",
			Script:   "echo ok",
			TimeoutS: 600,
		}
		want := "Command script:\necho ok"
		if got := messagepolicy.PolicyPromptText(nil, nil, cmdReq); got != want {
			t.Fatalf("PolicyPromptText = %q, want %q", got, want)
		}
	})
}
