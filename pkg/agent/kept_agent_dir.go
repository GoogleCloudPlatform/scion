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

package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Kept stale agent directories (ptone/scion#4742).
//
// handleAgentDirWithoutConfig keeps an agent directory that has no config
// file when its workspace may hold work, or when that cannot be checked.
// Such a directory is treated like a soft-deleted agent: a marker file in it
// records when it was first kept and why, and once the soft-delete retention
// window has passed it is removed by the next GetAgent in the same agents
// root (for the agent itself, or by a sweep of the root). Removing it after
// the window is expected, not a failure.
//
// The removal does not inspect the workspace: it never runs git in it and
// never calls DeleteAgentFiles. It uses removeAgentDirAndPruneWorktrees, as
// the existing stale-directory removal does.

// keptAgentDirMarker is the marker file written into a kept agent directory.
const keptAgentDirMarker = "kept-agent-dir.json"

// keptAgentDirMarkerMaxSize bounds how much of a marker file is read.
const keptAgentDirMarkerMaxSize = 4096

// keptAgentDirPurgePrefix starts the name a kept directory is renamed to
// while it is removed.
const keptAgentDirPurgePrefix = ".kept-agent-dir-purge-"

// Reasons recorded in the marker.
const (
	keptReasonWorkspaceMayHoldWork = "workspace_may_hold_work"
	keptReasonWorkspaceCheckFailed = "workspace_check_failed"
)

// keptAgentDirDefaultRetention is the window used when
// server.hub.soft_delete_retention is unset (or invalid). Zero keeps kept
// directories forever, as before ptone/scion#4742; it mirrors the hub,
// whose purge skips soft-deleted agents while the retention is 0.
const keptAgentDirDefaultRetention time.Duration = 0

// keptAgentDirRetentionEnv is the environment form of
// server.hub.soft_delete_retention; it takes precedence over the settings
// file, as it does for the server.
const keptAgentDirRetentionEnv = "SCION_SERVER_HUB_SOFTDELETERETENTION"

// keptAgentDirRecord is the content of the marker file.
type keptAgentDirRecord struct {
	KeptAt time.Time `json:"keptAt"`
	Reason string    `json:"reason"`
}

// keptAgentDirNow and keptAgentDirRetention are variables so tests can
// replace them.
var (
	keptAgentDirNow       = time.Now
	keptAgentDirRetention = resolveKeptAgentDirRetention
	// keptAgentDirBeforeRecheck runs after a kept directory is renamed for
	// removal and before it is checked again.
	keptAgentDirBeforeRecheck = func(tomb string) {}
)

// resolveKeptAgentDirRetention returns the soft-delete retention window as
// this process's own settings resolve it: the environment variable, else
// server.hub.soft_delete_retention in the global settings file. A live
// change made through the hub's admin settings is not seen here.
func resolveKeptAgentDirRetention() time.Duration {
	raw := strings.TrimSpace(os.Getenv(keptAgentDirRetentionEnv))
	if raw == "" {
		if gs, _, err := config.LoadGlobalSettings(); err == nil && gs != nil && gs.Server != nil && gs.Server.Hub != nil {
			raw = strings.TrimSpace(gs.Server.Hub.SoftDeleteRetention)
		}
	}
	if raw == "" {
		return keptAgentDirDefaultRetention
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		slog.Warn("invalid soft_delete_retention; kept agent directories use the default window",
			"value", raw, "error", err, "default", keptAgentDirDefaultRetention)
		return keptAgentDirDefaultRetention
	}
	return d
}

// recordKeptAgentDir writes the marker into agentDir unless one (or any
// entry with that name) is already there: the first keep sets keptAt, so
// retries do not extend the window. Failures are logged and otherwise
// ignored; the directory is kept either way.
func recordKeptAgentDir(agentName, agentDir, reason string) {
	path := filepath.Join(agentDir, keptAgentDirMarker)
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return
	}
	data, err := json.Marshal(keptAgentDirRecord{KeptAt: keptAgentDirNow().UTC(), Reason: reason})
	if err != nil {
		return
	}
	// O_EXCL: never write through an entry created since the check.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		slog.Warn("GetAgent: could not record the kept agent directory",
			"agent", agentName, "agent_dir", agentDir, "error", err)
		return
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(path)
		slog.Warn("GetAgent: could not record the kept agent directory",
			"agent", agentName, "agent_dir", agentDir, "error", errors.Join(werr, cerr))
	}
}

// readKeptAgentDirRecord reads the marker in agentDir. It reports false
// when there is none, it is not a regular file, or it cannot be parsed.
func readKeptAgentDirRecord(agentDir string) (keptAgentDirRecord, bool) {
	path := filepath.Join(agentDir, keptAgentDirMarker)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return keptAgentDirRecord{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return keptAgentDirRecord{}, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, keptAgentDirMarkerMaxSize+1))
	if err != nil || len(data) > keptAgentDirMarkerMaxSize {
		return keptAgentDirRecord{}, false
	}
	var rec keptAgentDirRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.KeptAt.IsZero() {
		return keptAgentDirRecord{}, false
	}
	return rec, true
}

