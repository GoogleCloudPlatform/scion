package discord

import (
	"context"
	"crypto/rand"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/bwmarrin/discordgo"
)

const (
	// DefaultSearchRoot is the default base directory for file searches
	// when no send_search_root is configured.
	DefaultSearchRoot = "/scion-volumes/"

	// maxDiscordFileSize is the Discord attachment size limit (25 MB).
	maxDiscordFileSize = 25 * 1024 * 1024

	// maxSearchResults is the maximum number of file matches returned.
	maxSearchResults = 20

	// maxFilesWalked limits the number of files examined during search
	// to prevent hanging on huge directory trees.
	maxFilesWalked = 100_000

	// sendPathTTL is how long stored file paths remain valid for button clicks.
	sendPathTTL = 15 * time.Minute
)

// sendPathEntry stores a file path with a creation timestamp for TTL expiry.
type sendPathEntry struct {
	Path      string
	CreatedAt time.Time
}

// sendPathStore is a thread-safe in-memory map from short keys to file paths,
// used to work around Discord's 100-character custom ID limit.
type sendPathStore struct {
	mu      sync.Mutex
	entries map[string]sendPathEntry
}

// newSendPathStore creates a new sendPathStore.
func newSendPathStore() *sendPathStore {
	return &sendPathStore{entries: make(map[string]sendPathEntry)}
}

// Put stores a file path under a randomly generated short key and returns
// the key. Expired entries are cleaned up opportunistically.
func (s *sendPathStore) Put(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Opportunistic cleanup of expired entries.
	now := time.Now()
	for k, v := range s.entries {
		if now.Sub(v.CreatedAt) > sendPathTTL {
			delete(s.entries, k)
		}
	}

	key := randomKey(8)
	s.entries[key] = sendPathEntry{Path: path, CreatedAt: now}
	return key
}

// Get retrieves the file path for a key, returning empty string if not found
// or expired.
func (s *sendPathStore) Get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[key]
	if !ok {
		return ""
	}
	if time.Since(entry.CreatedAt) > sendPathTTL {
		delete(s.entries, key)
		return ""
	}
	return entry.Path
}

// randomKey returns a random hex string of the given byte length.
func randomKey(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// globalSendPaths is the package-level path store shared by HandleSend and
// the callback handler.
var globalSendPaths = newSendPathStore()

// fileMatch holds a matched file path and its modification time for sorting.
type fileMatch struct {
	Path        string // resolved (EvalSymlinks) absolute path
	DisplayName string // original filename for button labels (non-empty only for symlinks)
	ModTime     time.Time
}

// safeResolve cleans the path, verifies it is under root using filepath.Rel,
// resolves symlinks, and re-verifies the resolved path is still under root.
// It returns the resolved path or an error if any check fails.
//
// filepath.Rel is used instead of strings.HasPrefix to avoid prefix-confusion
// attacks (e.g. "/scion-volumes" matching "/scion-volumes-evil").
func safeResolve(path, root string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(absRoot)

	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cleaned := filepath.Clean(absPath)

	rel, err := filepath.Rel(root, cleaned)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is not under %q", cleaned, root)
	}

	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", err
	}
	resolved = filepath.Clean(resolved)

	relResolved, err := filepath.Rel(root, resolved)
	if err != nil || relResolved == ".." || strings.HasPrefix(relResolved, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("resolved path %q is not under %q", resolved, root)
	}

	return resolved, nil
}

// isUnderSearchRoot checks that a cleaned, resolved path is under root.
// Both the cleaned path and its symlink-resolved form must be under root
// to prevent directory traversal and symlink escape attacks.
func isUnderSearchRoot(path, root string) bool {
	_, err := safeResolve(path, root)
	return err == nil
}

// safeResolveMulti tries safeResolve against each root and returns the first
// successful result. Returns an error if the path is not under any root.
func safeResolveMulti(path string, roots []string) (string, error) {
	for _, root := range roots {
		if resolved, err := safeResolve(path, root); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("path %q is not under any allowed root", path)
}

// projectSearchRoots computes the search roots for a project by enumerating
// shared directories and the workspace path. It uses os.UserHomeDir() rather
// than hardcoding /home/scion.
func projectSearchRoots(slug, projectID string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	shortUUID := strings.ReplaceAll(projectID, "-", "")
	if len(shortUUID) > 8 {
		shortUUID = shortUUID[:8]
	}
	configDir := filepath.Join(home, ".scion", "project-configs",
		fmt.Sprintf("%s__%s", slug, shortUUID))
	sharedDirsRoot := filepath.Join(configDir, "shared-dirs")

	var roots []string

	// Enumerate shared dir subdirectories.
	entries, err := os.ReadDir(sharedDirsRoot)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				roots = append(roots, filepath.Join(sharedDirsRoot, e.Name()))
			}
		}
	}

	// Include the workspace path.
	workspacePath := filepath.Join(home, ".scion", "projects", slug)
	if info, err := os.Stat(workspacePath); err == nil && info.IsDir() {
		roots = append(roots, workspacePath)
	}

	return roots
}

