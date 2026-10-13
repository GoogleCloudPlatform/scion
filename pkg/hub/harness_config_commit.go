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
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
// migration (path) write through the same compare-and-swap
// (UpdateHarnessConfigContent) with the row as read; the image status and
// the build executor's BuiltImage have their own column writers.

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

// deriveHarnessConfigIndexFromDir is the directory variant of
// deriveHarnessConfigIndex (bootstrap, import).
func deriveHarnessConfigIndexFromDir(dir, name string) harnessConfigIndex {
	data, present, err := readHarnessConfigYAMLFromDir(dir)
	if err != nil {
		slog.Warn("harness-config commit: cannot read config.yaml; deriving it as empty",
			"harness_config", name, "dir", dir, "error", err)
		return deriveHarnessConfigIndex(nil, false, name)
	}
	return deriveHarnessConfigIndex(data, present, name)
}

// readHarnessConfigYAMLFromDir reads dir/config.yaml. present is false when
// the file does not exist. A file too large to parse is returned as present
// with no content (derived as unparsable).
func readHarnessConfigYAMLFromDir(dir string) ([]byte, bool, error) {
	fp := filepath.Join(dir, harnessConfigYAMLPath)
	fi, err := os.Stat(fp)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !fi.Mode().IsRegular() {
		return nil, false, nil
	}
	if fi.Size() > maxHarnessConfigFileSize {
		return nil, true, nil
	}
	data, err := os.ReadFile(fp)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
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
	// dir, when set, is a local directory holding the same files as the new
	// manifest (bootstrap, import, reimport). config.yaml is read from it
	// instead of storage.
	dir string
	// content holds file contents the caller already has (the file write
	// and upload handlers), keyed by manifest path; config.yaml is read
	// from it when present.
	content map[string][]byte
	// create persists the harness-config with CreateHarnessConfig instead
	// of UpdateHarnessConfigContent (clone, project clone).
	create bool
	// verifyAll verifies that every manifest object exists (finalize: an
	// old CLI re-uploads everything on "file not found"). Otherwise only
	// entries that are new or changed against the row as read are verified.
	verifyAll bool
	// written lists manifest paths whose objects the caller has just
	// written for this commit, so they are not verified again.
	written map[string]bool
}

// errHarnessConfigStorageNotConfigured is returned when a commit has no
// storage.
var errHarnessConfigStorageNotConfigured = errors.New("storage not configured")

// commitHarnessConfigFiles is the single commit path for a harness-config's
// files. It:
//
//  1. verifies that the manifest's objects exist under hc.StoragePath;
//  2. refuses a config.yaml whose provisioner could never provision an
//     agent (unusableProvisionerInYAML, the one content check);
//  3. computes ContentHash;
//  4. derives Harness and Config from config.yaml (deriveHarnessConfigIndex);
//  5. writes the row once: CreateHarnessConfig with opts.create, otherwise
//     UpdateHarnessConfigContent, a compare-and-swap against the content
//     hash the caller read. A row changed by another commit since then
//     fails with store.ErrHarnessConfigConflict, which the HTTP handlers
//     map to 409 harness_config_conflict;
//  6. after the row write, deletes the objects of files the row as read
//     listed and the new manifest drops (exact paths, never a prefix).
//
// BuiltImage is never written. On any error before the write, hc is left
// unchanged. Other fields the caller set on hc (Status, SourceURL, ...) are
// persisted as they are.
func (s *Server) commitHarnessConfigFiles(ctx context.Context, hc *store.HarnessConfig, next []store.TemplateFile, opts harnessConfigCommitOpts) error {
	stor := s.GetStorage()
	if stor == nil {
		return errHarnessConfigStorageNotConfigured
	}
	next = append([]store.TemplateFile(nil), next...)
	expected := store.HarnessConfigContentPrecondition{ContentHash: hc.ContentHash}

	if err := validateManifestFilePaths(next); err != nil {
		return err
	}
	if hc.StoragePath == "" {
		return fmt.Errorf("harness-config commit: %q has no storage path", hc.Name)
	}

	// 1. Verify the objects exist.
	previous := make(map[string]string, len(hc.Files))
	for _, f := range hc.Files {
		previous[f.Path] = f.Hash
	}
	for _, f := range next {
		if opts.written[f.Path] {
			continue
		}
		if !opts.verifyAll && !opts.create {
			if h, ok := previous[f.Path]; ok && h == f.Hash && h != "" {
				continue
			}
		}
		exists, err := stor.Exists(ctx, hc.StoragePath+"/"+f.Path)
		if err != nil || !exists {
			return &fileNotFoundError{path: f.Path}
		}
	}

	// 2. The content check.
	configYAML, present, err := s.readHarnessConfigYAML(ctx, stor, hc, next, opts)
	if err != nil {
		return fmt.Errorf("harness-config commit: %w", err)
	}
	if present {
		if perr := unusableProvisionerInYAML(hc.Name, configYAML); perr != nil {
			return &unusableHarnessConfigError{perr: perr}
		}
	}

	// 3. Content hash. 4. Derived fields.
	contentHash := computeContentHash(next)
	idx := deriveHarnessConfigIndex(configYAML, present, hc.Name)

	// 5. One row write.
	prevState := *hc
	previousFiles := hc.Files
	hc.Files = next
	hc.ContentHash = contentHash
	hc.Config = idx.Config
	if idx.Harness != "" {
		hc.Harness = idx.Harness
	} else {
		s.resourceLog.Warn("harness-config commit: config.yaml sets no harness and none can be inferred from the name; keeping the stored harness",
			"harness_config", hc.Name, "id", hc.ID, "harness", hc.Harness)
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

	// 6. Objects dropped from the manifest, after the row no longer lists
	// them.
	if !opts.create {
		s.deleteRemovedHarnessConfigFiles(ctx, stor, hc, previousFiles)
	}
	return nil
}

// readHarnessConfigYAML returns the bytes of the config.yaml the manifest
// next lists, from opts.content, opts.dir or storage, in that order. present
// is false when next has no config.yaml. A file too large to parse is
// returned as present with no content.
func (s *Server) readHarnessConfigYAML(ctx context.Context, stor storage.Storage, hc *store.HarnessConfig, next []store.TemplateFile, opts harnessConfigCommitOpts) ([]byte, bool, error) {
	if _, ok := templateFileByPath(next, harnessConfigYAMLPath); !ok {
		return nil, false, nil
	}
	if data, ok := opts.content[harnessConfigYAMLPath]; ok {
		return data, true, nil
	}
	if opts.dir != "" {
		data, present, err := readHarnessConfigYAMLFromDir(opts.dir)
		if err != nil {
			return nil, false, err
		}
		if present {
			return data, true, nil
		}
		// The directory lacks a file the manifest lists: read storage.
	}
	reader, _, err := stor.Download(ctx, hc.StoragePath+"/"+harnessConfigYAMLPath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, false, &fileNotFoundError{path: harnessConfigYAMLPath}
		}
		return nil, false, fmt.Errorf("read %s: %w", harnessConfigYAMLPath, err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, maxHarnessConfigFileSize+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", harnessConfigYAMLPath, err)
	}
	if int64(len(data)) > maxHarnessConfigFileSize {
		return nil, true, nil
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
