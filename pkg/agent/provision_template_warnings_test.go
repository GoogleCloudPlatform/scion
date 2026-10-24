/*
Copyright 2026 The Scion Authors.
*/

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func warningsContaining(ws []string, sub string) []string {
	var out []string
	for _, w := range ws {
		if strings.Contains(w, sub) {
			out = append(out, w)
		}
	}
	return out
}

// The deprecated-field warnings cover only what the template chain sets: a
// harness-config image (which every harness-config has) or an inline value
// is not reported as set by the template.
func TestResolveTemplate_TemplateWarningsIgnoreHarnessConfig(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-img", policyTestDecl)
	e.template(t, "plain", "model: small\n")
	e.template(t, "concrete", "model: tpl-model\n")

	cases := []struct {
		name     string
		template string
		inline   *api.ScionConfig
		sub      string
		want     int
	}{
		{"harness-config image is not a template field", "plain", nil, "'image'", 0},
		{"inline image is not a template field", "plain", &api.ScionConfig{Image: "inline:1"}, "'image'", 0},
		{"inline model is not a template field", "plain", &api.ScionConfig{Model: "inline-model"}, "'model'", 0},
		{"template model is reported once", "concrete", nil, "'model'", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := resolveTemplateAndHarnessConfig(context.Background(), tc.template, "hc-img", e.scion, e.scion, "", nil, tc.inline)
			if err != nil {
				t.Fatalf("resolveTemplateAndHarnessConfig: %v", err)
			}
			if got := warningsContaining(rt.TemplateWarnings, tc.sub); len(got) != tc.want {
				t.Errorf("%s warnings = %v (all: %v), want %d", tc.sub, got, rt.TemplateWarnings, tc.want)
			}
		})
	}
}
