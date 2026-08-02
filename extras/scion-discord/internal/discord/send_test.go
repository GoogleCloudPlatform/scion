package discord

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- sendPathStore tests ---

func TestSendPathStore_PutAndGet(t *testing.T) {
	store := newSendPathStore()
	key := store.Put("/some/file.txt")
	assert.NotEmpty(t, key)

	got := store.Get(key)
	assert.Equal(t, "/some/file.txt", got)
}

func TestSendPathStore_GetUnknownKey(t *testing.T) {
	store := newSendPathStore()
	got := store.Get("nonexistent")
	assert.Empty(t, got)
}

func TestSendPathStore_TTLExpiry(t *testing.T) {
	store := newSendPathStore()

	// Manually insert an entry that is already expired.
	store.mu.Lock()
	store.entries["old"] = sendPathEntry{
		Path:      "/expired/file.txt",
		CreatedAt: time.Now().Add(-sendPathTTL - time.Minute),
	}
	store.mu.Unlock()

	got := store.Get("old")
	assert.Empty(t, got, "expired entry should return empty")
}

func TestSendPathStore_OpportunisticCleanup(t *testing.T) {
	store := newSendPathStore()

	// Insert an expired entry manually.
	store.mu.Lock()
	store.entries["expired"] = sendPathEntry{
		Path:      "/old.txt",
		CreatedAt: time.Now().Add(-sendPathTTL - time.Minute),
	}
	store.mu.Unlock()

	// Put a new entry — should trigger cleanup.
	store.Put("/new.txt")

	store.mu.Lock()
	_, exists := store.entries["expired"]
	store.mu.Unlock()
	assert.False(t, exists, "expired entry should be cleaned up during Put")
}

func TestSendPathStore_ConcurrentAccess(t *testing.T) {
	store := newSendPathStore()
	var wg sync.WaitGroup

	// Concurrent writers.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := store.Put("/concurrent/file.txt")
			// Read back — may or may not get our entry if another goroutine
			// expired it, but should not panic.
			store.Get(key)
		}(i)
	}

	// Concurrent readers.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store.Get("somekey")
		}()
	}

	wg.Wait()
}

// --- searchFiles tests ---

func TestSearchFiles_MatchesSubstring(t *testing.T) {
	dir := setupSearchTestDir(t)

	matches := searchFiles(dir, "hello")
	assert.Len(t, matches, 1)
	assert.Contains(t, matches[0].Path, "hello.txt")
}

func TestSearchFiles_CaseInsensitive(t *testing.T) {
	dir := setupSearchTestDir(t)

	matches := searchFiles(dir, "HELLO")
	assert.Len(t, matches, 1)
	assert.Contains(t, matches[0].Path, "hello.txt")
}

func TestSearchFiles_NoMatches(t *testing.T) {
	dir := setupSearchTestDir(t)

	matches := searchFiles(dir, "nonexistent_xyz_abc")
	assert.Empty(t, matches)
}

func TestSearchFiles_SkipsHiddenDirs(t *testing.T) {
	dir := setupSearchTestDir(t)

	// The .hidden directory contains a file named "secret.txt".
	matches := searchFiles(dir, "secret")
	assert.Empty(t, matches, "files in hidden directories should be skipped")
}

func TestSearchFiles_SymlinkOutsideRoot(t *testing.T) {
	dir := setupSearchTestDir(t)

	// Create a file outside the search root.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "external.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("external"), 0o644))

	// Create a symlink inside the search root pointing outside.
	symlinkPath := filepath.Join(dir, "escape-link.txt")
	require.NoError(t, os.Symlink(outsideFile, symlinkPath))

	matches := searchFiles(dir, "escape-link")
	assert.Empty(t, matches, "symlinks pointing outside search root should be excluded")
}

func TestSearchFiles_SymlinkPrefixConfusion(t *testing.T) {
	// Create a root without trailing slash and a sibling with shared prefix.
	root := t.TempDir() // e.g. /tmp/TestXYZ123
	siblingDir := root + "-secrets"
	require.NoError(t, os.MkdirAll(siblingDir, 0o755))
	secretFile := filepath.Join(siblingDir, "key.pem")
	require.NoError(t, os.WriteFile(secretFile, []byte("secret-key"), 0o644))

	// Create a symlink inside root pointing to the sibling directory's file.
	symlinkPath := filepath.Join(root, "sneaky-link.pem")
	require.NoError(t, os.Symlink(secretFile, symlinkPath))

	// Search with root that has no trailing slash — must not leak the sibling file.
	matches := searchFiles(root, "sneaky-link")
	assert.Empty(t, matches, "symlinks resolving to a sibling dir with shared prefix must be excluded")
}

