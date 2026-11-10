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

package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Plugin lockstep (design D6, ptone/scion#6009): an update also installs,
// from the same release and verified by the same SHA256SUMS, the broker
// plugins installed in the plugin directory (~/.scion/plugins/broker).
// Plugins are go-plugin child processes of the hub, so the hub's restart
// restarts them on the new binaries.
//
// Only regular executable files named scion-plugin-<name> directly in the
// plugin directory are managed; that is how deploy.sh installs them
// (install -m 0755). Symlinks are left alone with a warning, and so are
// plugins configured with an explicit path elsewhere (they are not in the
// directory).

// PluginBinaryPrefix is the file name prefix of broker plugin binaries
// (pkg/plugin.PluginBinaryPrefix).
const PluginBinaryPrefix = "scion-plugin-"

// pluginStagingPrefix names the hidden staging directory an update creates
// in the plugin directory: on the same filesystem, so each plugin is
// replaced with one rename. Plugin discovery skips directories.
const pluginStagingPrefix = ".scion-update-"

// pluginStagingAppliedMarker is the file a plugin swap writes into its
// staging directory once the new builds are installed. The directory then
// holds only old/, the previous builds, which an in-process rollback needs
// until the restart; after the restart nothing uses it (a boot-failure
// rollback restores plugins from releases/<tag>/plugins). A marked staging
// directory is removed at the next start (RemoveAppliedPluginStaging), and
// quietly by the next update. A restore removes the marker first, so a
// directory kept by a failed restore (the only copies of old builds) is
// never taken for a finished update (ptone/scion#7177).
const pluginStagingAppliedMarker = "applied"

// pluginKeptPrefix names a staging directory moved aside because it holds
// the only copies of old plugin builds and its applied marker could not be
// removed. It is outside pluginStagingPrefix, so neither the startup cleanup
// nor the next update removes it; the operator does.
const pluginKeptPrefix = ".scion-plugins-kept-"

// pluginsSubdir is where a retained release keeps the plugin binaries that
// were installed with it: <install_root>/releases/<tag>/plugins/.
const pluginsSubdir = "plugins"

// pluginNameRE limits the <name> of scion-plugin-<name> to one safe path
// element that also yields a well-formed asset name.
var pluginNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// PluginAssetName is the release asset holding plugin name for this
// architecture (build-release.yml build-plugins).
func PluginAssetName(name string) string {
	return fmt.Sprintf("%s%s-linux-%s.tar.gz", PluginBinaryPrefix, name, runtime.GOARCH)
}

// PluginRetainer is implemented by installers that keep release
// directories (symlink). The update-binary executor copies the plugin
// binaries installed with each release into releases/<tag>/plugins/, so a
// rollback can restore them (see ReconcilePluginsAfterRollback). Installers
// without it skip retention.
type PluginRetainer interface {
	// ActiveRelease returns the release directory name current points to,
	// or "" when there is none.
	ActiveRelease() string
	// RetainPlugins replaces releases/<release>/plugins/ with copies of
	// bins (plugin file name -> source path).
	RetainPlugins(release string, bins map[string]string) error
}

// installedPlugin is a managed plugin binary in the plugin directory.
type installedPlugin struct {
	file string // scion-plugin-<name>
	name string // <name>
	path string // <dir>/<file>
}

// listInstalledPlugins returns the managed plugins in dir (sorted by file
// name) and a warning for each scion-plugin-* entry that is left alone. A
// missing dir has no plugins.
func listInstalledPlugins(dir string) ([]installedPlugin, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var plugins []installedPlugin
	var warnings []string
	for _, e := range entries {
		file := e.Name()
		name, ok := strings.CutPrefix(file, PluginBinaryPrefix)
		if !ok || name == "" {
			continue
		}
		p := filepath.Join(dir, file)
		switch {
		case e.Type()&os.ModeSymlink != 0:
			warnings = append(warnings, fmt.Sprintf("%s is a symlink; it is not updated with the hub", p))
			continue
		case !e.Type().IsRegular():
			continue
		}
		info, err := e.Info()
		if err != nil || info.Mode()&0o111 == 0 {
			continue // plugin discovery ignores it too
		}
		if !pluginNameRE.MatchString(name) {
			warnings = append(warnings, fmt.Sprintf("%s has an unexpected name; it is not updated with the hub", p))
			continue
		}
		plugins = append(plugins, installedPlugin{file: file, name: name, path: p})
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].file < plugins[j].file })
	return plugins, warnings, nil
}

