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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// MoveBundleHomeRecordFile is the broker-owned record written in <agentDir>
// (outside container-writable home/ and workspace/) when an agent starts. It
// captures the resolved in-container UnixUsername, absolute home path, and
// canonical home-relative secret/auth exclusion paths that start actually used,
// so move-bundle precheck and export never guess the user or miss injected
// secret/auth targets when scion-agent.json omits "user".
const MoveBundleHomeRecordFile = ".scion-move-bundle-home.json"

const (
	moveBundleHomeRecordVersion  = 1
	maxMoveBundleHomeRecordBytes = 1 << 20 // 1 MiB
)

var (
	// ErrMoveBundleHomeRecordMissing is returned when <agentDir> has no
	// MoveBundleHomeRecordFile (e.g. an agent started before this feature).
	ErrMoveBundleHomeRecordMissing = errors.New("move-bundle home record is missing")
	// ErrMoveBundleHomeRecordInvalid is returned when the record cannot be
	// decoded or lacks required fields (unixUsername, homeAbs).
	ErrMoveBundleHomeRecordInvalid = errors.New("move-bundle home record is invalid")
)

// MoveBundleHomeRecord is the persisted broker-owned record of an agent's
// resolved container home/workspace identity and staged secret/auth exclusions.
type MoveBundleHomeRecord struct {
	Version           int      `json:"version"`
	UnixUsername      string   `json:"unixUsername"`
	HomeAbs           string   `json:"homeAbs"`
	HomeExcludes      []string `json:"homeExcludes"`
	WorkspaceAbs      string   `json:"workspaceAbs,omitempty"`
	WorkspaceExcludes []string `json:"workspaceExcludes,omitempty"`
}