// containerPathPrefixes lists the path prefixes used inside agent containers
// for shared directories. Both the root-level mount and the in-workspace
// variant are supported.
var containerPathPrefixes = []string{
	"/scion-volumes/",
	"/workspace/.scion-volumes/",
}

// translateContainerPath converts a container-internal path to its host-side
// equivalent. If the path doesn't start with /scion-volumes/ or
// /workspace/.scion-volumes/, it is returned unchanged.
//
// The returned path is NOT validated for confinement; callers MUST pass it
// through safeResolve or safeResolveMulti before use.
func translateContainerPath(path, projectSlug, projectID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	for _, prefix := range containerPathPrefixes {
		if !strings.HasPrefix(path, prefix) {
			// Also match the prefix without trailing slash (bare shared dir name).
			bare := strings.TrimSuffix(prefix, "/")
			if path == bare {
				// e.g. "/scion-volumes" with no shared dir name — can't translate.
				return path
			}
			continue
		}

		// Strip the prefix to get "<sharedDirName>/remainder" or just "<sharedDirName>".
		after := strings.TrimPrefix(path, prefix)
		if after == "" {
			// Path is exactly the prefix (e.g. "/scion-volumes/") — no shared
			// dir name, so we can't translate.
			return path
		}

		// Split into shared dir name and optional remainder.
		var sharedDirName, remainder string
		if idx := strings.IndexByte(after, '/'); idx >= 0 {
			sharedDirName = after[:idx]
			remainder = after[idx+1:]
		} else {
			sharedDirName = after
		}

		hostSharedDir := config.SharedDirHostPath(home, projectSlug, projectID, sharedDirName)
		return filepath.Join(hostSharedDir, remainder)
	}
	return path
}

// HandleSend handles the /scion send <path> command.
// It resolves the channel's project binding to determine per-project search
// roots. If the channel is not linked and no send_search_root override is
// configured, it returns an error asking the user to run /scion setup.
// If path is an absolute path to an existing file under a valid root, it sends
// it directly. Otherwise it searches all roots for matching files and presents
// buttons for selection.
func (h *CommandHandler) HandleSend(s *discordgo.Session, i *discordgo.InteractionCreate) {
	pathArg := getSubcommandOption(i, "path")
	if pathArg == "" {
		h.followup(s, i, "Please provide a file path or search term.")
		return
	}

	// Resolve the channel link once for project context. The result is shared
	// by both path translation and search-root resolution to avoid a duplicate
	// store round-trip and a consistency hazard if the link changes between calls.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link, err := resolveChannelLink(ctx, s, h.store, i.ChannelID)
	if err != nil {
		h.log.Error("Failed to resolve channel link", "channel_id", i.ChannelID, "error", err)
	}
	if link != nil && link.Active {
		pathArg = translateContainerPath(pathArg, link.ProjectSlug, link.ProjectID)
	}

	// Resolve search roots: per-project roots from channel link, with
	// send_search_root as override/fallback.
	roots := h.resolveSearchRoots(link)
	if len(roots) == 0 {
		h.followup(s, i, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	// Case 1: Absolute path pointing to an existing file, confined to any root.
	if filepath.IsAbs(pathArg) {
		if resolved, err := safeResolveMulti(pathArg, roots); err == nil {
			info, err := os.Stat(resolved)
			if err == nil && !info.IsDir() {
				h.sendFile(s, i, resolved, info)
				return
			}
		}
	}

	// Case 2: Search for files matching the argument across all roots.
	var matches []fileMatch
	for _, root := range roots {
		matches = append(matches, searchFiles(root, pathArg)...)
	}

	if len(matches) == 0 {
		h.followup(s, i, fmt.Sprintf("No files found matching '%s'", pathArg))
		return
	}

	// Sort by modification time (most recent first).
	sort.Slice(matches, func(a, b int) bool {
		return matches[a].ModTime.After(matches[b].ModTime)
	})

	if len(matches) > maxSearchResults {
		matches = matches[:maxSearchResults]
	}

	// Build buttons. Store full paths in the path store to avoid
	// exceeding Discord's 100-char custom ID limit.
	// N3: detect duplicate basenames to disambiguate labels.
	labels := buildButtonLabels(matches)

	var rows []discordgo.MessageComponent
	var buttons []discordgo.MessageComponent

	for idx, m := range matches {
		key := globalSendPaths.Put(m.Path)
		buttons = append(buttons, discordgo.Button{
			Label:    labels[idx],
			Style:    discordgo.SecondaryButton,
			CustomID: fmt.Sprintf("send:file:%s", key),
		})
		if len(buttons) == 5 || idx == len(matches)-1 {
			rows = append(rows, discordgo.ActionsRow{Components: buttons})
			buttons = nil
		}
		// Discord allows max 5 action rows.
		if len(rows) >= 5 {
			break
		}
	}

	_, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:    fmt.Sprintf("Found %d file(s) matching '%s'. Select one to send:", len(matches), pathArg),
		Components: rows,
	})
	if err != nil {
		h.log.Error("Failed to send file search results", "error", err)
	}
}

