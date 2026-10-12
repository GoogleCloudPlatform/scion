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
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

func testStore(t *testing.T) FileStore {
	t.Helper()
	return FileStore{Dir: filepath.Join(t.TempDir(), "scion", "tunnels")}
}

func sampleRecord(endpoint, subject, session string, agents ...string) Record {
	opened := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	r := Record{
		Schema:     RecordSchema,
		PID:        4242,
		StartedAt:  opened,
		Endpoint:   endpoint,
		Credential: Identity{Kind: CredentialOAuth, Subject: subject, Scope: ScopeHub},
		SessionIDs: []string{session},
		Tunnels:    []TunnelRecord{},
	}
	for i, a := range agents {
		d := opened.Add(time.Hour)
		r.Tunnels = append(r.Tunnels, TunnelRecord{Agent: a, Project: "proj", Kind: "tcp", Port: 5173, OpenedAt: opened.Add(time.Duration(i) * time.Minute), AuthzDeadline: &d})
	}
	return r
}

func TestStateDir(t *testing.T) {
	if got := StateDir("/run/user/1000", "/home/u/.scion"); got != "/run/user/1000/scion/tunnels" {
		t.Fatalf("StateDir with XDG_RUNTIME_DIR = %q", got)
	}
	if got := StateDir("", "/home/u/.scion"); got != "/home/u/.scion/run/tunnels" {
		t.Fatalf("StateDir fallback = %q", got)
	}
}

func TestFileStoreOpenWriteListClose(t *testing.T) {
	st := testStore(t)
	w, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(st.Dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, %v; want 0700", fi.Mode().Perm(), err)
	}
	rec := sampleRecord("https://hub.example.com", "u1", "s1", "a1")
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	got, err := st.List()
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], rec) {
		t.Fatalf("List = %+v, %v; want the record", got, err)
	}
	entries, _ := os.ReadDir(st.Dir)
	for _, e := range entries {
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v, want 0600", e.Name(), info.Mode().Perm())
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := st.List(); err != nil || len(got) != 0 {
		t.Fatalf("List after Close = %+v, %v", got, err)
	}
	if entries, _ := os.ReadDir(st.Dir); len(entries) != 0 {
		t.Fatalf("files left after Close: %v", entries)
	}
}

func TestFileStoreMissingDirHasNoRecords(t *testing.T) {
	got, err := testStore(t).List()
	if err != nil || len(got) != 0 {
		t.Fatalf("List = %v, %v", got, err)
	}
}

