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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Attachment delivery into the agent's home (ptone/scion#4596, design
// section 8.4), against a fake runtime that runs the broker's scripts in a
// local shell with HOME set to a temporary agent home.

// inboxRuntime is a fake runtime: ExecWithStdin runs the command locally
// with HOME set to home, after unwrapping ExecAsUserCmd (the local user is
// not the agent user). It records every command.
type inboxRuntime struct {
	scionrt.Runtime
	name    string
	home    string
	mu      sync.Mutex
	cmds    [][]string
	stdins  []int
	failAll error
	// truncate, when set, cuts the stdin of every exec to that many bytes.
	truncate int
	hook     func(cmd []string) error
	// rewrite, when set, changes each command before it runs.
	rewrite func(cmd []string) []string
}

func (r *inboxRuntime) Name() string { return r.name }

func (r *inboxRuntime) ExecWithStdin(ctx context.Context, _ string, cmd []string, stdin io.Reader) (string, error) {
	data, _ := io.ReadAll(stdin)
	r.mu.Lock()
	r.cmds = append(r.cmds, cmd)
	r.stdins = append(r.stdins, len(data))
	r.mu.Unlock()
	if r.failAll != nil {
		return "", r.failAll
	}
	if r.truncate > 0 && len(data) > r.truncate {
		data = data[:r.truncate]
	}
	if r.hook != nil {
		if err := r.hook(cmd); err != nil {
			return "", err
		}
	}
	if r.rewrite != nil {
		cmd = r.rewrite(cmd)
	}
	argv := cmd
	if len(cmd) == 6 && cmd[0] == "sh" && cmd[3] == "exec-as-user" {
		argv = []string{"/bin/sh", "-c", cmd[5]}
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Env = append(os.Environ(), "HOME="+r.home)
	c.Stdin = bytes.NewReader(data)
	out, err := c.CombinedOutput()
	return string(out), err
}

func (r *inboxRuntime) commands() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.cmds...)
}

// fakeFetcher serves capability paths from a map of bodies.
type fakeFetcher struct {
	bodies  map[string][]byte
	block   chan struct{}
	mu      sync.Mutex
	fetched []string
}

func (f *fakeFetcher) FetchAttachmentDelivery(ctx context.Context, path string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.fetched = append(f.fetched, path)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	b, ok := f.bodies[path]
	if !ok {
		return nil, errors.New("refused")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// inboxServer returns a Server with a fresh spool.
func inboxServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{stateDir: t.TempDir()}
	s.initInboxSpool()
	require.NotEmpty(t, s.inbox.spool)
	return s
}

// wireFor returns a wire attachment for content named name, with its
// capability path served by f.
func wireFor(f *fakeFetcher, msgID, name string, content []byte) messages.WireAttachment {
	sum := sha256.Sum256(content)
	path := "/api/v1/attachment-deliveries/" + uuid.NewString()
	if f.bodies == nil {
		f.bodies = map[string][]byte{}
	}
	f.bodies[path] = content
	return messages.WireAttachment{ID: uuid.NewString(), Name: name, Size: int64(len(content)),
		SHA256: hex.EncodeToString(sum[:]), Path: "~/.scion/inbox/" + msgID + "/" + name, URL: path}
}

// deliver starts a delivery of wire for message msgID and runs its write
// against rt, returning the not-delivered text.
func deliver(t *testing.T, s *Server, f *fakeFetcher, rt *inboxRuntime, msgID string, wire ...messages.WireAttachment) string {
	t.Helper()
	d := startWith(s, f, msgID, wire...)
	require.NotNil(t, d)
	return d.BeforeText(context.Background(), rt, "container-1")
}

// startWith is startInboxDelivery with the fetcher set before redemption
// starts.
func startWith(s *Server, f *fakeFetcher, msgID string, wire ...messages.WireAttachment) *inboxDelivery {
	inboxFetcherOverride = f
	defer func() { inboxFetcherOverride = nil }()
	return s.startInboxDelivery("", "agent-1", msgID, 0, wire)
}

func readInbox(t *testing.T, home, msgID, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".scion", "inbox", msgID, name))
	require.NoError(t, err)
	return string(b)
}

func newInboxRuntime(t *testing.T, name string) *inboxRuntime {
	t.Helper()
	return &inboxRuntime{name: name, home: t.TempDir()}
}

