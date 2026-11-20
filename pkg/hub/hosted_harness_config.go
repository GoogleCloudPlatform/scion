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
	"net/http"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// On a hosted hub, a project-scope harness config is member content: its
// config.yaml is checked with hostedguard wherever it is stored (every
// commit through commitHarnessConfigFiles, the file APIs before they write,
// and import before anything is persisted) and again at dispatch, so a
// harness config stored before the check is refused at its next dispatch.
// Global harness configs are operator content and user-scope ones are not
// dispatched by name; neither is checked. A Runtime Broker trusts the copy
// the hub hydrates for it.

// hostedHarnessConfigError is a project-scope harness config refused on a
// hosted hub. Its message names the harness config and the refused entry,
// never a host path or a value.
type hostedHarnessConfigError struct {
	err error
}

func (e *hostedHarnessConfigError) Error() string { return e.err.Error() }

func (e *hostedHarnessConfigError) Unwrap() error { return e.err }

// checksHostedHarnessConfig reports whether harness configs of scope are
// checked: project scope, on a hosted hub.
func (s *Server) checksHostedHarnessConfig(scope string) bool {
	return !s.workstation && scope == store.HarnessConfigScopeProject
}

// hostedHarnessConfigYAMLRefusal checks the config.yaml of the member-written
// harness config name. present is false when there is no config.yaml; data
// is nil for one too large to read, which is refused because it cannot be
// checked. A config.yaml that does not parse is not refused here: no Runtime
// Broker can load it either. runtimeKind is the runtime that will run the
// agent, or "" when not known (when the config is stored), which leaves gcs
// volumes to the broker.
func hostedHarnessConfigYAMLRefusal(name string, data []byte, present bool, runtimeKind string) error {
	if !present {
		return nil
	}
	if data == nil {
		return &hostedHarnessConfigError{err: fmt.Errorf("%s: config.yaml is too large to check on a hosted server",
			hostedguard.Origin{Kind: hostedguard.OriginHarnessConfig, Name: name})}
	}
	entry, err := config.ParseHarnessConfigYAML(data)
	if err != nil {
		return nil
	}
	if err := hostedguard.CheckMemberConfig(&api.ScionConfig{Volumes: entry.Volumes},
		hostedguard.Origin{Kind: hostedguard.OriginHarnessConfig, Name: name}, runtimeKind); err != nil {
		return &hostedHarnessConfigError{err: err}
	}
	return nil
}

// hostedHarnessConfigManifestRefusal refuses a member-written harness
// config whose manifest holds a file named config.yaml in another spelling
// that full Unicode case folding (cases.Fold over the NFD form, which also
// maps ligatures such as U+FB01) makes equal to config.yaml: a Runtime
// Broker on a case-insensitive filesystem could read it as the harness
// config's config.yaml, which the Hub checks only under its exact name.
// Paths in a subdirectory never match.
func hostedHarnessConfigManifestRefusal(name string, files []store.TemplateFile) error {
	fold := cases.Fold()
	for _, f := range files {
		if f.Path != harnessConfigYAMLPath && fold.String(norm.NFD.String(f.Path)) == harnessConfigYAMLPath {
			return &hostedHarnessConfigError{err: fmt.Errorf("file %q in %s: config.yaml must be spelled in lower case on a hosted server",
				f.Path, hostedguard.Origin{Kind: hostedguard.OriginHarnessConfig, Name: name})}
		}
	}
	return nil
}

// writeHostedHarnessConfigRefusal writes err as 400 hosted_config_refused
// and returns true when it is (or wraps) a hostedHarnessConfigError.
func writeHostedHarnessConfigRefusal(w http.ResponseWriter, err error) bool {
	var he *hostedHarnessConfigError
	if !errors.As(err, &he) {
		return false
	}
	writeError(w, http.StatusBadRequest, hostedguard.ErrorCode, he.Error(), nil)
	return true
}