func TestFileStoreRefusesGroupOrWorldAccess(t *testing.T) {
	st := testStore(t)
	w, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Write(sampleRecord("https://hub.example.com", "u1", "s1")); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []os.FileMode{0o750, 0o707} {
		if err := os.Chmod(st.Dir, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := st.List(); err == nil || !strings.Contains(err.Error(), "group- or world-accessible") {
			t.Fatalf("dir mode %o: List = %v, want a refusal", mode, err)
		}
		if _, err := st.Open(); err == nil {
			t.Fatalf("dir mode %o: Open accepted", mode)
		}
	}
	if err := os.Chmod(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}

	recs, _ := filepath.Glob(filepath.Join(st.Dir, "*"+recordSuffix))
	if len(recs) != 1 {
		t.Fatalf("records %v", recs)
	}
	for _, mode := range []os.FileMode{0o640, 0o604} {
		if err := os.Chmod(recs[0], mode); err != nil {
			t.Fatal(err)
		}
		if _, err := st.List(); err == nil || !strings.Contains(err.Error(), "group- or world-accessible") {
			t.Fatalf("record mode %o: List = %v, want a refusal", mode, err)
		}
	}
	if err := os.Chmod(recs[0], 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := st.List(); err != nil || len(got) != 1 {
		t.Fatalf("List after restoring modes = %v, %v", got, err)
	}
}

// fakeFileInfo is a stat result with a chosen mode and owner.
type fakeFileInfo struct {
	mode os.FileMode
	uid  uint32
}

func (f fakeFileInfo) Name() string       { return "x" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

func TestCheckPrivateRefusesOtherOwner(t *testing.T) {
	me := uint32(os.Getuid())
	if err := checkPrivate("f", fakeFileInfo{mode: 0o600, uid: me}); err != nil {
		t.Fatalf("own 0600 file refused: %v", err)
	}
	err := checkPrivate("f", fakeFileInfo{mode: 0o600, uid: me + 1})
	if err == nil || !strings.Contains(err.Error(), "owned by another user") {
		t.Fatalf("file owned by another user: %v, want a refusal", err)
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func TestFileStoreHeldLockIsLiveReleasedLockIsPruned(t *testing.T) {
	st := testStore(t)
	live, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	if err := live.Write(sampleRecord("https://hub.example.com", "u1", "s-live")); err != nil {
		t.Fatal(err)
	}

	// A record whose lock nobody holds: its process has ended.
	dead := filepath.Join(st.Dir, "999-dead")
	data, _ := json.Marshal(sampleRecord("https://hub.example.com", "u1", "s-dead"))
	if err := os.WriteFile(dead+recordSuffix, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dead+lockSuffix, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A record with no lock file at all.
	orphan := filepath.Join(st.Dir, "998-orphan")
	if err := os.WriteFile(orphan+recordSuffix, data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0].SessionIDs, []string{"s-live"}) {
		t.Fatalf("List = %+v, want only the live record", got)
	}
	for _, p := range []string{dead + recordSuffix, dead + lockSuffix, orphan + recordSuffix} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s not pruned: %v", p, err)
		}
	}
}

func TestFileStoreRewriteIsAtomic(t *testing.T) {
	st := testStore(t)
	w, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	first := sampleRecord("https://hub.example.com", "u1", "s1", "a1")
	second := sampleRecord("https://hub.example.com", "u1", "s2", "a1", "a2")
	if err := w.Write(first); err != nil {
		t.Fatal(err)
	}
	recs, _ := filepath.Glob(filepath.Join(st.Dir, "*"+recordSuffix))
	before := mustStat(t, recs[0])
	if err := w.Write(second); err != nil {
		t.Fatal(err)
	}
	after := mustStat(t, recs[0])
	if os.SameFile(before, after) {
		t.Fatal("record rewritten in place, want a new file renamed over it")
	}
	data, err := os.ReadFile(recs[0])
	if err != nil {
		t.Fatal(err)
	}
	var got Record
	if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, second) {
		t.Fatalf("record = %+v, %v; want the second write", got, err)
	}
	if tmp, _ := filepath.Glob(filepath.Join(st.Dir, ".tmp-*")); len(tmp) != 0 {
		t.Fatalf("temporary files left: %v", tmp)
	}
}

func TestMergeStatus(t *testing.T) {
	recs := []Record{
		sampleRecord("https://Hub.example.com/", "u1", "s1", "a1"),
		sampleRecord("https://hub.example.com:443", "u1", "s2", "b1"),
		sampleRecord("https://hub.example.com", "u2", "s3", "c1"),
		sampleRecord("https://other.example.com", "u1", "s4", "d1"),
	}
	st := MergeStatus(recs, "")
	if len(st.Entries) != 3 {
		t.Fatalf("entries = %+v, want 3 (one per endpoint and identity)", st.Entries)
	}
	e := st.Entries[0]
	if e.Endpoint != "https://hub.example.com" || e.Credential.Subject != "u1" || !reflect.DeepEqual(e.SessionIDs, []string{"s1", "s2"}) || len(e.Tunnels) != 2 {
		t.Fatalf("merged entry = %+v", e)
	}
	if st.Entries[1].Credential.Subject != "u2" || st.Entries[2].Endpoint != "https://other.example.com" {
		t.Fatalf("entries = %+v", st.Entries)
	}

	filtered := MergeStatus(recs, "https://OTHER.example.com/")
	if len(filtered.Entries) != 1 || filtered.Entries[0].SessionIDs[0] != "s4" {
		t.Fatalf("filtered = %+v", filtered)
	}
	if empty := MergeStatus(nil, ""); empty.Entries == nil || len(empty.Entries) != 0 {
		t.Fatalf("empty status = %#v, want an empty list", empty)
	}
}

// The status JSON schema: endpoint, credential identity (kind, subject,
// scope), session ids and tunnels [agent, project, kind, port, opened_at,
// authz_deadline].
func TestStatusJSONSchema(t *testing.T) {
	data, err := json.Marshal(MergeStatus([]Record{sampleRecord("https://hub.example.com", "u1", "s1", "a1")}, ""))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string][]map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	entry := doc["entries"][0]
	assertKeys(t, entry, "credential", "endpoint", "session_ids", "tunnels")
	assertKeys(t, entry["credential"].(map[string]any), "kind", "scope", "subject")
	tunnel := entry["tunnels"].([]any)[0].(map[string]any)
	assertKeys(t, tunnel, "agent", "authz_deadline", "kind", "opened_at", "port", "project")
}

func assertKeys(t *testing.T, m map[string]any, want ...string) {
	t.Helper()
	var got []string
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys %v, want %v", got, want)
	}
}

