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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// harness_config_commit.go is the single commit path for a harness-config's
// files (ptone/scion#4219, TS-4a). Every writer of a harness-config's file
// manifest goes through commitHarnessConfigFiles: CLI finalize, the file
// write/upload/delete handlers, the bootstrap/import/reimport persistence
// hooks (including harness-configs bundled in a template), repair from
// storage, and harness-config and project clone. It is the only writer of
// Files, ContentHash and the derived fields (Harness and Config), and the
// derived fields come from one parser (config.ParseHarnessConfigYAML) with
// one rule: a key absent from config.yaml clears its field.
//
// Narrow writers that are not commits: archive-obsolete (status) and storage
// migration (path, legacy rows only) write through the same compare-and-swap
// (UpdateHarnessConfigContent) with the row as read
// (harnessConfigCommitPrecondition); the image status and the build
// executor's BuiltImage have their own column writers.
//
// Files are stored in the blob layout (harness_config_blobs.go,
// ptone/scion#4219 part 2).

// harnessConfigYAMLPath is the manifest path of a harness-config's own
// config file.
const harnessConfigYAMLPath = "config.yaml"

// harnessConfigIndex holds the fields derived from a harness-config's
// config.yaml.
type harnessConfigIndex struct {
	// Harness is the row's harness type: the config.yaml `harness:` key,
	// otherwise inferred from the harness-config's name. Empty when both
	// are empty; the commit then keeps the row's current Harness (the
	// column is NOT NULL and non-empty).
	Harness string
	// Config is the whole HarnessConfigData parsed from config.yaml, nil
	// when config.yaml is missing, does not parse, or sets none of its keys.
	Config *store.HarnessConfigData
}

// deriveHarnessConfigIndex derives a harness-config's index from the bytes
// of its config.yaml. present is false when the manifest has no config.yaml.
// name is the harness-config's name (the Harness fallback).
//
// Every HarnessConfigData field comes from config.yaml, and a key absent from
// config.yaml clears its field: there is no merge with what the row held.
// A file that does not parse is derived like a missing one and logged; it is
// not rejected (the provisioner check also passes such content through).
func deriveHarnessConfigIndex(configYAML []byte, present bool, name string) harnessConfigIndex {
	var idx harnessConfigIndex
	if present {
		entry, err := config.ParseHarnessConfigYAML(configYAML)
		if err != nil {
			slog.Warn("harness-config commit: config.yaml does not parse; deriving it as empty",
				"harness_config", name, "error", err)
		} else {
			idx.Harness = entry.Harness
			idx.Config = harnessConfigDataFromEntry(entry)
		}
	}
	if idx.Harness == "" {
		idx.Harness = inferHarnessFromName(name)
	}
	return idx
}

// harnessConfigDataFromEntry maps a parsed config.yaml onto the stored
// HarnessConfigData. It returns nil when no field is set.
func harnessConfigDataFromEntry(entry config.HarnessConfigEntry) *store.HarnessConfigData {
	cfg := &store.HarnessConfigData{
		Harness:          entry.Harness,
		Image:            entry.Image,
		User:             entry.User,
		Model:            entry.Model,
		Args:             entry.Args,
		Env:              entry.Env,
		AuthSelectedType: entry.AuthSelectedType,
		Secrets:          entry.Secrets,
		ModelAliases:     entry.ModelAliases,
	}
	if entry.NoAuthConfig != nil {
		cfg.NoAuthBehavior = entry.NoAuthConfig.Behavior
	}
	// Auth metadata with no auth types declares nothing the hub evaluates.
	if entry.Auth != nil && len(entry.Auth.Types) > 0 {
		cfg.AuthMeta = entry.Auth
	}
	if reflect.ValueOf(*cfg).IsZero() {
		return nil
	}
	return cfg
}

