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

package fswatcher

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Watcher is the core event loop: sets up fanotify, resolves PIDs, debounces,
// and writes NDJSON events.
type Watcher struct {
	cfg      Config
	roots    []string
	filter   *Filter
	resolver *Resolver
	logger   *Logger

	mu        sync.Mutex
	debounced map[debounceKey]*debounceEntry
	renames   map[renameKey]*renameEntry

	fanotifyFd int
}

type debounceKey struct {
	agentID string
	path    string
}

type debounceEntry struct {
	timer  *time.Timer
	action Action
}

type renameKey struct {
	agentID string
	dir     string
}

type renameEntry struct {
	fromPath string
	timer    *time.Timer
}

// NewWatcher creates a Watcher from the given configuration.
func NewWatcher(cfg Config, roots []string, filter *Filter, resolver *Resolver, logger *Logger) *Watcher {
	return &Watcher{
		cfg:       cfg,
		roots:     roots,
		filter:    filter,
		resolver:  resolver,
		logger:    logger,
		debounced: make(map[debounceKey]*debounceEntry),
		renames:   make(map[renameKey]*renameEntry),
	}
}

// AddRoot adds a new watch directory at runtime (used for dynamic project discovery).
func (w *Watcher) AddRoot(dir string) (bool, error) {
	w.mu.Lock()
	for _, r := range w.roots {
		if r == dir {
			w.mu.Unlock()
			return false, nil
		}
	}
	w.roots = append(w.roots, dir)
	w.mu.Unlock()

	if w.fanotifyFd > 0 {
		return true, w.markDirectory(dir)
	}
	return true, nil
}

// RemoveRoot removes a watch directory at runtime.
func (w *Watcher) RemoveRoot(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, r := range w.roots {
		if r == dir {
			w.roots = append(w.roots[:i], w.roots[i+1:]...)
			break
		}
	}
}

// Run starts the fanotify event loop. It blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	// FAN_CLASS_NOTIF | FAN_REPORT_DFID_NAME gives us notification-mode events
	// with directory file-handle + filename (kernel 5.9+).
	flags := uint(unix.FAN_CLASS_NOTIF | unix.FAN_REPORT_DFID_NAME | unix.FAN_CLOEXEC)
	fd, err := unix.FanotifyInit(flags, unix.O_RDONLY|unix.O_LARGEFILE|unix.O_CLOEXEC)
	if err != nil {
		return fmt.Errorf("fanotify_init: %w (are you running as root/CAP_SYS_ADMIN?)", err)
	}
	defer unix.Close(fd)
	w.fanotifyFd = fd

	for _, dir := range w.roots {
		if err := w.markDirectory(dir); err != nil {
			return fmt.Errorf("marking %s: %w", dir, err)
		}
	}

	if w.cfg.Debug {
		log.Printf("[watcher] fanotify fd=%d, flags=FAN_CLASS_NOTIF|FAN_REPORT_DFID_NAME|FAN_CLOEXEC", fd)
		log.Printf("[watcher] mark flags=FAN_MARK_ADD|FAN_MARK_FILESYSTEM, mask=ACCESS|CREATE|DELETE|CLOSE_WRITE|MOVED_FROM|MOVED_TO")
		log.Printf("[watcher] watching %d directories, debounce=%s", len(w.roots), w.cfg.Debounce)
		log.Printf("[watcher] entering event loop (poll timeout=500ms)")
	}

	return w.eventLoop(ctx)
}

func (w *Watcher) markDirectory(dir string) error {
	// FAN_MARK_ADD | FAN_MARK_FILESYSTEM marks the entire filesystem containing dir.
	// This captures events from all containers writing to bind-mounted paths on this FS.
	markFlags := uint(unix.FAN_MARK_ADD | unix.FAN_MARK_FILESYSTEM)
	mask := uint64(unix.FAN_ACCESS | unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_CLOSE_WRITE |
		unix.FAN_MOVED_FROM | unix.FAN_MOVED_TO)
	if w.cfg.Debug {
		log.Printf("[watcher] marking filesystem for dir: %s", dir)
	}
	return unix.FanotifyMark(w.fanotifyFd, markFlags, mask, -1, dir)
}

func (w *Watcher) eventLoop(ctx context.Context) error {
	buf := make([]byte, 4096*int(unsafe.Sizeof(unix.FanotifyEventMetadata{})))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Poll with a 500ms timeout so we can check ctx cancellation.
		fds := []unix.PollFd{{Fd: int32(w.fanotifyFd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 500)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("poll: %w", err)
		}
		if n == 0 {
			continue
		}

		bytesRead, err := unix.Read(w.fanotifyFd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("[watcher] read error: %v", err)
			continue
		}

		w.processEvents(buf[:bytesRead])
	}
}

