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

package transportauth

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

const (
	// EnvTransportTokenFile names the file holding the current hub-provided
	// transport token inside an agent container. sciontool init sets it for
	// its children after seeding the file; the long-lived hub client in
	// sciontool init rewrites the file on every refresh.
	EnvTransportTokenFile = "SCION_TRANSPORT_TOKEN_FILE"

	// TransportTokenFileName is the default file name, a sibling of the
	// agent credential file under $HOME/.scion.
	TransportTokenFileName = "transport-token"

	// transportTokenFileMaxBytes bounds a read of the transport token file.
	// A Google ID token is a compact JWT, well under this size.
	transportTokenFileMaxBytes = 16384
)

// Source labels reported by FileSource.Status.
const (
	SourceLabelFile      = "file"
	SourceLabelEnv       = "env"
	SourceLabelRefreshed = "refreshed"
)

// FileReadFunc reads the transport token file at path. Implementations must
// refuse anything that is not a regular file.
type FileReadFunc func(path string) (string, error)

// DefaultTransportTokenFilePath returns $HOME/.scion/transport-token, or
// "" when HOME is unset. It takes HOME from the environment, the same
// source ReadTransportTokenFile uses to decide what lies under HOME.
func DefaultTransportTokenFilePath() string {
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".scion", TransportTokenFileName)
}

// ReadTransportTokenFile is the default FileReadFunc. For a path under the
// current user's home directory, the home directory is trusted and opened
// normally (it may be reached through a linked prefix such as
// /home -> /var/home) and every component below it is opened without
// following a symbolic link. For any other path, its parent directory is
// trusted and the file itself is opened without following a link. Either
// way the file must be a single-link regular file no larger than the size
// limit; anything else is refused with an error. Callers running with
// elevated privileges should supply a stricter reader (sciontool uses its
// fd-relative, no-follow reader from the root directory).
func ReadTransportTokenFile(path string) (string, error) {
	base, rel, err := splitUnderHome(os.Getenv("HOME"), path)
	if err != nil {
		return "", err
	}
	// O_NOFOLLOW (below base) refuses a symlink atomically with each open;
	// O_NONBLOCK keeps a FIFO planted at the path from blocking the open.
	// The regular-file and link-count checks then run on the opened
	// descriptor, so there is no window between the check and the read.
	//
	// Trusts HOME as set by the launching environment (the user on a host,
	// the launcher in an agent). The directories above HOME must not be
	// writable by this user or any less-trusted user, so HOME itself cannot be
	// replaced; HOME may be writable. Entries below HOME are opened without
	// following links. For a path outside HOME, its parent directory is
	// trusted the same way.
	dirFd, leaf, err := dirfd.OpenParentNoFollowUnder(base, rel)
	if err != nil {
		return "", fmt.Errorf("transport token file %s: %w", path, err)
	}
	defer func() { _ = syscall.Close(dirFd) }()
	data, err := dirfd.ReadAtNoFollow(dirFd, leaf, transportTokenFileMaxBytes)
	if err != nil {
		return "", fmt.Errorf("transport token file %s: %w", path, err)
	}
	return string(data), nil
}

// splitUnderHome splits path into a trusted base directory and the
// relative path below it that ReadTransportTokenFile walks without
// following links. path is made absolute and cleaned lexically, so a ".."
// in it is resolved by name, not through any link it follows (stricter
// than the kernel's resolution). When the cleaned path lies strictly
// below home, on a path-separator boundary, base is home; otherwise base
// is path's own parent directory and rel is its final component.
func splitUnderHome(home, path string) (base, rel string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("transport token file %s: %w", path, err)
	}
	abs = filepath.Clean(abs)
	if home != "" {
		if h, herr := filepath.Abs(home); herr == nil {
			h = filepath.Clean(h)
			prefix := h
			if !strings.HasSuffix(prefix, string(filepath.Separator)) {
				prefix += string(filepath.Separator)
			}
			if strings.HasPrefix(abs, prefix) && len(abs) > len(prefix) {
				return h, abs[len(prefix):], nil
			}
		}
	}
	return filepath.Dir(abs), filepath.Base(abs), nil
}

