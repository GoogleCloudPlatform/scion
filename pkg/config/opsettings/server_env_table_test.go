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

package opsettings

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestServerEnvTableCoversRegistry: the env mapping table
// (pkg/config/server_env_table.go) is built from the settings schema; this
// keeps it complete against the registry. Every registry KoanfPath and
// every Layer-0 leaf the contract covers has a table entry, every table
// entry outside server is owned by a registry section, and the documented
// exclusions are neither in the table nor registry keys.
func TestServerEnvTableCoversRegistry(t *testing.T) {
	keys := envContractKeys(t, collectSchemaNodes(t))
	table := map[string]config.ServerEnvEntry{}
	for _, e := range config.ServerEnvEntries() {
		table[e.Path] = e
	}
	for _, k := range keys {
		if envContractStructuredKeys[k.Path] {
			continue
		}
		if _, ok := table[k.Path]; !ok {
			t.Errorf("%s: no env mapping table entry; add the key to the settings schema "+
				"(or to serverEnvExtraKeys) and, outside server, to serverEnvTopLevelKeys", k.Path)
		}
	}
	for path := range table {
		if strings.HasPrefix(path, "server.") {
			continue
		}
		if OwningSection(path) == "" && !isLayer0Key(path) {
			t.Errorf("%s: a top-level env mapping table entry no registry section owns; "+
				"remove its root from serverEnvTopLevelKeys", path)
		}
	}
	for k := range config.ServerEnvExcludedKeys {
		if _, ok := table[k]; ok {
			t.Errorf("%s is in ServerEnvExcludedKeys but has a table entry", k)
		}
		if IsLayer1Key(k) || isLayer0Key(k) {
			t.Errorf("%s is in ServerEnvExcludedKeys but the registry owns it; give it a table entry", k)
		}
	}
}

// TestContractFileHasNoBuildConstraint: the env contract tests run in
// make ci (go test -tags no_sqlite ./...), so their files must carry no
// build constraint that would leave them out.
func TestContractFileHasNoBuildConstraint(t *testing.T) {
	for _, f := range []string{"server_env_contract_test.go", "server_env_table_test.go"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "package ") {
				break
			}
			if strings.HasPrefix(line, "//go:build") || strings.HasPrefix(line, "// +build") {
				t.Errorf("%s has a build constraint (%s); the contract must run in make ci", f, line)
			}
		}
	}
}

// seedAliasName is the SCION_SEED_ form of a SCION_SERVER_ alias of key.
func seedAliasName(key envContractKey, alias string) string {
	rest := strings.TrimPrefix(alias, "SCION_SERVER_")
	if strings.HasPrefix(key.Path, "server.") {
		return "SCION_SEED_SERVER_" + rest
	}
	return "SCION_SEED_" + rest
}

// aliasReach reports whether the legacy alias name reaches key through
// consumer, as check does for the canonical name (without the empty-value
// checks, which do not depend on the spelling).
func (h *envContractHarness) aliasReach(t *testing.T, consumer string, key envContractKey, name string) (bool, string) {
	t.Helper()
	switch consumer {
	case consumerOpsettings, consumerSeed:
		load := func() koanfGetter { return config.LoadEnvKoanf() }
		if consumer == consumerSeed {
			name, load = seedAliasName(key, name), func() koanfGetter { return config.LoadSeedEnvKoanf() }
		}
		v := pickValue(key, reflect.Value{})
		reached, _ := koanfReach(t, name, key.Path, v, load)
		return reached, fmt.Sprintf("%s=%q does not land on %s", name, v, key.Path)
	case consumerGlobalConfig:
		leafOf, gcPath := globalConfigLeaf(key)
		for _, m := range h.gcModes {
			base, _ := leafOf(h.gcBaseline[m.name])
			v := pickValue(key, base)
			t.Setenv("HOME", m.home)
			t.Setenv(name, v)
			setEnvContext(t, key)
			gc, err := config.LoadGlobalConfig(h.configDir)
			_ = os.Unsetenv(name)
			for ctx := range envContractEnvContext[key.Path] {
				_ = os.Unsetenv(ctx)
			}
			if err != nil {
				return false, fmt.Sprintf("[%s] LoadGlobalConfig with %s=%q: %v", m.name, name, v, err)
			}
			if got, ok := leafOf(gc); !ok || !leafEquals(got, v) {
				return false, fmt.Sprintf("[%s] %s=%q: GlobalConfig %s = %s", m.name, name, v, gcPath, leafString(got, ok))
			}
		}
		return true, ""
	case consumerVersioned:
		return h.versionedReach(t, key, name)
	}
	return false, "unknown consumer " + consumer
}

