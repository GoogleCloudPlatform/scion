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
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// template_commit.go is the single commit path for a template's files
// (ptone/scion#4217). Every writer of a template's file manifest goes through
// commitTemplateFiles: CLI finalize, the file write/raw/upload/delete
// handlers, the import/reimport/bootstrap persistence hooks, repair from
// storage, and template and project clone. It is the only writer of Files,
// ContentHash and the derived fields (Harness, DefaultHarnessConfig,
// AgentConfig), and the derived fields come from one parser
// (config.ParseScionAgentConfig) with one key order, the broker's
// (config.ResolveHarnessConfigName).

// scionAgentConfigNames is the lookup order for a template's own agent config
// file. It matches config.GetScionAgentConfigPath and templateSkillRefs.
var scionAgentConfigNames = []string{"scion-agent.yaml", "scion-agent.yml", "scion-agent.json"}

// templateIndex holds the fields derived from a template's agent config.
type templateIndex struct {
	Harness              string
	DefaultHarnessConfig string
	AgentConfig          *api.ScionConfig
}

// deriveTemplateIndex derives a template's index from the bytes of its own
// agent config file. fileName is the file's name (it selects YAML or JSON);
// an empty fileName means the template has no agent config file.
//
//   - The snapshot is the parsed api.ScionConfig with per-agent fields
//     cleared. It is the named template's own file, not merged with any
//     base or default template.
//   - DefaultHarnessConfig uses the broker's key order (decision E5):
//     default_harness_config, then harness_config.
//   - Harness is the explicit `harness:` when set, otherwise inferred from
//     DefaultHarnessConfig, otherwise inferred from the template name.
//
// A file that does not parse is derived like a missing one (no snapshot, no
// DefaultHarnessConfig, Harness from the name) and logged as a warning; it is
// not rejected.
func deriveTemplateIndex(agentCfgBytes []byte, fileName, templateName string) templateIndex {
	var cfg *api.ScionConfig
	if fileName != "" {
		parsed, err := config.ParseScionAgentConfig(fileName, agentCfgBytes)
		if err != nil {
			slog.Warn("template commit: agent config does not parse; deriving from the template name",
				"template", templateName, "file", fileName, "error", err)
		} else {
			cfg = parsed
		}
	}
	idx := templateIndex{}
	if cfg != nil {
		clearPerAgentFields(cfg)
		idx.AgentConfig = cfg
		if res, err := config.ResolveHarnessConfigName(config.HarnessConfigInputs{TemplateCfg: cfg}); err == nil {
			idx.DefaultHarnessConfig = res.Name
		}
		idx.Harness = cfg.Harness
	}
	if idx.Harness == "" {
		idx.Harness = inferHarnessFromName(idx.DefaultHarnessConfig)
	}
	if idx.Harness == "" {
		idx.Harness = inferHarnessFromName(templateName)
	}
	return idx
}

// clearPerAgentFields drops the fields of a ScionConfig that describe one
// agent rather than a template.
func clearPerAgentFields(cfg *api.ScionConfig) {
	cfg.Task = ""
	cfg.Branch = ""
	cfg.ExplicitWorkspace = false
	cfg.EmptyPerAgentWorkspace = false
	cfg.Info = nil
}

// deriveTemplateIndexFromDir is the directory variant of deriveTemplateIndex:
// it reads the same agent config file from a local template directory (the
// import and bootstrap paths).
func deriveTemplateIndexFromDir(dir, templateName string) templateIndex {
	data, name, err := readTemplateAgentConfig(context.Background(), dirFileReader(dir), manifestFromDir(dir))
	if err != nil {
		slog.Warn("template commit: cannot read agent config; deriving from the template name",
			"template", templateName, "dir", dir, "error", err)
		return deriveTemplateIndex(nil, "", templateName)
	}
	return deriveTemplateIndex(data, name, templateName)
}

// manifestFromDir lists the agent config candidates present in dir as a
// minimal manifest, so the directory variant uses the same lookup as the
// storage path.
func manifestFromDir(dir string) []store.TemplateFile {
	var files []store.TemplateFile
	for _, name := range scionAgentConfigNames {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
			files = append(files, store.TemplateFile{Path: name})
		}
	}
	return files
}

// templateFileReader reads one template file by its manifest path.
type templateFileReader func(ctx context.Context, path string) ([]byte, error)

// errTemplateFileTooLarge marks a file too large to parse for derivation.
var errTemplateFileTooLarge = errors.New("file too large to parse")