// refuseHostedHarnessConfigYAML is the file APIs' check of a config.yaml
// about to be written to hc, before any object is written; the commit runs
// it again. It writes the refusal and returns true when refused.
func (s *Server) refuseHostedHarnessConfigYAML(w http.ResponseWriter, hc *store.HarnessConfig, data []byte) bool {
	if !s.checksHostedHarnessConfig(hc.Scope) {
		return false
	}
	return writeHostedHarnessConfigRefusal(w, hostedHarnessConfigYAMLRefusal(hc.Name, data, true, ""))
}

// hostedHarnessConfigImportError refuses an import or reimport into project
// scope on a hosted hub because one or more harness configs carry refused
// content. It names every refused harness config, ordered by name.
type hostedHarnessConfigImportError struct {
	errs []error
}

func (e *hostedHarnessConfigImportError) Error() string {
	msgs := make([]string, len(e.errs))
	for i, err := range e.errs {
		msgs[i] = err.Error()
	}
	return strings.Join(msgs, "; ")
}

// checkHostedHarnessConfigDirs applies hostedHarnessConfigYAMLRefusal to
// the config.yaml of every harness-config directory selected for import
// into scope, before anything is persisted. A config.yaml that cannot be
// read is left to the existing import handling, and the commit checks the
// collected copy again.
func (s *Server) checkHostedHarnessConfigDirs(dirs []resourceDir, scope string) error {
	if !s.checksHostedHarnessConfig(scope) {
		return nil
	}
	sorted := slices.Clone(dirs)
	slices.SortStableFunc(sorted, func(a, b resourceDir) int { return strings.Compare(a.name, b.name) })
	var errs []error
	for _, rd := range sorted {
		data, err := readLocalHarnessConfigFile(rd.path, harnessConfigYAMLPath)
		if err != nil {
			continue
		}
		if err := hostedHarnessConfigYAMLRefusal(rd.name, data, true, ""); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &hostedHarnessConfigImportError{errs: errs}
}

// writeImportContentRefusal writes an import refused by its content check:
// 422 harness_config_unusable for an unusable provisioner, 400
// hosted_config_refused for refused member content. It returns false for
// any other error.
func writeImportContentRefusal(w http.ResponseWriter, err error) bool {
	if writeUnusableProvisionerImportError(w, err) {
		return true
	}
	var he *hostedHarnessConfigImportError
	if errors.As(err, &he) {
		writeError(w, http.StatusBadRequest, hostedguard.ErrorCode, he.Error(), nil)
		return true
	}
	return false
}

// checkStoredHostedHarnessConfig checks, at dispatch, the stored config.yaml
// of the hub harness config id an agent is dispatched with, for runtimeKind
// ("" when not known; hostedRuntimeKind). Only a
// project-scope harness config on a hosted hub is checked. A record that no
// longer exists, has no config.yaml, or a hub without storage is left to the
// dispatch (the broker cannot hydrate it either); a storage read error fails
// the dispatch.
func (s *Server) checkStoredHostedHarnessConfig(ctx context.Context, id, runtimeKind string) error {
	if s.workstation || id == "" {
		return nil
	}
	hc, err := s.store.GetHarnessConfig(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hosted harness-config check: %w", err)
	}
	if !s.checksHostedHarnessConfig(hc.Scope) {
		return nil
	}
	if err := hostedHarnessConfigManifestRefusal(hc.Name, hc.Files); err != nil {
		return err
	}
	if _, ok := templateFileByPath(hc.Files, harnessConfigYAMLPath); !ok {
		return nil
	}
	stor := s.GetStorage()
	if stor == nil {
		return nil
	}
	data, err := s.readHarnessConfigFile(ctx, stor, hc, harnessConfigYAMLPath, maxHarnessConfigFileSize)
	switch {
	case errors.Is(err, errTemplateObjectTooLarge):
		return hostedHarnessConfigYAMLRefusal(hc.Name, nil, true, runtimeKind)
	case errors.Is(err, storage.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("hosted harness-config check: %w", err)
	}
	return hostedHarnessConfigYAMLRefusal(hc.Name, data, true, runtimeKind)
}
