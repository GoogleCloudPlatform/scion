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

package hub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// On a hosted hub, a harness config bundled in a project-scope template
// (harness-configs/<name>/config.yaml) is member content, like a
// project-scope harness config: it is checked with
// hostedHarnessConfigYAMLRefusal at every commit of the template's files
// (commitTemplateFiles: file writes, finalize, import, reimport and clone),
// by the file APIs before they write, by import before anything is
// persisted, and again at dispatch for the template the agent uses. A path
// a Runtime Broker's filesystem could read under another name (a
// non-ASCII, trailing-dot or "~" segment, or a case variant of
// harness-configs or config.yaml) is refused (hostedTemplatePathRefusal):
// the broker could load it as a bundled harness config, which the Hub
// checks only under its exact name. Global templates are operator content
// and are not checked.

const bundledHarnessConfigsDir = "harness-configs"

// checksHostedTemplate reports whether templates of scope have their bundled
// harness configs checked: project scope, on a hosted hub.
func (s *Server) checksHostedTemplate(scope string) bool {
	return !s.workstation && scope == store.TemplateScopeProject
}

// hostedTemplateError wraps the refusal of a bundled harness config with the
// template's name.
func hostedTemplateError(tmplName string, err error) error {
	var he *hostedHarnessConfigError
	if errors.As(err, &he) {
		return &hostedHarnessConfigError{err: fmt.Errorf("template %q: %w", tmplName, he.err)}
	}
	return err
}

// hostedTemplatePathRefusal refuses the template file path p when a
// Runtime Broker's filesystem could read it under another name than the Hub
// checks, in particular as a bundled harness config: its top-level segment,
// and under harness-configs/ the harness-config name and the entry in that
// harness config's root, must pass hostedRootFileNameRefusal (plain ASCII,
// no trailing dot or space, no "~", config.yaml only in lower case), and
// harness-configs must be spelled in lower case. Deeper segments are not
// restricted, as for a project-scope harness config.
func hostedTemplatePathRefusal(tmplName, p string) error {
	refuse := func(seg, reason string) error {
		return &hostedHarnessConfigError{err: fmt.Errorf("template %q: file %q: %q: %s on a hosted server", tmplName, p, seg, reason)}
	}
	parts := strings.Split(p, "/")
	if reason := hostedRootFileNameRefusal(parts[0]); reason != "" {
		return refuse(parts[0], reason)
	}
	if parts[0] != bundledHarnessConfigsDir {
		if strings.EqualFold(parts[0], bundledHarnessConfigsDir) {
			return refuse(parts[0], "harness-configs must be spelled in lower case")
		}
		return nil
	}
	for _, seg := range parts[1:min(3, len(parts))] {
		if reason := hostedRootFileNameRefusal(seg); reason != "" {
			return refuse(seg, reason)
		}
	}
	return nil
}

// hostedTemplateHarnessConfigsRefusal checks the bundled harness configs a
// member-written template's manifest lists, reading each config.yaml with
// read, for runtimeKind ("" when not known). A config.yaml too large to read
// is refused, as for a project-scope harness config. With skipMissing, a
// config.yaml whose object is gone is not refused (dispatch: the broker
// cannot hydrate it either); otherwise a read error fails the check.
func hostedTemplateHarnessConfigsRefusal(ctx context.Context, tmplName string, read templateFileReader, files []store.TemplateFile, runtimeKind string, skipMissing bool) error {
	sorted := slices.Clone(files)
	slices.SortStableFunc(sorted, func(a, b store.TemplateFile) int { return strings.Compare(a.Path, b.Path) })
	for _, f := range sorted {
		if err := hostedTemplatePathRefusal(tmplName, f.Path); err != nil {
			return err
		}
	}
	for _, f := range sorted {
		name, ok := bundledHarnessConfigName(f.Path)
		if !ok {
			continue
		}
		data, err := read(ctx, f.Path)
		switch {
		case errors.Is(err, errTemplateFileTooLarge):
			data = nil
		case skipMissing && errors.Is(err, storage.ErrNotFound):
			continue
		case err != nil:
			return fmt.Errorf("read %s: %w", f.Path, err)
		}
		if err := hostedHarnessConfigYAMLRefusal(name, data, true, runtimeKind); err != nil {
			return hostedTemplateError(tmplName, err)
		}
	}
	return nil
}

// hostedTemplateFileRefusal is the template file APIs' check of a file about
// to be written to tmpl at path p, before any object is written; the commit
// checks the whole template again.
func (s *Server) hostedTemplateFileRefusal(tmpl *store.Template, p string, data []byte) error {
	if !s.checksHostedTemplate(tmpl.Scope) {
		return nil
	}
	if err := hostedTemplatePathRefusal(tmpl.Name, p); err != nil {
		return err
	}
	name, ok := bundledHarnessConfigName(p)
	if !ok {
		return nil
	}
	if int64(len(data)) > maxHarnessConfigFileSize {
		data = nil // too large to check, as on the commit path
	}
	return hostedTemplateError(tmpl.Name, hostedHarnessConfigYAMLRefusal(name, data, true, ""))
}