// FileSource is a TokenSource backed by the transport token file that the
// long-lived hub client inside the agent keeps current. The injected
// environment value only bootstraps: whichever candidate (file, a value
// pushed in via SetToken, or the bootstrap value) expires last is used.
//
// The file is re-read whenever it is replaced (each write is a rename, so a
// new inode) or its modification time or size changes, so short-lived
// processes and long-lived ones alike pick up refreshed values without
// restarting.
//
// A file value whose expiry cannot be parsed is given a zero expiry, so it
// loses to any candidate whose expiry is known.
//
// While the file is being replaced, a read can open a file that has no link
// left by the time it is checked. When every read in one call fails that
// way, the value from the last successful read is kept for that call if it
// was read at most keepMax ago and has a parsed expiry that has not passed.
// Status reports this as FileRetained. Any other read failure clears it.
type FileSource struct {
	// WarnLog, if non-nil, is called when the token in use is near expiry,
	// and once each time the source starts keeping an earlier value while
	// the file is being replaced.
	WarnLog LogFunc

	path string
	read FileReadFunc

	mu sync.Mutex

	bootstrap       string
	bootstrapExpiry time.Time

	refreshed       string
	refreshedExpiry time.Time

	fileToken   string
	fileExpiry  time.Time
	fileModTime time.Time
	fileSize    int64
	fileInfo    os.FileInfo // identity of the last file read (inode on Unix)
	fileCtime   time.Time   // change time of the last file read
	fileNlink   uint64      // link count of the last file read
	fileReadAt  time.Time   // when the last read started
	fileErr     error

	// lastGoodAt is when the read that produced fileToken started.
	lastGoodAt time.Time
	// fileExpiryParsed is true when fileExpiry was parsed from fileToken.
	// fileExpiry and fileExpiryParsed are written together: fileExpiry is
	// set to a non-zero value only with fileExpiryParsed true, and every
	// other write sets them to zero and false. So while fileExpiryParsed is
	// false, fileExpiry is zero.
	fileExpiryParsed bool
	// fileRetained is true while fileToken is kept from an earlier read
	// because the file was being replaced.
	fileRetained bool
	// retainWarned is true once WarnLog has reported the current retained
	// episode.
	retainWarned bool

	// now returns the current time; tests replace it.
	now func() time.Time
}

// NewFileSource creates a FileSource for path. read may be nil, in which
// case ReadTransportTokenFile is used.
func NewFileSource(path string, read FileReadFunc) *FileSource {
	if read == nil {
		read = ReadTransportTokenFile
	}
	return &FileSource{path: path, read: read, now: time.Now}
}

// Path returns the file this source reads.
func (s *FileSource) Path() string { return s.path }

// SetBootstrap records the injected (environment) value. It is used only
// while no fresher value is available from the file or SetToken.
func (s *FileSource) SetBootstrap(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bootstrap = strings.TrimSpace(token)
	s.bootstrapExpiry = time.Time{}
	if s.bootstrap != "" {
		if exp, err := ParseTokenExpiry(s.bootstrap); err == nil {
			s.bootstrapExpiry = exp
		} else {
			s.bootstrapExpiry = time.Now().Add(DefaultTTL)
		}
	}
}

// SetToken records a refreshed value in memory. The caller is responsible
// for persisting it to the file; until the file changes, the in-memory
// value competes with the file on expiry.
func (s *FileSource) SetToken(token string, expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshed = token
	s.refreshedExpiry = expiry
}

// Token returns the freshest available transport token.
func (s *FileSource) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	tok, exp, _ := s.pickLocked()
	if tok == "" {
		return "", fmt.Errorf("oidc: no transport token available")
	}
	if !exp.IsZero() && time.Now().After(exp.Add(-RefreshMargin)) && s.WarnLog != nil {
		s.WarnLog("OIDC transport token is near expiry or expired, returning anyway")
	}
	return tok, nil
}

