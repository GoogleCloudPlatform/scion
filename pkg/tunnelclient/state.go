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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/filelock"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
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
	// Listeners are the open --local-port listeners, with or without a
	// connection. A record written before listeners were recorded has
	// none.
	Listeners []ListenerRecord `json:"listeners,omitempty"`
}

// ListenerRecord is one open local listener. It holds no credential, grant
// or URL.
type ListenerRecord struct {
	Agent   string `json:"agent"`
	Project string `json:"project"`
	Kind    string `json:"kind"`
	// LocalPort is the local port the listener accepts on (127.0.0.1),
	// not the agent port of a tunnel.
	LocalPort int       `json:"local_port"`
	OpenedAt  time.Time `json:"opened_at"`
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

// FileStore keeps one record per tunnel process in Dir. Each process
// holds an exclusive lock on its own lock file for its whole life; a
// record counts as live only while that lock is held. The record itself
// (<id>.json) is rewritten through a temporary file and a rename, so a
// reader never sees a partial record.
//
// Dir and its files are private (privatefs.Private: mode 0700/0600 on
// unix, an owner-only protected DACL on Windows). Every file operation goes
// through a privatefs.Tree anchored at Dir's parent, so on Windows no
// component from the anchor down may be a reparse point, and the privacy
// of Dir, of each record and of each lock file is checked on the handle
// that is then read or locked, never on a path checked beforehand.
type FileStore struct {
	Dir string
}

const (
	recordSuffix = ".json"
	lockSuffix   = ".lock"
	// maxRecordSize bounds a record read; a larger record is refused.
	maxRecordSize = 1 << 20
)

// openTree opens the tree anchored at Dir's parent (creating the anchor
// when create is set) and returns it with Dir's name within it.
func (f FileStore) openTree(create bool) (*privatefs.Tree, string, error) {
	t, err := privatefs.OpenTree(filepath.Dir(f.Dir), privatefs.TreeOptions{CreateAnchor: create})
	if err != nil {
		return nil, "", err
	}
	return t, filepath.Base(f.Dir), nil
}

// openDir opens the store directory through the tree and verifies, on its
// handle, that it is a private directory.
func (f FileStore) openDir(t *privatefs.Tree, base string) (*os.File, error) {
	d, err := t.OpenDir(base)
	if err != nil {
		return nil, refuse(err)
	}
	if err := privatefs.CheckPrivate(d); err != nil {
		_ = d.Close()
		return nil, refuse(err)
	}
	return d, nil
}

// refuse prefixes a privacy or reparse-point refusal; other errors (such
// as fs.ErrNotExist) pass unchanged.
func refuse(err error) error {
	if errors.Is(err, privatefs.ErrNotPrivate) || errors.Is(err, privatefs.ErrReparsePoint) {
		return fmt.Errorf("refusing tunnel state: %w", err)
	}
	return err
}

// Open implements StateStore.
func (f FileStore) Open() (RecordWriter, error) {
	t, base, err := f.openTree(true)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = t.Close()
		}
	}()
	if err := t.MkdirAll(base, privatefs.Private); err != nil {
		return nil, refuse(err)
	}
	d, err := f.openDir(t, base)
	if err != nil {
		return nil, err
	}
	_ = d.Close()
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(rnd[:]))
	lockRel := base + "/" + id + lockSuffix
	// The lock is created and locked under a dot-prefixed name that List
	// ignores, then renamed into place while held, so List never sees an
	// unlocked lock file of a live process. Its handle shares delete
	// access, so the rename (and Close's removal) work while it is open.
	tmpLock := base + "/." + id + lockSuffix + ".tmp"
	lf, err := t.Create(tmpLock, true, privatefs.Private)
	if err != nil {
		return nil, refuse(err)
	}
	lock, err := filelock.LockFile(lf, 0)
	if err != nil {
		_ = t.RemoveOpen(lf)
		_ = lf.Close()
		return nil, err
	}
	// Rename and, on failure, delete through the locked handle itself, so
	// nothing swapped in under the temporary name becomes the lock file.
	if err := t.RenameOpen(lf, lockRel); err != nil {
		_ = t.RemoveOpen(lf)
		lock.Unlock()
		return nil, err
	}
	ok = true
	return &fileRecord{tree: t, base: base, rel: base + "/" + id + recordSuffix, lockRel: lockRel, lock: lock}, nil
}

// fileRecord is one process's record in a FileStore.
type fileRecord struct {
	tree               *privatefs.Tree
	base, rel, lockRel string
	lock               *filelock.Lock
}