var sizeOfMeta = int(unsafe.Sizeof(unix.FanotifyEventMetadata{}))

func (w *Watcher) processEvents(buf []byte) {
	offset := 0
	for offset+sizeOfMeta <= len(buf) {
		meta := (*unix.FanotifyEventMetadata)(unsafe.Pointer(&buf[offset]))
		if meta.Event_len < uint32(sizeOfMeta) || offset+int(meta.Event_len) > len(buf) {
			break
		}

		pid := int(meta.Pid)
		mask := meta.Mask

		// Extract path info from FID records after the metadata.
		path, fileName := w.extractPathFromFID(buf[offset:offset+int(meta.Event_len)], meta)

		// Close the fd if one was provided (shouldn't happen in FID mode, but be safe).
		if meta.Fd >= 0 {
			unix.Close(int(meta.Fd))
		}

		fullPath := path
		if fileName != "" {
			fullPath = filepath.Join(path, fileName)
		}

		if fullPath != "" {
			w.handleRawEvent(pid, mask, fullPath)
		}

		offset += int(meta.Event_len)
	}
}

func (w *Watcher) extractPathFromFID(eventBuf []byte, meta *unix.FanotifyEventMetadata) (dir string, fileName string) {
	if meta.Fd != unix.FAN_NOFD {
		// Old-style event with fd, not FID. Read path from /proc/self/fd.
		var pathBuf [unix.PathMax]byte
		fdPath := fmt.Sprintf("/proc/self/fd/%d", meta.Fd)
		n, err := unix.Readlink(fdPath, pathBuf[:])
		if err != nil {
			return "", ""
		}
		return string(pathBuf[:n]), ""
	}

	// FID-based event. Parse the info record after metadata.
	infoOffset := int(meta.Metadata_len)
	if infoOffset+4 > len(eventBuf) {
		return "", ""
	}

	// fanotify_event_info_header: uint8 info_type, uint8 pad, uint16 len
	// infoType := eventBuf[infoOffset]
	// Then: fsid (8 bytes), then file_handle struct

	sizeOfHeader := 4 // info_type(1) + pad(1) + len(2)
	sizeOfFsid := 8   // struct __kernel_fsid_t

	fhOffset := infoOffset + sizeOfHeader + sizeOfFsid
	if fhOffset+8 > len(eventBuf) {
		return "", ""
	}

	// file_handle: uint32 handle_bytes, int32 handle_type, then f_handle[]
	handleBytes := binary.LittleEndian.Uint32(eventBuf[fhOffset : fhOffset+4])
	handleType := int32(binary.LittleEndian.Uint32(eventBuf[fhOffset+4 : fhOffset+8]))

	fhDataStart := fhOffset + 8
	fhDataEnd := fhDataStart + int(handleBytes)
	if fhDataEnd > len(eventBuf) {
		return "", ""
	}

	fh := unix.NewFileHandle(handleType, eventBuf[fhDataStart:fhDataEnd])

	// Try to extract filename after the file handle (for DFID_NAME info type).
	nameStart := fhDataEnd
	if nameStart < len(eventBuf) {
		nameEnd := bytes.IndexByte(eventBuf[nameStart:], 0)
		if nameEnd > 0 {
			fileName = string(eventBuf[nameStart : nameStart+nameEnd])
		}
	}

	// Open the directory by file handle to get its path.
	// We need a mount fd — try each root.
	w.mu.Lock()
	roots := make([]string, len(w.roots))
	copy(roots, w.roots)
	w.mu.Unlock()

	for _, root := range roots {
		mountFd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			continue
		}
		fd, err := unix.OpenByHandleAt(mountFd, fh, unix.O_RDONLY|unix.O_PATH)
		unix.Close(mountFd)
		if err != nil {
			continue
		}
		var pathBuf [unix.PathMax]byte
		fdPath := fmt.Sprintf("/proc/self/fd/%d", fd)
		n, err := unix.Readlink(fdPath, pathBuf[:])
		unix.Close(fd)
		if err == nil {
			return string(pathBuf[:n]), fileName
		}
	}

	return "", fileName
}

func (w *Watcher) handleRawEvent(pid int, mask uint64, absPath string) {
	// Check if this path is under any of our watched roots.
	relPath, underRoot := w.relativize(absPath)
	if !underRoot {
		return
	}

	if w.filter.ShouldIgnore(relPath) {
		if w.cfg.Debug {
			log.Printf("[watcher] filtered out: %s (pid=%d)", relPath, pid)
		}
		return
	}

	agentID := w.resolver.Resolve(pid)

	switch {
	case mask&unix.FAN_CREATE != 0:
		w.submitDebounced(agentID, absPath, relPath, ActionCreate)
	case mask&unix.FAN_CLOSE_WRITE != 0:
		w.submitDebounced(agentID, absPath, relPath, ActionModify)
	case mask&unix.FAN_DELETE != 0:
		w.submitDebounced(agentID, absPath, relPath, ActionDelete)
	case mask&unix.FAN_MOVED_FROM != 0:
		w.handleRenameFrom(agentID, absPath, relPath)
	case mask&unix.FAN_MOVED_TO != 0:
		w.handleRenameTo(agentID, absPath, relPath)
	case mask&unix.FAN_ACCESS != 0:
		w.submitDebounced(agentID, absPath, relPath, ActionRead)
	}
}