// TestInboxWriter_DeliversVerifiedFiles: verified files land in
// ~/.scion/inbox/<message-id>/ with private modes, and nothing is
// reported; the spool is removed afterwards (TestInboxSpool_RemovedAfterDelivery).
func TestInboxWriter_DeliversVerifiedFiles(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	d := startWith(s, f, msgID, wireFor(f, msgID, "a.txt", []byte("alpha")), wireFor(f, msgID, "b.txt", []byte("beta")))
	text := d.BeforeText(context.Background(), rt, "c1")
	require.Empty(t, text)
	require.Equal(t, "alpha", readInbox(t, rt.home, msgID, "a.txt"))
	require.Equal(t, "beta", readInbox(t, rt.home, msgID, "b.txt"))
	fi, err := os.Stat(filepath.Join(rt.home, ".scion", "inbox", msgID))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	entries, err := os.ReadDir(s.inbox.spool)
	require.NoError(t, err)
	require.Empty(t, entries, "the spool is removed after the delivery")
}

func TestInboxSpool_RemovedAfterDelivery(t *testing.T) {
	s := inboxServer(t)
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	d := startWith(s, f, msgID, wireFor(f, msgID, "a.txt", []byte("alpha")))
	d.BeforeText(context.Background(), &inboxRuntime{name: "docker", failAll: errors.New("exec failed")}, "c1")
	entries, err := os.ReadDir(s.inbox.spool)
	require.NoError(t, err)
	require.Empty(t, entries, "the spool is removed after a failed delivery too")
}

// TestInboxWriter_HashMismatchNotWritten (K20) and
// TestInboxWriter_SizeMismatchNotWritten.
func TestInboxWriter_HashMismatchNotWritten(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	w := wireFor(f, msgID, "a.txt", []byte("alpha"))
	f.bodies[w.URL] = []byte("ALPHA") // same size, other bytes
	d := startWith(s, f, msgID, w)
	text := d.BeforeText(context.Background(), rt, "c1")
	require.Contains(t, text, "Attachment not delivered: ~/.scion/inbox/"+msgID+"/a.txt (5 bytes): failed verification")
	_, err := os.Stat(filepath.Join(rt.home, ".scion", "inbox", msgID, "a.txt"))
	require.True(t, os.IsNotExist(err), "a file that fails verification is not written")
}

func TestInboxWriter_SizeMismatchNotWritten(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	w := wireFor(f, msgID, "a.txt", []byte("alpha"))
	f.bodies[w.URL] = []byte("alpha and more")
	d := startWith(s, f, msgID, w)
	require.Contains(t, d.BeforeText(context.Background(), rt, "c1"), "failed verification")
	_, err := os.Stat(filepath.Join(rt.home, ".scion", "inbox", msgID))
	require.True(t, os.IsNotExist(err))
}

// TestInboxWriter_FailureLeavesNoFinalDir: a write that fails after
// the claim (the rename fails) leaves no message directory, no claim and no
// temporary directory, and every file is reported as not written.
func TestInboxWriter_FailureLeavesNoFinalDir(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	// The inbox exists but is read-only after the claim: make mv fail by
	// putting a file where the final directory goes once the claim exists.
	rt.hook = func(cmd []string) error {
		inbox := filepath.Join(rt.home, ".scion", "inbox")
		require.NoError(t, os.MkdirAll(inbox, 0o700))
		// A dangling symlink at the final path: [ -e ] is false, mv fails.
		_ = os.Symlink(filepath.Join(rt.home, "missing", "x"), filepath.Join(inbox, msgID))
		require.NoError(t, os.MkdirAll(filepath.Join(rt.home, "missing"), 0o500))
		return nil
	}
	d := startWith(s, f, msgID, wireFor(f, msgID, "a.txt", []byte("alpha")))
	text := d.BeforeText(context.Background(), rt, "c1")
	require.Contains(t, text, "could not be written to your home")
	inbox := filepath.Join(rt.home, ".scion", "inbox")
	entries, err := os.ReadDir(inbox)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasPrefix(e.Name(), ".claim-"), "no claim is left behind")
		require.False(t, strings.HasPrefix(e.Name(), ".tmp-"), "no temporary directory is left behind")
		if e.Name() == msgID {
			fi, err := os.Lstat(filepath.Join(inbox, e.Name()))
			require.NoError(t, err)
			require.True(t, fi.Mode()&os.ModeSymlink != 0, "no message directory was created")
		}
	}
}