// Expiry returns the expiry of the token Token would return.
func (s *FileSource) Expiry() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	_, exp, _ := s.pickLocked()
	return exp
}

// FileSourceStatus describes a FileSource for diagnostics. It never
// carries token values.
type FileSourceStatus struct {
	Path string
	// InUse is the label of the candidate Token returns: "file", "env",
	// "refreshed" (in-memory, not yet visible in the file) or "" if none.
	InUse  string
	Expiry time.Time

	EnvPresent bool
	EnvExpiry  time.Time

	FilePresent bool
	FileExpiry  time.Time
	// FileModTime is when the file was last written (the last refresh as
	// seen by other processes).
	FileModTime time.Time
	// FileError is set when the file exists but could not be read.
	FileError error
	// FileRetained is true when the file was being replaced during every
	// read of the last attempt to load it, and the value from the read
	// before that is still in use (FileError then holds the read error).
	FileRetained bool
}

// Status reports which candidate is in use and the expiry of each, without
// exposing token values.
func (s *FileSource) Status() FileSourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	_, exp, label := s.pickLocked()
	st := FileSourceStatus{
		Path:        s.path,
		InUse:       label,
		Expiry:      exp,
		EnvPresent:  s.bootstrap != "",
		EnvExpiry:   s.bootstrapExpiry,
		FilePresent: s.fileToken != "",
		FileExpiry:  s.fileExpiry,
		FileModTime: s.fileModTime,

		FileRetained: s.fileRetained,
	}
	if s.fileErr != nil && !errors.Is(s.fileErr, fs.ErrNotExist) {
		st.FileError = s.fileErr
	}
	return st
}

// reloadAttempts bounds how many times reloadLocked reads the file in one
// call when the file keeps being replaced during the read.
const reloadAttempts = 3

// racyModTimeWindow is how close to the start of the last read a file's
// modification time may be before that read is not reused. A write that
// lands within the same timestamp tick as an earlier one can leave the
// metadata unchanged (the kernel stamps files from a coarse clock, and some
// filesystems store whole seconds), so a file modified that close to the
// read is read again until its modification time is safely in the past.
const racyModTimeWindow = 2 * time.Second

// racyRecheck is how often a file whose last read falls in the racy window
// is read again. Between those reads the earlier read is reused, so a write
// that left all of the metadata unchanged is picked up within racyRecheck.
const racyRecheck = 50 * time.Millisecond

// keepMax bounds how long after the last successful read its value can be
// kept while the file is being replaced. A refresh replaces the file in
// milliseconds, so this covers a burst of replacements while limiting how
// long a replaced value stays in use.
const keepMax = 2 * time.Second

