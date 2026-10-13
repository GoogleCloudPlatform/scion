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
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// finFirst is a target that sends greeting, half-closes its side at once,
// then reads the whole upload, reports it on got, and closes.
func finFirst(greeting string, got chan<- string) func(conduit.Stream) {
	return func(st conduit.Stream) {
		_, _ = io.WriteString(st, greeting)
		_ = st.(interface{ CloseWrite() error }).CloseWrite()
		data, _ := io.ReadAll(st)
		got <- string(data)
		_ = st.Close()
	}
}

// An upload sent after the remote side's FIN still arrives in full, and
// the FIN reaches the local connection as a TCP half-close.
func TestListenerUploadAfterRemoteFIN(t *testing.T) {
	h := newFakeHub(t)
	got := make(chan string, 1)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse { return openTunnelResp(c, finFirst("hello", got)) }
	})
	addr, cancel, done := listen(t, testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Read to EOF: the remote FIN half-closes the local connection.
	greeting, err := io.ReadAll(conn)
	if err != nil || string(greeting) != "hello" {
		t.Fatalf("read %q, %v; want the greeting then EOF", greeting, err)
	}
	upload := strings.Repeat("u", 300*1024) // more than one stream window
	if _, err := io.WriteString(conn, upload); err != nil {
		t.Fatalf("upload after the remote FIN: %v", err)
	}
	_ = conn.(*net.TCPConn).CloseWrite()
	if g := <-got; g != upload {
		t.Fatalf("target received %d bytes, want %d", len(g), len(upload))
	}
	cancel()
	<-done
}

// closeRecorder is a stdout that records its Close.
type closeRecorder struct {
	mu     sync.Mutex
	b      strings.Builder
	closed chan struct{}
	once   sync.Once
}

func newCloseRecorder() *closeRecorder { return &closeRecorder{closed: make(chan struct{})} }

func (c *closeRecorder) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.Write(p)
}

func (c *closeRecorder) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *closeRecorder) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

// With --stdio, the remote FIN closes stdout while stdin keeps flowing to
// the target until stdin ends.
func TestStdioRemoteFINClosesStdoutUploadContinues(t *testing.T) {
	h := newFakeHub(t)
	got := make(chan string, 1)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse { return openTunnelResp(c, finFirst("bye", got)) }
	})
	stdinR, stdinW := io.Pipe()
	stdout := newCloseRecorder()
	done := make(chan error, 1)
	go func() {
		done <- Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(stdinR, stdout))
	}()
	<-stdout.closed
	if stdout.String() != "bye" {
		t.Fatalf("stdout = %q before close", stdout.String())
	}
	if _, err := io.WriteString(stdinW, "late upload"); err != nil {
		t.Fatal(err)
	}
	_ = stdinW.Close()
	if g := <-got; g != "late upload" {
		t.Fatalf("target received %q", g)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v (exit %d)", err, CodeOf(err))
	}
}

// With --stdio, a StreamClose ends the run even though stdin stays open.
func TestStdioReturnsOnStreamCloseWithStdinOpen(t *testing.T) {
	cases := map[string]struct {
		close func(conduit.Stream)
		want  int
	}{
		"normal close": {close: func(st conduit.Stream) { _ = st.Close() }, want: ExitOK},
		"FIN then close": {close: func(st conduit.Stream) {
			_ = st.(interface{ CloseWrite() error }).CloseWrite()
			_ = st.Close()
		}, want: ExitOK},
		"FIN then authz_expired": {close: func(st conduit.Stream) {
			_ = st.(interface{ CloseWrite() error }).CloseWrite()
			_ = st.CloseWithCode(conduit.CloseUnauthenticated, "authz_expired")
		}, want: ExitPolicyClosed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newFakeHub(t)
			h.set(func() {
				h.handle = func(c rpcCall) *conduitv1.RpcResponse { return openTunnelResp(c, tc.close) }
			})
			stdinR, stdinW := io.Pipe() // never written or closed by the run
			defer func() { _ = stdinW.Close() }()
			err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), stdioReq(stdinR, newCloseRecorder()))
			if CodeOf(err) != tc.want {
				t.Fatalf("exit %d (%v), want %d", CodeOf(err), err, tc.want)
			}
		})
	}
}

// fakeStream is a conduit.Stream whose reads are fed by the test.
type fakeStream struct {
	reads   chan error // each receive ends one Read with that error
	resizes chan conduit.WindowSize
	closed  chan struct{}
	once    sync.Once
	state   conduit.StreamState
}

func newFakeStream() *fakeStream {
	return &fakeStream{reads: make(chan error, 2), resizes: make(chan conduit.WindowSize), closed: make(chan struct{})}
}

