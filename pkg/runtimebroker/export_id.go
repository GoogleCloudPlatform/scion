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

package runtimebroker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/google/uuid"
)

// exportIDReadTimeout bounds how long a descriptor build waits for the
// export identity marker. A hung NFS mount must not stall the heartbeat.
const exportIDReadTimeout = 5 * time.Second

// exportIDCacheTTL bounds how long the last successfully read export ID is
// reported while a newer read is still in flight.
const exportIDCacheTTL = 2 * time.Minute

// exportIDMarkerMaxBytes bounds how much of the marker file is read.
const exportIDMarkerMaxBytes = 256

// readOrCreateExportID returns the export identity UUID stored in the marker
// file <dir>/.scion-export-id, creating the marker with a fresh UUID when it
// does not exist. dir itself is never created: a missing sub-path root (a
// fresh export with no workspaces yet, or a mount that went away and left
// the bare mountpoint) is an error, so nothing is ever written to the
// broker's local disk under the mountpoint. Creation is race-free across brokers
// sharing the export: the UUID is written to a private temporary file which
// is then hard-linked to the marker name, so exactly one broker's link
// succeeds and every other broker reads the winner's UUID. A marker that
// does not hold a UUID is an error, never overwritten.
func readOrCreateExportID(dir string) (string, error) {
	marker := filepath.Join(dir, api.ExportIDMarkerName)
	id, err := readExportIDMarker(marker)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	tmp, err := os.CreateTemp(dir, api.ExportIDMarkerName+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create export identity marker in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	_, werr := tmp.WriteString(uuid.NewString() + "\n")
	if serr := tmp.Sync(); werr == nil {
		werr = serr
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", fmt.Errorf("write export identity marker %s: %w", tmpName, werr)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", fmt.Errorf("chmod export identity marker %s: %w", tmpName, err)
	}
	// Link fails with EEXIST when another broker created the marker first;
	// either way the marker now exists and is read back.
	if err := os.Link(tmpName, marker); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("create export identity marker %s: %w", marker, err)
	}
	return readExportIDMarker(marker)
}

// readExportIDMarker reads and validates the UUID in a marker file.
func readExportIDMarker(marker string) (string, error) {
	f, err := os.Open(marker)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, exportIDMarkerMaxBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", fmt.Errorf("read export identity marker %s: %w", marker, err)
	}
	id := strings.TrimSpace(string(buf[:n]))
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", fmt.Errorf("export identity marker %s does not hold a UUID", marker)
	}
	return parsed.String(), nil
}

// exportIDMarkerDir returns the directory holding the export identity
// marker for nfs: the sub-path root on Shares[0] under the broker's mount
// root, where agent workspaces are placed (nfsBackend.Resolve).
func exportIDMarkerDir(nfs *config.V1NFSConfig) (string, error) {
	if nfs == nil || len(nfs.Shares) == 0 || nfs.MountRoot == "" {
		return "", errors.New("no NFS share configured")
	}
	subPathRoot, err := config.ResolveSubPathRoot(nfs.SubPathRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(nfs.MountRoot, nfs.Shares[0].ID, subPathRoot), nil
}

// exportIDProbe reads the export identity marker with a deadline, running
// at most one read at a time. While a read is in flight (a concurrent
// descriptor build, or a hung mount) callers get the last successfully read
// ID if it is younger than exportIDCacheTTL, else "", at once instead of
// starting another read. A failed read clears the cached ID. The zero value
// is ready.
type exportIDProbe struct {
	mu       sync.Mutex
	inflight bool
	last     string
	lastAt   time.Time
	// read is the marker reader; nil uses readOrCreateExportID. now is the
	// clock; nil uses time.Now. Tests set them.
	read func(dir string) (string, error)
	now  func() time.Time
}

func (p *exportIDProbe) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// cachedLocked returns the last good ID if it is still fresh. p.mu is held.
func (p *exportIDProbe) cachedLocked() string {
	if p.last != "" && p.clock().Sub(p.lastAt) < exportIDCacheTTL {
		return p.last
	}
	return ""
}

// get returns the export ID read from dir. When the read fails it returns
// "". When an earlier read is still in flight, or this read takes longer
// than timeout, it returns the cached last good ID (see exportIDProbe).
func (p *exportIDProbe) get(dir string, timeout time.Duration) string {
	p.mu.Lock()
	if p.inflight {
		cached := p.cachedLocked()
		p.mu.Unlock()
		return cached
	}
	p.inflight = true
	read := p.read
	p.mu.Unlock()
	if read == nil {
		read = readOrCreateExportID
	}

	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, err := read(dir)
		p.mu.Lock()
		p.inflight = false
		if err != nil {
			p.last = ""
		} else {
			p.last, p.lastAt = id, p.clock()
		}
		p.mu.Unlock()
		done <- result{id, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		if r.err != nil {
			return ""
		}
		return r.id
	case <-timer.C:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.cachedLocked()
	}
}