// reloadLocked re-reads the file unless the last read succeeded and the
// file's metadata (identity, modification and change time, size and link
// count) is unchanged since then, and the read is either safely after the
// file's modification time or less than racyRecheck old.
//
// Each write replaces the file by rename. A read that opens the file just
// before it is replaced sees the old file with no link left, and the reader
// refuses it. When a read fails because the opened file had no link left
// (dirfd.ErrUnlinked), or the path now names a different file, the path is
// read again through the same reader, up to reloadAttempts reads in total.
// The unlinked check is needed because a filesystem can give the new file
// the inode number of a file it just freed, so the identity alone can match.
//
// When all reloadAttempts reads fail with dirfd.ErrUnlinked, the value from
// the last successful read is kept if canRetainLocked allows it; the error
// is recorded and the next call reads again. Any other failure, or the last
// allowed read failing otherwise, clears the file value and records the
// error.
func (s *FileSource) reloadLocked() {
	if s.path == "" {
		return
	}
	// allUnlinked stays true while every failed read in this call opened a
	// file that had no link left.
	allUnlinked := true
	// Iterates only via the continue below (a read that failed because the
	// file was replaced).
	for attempt := 1; attempt <= reloadAttempts; attempt++ {
		readAt := s.now()
		fi, err := os.Lstat(s.path)
		if err != nil {
			s.clearFileValueLocked(err)
			s.fileModTime, s.fileSize, s.fileInfo = time.Time{}, 0, nil
			s.fileCtime, s.fileNlink, s.fileReadAt = time.Time{}, 0, time.Time{}
			return
		}
		ctime, nlink, haveExtra := fileStatExtra(fi)
		if s.cachedReadValidLocked(fi, ctime, nlink, haveExtra, readAt) {
			return
		}
		s.fileInfo = fi
		s.fileModTime = fi.ModTime()
		s.fileSize = fi.Size()
		s.fileCtime, s.fileNlink = ctime, nlink
		s.fileReadAt = readAt

		data, err := s.read(s.path)
		if err != nil {
			// Recorded at once, so no later attempt in this call can reuse
			// the metadata of this failed read.
			s.fileErr = err
			if !errors.Is(err, dirfd.ErrUnlinked) {
				allUnlinked = false
			}
			if attempt < reloadAttempts && s.replacedDuringRead(fi, err) {
				continue
			}
			if attempt == reloadAttempts && allUnlinked && s.canRetainLocked(readAt) {
				s.retainLocked(err)
				return
			}
			s.clearFileValueLocked(err)
			return
		}
		s.fileErr = nil
		s.fileRetained, s.retainWarned = false, false
		s.lastGoodAt = readAt
		s.fileToken = strings.TrimSpace(data)
		s.fileExpiry, s.fileExpiryParsed = time.Time{}, false
		if s.fileToken == "" {
			return
		}
		// An unparseable value keeps a zero expiry: it is used only when no
		// candidate with a known expiry exists.
		if exp, err := ParseTokenExpiry(s.fileToken); err == nil {
			s.fileExpiry, s.fileExpiryParsed = exp, true
		}
		return
	}
}

// canRetainLocked reports whether the value from the last successful read
// may be kept at now: there is one, it was read at most keepMax ago, and its
// expiry was parsed and has not passed. A value whose expiry could not be
// parsed is never kept.
//
// The fileExpiryParsed check is defence in depth: while an unparsed expiry
// is always zero (see fileExpiryParsed), now.Before(s.fileExpiry) is already
// false without it. The "expiry not parsed" case of
// TestFileSource_CanRetainLocked is the one test that covers the check on
// its own, so keep it.
func (s *FileSource) canRetainLocked(now time.Time) bool {
	return s.fileToken != "" && !s.lastGoodAt.IsZero() &&
		now.Sub(s.lastGoodAt) <= keepMax &&
		s.fileExpiryParsed && now.Before(s.fileExpiry)
}

// retainLocked keeps the value from the last successful read after a call
// in which every read failed with err (dirfd.ErrUnlinked). The metadata is
// cleared and err recorded, so the next call reads the file again.
func (s *FileSource) retainLocked(err error) {
	s.fileErr = err
	s.fileRetained = true
	s.fileInfo, s.fileModTime, s.fileSize = nil, time.Time{}, 0
	s.fileCtime, s.fileNlink, s.fileReadAt = time.Time{}, 0, time.Time{}
	if !s.retainWarned && s.WarnLog != nil {
		s.WarnLog("transport token file %s is being replaced; using the value read before", s.path)
	}
	s.retainWarned = true
}

// clearFileValueLocked drops the file value and records err.
func (s *FileSource) clearFileValueLocked(err error) {
	s.fileToken, s.fileExpiry, s.fileExpiryParsed = "", time.Time{}, false
	s.lastGoodAt = time.Time{}
	s.fileRetained, s.retainWarned = false, false
	s.fileErr = err
}

