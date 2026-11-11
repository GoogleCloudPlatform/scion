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

package ws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
)

// serve starts an httptest server whose handler upgrades and hands each
// Conn to fn.
func serve(t *testing.T, opts Options, fn func(*Conn)) (url string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Conduit-Test") != "yes" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		c, err := Upgrade(w, r, nil, opts)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		fn(c)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dialer(url string) *Dialer {
	return &Dialer{URL: url, Header: func(context.Context) (http.Header, error) {
		return http.Header{"X-Conduit-Test": []string{"yes"}}, nil
	}}
}

func TestWSRoundTrip(t *testing.T) {
	url := serve(t, Options{}, func(c *Conn) {
		for {
			b, err := c.ReadFrame()
			if err != nil {
				return
			}
			if err := c.WriteFrame(append([]byte("echo:"), b...)); err != nil {
				return
			}
		}
	})
	c, err := dialer(url).Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if c.Transport() != transport.WS {
		t.Fatalf("Transport() = %q", c.Transport())
	}
	for _, msg := range []string{"a", strings.Repeat("b", 64*1024)} {
		if err := c.WriteFrame([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		b, err := c.ReadFrame()
		if err != nil || string(b) != "echo:"+msg {
			t.Fatalf("got %d bytes, err %v", len(b), err)
		}
	}
}

func TestWSNonBinaryRejected(t *testing.T) {
	url := serve(t, Options{}, func(c *Conn) {
		_ = c.Underlying().WriteMessage(websocket.TextMessage, []byte("{}"))
		_, _ = c.ReadFrame() // hold the connection open until the client closes
	})
	c, err := dialer(url).Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.ReadFrame(); !errors.Is(err, ErrNotBinary) {
		t.Fatalf("err = %v, want ErrNotBinary", err)
	}
}

func TestWSFrameTooLarge(t *testing.T) {
	got := make(chan error, 1)
	url := serve(t, Options{MaxMessageSize: 1024}, func(c *Conn) {
		_, err := c.ReadFrame()
		got <- err
	})
	c, err := dialer(url).Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.WriteFrame(make([]byte, 2048))
	if err := <-got; !errors.Is(err, transport.ErrFrameTooLarge) {
		t.Fatalf("server read err = %v, want ErrFrameTooLarge", err)
	}
}

func TestWSDialErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusServiceUnavailable
		if r.URL.Path == "/auth" {
			code = http.StatusForbidden
		}
		http.Error(w, "no", code)
	}))
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	for path, want := range map[string]struct {
		status int
		auth   bool
	}{"/auth": {403, true}, "/busy": {503, false}} {
		_, err := (&Dialer{URL: base + path}).Dial(context.Background())
		var de *DialError
		if !errors.As(err, &de) || de.StatusCode != want.status || de.IsAuthDenial() != want.auth {
			t.Fatalf("%s: err = %v, want DialError{%d}, auth=%v", path, err, want.status, want.auth)
		}
	}
	// Missing credentials are a 401 from the test server.
	_, err := (&Dialer{URL: serve(t, Options{}, func(*Conn) {})}).Dial(context.Background())
	var de *DialError
	if !errors.As(err, &de) || de.StatusCode != http.StatusUnauthorized || !de.IsAuthDenial() {
		t.Fatalf("err = %v, want 401 auth denial", err)
	}
}

func TestWSCloseWithCode(t *testing.T) {
	url := serve(t, Options{}, func(c *Conn) {
		_ = c.CloseWithCode(4503, strings.Repeat("r", 200)) // reason is truncated to fit
	})
	c, err := dialer(url).Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, err = c.ReadFrame()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != 4503 {
		t.Fatalf("err = %v, want close 4503", err)
	}
}
