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

package tunnelclient

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// RecordSchema is the version of the status record format.
const RecordSchema = 1

// Record is what one tunnel process publishes for scion tunnel status. It
// never holds a token, a token hash, a cookie or a grant.
type Record struct {
	Schema     int       `json:"schema"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	Endpoint   string    `json:"endpoint"`
	Credential Identity  `json:"credential"`
	// SessionIDs are the current session and every session still
	// carrying an open tunnel, sorted, without duplicates.
	SessionIDs []string       `json:"session_ids"`
	Tunnels    []TunnelRecord `json:"tunnels"`
}

// TunnelRecord is one open tunnel.
type TunnelRecord struct {
	Agent         string     `json:"agent"`
	Project       string     `json:"project"`
	Kind          string     `json:"kind"`
	Port          int        `json:"port"`
	OpenedAt      time.Time  `json:"opened_at"`
	AuthzDeadline *time.Time `json:"authz_deadline"`
}

// StateStore holds the status records of live tunnel processes.
type StateStore interface {
	// Open creates this process's record and holds it until Close.
	Open() (RecordWriter, error)
	// List returns the records of live processes, removing those of
	// processes that have ended.
	List() ([]Record, error)
}

// RecordWriter is this process's record.
type RecordWriter interface {
	// Write replaces the record atomically.
	Write(Record) error
	// Close removes the record and releases it.
	Close() error
}

// StateDir returns the directory of tunnel status records:
// $XDG_RUNTIME_DIR/scion/tunnels when xdgRuntimeDir is set, otherwise
// <scionDir>/run/tunnels (scionDir is the user's ~/.scion).
func StateDir(xdgRuntimeDir, scionDir string) string {
	if xdgRuntimeDir != "" {
		return filepath.Join(xdgRuntimeDir, "scion", "tunnels")
	}
	return filepath.Join(scionDir, "run", "tunnels")
}

// FileStore keeps one record per tunnel process in Dir (mode 0700, files
// 0600). Each process holds an exclusive flock on its own lock file for
// its whole life; a record counts as live only while that lock is held.
// The record itself (<id>.json) is rewritten through a temporary file and
// a rename, so a reader never sees a partial record.
type FileStore struct {
	Dir string
}

const (
	recordSuffix = ".json"
	lockSuffix   = ".lock"
)

// checkPrivate refuses a path that is group- or world-accessible or owned
// by another user.
func checkPrivate(path string, fi fs.FileInfo) error {
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("refusing tunnel state %s: it is group- or world-accessible (mode %04o)", path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("refusing tunnel state %s: it is owned by another user", path)
	}
	return nil
}

// checkDir verifies the store directory.
func (f FileStore) checkDir() error {
	fi, err := os.Lstat(f.Dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing tunnel state %s: not a directory", f.Dir)
	}
	return checkPrivate(f.Dir, fi)
}

// Open implements StateStore.
func (f FileStore) Open() (RecordWriter, error) {
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := f.checkDir(); err != nil {
		return nil, err
	}
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(rnd[:]))
	lockPath := filepath.Join(f.Dir, id+lockSuffix)
	// The lock is created and locked under a dot-prefixed name that List
	// ignores, then renamed into place, so List never sees an unlocked
	// lock file of a live process.
	tmpLock := filepath.Join(f.Dir, "."+id+lockSuffix+".tmp")
	lf, err := os.OpenFile(tmpLock, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lf.Close()
		_ = os.Remove(tmpLock)
		return nil, err
	}
	if err := os.Rename(tmpLock, lockPath); err != nil {
		_ = lf.Close()
		_ = os.Remove(tmpLock)
		return nil, err
	}
	return &fileRecord{dir: f.Dir, path: filepath.Join(f.Dir, id+recordSuffix), lockPath: lockPath, lock: lf}, nil
}

// fileRecord is one process's record in a FileStore.
type fileRecord struct {
	dir, path, lockPath string
	lock                *os.File
}

// Write implements RecordWriter: temporary file, fsync, rename.
func (r *fileRecord) Write(rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, r.path); err != nil {
		return err
	}
	ok = true
	return nil
}

// Close implements RecordWriter: delete the record, then release the lock.
func (r *fileRecord) Close() error {
	err := os.Remove(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	_ = os.Remove(r.lockPath)
	_ = syscall.Flock(int(r.lock.Fd()), syscall.LOCK_UN)
	_ = r.lock.Close()
	return err
}

// List implements StateStore. A missing directory has no records. A
// directory or record that is group- or world-accessible or owned by
// another user is refused.
func (f FileStore) List() ([]Record, error) {
	if err := f.checkDir(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			// Temporary files of writers: never read or removed here.
			continue
		}
		switch {
		case strings.HasSuffix(name, recordSuffix):
			ids[strings.TrimSuffix(name, recordSuffix)] = true
		case strings.HasSuffix(name, lockSuffix):
			ids[strings.TrimSuffix(name, lockSuffix)] = true
		}
	}
	var out []Record
	for id := range ids {
		rec, live, err := f.read(id)
		if err != nil {
			return nil, err
		}
		if live && rec != nil {
			out = append(out, *rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].PID < out[j].PID
	})
	return out, nil
}

// read returns the record id and whether its process is live. A record
// whose lock can be taken belongs to an ended process: it is removed
// while the lock is held, then the lock is released.
func (f FileStore) read(id string) (*Record, bool, error) {
	recPath := filepath.Join(f.Dir, id+recordSuffix)
	lockPath := filepath.Join(f.Dir, id+lockSuffix)
	for _, p := range []string{recPath, lockPath} {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if !fi.Mode().IsRegular() {
			return nil, false, fmt.Errorf("refusing tunnel state %s: not a regular file", p)
		}
		if err := checkPrivate(p, fi); err != nil {
			return nil, false, err
		}
	}
	lf, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		// No lock: the process ended between removing its lock and its
		// record, or never finished starting. Nothing can hold it.
		_ = os.Remove(recPath)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = lf.Close() }()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		// Not held: the process has ended. Prune under the lock.
		_ = os.Remove(recPath)
		_ = os.Remove(lockPath)
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		return nil, false, nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, false, err
	}
	data, err := os.ReadFile(recPath)
	if errors.Is(err, fs.ErrNotExist) {
		// Live, but the first record is not written yet.
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil || rec.Schema != RecordSchema {
		// Unreadable or a different schema: skip it.
		return nil, true, nil
	}
	return &rec, true, nil
}

// StatusEntry is one (endpoint, credential identity) with its sessions and
// open tunnels.
type StatusEntry struct {
	Endpoint   string         `json:"endpoint"`
	Credential Identity       `json:"credential"`
	SessionIDs []string       `json:"session_ids"`
	Tunnels    []TunnelRecord `json:"tunnels"`
}

// Status is the output of scion tunnel status.
type Status struct {
	Entries []StatusEntry `json:"entries"`
}

// MergeStatus merges live records into one entry per (normalised endpoint,
// credential identity). When endpoint is not empty, only records for that
// hub are kept.
func MergeStatus(records []Record, endpoint string) Status {
	want := ""
	if endpoint != "" {
		want = NormalizeEndpoint(endpoint)
	}
	type key = SessionKey
	byKey := map[key]*StatusEntry{}
	var order []key
	for _, r := range records {
		k := KeyFor(r.Endpoint, r.Credential)
		if want != "" && k.Endpoint != want {
			continue
		}
		e, ok := byKey[k]
		if !ok {
			e = &StatusEntry{Endpoint: k.Endpoint, Credential: r.Credential, SessionIDs: []string{}, Tunnels: []TunnelRecord{}}
			byKey[k] = e
			order = append(order, k)
		}
		for _, id := range r.SessionIDs {
			if id != "" && !contains(e.SessionIDs, id) {
				e.SessionIDs = append(e.SessionIDs, id)
			}
		}
		e.Tunnels = append(e.Tunnels, r.Tunnels...)
	}
	st := Status{Entries: []StatusEntry{}}
	for _, k := range order {
		e := byKey[k]
		sort.Strings(e.SessionIDs)
		sortTunnels(e.Tunnels)
		st.Entries = append(st.Entries, *e)
	}
	sort.SliceStable(st.Entries, func(i, j int) bool {
		a, b := st.Entries[i], st.Entries[j]
		if a.Endpoint != b.Endpoint {
			return a.Endpoint < b.Endpoint
		}
		if a.Credential.Kind != b.Credential.Kind {
			return a.Credential.Kind < b.Credential.Kind
		}
		return a.Credential.Subject < b.Credential.Subject
	})
	return st
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// sortTunnels orders tunnels by open time, then agent.
func sortTunnels(ts []TunnelRecord) {
	sort.SliceStable(ts, func(i, j int) bool {
		if !ts[i].OpenedAt.Equal(ts[j].OpenedAt) {
			return ts[i].OpenedAt.Before(ts[j].OpenedAt)
		}
		return ts[i].Agent < ts[j].Agent
	})
}