// resolveSearchRoots determines the search roots for a /scion send command.
// If the channel is linked to a project, per-project roots are computed.
// If send_search_root is configured (h.searchRoot != DefaultSearchRoot), it is
// used as an override when present, or as a fallback when no project link exists.
//
// The caller resolves the channel link once and passes it in so that the same
// result is shared with path translation (avoiding a duplicate store round-trip).
func (h *CommandHandler) resolveSearchRoots(link *ChannelLink) []string {
	configuredOverride := h.searchRoot != DefaultSearchRoot && h.searchRoot != ""

	if link == nil || !link.Active {
		// No project link — use configured override if available.
		if configuredOverride {
			return []string{h.searchRoot}
		}
		return nil
	}

	// Compute per-project roots from the channel link.
	roots := projectSearchRoots(link.ProjectSlug, link.ProjectID)

	// If a configured override is present, prepend it.
	if configuredOverride {
		roots = append([]string{h.searchRoot}, roots...)
	}

	// Fallback: if no project roots were found, use the default search root.
	// Note: when configuredOverride is true, h.searchRoot was already prepended
	// above, so len(roots) >= 1 and this branch is only reachable without an override.
	if len(roots) == 0 {
		return []string{DefaultSearchRoot}
	}

	return deduplicateRoots(roots)
}

// deduplicateRoots removes exact duplicates (after filepath.Clean) and roots
// that are subdirectories of other roots in the list, since the parent root's
// search will already cover the subdirectory.
func deduplicateRoots(roots []string) []string {
	seen := make(map[string]bool)
	var unique []string
	for _, r := range roots {
		cleaned := filepath.Clean(r)
		if !seen[cleaned] {
			seen[cleaned] = true
			unique = append(unique, cleaned)
		}
	}
	// Remove roots that are subdirectories of other roots.
	var filtered []string
	for _, r := range unique {
		isChild := false
		for _, other := range unique {
			if r != other {
				rel, err := filepath.Rel(other, r)
				if err == nil && !strings.HasPrefix(rel, "..") {
					isChild = true
					break
				}
			}
		}
		if !isChild {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// displayBase returns the display name for a match. If DisplayName is set
// (symlink matches), it is returned; otherwise filepath.Base of Path is used.
func (m fileMatch) displayBase() string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return filepath.Base(m.Path)
}

// buildButtonLabels returns a label for each match. When multiple matches
// share the same display name, the parent directory is prepended to disambiguate.
func buildButtonLabels(matches []fileMatch) []string {
	labels := make([]string, len(matches))

	// Count how many times each display name appears.
	baseCounts := make(map[string]int)
	for _, m := range matches {
		baseCounts[m.displayBase()]++
	}

	for idx, m := range matches {
		base := m.displayBase()
		if baseCounts[base] > 1 {
			parent := filepath.Base(filepath.Dir(m.Path))
			label := parent + "/" + base
			// Discord button labels max 80 chars; use rune slicing
			// to avoid cutting multi-byte UTF-8 characters.
			runes := []rune(label)
			if len(runes) > 80 {
				label = string(runes[:80])
			}
			labels[idx] = label
		} else {
			label := base
			runes := []rune(label)
			if len(runes) > 80 {
				label = string(runes[:80])
			}
			labels[idx] = label
		}
	}
	return labels
}

// sendFile reads a file and sends it as a Discord attachment.
func (h *CommandHandler) sendFile(s *discordgo.Session, i *discordgo.InteractionCreate, path string, info os.FileInfo) {
	if info.Size() > maxDiscordFileSize {
		h.followup(s, i, fmt.Sprintf("File too large to send (%.1f MB, limit is 25 MB).",
			float64(info.Size())/(1024*1024)))
		return
	}

	file, err := os.Open(path)
	if err != nil {
		h.log.Error("Failed to open file for send", "path", path, "error", err)
		h.followup(s, i, fmt.Sprintf("Could not read file: %s", filepath.Base(path)))
		return
	}
	defer file.Close()

	_, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: fmt.Sprintf("📎 `%s`", filepath.Base(path)),
		Files: []*discordgo.File{
			{Name: filepath.Base(path), Reader: file},
		},
	})
	if err != nil {
		h.log.Error("Failed to send file attachment", "path", path, "error", err)
		h.followup(s, i, "Failed to send file. Please try again.")
	}
}

