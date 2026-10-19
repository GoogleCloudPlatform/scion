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

package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// imageRegistryLayerOrder is the precedence order the resolver must apply.
var imageRegistryLayerOrder = []ImageRegistrySource{
	ImageRegistrySourceNodeEnv,
	ImageRegistrySourceHubDBProfile,
	ImageRegistrySourceHubDB,
	ImageRegistrySourceFileProfile,
	ImageRegistrySourceFile,
}

// layersFromMask sets layer i (in imageRegistryLayerOrder) to "v<i>" when
// bit i of mask is set.
func layersFromMask(mask int) ImageRegistryLayers {
	v := func(i int) string {
		if mask&(1<<i) != 0 {
			return "v" + strconv.Itoa(i)
		}
		return ""
	}
	return ImageRegistryLayers{NodeEnv: v(0), HubDBProfile: v(1), HubDB: v(2), FileProfile: v(3), File: v(4)}
}

// TestResolveImageRegistryLayers_AllCombinations checks every set/unset
// combination of the five layers: the highest set layer wins.
func TestResolveImageRegistryLayers_AllCombinations(t *testing.T) {
	for mask := 0; mask < 1<<len(imageRegistryLayerOrder); mask++ {
		got := ResolveImageRegistryLayers(layersFromMask(mask))
		want := ResolvedImageRegistry{Source: ImageRegistrySourceDefault}
		for i, src := range imageRegistryLayerOrder {
			if mask&(1<<i) != 0 {
				want = ResolvedImageRegistry{Value: "v" + strconv.Itoa(i), Source: src}
				break
			}
		}
		if got != want {
			t.Errorf("mask %05b: got %+v, want %+v", mask, got, want)
		}
	}
}

// TestResolveImageRegistryLayers_WhitespaceIsUnset checks that a blank
// layer does not win and that a value is returned trimmed.
func TestResolveImageRegistryLayers_WhitespaceIsUnset(t *testing.T) {
	got := ResolveImageRegistryLayers(ImageRegistryLayers{NodeEnv: "  ", HubDB: "\t", File: " f.example "})
	if got.Value != "f.example" || got.Source != ImageRegistrySourceFile {
		t.Fatalf("got %+v, want f.example from file", got)
	}
}

