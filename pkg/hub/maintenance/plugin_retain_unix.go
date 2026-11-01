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

//go:build unix

package maintenance

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var _ PluginRetainer = (*symlinkInstaller)(nil)

// ActiveRelease implements PluginRetainer: the release directory current
// points to, "" when current is not a valid release link.
func (s *symlinkInstaller) ActiveRelease() string {
	target, err := s.checkReleaseLink(currentLinkName)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// RetainPlugins implements PluginRetainer: it writes copies of bins to
// releases/<release>/plugins.partial/ and renames that directory over
// releases/<release>/plugins/. The release must be a directory directly
// under releases/.
func (s *symlinkInstaller) RetainPlugins(release string, bins map[string]string) error {
	if !releaseDirRE.MatchString(release) || strings.HasSuffix(release, partialSuffix) {
		return fmt.Errorf("invalid release name %q", release)
	}
	if err := s.checkReleaseTarget("release", filepath.Join(releasesDirName, release)); err != nil {
		return err
	}
	final := filepath.Join(s.releasesDir(), release, pluginsSubdir)
	partial := final + partialSuffix
	if err := os.RemoveAll(partial); err != nil {
		return fmt.Errorf("remove stale %s: %w", partial, err)
	}
	if err := os.Mkdir(partial, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", partial, err)
	}
	files := make([]string, 0, len(bins))
	for f := range bins {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		if name, ok := strings.CutPrefix(f, PluginBinaryPrefix); !ok || !pluginNameRE.MatchString(name) {
			_ = os.RemoveAll(partial)
			return fmt.Errorf("invalid plugin file name %q", f)
		}
		if err := copyExecutable(bins[f], filepath.Join(partial, f)); err != nil {
			_ = os.RemoveAll(partial)
			return err
		}
	}
	if err := os.RemoveAll(final); err != nil {
		_ = os.RemoveAll(partial)
		return fmt.Errorf("remove old %s: %w", final, err)
	}
	if err := os.Rename(partial, final); err != nil {
		_ = os.RemoveAll(partial)
		return fmt.Errorf("rename %s into place: %w", partial, err)
	}
	return nil
}
