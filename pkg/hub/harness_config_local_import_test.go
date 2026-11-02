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

//go:build !no_sqlite && !windows

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// jsonLogLines decodes the JSON log lines in buf.
func jsonLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// The startup bootstrap loads a harness-config's config.yaml confined to its
// directory: a linked config.yaml fails the load, so the directory is skipped
// before collection, and no row is stored for it.
func TestBootstrapHarnessConfigsFromDir_LinkedConfigYAMLNotLoaded(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	var logBuf bytes.Buffer
	srv.resourceLog = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	parent := localLinkedConfigYAMLTree(t)

	if err := srv.BootstrapHarnessConfigsFromDir(context.Background(), parent); err != nil {
		t.Fatal(err)
	}

	stored := harnessConfigsByName(t, s)
	_, byDir := stored["claude-real"]
	_, byName := stored["real-cfg"]
	if !byDir && !byName {
		t.Fatalf("claude-real was not stored (positive control); stored: %v", stored)
	}
	for name, hc := range stored {
		if name == "claude-linked" || name == "outside-cfg" {
			t.Errorf("a harness-config whose config.yaml is a link was stored: %+v", hc)
		}
	}
	loadSkipped := false
	for _, rec := range jsonLogLines(t, &logBuf) {
		if rec["config"] != "claude-linked" {
			continue
		}
		switch rec["msg"] {
		case "harness config bootstrap: failed to load config, skipping":
			loadSkipped = true
		default:
			t.Errorf("claude-linked reached %q; want it skipped at the load", rec["msg"])
		}
	}
	if !loadSkipped {
		t.Error("claude-linked was not skipped at the config.yaml load")
	}
}

// On the import path, a harness-config directory admitted for import (here
// directly, past discovery) whose config.yaml is a link, with a name from
// which a harness can be inferred, is refused at the commit and not stored,
// active or otherwise.
func TestHarnessConfigImport_LinkedConfigYAMLRefusedPastDiscovery(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	parent := localLinkedConfigYAMLTree(t)
	kind := srv.harnessConfigImportKind()
	// The store as a pre-collection read that followed the link would
	// have chosen it.
	kind.newStore = func(string) (*ResourceStore, error) { return srv.harnessConfigStore("claude"), nil }
	kind.checkContent = nil
	dirs := []resourceDir{{name: "claude-linked", path: filepath.Join(parent, "claude-linked")}}

	imported := srv.importResourceDirs(context.Background(), dirs, nil, store.HarnessConfigScopeGlobal, "", kind, nil)
	if len(imported) != 0 {
		t.Errorf("imported = %v, want nothing", imported)
	}
	if hc, ok := harnessConfigsByName(t, s)["claude-linked"]; ok {
		t.Errorf("a harness-config whose config.yaml is a link was stored: %+v", hc)
	}
}