// --- safeResolve tests ---

func TestSafeResolve_ValidPath(t *testing.T) {
	if _, err := os.Stat(DefaultSearchRoot); os.IsNotExist(err) {
		t.Skip("DefaultSearchRoot does not exist on this host")
	}

	tmpFile, err := os.CreateTemp(DefaultSearchRoot, "test-send-*.txt")
	if err != nil {
		t.Skip("cannot create temp file in DefaultSearchRoot")
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	resolved, err := safeResolve(tmpFile.Name(), DefaultSearchRoot)
	assert.NoError(t, err)
	assert.Equal(t, tmpFile.Name(), resolved)
}

func TestSafeResolve_RejectsOutsidePath(t *testing.T) {
	_, err := safeResolve("/etc/passwd", DefaultSearchRoot)
	assert.Error(t, err)
	_, err = safeResolve("/tmp/something", DefaultSearchRoot)
	assert.Error(t, err)
}

func TestSafeResolve_RejectsTraversal(t *testing.T) {
	_, err := safeResolve("/scion-volumes/../etc/passwd", DefaultSearchRoot)
	assert.Error(t, err)
	_, err = safeResolve("/scion-volumes/./../../etc/shadow", DefaultSearchRoot)
	assert.Error(t, err)
}

func TestSafeResolve_RejectsSymlinkEscape(t *testing.T) {
	if _, err := os.Stat(DefaultSearchRoot); os.IsNotExist(err) {
		t.Skip("DefaultSearchRoot does not exist on this host")
	}

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o644))

	symlinkPath := filepath.Join(DefaultSearchRoot, "test-escape-link-"+randomKey(4))
	err := os.Symlink(outsideFile, symlinkPath)
	if err != nil {
		t.Skip("cannot create symlink in DefaultSearchRoot")
	}
	defer os.Remove(symlinkPath)

	_, resolveErr := safeResolve(symlinkPath, DefaultSearchRoot)
	assert.Error(t, resolveErr, "symlink inside DefaultSearchRoot pointing outside should be rejected")
}

// --- isUnderSearchRoot tests ---

func TestIsUnderSearchRoot_ValidPath(t *testing.T) {
	// This test uses a real path under /scion-volumes if it exists,
	// otherwise we test the logic indirectly.
	if _, err := os.Stat(DefaultSearchRoot); os.IsNotExist(err) {
		t.Skip("DefaultSearchRoot does not exist on this host")
	}

	// Create a temp file under DefaultSearchRoot for testing.
	tmpFile, err := os.CreateTemp(DefaultSearchRoot, "test-send-*.txt")
	if err != nil {
		t.Skip("cannot create temp file in DefaultSearchRoot")
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	assert.True(t, isUnderSearchRoot(tmpFile.Name(), DefaultSearchRoot))
}

func TestIsUnderSearchRoot_RejectsOutsidePath(t *testing.T) {
	assert.False(t, isUnderSearchRoot("/etc/passwd", DefaultSearchRoot))
	assert.False(t, isUnderSearchRoot("/tmp/something", DefaultSearchRoot))
	assert.False(t, isUnderSearchRoot("/root/.ssh/id_rsa", DefaultSearchRoot))
}

func TestIsUnderSearchRoot_RejectsTraversal(t *testing.T) {
	assert.False(t, isUnderSearchRoot("/scion-volumes/../etc/passwd", DefaultSearchRoot))
	assert.False(t, isUnderSearchRoot("/scion-volumes/./../../etc/shadow", DefaultSearchRoot))
}

func TestIsUnderSearchRoot_RejectsSymlinkEscape(t *testing.T) {
	if _, err := os.Stat(DefaultSearchRoot); os.IsNotExist(err) {
		t.Skip("DefaultSearchRoot does not exist on this host")
	}

	// Create a temp dir for the symlink target.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o644))

	// Create a symlink inside DefaultSearchRoot pointing outside.
	symlinkPath := filepath.Join(DefaultSearchRoot, "test-escape-link-"+randomKey(4))
	err := os.Symlink(outsideFile, symlinkPath)
	if err != nil {
		t.Skip("cannot create symlink in DefaultSearchRoot")
	}
	defer os.Remove(symlinkPath)

	assert.False(t, isUnderSearchRoot(symlinkPath, DefaultSearchRoot),
		"symlink inside DefaultSearchRoot pointing outside should be rejected")
}

// --- safeResolve with custom root tests ---

func TestSafeResolve_CustomRoot(t *testing.T) {
	root := t.TempDir() + "/"
	f, err := os.CreateTemp(root, "custom-*.txt")
	require.NoError(t, err)
	f.Close()

	resolved, err := safeResolve(f.Name(), root)
	assert.NoError(t, err)
	assert.Equal(t, f.Name(), resolved)
}

func TestSafeResolve_CustomRoot_RejectsOutside(t *testing.T) {
	root := t.TempDir() + "/"
	_, err := safeResolve("/etc/passwd", root)
	assert.Error(t, err)
}

func TestIsUnderSearchRoot_CustomRoot(t *testing.T) {
	root := t.TempDir() + "/"
	f, err := os.CreateTemp(root, "custom-*.txt")
	require.NoError(t, err)
	f.Close()

	assert.True(t, isUnderSearchRoot(f.Name(), root))
	assert.False(t, isUnderSearchRoot("/etc/passwd", root))
}

func TestSafeResolve_CustomRoot_NoTrailingSlash(t *testing.T) {
	root := t.TempDir() // no trailing slash
	outside := root + "-evil"
	require.NoError(t, os.MkdirAll(outside, 0o755))
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("x"), 0o644))

	_, err := safeResolve(secret, root)
	assert.Error(t, err, "must not accept a file under a sibling with a shared prefix")
}