// deriveHarnessConfigIndexFromCollected is the collected-directory variant
// of deriveHarnessConfigIndex (bootstrap, import): config.yaml is read
// through the collected directory's root.
func deriveHarnessConfigIndexFromCollected(cd *collectedDir, name string) harnessConfigIndex {
	data, present, err := cd.harnessConfigYAML()
	if err != nil {
		slog.Warn("harness-config commit: cannot read config.yaml; deriving it as empty",
			"harness_config", name, "dir", cd.dir, "error", err)
		return deriveHarnessConfigIndex(nil, false, name)
	}
	return deriveHarnessConfigIndex(data, present, name)
}

// harnessConfigIndexMatches reports whether hc's stored derived fields equal
// idx (the Harness fallback included). The configs are compared in their
// stored (JSON) form.
func harnessConfigIndexMatches(hc *store.HarnessConfig, idx harnessConfigIndex) bool {
	if idx.Harness != "" && hc.Harness != idx.Harness {
		return false
	}
	a, errA := json.Marshal(hc.Config)
	b, errB := json.Marshal(idx.Config)
	return errA == nil && errB == nil && string(a) == string(b)
}

// unusableHarnessConfigError reports a harness-config commit refused because
// config.yaml declares a provisioner that could never provision an agent.
type unusableHarnessConfigError struct {
	perr *harness.UnusableProvisionerError
}

func (e *unusableHarnessConfigError) Error() string { return e.perr.PublicMessage() }

func (e *unusableHarnessConfigError) Unwrap() error { return e.perr }

