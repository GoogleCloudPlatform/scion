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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// lockstepPlugin is a plugin asset of a fake release.
type lockstepPlugin struct {
	name     string
	body     string // the plugin binary inside the asset
	noSum    bool   // SHA256SUMS has no line for the asset
	badSum   bool   // SHA256SUMS has a wrong line for the asset
	emptySum bool   // SHA256SUMS lists the empty-file hash for the asset
	noAsset  bool   // the release does not publish the asset
	entry    string // archive entry name; default scion-plugin-<name>
}

// addLockstepRelease publishes tag like addRelease, plus plugin assets
// (scion-plugin-<name>-linux-<arch>.tar.gz holding scion-plugin-<name>)
// listed in the same SHA256SUMS.
func (f *fakeGitHub) addLockstepRelease(tag, published string, plugins ...lockstepPlugin) {
	f.t.Helper()
	script := fmt.Sprintf("#!/bin/sh\necho '{\"version\":\"%s\"}'\n", tag)
	tarball := createTestTarball(f.t, map[string][]byte{"scion": []byte(script)})
	sum := sha256.Sum256(tarball)
	binAsset := BinaryAssetName()
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), binAsset)
	f.files[downloadPath(tag, binAsset)] = tarball
	assets := []string{
		fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, binAsset, "https://github.com"+downloadPath(tag, binAsset)),
		fmt.Sprintf(`{"name":"SHA256SUMS","browser_download_url":%q}`, "https://github.com"+downloadPath(tag, ChecksumsAssetName)),
	}
	for _, p := range plugins {
		if p.noAsset {
			continue
		}
		asset := PluginAssetName(p.name)
		entry := p.entry
		if entry == "" {
			entry = PluginBinaryPrefix + p.name
		}
		pt := createTestTarball(f.t, map[string][]byte{entry: []byte(p.body)})
		ps := sha256.Sum256(pt)
		switch {
		case p.emptySum:
			sums += fmt.Sprintf("%s  %s\n", emptySHA256, asset)
		case p.badSum:
			sums += fmt.Sprintf("%s  %s\n", strings.Repeat("ab", 32), asset)
		case !p.noSum:
			sums += fmt.Sprintf("%s  %s\n", hex.EncodeToString(ps[:]), asset)
		}
		f.files[downloadPath(tag, asset)] = pt
		assets = append(assets, fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, asset, "https://github.com"+downloadPath(tag, asset)))
	}
	f.files[downloadPath(tag, ChecksumsAssetName)] = []byte(sums)
	f.releases[tag] = fmt.Sprintf(`{"tag_name":%q,"draft":false,"published_at":%q,"assets":[%s]}`,
		tag, published, strings.Join(assets, ","))
}

// pluginDirWith creates a plugin directory holding executable
// scion-plugin-<name> files with the given contents.
func pluginDirWith(t *testing.T, plugins map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "plugins", "broker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range plugins {
		if err := os.WriteFile(filepath.Join(dir, PluginBinaryPrefix+name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readFileString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wantFile(t *testing.T, p, want string) {
	t.Helper()
	if got := readFileString(t, p); got != want {
		t.Fatalf("%s = %q, want %q", p, got, want)
	}
}

func noStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("%s left in %s", e.Name(), dir)
		}
	}
}

func symlinkLockstepSetup(t *testing.T, plugins map[string]string) (*fakeGitHub, string, string, *int, *BinaryUpdateExecutor) {
	t.Helper()
	gh := newFakeGitHub(t)
	root := installLayout(t, "nightly-20261022", "nightly-20261021")
	if err := os.Symlink("releases/nightly-20261021", filepath.Join(root, prevLinkName)); err != nil {
		t.Fatal(err)
	}
	opts := symlinkOpts(root)
	signals := 0
	opts.Signal = func() error { signals++; return nil }
	inst := Detect(context.Background(), opts)
	if inst.Name() != StrategySymlink {
		t.Fatalf("Detect = %s (%s)", inst.Name(), inst.Status().Reason)
	}
	dir := pluginDirWith(t, plugins)
	e := newTestExecutor(gh, inst, "nightly-20261022")
	e.pluginDir = dir
	return gh, root, dir, &signals, e
}

