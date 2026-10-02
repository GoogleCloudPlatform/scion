package runtimebroker

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// hubOnlyTZSettings declares TZ as an empty-valued env key in a harness_configs
// entry and in a profile harness override (both Phase 2 sources), and as a
// harness_configs secret (Phase 3). CUSTOM_REQUIRED_KEY is the control: an
// ordinary empty-valued key that must still be required.
const hubOnlyTZSettings = `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      TZ: ""
      CUSTOM_REQUIRED_KEY: ""
    secrets:
      - key: TZ
profiles:
  default:
    runtime: mock
    harness_overrides:
      claude:
        env:
          TZ: ""
`

// TestExtractRequiredEnvKeys_NeverRequiresTZ pins that TZ is never a required
// (gathered) key, whichever phase would otherwise list it: for a
// hub-dispatched agent only the hub supplies TZ, and an empty TZ means
// "unset", never "ask".
func TestExtractRequiredEnvKeys_NeverRequiresTZ(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nenv:\n  TZ: \"\"\n",
		hubOnlyTZSettings)

	var req CreateAgentRequest
	req.ProjectPath = projectDir
	req.Config = &CreateAgentConfig{HarnessConfig: "claude", Profile: "default"}
	req.RequiredSecrets = []api.RequiredSecret{{Key: "TZ"}}

	required, secretInfo, alternatives, _ := srv.extractRequiredEnvKeys(req, "")
	if slices.Contains(required, "TZ") {
		t.Errorf("TZ must never be a required env key; required=%v", required)
	}
	if _, ok := secretInfo["TZ"]; ok {
		t.Errorf("TZ must not be offered as a secret-eligible key; secretInfo=%v", secretInfo)
	}
	if _, ok := alternatives["TZ"]; ok {
		t.Errorf("TZ must not carry alternatives; alternatives=%v", alternatives)
	}
	if !slices.Contains(required, "CUSTOM_REQUIRED_KEY") {
		t.Errorf("control key CUSTOM_REQUIRED_KEY should still be required; required=%v", required)
	}
}

// TestEnvGather_EmptyTZDoesNotBlockCreate drives the env-gather path end to
// end: with TZ the only empty-valued key, the create proceeds (201) instead
// of asking the hub for TZ.
func TestEnvGather_EmptyTZDoesNotBlockCreate(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\n",
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      TZ: ""
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-empty-tz",
		"id": "agent-uuid-empty-tz",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {"ANTHROPIC_API_KEY": "sk-test"},
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (an empty TZ is never gathered), got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"TZ"`) {
		t.Errorf("response must not ask for TZ: %s", w.Body.String())
	}
}

// TestExtractRequiredEnvKeys_TZNeverAnAuthAlternative covers the alternatives
// map, which is filled only from harness-config auth key groups: an unmet
// any_of group led by TZ must not surface TZ as a required key or carry
// alternatives under it. CUSTOM_AUTH_KEY is the control group.
func TestExtractRequiredEnvKeys_TZNeverAnAuthAlternative(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		`harness: claude
image: test-image
user: scion
auth_selected_type: api-key
auth:
  default_type: api-key
  types:
    api-key:
      required_env:
        - any_of: ["TZ", "TZ_ALT"]
        - any_of: ["CUSTOM_AUTH_KEY", "CUSTOM_AUTH_ALT"]
`,
		`
schema_version: "1"
profiles:
  default:
    runtime: mock
`)

	var req CreateAgentRequest
	req.ProjectPath = projectDir
	req.Config = &CreateAgentConfig{HarnessConfig: "claude", Profile: "default"}

	required, _, alternatives, _ := srv.extractRequiredEnvKeys(req, "")
	if slices.Contains(required, "TZ") {
		t.Errorf("TZ must never be a required env key; required=%v", required)
	}
	if alts, ok := alternatives["TZ"]; ok {
		t.Errorf("TZ must not carry alternatives; alternatives[TZ]=%v", alts)
	}
	if !slices.Contains(required, "CUSTOM_AUTH_KEY") {
		t.Errorf("control: CUSTOM_AUTH_KEY should be required; required=%v", required)
	}
	if got := alternatives["CUSTOM_AUTH_KEY"]; !slices.Equal(got, []string{"CUSTOM_AUTH_ALT"}) {
		t.Errorf("control: alternatives[CUSTOM_AUTH_KEY] = %v, want [CUSTOM_AUTH_ALT]", got)
	}
}