func (f *fakeStream) Read([]byte) (int, error)           { return 0, <-f.reads }
func (f *fakeStream) Write(p []byte) (int, error)        { return len(p), nil }
func (f *fakeStream) Close() error                       { return f.CloseWithCode(conduit.CloseNormal, "") }
func (f *fakeStream) CloseWrite() error                  { return nil }
func (f *fakeStream) Resize(uint16, uint16) error        { return nil }
func (f *fakeStream) ID() uint32                         { return 2 }
func (f *fakeStream) State() conduit.StreamState         { return f.state }
func (f *fakeStream) Resizes() <-chan conduit.WindowSize { return f.resizes }
func (f *fakeStream) CloseWithCode(uint32, string) error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// The stream ends (Resizes closes) before the download's read returns
// EOF, while the upload stays blocked on the local reader: splice must
// still return, with nil for a normal close.
func TestSpliceReturnsWhenStreamEndsBeforeDownloadEOF(t *testing.T) {
	for i := 0; i < 200; i++ {
		fs := newFakeStream()
		blocked, unblock := io.Pipe() // the upload's local reader never yields
		done := make(chan error, 1)
		go func() { _, err := splice(blocked, io.Discard, nil, fs); done <- err }()
		close(fs.resizes)  // the stream ends first
		fs.reads <- io.EOF // then the download sees EOF
		fs.reads <- io.EOF // and the final read reports a normal close
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("iteration %d: splice = %v, want nil", i, err)
			}
		case <-runCtx(t).Done():
			t.Fatalf("iteration %d: splice did not return after the stream ended", i)
		}
		_ = unblock.Close()
	}
}

// The same with a coded close: the final read reports the code.
func TestSpliceStreamEndedReportsCode(t *testing.T) {
	fs := newFakeStream()
	blocked, unblock := io.Pipe()
	defer func() { _ = unblock.Close() }()
	done := make(chan error, 1)
	go func() { _, err := splice(blocked, io.Discard, nil, fs); done <- err }()
	close(fs.resizes)
	fs.reads <- io.EOF
	fs.reads <- &conduit.CloseError{Code: conduit.CloseUnauthenticated, Reason: "authz_expired"}
	if err := <-done; CodeOf(err) != ExitPolicyClosed {
		t.Fatalf("splice = %v (exit %d), want %d", err, CodeOf(err), ExitPolicyClosed)
	}
}

// uploadHolder is a client-session Interceptor that holds the outbound
// frame carrying payload until release is closed.
type uploadHolder struct {
	payload string
	held    chan struct{}
	release chan struct{}
	once    sync.Once
}

func newUploadHolder(payload string) *uploadHolder {
	return &uploadHolder{payload: payload, held: make(chan struct{}), release: make(chan struct{})}
}

func (u *uploadHolder) intercept(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
	if d := f.GetStreamData(); dir == conduit.Outbound && d != nil && strings.Contains(string(d.GetData()), u.payload) {
		u.once.Do(func() { close(u.held) })
		<-u.release
	}
	return []*conduitv1.Frame{f}
}

// notReturnedYet fails if done delivers within a short window. A correct
// implementation never returns here, so the window only limits how surely
// a regression is caught; it cannot fail a correct run.
func notReturnedYet(t *testing.T, done <-chan error, what string) {
	t.Helper()
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		t.Fatalf("%s returned (%v) before its queued data was written", what, err)
	case <-timer.C:
	}
}

// Run does not return (and close the session) while the tunnel's upload
// and close are still queued: the target gets the whole upload.
func TestStdioRunWaitsForQueuedUploadBeforeReturning(t *testing.T) {
	h := newFakeHub(t)
	got := make(chan string, 1)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse { return openTunnelResp(c, finFirst("bye", got)) }
	})
	hold := newUploadHolder("late upload")
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
	cfg.Session.Interceptor = hold.intercept
	stdinR, stdinW := io.Pipe()
	stdout := newCloseRecorder()
	done := make(chan error, 1)
	go func() { done <- Run(runCtx(t), cfg, stdioReq(stdinR, stdout)) }()
	<-stdout.closed
	if _, err := io.WriteString(stdinW, "late upload"); err != nil {
		t.Fatal(err)
	}
	_ = stdinW.Close()
	<-hold.held
	notReturnedYet(t, done, "Run")
	close(hold.release)
	if g := <-got; g != "late upload" {
		t.Fatalf("target received %q", g)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v (exit %d)", err, CodeOf(err))
	}
}

