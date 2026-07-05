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

package resources

import (
	"fmt"
	"io/fs"

	harnesses "github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// BundledResource describes a built-in resource shipped with the binary.
type BundledResource struct {
	Kind      storage.ResourceKind
	Name      string
	Scope     string
	ScopeID   string
	SourceURL string
	FS        fs.FS
	Root      string
}

func bundledVersion() string {
	v := version.Version
	if v == "" {
		return "dev"
	}
	return v
}

func sourceURL(kind storage.ResourceKind, name string) string {
	return fmt.Sprintf("builtin://scion/%s/%s/%s", bundledVersion(), kind, name)
}

func harnessConfigSourceURL(name string) string {
	return fmt.Sprintf("git+https://github.com/GoogleCloudPlatform/scion/harnesses/%s", name)
}

// BuiltinTemplates returns the bundled template resources.
func BuiltinTemplates() []BundledResource {
	templateFS, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		panic(fmt.Sprintf("resources: sub templates FS: %v", err))
	}
	return []BundledResource{
		{
			Kind:      storage.ResourceKindTemplate,
			Name:      "default",
			Scope:     "global",
			ScopeID:   "",
			SourceURL: sourceURL(storage.ResourceKindTemplate, "default"),
			FS:        templateFS,
			Root:      "default",
		},
	}
}

// BuiltinHarnessConfigs returns the bundled harness-config resources.
func BuiltinHarnessConfigs() []BundledResource {
	var configs []BundledResource
	entries, err := fs.ReadDir(harnesses.FS, ".")
	if err != nil {
		panic(fmt.Sprintf("resources: read harnesses FS: %v", err))
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		sub, err := fs.Sub(harnesses.FS, name)
		if err != nil {
			panic(fmt.Sprintf("resources: sub harness FS %q: %v", name, err))
		}
		configs = append(configs, BundledResource{
			Kind:      storage.ResourceKindHarnessConfig,
			Name:      name,
			Scope:     "global",
			ScopeID:   "",
			SourceURL: harnessConfigSourceURL(name),
			FS:        sub,
			Root:      ".",
		})
	}
	return configs
}

// BuiltinResources returns all bundled resources (templates + harness-configs).
func BuiltinResources() []BundledResource {
	return append(BuiltinTemplates(), BuiltinHarnessConfigs()...)
}

// PlatformSkillsFS returns the embedded filesystem containing platform skills.
// Each top-level directory under platform_skills/ is a skill directory
// containing at minimum a SKILL.md file.
func PlatformSkillsFS() fs.FS {
	sub, err := fs.Sub(platformSkillsFS, "platform_skills")
	if err != nil {
		panic(fmt.Sprintf("resources: sub platform_skills FS: %v", err))
	}
	return sub
}