// A running tunnel publishes its record while open and removes it on
// exit; neither the record nor the status holds the secret.
func TestRunPublishesRecordWithoutSecret(t *testing.T) {
	const secret = "secret-token-5d1c"
	h := newFakeHub(t)
	h.set(func() { h.users[secret] = "u1" })
	rest := newFakeREST()
	rest.users[secret] = User{ID: "u1"}
	st := testStore(t)
	cfg := testConfig(h, rest, credSeq(oauth(secret)))
	cfg.State = st
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, conn, "hi")

	recs, err := st.List()
	if err != nil || len(recs) != 1 {
		t.Fatalf("List = %+v, %v", recs, err)
	}
	r := recs[0]
	if r.Endpoint != h.srv.URL || !reflect.DeepEqual(r.SessionIDs, []string{"s1"}) || r.Credential != (Identity{Kind: CredentialOAuth, Subject: "u1", Scope: ScopeHub}) {
		t.Fatalf("record = %+v", r)
	}
	if len(r.Tunnels) != 1 || r.Tunnels[0].Agent != "a1" || r.Tunnels[0].Port != 8080 || r.Tunnels[0].Kind != "tcp" || r.Tunnels[0].AuthzDeadline == nil {
		t.Fatalf("tunnels = %+v", r.Tunnels)
	}
	entries, _ := os.ReadDir(st.Dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(st.Dir, e.Name()))
		if strings.Contains(string(b), secret) {
			t.Fatalf("%s holds the secret", e.Name())
		}
	}
	out, _ := json.Marshal(MergeStatus(recs, ""))
	if strings.Contains(string(out), secret) {
		t.Fatal("status output holds the secret")
	}

	_ = conn.Close()
	cancel()
	<-done
	if recs, err := st.List(); err != nil || len(recs) != 0 {
		t.Fatalf("records after exit: %+v, %v", recs, err)
	}
}

// List never prunes the lock of a live process, even before its first
// record is written, and ignores writers' dot-prefixed temporary files.
func TestFileStoreListCannotPruneHeldLock(t *testing.T) {
	st := testStore(t)
	w, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	stray := filepath.Join(st.Dir, ".123-abc.lock.tmp")
	if err := os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := st.List(); err != nil || len(got) != 0 {
		t.Fatalf("List before the first record = %v, %v", got, err)
	}
	locks, _ := filepath.Glob(filepath.Join(st.Dir, "*"+lockSuffix))
	if len(locks) != 1 {
		t.Fatalf("lock files after List = %v, want the live one kept", locks)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("List touched a dot-prefixed file: %v", err)
	}
	if err := w.Write(sampleRecord("https://hub.example.com", "u1", "s1")); err != nil {
		t.Fatal(err)
	}
	if got, err := st.List(); err != nil || len(got) != 1 {
		t.Fatalf("List after the first record = %v, %v", got, err)
	}
}

// memRecord is a RecordWriter that keeps every record written.
type memRecord struct {
	mu   sync.Mutex
	recs []Record
}

func (m *memRecord) Write(r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, r)
	return nil
}

func (m *memRecord) Close() error { return nil }

func (m *memRecord) all() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Record(nil), m.recs...)
}

// A record older than the newest one written is never written over it.
func TestPublishSkipsStaleRecords(t *testing.T) {
	mem := &memRecord{}
	c := &client{cfg: Config{Endpoint: "https://hub.example.com"}, record: mem, tunnels: map[*tunnelEntry]struct{}{}}
	c.publish()
	if len(mem.all()) != 1 {
		t.Fatalf("records = %d, want 1", len(mem.all()))
	}
	c.written = 100 // a newer record was written meanwhile
	c.publish()
	if len(mem.all()) != 1 {
		t.Fatalf("a stale record was written over a newer one (%d records)", len(mem.all()))
	}
}

