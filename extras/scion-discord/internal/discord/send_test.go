package discord

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

	matches := searchFilesInDir(dir, "hello")
	assert.Len(t, matches, 1)
	assert.Contains(t, matches[0].Path, "hello.txt")
}

func TestSearchFiles_CaseInsensitive(t *testing.T) {
	dir := setupSearchTestDir(t)

	matches := searchFilesInDir(dir, "HELLO")
	assert.Len(t, matches, 1)
	assert.Contains(t, matches[0].Path, "hello.txt")
}

func TestSearchFiles_NoMatches(t *testing.T) {
	dir := setupSearchTestDir(t)

	matches := searchFilesInDir(dir, "nonexistent_xyz_abc")
	assert.Empty(t, matches)
}

func TestSearchFiles_SkipsHiddenDirs(t *testing.T) {
	dir := setupSearchTestDir(t)

	// The .hidden directory contains a file named "secret.txt".
	matches := searchFilesInDir(dir, "secret")
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

	matches := searchFilesInDir(dir, "escape-link")
	assert.Empty(t, matches, "symlinks pointing outside search root should be excluded")
}

// --- isUnderSearchRoot tests ---

func TestIsUnderSearchRoot_ValidPath(t *testing.T) {
	// This test uses a real path under /scion-volumes if it exists,
	// otherwise we test the logic indirectly.
	if _, err := os.Stat(searchRoot); os.IsNotExist(err) {
		t.Skip("searchRoot does not exist on this host")
	}

	// Create a temp file under searchRoot for testing.
	tmpFile, err := os.CreateTemp(searchRoot, "test-send-*.txt")
	if err != nil {
		t.Skip("cannot create temp file in searchRoot")
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	assert.True(t, isUnderSearchRoot(tmpFile.Name()))
}

func TestIsUnderSearchRoot_RejectsOutsidePath(t *testing.T) {
	assert.False(t, isUnderSearchRoot("/etc/passwd"))
	assert.False(t, isUnderSearchRoot("/tmp/something"))
	assert.False(t, isUnderSearchRoot("/root/.ssh/id_rsa"))
}

func TestIsUnderSearchRoot_RejectsTraversal(t *testing.T) {
	assert.False(t, isUnderSearchRoot("/scion-volumes/../etc/passwd"))
	assert.False(t, isUnderSearchRoot("/scion-volumes/./../../etc/shadow"))
}

func TestIsUnderSearchRoot_RejectsSymlinkEscape(t *testing.T) {
	if _, err := os.Stat(searchRoot); os.IsNotExist(err) {
		t.Skip("searchRoot does not exist on this host")
	}

	// Create a temp dir for the symlink target.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o644))

	// Create a symlink inside searchRoot pointing outside.
	symlinkPath := filepath.Join(searchRoot, "test-escape-link-"+randomKey(4))
	err := os.Symlink(outsideFile, symlinkPath)
	if err != nil {
		t.Skip("cannot create symlink in searchRoot")
	}
	defer os.Remove(symlinkPath)

	assert.False(t, isUnderSearchRoot(symlinkPath),
		"symlink inside searchRoot pointing outside should be rejected")
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

// searchFilesInDir is a test helper that runs the search logic against
// an arbitrary directory root instead of the hardcoded searchRoot.
// It duplicates the core logic of searchFiles to allow testing with
// temp directories.
func searchFilesInDir(root, query string) []fileMatch {
	lowerQuery := strings.ToLower(query)
	var matches []fileMatch
	filesWalked := 0

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		filesWalked++
		if filesWalked > maxFilesWalked {
			return filepath.SkipAll
		}

		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}

		if strings.Contains(strings.ToLower(path), lowerQuery) {
			// Verify symlink target is under the root.
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || !strings.HasPrefix(resolved, root) {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return nil
			}
			matches = append(matches, fileMatch{
				Path:    path,
				ModTime: info.ModTime(),
			})
		}

		return nil
	})

	return matches
}
