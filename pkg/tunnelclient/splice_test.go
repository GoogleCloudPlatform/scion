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
	"io"
	"net"
	"strings"
	"sync"
	"testing"

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
		go func() { done <- splice(blocked, io.Discard, nil, fs) }()
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
	go func() { done <- splice(blocked, io.Discard, nil, fs) }()
	close(fs.resizes)
	fs.reads <- io.EOF
	fs.reads <- &conduit.CloseError{Code: conduit.CloseUnauthenticated, Reason: "authz_expired"}
	if err := <-done; CodeOf(err) != ExitPolicyClosed {
		t.Fatalf("splice = %v (exit %d), want %d", err, CodeOf(err), ExitPolicyClosed)
	}
}