func storageFileReader(stor storage.Storage, storagePath string) templateFileReader {
	return func(ctx context.Context, p string) ([]byte, error) {
		reader, _, err := stor.Download(ctx, storagePath+"/"+p)
		if err != nil {
			return nil, err
		}
		if reader == nil {
			return nil, storage.ErrNotFound
		}
		defer func() { _ = reader.Close() }()
		data, err := io.ReadAll(io.LimitReader(reader, maxTemplateFileSize+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxTemplateFileSize {
			return nil, errTemplateFileTooLarge
		}
		return data, nil
	}
}

func dirFileReader(dir string) templateFileReader {
	return func(_ context.Context, p string) ([]byte, error) {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		fi, err := os.Stat(fp)
		if err != nil {
			return nil, err
		}
		if fi.Size() > maxTemplateFileSize {
			return nil, errTemplateFileTooLarge
		}
		return os.ReadFile(fp)
	}
}

// readTemplateAgentConfig returns the bytes and name of the first agent
// config file listed in files (scionAgentConfigNames order). name is "" when
// the manifest lists none. A file too large to parse is returned as
// unparsable content rather than an error.
func readTemplateAgentConfig(ctx context.Context, read templateFileReader, files []store.TemplateFile) ([]byte, string, error) {
	present := make(map[string]bool, len(files))
	for _, f := range files {
		present[f.Path] = true
	}
	for _, name := range scionAgentConfigNames {
		if !present[name] {
			continue
		}
		data, err := read(ctx, name)
		if errors.Is(err, errTemplateFileTooLarge) {
			return nil, name, nil
		}
		if err != nil {
			return nil, "", fmt.Errorf("read %s: %w", name, err)
		}
		return data, name, nil
	}
	return nil, "", nil
}

// bundledHarnessConfigName returns the harness-config name when p is a
// bundled harness-config's config.yaml (harness-configs/<name>/config.yaml).
func bundledHarnessConfigName(p string) (string, bool) {
	parts := strings.Split(p, "/")
	if len(parts) != 3 || parts[0] != "harness-configs" || parts[2] != "config.yaml" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// unusableBundledHarnessConfigError reports a template whose bundled
// harness-config has a provisioner that could never provision an agent.
type unusableBundledHarnessConfigError struct {
	path string
	perr *harness.UnusableProvisionerError
}

func (e *unusableBundledHarnessConfigError) Error() string {
	return "template bundles " + e.path + ": " + e.perr.PublicMessage()
}

func (e *unusableBundledHarnessConfigError) Unwrap() error { return e.perr }

// checkBundledHarnessConfigs validates every bundled harness-config listed in
// files with harness.CheckProvisionerUsable. Content that does not parse is
// left alone, as on the harness-config upload paths.
func checkBundledHarnessConfigs(ctx context.Context, read templateFileReader, files []store.TemplateFile) error {
	for _, f := range files {
		name, ok := bundledHarnessConfigName(f.Path)
		if !ok {
			continue
		}
		data, err := read(ctx, f.Path)
		if errors.Is(err, errTemplateFileTooLarge) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", f.Path, err)
		}
		if err := checkBundledHarnessConfigContent(f.Path, name, data); err != nil {
			return err
		}
	}
	return nil
}

// checkBundledHarnessConfigContent checks one bundled harness-config
// config.yaml. The file handlers also call it before writing the object, so
// refused content never reaches storage on those paths.
func checkBundledHarnessConfigContent(p, name string, data []byte) error {
	entry, err := config.ParseHarnessConfigYAML(data)
	if err != nil {
		return nil
	}
	if perr := harness.CheckProvisionerUsable(name, nil, entry); perr != nil {
		return &unusableBundledHarnessConfigError{path: p, perr: perr}
	}
	return nil
}

// refuseUnusableBundledHarnessConfig writes 422 and returns true when p is a
// bundled harness-config config.yaml whose provisioner is unusable.
func refuseUnusableBundledHarnessConfig(w http.ResponseWriter, p string, data []byte) bool {
	name, ok := bundledHarnessConfigName(p)
	if !ok {
		return false
	}
	if err := checkBundledHarnessConfigContent(p, name, data); err != nil {
		writeTemplateCommitError(w, err)
		return true
	}
	return false
}

// commitOpts selects where a commit reads file content and how it persists.
type commitOpts struct {
	// dir, when set, is a local directory holding the same files as the new
	// manifest (import, reimport, bootstrap). Derivation and the bundled
	// harness-config check read from it instead of storage.
	dir string
	// create persists the template with CreateTemplate instead of
	// UpdateTemplate (template and project clone).
	create bool
}

// errTemplateStorageNotConfigured is returned when a commit has no storage.
var errTemplateStorageNotConfigured = errors.New("storage not configured")

// commitTemplateFiles is the single commit path for a template's files. It:
//
//  1. verifies that every object in next exists in storage;
//  2. computes ContentHash;
//  3. derives Harness, DefaultHarnessConfig and AgentConfig (deriveTemplateIndex);
//  4. validates bundled harness-configs/*/config.yaml (harness.CheckProvisionerUsable);
//  5. writes the row once (UpdateTemplate, or CreateTemplate with opts.create);
//  6. deletes the storage objects of paths in the old manifest but not in next
//     (diff-based, never a prefix sweep).
//
// The removed-object delete runs after the row write so a refused or failed
// commit never leaves the row pointing at deleted objects. On any error before
// the write, tmpl is left unchanged. Other fields the caller set on tmpl
// (Status, SourceURL, Config, ...) are persisted as they are.
func (s *Server) commitTemplateFiles(ctx context.Context, tmpl *store.Template, next []store.TemplateFile, opts commitOpts) error {
	stor := s.GetStorage()
	if stor == nil {
		return errTemplateStorageNotConfigured
	}
	next = append([]store.TemplateFile(nil), next...)

	// 1. Verify the objects exist (and the paths are canonical).
	if _, err := verifyAndFinalizeFiles(ctx, stor, tmpl.StoragePath, next); err != nil {
		return err
	}

	// 2. Content hash.
	contentHash := computeContentHash(next)

	// 3. Derive the index from the template's own agent config.
	read := storageFileReader(stor, tmpl.StoragePath)
	if opts.dir != "" {
		read = dirFileReader(opts.dir)
	}
	var idx templateIndex
	if opts.dir != "" {
		idx = deriveTemplateIndexFromDir(opts.dir, tmpl.Name)
	} else {
		data, name, err := readTemplateAgentConfig(ctx, read, next)
		if err != nil {
			return fmt.Errorf("template commit: %w", err)
		}
		idx = deriveTemplateIndex(data, name, tmpl.Name)
	}

	// 4. Refuse a bundled harness-config that could never provision an agent.
	if err := checkBundledHarnessConfigs(ctx, read, next); err != nil {
		return err
	}

	// 5. One row write.
	previous := tmpl.Files
	prevState := *tmpl
	tmpl.Files = next
	tmpl.ContentHash = contentHash
	tmpl.Harness = idx.Harness
	tmpl.DefaultHarnessConfig = idx.DefaultHarnessConfig
	tmpl.AgentConfig = idx.AgentConfig
	var err error
	if opts.create {
		err = s.store.CreateTemplate(ctx, tmpl)
	} else {
		err = s.store.UpdateTemplate(ctx, tmpl)
	}
	if err != nil {
		*tmpl = prevState
		return err
	}

	// 6. Delete objects dropped from the manifest.
	s.deleteRemovedTemplateFiles(ctx, stor, tmpl, previous)
	return nil
}

// deleteRemovedTemplateFiles deletes the storage objects of files listed in
// previous but no longer in tmpl.Files. Only those exact paths are deleted,
// never a prefix: clones and renamed templates can share a storage prefix.
// Non-canonical legacy paths are skipped. Failures are logged and do not fail
// the commit, because the row is already written.
func (s *Server) deleteRemovedTemplateFiles(ctx context.Context, stor storage.Storage, tmpl *store.Template, previous []store.TemplateFile) {
	if tmpl.StoragePath == "" || len(previous) == 0 {
		return
	}
	current := make(map[string]struct{}, len(tmpl.Files))
	for _, f := range tmpl.Files {
		current[f.Path] = struct{}{}
	}
	for _, f := range previous {
		if _, ok := current[f.Path]; ok {
			continue
		}
		if !isCanonicalResourceFilePath(f.Path) {
			s.templateLog.Warn("template commit: skipping delete of non-canonical removed path",
				"template", tmpl.Name, "id", tmpl.ID)
			continue
		}
		if err := stor.Delete(ctx, tmpl.StoragePath+"/"+f.Path); err != nil && !errors.Is(err, storage.ErrNotFound) {
			s.templateLog.Warn("template commit: failed to delete removed file",
				"template", tmpl.Name, "id", tmpl.ID, "path", f.Path, "error", err)
		}
	}
}

// writeTemplateCommitError maps a commitTemplateFiles error to an HTTP
// response.
func writeTemplateCommitError(w http.ResponseWriter, err error) {
	if writeInvalidFilePathError(w, err) {
		return
	}
	var notFound *fileNotFoundError
	if errors.As(err, &notFound) {
		ValidationError(w, err.Error(), nil)
		return
	}
	var unusable *unusableBundledHarnessConfigError
	if errors.As(err, &unusable) {
		writeError(w, http.StatusUnprocessableEntity, harnessConfigUnusableErrorCode, unusable.Error(), nil)
		return
	}
	if errors.Is(err, errTemplateStorageNotConfigured) {
		RuntimeError(w, "Storage not configured")
		return
	}
	writeErrorFromErr(w, err, "")
}

// upsertTemplateFile returns a copy of files with entry added, or replacing
// the entry with the same path (keeping the existing Mode when entry has
// none).
func upsertTemplateFile(files []store.TemplateFile, entry store.TemplateFile) []store.TemplateFile {
	out := make([]store.TemplateFile, 0, len(files)+1)
	found := false
	for _, f := range files {
		if f.Path == entry.Path {
			e := entry
			if e.Mode == "" {
				e.Mode = f.Mode
			}
			out = append(out, e)
			found = true
			continue
		}
		out = append(out, f)
	}
	if !found {
		out = append(out, entry)
	}
	return out
}

// removeTemplateFile returns a copy of files without the entry for p.
func removeTemplateFile(files []store.TemplateFile, p string) []store.TemplateFile {
	out := make([]store.TemplateFile, 0, len(files))
	for _, f := range files {
		if f.Path != p {
			out = append(out, f)
		}
	}
	return out
}
