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