// TestInboxWriter_TruncatedStreamLeavesNoFinalDir (K26): when extraction
// fails part way (the tar stream ends early), no message directory is
// left with a partial file, and the file is reported as not written.
func TestInboxWriter_TruncatedStreamLeavesNoFinalDir(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	rt.truncate = 1024 // the header and part of the file's data
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	d := startWith(s, f, msgID, wireFor(f, msgID, "big.txt", bytes.Repeat([]byte("x"), 8192)))
	text := d.BeforeText(context.Background(), rt, "c1")
	require.Contains(t, text, "could not be written to your home")
	_, err := os.Stat(filepath.Join(rt.home, ".scion", "inbox", msgID))
	require.True(t, os.IsNotExist(err), "a failed write leaves no message directory")
}

// TestInboxWriter_ExistingFinalDir (K30): a delivery of a message already
// delivered (its claim and directory exist) changes nothing and nests
// nothing.
func TestInboxWriter_ExistingFinalDir(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	require.Empty(t, deliver(t, s, f, rt, msgID, wireFor(f, msgID, "a.txt", []byte("first"))))
	require.Empty(t, deliver(t, s, f, rt, msgID, wireFor(f, msgID, "a.txt", []byte("second"))))
	require.Equal(t, "first", readInbox(t, rt.home, msgID, "a.txt"))
	entries, err := os.ReadDir(filepath.Join(rt.home, ".scion", "inbox", msgID))
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing nested into the existing directory")
}

// TestInboxWriter_ConcurrentSameMessage: two concurrent writers of one
// message: exactly one renames, and neither nests.
func TestInboxWriter_ConcurrentSameMessage(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	msgID := uuid.NewString()
	var ds []*inboxDelivery
	for i := 0; i < 2; i++ {
		f := &fakeFetcher{}
		ds = append(ds, startWith(s, f, msgID, wireFor(f, msgID, "a.txt", []byte{byte('0' + i)})))
	}
	var wg sync.WaitGroup
	texts := make([]string, len(ds))
	for i, d := range ds {
		wg.Add(1)
		go func(i int, d *inboxDelivery) {
			defer wg.Done()
			texts[i] = d.BeforeText(context.Background(), rt, "c1")
		}(i, d)
	}
	wg.Wait()
	for i, text := range texts {
		require.Empty(t, text, "writer %d: a concurrent delivery of the same message counts as delivered", i)
	}
	entries, err := os.ReadDir(filepath.Join(rt.home, ".scion", "inbox", msgID))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "a.txt", entries[0].Name())
	top, err := os.ReadDir(filepath.Join(rt.home, ".scion", "inbox"))
	require.NoError(t, err)
	for _, e := range top {
		require.False(t, strings.HasPrefix(e.Name(), ".tmp-") || strings.HasPrefix(e.Name(), ".upload-"), "left behind: %s", e.Name())
	}
}

// TestInboxWriter_TwoBufferedMessages: two messages delivered one after the
// other keep their own directories, and a failure of one does not touch
// the other (verification, the temporary directory and the claim, across
// messages).
func TestInboxWriter_TwoBufferedMessages(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	f := &fakeFetcher{}
	m1, m2 := uuid.NewString(), uuid.NewString()
	bad := wireFor(f, m2, "b.txt", []byte("beta"))
	f.bodies[bad.URL] = []byte("BETA")
	d1 := startWith(s, f, m1, wireFor(f, m1, "a.txt", []byte("alpha")))
	d2 := startWith(s, f, m2, bad, wireFor(f, m2, "c.txt", []byte("gamma")))
	require.Empty(t, d1.BeforeText(context.Background(), rt, "c1"))
	text := d2.BeforeText(context.Background(), rt, "c1")
	require.Contains(t, text, "b.txt (4 bytes): failed verification")
	require.NotContains(t, text, "c.txt")
	require.Equal(t, "alpha", readInbox(t, rt.home, m1, "a.txt"))
	require.Equal(t, "gamma", readInbox(t, rt.home, m2, "c.txt"))
	_, err := os.Stat(filepath.Join(rt.home, ".scion", "inbox", m2, "b.txt"))
	require.True(t, os.IsNotExist(err))
}