// pluginUpdate is one plugin of a lockstep plan.
type pluginUpdate struct {
	installedPlugin
	// bin is the verified, extracted binary of the target release, or ""
	// when the release has no asset for it and it is kept.
	bin string
}

// pluginPlan is the outcome of the download phase for plugins.
type pluginPlan struct {
	dir     string
	plugins []pluginUpdate
}

// replacements returns the plugins that get a new binary.
func (p *pluginPlan) replacements() []pluginUpdate {
	if p == nil {
		return nil
	}
	var out []pluginUpdate
	for _, u := range p.plugins {
		if u.bin != "" {
			out = append(out, u)
		}
	}
	return out
}

// pluginWarn writes a lockstep warning to the run log and the hub log.
func pluginWarn(logger io.Writer, msg string) {
	logging.Subsystem("hub.maintenance.update-binary").Warn("Plugin lockstep", "warning", msg)
	_, _ = fmt.Fprintf(logger, "WARNING: %s\n", msg)
}

// hasInstalledPlugins reports whether lockstep has plugins to manage.
func (e *BinaryUpdateExecutor) hasInstalledPlugins() bool {
	if e.pluginDir == "" {
		return false
	}
	plugins, _, err := listInstalledPlugins(e.pluginDir)
	return err == nil && len(plugins) > 0
}

// pluginDirSkipReason returns why lockstep cannot replace plugins in dir,
// or "" when it can: a plugin directory that is itself a symlink (the
// rollback reconcile refuses one too, so lockstep would not survive a
// rollback there) or one the hub cannot write.
func pluginDirSkipReason(dir string) string {
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Sprintf("the plugin directory %s is a symlink", dir)
	}
	if !isWritableDir(dir) {
		return fmt.Sprintf("the plugin directory %s is not writable by the hub", dir)
	}
	return ""
}

// checksumSkip reports whether err from ExpectedChecksum is a missing or
// bad SHA256SUMS line, which skips the plugin (design D2) instead of
// failing the update.
func checksumSkip(err error) bool {
	return errors.Is(err, ErrChecksumMissing) || errors.Is(err, ErrChecksumBad)
}

// pluginRisks returns, for the downgrade risk list, what lockstep cannot
// do for target: plugins the release publishes no asset or no usable
// SHA256SUMS line for, and a plugin directory lockstep skips. The
// SHA256SUMS file is fetched (pinned and capped) only when an installed
// plugin has an asset; if it cannot be fetched, the per-line risks are
// left out (the run checks again and warns).
func (e *BinaryUpdateExecutor) pluginRisks(ctx context.Context, rel *Release, target string) []string {
	if e.pluginDir == "" {
		return nil
	}
	plugins, _, err := listInstalledPlugins(e.pluginDir)
	if err != nil || len(plugins) == 0 {
		return nil
	}
	if reason := pluginDirSkipReason(e.pluginDir); reason != "" {
		return []string{fmt.Sprintf("Broker plugins are not updated (%s), so they stay at their current builds and may fail with %s.",
			reason, target)}
	}
	var risks []string
	var sums []byte
	sumsFetched := false
	for _, p := range plugins {
		assetName := PluginAssetName(p.name)
		if _, ok := rel.Asset(assetName); !ok {
			risks = append(risks, fmt.Sprintf("%s publishes no %s asset: the installed %s stays at its current build and may fail with %s.",
				target, assetName, p.file, target))
			continue
		}
		if !sumsFetched {
			sums, sumsFetched = e.fetchReleaseSums(ctx, rel, target), true
		}
		if sums == nil {
			continue
		}
		if _, err := ExpectedChecksum(sums, assetName); checksumSkip(err) {
			risks = append(risks, fmt.Sprintf("%s has no usable %s line for %s (%v): the installed %s stays at its current build and may fail with %s.",
				target, ChecksumsAssetName, assetName, err, p.file, target))
		}
	}
	return risks
}

// fetchReleaseSums downloads rel's SHA256SUMS through the pinned, capped
// download path; nil when it cannot.
func (e *BinaryUpdateExecutor) fetchReleaseSums(ctx context.Context, rel *Release, target string) []byte {
	asset, ok := rel.Asset(ChecksumsAssetName)
	if !ok || CheckAssetURL(e.repo, target, asset.URL) != nil {
		return nil
	}
	var buf bytes.Buffer
	if _, err := fetchCapped(ctx, downloadClient(e.httpClient), asset.URL, maxChecksumsSize, &buf); err != nil {
		return nil
	}
	return buf.Bytes()
}