// harnessConfigCommitOpts selects where a commit reads file content and how
// it persists.
type harnessConfigCommitOpts struct {
	// collected, when set, is the collected directory holding the same files
	// as the new manifest (bootstrap, import, reimport). config.yaml is read
	// from it, through its root, instead of storage, and a blob the commit is
	// missing is written from it.
	collected *collectedDir
	// content holds file contents the caller already has (the file write
	// and upload handlers), keyed by manifest path; config.yaml is read
	// from it when present.
	content map[string][]byte
	// create persists the harness-config with CreateHarnessConfig instead
	// of UpdateHarnessConfigContent (clone, project clone).
	create bool
	// copyFrom, when set, is the harness-config a clone copies missing
	// blobs from (its blobs, or its path objects for a legacy source).
	copyFrom *store.HarnessConfig
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

// blobOpts is the part of o the shared blob resolver reads.
func (o harnessConfigCommitOpts) blobOpts() commitOpts {
	return commitOpts{collected: o.collected, fromStaging: o.fromStaging, uploadID: o.uploadID, written: o.written}
}

// harnessConfigCommitPrecondition is the stored state a commit of hc
// requires: the content hash, layout and storage path the caller read
// (ptone/scion#4219). The storage path is in the predicate so that a commit
// computed before a storage migration moved the row cannot write the old
// path back after the migration removed the old tree, and the layout so that
// a commit that read the legacy row cannot overwrite its migration.
func harnessConfigCommitPrecondition(hc *store.HarnessConfig) store.HarnessConfigContentPrecondition {
	return store.HarnessConfigContentPrecondition{ContentHash: hc.ContentHash, Layout: hc.Layout, StoragePath: hc.StoragePath}
}

// errHarnessConfigStorageNotConfigured is returned when a commit has no
// storage.
var errHarnessConfigStorageNotConfigured = errors.New("storage not configured")

// commitHarnessConfigFiles is the single commit path for a harness-config's
// files. It:
//
//  1. makes every blob the new manifest introduces exist under the row's
//     content base (ensureBlobs): staged uploads are hashed and moved, a
//     legacy row's files are copied in, a clone's are copied from its
//     source, and a directory's files are written from it. Blobs the row as
//     read already references are not touched, so a single-file edit costs
//     a constant number of storage calls whatever the manifest's size;
//  2. refuses a config.yaml whose provisioner could never provision an
//     agent (unusableProvisionerInYAML, the one content check);
//  3. computes ContentHash;
//  4. derives Harness and Config from config.yaml (deriveHarnessConfigIndex);
//  5. writes the row once, in the blob layout: CreateHarnessConfig with
//     opts.create, otherwise UpdateHarnessConfigContent, a compare-and-swap
//     against the content hash, layout and storage path the caller read. A
//     row changed by another commit (or moved by a migration) since then
//     fails with store.ErrHarnessConfigConflict, which the HTTP handlers
//     map to 409 harness_config_conflict;
//  6. when the commit migrated a legacy row, removes the legacy tree unless
//     another harness-config shares or nests under that path.
//
// Blobs are never deleted by a commit: files dropped from the manifest stay
// for in-flight hydrations until the blob garbage collector removes them
// after its grace period (this replaces the prefix reconcile and the
// removed-file deletes of the legacy layout, ptone/scion#3170). BuiltImage
// is never written. On any error before the write, hc is left unchanged.
// Other fields the caller set on hc (Status, SourceURL, ...) are persisted
// as they are.
func (s *Server) commitHarnessConfigFiles(ctx context.Context, hc *store.HarnessConfig, next []store.TemplateFile, opts harnessConfigCommitOpts) error {
	stor := s.GetStorage()
	if stor == nil {
		return errHarnessConfigStorageNotConfigured
	}
	next = append([]store.TemplateFile(nil), next...)
	expected := harnessConfigCommitPrecondition(hc)

	if err := validateManifestFilePaths(next); err != nil {
		return err
	}
	if opts.collected != nil {
		// A bootstrap or import commit needs the collected config.yaml; it
		// is refused before any blob is written.
		if _, ok := opts.collected.entry(harnessConfigYAMLPath); !ok {
			return fmt.Errorf("harness-config commit: no config.yaml was collected for %q (missing, a link or not a regular file)", hc.Name)
		}
	}

	// 1. Make the introduced blobs exist under the content base. A legacy
	// row is its own copy source (its migration), read from its legacy
	// path.
	base := s.harnessConfigContentBase(hc)
	row := harnessConfigBlobRow(hc)
	if !row.isBlobs() {
		row.StoragePath = ""
		if len(hc.Files) > 0 || hc.StoragePath != "" {
			row.StoragePath = s.harnessConfigLegacyPath(hc)
		}
	}
	var copyFrom *blobRow
	if opts.copyFrom != nil {
		copyFrom = harnessConfigBlobRow(opts.copyFrom)
		if !copyFrom.isBlobs() {
			copyFrom.StoragePath = s.harnessConfigLegacyPath(opts.copyFrom)
		}
	}
	next, _, err := s.ensureBlobs(ctx, stor, row, copyFrom, base, next, opts.blobOpts(), s.resourceLog, "harness-config commit")
	if err != nil {
		return err
	}

	// 2. The content check.
	configYAML, present, err := s.readHarnessConfigYAML(ctx, stor, base, next, opts)
	if err != nil {
		return fmt.Errorf("harness-config commit: %w", err)
	}
	if present {
		if perr := unusableProvisionerInYAML(hc.Name, configYAML); perr != nil {
			return &unusableHarnessConfigError{perr: perr}
		}
	}

	// 3. Content hash. 4. Derived fields.
	contentHash := s.computeContentHash(next)
	idx := deriveHarnessConfigIndex(configYAML, present, hc.Name)

	// 5. One row write, in the blob layout.
	prevState := *hc
	// Only a recorded legacy path is removed after the migration: a row
	// without one never owned a tree it could prove is its own.
	migratedFrom := ""
	if !isHarnessConfigBlobLayout(hc) && !opts.create {
		migratedFrom = hc.StoragePath
	}
	hc.Files = next
	hc.ContentHash = contentHash
	hc.Config = idx.Config
	if idx.Harness == "" && opts.collected != nil && (!present || configYAML != nil) {
		// A bootstrap or import commit stores the harness its collected
		// config.yaml gives. Every caller validated that config.yaml before
		// collection (the schema requires a non-empty harness), so an empty
		// one means the collected file differs from what was validated: fail
		// rather than keep the harness the row was created with. A
		// config.yaml too large to parse (present, no content) keeps the
		// stored harness with the warning below, as before.
		*hc = prevState
		return fmt.Errorf("harness-config commit: the collected config.yaml of %q sets no harness and none can be inferred from the name", hc.Name)
	}
	if idx.Harness != "" {
		hc.Harness = idx.Harness
	} else {
		s.resourceLog.Warn("harness-config commit: config.yaml sets no harness and none can be inferred from the name; keeping the stored harness",
			"harness_config", hc.Name, "id", hc.ID, "harness", hc.Harness)
	}
	hc.Layout = store.HarnessConfigLayoutBlobs
	if hc.StoragePath != base {
		hc.StoragePath = base
		bucket := hc.StorageBucket
		if bucket == "" {
			bucket = stor.Bucket()
		}
		hc.StorageBucket = bucket
		hc.StorageURI = storage.StorageURIForPath(bucket, base)
	}
	if opts.create {
		err = s.store.CreateHarnessConfig(ctx, hc)
	} else {
		err = s.store.UpdateHarnessConfigContent(ctx, hc, expected)
	}
	if err != nil {
		*hc = prevState
		return err
	}

	// 6. A migrated legacy row's old tree.
	if migratedFrom != "" {
		s.removeLegacyHarnessConfigTree(ctx, stor, hc, migratedFrom)
	}
	return nil
}

// readHarnessConfigYAML returns the bytes of the config.yaml the manifest
// next lists, from opts.content, opts.collected or its blob under base, in that
// order. present is false when next has no config.yaml. A file too large to
// parse is returned as present with no content.
func (s *Server) readHarnessConfigYAML(ctx context.Context, stor storage.Storage, base string, next []store.TemplateFile, opts harnessConfigCommitOpts) ([]byte, bool, error) {
	if _, ok := templateFileByPath(next, harnessConfigYAMLPath); !ok {
		return nil, false, nil
	}
	if data, ok := opts.content[harnessConfigYAMLPath]; ok {
		return data, true, nil
	}
	if opts.collected != nil {
		data, present, err := opts.collected.harnessConfigYAML()
		if err != nil {
			return nil, false, err
		}
		if present {
			return data, true, nil
		}
		// Not reached for a commit through commitHarnessConfigFiles, which
		// has already required the collected config.yaml; a caller without
		// one reads storage.
	}
	data, err := readHarnessConfigBlob(ctx, stor, base, next, harnessConfigYAMLPath, maxHarnessConfigFileSize)
	switch {
	case errors.Is(err, errTemplateObjectTooLarge):
		return nil, true, nil
	case errors.Is(err, storage.ErrNotFound):
		return nil, false, &fileNotFoundError{path: harnessConfigYAMLPath}
	case err != nil:
		return nil, false, err
	}
	return data, true, nil
}

// writeHarnessConfigCommitError maps a commitHarnessConfigFiles error to an
// HTTP response.
func writeHarnessConfigCommitError(w http.ResponseWriter, err error) {
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
	var unusable *unusableHarnessConfigError
	if errors.As(err, &unusable) {
		writeUnusableProvisioner(w, unusable.perr)
		return
	}
	if errors.Is(err, errHarnessConfigStorageNotConfigured) {
		RuntimeError(w, "Storage not configured")
		return
	}
	if errors.Is(err, store.ErrHarnessConfigConflict) {
		writeError(w, http.StatusConflict, ErrCodeHarnessConfigConflict,
			"harness config was changed by another commit; re-read it and retry", nil)
		return
	}
	writeErrorFromErr(w, err, "")
}

// effectiveHarnessConfigImage is the image to show and check for hc: the
// image the build executor last built (BuiltImage) when set, otherwise the
// config.yaml image (Config.Image).
func effectiveHarnessConfigImage(hc *store.HarnessConfig) string {
	if hc == nil {
		return ""
	}
	if hc.BuiltImage != "" {
		return hc.BuiltImage
	}
	if hc.Config != nil {
		return hc.Config.Image
	}
	return ""
}
