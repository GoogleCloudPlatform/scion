package discord

import (
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
	Path    string
	ModTime time.Time
}

// safeResolve cleans the path, verifies it starts with root, resolves
// symlinks, and re-verifies the resolved path is still under root.
// It returns the resolved path or an error if any check fails.
func safeResolve(path, root string) (string, error) {
	cleaned := filepath.Clean(path)
	if !strings.HasPrefix(cleaned, root) {
		return "", fmt.Errorf("path %q does not start with %q", cleaned, root)
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(resolved, root) {
		return "", fmt.Errorf("resolved path %q does not start with %q", resolved, root)
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

// HandleSend handles the /scion send <path> command.
// If path is an absolute path to an existing file under searchRoot, it sends
// it directly. Otherwise it searches /scion-volumes/ for matching files and
// presents buttons for selection.
func (h *CommandHandler) HandleSend(s *discordgo.Session, i *discordgo.InteractionCreate) {
	pathArg := getSubcommandOption(i, "path")
	if pathArg == "" {
		h.followup(s, i, "Please provide a file path or search term.")
		return
	}

	// Case 1: Absolute path pointing to an existing file, confined to searchRoot.
	if filepath.IsAbs(pathArg) {
		if resolved, err := safeResolve(pathArg, h.searchRoot); err == nil {
			info, err := os.Stat(resolved)
			if err == nil && !info.IsDir() {
				h.sendFile(s, i, resolved, info)
				return
			}
		}
	}

	// Case 2: Search for files matching the argument.
	matches := searchFiles(h.searchRoot, pathArg)

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

	_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:    fmt.Sprintf("Found %d file(s) matching '%s'. Select one to send:", len(matches), pathArg),
		Components: rows,
	})
	if err != nil {
		h.log.Error("Failed to send file search results", "error", err)
	}
}

// buildButtonLabels returns a label for each match. When multiple matches
// share the same basename, the parent directory is prepended to disambiguate.
func buildButtonLabels(matches []fileMatch) []string {
	labels := make([]string, len(matches))

	// Count how many times each basename appears.
	baseCounts := make(map[string]int)
	for _, m := range matches {
		baseCounts[filepath.Base(m.Path)]++
	}

	for idx, m := range matches {
		base := filepath.Base(m.Path)
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
				if err != nil || !strings.HasPrefix(resolved, root) {
					return nil
				}
				// Filter out symlinks pointing to directories.
				targetInfo, err := os.Stat(resolved)
				if err != nil || targetInfo.IsDir() {
					return nil
				}
				// Use target file ModTime for symlinks.
				matches = append(matches, fileMatch{
					Path:    path,
					ModTime: targetInfo.ModTime(),
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
func handleSendFileCallback(s *discordgo.Session, i *discordgo.InteractionCreate, key, root string, log *slog.Logger) {
	path := globalSendPaths.Get(key)
	if path == "" {
		respondSendUpdate(s, i, "This file link has expired. Please use `/scion send` again.", log)
		return
	}

	// Verify path is still confined to the search root (resolves symlinks).
	resolved, err := safeResolve(path, root)
	if err != nil {
		log.Warn("Send callback path failed confinement check", "path", path, "error", err)
		respondSendUpdate(s, i, "This file is no longer accessible.", log)
		return
	}

	info, err := os.Stat(resolved)
	if err != nil {
		log.Error("Failed to stat file for send callback", "path", resolved, "error", err)
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

	file, err := os.Open(resolved)
	if err != nil {
		log.Error("Failed to open file for send callback", "path", resolved, "error", err)
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
		log.Error("Failed to send file from callback", "path", resolved, "error", err)
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