// TestInboxWriter_NotDeliveredLine: the lines come after a blank line, name
// the path the envelope listed and carry no command; an entry without URL
// is reported as not downloaded.
func TestInboxWriter_NotDeliveredLine(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	w := wireFor(f, msgID, "a.txt", []byte("alpha"))
	w.URL = ""
	text := deliver(t, s, f, rt, msgID, w)
	require.True(t, strings.HasPrefix(text, "\n\n"), "after a blank line")
	require.Equal(t, "\n\nAttachment not delivered: ~/.scion/inbox/"+msgID+"/a.txt (5 bytes): could not be downloaded. Ask the sender to resend it.", text)
	require.NotContains(t, text, "scion ")
}

// TestInboxWriter_RedeemsAtReceipt: redemption starts at once, on a context
// that outlives the request, without waiting; the write waits for it within
// the bound, and a download still running at the bound is reported as not
// downloaded.
func TestInboxWriter_RedeemsAtReceipt(t *testing.T) {
	old := inboxRedeemTimeout
	inboxRedeemTimeout = 300 * time.Millisecond
	t.Cleanup(func() { inboxRedeemTimeout = old })
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	f := &fakeFetcher{block: make(chan struct{})}
	msgID := uuid.NewString()
	start := time.Now()
	d := startWith(s, f, msgID, wireFor(f, msgID, "a.txt", []byte("alpha")))
	require.Less(t, time.Since(start), 100*time.Millisecond, "starting does not wait for the download")
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.fetched) == 1
	}, time.Second, 5*time.Millisecond, "redemption started at receipt")
	text := d.BeforeText(context.Background(), rt, "c1")
	require.Contains(t, text, "could not be downloaded", "still downloading at the bound")
}

// TestInboxSpool_CleanedAtStart: leftover spools are removed at start.
func TestInboxSpool_CleanedAtStart(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "inbox-spool", "msg-xyz")
	require.NoError(t, os.MkdirAll(left, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(left, "a.txt"), []byte("x"), 0o600))
	s := &Server{stateDir: dir}
	s.initInboxSpool()
	entries, err := os.ReadDir(s.inbox.spool)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// TestInboxWriter_SubstrateStdinUnsupported: a Substrate exec that cannot
// take stdin fails the write, and the files are reported as not written.
func TestInboxWriter_SubstrateStdinUnsupported(t *testing.T) {
	s := inboxServer(t)
	rt := &inboxRuntime{name: "substrate", failAll: errors.New("exec stdin is not supported")}
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	require.Contains(t, deliver(t, s, f, rt, msgID, wireFor(f, msgID, "a.txt", []byte("alpha"))), "could not be written to your home")
}

// TestInboxWriter_SubstrateChunks: a Substrate write sends the tar in
// chunks of at most 512 KiB, then extracts it.
func TestInboxWriter_SubstrateChunks(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "substrate")
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	big := bytes.Repeat([]byte("x"), 900<<10)
	require.Empty(t, deliver(t, s, f, rt, msgID, wireFor(f, msgID, "big.bin", big)))
	require.Equal(t, len(big), len(readInbox(t, rt.home, msgID, "big.bin")))
	rt.mu.Lock()
	defer rt.mu.Unlock()
	require.GreaterOrEqual(t, len(rt.stdins), 3, "at least two chunks and the extract")
	for _, n := range rt.stdins {
		require.LessOrEqual(t, n, substrateChunkBytes)
	}
}

// TestInboxWriter_CloudRunRunsAsScion (K27): the write runs as the agent
// user through ExecAsUserCmd (the Cloud Run exec connector runs as root).
func TestInboxWriter_CloudRunRunsAsScion(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "cloudrun")
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	require.Empty(t, deliver(t, s, f, rt, msgID, wireFor(f, msgID, "a.txt", []byte("alpha"))))
	cmds := rt.commands()
	require.NotEmpty(t, cmds)
	for _, c := range cmds {
		require.Len(t, c, 6)
		require.Equal(t, "exec-as-user", c[3])
		require.Equal(t, "scion", c[4], "the write runs as the agent user")
	}
}