// preparePlugins is the plugin part of the download phase: it lists the
// managed plugins, and for each one the release has an asset and a
// SHA256SUMS line for, checks the asset URL pin, downloads it (capped like
// the hub tarball), verifies it against that line and extracts the plugin
// binary into tmpDir.
//
// Kept, with a warning (nothing unverified is installed): a plugin without
// an asset, or whose SHA256SUMS line is missing or bad (design D2). Skipped,
// with a warning: an unreadable, unwritable or symlinked plugin directory.
// An error, so the update fails before anything is replaced: a URL outside
// the release, a failed download, a downloaded hash that differs from its
// line, or a bad archive.
func (e *BinaryUpdateExecutor) preparePlugins(ctx context.Context, logger io.Writer, client *http.Client,
	rel *Release, target string, sums []byte, tmpDir string) (*pluginPlan, error) {
	if e.pluginDir == "" {
		return nil, nil
	}
	plugins, warnings, err := listInstalledPlugins(e.pluginDir)
	if err != nil {
		pluginWarn(logger, fmt.Sprintf("cannot read the plugin directory %s (%v); broker plugins are not updated", e.pluginDir, err))
		return nil, nil
	}
	if len(plugins) == 0 && len(warnings) == 0 {
		return nil, nil
	}
	_, _ = fmt.Fprintf(logger, "\n==> Broker plugins (%s)\n", e.pluginDir)
	for _, w := range warnings {
		pluginWarn(logger, w)
	}
	if len(plugins) == 0 {
		return nil, nil
	}
	if reason := pluginDirSkipReason(e.pluginDir); reason != "" {
		pluginWarn(logger, fmt.Sprintf("%s; broker plugins are not updated and may not work with %s", reason, target))
		return nil, nil
	}

	plan := &pluginPlan{dir: e.pluginDir}
	for _, p := range plugins {
		u := pluginUpdate{installedPlugin: p}
		assetName := PluginAssetName(p.name)
		asset, ok := rel.Asset(assetName)
		if !ok {
			pluginWarn(logger, fmt.Sprintf("release %s has no %s asset; %s is left at its current build and may not work with %s",
				target, assetName, p.file, target))
			plan.plugins = append(plan.plugins, u)
			continue
		}
		if err := CheckAssetURL(e.repo, target, asset.URL); err != nil {
			return nil, fmt.Errorf("release %s: %w", target, err)
		}
		expected, err := ExpectedChecksum(sums, assetName)
		if checksumSkip(err) {
			pluginWarn(logger, fmt.Sprintf("release %s: %v; %s is left at its current build and may not work with %s",
				target, err, p.file, target))
			plan.plugins = append(plan.plugins, u)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checksum verification failed for %s: %w", assetName, err)
		}
		dir := filepath.Join(tmpDir, "plugins", p.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
		_, _ = fmt.Fprintf(logger, "Downloading %s\n", asset.URL)
		tarball := filepath.Join(dir, assetName)
		actual, n, err := downloadHashed(ctx, client, asset.URL, tarball, e.maxBytes)
		if err != nil {
			return nil, fmt.Errorf("download %s failed: %w", assetName, err)
		}
		if actual != expected {
			return nil, fmt.Errorf("checksum mismatch for %q: expected %s, got %s", assetName, expected, actual)
		}
		_, _ = fmt.Fprintf(logger, "Downloaded %d bytes; checksum verified for %s: %s\n", n, assetName, actual)
		bin, err := extractRootEntry(tarball, dir, p.file, logger, DefaultMaxExtractBytes)
		if err != nil {
			return nil, fmt.Errorf("extract %s: %w", assetName, err)
		}
		u.bin = bin
		plan.plugins = append(plan.plugins, u)
	}
	return plan, nil
}

// retainPlugins copies, on installers that keep release directories, the
// plugin binaries installed now into releases/<oldRelease>/plugins/ and
// the ones the plan installs (new or kept) into releases/<newRelease>/plugins/.
// It runs before Activate, so the plugin directory still holds the old
// binaries. Failures are warnings: retention only serves a later rollback.
func retainPlugins(logger io.Writer, inst Installer, plan *pluginPlan, oldRelease, newRelease string) {
	r, ok := inst.(PluginRetainer)
	if !ok || plan == nil || len(plan.plugins) == 0 {
		return
	}
	oldBins := make(map[string]string, len(plan.plugins))
	newBins := make(map[string]string, len(plan.plugins))
	for _, u := range plan.plugins {
		oldBins[u.file] = u.path
		newBins[u.file] = u.path
		if u.bin != "" {
			newBins[u.file] = u.bin
		}
	}
	if oldRelease != "" && oldRelease != newRelease {
		if err := r.RetainPlugins(oldRelease, oldBins); err != nil {
			pluginWarn(logger, fmt.Sprintf("cannot keep the current plugin binaries with release %s: %v", oldRelease, err))
		}
	}
	if err := r.RetainPlugins(newRelease, newBins); err != nil {
		pluginWarn(logger, fmt.Sprintf("cannot keep the plugin binaries with release %s: %v", newRelease, err))
	}
}

// pluginSwap replaces plugin binaries in the plugin directory, one rename
// each, and can put the old ones back. The new binaries and hard links to
// the old ones are kept in a hidden staging directory inside the plugin
// directory until commit.
type pluginSwap struct {
	dir     string
	staging string
	updates []pluginUpdate
	// swapped are the plugins replaced and not yet put back, in order.
	swapped []pluginUpdate
	// keep is set once a restore failed: the staging directory holds the
	// only copies of old binaries and is not removed.
	keep bool
}

// stagePluginSwap removes staging directories left by an interrupted run
// (logged: one may hold old binaries a failed restore left behind), then
// stages the replacements of plan. Nothing in the plugin directory
// changes. It returns nil when there is nothing to replace.
func stagePluginSwap(logger io.Writer, plan *pluginPlan) (*pluginSwap, error) {
	updates := plan.replacements()
	if len(updates) == 0 {
		return nil, nil
	}
	removeStalePluginStaging(logger, plan.dir)
	staging, err := os.MkdirTemp(plan.dir, pluginStagingPrefix)
	if err != nil {
		return nil, fmt.Errorf("create a staging directory in %s: %w", plan.dir, err)
	}
	sw := &pluginSwap{dir: plan.dir, staging: staging, updates: updates}
	for _, sub := range []string{"new", "old"} {
		if err := os.Mkdir(filepath.Join(staging, sub), 0o700); err != nil {
			sw.cleanup()
			return nil, fmt.Errorf("create %s: %w", filepath.Join(staging, sub), err)
		}
	}
	for _, u := range updates {
		if err := copyExecutable(u.bin, sw.newPath(u)); err != nil {
			sw.cleanup()
			return nil, err
		}
		if err := linkOrCopy(u.path, sw.oldPath(u)); err != nil {
			sw.cleanup()
			return nil, fmt.Errorf("back up %s: %w", u.path, err)
		}
	}
	return sw, nil
}

func (sw *pluginSwap) newPath(u pluginUpdate) string { return filepath.Join(sw.staging, "new", u.file) }
func (sw *pluginSwap) oldPath(u pluginUpdate) string { return filepath.Join(sw.staging, "old", u.file) }

// apply renames each new binary over the installed one. On failure the
// plugins already replaced are put back before the error is returned; any
// that cannot be stay in swapped, so a later restore retries them.
func (sw *pluginSwap) apply(logger io.Writer) error {
	for _, u := range sw.updates {
		if err := os.Rename(sw.newPath(u), u.path); err != nil {
			err = fmt.Errorf("install %s: %w", u.path, err)
			if rerr := sw.restore(); rerr != nil {
				return fmt.Errorf("%w; restoring the replaced plugins also failed: %v", err, rerr)
			}
			return err
		}
		sw.swapped = append(sw.swapped, u)
		_, _ = fmt.Fprintf(logger, "Installed %s\n", u.path)
	}
	return nil
}

// restore puts back the old binaries of the plugins still swapped, each
// with one rename, then removes the staging directory. Plugins it cannot
// put back stay swapped, and the staging directory (their old binaries) is
// kept.
func (sw *pluginSwap) restore() error {
	marker := filepath.Join(sw.staging, pluginStagingAppliedMarker)
	markerErr := removePluginStagingMarker(marker)
	var errs []error
	var failed []pluginUpdate
	for i := len(sw.swapped) - 1; i >= 0; i-- {
		u := sw.swapped[i]
		if err := os.Rename(sw.oldPath(u), u.path); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", u.path, err))
			failed = append([]pluginUpdate{u}, failed...)
		}
	}
	sw.swapped = failed
	if len(errs) > 0 {
		sw.keep = true
		// The kept directory holds the only copies of old builds, so it must
		// not stay marked applied: the next start would remove it. Retry a
		// marker removal that failed, and say so if it still fails.
		if markerErr != nil {
			if markerErr = removePluginStagingMarker(marker); markerErr != nil {
				errs = append(errs, sw.moveKeptStaging(markerErr))
			}
		}
		return fmt.Errorf("%w (the old binaries are kept in %s)", errors.Join(errs...), filepath.Join(sw.staging, "old"))
	}
	sw.keep = false
	sw.cleanup()
	return nil
}

