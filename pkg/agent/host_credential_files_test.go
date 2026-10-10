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
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// hostCredTestSetup returns a home dir holding an OAuth login file and an
// unnamed credential file, and auth metadata declaring both plus one file
// that does not exist.
func hostCredTestSetup(t *testing.T) (string, *config.HarnessAuthMetadata) {
	t.Helper()
	home := t.TempDir()
	for rel, content := range map[string]string{
		".claude/.credentials.json": `{"oauth":"host"}`,
		".unnamed/creds.json":       `{}`,
	} {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	meta := &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"auth-file": {RequiredFiles: []config.HarnessAuthFileRequirement{
				{Name: "claude-auth", Field: "ClaudeAuthFile", TargetSuffix: "/.claude/.credentials.json"},
				{Name: "missing", Field: "MissingFile", TargetSuffix: "/.missing/creds.json"},
				{Field: "UnnamedFile", TargetSuffix: "/.unnamed/creds.json"},
			}},
		},
	}
	return home, meta
}

func TestInjectHostCredentialFiles(t *testing.T) {
	home, meta := hostCredTestSetup(t)
	wantSecret := api.ResolvedSecret{
		Name:   "claude-auth",
		Type:   "file",
		Target: "~/.claude/.credentials.json",
		Value:  `{"oauth":"host"}`,
		Source: "runtime_broker",
	}
	other := api.ResolvedSecret{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "v"}

	t.Run("flag set appends existing named files", func(t *testing.T) {
		opts := &api.StartOptions{BrokerMode: true, HostCredentialFiles: true, ResolvedSecrets: []api.ResolvedSecret{other}}
		injected := injectHostCredentialFiles(opts, meta, home)
		if !reflect.DeepEqual(injected, []string{"claude-auth"}) {
			t.Fatalf("injected = %v", injected)
		}
		want := []api.ResolvedSecret{other, wantSecret}
		if !reflect.DeepEqual(opts.ResolvedSecrets, want) {
			t.Fatalf("ResolvedSecrets = %+v, want %+v", opts.ResolvedSecrets, want)
		}
	})

	t.Run("same-name hub secret wins", func(t *testing.T) {
		hub := api.ResolvedSecret{Name: "claude-auth", Type: "file", Target: "/somewhere/else.json", Value: "hub", Source: "user"}
		opts := &api.StartOptions{BrokerMode: true, HostCredentialFiles: true, ResolvedSecrets: []api.ResolvedSecret{hub}}
		if injected := injectHostCredentialFiles(opts, meta, home); injected != nil {
			t.Fatalf("expected nothing injected, got %v", injected)
		}
		if !reflect.DeepEqual(opts.ResolvedSecrets, []api.ResolvedSecret{hub}) {
			t.Fatalf("ResolvedSecrets changed: %+v", opts.ResolvedSecrets)
		}
	})

	for _, target := range []string{"~/.claude/.credentials.json", "/home/scion/.claude/.credentials.json"} {
		t.Run("same-target hub secret wins "+target, func(t *testing.T) {
			hub := api.ResolvedSecret{Name: "my-claude", Type: "file", Target: target, Value: "hub", Source: "user"}
			opts := &api.StartOptions{BrokerMode: true, HostCredentialFiles: true, ResolvedSecrets: []api.ResolvedSecret{hub}}
			if injected := injectHostCredentialFiles(opts, meta, home); injected != nil {
				t.Fatalf("expected nothing injected, got %v", injected)
			}
			if len(opts.ResolvedSecrets) != 1 {
				t.Fatalf("ResolvedSecrets changed: %+v", opts.ResolvedSecrets)
			}
		})
	}

	t.Run("flag unset injects nothing", func(t *testing.T) {
		opts := &api.StartOptions{BrokerMode: true}
		if injected := injectHostCredentialFiles(opts, meta, home); injected != nil || opts.ResolvedSecrets != nil {
			t.Fatalf("expected nothing, got %v / %+v", injected, opts.ResolvedSecrets)
		}
	})

	t.Run("non-broker mode injects nothing", func(t *testing.T) {
		opts := &api.StartOptions{HostCredentialFiles: true}
		if injected := injectHostCredentialFiles(opts, meta, home); injected != nil || opts.ResolvedSecrets != nil {
			t.Fatalf("expected nothing, got %v / %+v", injected, opts.ResolvedSecrets)
		}
	})

	t.Run("nil auth metadata injects nothing", func(t *testing.T) {
		opts := &api.StartOptions{BrokerMode: true, HostCredentialFiles: true}
		if injected := injectHostCredentialFiles(opts, nil, home); injected != nil || opts.ResolvedSecrets != nil {
			t.Fatalf("expected nothing, got %v / %+v", injected, opts.ResolvedSecrets)
		}
	})
}

func TestAppendHostCredentialFileSecrets_SuffixWithoutSlash(t *testing.T) {
	home, _ := hostCredTestSetup(t)
	meta := &config.HarnessAuthMetadata{Types: map[string]config.HarnessAuthTypeMetadata{
		"t": {RequiredFiles: []config.HarnessAuthFileRequirement{
			{Name: "claude-auth", Field: "ClaudeAuthFile", TargetSuffix: ".claude/.credentials.json"},
		}},
	}}
	opts := &api.StartOptions{BrokerMode: true, HostCredentialFiles: true}
	injectHostCredentialFiles(opts, meta, home)
	if len(opts.ResolvedSecrets) != 1 || opts.ResolvedSecrets[0].Target != "~/.claude/.credentials.json" {
		t.Fatalf("ResolvedSecrets = %+v", opts.ResolvedSecrets)
	}
}

// TestInjectHostCredentialFiles_SkipsGcloudADC: ADC stays behind the
// broker's auto_inject_gcloud_adc opt-in even when a harness config declares
// it as a required file with a target suffix (as grok-build does).
func TestInjectHostCredentialFiles_SkipsGcloudADC(t *testing.T) {
	home, _ := hostCredTestSetup(t)
	adc := filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
	if err := os.MkdirAll(filepath.Dir(adc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adc, []byte(`{"type":"authorized_user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := &config.HarnessAuthMetadata{Types: map[string]config.HarnessAuthTypeMetadata{
		"vertex-ai": {RequiredFiles: []config.HarnessAuthFileRequirement{
			{Name: "gcloud-adc", Type: "file", Field: "GoogleAppCredentials",
				TargetSuffix:                         "/.config/gcloud/application_default_credentials.json",
				SkippedWhenGCPServiceAccountAssigned: true},
			// Same file under another name and field: still ADC by target.
			{Name: "my-adc", Field: "OtherADC", TargetSuffix: "/.config/gcloud/application_default_credentials.json"},
		}},
		"oauth": {RequiredFiles: []config.HarnessAuthFileRequirement{
			{Name: "claude-auth", Field: "ClaudeAuthFile", TargetSuffix: "/.claude/.credentials.json"},
		}},
	}}
	opts := &api.StartOptions{BrokerMode: true, HostCredentialFiles: true}
	injected := injectHostCredentialFiles(opts, meta, home)
	if !reflect.DeepEqual(injected, []string{"claude-auth"}) {
		t.Fatalf("injected = %v, want only claude-auth", injected)
	}
	for _, s := range opts.ResolvedSecrets {
		if s.Name == "gcloud-adc" || s.Name == "my-adc" {
			t.Fatalf("ADC injected through host credentials: %+v", s)
		}
	}
}