// checkHostedTemplateDirs checks the bundled harness configs of every
// template directory selected for import into scope, before anything is
// persisted. It names every refused template, ordered by name. A file that
// cannot be read is left to the existing import handling; the commit checks
// the collected copy again.
func (s *Server) checkHostedTemplateDirs(dirs []resourceDir, scope string) error {
	if !s.checksHostedTemplate(scope) {
		return nil
	}
	sorted := slices.Clone(dirs)
	slices.SortStableFunc(sorted, func(a, b resourceDir) int { return strings.Compare(a.name, b.name) })
	var errs []error
	for _, rd := range sorted {
		if err := checkLocalTemplateHarnessConfigs(rd); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return &hostedHarnessConfigImportError{errs: errs}
	}
	return nil
}

// checkLocalTemplateHarnessConfigs checks the local template directory rd:
// its top-level names and the entries of harness-configs/ and of each
// bundled harness config's root (hostedTemplatePathRefusal), and each
// bundled config.yaml.
func checkLocalTemplateHarnessConfigs(rd resourceDir) error {
	top, err := os.ReadDir(rd.path)
	if err != nil {
		return nil
	}
	for _, d := range top {
		if err := hostedTemplatePathRefusal(rd.name, d.Name()); err != nil {
			return err
		}
		if !d.IsDir() || d.Name() != bundledHarnessConfigsDir {
			continue
		}
		hcs, err := os.ReadDir(filepath.Join(rd.path, d.Name()))
		if err != nil {
			continue
		}
		for _, hc := range hcs {
			if err := hostedTemplatePathRefusal(rd.name, d.Name()+"/"+hc.Name()); err != nil {
				return err
			}
			if !hc.IsDir() {
				continue
			}
			hcPath := filepath.Join(rd.path, d.Name(), hc.Name())
			files, err := os.ReadDir(hcPath)
			if err != nil {
				continue
			}
			for _, f := range files {
				if err := hostedTemplatePathRefusal(rd.name, d.Name()+"/"+hc.Name()+"/"+f.Name()); err != nil {
					return err
				}
				if f.Name() != harnessConfigYAMLPath {
					continue
				}
				data, err := readLocalHarnessConfigFile(hcPath, harnessConfigYAMLPath)
				if errors.Is(err, errLocalHarnessConfigTooLarge) {
					data, err = nil, nil
				}
				if err != nil {
					continue
				}
				if err := hostedHarnessConfigYAMLRefusal(hc.Name(), data, true, ""); err != nil {
					return hostedTemplateError(rd.name, err)
				}
			}
		}
	}
	return nil
}

// SetHostedTemplateCheck sets the check hostedConfigPrecheck applies to the
// hub template an agent is dispatched with (Server.checkStoredHostedTemplate).
// Nil checks nothing.
func (d *HTTPAgentDispatcher) SetHostedTemplateCheck(check func(ctx context.Context, templateID, runtimeKind string) error) {
	d.hostedTemplateCheck = check
}

// checkStoredHostedTemplate checks, at dispatch, the bundled harness configs
// of the hub template id an agent is dispatched with, for runtimeKind ("" when
// not known; hostedRuntimeKind). Only a project-scope template on a hosted
// hub is checked. A record that no longer exists, an object that is gone or
// a hub without storage is left to the dispatch; a storage read error fails
// the dispatch.
func (s *Server) checkStoredHostedTemplate(ctx context.Context, id, runtimeKind string) error {
	if s.workstation || id == "" {
		return nil
	}
	tmpl, err := s.store.GetTemplate(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hosted template check: %w", err)
	}
	if !s.checksHostedTemplate(tmpl.Scope) {
		return nil
	}
	stor := s.GetStorage()
	if stor == nil {
		for _, f := range tmpl.Files {
			if err := hostedTemplatePathRefusal(tmpl.Name, f.Path); err != nil {
				return err
			}
		}
		return nil
	}
	read := func(ctx context.Context, p string) ([]byte, error) {
		f, ok := templateFileByPath(tmpl.Files, p)
		if !ok {
			return nil, storage.ErrNotFound
		}
		data, err := readTemplateObject(ctx, stor, templateObjectPath(tmpl, f), maxTemplateFileSize)
		if errors.Is(err, errTemplateObjectTooLarge) {
			return nil, errTemplateFileTooLarge
		}
		return data, err
	}
	if err := hostedTemplateHarnessConfigsRefusal(ctx, tmpl.Name, read, tmpl.Files, runtimeKind, true); err != nil {
		var he *hostedHarnessConfigError
		if errors.As(err, &he) {
			return err
		}
		return fmt.Errorf("hosted template check: %w", err)
	}
	return nil
}