// TestInboxPrune_OnlyInboxDirs (K28): pruning removes old message
// directories and their claims, and leaves a symlink to outside the inbox
// and an .nfs file alone.
func TestInboxPrune_OnlyInboxDirs(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	inbox := filepath.Join(rt.home, ".scion", "inbox")
	old := filepath.Join(inbox, "old-message")
	require.NoError(t, os.MkdirAll(old, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(inbox, ".claim-old-message"), 0o700))
	outside := filepath.Join(rt.home, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o600))
	link := filepath.Join(inbox, "link-out")
	require.NoError(t, os.Symlink(outside, link))
	nfs := filepath.Join(inbox, ".nfs0000123")
	require.NoError(t, os.WriteFile(nfs, []byte("x"), 0o600))
	// Stale claims without a message directory: a claim file, and a claim
	// directory from an older broker.
	staleFile := filepath.Join(inbox, ".claim-gone-file")
	require.NoError(t, os.WriteFile(staleFile, nil, 0o600))
	staleDir := filepath.Join(inbox, ".claim-gone-dir")
	require.NoError(t, os.MkdirAll(staleDir, 0o700))
	past := time.Now().Add(-30 * 24 * time.Hour)
	for _, p := range []string{old, filepath.Join(inbox, ".claim-old-message"), outside, staleFile, staleDir} {
		require.NoError(t, os.Chtimes(p, past, past))
	}
	s.maybePruneInbox(rt, "c1", "agent-1", 7)
	_, err := os.Stat(old)
	require.True(t, os.IsNotExist(err), "an old message directory is pruned")
	_, err = os.Stat(filepath.Join(inbox, ".claim-old-message"))
	require.True(t, os.IsNotExist(err), "its claim is pruned with it")
	for _, p := range []string{staleFile, staleDir} {
		_, err = os.Lstat(p)
		require.True(t, os.IsNotExist(err), "a stale claim without a message directory is pruned: %s", p)
	}
	_, err = os.Lstat(link)
	require.NoError(t, err, "a symlink is left alone")
	b, err := os.ReadFile(filepath.Join(outside, "keep.txt"))
	require.NoError(t, err)
	require.Equal(t, "keep", string(b), "nothing outside the inbox is touched")
	_, err = os.Stat(nfs)
	require.NoError(t, err, "an .nfs file is left alone")
}

// TestInboxWriter_FinalDirWithoutClaimCountsAsDelivered: a message
// directory that already exists when the claim is fresh (its claim was
// pruned) counts as delivered: nothing is nested or replaced, and no claim
// or temporary directory is left to block a later delivery wrongly.
func TestInboxWriter_FinalDirWithoutClaimCountsAsDelivered(t *testing.T) {
	s := inboxServer(t)
	rt := newInboxRuntime(t, "docker")
	f := &fakeFetcher{}
	msgID := uuid.NewString()
	final := filepath.Join(rt.home, ".scion", "inbox", msgID)
	require.NoError(t, os.MkdirAll(final, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(final, "a.txt"), []byte("first"), 0o600))
	require.Empty(t, deliver(t, s, f, rt, msgID, wireFor(f, msgID, "a.txt", []byte("second"))))
	require.Equal(t, "first", readInbox(t, rt.home, msgID, "a.txt"))
	entries, err := os.ReadDir(final)
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing nested")
	top, err := os.ReadDir(filepath.Dir(final))
	require.NoError(t, err)
	for _, e := range top {
		require.False(t, strings.HasPrefix(e.Name(), ".tmp-"), "no temporary directory is left behind")
	}
}

// TestInboxCommand_SandboxVector: the Cloud Run sandbox gets an absolute
// shell (its exec has no PATH) and every script sets its own PATH; other
// runtimes go through ExecAsUserCmd as the agent user.
func TestInboxCommand_SandboxVector(t *testing.T) {
	cmd := inboxCommand(&inboxRuntime{name: "cloudrun-sandbox"}, inboxWriteScript, "m1", "r1")
	require.Equal(t, []string{"/bin/sh", "-c", inboxWriteScript, "scion-inbox", "m1", "r1"}, cmd)
	for _, script := range []string{inboxWriteScript, inboxAppendScript, inboxPruneScript} {
		require.True(t, strings.HasPrefix(script, inboxScriptPath), "every script sets PATH first")
	}
	other := inboxCommand(&inboxRuntime{name: "docker"}, inboxWriteScript, "m1", "r1")
	require.Len(t, other, 6)
	require.Equal(t, "exec-as-user", other[3])
	require.Equal(t, "scion", other[4])
}

