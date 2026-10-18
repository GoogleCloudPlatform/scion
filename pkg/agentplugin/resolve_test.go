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

package agentplugin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestResolveTemplateRef(t *testing.T) {
	base := t.TempDir()
	tpl := filepath.Join(base, "tpl")
	writeFiles(t, base, map[string]string{
		"tpl/plugins/acme/plugin.json":     minimalManifest,
		"tpl/real/nested/p/plugin.json":    minimalManifest,
		"tpl/plugins/file":                 "x",
		"outside/evil/plugin.json":         minimalManifest,
		"tpl/realparent/inner/plugin.json": minimalManifest,
	})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink("acme", filepath.Join(tpl, "plugins/linked")))
	must(os.Symlink("realparent", filepath.Join(tpl, "linkparent")))
	must(os.Symlink("../../outside/evil", filepath.Join(tpl, "plugins/escape")))

	ok := []string{"plugins/acme", "./plugins/acme", "real/nested/p", "plugins/acme/"}
	for _, p := range ok {
		r, err := ResolveTemplateRef(tpl, api.AgentPluginReference{Path: p})
		if err != nil {
			t.Errorf("%q: %v", p, err)
			continue
		}
		if _, err := r.Lstat("plugin.json"); err != nil {
			t.Errorf("%q: plugin.json not visible: %v", p, err)
		}
		_ = r.Close()
	}
	invalid := []string{"", "/abs/path", "../outside/evil", "plugins/../plugins/acme", ".", "./", `plugins\acme`}
	for _, p := range invalid {
		if _, err := ResolveTemplateRef(tpl, api.AgentPluginReference{Path: p}); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("%q: err = %v, want ErrInvalidPath", p, err)
		}
	}
	notFound := []string{"plugins/missing", "plugins/file", "plugins/linked", "linkparent/inner", "plugins/escape"}
	for _, p := range notFound {
		if _, err := ResolveTemplateRef(tpl, api.AgentPluginReference{Path: p}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: err = %v, want ErrNotFound", p, err)
		}
	}
}