// TestPluginLockstep_SymlinkUpdate: the target's plugin builds are installed
// before the restart; a plugin without an asset and a symlinked plugin are
// left with warnings; both releases keep their plugins under
// releases/<tag>/plugins/.
func TestPluginLockstep_SymlinkUpdate(t *testing.T) {
	requireLinux(t)
	gh, root, dir, signals, e := symlinkLockstepSetup(t, map[string]string{
		"telegram": "telegram 22",
		"slack":    "slack 22",
	})
	dev := filepath.Join(t.TempDir(), "my-dev-build")
	if err := os.WriteFile(dev, []byte("dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dev, filepath.Join(dir, PluginBinaryPrefix+"dev")); err != nil {
		t.Fatal(err)
	}
	// A stale staging directory of an interrupted run is removed.
	stale := filepath.Join(dir, pluginStagingPrefix+"old")
	if err := os.MkdirAll(filepath.Join(stale, "new"), 0o700); err != nil {
		t.Fatal(err)
	}
	gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z",
		lockstepPlugin{name: "telegram", body: "telegram 23"},
		lockstepPlugin{name: "slack", noAsset: true})

	var log bytes.Buffer
	err := e.RunWithBeforeRestart(context.Background(), &log, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error {
			// Installed before the restart.
			wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 23")
			if *signals != 0 {
				t.Error("signalled before the hook")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	if *signals != 1 {
		t.Fatalf("signals = %d", *signals)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 23")
	wantFile(t, filepath.Join(dir, "scion-plugin-slack"), "slack 22")
	wantFile(t, dev, "dev")
	if target, err := os.Readlink(filepath.Join(dir, "scion-plugin-dev")); err != nil || target != dev {
		t.Fatalf("symlinked plugin changed: %q, %v", target, err)
	}
	noStaging(t, dir)

	out := log.String()
	for _, want := range []string{
		"WARNING: release nightly-20261023 has no " + PluginAssetName("slack"),
		"scion-plugin-dev is a symlink",
		"checksum verified for " + PluginAssetName("telegram"),
		"Installed " + filepath.Join(dir, "scion-plugin-telegram"),
		"WARNING: removing " + stale,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}

	// Retention for rollback symmetry.
	oldPlugins := filepath.Join(root, "releases", "nightly-20261022", "plugins")
	newPlugins := filepath.Join(root, "releases", "nightly-20261023", "plugins")
	wantFile(t, filepath.Join(oldPlugins, "scion-plugin-telegram"), "telegram 22")
	wantFile(t, filepath.Join(oldPlugins, "scion-plugin-slack"), "slack 22")
	wantFile(t, filepath.Join(newPlugins, "scion-plugin-telegram"), "telegram 23")
	wantFile(t, filepath.Join(newPlugins, "scion-plugin-slack"), "slack 22")
	if _, err := os.Lstat(filepath.Join(newPlugins, "scion-plugin-dev")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unmanaged plugin retained: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(oldPlugins, "scion-plugin-telegram")); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o755 {
		t.Fatalf("retained plugin is not a 0755 copy: %v, %v", fi, err)
	}
}

// TestPluginLockstep_VerificationFailsClosed: a downloaded plugin whose
// hash differs from its SHA256SUMS line, a malformed archive, or a URL
// outside the release fails the whole update before the snapshot: nothing
// is staged, activated or replaced.
func TestPluginLockstep_VerificationFailsClosed(t *testing.T) {
	requireLinux(t)
	for name, tc := range map[string]struct {
		plugin  lockstepPlugin
		mutate  func(gh *fakeGitHub)
		wantErr string
	}{
		"downloaded hash differs from its line": {plugin: lockstepPlugin{name: "telegram", body: "telegram 23", badSum: true}, wantErr: "checksum mismatch"},
		"archive without the plugin entry": {
			plugin:  lockstepPlugin{name: "telegram", body: "telegram 23", entry: "scion-plugin-other"},
			wantErr: "no 'scion-plugin-telegram' binary",
		},
		"asset URL outside the release": {
			plugin: lockstepPlugin{name: "telegram", body: "telegram 23"},
			mutate: func(gh *fakeGitHub) {
				gh.releases["nightly-20261023"] = strings.Replace(gh.releases["nightly-20261023"],
					"https://github.com"+downloadPath("nightly-20261023", PluginAssetName("telegram")),
					"https://github.com"+downloadPath("nightly-20261001", PluginAssetName("telegram")), 1)
			},
			wantErr: "release nightly-20261023",
		},
	} {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z", tc.plugin)
			if tc.mutate != nil {
				tc.mutate(gh)
			}
			inst := &fakeInstaller{}
			snaps := &fakeDBSnapshotter{calls: &inst.calls}
			e := newTestExecutor(gh, inst, "nightly-20261022")
			e.snapshots = snaps
			dir := pluginDirWith(t, map[string]string{"telegram": "telegram 22"})
			e.pluginDir = dir

			var log bytes.Buffer
			err := e.Run(context.Background(), &log, map[string]string{"target_version": "nightly-20261023"})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "nothing was installed") {
				t.Fatalf("err = %v, want %q\n%s", err, tc.wantErr, log.String())
			}
			if !slices.Equal(inst.calls, []string{"preflight"}) {
				t.Fatalf("calls = %v, want preflight only", inst.calls)
			}
			wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 22")
			noStaging(t, dir)
		})
	}
}

// TestPluginLockstep_UnwritableDirSkips: the hub update goes ahead and the
// plugins are left, with a warning.
func TestPluginLockstep_UnwritableDirSkips(t *testing.T) {
	requireLinux(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0555 directory")
	}
	gh := newFakeGitHub(t)
	gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z", lockstepPlugin{name: "telegram", body: "telegram 23"})
	inst := &fakeInstaller{}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	dir := pluginDirWith(t, map[string]string{"telegram": "telegram 22"})
	e.pluginDir = dir
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	var log bytes.Buffer
	if err := e.Run(context.Background(), &log, map[string]string{"target_version": "nightly-20261023"}); err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	if !slices.Contains(inst.calls, "restart") {
		t.Fatalf("calls = %v", inst.calls)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 22")
	if !strings.Contains(log.String(), "is not writable by the hub; broker plugins are not updated") {
		t.Fatalf("log lacks the warning:\n%s", log.String())
	}
	if gh.requested(PluginAssetName("telegram")) {
		t.Fatal("downloaded a plugin that cannot be installed")
	}
}

// TestPluginLockstep_NoPluginDir: no plugin directory (or lockstep off)
// changes nothing in the update.
func TestPluginLockstep_NoPluginDir(t *testing.T) {
	requireLinux(t)
	for name, dir := range map[string]string{
		"off":     "",
		"missing": filepath.Join(t.TempDir(), "no-such-dir"),
	} {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z", lockstepPlugin{name: "telegram", body: "telegram 23"})
			inst := &fakeInstaller{}
			e := newTestExecutor(gh, inst, "nightly-20261022")
			e.pluginDir = dir
			var log bytes.Buffer
			if err := e.Run(context.Background(), &log, map[string]string{"target_version": "nightly-20261023"}); err != nil {
				t.Fatalf("Run: %v\n%s", err, log.String())
			}
			if strings.Contains(log.String(), "plugin") || gh.requested(PluginAssetName("telegram")) {
				t.Fatalf("plugin work without plugins:\n%s", log.String())
			}
		})
	}
}

