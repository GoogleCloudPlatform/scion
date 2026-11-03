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

package runtimebroker

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// checkRequestVolumes refuses volumes sent by the Hub (a create's config and
// inline config, a start's inline config) that reach a Runtime Broker
// control path, before anything from the request is persisted.
func checkRequestVolumes(volumeSets ...[]api.VolumeMount) error {
	for _, vols := range volumeSets {
		if err := runtime.CheckVolumeSources(vols); err != nil {
			return err
		}
		if err := runtime.CheckWorkspaceVolumeSources(vols); err != nil {
			return err
		}
	}
	return nil
}

// writeControlPathRefusal writes a refused mount source as a 400 with the
// control_path_mount code. The message names the source as requested and
// the class of what it reaches, never a resolved host path.
func writeControlPathRefusal(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadRequest, brokercontrol.ErrorCode, err.Error(), nil)
}

// controlPathsOwner is the brokercontrol.SetExtraPaths owner for one
// server's configured control paths.
func controlPathsOwner(brokerID string) string {
	return "runtimebroker/" + brokerID
}

// registerControlPaths registers this server's control paths that can be
// configured outside the Scion global directory: its state directory, a
// configured credentials file, and a co-located Hub's SQLite database file.
func registerControlPaths(brokerID, stateDir, credentialsPath string) {
	var entries []brokercontrol.Entry
	if stateDir != "" {
		entries = append(entries, brokercontrol.Entry{Path: absOrSelf(stateDir), Kind: brokercontrol.Dir, Class: brokercontrol.ClassHostState})
	}
	if credentialsPath != "" {
		entries = append(entries, brokercontrol.Entry{Path: absOrSelf(credentialsPath), Kind: brokercontrol.File, Class: brokercontrol.ClassCredentials})
	}
	if gc, err := config.LoadGlobalConfig(""); err != nil || gc == nil {
		// Only the default database location in the global directory is
		// protected then; a custom one cannot be identified.
		if err == nil {
			err = errors.New("no configuration was loaded")
		}
		slog.Warn("Runtime Broker control paths: could not load the global configuration; a custom Hub database location is not protected from agent mounts", "error", err)
	} else if p := sqliteDatabaseFile(gc.Database); p != "" {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			entries = append(entries, brokercontrol.Entry{Path: p + suffix, Kind: brokercontrol.File, Class: brokercontrol.ClassHubState})
		}
	}
	brokercontrol.SetExtraPaths(controlPathsOwner(brokerID), entries)
}

// sqliteDatabaseFile returns the database file of a SQLite Hub database
// configuration ("path", "file:path?query"), or "" for another driver or an
// in-memory database.
func sqliteDatabaseFile(db config.DatabaseConfig) string {
	if db.Driver != "sqlite" || db.URL == "" {
		return ""
	}
	p := strings.TrimPrefix(db.URL, "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if p == "" || strings.HasPrefix(p, ":memory:") {
		return ""
	}
	return absOrSelf(p)
}

// absOrSelf makes p absolute against the working directory. If that fails,
// p is returned unchanged; brokercontrol then refuses every mount until the
// path is fixed (fail closed).
func absOrSelf(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// checkLayoutRoots refuses a broker layout root under which agent mounts are
// made or shares are mounted (the worktree base, the NFS mount root) when it
// equals or contains a Runtime Broker control path. The server then refuses
// to start.
func checkLayoutRoots(cfg ServerConfig) error {
	type root struct{ name, path string }
	roots := []root{{"the worktree base directory", cfg.WorktreeBase}}
	if cfg.NFSConfig != nil {
		roots = append(roots, root{"the NFS mount root (nfs.mount_root of the workspace storage settings)", cfg.NFSConfig.MountRoot})
	}
	for _, r := range roots {
		if r.path == "" {
			continue
		}
		if _, err := brokercontrol.CheckNamed(r.path); err != nil {
			var re *brokercontrol.RefusalError
			if errors.As(err, &re) {
				slog.Error("Runtime Broker layout root reaches Runtime Broker control paths", "root", r.name, "detail", re.Detail())
			}
			return fmt.Errorf("%s: %w", r.name, err)
		}
	}
	return nil
}