// TestResolveImageRegistryLayers_ScopeRows names the cross-source rows the
// per-source scope rule decides (ptone/scion#3916).
func TestResolveImageRegistryLayers_ScopeRows(t *testing.T) {
	tests := []struct {
		name   string
		layers ImageRegistryLayers
		want   ResolvedImageRegistry
	}{
		{"DB profile beats DB top", ImageRegistryLayers{HubDBProfile: "p", HubDB: "t"}, ResolvedImageRegistry{"p", ImageRegistrySourceHubDBProfile}},
		{"DB top beats file profile", ImageRegistryLayers{HubDB: "t", FileProfile: "fp"}, ResolvedImageRegistry{"t", ImageRegistrySourceHubDB}},
		{"DB top beats a flat instance value", ImageRegistryLayers{HubDB: "t", FileProfile: "inst", File: "f"}, ResolvedImageRegistry{"t", ImageRegistrySourceHubDB}},
		{"node env beats DB profile", ImageRegistryLayers{NodeEnv: "e", HubDBProfile: "p"}, ResolvedImageRegistry{"e", ImageRegistrySourceNodeEnv}},
		{"file profile beats file top", ImageRegistryLayers{FileProfile: "fp", File: "f"}, ResolvedImageRegistry{"fp", ImageRegistrySourceFileProfile}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveImageRegistryLayers(tt.layers); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestImageRegistryFromEnv(t *testing.T) {
	t.Setenv(EnvImageRegistry, " env.example/r ")
	if got := ImageRegistryFromEnv(); got != "env.example/r" {
		t.Fatalf("got %q", got)
	}
}

// writeHomeSettings points HOME at a temp dir with ~/.scion/settings.yaml
// holding content, and returns the .scion directory.
func writeHomeSettings(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestImageRegistryExcludedFromSettingsEnv checks that SCION_IMAGE_REGISTRY
// never folds into a settings load: it is the node env layer only.
func TestImageRegistryExcludedFromSettingsEnv(t *testing.T) {
	dir := writeHomeSettings(t, "schema_version: \"1\"\nimage_registry: file.example\n")
	t.Setenv(EnvImageRegistry, "env.example")
	vs, err := LoadVersionedSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if vs.ImageRegistry != "file.example" {
		t.Fatalf("ImageRegistry = %q, want the file value only", vs.ImageRegistry)
	}
	got := ResolveImageRegistryLayers(vs.ImageRegistryLayers("", ImageRegistryFromEnv()))
	if got.Value != "env.example" || got.Source != ImageRegistrySourceNodeEnv {
		t.Fatalf("resolved %+v, want env.example from node_env", got)
	}

	// With no file value, the env does not appear as one either.
	dir = writeHomeSettings(t, "schema_version: \"1\"\n")
	vs, err = LoadVersionedSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if vs.ImageRegistry != "" {
		t.Fatalf("ImageRegistry = %q, want empty", vs.ImageRegistry)
	}
}

// TestImageRegistryFileTier_Bootstrap checks the file tier of a hub
// replica: coded defaults < SCION_SEED_IMAGEREGISTRY < settings.yaml <
// SCION_SERVER_IMAGEREGISTRY, never SCION_IMAGE_REGISTRY or
// SCION_MAINTENANCE_IMAGE_REGISTRY, and profile values from the same merge.
func TestImageRegistryFileTier_Bootstrap(t *testing.T) {
	t.Setenv(EnvImageRegistry, "node.example")
	t.Setenv(EnvMaintenanceImageRegistry, "maint.example")

	t.Run("SERVER_ beats yaml", func(t *testing.T) {
		writeHomeSettings(t, "schema_version: \"1\"\nimage_registry: yaml.example\n")
		t.Setenv("SCION_SERVER_IMAGEREGISTRY", "server.example")
		if got := ImageRegistryFileTierFromKoanf(LoadBootstrapKoanf()).Top; got != "server.example" {
			t.Fatalf("Top = %q, want server.example", got)
		}
	})
	t.Run("yaml beats SEED_", func(t *testing.T) {
		writeHomeSettings(t, "schema_version: \"1\"\nimage_registry: yaml.example\n")
		t.Setenv("SCION_SEED_IMAGEREGISTRY", "seed.example")
		if got := ImageRegistryFileTierFromKoanf(LoadBootstrapKoanf()).Top; got != "yaml.example" {
			t.Fatalf("Top = %q, want yaml.example", got)
		}
	})
	t.Run("SEED_ alone", func(t *testing.T) {
		writeHomeSettings(t, "schema_version: \"1\"\n")
		t.Setenv("SCION_SEED_IMAGEREGISTRY", "seed.example")
		if got := ImageRegistryFileTierFromKoanf(LoadBootstrapKoanf()).Top; got != "seed.example" {
			t.Fatalf("Top = %q, want seed.example", got)
		}
	})
	t.Run("top is not profile-resolved; profiles come from the merge", func(t *testing.T) {
		writeHomeSettings(t, `schema_version: "1"
image_registry: top.example
active_profile: k8s
profiles:
  k8s:
    runtime: kubernetes
    image_registry: k8s.example
  local:
    runtime: docker
`)
		tier := ImageRegistryFileTierFromKoanf(LoadBootstrapKoanf())
		if tier.Top != "top.example" {
			t.Fatalf("Top = %q, want top.example (never the active profile's)", tier.Top)
		}
		if tier.Profiles["k8s"] != "k8s.example" || tier.Profiles["local"] != "" {
			t.Fatalf("Profiles = %v", tier.Profiles)
		}
	})
	t.Run("none", func(t *testing.T) {
		writeHomeSettings(t, "schema_version: \"1\"\n")
		if got := ImageRegistryFileTierFromKoanf(LoadBootstrapKoanf()).Top; got != "" {
			t.Fatalf("Top = %q, want empty (neither node nor maintenance env)", got)
		}
	})
}

// TestImageRegistryFileTier_ProfileValue pins the catalog rule: a profile
// in the catalog in effect takes its file value, also when a managed row
// holds its definition without a registry; a profile outside it (retained
// built-in, missing) has none.
func TestImageRegistryFileTier_ProfileValue(t *testing.T) {
	tier := ImageRegistryFileTier{Top: "t", Profiles: map[string]string{"k8s": "b"}}
	if got := tier.ProfileValue("k8s", []string{"k8s", "local"}); got != "b" {
		t.Errorf("in catalog: %q, want b", got)
	}
	if got := tier.ProfileValue("k8s", []string{"k8s"}); got != "b" {
		t.Errorf("managed row containing k8s without a key inherits the file value: got %q", got)
	}
	if got := tier.ProfileValue("k8s", []string{"other"}); got != "" {
		t.Errorf("retained built-in / outside the catalog: %q, want empty", got)
	}
	if got := tier.ProfileValue("", []string{"k8s"}); got != "" {
		t.Errorf("empty profile: %q", got)
	}
}

func TestEffectiveDefaultProfile(t *testing.T) {
	tests := []struct {
		configured string
		catalog    []string
		want       string
	}{
		{"a", []string{"a", "b"}, "a"},
		{"x", []string{"only"}, "only"},
		{"only", []string{"only"}, "only"},
		{"x", []string{"a", "b"}, "x"},
		{"x", nil, "x"},
		{"", []string{"only"}, "only"},
	}
	for _, tt := range tests {
		if got := EffectiveDefaultProfile(tt.configured, tt.catalog); got != tt.want {
			t.Errorf("EffectiveDefaultProfile(%q, %v) = %q, want %q", tt.configured, tt.catalog, got, tt.want)
		}
	}
}

// TestVersionedSettings_ImageRegistryLayers covers the builder for a
// remote broker or the CLI (no hub inputs), a co-located broker (hub
// inputs from the overlay) and a flat instance view.
func TestVersionedSettings_ImageRegistryLayers(t *testing.T) {
	file := func() *VersionedSettings {
		return &VersionedSettings{
			ImageRegistry: "file-top",
			ActiveProfile: "k8s",
			Profiles: map[string]V1ProfileConfig{
				"k8s":   {Runtime: "kubernetes", ImageRegistry: "file-k8s"},
				"local": {Runtime: "docker"},
			},
		}
	}

	t.Run("no hub inputs", func(t *testing.T) {
		got := file().ImageRegistryLayers("", "env")
		want := ImageRegistryLayers{NodeEnv: "env", FileProfile: "file-k8s", File: "file-top"}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("co-located hub inputs", func(t *testing.T) {
		vs := file()
		o := NewSettingsOverlay()
		o.UpdateImageRegistryInputs(ImageRegistryHubInputs{
			ManagedTop:      "db-top",
			ManagedProfiles: map[string]string{"local": "db-local"},
			FileTier:        ImageRegistryFileTier{Top: "boot-top", Profiles: map[string]string{"k8s": "boot-k8s"}},
			Catalog:         []string{"k8s", "local"},
		})
		o.Apply(vs)
		if got := vs.ImageRegistryLayers("k8s", ""); got != (ImageRegistryLayers{HubDB: "db-top", FileProfile: "boot-k8s", File: "boot-top"}) {
			t.Fatalf("k8s: %+v", got)
		}
		if got := ResolveImageRegistryLayers(vs.ImageRegistryLayers("k8s", "")); got != (ResolvedImageRegistry{"db-top", ImageRegistrySourceHubDB}) {
			t.Fatalf("k8s resolved %+v: a managed top beats a file profile", got)
		}
		if got := ResolveImageRegistryLayers(vs.ImageRegistryLayers("local", "")); got != (ResolvedImageRegistry{"db-local", ImageRegistrySourceHubDBProfile}) {
			t.Fatalf("local resolved %+v", got)
		}
		if got := ResolveImageRegistryLayers(vs.ImageRegistryLayers("local", "env")); got.Source != ImageRegistrySourceNodeEnv {
			t.Fatalf("node env must win: %+v", got)
		}
	})

	t.Run("flat view has no profile layers", func(t *testing.T) {
		vs := file()
		o := NewSettingsOverlay()
		o.UpdateImageRegistryInputs(ImageRegistryHubInputs{
			ManagedProfiles: map[string]string{"inst": "db-profile-same-name"},
			FileTier:        ImageRegistryFileTier{Top: "boot-top"},
			Catalog:         []string{"inst"},
		})
		o.Apply(vs)
		view := vs.ForFlatInstance(&FlatInstancePolicy{InstanceKey: "inst", Defaults: V1InstanceAgentDefaults{ImageRegistry: "instance-reg"}})
		got := view.ImageRegistryLayers("inst", "")
		if got != (ImageRegistryLayers{FileProfile: "instance-reg", File: "boot-top"}) {
			t.Fatalf("got %+v: a managed profile of the same name must not apply", got)
		}
		o.UpdateImageRegistryInputs(ImageRegistryHubInputs{ManagedTop: "db-top", FileTier: ImageRegistryFileTier{Top: "boot-top"}})
		vs = file()
		o.Apply(vs)
		view = vs.ForFlatInstance(&FlatInstancePolicy{InstanceKey: "inst", Defaults: V1InstanceAgentDefaults{ImageRegistry: "instance-reg"}})
		if got := ResolveImageRegistryLayers(view.ImageRegistryLayers("", "")); got != (ResolvedImageRegistry{"db-top", ImageRegistrySourceHubDB}) {
			t.Fatalf("resolved %+v: a managed top beats the instance value", got)
		}
	})
}

// TestSettingsOverlay_ImageRegistryInputs pins the overlay feed: it
// activates the overlay with no catalog section, it is not gated on the
// env, and an empty profile resolves through the settings' own live
// active profile.
func TestSettingsOverlay_ImageRegistryInputs(t *testing.T) {
	t.Setenv(EnvImageRegistry, "env.example")
	o := NewSettingsOverlay()
	o.UpdateImageRegistryInputs(ImageRegistryHubInputs{
		ManagedProfiles: map[string]string{"stale": "db-stale", "live": "db-live"},
		Catalog:         []string{"live", "stale"},
	})
	vs := &VersionedSettings{ActiveProfile: "live", Profiles: map[string]V1ProfileConfig{"live": {}, "stale": {}}}
	o.Apply(vs)
	if vs.imageRegistryHub == nil {
		t.Fatal("inputs not applied without a catalog section")
	}
	if got := vs.SettingsImageRegistry(""); got != "db-live" {
		t.Fatalf("empty profile resolved %q, want the live active profile's db-live", got)
	}
}

// TestImageRegistryEnvReadOnlyByHelper is the guard that only
// ImageRegistryFromEnv reads SCION_IMAGE_REGISTRY: no other non-test Go
// file under pkg/ or cmd/ passes that name (as a literal or as
// EnvImageRegistry) to os.Getenv or os.LookupEnv.
func TestImageRegistryEnvReadOnlyByHelper(t *testing.T) {
	root := filepath.Join("..", "..")
	var bad []string
	for _, sub := range []string{"pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, sub), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if ok && fn.Name.Name == "ImageRegistryFromEnv" && filepath.Base(path) == "image_registry.go" {
					return false
				}
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv") {
					return true
				}
				switch a := call.Args[0].(type) {
				case *ast.BasicLit:
					if a.Value == `"SCION_IMAGE_REGISTRY"` {
						bad = append(bad, path)
					}
				case *ast.Ident:
					if a.Name == "EnvImageRegistry" {
						bad = append(bad, path)
					}
				case *ast.SelectorExpr:
					if a.Sel.Name == "EnvImageRegistry" {
						bad = append(bad, path)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("SCION_IMAGE_REGISTRY read outside config.ImageRegistryFromEnv: %v", bad)
	}
}
