//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func createFakePlaywrightSeedDir(t *testing.T, revs ...string) string {
	t.Helper()
	seedDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(seedDir): %v", err)
	}
	fakeFfmpeg := filepath.Join(seedDir, "fake-ffmpeg")
	if err := os.WriteFile(fakeFfmpeg, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake-ffmpeg: %v", err)
	}
	for _, rev := range revs {
		revDir := filepath.Join(seedDir, "ffmpeg-"+rev)
		if err := os.MkdirAll(revDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", revDir, err)
		}
		if err := os.Symlink(fakeFfmpeg, filepath.Join(revDir, "ffmpeg-linux")); err != nil {
			t.Fatalf("symlink ffmpeg-linux: %v", err)
		}
		for _, name := range []string{"INSTALLATION_COMPLETE", "DEPENDENCIES_VALIDATED"} {
			if err := os.WriteFile(filepath.Join(revDir, name), nil, 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		if err := os.WriteFile(filepath.Join(revDir, "browsers.json"), []byte(`{"browsers":[{"name":"ffmpeg","revision":"`+rev+`"}]}`+"\n"), 0o644); err != nil {
			t.Fatalf("write browsers.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(revDir, "package.json"), []byte(`{"name":"scion-system-ffmpeg","version":"0.0.0"}`+"\n"), 0o644); err != nil {
			t.Fatalf("write package.json: %v", err)
		}
	}
	return seedDir
}

func evalTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return d
}

// TestSeedPlaywrightFfmpegCache_AbsentSeedDirIsSilentNoOp verifies that when
// the image does not contain /usr/local/share/scion-playwright-ffmpeg, the
// init hook is a silent no-op and does not create ~/.cache or ms-playwright.
func TestSeedPlaywrightFfmpegCache_AbsentSeedDirIsSilentNoOp(t *testing.T) {
	home := evalTempDir(t)
	missingSeed := filepath.Join(evalTempDir(t), "does-not-exist")

	seedPlaywrightFfmpegCache(home, missingSeed, 0, 0)

	if _, err := os.Lstat(filepath.Join(home, ".cache")); !os.IsNotExist(err) {
		t.Errorf("expected ~/.cache to remain absent when seed dir is missing, got Lstat err = %v", err)
	}
}

// TestSeedPlaywrightFfmpegCache_FreshHomeSeedsRevisionsAndChowns verifies a
// fresh agent home receives symlinks for all valid ffmpeg-<rev> directories in
// the seed tree, exact no-newline .links/scion-ffmpeg-<rev> back-references,
// ignores unrelated entries in the seed tree, and applies uid:gid ownership.
func TestSeedPlaywrightFfmpegCache_FreshHomeSeedsRevisionsAndChowns(t *testing.T) {
	home := evalTempDir(t)
	t.Setenv("XDG_CACHE_HOME", "")
	seedDir := createFakePlaywrightSeedDir(t, "1011", "1013")

	// Non-ffmpeg or symlink entries inside seedDir must be ignored.
	if err := os.MkdirAll(filepath.Join(seedDir, "chromium-1200"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(seedDir, "ffmpeg-notanumber"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(seedDir, "ffmpeg-1013"), filepath.Join(seedDir, "ffmpeg-9999")); err != nil {
		t.Fatal(err)
	}

	// First call exercises real kernel fchown/fchownat(AT_SYMLINK_NOFOLLOW)
	// using the test process's own uid/gid.
	seedPlaywrightFfmpegCache(home, seedDir, os.Getuid(), os.Getgid())

	regDir := filepath.Join(home, ".cache", "ms-playwright")
	for _, rev := range []string{"1011", "1013"} {
		wantTarget := filepath.Join(seedDir, "ffmpeg-"+rev)
		gotTarget, err := os.Readlink(filepath.Join(regDir, "ffmpeg-"+rev))
		if err != nil {
			t.Fatalf("readlink ffmpeg-%s: %v", rev, err)
		}
		if gotTarget != wantTarget {
			t.Errorf("ffmpeg-%s -> %q, want %q", rev, gotTarget, wantTarget)
		}

		linkPath := filepath.Join(regDir, ".links", "scion-ffmpeg-"+rev)
		linkBytes, err := os.ReadFile(linkPath)
		if err != nil {
			t.Fatalf("read %s: %v", linkPath, err)
		}
		if string(linkBytes) != wantTarget {
			t.Errorf(".links/scion-ffmpeg-%s = %q, want exact %q (no trailing newline)", rev, string(linkBytes), wantTarget)
		}
	}

	for _, ignored := range []string{"chromium-1200", "ffmpeg-notanumber", "ffmpeg-9999"} {
		if _, err := os.Lstat(filepath.Join(regDir, ignored)); !os.IsNotExist(err) {
			t.Errorf("unexpected seeded entry %s (err=%v)", ignored, err)
		}
	}

	// Second call verifies arbitrary uid:gid forwarding through the inner
	// fchown/lchownAt seams on an already-existing .cache/ms-playwright dir.
	var fchownCalls, lchownCalls int
	origFchown := playwrightFfmpegFchown
	origLchown := playwrightFfmpegLchownAt
	playwrightFfmpegFchown = func(_ int, uid, gid int) error {
		if uid != 1234 || gid != 5678 {
			t.Errorf("fchown got (%d, %d), want (1234, 5678)", uid, gid)
		}
		fchownCalls++
		return nil
	}
	playwrightFfmpegLchownAt = func(_ int, _ string, uid, gid int) error {
		if uid != 1234 || gid != 5678 {
			t.Errorf("lchownAt got (%d, %d), want (1234, 5678)", uid, gid)
		}
		lchownCalls++
		return nil
	}
	t.Cleanup(func() {
		playwrightFfmpegFchown = origFchown
		playwrightFfmpegLchownAt = origLchown
	})

	home2 := evalTempDir(t)
	t.Setenv("XDG_CACHE_HOME", "")
	if err := os.MkdirAll(filepath.Join(home2, ".cache", "ms-playwright"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedPlaywrightFfmpegCache(home2, seedDir, 1234, 5678)
	if fchownCalls < 3 {
		t.Errorf("fchownCalls = %d, want >= 3 (.links dir + 2 link files)", fchownCalls)
	}
	if lchownCalls != 2 {
		t.Errorf("lchownCalls = %d, want 2 (one per seeded symlink)", lchownCalls)
	}
}

// TestSeedPlaywrightFfmpegCache_ExistingMatchingIsIdempotentWithoutRewrite
// verifies that on warm restarts where the seeded symlinks and .links files
// already match, neither the symlinks nor the .links files are rewritten.
func TestSeedPlaywrightFfmpegCache_ExistingMatchingIsIdempotentWithoutRewrite(t *testing.T) {
	home := evalTempDir(t)
	t.Setenv("XDG_CACHE_HOME", "")
	seedDir := createFakePlaywrightSeedDir(t, "1013")

	seedPlaywrightFfmpegCache(home, seedDir, 0, 0)

	regDir := filepath.Join(home, ".cache", "ms-playwright")
	symlinkPath := filepath.Join(regDir, "ffmpeg-1013")
	linkFilePath := filepath.Join(regDir, ".links", "scion-ffmpeg-1013")

	inodeOf := func(p string) uint64 {
		t.Helper()
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("Lstat(%s): %v", p, err)
		}
		return fi.Sys().(*syscall.Stat_t).Ino
	}
	symIno1 := inodeOf(symlinkPath)
	fileIno1 := inodeOf(linkFilePath)

	seedPlaywrightFfmpegCache(home, seedDir, 0, 0)

	if symIno2 := inodeOf(symlinkPath); symIno2 != symIno1 {
		t.Errorf("matching symlink was recreated on warm restart (ino %d -> %d)", symIno1, symIno2)
	}
	if fileIno2 := inodeOf(linkFilePath); fileIno2 != fileIno1 {
		t.Errorf("matching .links file was rewritten on warm restart (ino %d -> %d)", fileIno1, fileIno2)
	}
}

// TestSeedPlaywrightFfmpegCache_RefreshesStaleEntriesAndPrunesRetiredSeedLinks
// verifies stale seed symlinks (pointing to a wrong/old path inside seedDir or
// with stale/missing .links content) are refreshed, and retired seed symlinks
// from older base images are pruned cleanly.
func TestSeedPlaywrightFfmpegCache_RefreshesStaleEntriesAndPrunesRetiredSeedLinks(t *testing.T) {
	home := evalTempDir(t)
	t.Setenv("XDG_CACHE_HOME", "")
	seedDir := createFakePlaywrightSeedDir(t, "1011", "1013")

	regDir := filepath.Join(home, ".cache", "ms-playwright")
	linksDir := filepath.Join(regDir, ".links")
	if err := os.MkdirAll(linksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Stale symlink target inside seedDir + stale .links file.
	staleTarget := filepath.Join(seedDir, "ffmpeg-1010")
	if err := os.Symlink(staleTarget, filepath.Join(regDir, "ffmpeg-1013")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linksDir, "scion-ffmpeg-1013"), []byte(staleTarget+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Valid symlink target, missing .links file.
	if err := os.Symlink(filepath.Join(seedDir, "ffmpeg-1011"), filepath.Join(regDir, "ffmpeg-1011")); err != nil {
		t.Fatal(err)
	}

	// 3. Retired revision symlink from an older image pointing into seedDir.
	retiredTarget := filepath.Join(seedDir, "ffmpeg-1008")
	if err := os.Symlink(retiredTarget, filepath.Join(regDir, "ffmpeg-1008")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linksDir, "scion-ffmpeg-1008"), []byte(retiredTarget), 0o644); err != nil {
		t.Fatal(err)
	}

	seedPlaywrightFfmpegCache(home, seedDir, 0, 0)

	// 1 & 2 should now be valid and up to date.
	for _, rev := range []string{"1011", "1013"} {
		want := filepath.Join(seedDir, "ffmpeg-"+rev)
		got, err := os.Readlink(filepath.Join(regDir, "ffmpeg-"+rev))
		if err != nil || got != want {
			t.Errorf("ffmpeg-%s readlink = (%q, %v), want (%q, nil)", rev, got, err, want)
		}
		raw, err := os.ReadFile(filepath.Join(linksDir, "scion-ffmpeg-"+rev))
		if err != nil || string(raw) != want {
			t.Errorf(".links/scion-ffmpeg-%s = (%q, %v), want (%q, nil)", rev, string(raw), err, want)
		}
	}

	// 3 (retired seed symlink + its .links entry) should be pruned.
	if _, err := os.Lstat(filepath.Join(regDir, "ffmpeg-1008")); !os.IsNotExist(err) {
		t.Errorf("expected retired seed symlink ffmpeg-1008 to be pruned, got err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(linksDir, "scion-ffmpeg-1008")); !os.IsNotExist(err) {
		t.Errorf("expected retired .links/scion-ffmpeg-1008 to be pruned, got err = %v", err)
	}
}

// TestSeedPlaywrightFfmpegCache_PreservesUserOwnedDirsAndForeignSymlinks
// verifies that existing real directories (e.g. a previously downloaded
// ffmpeg-<rev> or webkit/firefox caches) and foreign symlinks pointing outside
// seedDir are never followed, modified, or removed.
func TestSeedPlaywrightFfmpegCache_PreservesUserOwnedDirsAndForeignSymlinks(t *testing.T) {
	home := evalTempDir(t)
	t.Setenv("XDG_CACHE_HOME", "")
	seedDir := createFakePlaywrightSeedDir(t, "1011", "1012", "1013")
	outside := evalTempDir(t)

	regDir := filepath.Join(home, ".cache", "ms-playwright")
	linksDir := filepath.Join(regDir, ".links")
	if err := os.MkdirAll(linksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing real downloaded ffmpeg-1013 directory.
	userFfmpegDir := filepath.Join(regDir, "ffmpeg-1013")
	if err := os.MkdirAll(userFfmpegDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userFfmpegDir, "ffmpeg-linux"), []byte("user-downloaded"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing foreign symlink at ffmpeg-1011 and retired foreign symlink at ffmpeg-1008.
	custom1011 := filepath.Join(outside, "custom-1011")
	custom1008 := filepath.Join(outside, "custom-1008")
	if err := os.Symlink(custom1011, filepath.Join(regDir, "ffmpeg-1011")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(custom1008, filepath.Join(regDir, "ffmpeg-1008")); err != nil {
		t.Fatal(err)
	}

	// Pre-existing runtime browser download (webkit) and user .links entry.
	webkitMarker := filepath.Join(regDir, "webkit-2105", "INSTALLATION_COMPLETE")
	if err := os.MkdirAll(filepath.Dir(webkitMarker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(webkitMarker, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	userLinkFile := filepath.Join(linksDir, "deadbeef1234")
	if err := os.WriteFile(userLinkFile, []byte("/workspace/node_modules/playwright-core"), 0o644); err != nil {
		t.Fatal(err)
	}

	seedPlaywrightFfmpegCache(home, seedDir, 0, 0)

	// User real dir ffmpeg-1013 untouched, no scion-ffmpeg-1013 .links file written.
	fi, err := os.Lstat(userFfmpegDir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("user real dir ffmpeg-1013 changed: fi=%v err=%v", fi, err)
	}
	gotBin, err := os.ReadFile(filepath.Join(userFfmpegDir, "ffmpeg-linux"))
	if err != nil || string(gotBin) != "user-downloaded" {
		t.Errorf("user ffmpeg-1013/ffmpeg-linux = (%q, %v), want user-downloaded", string(gotBin), err)
	}
	if _, err := os.Lstat(filepath.Join(linksDir, "scion-ffmpeg-1013")); !os.IsNotExist(err) {
		t.Errorf("unexpected scion-ffmpeg-1013 link file created for user real dir: err=%v", err)
	}

	// Foreign symlinks ffmpeg-1011 and ffmpeg-1008 untouched.
	if got, err := os.Readlink(filepath.Join(regDir, "ffmpeg-1011")); err != nil || got != custom1011 {
		t.Errorf("foreign symlink ffmpeg-1011 = (%q, %v), want %q", got, err, custom1011)
	}
	if _, err := os.Lstat(filepath.Join(linksDir, "scion-ffmpeg-1011")); !os.IsNotExist(err) {
		t.Errorf("unexpected scion-ffmpeg-1011 link file created for foreign symlink: err=%v", err)
	}
	if got, err := os.Readlink(filepath.Join(regDir, "ffmpeg-1008")); err != nil || got != custom1008 {
		t.Errorf("foreign symlink ffmpeg-1008 = (%q, %v), want %q", got, err, custom1008)
	}

	// WebKit cache and user .links entry untouched.
	if _, err := os.Stat(webkitMarker); err != nil {
		t.Errorf("webkit cache lost: %v", err)
	}
	if raw, err := os.ReadFile(userLinkFile); err != nil || string(raw) != "/workspace/node_modules/playwright-core" {
		t.Errorf("user .links entry changed: (%q, %v)", string(raw), err)
	}

	// Absent ffmpeg-1012 WAS seeded alongside the preserved entries.
	want1012 := filepath.Join(seedDir, "ffmpeg-1012")
	if got, err := os.Readlink(filepath.Join(regDir, "ffmpeg-1012")); err != nil || got != want1012 {
		t.Errorf("ffmpeg-1012 = (%q, %v), want %q", got, err, want1012)
	}
}

// TestSeedPlaywrightFfmpegCache_RefusesSymlinkAttacksNonFatally verifies that
// symlinks planted by the workload at .cache, ms-playwright, .links, or a
// .links leaf file are refused without following, overwriting, or failing init.
func TestSeedPlaywrightFfmpegCache_RefusesSymlinkAttacksNonFatally(t *testing.T) {
	seedDir := createFakePlaywrightSeedDir(t, "1013")

	t.Run("symlink at .cache", func(t *testing.T) {
		home := evalTempDir(t)
		t.Setenv("XDG_CACHE_HOME", "")
		victimDir := evalTempDir(t)
		if err := os.Symlink(victimDir, filepath.Join(home, ".cache")); err != nil {
			t.Fatal(err)
		}

		seedPlaywrightFfmpegCache(home, seedDir, 0, 0)

		entries, err := os.ReadDir(victimDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("symlink at .cache was followed; victimDir contains %v", entries)
		}
	})

	t.Run("symlink at ms-playwright", func(t *testing.T) {
		home := evalTempDir(t)
		t.Setenv("XDG_CACHE_HOME", "")
		victimDir := evalTempDir(t)
		if err := os.MkdirAll(filepath.Join(home, ".cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victimDir, filepath.Join(home, ".cache", "ms-playwright")); err != nil {
			t.Fatal(err)
		}

		seedPlaywrightFfmpegCache(home, seedDir, 0, 0)

		entries, err := os.ReadDir(victimDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("symlink at ms-playwright was followed; victimDir contains %v", entries)
		}
	})

	t.Run("symlink at .links", func(t *testing.T) {
		home := evalTempDir(t)
		t.Setenv("XDG_CACHE_HOME", "")
		victimDir := evalTempDir(t)
		regDir := filepath.Join(home, ".cache", "ms-playwright")
		if err := os.MkdirAll(regDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victimDir, filepath.Join(regDir, ".links")); err != nil {
			t.Fatal(err)
		}

		seedPlaywrightFfmpegCache(home, seedDir, 0, 0)

		entries, err := os.ReadDir(victimDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("symlink at .links was followed; victimDir contains %v", entries)
		}
	})

	t.Run("symlink at .links/scion-ffmpeg-1013", func(t *testing.T) {
		home := evalTempDir(t)
		t.Setenv("XDG_CACHE_HOME", "")
		victimFile := filepath.Join(evalTempDir(t), "secret")
		if err := os.WriteFile(victimFile, []byte("do-not-overwrite"), 0o600); err != nil {
			t.Fatal(err)
		}
		linksDir := filepath.Join(home, ".cache", "ms-playwright", ".links")
		if err := os.MkdirAll(linksDir, 0o755); err != nil {
			t.Fatal(err)
		}
		linkLeaf := filepath.Join(linksDir, "scion-ffmpeg-1013")
		if err := os.Symlink(victimFile, linkLeaf); err != nil {
			t.Fatal(err)
		}

		seedPlaywrightFfmpegCache(home, seedDir, 0, 0)

		got, err := os.ReadFile(victimFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "do-not-overwrite" {
			t.Errorf("victim file modified via .links symlink: got %q", string(got))
		}
		fi, err := os.Lstat(linkLeaf)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("planted symlink at %s should have been refused in place, got fi=%v err=%v", linkLeaf, fi, err)
		}
	})
}

// TestSeedPlaywrightFfmpegCache_HonorsXDGCacheHomeOnlyUnderAgentHome verifies
// XDG_CACHE_HOME is respected when inside agentHome, and ignored (falling back
// to ~/.cache/ms-playwright without writing outside agentHome) otherwise.
func TestSeedPlaywrightFfmpegCache_HonorsXDGCacheHomeOnlyUnderAgentHome(t *testing.T) {
	seedDir := createFakePlaywrightSeedDir(t, "1013")

	t.Run("inside agentHome", func(t *testing.T) {
		home := evalTempDir(t)
		customCache := filepath.Join(home, ".custom-cache")
		t.Setenv("XDG_CACHE_HOME", customCache)

		seedPlaywrightFfmpegCache(home, seedDir, 0, 0)

		if _, err := os.Readlink(filepath.Join(customCache, "ms-playwright", "ffmpeg-1013")); err != nil {
			t.Errorf("expected seeded symlink in XDG_CACHE_HOME under agentHome: %v", err)
		}
	})

	t.Run("outside agentHome falls back to ~/.cache", func(t *testing.T) {
		home := evalTempDir(t)
		outsideCache := evalTempDir(t)
		t.Setenv("XDG_CACHE_HOME", outsideCache)

		seedPlaywrightFfmpegCache(home, seedDir, 0, 0)

		if _, err := os.Lstat(filepath.Join(outsideCache, "ms-playwright")); !os.IsNotExist(err) {
			t.Errorf("wrote outside agentHome into XDG_CACHE_HOME: err=%v", err)
		}
		if _, err := os.Readlink(filepath.Join(home, ".cache", "ms-playwright", "ffmpeg-1013")); err != nil {
			t.Errorf("expected fallback seed in ~/.cache/ms-playwright: %v", err)
		}
	})
}

// TestRunInit_SeedsPlaywrightFfmpegCacheEndToEnd verifies RunInit wires
// seedPlaywrightFfmpegCache into container startup before launching the child.
func TestRunInit_SeedsPlaywrightFfmpegCacheEndToEnd(t *testing.T) {
	agentHome := evalTempDir(t)
	setupRunInitAsRootlessScion(t, agentHome)
	t.Setenv("XDG_CACHE_HOME", "")

	seedDir := createFakePlaywrightSeedDir(t, "1013")
	origSeedDir := playwrightFfmpegSeedDir
	playwrightFfmpegSeedDir = seedDir
	t.Cleanup(func() { playwrightFfmpegSeedDir = origSeedDir })

	code := RunInit([]string{"true"}, InitRunOptions{DisableTermSignalForwarding: true})
	if code != 0 {
		t.Fatalf("RunInit() = %d, want 0", code)
	}

	gotTarget, err := os.Readlink(filepath.Join(agentHome, ".cache", "ms-playwright", "ffmpeg-1013"))
	if err != nil {
		t.Fatalf("RunInit did not seed ~/.cache/ms-playwright/ffmpeg-1013: %v", err)
	}
	wantTarget := filepath.Join(seedDir, "ffmpeg-1013")
	if gotTarget != wantTarget {
		t.Errorf("seeded symlink -> %q, want %q", gotTarget, wantTarget)
	}
}
