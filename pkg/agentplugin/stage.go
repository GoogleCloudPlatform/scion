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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TemplateSource is one template of the chain with its own agent_plugins.
type TemplateSource struct {
	Name string                     // template reference, for messages
	Dir  string                     // template directory
	Refs []api.AgentPluginReference // that template's own agent_plugins
}

// StageRequest describes one staging run.
type StageRequest struct {
	Chain     []TemplateSource // in chain order: default first, leaf last
	AgentHome string
	// ReservedSkillNames are the skill directory names the skills dir already
	// holds from this provisioning (template, platform, bank). A plugin with the
	// same name is flagged skills_dir_conflict (only claude acts on it).
	ReservedSkillNames map[string]bool
	Limits             Limits
}

// StageResult is the outcome of Stage.
type StageResult struct {
	Index       *Index       // nil when nothing staged
	Diagnostics []Diagnostic // all warnings, in order
}

type candidate struct {
	tpl      TemplateSource
	ref      api.AgentPluginReference
	root     *os.Root
	manifest *Manifest
	diags    []Diagnostic
}

// Stage resolves, validates and copies every ref, then writes
// inputs/plugins/<name>/ and inputs/plugins.json under AgentHome, replacing
// whatever an earlier run staged. A later template's plugin with the same
// manifest name replaces an earlier one (warning). A non-optional ref with a
// fatal error makes Stage return an error (provisioning fails); an optional
// ref with a fatal error is skipped with a warning.
//
// Each bundle is copied into inputs/plugins/.tmp-<name> and then renamed,
// so a half-copied bundle is never indexed. The bundle directory name is
// always the validated manifest name, never the source directory name.
func Stage(ctx context.Context, req StageRequest) (*StageResult, error) {
	res := &StageResult{}
	lim := req.Limits
	if lim == (Limits{}) {
		lim = DefaultLimits
	}

	home, err := os.OpenRoot(req.AgentHome)
	if err != nil {
		return nil, fmt.Errorf("open agent home: %w", err)
	}
	defer func() { _ = home.Close() }()
	if err := home.MkdirAll(InputsRel, 0o755); err != nil {
		return nil, fmt.Errorf("create staged inputs: %w", err)
	}
	if ok, err := realDirs(home, InputsRel); err != nil || !ok {
		return nil, fmt.Errorf("staged inputs directory %s is not a real directory", InputsRel)
	}
	pluginsRel := filepath.Join(InputsRel, PluginsDirName)
	indexRel := filepath.Join(InputsRel, IndexFileName)
	if err := home.RemoveAll(pluginsRel); err != nil {
		return nil, fmt.Errorf("reset staged plugins: %w", err)
	}
	if err := home.Remove(indexRel); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reset staged plugin index: %w", err)
	}

	var closers []*os.Root
	defer func() {
		for _, r := range closers {
			_ = r.Close()
		}
	}()

	// fail turns a fatal per-ref problem into an error, or a warning for an
	// optional ref.
	fail := func(ref api.AgentPluginReference, msg string) error {
		if !ref.Optional {
			return errors.New(msg)
		}
		res.Diagnostics = append(res.Diagnostics, warning("%s (optional plugin skipped)", msg))
		return nil
	}

	// Phase 1: resolve and validate every ref; later names replace earlier.
	var order []string
	winners := map[string]*candidate{}
	for _, tpl := range req.Chain {
		for _, ref := range tpl.Refs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			root, err := ResolveTemplateRef(tpl.Dir, ref)
			if err != nil {
				var msg string
				switch {
				case errors.Is(err, ErrInvalidPath):
					msg = fmt.Sprintf("template %s: path %q must be relative to the template directory", tpl.Name, ref.Path)
				case errors.Is(err, ErrNotFound):
					msg = fmt.Sprintf("template %s: plugin dir %q not found or is a symlink", tpl.Name, ref.Path)
				default:
					msg = fmt.Sprintf("template %s: plugin dir %q: %v", tpl.Name, ref.Path, err)
				}
				if err := fail(ref, msg); err != nil {
					return nil, err
				}
				continue
			}
			closers = append(closers, root)
			m, diags, err := LoadManifest(root)
			if err != nil {
				var se *UnsupportedSchemaError
				msg := fmt.Sprintf("plugin at %q: %v", ref.Path, err)
				if errors.As(err, &se) && se.Name != "" {
					msg = se.Error()
				}
				if err := fail(ref, msg); err != nil {
					return nil, err
				}
				continue
			}
			if prev, ok := winners[m.Name]; ok {
				res.Diagnostics = append(res.Diagnostics, Diagnostic{Plugin: m.Name, Severity: SeverityWarning,
					Message: fmt.Sprintf("from template %s replaces the one from %s", tpl.Name, prev.tpl.Name)})
			} else {
				order = append(order, m.Name)
			}
			winners[m.Name] = &candidate{tpl: tpl, ref: ref, root: root, manifest: m, diags: diags}
		}
	}
	if len(order) == 0 {
		return res, nil
	}

	// Phase 2: copy, inspect and index the winners.
	if err := home.Mkdir(pluginsRel, 0o755); err != nil {
		return nil, fmt.Errorf("create staged plugins dir: %w", err)
	}
	idx := &Index{SchemaVersion: IndexSchemaVersion, SpecVersion: SpecVersion}
	for _, name := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := winners[name]
		entry, err := stageOne(home, pluginsRel, c, req.ReservedSkillNames, lim)
		if err != nil {
			if err := fail(c.ref, fmt.Sprintf("plugin %q: %v", name, err)); err != nil {
				return nil, err
			}
			continue
		}
		for _, d := range entry.Diagnostics {
			d.Plugin = name
			res.Diagnostics = append(res.Diagnostics, d)
		}
		idx.Plugins = append(idx.Plugins, *entry)
	}
	if len(idx.Plugins) == 0 {
		_ = home.RemoveAll(pluginsRel)
		return res, nil
	}
	if err := writeIndex(home, indexRel, idx); err != nil {
		return nil, err
	}
	res.Index = idx
	return res, nil
}

