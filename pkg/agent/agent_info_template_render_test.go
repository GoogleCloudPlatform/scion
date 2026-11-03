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

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GetAgent (the existing-agent path used to render an agent again) uses the
// template named in a legacy agent's agent-info.json only when it is a
// template slug: a relative path, an absolute directory or a remote source
// never selects the template whose config is merged. Ignoring it logs a
// warning that names the agent and does not echo the value.
func TestGetAgentIgnoresNonSlugAgentInfoTemplate(t *testing.T) {
	const outsideModel = "outside-template-model"
	env := newHashTemplateEnv(t)
	outside := filepath.Join(t.TempDir(), "outside-template")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "scion-agent.json"), []byte(`{"model":"`+outsideModel+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Join(env.scionDir, "templates"), outside)
	if err != nil {
		t.Fatal(err)
	}
	record := func(template string) string {
		b, _ := json.Marshal(map[string]string{"name": env.agentName, "template": template})
		return string(b)
	}

	var logs bytes.Buffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLog) })
	const warning = "not a template name"

	// Control: a template slug from agent-info.json is used, without a warning.
	env.writeInfo(t, record("web-dev"))
	_, _, _, cfg, err := GetAgent(context.Background(), env.agentName, "", "", "", env.scionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("control: GetAgent: %v", err)
	}
	if cfg.Model != localWebDevModel {
		t.Fatalf("control: cfg.Model = %q, want %q", cfg.Model, localWebDevModel)
	}
	if strings.Contains(logs.String(), warning) {
		t.Fatalf("control: a template slug logged a warning: %s", logs.String())
	}

	for _, v := range []string{rel, outside, ":local:" + outside} {
		logs.Reset()
		env.writeInfo(t, record(v))
		_, _, _, cfg, err := GetAgent(context.Background(), env.agentName, "", "", "", env.scionDir, "", "", "", "")
		if err == nil && cfg != nil && cfg.Model == outsideModel {
			t.Fatalf("agent-info.json template %q selected a template outside the template directories", v)
		}
		out := logs.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, warning) || !strings.Contains(out, "agent="+env.agentName) {
			t.Errorf("agent-info.json template %q: no warning naming the agent: %s", v, out)
		}
		if strings.Contains(out, v) {
			t.Errorf("the warning echoes the agent-info.json template %q: %s", v, out)
		}
	}
}