func TestIsUnderSearchRoot_CustomRoot_NoTrailingSlash(t *testing.T) {
	root := t.TempDir() // no trailing slash
	outside := root + "-evil"
	require.NoError(t, os.MkdirAll(outside, 0o755))
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("x"), 0o644))

	assert.False(t, isUnderSearchRoot(secret, root),
		"must not accept a file under a sibling with a shared prefix")
}

// --- buildButtonLabels tests ---

func TestBuildButtonLabels_UniqueBasenames(t *testing.T) {
	matches := []fileMatch{
		{Path: "/scion-volumes/a/foo.txt"},
		{Path: "/scion-volumes/b/bar.txt"},
	}
	labels := buildButtonLabels(matches)
	assert.Equal(t, "foo.txt", labels[0])
	assert.Equal(t, "bar.txt", labels[1])
}

func TestBuildButtonLabels_DuplicateBasenames(t *testing.T) {
	matches := []fileMatch{
		{Path: "/scion-volumes/project-a/README.md"},
		{Path: "/scion-volumes/project-b/README.md"},
	}
	labels := buildButtonLabels(matches)
	assert.Equal(t, "project-a/README.md", labels[0])
	assert.Equal(t, "project-b/README.md", labels[1])
}

// --- safeResolveMulti tests ---

func TestSafeResolveMulti_MatchesFirstRoot(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()
	f, err := os.CreateTemp(root1, "multi-*.txt")
	require.NoError(t, err)
	f.Close()

	resolved, err := safeResolveMulti(f.Name(), []string{root1, root2})
	assert.NoError(t, err)
	assert.Equal(t, f.Name(), resolved)
}

func TestSafeResolveMulti_MatchesSecondRoot(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()
	f, err := os.CreateTemp(root2, "multi-*.txt")
	require.NoError(t, err)
	f.Close()

	resolved, err := safeResolveMulti(f.Name(), []string{root1, root2})
	assert.NoError(t, err)
	assert.Equal(t, f.Name(), resolved)
}

func TestSafeResolveMulti_RejectsOutsideAll(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()

	_, err := safeResolveMulti("/etc/passwd", []string{root1, root2})
	assert.Error(t, err)
}

func TestSafeResolveMulti_EmptyRoots(t *testing.T) {
	_, err := safeResolveMulti("/etc/passwd", nil)
	assert.Error(t, err)
}

// --- safeResolve with root=/ edge case ---

func TestSafeResolve_RootSlash(t *testing.T) {
	// When root is /, any absolute path should resolve successfully.
	tmpFile, err := os.CreateTemp(t.TempDir(), "rootslash-*.txt")
	require.NoError(t, err)
	tmpFile.Close()

	resolved, err := safeResolve(tmpFile.Name(), "/")
	assert.NoError(t, err)
	assert.Equal(t, tmpFile.Name(), resolved)
}