// stageOne copies one validated plugin to pluginsRel/<name> and builds its
// index entry. On error nothing is left under pluginsRel for it.
func stageOne(home *os.Root, pluginsRel string, c *candidate, reserved map[string]bool, lim Limits) (*IndexEntry, error) {
	name := c.manifest.Name
	tmpRel := filepath.Join(pluginsRel, ".tmp-"+name)
	finalRel := filepath.Join(pluginsRel, name)
	_ = home.RemoveAll(tmpRel)
	rep, err := CopyTree(c.root, ".", home, tmpRel, lim)
	if err != nil {
		var le *LimitError
		if errors.As(err, &le) {
			return nil, le
		}
		return nil, fmt.Errorf("copy bundle: %w", err)
	}
	diags := append([]Diagnostic(nil), c.diags...)
	for _, s := range rep.Skipped {
		diags = append(diags, warning("skipped symlink or special file %s (not followed)", s))
	}
	if err := home.RemoveAll(finalRel); err != nil {
		_ = home.RemoveAll(tmpRel)
		return nil, err
	}
	if err := home.Rename(tmpRel, finalRel); err != nil {
		_ = home.RemoveAll(tmpRel)
		return nil, fmt.Errorf("finalize staged bundle: %w", err)
	}
	cleanup := func(err error) (*IndexEntry, error) {
		_ = home.RemoveAll(finalRel)
		return nil, err
	}

	staged, err := home.OpenRoot(finalRel)
	if err != nil {
		return cleanup(err)
	}
	defer func() { _ = staged.Close() }()
	digest, err := TreeDigest(staged, ".")
	if err != nil {
		return cleanup(fmt.Errorf("digest: %w", err))
	}
	skills, sdiags := DiscoverSkills(staged)
	diags = append(diags, sdiags...)
	mcp, mdiags := LoadMCP(staged, c.manifest.Schema)
	diags = append(diags, mdiags...)
	if skills == nil {
		skills = []string{}
	}
	return &IndexEntry{
		Name:              name,
		Version:           c.manifest.Version,
		Description:       c.manifest.Description,
		Dir:               filepath.ToSlash(filepath.Join(PluginsDirName, name)),
		Digest:            digest,
		Source:            IndexSource{Template: c.tpl.Name, Path: c.ref.Path},
		Skills:            skills,
		SkillsDirConflict: reserved[name],
		MCP:               mcp,
		Diagnostics:       diags,
	}, nil
}

func writeIndex(home *os.Root, indexRel string, idx *Index) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plugin index: %w", err)
	}
	data = append(data, '\n')
	tmp := indexRel + ".tmp"
	_ = home.Remove(tmp)
	if err := home.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write plugin index: %w", err)
	}
	if err := home.Rename(tmp, indexRel); err != nil {
		_ = home.Remove(tmp)
		return fmt.Errorf("write plugin index: %w", err)
	}
	return nil
}