// cleanup removes the staging directory, unless a failed restore left old
// binaries in it.
func (sw *pluginSwap) cleanup() {
	if sw.keep {
		return
	}
	_ = os.RemoveAll(sw.staging)
}

// markApplied records that the new builds are installed: it removes the
// now-empty new/ and writes pluginStagingAppliedMarker, keeping old/ for a
// rollback until the restart. Best-effort: a failure only leaves the
// directory unmarked, to be removed with a warning by the next update.
func (sw *pluginSwap) markApplied(logger io.Writer) {
	_ = os.Remove(filepath.Join(sw.staging, "new"))
	marker := filepath.Join(sw.staging, pluginStagingAppliedMarker)
	body := []byte("The new broker plugin builds are installed; old/ holds the previous builds until the hub restarts.\n")
	if err := os.WriteFile(marker, body, 0o644); err != nil {
		pluginWarn(logger, fmt.Sprintf("could not mark %s as applied (%v); the next update removes it", sw.staging, err))
	}
}

// moveKeptStaging is the last resort when a staging directory holding the
// only old builds stays marked applied: it renames the directory off
// pluginStagingPrefix (to pluginKeptPrefix + a UTC timestamp) so no cleanup
// removes it, and returns an error that says where the backups are, or, if
// the rename fails too, that they must be moved aside by hand.
func (sw *pluginSwap) moveKeptStaging(markerErr error) error {
	kept := filepath.Join(sw.dir, pluginKeptPrefix+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(sw.staging, kept); err != nil {
		return fmt.Errorf("%s is still marked applied (%v) and could not be moved aside (%v), so the next hub start would remove these backups: move %s aside before restarting the hub",
			sw.staging, markerErr, err, filepath.Join(sw.staging, "old"))
	}
	sw.staging = kept
	return fmt.Errorf("its applied marker could not be removed (%v), so the backups were moved to %s, which no cleanup removes", markerErr, kept)
}

// removePluginStagingMarker removes an applied marker; a marker that is
// already gone is not an error. A variable so tests can make it fail.
var removePluginStagingMarker = func(marker string) error {
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// isAppliedPluginStaging reports whether p is a staging directory marked by
// markApplied.
func isAppliedPluginStaging(p string) bool {
	fi, err := os.Lstat(filepath.Join(p, pluginStagingAppliedMarker))
	return err == nil && fi.Mode().IsRegular()
}

// removeStalePluginStaging removes staging directories of earlier runs.
// One left by a successful update (marked applied) is removed with a plain
// note; any other with a warning naming it.
func removeStalePluginStaging(logger io.Writer, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), pluginStagingPrefix) {
			p := filepath.Join(dir, e.Name())
			if isAppliedPluginStaging(p) {
				_, _ = fmt.Fprintf(logger, "Removing %s, the previous plugin builds kept by an earlier successful update.\n", p)
			} else {
				pluginWarn(logger, fmt.Sprintf("removing %s, left by an interrupted or failed plugin update (it may hold old plugin binaries under old/)", p))
			}
			_ = os.RemoveAll(p)
		}
	}
}

// RemoveAppliedPluginStaging removes the staging directories in pluginDir
// that a successful update left behind (marked applied), and returns their
// paths. The hub calls it at startup, after the restart that made their
// old/ backups unnecessary. Unmarked staging directories are left for the
// next update, which removes them with a warning. A missing pluginDir is
// not an error. A pluginDir that is itself a symlink is left alone, as the
// rollback reconcile leaves it: lockstep never stages plugins there
// (pluginDirSkipReason), so nothing in its target is this hub's to remove.
func RemoveAppliedPluginStaging(pluginDir string) ([]string, error) {
	fi, err := os.Lstat(pluginDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(pluginDir)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), pluginStagingPrefix) {
			continue
		}
		p := filepath.Join(pluginDir, e.Name())
		if !isAppliedPluginStaging(p) {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, p)
	}
	return removed, errors.Join(errs...)
}

// linkOrCopy hard-links src to dst, or copies it where hard links are not
// supported.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyExecutable(src, dst)
}
