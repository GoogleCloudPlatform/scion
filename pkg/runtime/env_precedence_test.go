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

package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

type precedenceTestHarness struct {
	harness.Generic
	env       map[string]string
	telemetry map[string]string
}

func (h *precedenceTestHarness) GetEnv(agentName, agentHome, unixUsername string) map[string]string {
	out := h.Generic.GetEnv(agentName, agentHome, unixUsername)
	for k, v := range h.env {
		out[k] = v
	}
	return out
}

func (h *precedenceTestHarness) GetTelemetryEnv() map[string]string {
	return h.telemetry
}

// TestHookWithheldMarker_MatchesActualRuntimeEmission verifies that across
// every runtime, the SCION_HOOK_WITHHELD_ENV marker emitted by the runtime's
// real container env assembly agrees with the actual winning value emitted for
// every key across all env sources (cfg.Env HC, cfg.Env non-HC,
// Harness.GetEnv, Harness.GetTelemetryEnv, ResolvedAuth, ResolvedSecrets,
// FallbackEnv). Because withholding is recorded at the exact emission site,
// any reordering or bypass immediately breaks this invariant.
func TestHookWithheldMarker_MatchesActualRuntimeEmission(t *testing.T) {
	const (
		valCfgHC    = "from-config-hc"
		valCfgNonHC = "from-config-non-hc"
		valTpl      = "from-harness-template"
		valTelem    = "from-harness-telemetry"
		valAuth     = "from-resolved-auth"
		valSec      = "from-resolved-secret"
		valFallback = "from-fallback"
	)

	makeCfg := func(rtName string) RunConfig {
		return RunConfig{
			Harness: &precedenceTestHarness{
				env: map[string]string{
					"K_CFGHC_TPL":          valTpl,
					"K_CFGNONHC_TPL":       valTpl,
					"K_TPL_AUTH":           valTpl,
					"K_TPL_SEC":            valTpl,
					"K_TPL_TELEM":          valTpl,
					"K_TPL_FB":             valTpl,
					"GOOGLE_CLOUD_PROJECT": valTpl,
					"TPL_ONLY":             valTpl,
					"TPL_EMPTY":            "",
				},
				telemetry: map[string]string{
					"K_TPL_TELEM": valTelem,
				},
			},
			Name:             "prec-agent",
			UnixUsername:     "scion",
			Image:            "scion-agent:latest",
			RuntimeName:      rtName,
			TelemetryEnabled: true,
			Labels:           map[string]string{"agent_id": "agent-prec", "scion.name": "prec-agent"},
			Env: []string{
				"K_CFGHC_TPL=" + valCfgHC,
				"K_CFGHC_AUTH=" + valCfgHC,
				"K_CFGHC_SEC=" + valCfgHC,
				"K_CFGHC_FB=" + valCfgHC,
				"CFGHC_ONLY=" + valCfgHC,
				"K_CFGNONHC_TPL=" + valCfgNonHC,
				"CFGNONHC_ONLY=" + valCfgNonHC,
			},
			HookWithheldEnv: []string{
				"K_CFGHC_TPL",
				"K_CFGHC_AUTH",
				"K_CFGHC_SEC",
				"K_CFGHC_FB",
				"CFGHC_ONLY",
			},
			ResolvedAuth: &api.ResolvedAuth{
				Method: "api-key",
				EnvVars: map[string]string{
					"K_CFGHC_AUTH":         valAuth,
					"K_TPL_AUTH":           valAuth,
					"GOOGLE_CLOUD_PROJECT": valAuth,
				},
			},
			ResolvedSecrets: []api.ResolvedSecret{
				{Type: "environment", Name: "sec-cfghc", Target: "K_CFGHC_SEC", Value: valSec},
				{Type: "environment", Name: "sec-tpl", Target: "K_TPL_SEC", Value: valSec},
			},
			FallbackEnv: map[string]string{
				"K_CFGHC_FB":    valFallback,
				"K_TPL_FB":      valFallback,
				"FALLBACK_ONLY": valFallback,
			},
		}
	}

	// emitActual invokes each runtime's real production env assembly and
	// returns both the final effective container env map and the parsed
	// SCION_HOOK_WITHHELD_ENV marker slice.
	emitActual := func(t *testing.T, rtName string, cfg RunConfig) (map[string]string, []string) {
		t.Helper()
		out := make(map[string]string)
		switch rtName {
		case "docker", "podman", "container":
			args, err := buildCommonRunArgs(cfg)
			if err != nil {
				t.Fatalf("buildCommonRunArgs(%s): %v", rtName, err)
			}
			if rtName == "container" {
				args = stripUnsupportedAppleFlags(args[3:])
			}
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-e" {
					if k, v, ok := strings.Cut(args[i+1], "="); ok {
						out[k] = v
					}
					i++
				}
			}
		case "kubernetes":
			pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			for _, c := range pod.Spec.Containers {
				if c.Name != agentContainerName {
					continue
				}
				for _, ev := range c.Env {
					if ev.Value != "" {
						out[ev.Name] = ev.Value
					} else if ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef != nil {
						out[ev.Name] = valSec
					}
				}
			}
		case "cloudrun":
			rt, err := NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"})
			if err != nil {
				t.Fatal(err)
			}
			_, resolvedAuthStaged, _, err := foldCloudRunSecretsAndAuth(&cfg, "", nil)
			if err != nil {
				t.Fatalf("foldCloudRunSecretsAndAuth: %v", err)
			}
			inst, err := rt.buildCloudRunInstance(cfg, 1000, 1000, nil)
			if err != nil {
				t.Fatalf("buildCloudRunInstance: %v", err)
			}
			for _, ev := range inst.Containers[0].Env {
				out[ev.GetName()] = ev.GetValue()
			}
			for k, v := range resolvedAuthStaged {
				if _, exists := out[k]; !exists {
					out[k] = v
				}
			}
		case "cloudrun-sandbox":
			secretKeys, err := applyResolvedSecretsToEnv(&cfg, sandboxEnvLimit, sandboxRuntimeEnvKeys...)
			if err != nil {
				t.Fatalf("applyResolvedSecretsToEnv: %v", err)
			}
			env, marker := assembleSandboxEnv(cfg, scionPaths{agentHome: "/tmp/home", workspace: "/tmp/ws"}, secretKeys)
			out = env
			if marker != "" {
				out[envpolicy.HookWithheldEnvVar] = marker
			}
		case "substrate":
			out = buildBootstrapEnv(cfg)
		default:
			t.Fatalf("unhandled runtime %q", rtName)
		}
		return out, envpolicy.ParseWithheldMarker(out[envpolicy.HookWithheldEnvVar])
	}

	runtimes := []string{"docker", "podman", "container", "kubernetes", "cloudrun", "cloudrun-sandbox", "substrate"}
	keys := []string{
		"K_CFGHC_TPL",
		"K_CFGHC_AUTH",
		"K_CFGHC_SEC",
		"K_CFGHC_FB",
		"CFGHC_ONLY",
		"K_CFGNONHC_TPL",
		"CFGNONHC_ONLY",
		"K_TPL_AUTH",
		"K_TPL_SEC",
		"K_TPL_TELEM",
		"K_TPL_FB",
		"GOOGLE_CLOUD_PROJECT",
		"TPL_ONLY",
		"FALLBACK_ONLY",
	}

	for _, rtName := range runtimes {
		t.Run(rtName, func(t *testing.T) {
			cfg := makeCfg(rtName)
			actual, withheld := emitActual(t, rtName, cfg)

			if slices.Contains(withheld, "SCION_AGENT_NAME") {
				t.Errorf("runtime %s: SCION_AGENT_NAME must never be withheld; marker=%v", rtName, withheld)
			}
			// On runtimes that skip empty GetEnv values, TPL_EMPTY is not emitted
			// and must not be withheld. (cloudrun-sandbox emits empty GetEnv values
			// verbatim, so it withholds TPL_EMPTY when present.)
			if _, emitted := actual["TPL_EMPTY"]; !emitted && slices.Contains(withheld, "TPL_EMPTY") {
				t.Errorf("runtime %s: unemitted TPL_EMPTY should not be withheld; marker=%v", rtName, withheld)
			}

			for _, k := range keys {
				gotVal := actual[k]
				wantWithheld := gotVal == valCfgHC || gotVal == valTpl
				gotWithheld := slices.Contains(withheld, k)
				if gotWithheld != wantWithheld {
					t.Errorf("runtime %s key %s: actual emitted=%q, withheld=%t, want withheld=%t (marker=%v)",
						rtName, k, gotVal, gotWithheld, wantWithheld, withheld)
				}
			}
		})
	}
}