// hasKeptAgentDirMarker reports whether agentDir holds a regular marker
// file, without reading it.
func hasKeptAgentDirMarker(agentDir string) bool {
	info, err := os.Lstat(filepath.Join(agentDir, keptAgentDirMarker))
	return err == nil && info.Mode().IsRegular()
}

// keptAgentDirExpired reports whether agentDir is a kept directory whose
// window has passed: it has no config file and a valid marker at least
// retention old. A retention of 0 never expires.
func keptAgentDirExpired(agentDir string, retention time.Duration, now time.Time) (keptAgentDirRecord, bool) {
	if retention <= 0 {
		return keptAgentDirRecord{}, false
	}
	if config.GetScionAgentConfigPath(agentDir) != "" {
		return keptAgentDirRecord{}, false
	}
	rec, ok := readKeptAgentDirRecord(agentDir)
	if !ok || now.Sub(rec.KeptAt) < retention {
		return rec, false
	}
	return rec, true
}

// keptAgentHomesEmpty reports whether the agent's homes hold nothing: the
// in-dir home under agentDir and, when it lies outside agentDir, agentHome.
// A home that cannot be read counts as holding data. A kept directory is
// only ever recorded with empty homes; one that has home data since is left
// to handleAgentDirWithoutConfig's home rules.
func keptAgentHomesEmpty(agentDir, agentHome string) bool {
	homes := []string{filepath.Join(agentDir, "home")}
	if agentHome != "" && !pathWithin(agentHome, agentDir) {
		homes = append(homes, agentHome)
	}
	for _, h := range homes {
		if has, err := dirHasEntries(h); err != nil || has {
			return false
		}
	}
	return true
}

// purgeExpiredKeptAgentDir removes agentDir when keptAgentDirExpired holds
// for it and its homes are empty, and reports whether it did. agentHome is
// the agent's home path (it may lie outside agentDir; it is checked, not
// removed). The directory is first renamed to a unique name in the same
// parent and checked again there, so a directory that another process
// re-provisioned under the same name in the meantime (it has no marker) is
// put back instead of removed.
func purgeExpiredKeptAgentDir(agentName, agentDir, agentHome string, retention time.Duration) bool {
	now := keptAgentDirNow()
	if _, ok := keptAgentDirExpired(agentDir, retention, now); !ok || !keptAgentHomesEmpty(agentDir, agentHome) {
		return false
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return false
	}
	tomb := filepath.Join(filepath.Dir(agentDir), keptAgentDirPurgePrefix+hex.EncodeToString(suffix))
	if err := os.Rename(agentDir, tomb); err != nil {
		// Gone already, or not ours to move: leave it.
		return false
	}
	keptAgentDirBeforeRecheck(tomb)
	// An in-dir home moved with the directory; an external one did not.
	extHome := agentHome
	if extHome != "" && pathWithin(extHome, agentDir) {
		extHome = ""
	}
	rec, ok := keptAgentDirExpired(tomb, retention, now)
	if ok {
		ok = keptAgentHomesEmpty(tomb, extHome)
	}
	if !ok {
		if err := os.Rename(tomb, agentDir); err != nil {
			slog.Warn("GetAgent: could not restore an agent directory moved for a kept-directory check",
				"agent", agentName, "agent_dir", agentDir, "moved_to", tomb, "error", err)
		}
		return false
	}
	removeAgentDirAndPruneWorktrees(tomb, filepath.Join(tomb, "workspace"), true)
	slog.Info("GetAgent: removed kept agent directory after the soft-delete retention window",
		"agent", agentName, "agent_dir", agentDir, "kept_at", rec.KeptAt,
		"reason", rec.Reason, "retention", retention)
	return true
}

// sweepExpiredKeptAgentDirs removes the expired kept directories in
// agentsRoot other than skipName's (GetAgent handles that one itself).
// homeOf returns an agent's home path. Only plain directories holding a
// marker are considered; the retention is resolved only when one is found.
func sweepExpiredKeptAgentDirs(agentsRoot, skipName string, homeOf func(name string) string) {
	entries, err := os.ReadDir(agentsRoot)
	if err != nil {
		return
	}
	var candidates []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == skipName {
			continue
		}
		if hasKeptAgentDirMarker(filepath.Join(agentsRoot, e.Name())) {
			candidates = append(candidates, e.Name())
		}
	}
	if len(candidates) == 0 {
		return
	}
	retention := keptAgentDirRetention()
	if retention <= 0 {
		return
	}
	for _, name := range candidates {
		purgeExpiredKeptAgentDir(name, filepath.Join(agentsRoot, name), homeOf(name), retention)
	}
}
