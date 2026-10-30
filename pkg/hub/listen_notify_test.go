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

//go:build !no_sqlite

package hub

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestListenThenNotify_HookPanicIsRecovered(t *testing.T) {
	done := make(chan struct{})
	ln, err := listenThenNotify("127.0.0.1:0", func() {
		defer close(done)
		panic("hook failure")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook did not run")
	}
	// The process is still alive and the listener still accepts.
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("the listener stopped accepting after a hook panic: %v", err)
	}
	_ = conn.Close()
}

// waitStartWithHook runs start in a goroutine, waits for the on-listening
// hook, then cancels ctx and waits for start to return.
func waitStartWithHook(t *testing.T, fired <-chan struct{}, start func(ctx context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- start(ctx) }()
	select {
	case <-fired:
	case err := <-errCh:
		t.Fatalf("Start returned before the hook fired: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the on-listening hook did not fire")
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after the context was cancelled")
	}
}

func TestServerStart_FiresOnListening(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := newTestHubServer(t, ServerConfig{Host: "127.0.0.1", Port: 0}, s)
	if err != nil {
		t.Fatal(err)
	}
	fired := make(chan struct{}, 1)
	srv.SetOnListening(func() { fired <- struct{}{} })
	waitStartWithHook(t, fired, srv.Start)
}

func TestWebServerStart_FiresOnListening(t *testing.T) {
	ws := NewWebServer(WebServerConfig{Host: "127.0.0.1"})
	ws.config.Port = 0 // NewWebServer defaults 0 to 8080; use an ephemeral port
	fired := make(chan struct{}, 1)
	ws.SetOnListening(func() { fired <- struct{}{} })
	waitStartWithHook(t, fired, ws.Start)
}

func TestWebServerStart_BindFailureSkipsOnListening(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	port := taken.Addr().(*net.TCPAddr).Port

	ws := NewWebServer(WebServerConfig{Host: "127.0.0.1", Port: port})
	fired := make(chan struct{}, 1)
	ws.SetOnListening(func() { fired <- struct{}{} })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ws.Start(ctx); err == nil {
		t.Fatal("Start on a taken port succeeded")
	}
	select {
	case <-fired:
		t.Fatal("the hook fired after a failed bind")
	case <-time.After(200 * time.Millisecond):
	}
}