// Records, status JSON and status text never show the endpoint's userinfo,
// query or fragment.
func TestPublishRedactsEndpoint(t *testing.T) {
	st := testStore(t)
	w, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	ep := "https://alice:s3cret@hub.example.com/base/?token=TOK123#frag"
	c := &client{cfg: Config{Endpoint: ep}, record: w, tunnels: map[*tunnelEntry]struct{}{}}
	c.publish()
	recs, err := st.List()
	if err != nil || len(recs) != 1 {
		t.Fatalf("List = %v, %v", recs, err)
	}
	if recs[0].Endpoint != "https://hub.example.com/base" {
		t.Fatalf("record endpoint = %q", recs[0].Endpoint)
	}
	out, _ := json.Marshal(MergeStatus(recs, ""))
	files, _ := os.ReadDir(st.Dir)
	texts := []string{string(out)}
	for _, f := range files {
		b, _ := os.ReadFile(filepath.Join(st.Dir, f.Name()))
		texts = append(texts, string(b))
	}
	for _, text := range texts {
		for _, secret := range []string{"alice", "s3cret", "TOK123", "frag"} {
			if strings.Contains(text, secret) {
				t.Fatalf("%q contains %q", text, secret)
			}
		}
	}
}

// Error lines never show the endpoint's userinfo, query or fragment.
func TestErrorLinesRedactEndpoint(t *testing.T) {
	h := newFakeHub(t)
	secrets := []string{"alice", "s3cret", "TOK123", "frag"}
	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		for _, s := range secrets {
			if strings.Contains(err.Error(), s) {
				t.Errorf("%s: error %q contains %q", name, err, s)
			}
		}
	}
	withUser := strings.Replace(h.srv.URL, "http://", "http://alice:s3cret@", 1) + "/?token=TOK123#frag"
	cfg := testConfig(h, newFakeREST(), credSeq(Credential{}))
	cfg.Endpoint = withUser
	check("no credential", Run(runCtx(t), cfg, stdioReq(strings.NewReader("x"), io.Discard)))

	cfg = testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
	cfg.Endpoint = withUser // the dial is refused for the userinfo
	check("dial with userinfo", Run(runCtx(t), cfg, stdioReq(strings.NewReader("x"), io.Discard)))

	rest := newFakeREST()
	rest.users["unknown-token"] = User{ID: "u1"}
	cfg = testConfig(h, rest, credSeq(oauth("unknown-token")))
	cfg.Endpoint = h.srv.URL + "/?token=TOK123#frag" // the dial is refused with 401
	err := Run(runCtx(t), cfg, stdioReq(strings.NewReader("x"), io.Discard))
	if CodeOf(err) != ExitCredential {
		t.Fatalf("dial 401: exit %d (%v)", CodeOf(err), err)
	}
	check("dial 401 login hint", err)

	rest = newFakeREST()
	rest.whoamiErr = errors.New(`Get "` + withUser + `": connection refused`)
	cfg = testConfig(h, rest, credSeq(oauth("tok1")))
	cfg.Endpoint = withUser
	check("REST transport error", Run(runCtx(t), cfg, stdioReq(strings.NewReader("x"), io.Discard)))
}

// The record lists every session that carries an open tunnel, plus the
// current one.
func TestRecordListsSessionsWithOpenTunnels(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			if c.session == 1 && c.n > 1 {
				return jsonResp(401, errBody("unauthorized", "credential refused", ""))
			}
			return openTunnelResp(c, echo)
		}
	})
	st := testStore(t)
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1"), oauth("tok2")))
	cfg.State = st
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, first, "on session 1")
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	roundTrip(t, second, "on session 2")

	recs, err := st.List()
	if err != nil || len(recs) != 1 || !reflect.DeepEqual(recs[0].SessionIDs, []string{"s1", "s2"}) {
		t.Fatalf("records = %+v, %v; want session_ids [s1 s2]", recs, err)
	}

	var s1 conduit.LocalSession
	h.set(func() { s1 = h.live[0] })
	_ = first.Close()
	<-s1.Done() // its last tunnel ended: the record was rewritten first
	recs, err = st.List()
	if err != nil || len(recs) != 1 || !reflect.DeepEqual(recs[0].SessionIDs, []string{"s2"}) {
		t.Fatalf("records = %+v, %v; want session_ids [s2]", recs, err)
	}
	cancel()
	<-done
}
