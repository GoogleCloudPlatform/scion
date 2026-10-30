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

package util

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// maxSeedConfigLayerSize caps how much of one repository config file the seed
// check reads.
const maxSeedConfigLayerSize = 1 << 20 // 1 MiB

// Fixed labels for the config layers an agent's git reads raw (bound read-only
// from the host, never rewritten). They name the layer in a refusal and are
// never built from a path.
const (
	seedLayerCommonWorktree = "config.worktree"
	seedLayerOwnWorktree    = "the worktree's config.worktree"
	// seedLayerCombined labels a cross-check over .git/config and the
	// config.worktree layers above, where the differing key may come from any
	// of them.
	seedLayerCombined = ".git/config or a config.worktree"
)

// CheckSeedRepoConfigLayers checks, before an agent is seeded, the repository
// config files the agent's git reads besides its seeded copy of .git/config:
// <commonDir>/config.worktree and, when adminName is set (the broker-trusted
// name from TrustedWorktreeAdminName), <commonDir>/worktrees/<adminName>/
// config.worktree. These layers are bound raw, so nothing is redacted from
// them: every key in them is checked with the seed rules (seedUnpinnedRefuse
// applies) and NO exemption, so a key the redactor would drop refuses too. They
// are checked whether or not extensions.worktreeConfig is set; an absent file
// is empty.
//
// <commonDir>/config is checked again here with the seed rules and the
// isDroppedKey exemption, exactly as PrepareSeededGitConfig checks it, so no
// include directive in it is followed by the git call below. Then the parsed
// entries of all files are cross-checked against git's own listing
// (crossCheckAgainstGit), run with GIT_DIR and GIT_COMMON_DIR set explicitly so
// git never resolves a work-tree gitfile, and with no global or system config.
//
// It returns a *RepoConfigRefusedError (Layer names the file) for the first
// refused key, and an error for anything it cannot read or parse (fail closed).
func CheckSeedRepoConfigLayers(ctx context.Context, commonDir, adminName string) error {
	return checkSeedRepoConfigLayers(ctx, commonDir, adminName, readSeedConfigLayer)
}

// checkSeedRepoConfigLayers is CheckSeedRepoConfigLayers with the layer reader
// passed in, so a test can make the parsed set differ from what git reads and
// see the cross-check refuse.
func checkSeedRepoConfigLayers(ctx context.Context, commonDir, adminName string, read func(path, label string) ([]gitConfigVar, error)) error {
	if err := refuseRootCommonDirRedirect(filepath.Dir(commonDir), true, AudienceRemote); err != nil {
		return err
	}
	type layer struct {
		path   string
		label  string
		exempt func(gitConfigVar) bool
	}
	layers := []layer{
		{filepath.Join(commonDir, "config"), "", isDroppedKey},
		{filepath.Join(commonDir, "config.worktree"), seedLayerCommonWorktree, nil},
	}
	gitDir := commonDir
	if adminName != "" {
		gitDir = filepath.Join(commonDir, "worktrees", adminName)
		layers = append(layers, layer{filepath.Join(gitDir, "config.worktree"), seedLayerOwnWorktree, nil})
	}
	var parsed []gitConfigVar
	for _, l := range layers {
		vars, err := read(l.path, l.label)
		if err != nil {
			return err
		}
		if err := checkRepoConfigVars("", l.label, vars, RepoPolicyLinked, repoConfigAllowOptions{seed: true}, l.exempt); err != nil {
			return err
		}
		parsed = append(parsed, vars...)
	}
	return crossCheckAgainstGit(ctx, commonDir, seedGitEnv(gitDir, commonDir), parsed, seedLayerCombined, true, AudienceRemote)
}

// readSeedConfigLayer reads one config file as data (no symlink followed, no
// FIFO blocked) and parses it. An absent file is empty. label names the layer
// in errors ("" is the repository's .git/config).
func readSeedConfigLayer(path, label string) ([]gitConfigVar, error) {
	name := label
	if name == "" {
		name = ".git/config"
	}
	data, err := readSmallRegularFile(path, maxSeedConfigLayerSize)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot read the repository's %s to check it (refusing): %w", name, err)
	}
	vars, err := parseGitConfigFile(data)
	if err != nil {
		return nil, fmt.Errorf("cannot parse the repository's %s (refusing): %w", name, err)
	}
	return vars, nil
}