// Write implements RecordWriter: temporary file, fsync, rename.
func (r *fileRecord) Write(rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, _, err := r.tree.CreateTemp(r.base, ".tmp-*", privatefs.Private)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			// Delete through the open handle, never by name.
			_ = r.tree.RemoveOpen(tmp)
			_ = tmp.Close()
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	// Rename through the still-open handle, so the file renamed into place
	// is the one just written, whatever happened to its temporary name.
	if err := r.tree.RenameOpen(tmp, r.rel); err != nil {
		return err
	}
	ok = true
	return tmp.Close()
}

// Close implements RecordWriter: delete the record, then release the lock.
func (r *fileRecord) Close() error {
	err := r.tree.Remove(r.rel)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	_ = r.tree.Remove(r.lockRel)
	r.lock.Unlock()
	_ = r.tree.Close()
	return err
}

// List implements StateStore. A missing directory has no records. A
// directory, record or lock file that is accessible to others or owned by
// another user, or that is a symlink (on Windows: any reparse point), is
// refused.
func (f FileStore) List() ([]Record, error) {
	t, base, err := f.openTree(false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = t.Close() }()
	d, err := f.openDir(t, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
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
		rec, live, err := read(t, base, id)
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

// openPrivate opens rel read-only through the tree, verified private on
// the opened handle. A missing file returns (nil, nil).
func openPrivate(t *privatefs.Tree, rel string) (*os.File, error) {
	fh, err := t.OpenPrivate(rel, os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, refuse(err)
	}
	return fh, nil
}

// read returns the record id and whether its process is live. A record
// whose lock can be taken belongs to an ended process: it is removed
// while the lock is held, then the lock is released.
//
// The record and the lock are each checked private on the handle that is
// then read or locked. The liveness probe locks that same verified handle
// (filelock.LockFile), rather than reopening the lock by path. The record
// is opened before the probe and read after it, so it may be one rewrite
// older than the newest (each rewrite is a complete record).
func read(t *privatefs.Tree, base, id string) (*Record, bool, error) {
	recRel := base + "/" + id + recordSuffix
	lockRel := base + "/" + id + lockSuffix
	rf, err := openPrivate(t, recRel)
	if err != nil {
		return nil, false, err
	}
	if rf != nil {
		defer func() { _ = rf.Close() }()
	}
	lf, err := openPrivate(t, lockRel)
	if err != nil {
		return nil, false, err
	}
	if lf == nil {
		// No lock: the process ended between removing its lock and its
		// record, or never finished starting. Nothing can hold it.
		if rf != nil {
			_ = t.Remove(recRel)
		}
		return nil, false, nil
	}
	lock, err := filelock.LockFile(lf, 0)
	if err == nil {
		// Not held: the process has ended. Prune under the lock.
		_ = t.Remove(recRel)
		_ = t.Remove(lockRel)
		lock.Unlock()
		return nil, false, nil
	}
	_ = lf.Close()
	if !errors.Is(err, filelock.ErrTimeout) {
		return nil, false, err
	}
	if rf == nil {
		// Live, but the first record is not written yet.
		return nil, true, nil
	}
	data, err := io.ReadAll(io.LimitReader(rf, maxRecordSize+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxRecordSize {
		return nil, false, fmt.Errorf("refusing tunnel state %s: record larger than %d bytes", recRel, maxRecordSize)
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
	Endpoint   string           `json:"endpoint"`
	Credential Identity         `json:"credential"`
	SessionIDs []string         `json:"session_ids"`
	Tunnels    []TunnelRecord   `json:"tunnels"`
	Listeners  []ListenerRecord `json:"listeners"`
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
			e = &StatusEntry{Endpoint: k.Endpoint, Credential: r.Credential, SessionIDs: []string{}, Tunnels: []TunnelRecord{}, Listeners: []ListenerRecord{}}
			byKey[k] = e
			order = append(order, k)
		}
		for _, id := range r.SessionIDs {
			if id != "" && !contains(e.SessionIDs, id) {
				e.SessionIDs = append(e.SessionIDs, id)
			}
		}
		e.Tunnels = append(e.Tunnels, r.Tunnels...)
		e.Listeners = append(e.Listeners, r.Listeners...)
	}
	st := Status{Entries: []StatusEntry{}}
	for _, k := range order {
		e := byKey[k]
		sort.Strings(e.SessionIDs)
		sortTunnels(e.Tunnels)
		sortListeners(e.Listeners)
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

// sortListeners orders listeners by open time, then local port.
func sortListeners(ls []ListenerRecord) {
	sort.SliceStable(ls, func(i, j int) bool {
		if !ls[i].OpenedAt.Equal(ls[j].OpenedAt) {
			return ls[i].OpenedAt.Before(ls[j].OpenedAt)
		}
		return ls[i].LocalPort < ls[j].LocalPort
	})
}