// TestInboxSpool_PrivateWithoutStateDir: without a state directory the
// spool is a new private temporary directory, never a reused path.
func TestInboxSpool_PrivateWithoutStateDir(t *testing.T) {
	a := &Server{}
	a.initInboxSpool()
	b := &Server{}
	b.initInboxSpool()
	t.Cleanup(func() { _ = os.RemoveAll(a.inbox.spool); _ = os.RemoveAll(b.inbox.spool) })
	require.NotEmpty(t, a.inbox.spool)
	require.NotEqual(t, a.inbox.spool, b.inbox.spool, "a new directory each time")
	info, err := os.Lstat(a.inbox.spool)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// inboxMoveLine is the move of the write script, which
// TestInboxWriter_ClaimHoldsAConcurrentWriter holds.
const inboxMoveLine = `    mv "$tmp" "$inbox/$1"`

// TestInboxWriter_ClaimHoldsAConcurrentWriter (K30b): one writer of a
// message is held after its claim and existence check, just before its
// move, while a second writer of the same message runs. The second finds
// the claim taken and counts the message as delivered without moving
// anything; when the first is released it moves its directory, and nothing
// is nested. The second writer runs once with the system mkdir and once
// with a mkdir that also succeeds when the directory already exists (as
// uutils coreutils 0.8 does when it loses a race, ptone/scion#6281): the
// claim must not depend on mkdir(1) failing on an existing directory.
func TestInboxWriter_ClaimHoldsAConcurrentWriter(t *testing.T) {
	t.Run("system mkdir", func(t *testing.T) { testClaimHoldsAConcurrentWriter(t, false) })
	t.Run("mkdir that succeeds on an existing directory", func(t *testing.T) { testClaimHoldsAConcurrentWriter(t, true) })
}

func testClaimHoldsAConcurrentWriter(t *testing.T, lenientMkdir bool) {
	s := inboxServer(t)
	held := newInboxRuntime(t, "docker")
	other := &inboxRuntime{name: "docker", home: held.home}
	if lenientMkdir {
		other.rewrite = lenientMkdirRewrite(t)
	}
	reached := filepath.Join(held.home, ".reached")
	release := filepath.Join(held.home, ".release")
	require.Contains(t, inboxWriteScript, inboxMoveLine)
	held.rewrite = func(cmd []string) []string {
		out := append([]string(nil), cmd...)
		out[len(out)-1] = strings.Replace(out[len(out)-1], inboxMoveLine,
			`    touch "$HOME/.reached"; while [ ! -e "$HOME/.release" ]; do sleep 0.05; done`+"\n"+inboxMoveLine, 1)
		return out
	}
	// A failure before the release must not leave the held shell waiting.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	msgID := uuid.NewString()
	f1, f2 := &fakeFetcher{}, &fakeFetcher{}
	d1 := startWith(s, f1, msgID, wireFor(f1, msgID, "a.txt", []byte("one")))
	d2 := startWith(s, f2, msgID, wireFor(f2, msgID, "a.txt", []byte("two")))
	first := make(chan string, 1)
	go func() { first <- d1.BeforeText(context.Background(), held, "c1") }()
	require.Eventually(t, func() bool { _, err := os.Stat(reached); return err == nil },
		10*time.Second, 10*time.Millisecond, "the first writer reaches its move")
	require.Empty(t, d2.BeforeText(context.Background(), other, "c1"), "a held claim counts the message as delivered")
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	require.Empty(t, <-first)
	entries, err := os.ReadDir(filepath.Join(held.home, ".scion", "inbox", msgID))
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing nested")
	require.Equal(t, "one", readInbox(t, held.home, msgID, "a.txt"), "the claim holder's files")
}

// lenientMkdirRewrite returns a command rewrite that puts first on the
// script's PATH a mkdir which also succeeds when its directory already
// exists, without -p.
func lenientMkdirRewrite(t *testing.T) func([]string) []string {
	t.Helper()
	realMkdir, err := exec.LookPath("mkdir")
	require.NoError(t, err)
	dir := t.TempDir()
	shim := "#!/bin/sh\nfor last; do :; done\n" + shellQuote(realMkdir) + ` "$@" 2>/dev/null || [ -d "$last" ]` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mkdir"), []byte(shim), 0o755))
	require.Contains(t, inboxScriptPath, "PATH=/usr/local/sbin:")
	return func(cmd []string) []string {
		out := append([]string(nil), cmd...)
		out[len(out)-1] = strings.Replace(out[len(out)-1], "PATH=/usr/local/sbin:", "PATH="+dir+":/usr/local/sbin:", 1)
		return out
	}
}