// The peer closes right after reading our FIN (before our own close): Run
// returns 0 promptly, not after the flush timeout.
func TestStdioPeerClosesAfterOurFINReturnsPromptly(t *testing.T) {
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			return openTunnelResp(c, func(st conduit.Stream) {
				_, _ = io.ReadAll(st) // until our FIN
				_, _ = io.WriteString(st, "ack")
				_ = st.Close() // no FIN first
			})
		}
	})
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
	cfg.FlushTimeout = time.Minute // a hang would end in exit 6 after this
	var out syncBuffer
	if err := Run(runCtx(t), cfg, stdioReq(strings.NewReader("data"), &out)); err != nil {
		t.Fatalf("Run: %v (exit %d)", err, CodeOf(err))
	}
	if out.String() != "ack" {
		t.Fatalf("stdout = %q", out.String())
	}
}

// When the queued data cannot be written in time, the run says so with
// exit 6 instead of a silent 0.
func TestStdioFlushTimeoutExits6(t *testing.T) {
	h := newFakeHub(t)
	got := make(chan string, 1)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse { return openTunnelResp(c, finFirst("bye", got)) }
	})
	hold := newUploadHolder("late upload")
	t.Cleanup(func() { close(hold.release) })
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1")))
	cfg.Session.Interceptor = hold.intercept
	cfg.FlushTimeout = 50 * time.Millisecond
	stdinR, stdinW := io.Pipe()
	stdout := newCloseRecorder()
	done := make(chan error, 1)
	go func() { done <- Run(runCtx(t), cfg, stdioReq(stdinR, stdout)) }()
	<-stdout.closed
	if _, err := io.WriteString(stdinW, "late upload"); err != nil {
		t.Fatal(err)
	}
	_ = stdinW.Close()
	err := <-done
	if CodeOf(err) != ExitHubUnavailable || !strings.Contains(err.Error(), "may not have reached the hub") {
		t.Fatalf("Run = %v (exit %d), want %d saying the data may not have arrived", err, CodeOf(err), ExitHubUnavailable)
	}
	assertOneLine(t, err.Error())
}

// Listener twin: a replaced session is not closed while its last tunnel's
// queued upload is still being written.
func TestListenerReplacedSessionFlushesBeforeClose(t *testing.T) {
	h := newFakeHub(t)
	got := make(chan string, 1)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			switch {
			case c.session == 1 && c.n == 1:
				return openTunnelResp(c, finFirst("hello", got))
			case c.session == 1:
				return jsonResp(401, errBody("unauthorized", "credential refused", ""))
			}
			return openTunnelResp(c, echo)
		}
	})
	hold := newUploadHolder("late upload")
	cfg := testConfig(h, newFakeREST(), credSeq(oauth("tok1"), oauth("tok2")))
	cfg.Session.Interceptor = hold.intercept
	addr, cancel, done := listen(t, cfg, Request{Agent: "a1", Project: "proj", Port: 8080})
	defer cancel()

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if greeting, err := io.ReadAll(first); err != nil || string(greeting) != "hello" {
		t.Fatalf("read %q, %v", greeting, err)
	}
	second, err := net.Dial("tcp", addr) // a 401 moves new tunnels to session 2
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	roundTrip(t, second, "on session 2")

	var s1 conduit.LocalSession
	h.set(func() { s1 = h.live[0] })
	if _, err := io.WriteString(first, "late upload"); err != nil {
		t.Fatal(err)
	}
	_ = first.(*net.TCPConn).CloseWrite()
	<-hold.held
	ended := make(chan error, 1)
	go func() { <-s1.Done(); ended <- nil }()
	notReturnedYet(t, ended, "session 1")
	close(hold.release)
	if g := <-got; g != "late upload" {
		t.Fatalf("target received %q", g)
	}
	cancel()
	<-done
}

// Right after our close, a stream already in StateFailed (for example,
// the session dropped) may have lost queued data: exit 6, not 0. Only
// StateClosed returns 0 at once.
func TestAwaitFlushTerminalStates(t *testing.T) {
	c := &client{cfg: Config{Endpoint: "https://hub.example.com"}}
	entry := &tunnelEntry{} // not consulted for a terminal state
	failed := newFakeStream()
	failed.state = conduit.StateFailed
	err := c.awaitFlush(context.Background(), failed, entry)
	if CodeOf(err) != ExitHubUnavailable || !strings.Contains(err.Error(), "may not have reached the hub") {
		t.Fatalf("awaitFlush(Failed) = %v (exit %d), want %d", err, CodeOf(err), ExitHubUnavailable)
	}
	closed := newFakeStream()
	closed.state = conduit.StateClosed
	if err := c.awaitFlush(context.Background(), closed, entry); err != nil {
		t.Fatalf("awaitFlush(Closed) = %v, want nil", err)
	}
}