// ExpandAbsRelTarget resolves an absolute container secret/auth target path
// under rootAbs (such as "/workspace/.env" under "/workspace") into a canonical
// slash-separated path relative to rootAbs. Returns ("", false) when target is
// not an absolute path strictly inside rootAbs, or rootAbs is empty/root.
func ExpandAbsRelTarget(target, rootAbs string) (string, bool) {
	target = strings.TrimSpace(target)
	if !strings.HasPrefix(target, "/") {
		return "", false
	}
	cleanRoot := path.Clean(strings.TrimSpace(rootAbs))
	if !strings.HasPrefix(cleanRoot, "/") || cleanRoot == "/" {
		return "", false
	}
	abs := path.Clean(target)
	prefix := cleanRoot + "/"
	if !strings.HasPrefix(abs, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(abs, prefix)
	if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// ExpandHomeRelTarget resolves a container secret/auth target path (either
// "~/..." or an absolute path under homeAbs) into a canonical slash-separated
// path relative to homeAbs. Returns ("", false) when target is outside homeAbs,
// empty, or invalid. Unlike placedInK8sHome, this expands "~/" directly
// against homeAbs without requiring a separate username lookup.
func ExpandHomeRelTarget(target, homeAbs string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	cleanHome := path.Clean(homeAbs)
	if !strings.HasPrefix(cleanHome, "/") || cleanHome == "/" || cleanHome == "/home" {
		return "", false
	}
	var abs string
	switch {
	case target == "~":
		return "", false
	case strings.HasPrefix(target, "~/"):
		abs = path.Join(cleanHome, strings.TrimPrefix(target, "~/"))
	case strings.HasPrefix(target, "/"):
		abs = path.Clean(target)
	default:
		return "", false
	}
	prefix := cleanHome + "/"
	if !strings.HasPrefix(abs, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(abs, prefix)
	if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// BuildMoveBundleHomeRecord constructs the broker-owned home record from the
// resolved RunConfig that Start is about to launch.
func BuildMoveBundleHomeRecord(runCfg runtime.RunConfig) (*MoveBundleHomeRecord, error) {
	user := strings.TrimSpace(runCfg.UnixUsername)
	if user == "" {
		return nil, fmt.Errorf("%w: empty unixUsername", ErrMoveBundleHomeRecordInvalid)
	}
	homeAbs := path.Clean(util.GetHomeDir(user))
	if !strings.HasPrefix(homeAbs, "/") || homeAbs == "/" || homeAbs == "/home" {
		return nil, fmt.Errorf("%w: invalid resolved homeAbs %q", ErrMoveBundleHomeRecordInvalid, homeAbs)
	}

	wsAbs := path.Clean(strings.TrimSpace(runCfg.ContainerWorkspace))
	if !strings.HasPrefix(wsAbs, "/") || wsAbs == "/" {
		wsAbs = "/workspace"
	}

	seen := make(map[string]struct{})
	add := func(rel string) {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			return
		}
		seen[rel] = struct{}{}
	}
	seenWS := make(map[string]struct{})
	addWS := func(rel string) {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			return
		}
		seenWS[rel] = struct{}{}
	}

	for _, rel := range runtime.PortableHomeExcludes(runCfg) {
		add(rel)
	}
	// Independently expand every file-type ResolvedSecret and ResolvedAuth.Files
	// target against both homeAbs and wsAbs so nothing can be silently dropped.
	for _, sec := range runCfg.ResolvedSecrets {
		if sec.Type == "file" && sec.Target != "" {
			if rel, ok := ExpandHomeRelTarget(sec.Target, homeAbs); ok {
				add(rel)
			}
			if rel, ok := ExpandAbsRelTarget(sec.Target, wsAbs); ok {
				addWS(rel)
			}
		}
	}
	if runCfg.ResolvedAuth != nil {
		for _, f := range runCfg.ResolvedAuth.Files {
			if f.SourcePath != "" && f.ContainerPath != "" {
				if rel, ok := ExpandHomeRelTarget(f.ContainerPath, homeAbs); ok {
					add(rel)
				}
				if rel, ok := ExpandAbsRelTarget(f.ContainerPath, wsAbs); ok {
					addWS(rel)
				}
			}
		}
	}

	excludes := make([]string, 0, len(seen))
	for rel := range seen {
		excludes = append(excludes, rel)
	}
	slices.Sort(excludes)

	wsExcludes := make([]string, 0, len(seenWS))
	for rel := range seenWS {
		wsExcludes = append(wsExcludes, rel)
	}
	slices.Sort(wsExcludes)

	return &MoveBundleHomeRecord{
		Version:           moveBundleHomeRecordVersion,
		UnixUsername:      user,
		HomeAbs:           homeAbs,
		HomeExcludes:      excludes,
		WorkspaceAbs:      wsAbs,
		WorkspaceExcludes: wsExcludes,
	}, nil
}

// WriteMoveBundleHomeRecord writes <agentDir>/.scion-move-bundle-home.json
// using privatefs so it has owner-only permissions/DACL and is never written
// through a symlink. Any existing record is removed first so a failed write
// never leaves a stale record behind (fail closed).
func WriteMoveBundleHomeRecord(agentDir string, runCfg runtime.RunConfig) error {
	// Best-effort remove any existing record first so a build or write error
	// never leaves a stale record on disk.
	if root, rmOpenErr := os.OpenRoot(agentDir); rmOpenErr == nil {
		_ = root.Remove(MoveBundleHomeRecordFile)
		_ = root.Close()
	}

	rec, err := BuildMoveBundleHomeRecord(runCfg)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal move-bundle home record: %w", err)
	}
	data = append(data, '\n')

	tree, err := privatefs.OpenTree(agentDir, privatefs.TreeOptions{})
	if err != nil {
		return fmt.Errorf("open agent dir for move-bundle home record: %w", err)
	}
	defer func() { _ = tree.Close() }()
	if err := tree.WriteFile(MoveBundleHomeRecordFile, data, privatefs.Private); err != nil {
		return fmt.Errorf("write move-bundle home record: %w", err)
	}
	return nil
}

// MoveBundleAgentDirs holds the resolved host paths for an agent's bundle
// roots, computed with the same helpers Provision and Start use.
type MoveBundleAgentDirs struct {
	AgentDir        string
	WorkspaceDir    string
	HomeDir         string
	ExportAgentDir  string
	SharedWorkspace bool
}

// ResolveMoveBundleAgentDirs resolves an agent's state, workspace, and home
// directories using the same provisionAgentStateDir and
// sharedLayoutAgentWorkspace helpers that Provision and Start use (including
// HubProjectID split-storage resolution). When the agent's workspace lives on
// a shared project storage layout, SharedWorkspace is reported true so Phase 1
// precheck/export can refuse it cleanly.
func ResolveMoveBundleAgentDirs(ctx context.Context, runtimeName string, opts api.StartOptions) (MoveBundleAgentDirs, error) {
	projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
	if err != nil {
		return MoveBundleAgentDirs{}, fmt.Errorf("resolve project dir: %w", err)
	}
	if _, err := checkAgentDirContained(projectDir, opts.Name, false); err != nil {
		return MoveBundleAgentDirs{}, fmt.Errorf("agent dir containment: %w", err)
	}
	ctx = contextWithAgentRuntime(ctx, runtimeName)
	agentDir, err := provisionAgentStateDir(ctx, opts)
	if err != nil {
		return MoveBundleAgentDirs{}, fmt.Errorf("resolve agent state dir: %w", err)
	}
	sharedWS, err := sharedLayoutAgentWorkspace(ctx, nil, projectDir, opts.Name, agentDir)
	if err != nil {
		return MoveBundleAgentDirs{}, err
	}
	homeDir := config.GetAgentHomePath(projectDir, opts.Name)
	if agentDir != config.GetAgentDir(projectDir, opts.Name, false) {
		homeDir = filepath.Join(agentDir, "home")
	}
	return MoveBundleAgentDirs{
		AgentDir:        agentDir,
		WorkspaceDir:    filepath.Join(agentDir, "workspace"),
		HomeDir:         homeDir,
		SharedWorkspace: sharedWS != "",
	}, nil
}

// ReadMoveBundleHomeRecord reads and validates
// <agentDir>/.scion-move-bundle-home.json via privatefs. Returns
// ErrMoveBundleHomeRecordMissing if the file does not exist, or
// ErrMoveBundleHomeRecordInvalid if it is malformed or incomplete.
func ReadMoveBundleHomeRecord(agentDir string) (*MoveBundleHomeRecord, error) {
	tree, err := privatefs.OpenTree(agentDir, privatefs.TreeOptions{})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrMoveBundleHomeRecordMissing
		}
		return nil, fmt.Errorf("%w: open agent dir: %v", ErrMoveBundleHomeRecordInvalid, err)
	}
	defer func() { _ = tree.Close() }()

	data, err := tree.ReadPrivate(MoveBundleHomeRecordFile, maxMoveBundleHomeRecordBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrMoveBundleHomeRecordMissing
		}
		return nil, fmt.Errorf("%w: read record: %v", ErrMoveBundleHomeRecordInvalid, err)
	}

	var rec MoveBundleHomeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%w: decode record: %v", ErrMoveBundleHomeRecordInvalid, err)
	}
	if rec.Version != moveBundleHomeRecordVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrMoveBundleHomeRecordInvalid, rec.Version)
	}
	rec.UnixUsername = strings.TrimSpace(rec.UnixUsername)
	rec.HomeAbs = path.Clean(strings.TrimSpace(rec.HomeAbs))
	if rec.UnixUsername == "" || !strings.HasPrefix(rec.HomeAbs, "/") || rec.HomeAbs == "/" || rec.HomeAbs == "/home" {
		return nil, fmt.Errorf("%w: missing or invalid unixUsername/homeAbs", ErrMoveBundleHomeRecordInvalid)
	}
	rec.WorkspaceAbs = path.Clean(strings.TrimSpace(rec.WorkspaceAbs))
	if !strings.HasPrefix(rec.WorkspaceAbs, "/") || rec.WorkspaceAbs == "/" {
		rec.WorkspaceAbs = "/workspace"
	}
	return &rec, nil
}