func (w *Watcher) handleRenameFrom(agentID, absPath, relPath string) {
	key := renameKey{agentID: agentID, dir: filepath.Dir(absPath)}

	w.mu.Lock()
	defer w.mu.Unlock()

	entry := &renameEntry{fromPath: relPath}
	entry.timer = time.AfterFunc(w.cfg.Debounce, func() {
		w.mu.Lock()
		if w.renames[key] == entry {
			delete(w.renames, key)
		}
		w.mu.Unlock()
		w.emitEvent(agentID, relPath, ActionRenameFrom)
	})
	// Cancel any previous pending rename for same key.
	if prev, ok := w.renames[key]; ok {
		prev.timer.Stop()
	}
	w.renames[key] = entry
}

func (w *Watcher) handleRenameTo(agentID, absPath, relPath string) {
	key := renameKey{agentID: agentID, dir: filepath.Dir(absPath)}

	w.mu.Lock()
	pending, hasPending := w.renames[key]
	if hasPending {
		pending.timer.Stop()
		delete(w.renames, key)
	}
	w.mu.Unlock()

	if hasPending && isTempFile(pending.fromPath) {
		// Editor save pattern: write-to-temp + rename → coalesce to modify.
		if w.cfg.Debug {
			log.Printf("[watcher] rename coalesced: %s → %s (agent=%q) → modify", pending.fromPath, relPath, agentID)
		}
		w.submitDebounced(agentID, absPath, relPath, ActionModify)
	} else {
		if hasPending {
			w.emitEvent(agentID, pending.fromPath, ActionRenameFrom)
		}
		w.emitEvent(agentID, relPath, ActionRenameTo)
	}
}

func (w *Watcher) submitDebounced(agentID, absPath, relPath string, action Action) {
	key := debounceKey{agentID: agentID, path: relPath}

	w.mu.Lock()
	defer w.mu.Unlock()

	if existing, ok := w.debounced[key]; ok {
		existing.timer.Stop()
		existing.action = action
		existing.timer = time.AfterFunc(w.cfg.Debounce, func() {
			w.mu.Lock()
			delete(w.debounced, key)
			w.mu.Unlock()
			w.emitEventWithSize(agentID, relPath, action, absPath)
		})
		return
	}

	entry := &debounceEntry{action: action}
	entry.timer = time.AfterFunc(w.cfg.Debounce, func() {
		w.mu.Lock()
		delete(w.debounced, key)
		w.mu.Unlock()
		w.emitEventWithSize(agentID, relPath, action, absPath)
	})
	w.debounced[key] = entry
}

func (w *Watcher) emitEvent(agentID, relPath string, action Action) {
	w.emitEventWithSize(agentID, relPath, action, "")
}

func (w *Watcher) emitEventWithSize(agentID, relPath string, action Action, absPath string) {
	ev := Event{
		Timestamp: time.Now().UTC(),
		AgentID:   agentID,
		Action:    action,
		Path:      relPath,
	}

	if action != ActionDelete && action != ActionRenameFrom && absPath != "" {
		if info, err := os.Stat(absPath); err == nil {
			size := info.Size()
			ev.Size = &size
		}
	}

	if err := w.logger.Write(ev); err != nil {
		log.Printf("[watcher] log write error: %v", err)
	}
}

func (w *Watcher) relativize(absPath string) (string, bool) {
	w.mu.Lock()
	roots := make([]string, len(w.roots))
	copy(roots, w.roots)
	w.mu.Unlock()

	for _, root := range roots {
		if rel, err := filepath.Rel(root, absPath); err == nil && !strings.HasPrefix(rel, "..") {
			return rel, true
		}
	}
	return absPath, false
}

// isTempFile returns true if the filename looks like an editor temp file.
func isTempFile(path string) bool {
	base := filepath.Base(path)
	if strings.HasPrefix(base, ".") {
		return true
	}
	if strings.HasSuffix(base, "~") {
		return true
	}
	if strings.HasSuffix(base, ".swp") || strings.HasSuffix(base, ".swo") {
		return true
	}
	if strings.HasSuffix(base, ".tmp") {
		return true
	}
	return false
}