func TestSafeResolve_RootSlash_RejectsTraversal(t *testing.T) {
	// Path traversal beyond / should still be caught — cleaned path stays under /.
	tmpFile, err := os.CreateTemp(t.TempDir(), "rootslash-*.txt")
	require.NoError(t, err)
	tmpFile.Close()

	resolved, err := safeResolve("/../"+tmpFile.Name(), "/")
	assert.NoError(t, err)
	assert.Equal(t, tmpFile.Name(), resolved)
}

// --- searchFiles with multiple roots ---

func TestSearchFiles_MultipleRoots(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir1, "report.txt"), []byte("r"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir2, "data.txt"), []byte("d"), 0o644))

	// Search each root separately and merge (mimicking HandleSend behavior).
	var matches []fileMatch
	for _, root := range []string{dir1, dir2} {
		matches = append(matches, searchFiles(root, ".txt")...)
	}

	assert.Len(t, matches, 2)
	paths := []string{matches[0].Path, matches[1].Path}
	assert.Contains(t, paths[0]+paths[1], "report.txt")
	assert.Contains(t, paths[0]+paths[1], "data.txt")
}

// --- projectSearchRoots tests ---

func TestProjectSearchRoots_DiscoversDirs(t *testing.T) {
	// Set up a fake home directory structure.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	slug := "test-proj"
	projectID := "550e8400-e29b-41d4-a716-446655440000"

	// Compute expected paths.
	shortUUID := "550e8400"
	configDir := filepath.Join(fakeHome, ".scion", "project-configs",
		slug+"__"+shortUUID)
	sharedDirsRoot := filepath.Join(configDir, "shared-dirs")

	// Create shared dir subdirectories.
	require.NoError(t, os.MkdirAll(filepath.Join(sharedDirsRoot, "scratchpad"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(sharedDirsRoot, "cache"), 0o755))

	// Create workspace directory.
	workspaceDir := filepath.Join(fakeHome, ".scion", "projects", slug)
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))

	roots := projectSearchRoots(slug, projectID)

	assert.Len(t, roots, 3)
	assert.Contains(t, roots, filepath.Join(sharedDirsRoot, "cache"))
	assert.Contains(t, roots, filepath.Join(sharedDirsRoot, "scratchpad"))
	assert.Contains(t, roots, workspaceDir)
}

func TestProjectSearchRoots_NoSharedDirs(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	slug := "empty-proj"
	projectID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Create only workspace directory.
	workspaceDir := filepath.Join(fakeHome, ".scion", "projects", slug)
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))

	roots := projectSearchRoots(slug, projectID)

	assert.Len(t, roots, 1)
	assert.Equal(t, workspaceDir, roots[0])
}

func TestProjectSearchRoots_NothingExists(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	roots := projectSearchRoots("nonexistent", "00000000-0000-0000-0000-000000000000")

	assert.Empty(t, roots)
}

// --- searchFiles symlink resolved path tests ---

func TestSearchFiles_SymlinkStoresResolvedPath(t *testing.T) {
	dir := t.TempDir()

	// Create a real file.
	realFile := filepath.Join(dir, "real-data.txt")
	require.NoError(t, os.WriteFile(realFile, []byte("data"), 0o644))

	// Create a symlink to the real file within the same root.
	symlinkPath := filepath.Join(dir, "link-to-data.txt")
	require.NoError(t, os.Symlink(realFile, symlinkPath))

	matches := searchFiles(dir, "link-to-data")
	require.Len(t, matches, 1)

	// Path should be the resolved (real) path, not the symlink.
	assert.Equal(t, realFile, matches[0].Path)
	// DisplayName should preserve the original symlink basename.
	assert.Equal(t, "link-to-data.txt", matches[0].DisplayName)
}

func TestSearchFiles_RegularFileNoDisplayName(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("x"), 0o644))

	matches := searchFiles(dir, "plain")
	require.Len(t, matches, 1)
	assert.Empty(t, matches[0].DisplayName, "regular files should not set DisplayName")
}

// --- buildButtonLabels with DisplayName ---

func TestBuildButtonLabels_UsesDisplayName(t *testing.T) {
	matches := []fileMatch{
		{Path: "/resolved/target/real.txt", DisplayName: "my-link.txt"},
		{Path: "/scion-volumes/b/bar.txt"},
	}
	labels := buildButtonLabels(matches)
	assert.Equal(t, "my-link.txt", labels[0])
	assert.Equal(t, "bar.txt", labels[1])
}