// TestRuntimeEnvEmission_WithheldMarkerAlwaysFromTracker is a structural guard
// verifying that no non-test file in pkg/runtime formats cfg.HookWithheldEnv
// directly into SCION_HOOK_WITHHELD_ENV, bypassing hookWithheldTracker.
func TestRuntimeEnvEmission_WithheldMarkerAlwaysFromTracker(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	fset := token.NewFileSet()
	markerFiles := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// env_precedence.go owns hookWithheldTracker itself; common.go also
		// references HookWithheldEnvVar in reservedAgentEnvName.
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		var emitsMarker, usesTracker, formatsRawField bool
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "FormatWithheldMarker" && name != "env_precedence.go" {
					formatsRawField = true
				}
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "newHookWithheldTracker" {
				usesTracker = true
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "HookWithheldEnvVar" {
				emitsMarker = true
			}
			return true
		})
		if formatsRawField {
			t.Errorf("%s calls FormatWithheldMarker directly; route marker emission through hookWithheldTracker", name)
		}
		if emitsMarker && name != "env_precedence.go" && !usesTracker {
			t.Errorf("%s references HookWithheldEnvVar without constructing hookWithheldTracker", name)
		}
		if usesTracker && name != "env_precedence.go" {
			markerFiles++
		}
	}
	if markerFiles < 5 {
		t.Errorf("expected at least 5 runtime files using hookWithheldTracker (common, k8s, cloudrun, cloudrun-sandbox, substrate), found %d", markerFiles)
	}
}