// searchFiles walks root looking for files whose path contains the given
// query (case-insensitive). Symlinks that resolve outside root are excluded
// to prevent symlink escape attacks.
func searchFiles(root, query string) []fileMatch {
	lowerQuery := strings.ToLower(query)
	var matches []fileMatch
	filesWalked := 0

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}

		filesWalked++
		if filesWalked > maxFilesWalked {
			return filepath.SkipAll
		}

		// Skip hidden directories (any directory starting with ".").
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}

		// Match file paths case-insensitively.
		if strings.Contains(strings.ToLower(path), lowerQuery) {
			if d.Type()&fs.ModeSymlink != 0 {
				// Symlink: resolve and verify target is still under root.
				resolved, err := filepath.EvalSymlinks(path)
				if err != nil || !isUnderSearchRoot(resolved, root) {
					return nil
				}
				// Filter out symlinks pointing to directories.
				targetInfo, err := os.Stat(resolved)
				if err != nil || targetInfo.IsDir() {
					return nil
				}
				// Store the resolved target path (defense-in-depth against
				// TOCTOU symlink retargeting) and preserve the original
				// symlink name for button labels via DisplayName.
				matches = append(matches, fileMatch{
					Path:        resolved,
					DisplayName: filepath.Base(path),
					ModTime:     targetInfo.ModTime(),
				})
			} else {
				// Regular file: no symlink resolution needed.
				info, err := d.Info()
				if err != nil {
					return nil
				}
				matches = append(matches, fileMatch{
					Path:    path,
					ModTime: info.ModTime(),
				})
			}
		}

		return nil
	})

	return matches
}

// handleSendFileCallback is called by the CallbackHandler when a send:file
// button is clicked. It looks up the stored path and sends the file.
//
// The stored path was already validated via safeResolve at search time, so the
// callback only needs to verify the file still exists and is readable. The
// path confinement check is not repeated here because the search roots may
// differ from the callback handler's single searchRoot (per-project roots are
// resolved dynamically based on the channel's project binding).
func handleSendFileCallback(s *discordgo.Session, i *discordgo.InteractionCreate, key string, log *slog.Logger) {
	path := globalSendPaths.Get(key)
	if path == "" {
		respondSendUpdate(s, i, "This file link has expired. Please use `/scion send` again.", log)
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		log.Error("Failed to stat file for send callback", "path", path, "error", err)
		respondSendUpdate(s, i, fmt.Sprintf("File not found: %s", filepath.Base(path)), log)
		return
	}

	if info.IsDir() {
		respondSendUpdate(s, i, "Cannot send a directory.", log)
		return
	}

	if info.Size() > maxDiscordFileSize {
		respondSendUpdate(s, i, fmt.Sprintf("File too large to send (%.1f MB, limit is 25 MB).",
			float64(info.Size())/(1024*1024)), log)
		return
	}

	file, err := os.Open(path)
	if err != nil {
		log.Error("Failed to open file for send callback", "path", path, "error", err)
		respondSendUpdate(s, i, fmt.Sprintf("Could not read file: %s", filepath.Base(path)), log)
		return
	}
	defer file.Close()

	// N2: Edit original button message to indicate which file was sent.
	sentContent := fmt.Sprintf("Sent file: `%s`", filepath.Base(path))
	respondSendUpdate(s, i, sentContent, log)

	// Send the file as a new followup message.
	_, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: fmt.Sprintf("📎 `%s`", filepath.Base(path)),
		Files: []*discordgo.File{
			{Name: filepath.Base(path), Reader: file},
		},
	})
	if err != nil {
		log.Error("Failed to send file from callback", "path", path, "error", err)
	}
}

// respondSendUpdate edits the interaction response for send button callbacks.
func respondSendUpdate(s *discordgo.Session, i *discordgo.InteractionCreate, content string, log *slog.Logger) {
	edit := &discordgo.WebhookEdit{
		Content: &content,
	}
	empty := []discordgo.MessageComponent{}
	edit.Components = &empty
	_, err := s.InteractionResponseEdit(i.Interaction, edit)
	if err != nil {
		log.Error("Failed to edit send interaction response", "error", err)
	}
}