// cachedReadValidLocked reports whether the value from the last read can be
// reused at now for a file whose current Lstat is fi. Only a successful read
// is reused: a failure can be caused by properties such as the link count
// that the reader checks on the open file.
func (s *FileSource) cachedReadValidLocked(fi os.FileInfo, ctime time.Time, nlink uint64, haveExtra bool, now time.Time) bool {
	if s.fileErr != nil || s.fileInfo == nil || !haveExtra {
		return false
	}
	if !os.SameFile(s.fileInfo, fi) || !fi.ModTime().Equal(s.fileModTime) ||
		fi.Size() != s.fileSize || !ctime.Equal(s.fileCtime) || nlink != s.fileNlink {
		return false
	}
	if s.fileModTime.Before(s.fileReadAt.Add(-racyModTimeWindow)) {
		return true
	}
	// A file modified within racyModTimeWindow of the last read could have
	// been changed again after that read without changing its metadata, so
	// it is read again once the last read is racyRecheck old.
	return now.Sub(s.fileReadAt) < racyRecheck
}

// replacedDuringRead reports whether a read that failed with err should be
// retried because the file was replaced while it was being read: the reader
// found the opened file had no link left, or the path, checked again with
// Lstat (which does not follow a link at the final component), now names a
// different file than fi.
func (s *FileSource) replacedDuringRead(fi os.FileInfo, err error) bool {
	if errors.Is(err, dirfd.ErrUnlinked) {
		return true
	}
	now, lerr := os.Lstat(s.path)
	return lerr == nil && !os.SameFile(fi, now)
}

// pickLocked returns the candidate with the latest expiry. On a tie the
// file wins over the in-memory value, which wins over the bootstrap value.
func (s *FileSource) pickLocked() (string, time.Time, string) {
	type candidate struct {
		tok   string
		exp   time.Time
		label string
	}
	var best candidate
	for _, c := range []candidate{
		{s.fileToken, s.fileExpiry, SourceLabelFile},
		{s.refreshed, s.refreshedExpiry, SourceLabelRefreshed},
		{s.bootstrap, s.bootstrapExpiry, SourceLabelEnv},
	} {
		if c.tok == "" {
			continue
		}
		if best.tok == "" || c.exp.After(best.exp) {
			best = c
		}
	}
	return best.tok, best.exp, best.label
}

// fileSourceFromEnv builds a FileSource when the process is inside an
// agent that received an injected transport token: either
// SCION_TRANSPORT_TOKEN_FILE is set (by sciontool init for its children),
// or SCION_TRANSPORT_TOKEN is set (a process that still sees the bootstrap
// value, such as a shell exec'd into the container), in which case the
// default $HOME/.scion/transport-token is consulted. Returns nil outside
// agents, so hosts and brokers are unaffected.
func fileSourceFromEnv(read FileReadFunc) *FileSource {
	path := os.Getenv(EnvTransportTokenFile)
	envTok := os.Getenv(EnvTransportToken)
	if path == "" && envTok == "" {
		return nil
	}
	if path == "" {
		path = DefaultTransportTokenFilePath()
	}
	src := NewFileSource(path, read)
	src.SetBootstrap(envTok)
	return src
}

// lateFileSourceFromEnv builds a FileSource for the default transport token
// file when SCION_TRANSPORT_MODE names a proxy mode, no transport token was
// injected, and the file exists. sciontool init removes a leftover file at
// start when no transport token was injected, so the file then exists only
// because a token refresh or reset-auth delivered a transport token after
// the agent started. Processes that were already running, and so never saw
// SCION_TRANSPORT_TOKEN_FILE, pick it up this way. Returns nil otherwise,
// so hosts and agents without a proxy mode are unaffected.
func lateFileSourceFromEnv(read FileReadFunc) *FileSource {
	if !IsProxyMode(os.Getenv(EnvTransportMode)) {
		return nil
	}
	path := DefaultTransportTokenFilePath()
	if path == "" {
		return nil
	}
	if _, err := os.Lstat(path); err != nil {
		return nil
	}
	return NewFileSource(path, read)
}