// TestPluginLockstep_HookFailureRestoresPlugins: when the hub is rolled
// back (Deactivate), the plugins are put back too.
func TestPluginLockstep_HookFailureRestoresPlugins(t *testing.T) {
	requireLinux(t)
	gh, root, dir, signals, e := symlinkLockstepSetup(t, map[string]string{"telegram": "telegram 22"})
	gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z", lockstepPlugin{name: "telegram", body: "telegram 23"})

	var log bytes.Buffer
	err := e.RunWithBeforeRestart(context.Background(), &log, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error { return errors.New("db down") })
	if err == nil || !strings.Contains(err.Error(), "rolled back, nothing changed") {
		t.Fatalf("err = %v\n%s", err, log.String())
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
		t.Fatalf("current = %q", cur)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 22")
	noStaging(t, dir)
	if *signals != 0 || !strings.Contains(log.String(), "Restored the previous broker plugins") {
		t.Fatalf("signals = %d\n%s", *signals, log.String())
	}
}

// TestPluginLockstep_RestartFailureRestoresPlugins: same for a failed
// restart request.
func TestPluginLockstep_RestartFailureRestoresPlugins(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z", lockstepPlugin{name: "telegram", body: "telegram 23"})
	root := installLayout(t, "nightly-20261022")
	opts := symlinkOpts(root)
	opts.Signal = func() error { return errors.New("kill: operation not permitted") }
	inst := Detect(context.Background(), opts)
	e := newTestExecutor(gh, inst, "nightly-20261022")
	dir := pluginDirWith(t, map[string]string{"telegram": "telegram 22"})
	e.pluginDir = dir

	err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"})
	if err == nil || !strings.Contains(err.Error(), "restarting the hub failed") {
		t.Fatalf("err = %v", err)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 22")
	noStaging(t, dir)
}

// TestPluginLockstep_SudoInstallKeepsPluginsWithTheHub: an installer that
// cannot be rolled back keeps the new hub, so the new plugins stay too.
func TestPluginLockstep_SudoInstallKeepsPluginsWithTheHub(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z", lockstepPlugin{name: "telegram", body: "telegram 23"})
	inst := &fakeInstaller{}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	dir := pluginDirWith(t, map[string]string{"telegram": "telegram 22"})
	e.pluginDir = dir
	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error { return errors.New("db down") })
	if err == nil || !strings.Contains(err.Error(), "cannot be rolled back") {
		t.Fatalf("err = %v", err)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 23")
	noStaging(t, dir)
}

// TestPluginSwap_AtomicAndRestoresOnFailure: each plugin is replaced by one
// rename (a process holding the old file keeps it intact), and a failure
// part-way puts the plugins already replaced back.
func TestPluginSwap_AtomicAndRestoresOnFailure(t *testing.T) {
	dir := pluginDirWith(t, map[string]string{"a": "a old", "b": "b old"})
	newBin := func(body string) string {
		p := filepath.Join(t.TempDir(), "bin")
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	plan := func() *pluginPlan {
		return &pluginPlan{dir: dir, plugins: []pluginUpdate{
			{installedPlugin: installedPlugin{file: "scion-plugin-a", name: "a", path: filepath.Join(dir, "scion-plugin-a")}, bin: newBin("a new")},
			{installedPlugin: installedPlugin{file: "scion-plugin-b", name: "b", path: filepath.Join(dir, "scion-plugin-b")}, bin: newBin("b new")},
		}}
	}

	t.Run("failure part-way restores", func(t *testing.T) {
		sw, err := stagePluginSwap(io.Discard, plan())
		if err != nil {
			t.Fatal(err)
		}
		// b becomes something a file cannot be renamed over.
		bPath := filepath.Join(dir, "scion-plugin-b")
		if err := os.Rename(bPath, bPath+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(bPath, "x"), 0o755); err != nil {
			t.Fatal(err)
		}
		err = sw.apply(io.Discard)
		if err == nil || !strings.Contains(err.Error(), "scion-plugin-b") {
			t.Fatalf("apply err = %v", err)
		}
		wantFile(t, filepath.Join(dir, "scion-plugin-a"), "a old")
		if err := os.RemoveAll(bPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(bPath+".saved", bPath); err != nil {
			t.Fatal(err)
		}
		noStaging(t, dir)
	})

	t.Run("rename keeps open handles intact", func(t *testing.T) {
		held, err := os.Open(filepath.Join(dir, "scion-plugin-a"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.Close() }()
		sw, err := stagePluginSwap(io.Discard, plan())
		if err != nil {
			t.Fatal(err)
		}
		if err := sw.apply(io.Discard); err != nil {
			t.Fatal(err)
		}
		wantFile(t, filepath.Join(dir, "scion-plugin-a"), "a new")
		wantFile(t, filepath.Join(dir, "scion-plugin-b"), "b new")
		old, err := io.ReadAll(held)
		if err != nil || string(old) != "a old" {
			t.Fatalf("held handle reads %q, %v", old, err)
		}
		if err := sw.restore(); err != nil {
			t.Fatal(err)
		}
		wantFile(t, filepath.Join(dir, "scion-plugin-a"), "a old")
		wantFile(t, filepath.Join(dir, "scion-plugin-b"), "b old")
		noStaging(t, dir)
	})
}

func TestListInstalledPlugins(t *testing.T) {
	dir := pluginDirWith(t, map[string]string{"telegram": "t", "Bad.Name": "x"})
	if err := os.WriteFile(filepath.Join(dir, "scion-plugin-noexec"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "scion-plugin-adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("r"), 0o755); err != nil {
		t.Fatal(err)
	}
	plugins, warnings, err := listInstalledPlugins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 || plugins[0].name != "telegram" || plugins[0].path != filepath.Join(dir, "scion-plugin-telegram") {
		t.Fatalf("plugins = %+v", plugins)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "scion-plugin-Bad.Name has an unexpected name") {
		t.Fatalf("warnings = %v", warnings)
	}
	if p, w, err := listInstalledPlugins(filepath.Join(dir, "missing")); p != nil || w != nil || err != nil {
		t.Fatalf("missing dir = %v, %v, %v", p, w, err)
	}
}

// TestPluginLockstep_DowngradeRiskList: the downgrade confirmation names
// the plugins the target does not publish.
func TestPluginLockstep_DowngradeRiskList(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addLockstepRelease("nightly-20261020", "2026-10-20T04:00:00Z",
		lockstepPlugin{name: "telegram", body: "telegram 20"},
		lockstepPlugin{name: "slack", noAsset: true},
		lockstepPlugin{name: "teams", body: "teams 20", noSum: true})
	inst := &fakeInstaller{}
	e := newTestExecutor(gh, inst, "nightly-20261023")
	e.snapshots = &fakeDBSnapshotter{}
	e.pluginDir = pluginDirWith(t, map[string]string{"telegram": "t", "slack": "s", "teams": "m"})

	err := e.CheckTarget(context.Background(), map[string]string{"target_version": "nightly-20261020"})
	wantCode(t, err, CodeDowngradeRequiresConfirmation)
	re, _ := IsRequestError(err)
	all := strings.Join(re.Details["risks"].([]string), "\n")
	if !strings.Contains(all, "nightly-20261020 publishes no "+PluginAssetName("slack")) {
		t.Fatalf("risks lack the slack asset:\n%s", all)
	}
	if !strings.Contains(all, "nightly-20261020 has no usable SHA256SUMS line for "+PluginAssetName("teams")) {
		t.Fatalf("risks lack the teams checksum line:\n%s", all)
	}
	if strings.Contains(all, PluginAssetName("telegram")) {
		t.Fatalf("risks name a published asset:\n%s", all)
	}
}

func TestExtractRootEntry_Plugin(t *testing.T) {
	tarball := createTestTarball(t, map[string][]byte{"scion-plugin-telegram": []byte("tg"), "README": []byte("r")})
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(p, tarball, 0o644); err != nil {
		t.Fatal(err)
	}
	bin, err := extractRootEntry(p, t.TempDir(), "scion-plugin-telegram", io.Discard, DefaultMaxExtractBytes)
	if err != nil {
		t.Fatal(err)
	}
	wantFile(t, bin, "tg")
	if _, err := extractRootEntry(p, t.TempDir(), "scion-plugin-slack", io.Discard, DefaultMaxExtractBytes); err == nil ||
		!strings.Contains(err.Error(), "no 'scion-plugin-slack' binary") {
		t.Fatalf("missing entry err = %v", err)
	}
	nested := createTestTarball(t, map[string][]byte{"dist/scion-plugin-telegram": []byte("tg")})
	if err := os.WriteFile(p, nested, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractRootEntry(p, t.TempDir(), "scion-plugin-telegram", io.Discard, DefaultMaxExtractBytes); err == nil ||
		!strings.Contains(err.Error(), "nested") {
		t.Fatalf("nested entry err = %v", err)
	}
}

// TestPluginLockstep_UnusableSumsLineKeepsPlugin: a plugin whose SHA256SUMS
// line is missing or lists the empty-file hash is kept at its current
// build with a warning (design D2); the update and the other plugins go
// ahead, and nothing unverified is downloaded or installed.
func TestPluginLockstep_UnusableSumsLineKeepsPlugin(t *testing.T) {
	requireLinux(t)
	for name, p := range map[string]lockstepPlugin{
		"no line":         {name: "slack", body: "slack 23", noSum: true},
		"empty-file hash": {name: "slack", body: "slack 23", emptySum: true},
	} {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z",
				lockstepPlugin{name: "telegram", body: "telegram 23"}, p)
			inst := &fakeInstaller{}
			e := newTestExecutor(gh, inst, "nightly-20261022")
			dir := pluginDirWith(t, map[string]string{"telegram": "telegram 22", "slack": "slack 22"})
			e.pluginDir = dir

			var log bytes.Buffer
			if err := e.Run(context.Background(), &log, map[string]string{"target_version": "nightly-20261023"}); err != nil {
				t.Fatalf("Run: %v\n%s", err, log.String())
			}
			wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 23")
			wantFile(t, filepath.Join(dir, "scion-plugin-slack"), "slack 22")
			if !strings.Contains(log.String(), "WARNING: release nightly-20261023: ") ||
				!strings.Contains(log.String(), "scion-plugin-slack is left at its current build") {
				t.Fatalf("log lacks the warning:\n%s", log.String())
			}
			if gh.requested(PluginAssetName("slack")) {
				t.Fatal("downloaded a plugin that cannot be verified")
			}
		})
	}
}

// failingDeactivator can roll back in principle, but its Deactivate fails.
type failingDeactivator struct{ fakeInstaller }

func (f *failingDeactivator) Deactivate(context.Context) error {
	f.calls = append(f.calls, "deactivate")
	return errors.New("current link is gone")
}

// TestPluginLockstep_DeactivateFailureKeepsPlugins: when the hub cannot be
// rolled back, the new hub runs after the next restart, so the plugins
// stay at its builds (never restored on their own) and staging is removed.
func TestPluginLockstep_DeactivateFailureKeepsPlugins(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z", lockstepPlugin{name: "telegram", body: "telegram 23"})
	inst := &failingDeactivator{}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	dir := pluginDirWith(t, map[string]string{"telegram": "telegram 22"})
	e.pluginDir = dir

	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error { return errors.New("db down") })
	if err == nil || !strings.Contains(err.Error(), "rolling back nightly-20261023 also failed") {
		t.Fatalf("err = %v", err)
	}
	if !slices.Contains(inst.calls, "deactivate") {
		t.Fatalf("calls = %v", inst.calls)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 23")
	noStaging(t, dir)
}

// activateThen runs then after a successful Activate of the wrapped
// installer, and forwards Deactivate.
type activateThen struct {
	Installer
	d    Deactivator
	then func()
}

func (a *activateThen) Activate(ctx context.Context, staged string) error {
	if err := a.Installer.Activate(ctx, staged); err != nil {
		return err
	}
	a.then()
	return nil
}

func (a *activateThen) Deactivate(ctx context.Context) error { return a.d.Deactivate(ctx) }

// TestPluginLockstep_SwapFailureRollsBackHub: a plugin that cannot be
// renamed into place after Activate rolls the hub back, the plugins stay
// at the old builds, and the hub is not restarted.
func TestPluginLockstep_SwapFailureRollsBackHub(t *testing.T) {
	requireLinux(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0555 directory")
	}
	gh := newFakeGitHub(t)
	gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z", lockstepPlugin{name: "telegram", body: "telegram 23"})
	root := installLayout(t, "nightly-20261022")
	opts := symlinkOpts(root)
	signals := 0
	opts.Signal = func() error { signals++; return nil }
	sym := Detect(context.Background(), opts)
	if sym.Name() != StrategySymlink {
		t.Fatalf("Detect = %s (%s)", sym.Name(), sym.Status().Reason)
	}
	dir := pluginDirWith(t, map[string]string{"telegram": "telegram 22"})
	inst := &activateThen{Installer: sym, d: sym.(Deactivator), then: func() {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
	}}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	e := newTestExecutor(gh, inst, "nightly-20261022")
	e.pluginDir = dir

	err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"})
	if err == nil || !strings.Contains(err.Error(), "installing the broker plugins failed") ||
		!strings.Contains(err.Error(), "rolled back, nothing changed") {
		t.Fatalf("err = %v", err)
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
		t.Fatalf("current = %q", cur)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 22")
	if signals != 0 {
		t.Fatalf("signalled %d times", signals)
	}
}

// TestPluginLockstep_SymlinkedPluginDirSkips: a plugin directory that is a
// symlink is skipped with a warning, as the rollback reconcile skips it.
func TestPluginLockstep_SymlinkedPluginDirSkips(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z", lockstepPlugin{name: "telegram", body: "telegram 23"})
	inst := &fakeInstaller{}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	realDir := pluginDirWith(t, map[string]string{"telegram": "telegram 22"})
	link := filepath.Join(t.TempDir(), "broker")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	e.pluginDir = link
	var log bytes.Buffer
	if err := e.Run(context.Background(), &log, map[string]string{"target_version": "nightly-20261023"}); err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	wantFile(t, filepath.Join(realDir, "scion-plugin-telegram"), "telegram 22")
	if !strings.Contains(log.String(), link+" is a symlink; broker plugins are not updated") {
		t.Fatalf("log lacks the warning:\n%s", log.String())
	}
}

// TestPluginSwap_RestoreFailureKeepsBackups: a restore that cannot put a
// plugin back keeps the staging directory (the old binary), the rollback
// error does not claim nothing changed, and a later restore retries.
func TestPluginSwap_RestoreFailureKeepsBackups(t *testing.T) {
	requireLinux(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0555 directory")
	}
	dir := pluginDirWith(t, map[string]string{"a": "a old"})
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(bin, []byte("a new"), 0o755); err != nil {
		t.Fatal(err)
	}
	sw, err := stagePluginSwap(io.Discard, &pluginPlan{dir: dir, plugins: []pluginUpdate{
		{installedPlugin: installedPlugin{file: "scion-plugin-a", name: "a", path: filepath.Join(dir, "scion-plugin-a")}, bin: bin},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.apply(io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	inst := &ctxAwareDeactivator{}
	e := newTestExecutor(newFakeGitHub(t), inst, "nightly-20261022")
	err = e.undoActivation(context.Background(), &bytes.Buffer{}, "nightly-20261023", sw, errors.New("hook failed"))
	if err == nil || !strings.Contains(err.Error(), "restoring the previous broker plugins failed") ||
		strings.Contains(err.Error(), "nothing changed") || !inst.deactivated {
		t.Fatalf("err = %v, deactivated = %v", err, inst.deactivated)
	}
	if _, err := os.Stat(filepath.Join(sw.staging, "old", "scion-plugin-a")); err != nil {
		t.Fatalf("old binary not kept: %v", err)
	}
	sw.cleanup() // a kept backup survives cleanup
	if _, err := os.Stat(sw.staging); err != nil {
		t.Fatalf("staging removed: %v", err)
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sw.restore(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-a"), "a old")
	noStaging(t, dir)
}

// stageThen is a fakeInstaller that runs then after a successful Stage.
type stageThen struct {
	fakeInstaller
	then func()
}

func (s *stageThen) Stage(ctx context.Context, tag, bin string) (string, error) {
	staged, err := s.fakeInstaller.Stage(ctx, tag, bin)
	if err == nil {
		s.then()
	}
	return staged, err
}

// TestPluginLockstep_StageFailureNeverActivates pins the order "stage the
// plugin replacements, then Activate": a plugin directory that becomes
// unwritable after the download phase makes staging fail, and the hub
// release is never activated, restarted or rolled back.
func TestPluginLockstep_StageFailureNeverActivates(t *testing.T) {
	requireLinux(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0555 directory")
	}
	gh := newFakeGitHub(t)
	gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z", lockstepPlugin{name: "telegram", body: "telegram 23"})
	dir := pluginDirWith(t, map[string]string{"telegram": "telegram 22"})
	inst := &stageThen{then: func() {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
	}}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	e := newTestExecutor(gh, inst, "nightly-20261022")
	e.pluginDir = dir

	err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"})
	if err == nil || !strings.Contains(err.Error(), "stage the broker plugins, nothing was activated") {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(inst.calls, []string{"preflight", "stage:nightly-20261023"}) {
		t.Fatalf("calls = %v, want no activate or restart", inst.calls)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 22")
}
