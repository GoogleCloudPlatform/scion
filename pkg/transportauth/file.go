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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
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

// DefaultTransportTokenFilePath returns $HOME/.scion/transport-token for
// the current user, or "" when the home directory cannot be resolved.
func DefaultTransportTokenFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".scion", TransportTokenFileName)
}

// ReadTransportTokenFile is the default FileReadFunc. It refuses symlinks
// and anything that is not a regular file, and bounds the read size.
// Callers running with elevated privileges should supply a stricter reader
// (sciontool uses its fd-relative, no-follow reader).
func ReadTransportTokenFile(path string) (string, error) {
	// O_NOFOLLOW refuses a symlink at the leaf atomically with the open;
	// O_NONBLOCK keeps a FIFO planted at the path from blocking the open.
	// The regular-file check then runs on the opened descriptor, so there
	// is no window between the check and the read.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("transport token file %s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, transportTokenFileMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > transportTokenFileMaxBytes {
		return "", fmt.Errorf("transport token file %s is too large", path)
	}
	return string(data), nil
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
type FileSource struct {
	// WarnLog, if non-nil, is called when the token in use is near expiry.
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
	fileErr     error
}

// NewFileSource creates a FileSource for path. read may be nil, in which
// case ReadTransportTokenFile is used.
func NewFileSource(path string, read FileReadFunc) *FileSource {
	if read == nil {
		read = ReadTransportTokenFile
	}
	return &FileSource{path: path, read: read}
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
	}
	if s.fileErr != nil && !os.IsNotExist(s.fileErr) {
		st.FileError = s.fileErr
	}
	return st
}

// reloadLocked re-reads the file when it has been replaced or its
// modification time or size has changed since the last read.
func (s *FileSource) reloadLocked() {
	if s.path == "" {
		return
	}
	fi, err := os.Lstat(s.path)
	if err != nil {
		s.fileToken, s.fileExpiry = "", time.Time{}
		s.fileModTime, s.fileSize, s.fileInfo = time.Time{}, 0, nil
		s.fileErr = err
		return
	}
	if s.fileInfo != nil && os.SameFile(s.fileInfo, fi) &&
		fi.ModTime().Equal(s.fileModTime) && fi.Size() == s.fileSize {
		return
	}
	s.fileInfo = fi
	s.fileModTime = fi.ModTime()
	s.fileSize = fi.Size()

	data, err := s.read(s.path)
	if err != nil {
		s.fileToken, s.fileExpiry, s.fileErr = "", time.Time{}, err
		return
	}
	s.fileErr = nil
	s.fileToken = strings.TrimSpace(data)
	s.fileExpiry = time.Time{}
	if s.fileToken == "" {
		return
	}
	// An unparseable value keeps a zero expiry: it is used only when no
	// candidate with a known expiry exists.
	if exp, err := ParseTokenExpiry(s.fileToken); err == nil {
		s.fileExpiry = exp
	}
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
func fileSourceFromEnv() *FileSource {
	path := os.Getenv(EnvTransportTokenFile)
	envTok := os.Getenv(EnvTransportToken)
	if path == "" && envTok == "" {
		return nil
	}
	if path == "" {
		path = DefaultTransportTokenFilePath()
	}
	src := NewFileSource(path, nil)
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
func lateFileSourceFromEnv() *FileSource {
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
	return NewFileSource(path, nil)
}