// TestServerEnvAliasesReach: every legacy alias of a contract key reaches
// the key through every consumer the canonical name does
// (SCION_SERVER_HUB_ADMIN_EMAILS reaches server.hub.admin_emails wherever
// SCION_SERVER_HUB_ADMINEMAILS does).
func TestServerEnvAliasesReach(t *testing.T) {
	clearSettingsEnv(t)
	keys := envContractKeys(t, collectSchemaNodes(t))
	entries := map[string]config.ServerEnvEntry{}
	for _, e := range config.ServerEnvEntries() {
		entries[e.Path] = e
	}
	h := newEnvContractHarness(t)
	checked := 0
	for _, k := range keys {
		e, ok := entries[k.Path]
		if envContractStructuredKeys[k.Path] || !ok {
			continue // TestServerEnvTableCoversRegistry reports a missing entry
		}
		for _, alias := range e.Aliases {
			checked++
			t.Run(k.Path+"/"+alias, func(t *testing.T) {
				for _, consumer := range envContractConsumers {
					if !consumerApplies(consumer, k) {
						continue
					}
					if reached, detail := h.aliasReach(t, consumer, k, alias); !reached {
						t.Errorf("%s: alias %s does not reach the key through %s: %s", k.Path, alias, consumer, detail)
					}
				}
			})
		}
	}
	if checked < 100 {
		t.Fatalf("checked only %d aliases; the alias derivation is broken", checked)
	}
}

// TestDefaultModelFromEnv: SCION_SEED_DEFAULT_MODEL (an alias) and
// SCION_SEED_DEFAULTMODEL seed default_model, and SCION_SERVER_DEFAULTMODEL
// reaches the bootstrap merge and is reported as an env override (the admin
// page's env badge source).
func TestDefaultModelFromEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"SCION_SEED_DEFAULT_MODEL", "SCION_SEED_DEFAULTMODEL"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "gemini-envtest")
			if got := config.LoadSeedEnvKoanf().String("default_model"); got != "gemini-envtest" {
				t.Errorf("seed layer default_model = %q", got)
			}
			if got := config.LoadBootstrapKoanf().String("default_model"); got != "gemini-envtest" {
				t.Errorf("bootstrap default_model = %q", got)
			}
		})
	}
	t.Setenv("SCION_SERVER_DEFAULTMODEL", "claude-envtest")
	if got := config.LoadBootstrapKoanf().String("default_model"); got != "claude-envtest" {
		t.Errorf("bootstrap default_model = %q, want the SCION_SERVER_ value over the seed", got)
	}
	found := false
	for _, k := range DetectEnvOverrides(config.LoadEnvKoanf()) {
		found = found || k == "default_model"
	}
	if !found {
		t.Error("default_model is not reported as an env override")
	}
}

// TestBooleanFalseFromEnvValidates: SCION_SERVER_<bool>=false reaches the
// bootstrap section as the boolean false, so the section validates and the
// setting is off.
func TestBooleanFalseFromEnvValidates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_SERVER_HUB_AUTOSUSPENDSTALLED", "false")
	t.Setenv("SCION_SERVER_TELEMETRY_ENABLED", "false")
	t.Setenv("SCION_SERVER_DEFAULTMAXTURNS", "12")
	k := config.LoadBootstrapKoanf()
	for _, sec := range []string{"lifecycle", "telemetry", "agent_defaults"} {
		doc, err := ExtractSectionFromKoanf(k, sec)
		if err != nil {
			t.Fatalf("%s: %v", sec, err)
		}
		if errs := Validate(sec, doc); len(errs) > 0 {
			t.Errorf("%s: %s does not validate: %v", sec, doc, errs)
		}
	}
	var lc LifecycleSettings
	doc, _ := ExtractSectionFromKoanf(k, "lifecycle")
	if err := json.Unmarshal(doc, &lc); err != nil {
		t.Fatalf("decode lifecycle: %v", err)
	}
	if lc.AutoSuspendStalled == nil || *lc.AutoSuspendStalled {
		t.Errorf("auto_suspend_stalled = %v, want false", lc.AutoSuspendStalled)
	}
}