// --- deduplicateRoots tests ---

func TestDeduplicateRoots_ExactDuplicates(t *testing.T) {
	roots := []string{"/a/b", "/c/d", "/a/b"}
	result := deduplicateRoots(roots)
	assert.Equal(t, []string{"/a/b", "/c/d"}, result)
}

func TestDeduplicateRoots_SubdirectoryRemoved(t *testing.T) {
	roots := []string{"/a", "/a/b/c"}
	result := deduplicateRoots(roots)
	assert.Equal(t, []string{"/a"}, result)
}

func TestDeduplicateRoots_NoOverlap(t *testing.T) {
	roots := []string{"/a", "/b", "/c"}
	result := deduplicateRoots(roots)
	assert.Equal(t, []string{"/a", "/b", "/c"}, result)
}

func TestDeduplicateRoots_CleansPaths(t *testing.T) {
	roots := []string{"/a/b/", "/a/b"}
	result := deduplicateRoots(roots)
	assert.Len(t, result, 1)
	assert.Equal(t, "/a/b", result[0])
}

func TestDeduplicateRoots_SingleRoot(t *testing.T) {
	roots := []string{"/only"}
	result := deduplicateRoots(roots)
	assert.Equal(t, []string{"/only"}, result)
}

func TestDeduplicateRoots_Empty(t *testing.T) {
	result := deduplicateRoots(nil)
	assert.Nil(t, result)
}

// --- translateContainerPath tests ---

func TestTranslateContainerPath_ScionVolumes(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	slug := "my-proj"
	projectID := "aabbccdd-1122-3344-5566-778899aabbcc"

	got := translateContainerPath("/scion-volumes/scratchpad/foo/bar.md", slug, projectID)

	expected := filepath.Join(fakeHome, ".scion", "project-configs",
		"my-proj__aabbccdd", "shared-dirs", "scratchpad", "foo", "bar.md")
	assert.Equal(t, expected, got)
}

func TestTranslateContainerPath_ScionVolumesNoRemainder(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	slug := "my-proj"
	projectID := "aabbccdd-1122-3344-5566-778899aabbcc"

	got := translateContainerPath("/scion-volumes/scratchpad", slug, projectID)

	expected := filepath.Join(fakeHome, ".scion", "project-configs",
		"my-proj__aabbccdd", "shared-dirs", "scratchpad")
	assert.Equal(t, expected, got)
}

func TestTranslateContainerPath_WorkspaceScionVolumes(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	slug := "my-proj"
	projectID := "aabbccdd-1122-3344-5566-778899aabbcc"

	got := translateContainerPath("/workspace/.scion-volumes/scratchpad/foo.md", slug, projectID)

	expected := filepath.Join(fakeHome, ".scion", "project-configs",
		"my-proj__aabbccdd", "shared-dirs", "scratchpad", "foo.md")
	assert.Equal(t, expected, got)
}

func TestTranslateContainerPath_NonContainerPathUnchanged(t *testing.T) {
	got := translateContainerPath("/home/scion/something", "slug", "id")
	assert.Equal(t, "/home/scion/something", got)
}

func TestTranslateContainerPath_PartialQueryUnchanged(t *testing.T) {
	got := translateContainerPath("report.txt", "slug", "id")
	assert.Equal(t, "report.txt", got)
}

func TestTranslateContainerPath_BarePrefix(t *testing.T) {
	// "/scion-volumes" with no shared dir name — can't translate.
	got := translateContainerPath("/scion-volumes", "slug", "id")
	assert.Equal(t, "/scion-volumes", got)
}

func TestTranslateContainerPath_TrailingSlashOnly(t *testing.T) {
	// "/scion-volumes/" with no shared dir name — can't translate.
	got := translateContainerPath("/scion-volumes/", "slug", "id")
	assert.Equal(t, "/scion-volumes/", got)
}

// --- test helpers ---

// setupSearchTestDir creates a temporary directory structure for search tests.
// Structure:
//
//	<root>/
//	  hello.txt
//	  subdir/
//	    world.txt
//	  .hidden/
//	    secret.txt
func setupSearchTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "subdir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "subdir", "world.txt"), []byte("world"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".hidden"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden", "secret.txt"), []byte("secret"), 0o644))

	return dir
}
