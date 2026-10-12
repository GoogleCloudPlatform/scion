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
// config.yaml with the shared harness-config content check
// (unusableProvisionerInYAML). The file handlers also call it before writing the object, so
// refused content never reaches storage on those paths.
func checkBundledHarnessConfigContent(p, name string, data []byte) error {
	if perr := unusableProvisionerInYAML(name, data); perr != nil {
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
	// harness-config check read from it instead of storage, and a blob the
	// commit is missing is written from it.
	dir string
	// create persists the template with CreateTemplate instead of
	// UpdateTemplateContent (template and project clone).
	create bool
	// expectedContentHash, when set, is the content hash the commit
	// requires the stored row to have. It overrides the default, which is
	// tmpl.ContentHash as the caller read it. Finalize sets it from the
	// client's expectedContentHash, so the compare-and-swap covers the
	// client's whole diff, upload and finalize window (ptone/scion#4221).
	expectedContentHash string
	// copyFrom, when set, is the template a clone copies missing blobs
	// from (its blobs, or its path objects for a legacy source).
	copyFrom *store.Template
	// fromStaging makes the commit move staged uploads into blobs
	// (finalize). uploadID, when set, names the upload whose staging
	// directory holds them; without it (older clients) any staged object
	// for the path whose hash matches is used.
	fromStaging bool
	uploadID    string
	// written lists blob object paths the caller has just written for this
	// commit (the file APIs and the bootstrap upload), so they are not
	// checked again.
	written map[string]bool
}

// errTemplateStorageNotConfigured is returned when a commit has no storage.
var errTemplateStorageNotConfigured = errors.New("storage not configured")

// commitTemplateFiles is the single commit path for a template's files. It:
//
//  1. makes every blob the new manifest introduces exist under the row's
//     content base (ensureTemplateBlobs): staged uploads are hashed and
//     moved, a legacy row's files are copied in, a clone's are copied from
//     its source. Blobs the row as read already references are not touched,
//     so a single-file edit costs the same on a 2-file and a 200-file
//     template;
//  2. computes ContentHash;
//  3. derives Harness, DefaultHarnessConfig and AgentConfig (deriveTemplateIndex);
//  4. validates the bundled harness-configs/*/config.yaml the commit
//     introduces (harness.CheckProvisionerUsable);
//  5. writes the row once, in the blob layout: CreateTemplate with
//     opts.create, otherwise UpdateTemplateContent, a compare-and-swap
//     against the content hash and layout the caller read (or
//     opts.expectedContentHash). A row changed by another commit since then
//     fails with store.ErrTemplateConflict, which the HTTP handlers map to
//     409 template_conflict (ptone/scion#4221);
//  6. when the commit migrated a legacy row, removes the legacy tree unless
//     another row shares that path.
//
// Blobs are never deleted by a commit: files dropped from the manifest stay
// for in-flight hydrations until the blob garbage collector removes them
// after its grace period. On any error before the write, tmpl is left
// unchanged. Other fields the caller set on tmpl (Status, SourceURL, Config,
// ...) are persisted as they are.
func (s *Server) commitTemplateFiles(ctx context.Context, tmpl *store.Template, next []store.TemplateFile, opts commitOpts) error {
	stor := s.GetStorage()
	if stor == nil {
		return errTemplateStorageNotConfigured
	}
	next = append([]store.TemplateFile(nil), next...)
	expected := templateCommitPrecondition(tmpl, opts)

	if err := validateManifestFilePaths(next); err != nil {
		return err
	}

	// 1. Make the introduced blobs exist under the content base.
	base := s.templateContentBase(tmpl)
	next, introduced, err := s.ensureTemplateBlobs(ctx, stor, tmpl, base, next, opts)
	if err != nil {
		return err
	}

	// 2. Content hash.
	contentHash := computeContentHash(next)

	// 3. Derive the index from the template's own agent config.
	read := blobFileReader(stor, base, next)
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

	// 4. Refuse a bundled harness-config that could never provision an
	// agent. Bundled configs the row already had were checked when they
	// were committed.
	if err := checkBundledHarnessConfigs(ctx, read, introduced); err != nil {
		return err
	}

	// 5. One row write, in the blob layout.
	prevState := *tmpl
	migratedFrom := ""
	if !isBlobLayout(tmpl) {
		migratedFrom = tmpl.StoragePath
	}
	tmpl.Files = next
	tmpl.ContentHash = contentHash
	tmpl.Harness = idx.Harness
	tmpl.DefaultHarnessConfig = idx.DefaultHarnessConfig
	tmpl.AgentConfig = idx.AgentConfig
	tmpl.Layout = store.TemplateLayoutBlobs
	if tmpl.StoragePath != base {
		tmpl.StoragePath = base
		bucket := tmpl.StorageBucket
		if bucket == "" {
			bucket = stor.Bucket()
		}
		tmpl.StorageBucket = bucket
		tmpl.StorageURI = storage.StorageURIForPath(bucket, base)
	}
	if opts.create {
		err = s.store.CreateTemplate(ctx, tmpl)
	} else {
		err = s.store.UpdateTemplateContent(ctx, tmpl, expected)
	}
	if err != nil {
		*tmpl = prevState
		return err
	}

	// 6. A migrated legacy row's old tree.
	if migratedFrom != "" {
		s.removeLegacyTemplateTree(ctx, stor, tmpl, migratedFrom)
	}

	// A template that sets environment keys is committed as is (the
	// AgentConfig snapshot stays a faithful parse), but agents ignore those
	// keys (ptone/scion#4524), so say so.
	if keys := config.TemplateEnvironmentKeys(tmpl.AgentConfig, nil); len(keys) > 0 {
		slog.Warn("template commit: template sets environment keys; agents ignore them (environment comes from settings)",
			"template", tmpl.Name, "template_id", tmpl.ID, "scope", tmpl.Scope, "keys", keys)
	}
	return nil
}

// templateCommitPrecondition returns the stored state a commit of tmpl
// requires: the content hash and layout the caller read, unless opts names
// the content hash.
//
// The precondition is built from the row as read, and opts overrides only
// the fields it names, so every field of TemplateContentPrecondition (the
// layout included, ptone/scion#4221) is checked for every commit, including
// a finalize that names its content hash.
func templateCommitPrecondition(tmpl *store.Template, opts commitOpts) store.TemplateContentPrecondition {
	p := store.TemplateContentPrecondition{ContentHash: tmpl.ContentHash, Layout: tmpl.Layout}
	if opts.expectedContentHash != "" {
		p.ContentHash = opts.expectedContentHash
	}
	return p
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
	var mismatch *hashMismatchError
	if errors.As(err, &mismatch) {
		ValidationError(w, err.Error(), nil)
		return
	}
	var badHash *invalidFileHashError
	if errors.As(err, &badHash) {
		ValidationError(w, err.Error(), map[string]interface{}{"field": "manifest.files[].hash"})
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
	if errors.Is(err, store.ErrTemplateConflict) {
		writeError(w, http.StatusConflict, ErrCodeTemplateConflict,
			"template was changed by another commit; re-read it and retry", nil)
		return
	}
	writeErrorFromErr(w, err, "")
}

// isTemplateCommitRefusal reports whether err is a commit refused for its
// content (an unusable bundled harness-config), which callers outside the
// template handlers map with writeTemplateCommitError rather than as an
// internal error.
func isTemplateCommitRefusal(err error) bool {
	var unusable *unusableBundledHarnessConfigError
	return errors.As(err, &unusable)
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

// Template job config (ptone/scion#4222).
//
// The hub reads a template's job keys (model, secrets, message_mode, skills)
// from its AgentConfig snapshot, never from the template's storage files at
// dispatch and never from the row's API-written Config. Every such read goes
// through templateJobConfig, which passes the snapshot through
// config.StripTemplateEnvironmentKeys with no harness credentials: on the
// hub that strips the fixed reserved keys, and the broker strips again with
// the harness credentials. The snapshot itself stays a faithful parse.
//
// A row committed before the snapshot existed has none. The first read
// derives it from the row's agent config file and persists it
// (ensureTemplateAgentConfig), and BackfillTemplateAgentConfigs does the
// same for every active row at startup, so rows touched only by the repair
// path are covered too.

// templateJobConfig returns tmpl's job config: its AgentConfig snapshot,
// derived and persisted first when the row has none, without environment
// keys. Nil when the template has no usable agent config. tmpl may be nil.
func (s *Server) templateJobConfig(ctx context.Context, tmpl *store.Template) *api.ScionConfig {
	if tmpl == nil {
		return nil
	}
	cfg, _ := config.StripTemplateEnvironmentKeys(s.ensureTemplateAgentConfig(ctx, tmpl), nil)
	return cfg
}

// templateJobConfigByID is templateJobConfig for a template ID. A lookup
// failure yields nil.
func (s *Server) templateJobConfigByID(ctx context.Context, templateID string) *api.ScionConfig {
	if templateID == "" {
		return nil
	}
	tmpl, err := s.store.GetTemplate(ctx, templateID)
	if err != nil || tmpl == nil {
		return nil
	}
	return s.templateJobConfig(ctx, tmpl)
}

// templateHasAgentConfigFile reports whether tmpl's manifest lists an agent
// config file.
func templateHasAgentConfigFile(tmpl *store.Template) bool {
	for _, name := range scionAgentConfigNames {
		if _, ok := templateFileByPath(tmpl.Files, name); ok {
			return true
		}
	}
	return false
}

// ensureTemplateAgentConfig returns tmpl.AgentConfig. When the row has no
// snapshot but its manifest lists an agent config file, it derives the
// snapshot the way a commit does (deriveTemplateIndex), sets it on tmpl, and
// persists it with store.SetTemplateAgentConfig. That writes only the
// snapshot column, only while the row still has no snapshot and the content
// hash and layout read, so a concurrent commit is never reverted and no
// metadata column is touched. It re-sets Updated to the value it read, so a
// metadata edit landing between that read and the write can see its Updated
// time (only) rolled back; tightening that is a follow-up.
//
// A file that cannot be read or does not parse yields nil and writes
// nothing. A parse failure or a missing object is remembered and not read
// again until the content hash changes; a transient read error (cancelled
// request, timeout, storage error) is retried on the next call. Calling it
// again on a row that has its snapshot is a no-op, so it is idempotent.
func (s *Server) ensureTemplateAgentConfig(ctx context.Context, tmpl *store.Template) *api.ScionConfig {
	if tmpl == nil {
		return nil
	}
	if tmpl.AgentConfig != nil || !templateHasAgentConfigFile(tmpl) {
		return tmpl.AgentConfig
	}
	missKey := tmpl.ID + "@" + tmpl.ContentHash
	if _, missed := s.templateAgentConfigMisses.Load(missKey); missed {
		return nil
	}
	stor := s.GetStorage()
	if stor == nil {
		return nil
	}
	read := func(ctx context.Context, p string) ([]byte, error) {
		f, ok := templateFileByPath(tmpl.Files, p)
		if !ok {
			return nil, storage.ErrNotFound
		}
		return readDispatchTemplateFile(ctx, stor, templateObjectPath(tmpl, f))
	}
	data, name, err := readTemplateAgentConfig(ctx, read, tmpl.Files)
	if err != nil {
		slog.WarnContext(ctx, "template agent config: cannot read the agent config to derive the snapshot",
			"template", tmpl.Name, "template_id", tmpl.ID, "error", err)
		// Only a deterministic miss is remembered: the object is gone. A
		// cancelled request, a timeout or a storage error is retried on
		// the next call.
		if errors.Is(err, storage.ErrNotFound) && ctx.Err() == nil {
			s.templateAgentConfigMisses.Store(missKey, struct{}{})
		}
		return nil
	}
	idx := deriveTemplateIndex(data, name, tmpl.Name)
	if idx.AgentConfig == nil {
		// The file was read and does not parse (or is too large): the
		// same content always derives the same nothing.
		s.templateAgentConfigMisses.Store(missKey, struct{}{})
		return nil
	}
	tmpl.AgentConfig = idx.AgentConfig
	s.persistTemplateAgentConfig(ctx, tmpl.ID, tmpl.Name, tmpl.ContentHash, tmpl.Layout, idx.AgentConfig)
	return tmpl.AgentConfig
}

// persistTemplateAgentConfig stores cfg as the AgentConfig of template id
// through store.SetTemplateAgentConfig: only that column is written, and
// only while the row still has no snapshot and the content hash and layout
// it was derived from. Failures are logged; the caller already has the
// derived value.
func (s *Server) persistTemplateAgentConfig(ctx context.Context, id, name, contentHash, layout string, cfg *api.ScionConfig) {
	written, err := s.store.SetTemplateAgentConfig(ctx, id, cfg, store.TemplateContentPrecondition{ContentHash: contentHash, Layout: layout})
	switch {
	case err != nil:
		slog.WarnContext(ctx, "template agent config: cannot store the derived snapshot",
			"template", name, "template_id", id, "error", err)
	case written:
		slog.InfoContext(ctx, "template agent config: derived and stored the missing snapshot",
			"template", name, "template_id", id)
	}
	// Not written: a concurrent commit or delete owns the row now.
}

// templateAgentConfigBackfillPage and templateAgentConfigBackfillLimit bound
// one startup backfill pass: rows are listed a page at a time, up to the
// limit in total. Rows past the limit are derived lazily on first read.
const (
	templateAgentConfigBackfillPage  = 500
	templateAgentConfigBackfillLimit = 10000
)

// BackfillTemplateAgentConfigs derives and stores the AgentConfig snapshot of
// every active template row that lists an agent config file but has no
// snapshot (ensureTemplateAgentConfig), sequentially, paging through at most
// templateAgentConfigBackfillLimit rows. It returns the number of rows that
// gained a snapshot. Rows that already have one are skipped, so a second
// call does nothing (idempotent).
func (s *Server) BackfillTemplateAgentConfigs(ctx context.Context) int {
	if s.GetStorage() == nil {
		return 0
	}
	derived, scanned := 0, 0
	cursor := ""
	for scanned < templateAgentConfigBackfillLimit && ctx.Err() == nil {
		result, err := s.store.ListTemplates(ctx, store.TemplateFilter{
			Status: store.TemplateStatusActive,
		}, store.ListOptions{Limit: templateAgentConfigBackfillPage, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			s.resourceLog.Warn("template agent config backfill: failed to list templates", "error", err)
			break
		}
		if result == nil {
			break
		}
		for i := range result.Items {
			if ctx.Err() != nil {
				break
			}
			scanned++
			t := &result.Items[i]
			if t.AgentConfig != nil || !templateHasAgentConfigFile(t) {
				continue
			}
			if s.ensureTemplateAgentConfig(ctx, t) != nil {
				derived++
			}
		}
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
	}
	if cursor != "" && scanned >= templateAgentConfigBackfillLimit {
		s.resourceLog.Info("template agent config backfill: row limit reached; rows past it are derived lazily on first read",
			"limit", templateAgentConfigBackfillLimit)
	}
	if derived > 0 {
		s.resourceLog.Info("template agent config backfill: derived missing snapshots", "count", derived)
	}
	return derived
}

// templateJobConfig returns a Hub template's job config for dispatch: the
// server's provider (Server.templateJobConfigByID, which derives a missing
// snapshot once) when one is registered, otherwise the row's stored
// snapshot without environment keys.
func (d *HTTPAgentDispatcher) templateJobConfig(ctx context.Context, templateID string) *api.ScionConfig {
	if templateID == "" {
		return nil
	}
	if d.templateJobConfigProvider != nil {
		return d.templateJobConfigProvider(ctx, templateID)
	}
	tmpl, err := d.store.GetTemplate(ctx, templateID)
	if err != nil || tmpl == nil {
		return nil
	}
	cfg, _ := config.StripTemplateEnvironmentKeys(tmpl.AgentConfig, nil)
	return cfg
}
