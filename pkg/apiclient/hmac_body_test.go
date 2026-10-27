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

package apiclient

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

type hmacTrackedBody struct {
	io.Reader
	closeCalls int
	closeErr   error
}

func (b *hmacTrackedBody) Close() error {
	b.closeCalls++
	return b.closeErr
}

func TestBuildCanonicalString_BodyOwnership(t *testing.T) {
	const payload = `{"key":"value"}`
	for _, tc := range []struct {
		name       string
		length     int64
		readErr    error
		closeErr   error
		wantClosed bool
	}{
		{name: "body", length: int64(len(payload)), wantClosed: true},
		{name: "close_error", length: int64(len(payload)), closeErr: errors.New("close failed"), wantClosed: true},
		{name: "read_error", length: int64(len(payload)), readErr: errors.New("read failed")},
		{name: "zero_length", length: 0},
		{name: "unknown_length", length: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reader io.Reader = strings.NewReader(payload)
			if tc.readErr != nil {
				reader = io.MultiReader(reader, iotest.ErrReader(tc.readErr))
			}
			body := &hmacTrackedBody{Reader: reader, closeErr: tc.closeErr}
			req, err := http.NewRequest(http.MethodPost, "https://hub.invalid/test", body)
			if err != nil {
				t.Fatal(err)
			}
			req.ContentLength = tc.length
			canonical := BuildCanonicalString(req, "123", "nonce")
			want := "POST\n/test\n\n123\nnonce\n"
			if tc.wantClosed {
				hash := sha256.Sum256([]byte(payload))
				want += base64.StdEncoding.EncodeToString(hash[:])
				if body.closeCalls != 1 {
					t.Errorf("original body closed %d times, want 1", body.closeCalls)
				}
				// Signing again must preserve the bytes without closing the
				// original resource a second time.
				if again := BuildCanonicalString(req, "123", "nonce"); string(again) != want {
					t.Errorf("repeated canonical string = %q, want %q", again, want)
				}
			} else if req.Body != body || body.closeCalls != 0 {
				t.Error("unreplaced body must remain open for its owner to close")
			}
			if string(canonical) != want {
				t.Errorf("canonical string = %q, want %q", canonical, want)
			}
			if tc.readErr == nil {
				data, err := io.ReadAll(req.Body)
				if err != nil || string(data) != payload {
					t.Errorf("body = %q, %v; want %q, nil", data, err, payload)
				}
			}
			_ = req.Body.Close()
			if body.closeCalls != 1 {
				t.Errorf("original body closed %d times after cleanup, want 1", body.closeCalls)
			}
		})
	}
}

func TestBuildCanonicalString_AbsentBody(t *testing.T) {
	for _, body := range []io.ReadCloser{nil, http.NoBody} {
		req, err := http.NewRequest(http.MethodPost, "https://hub.invalid/test", body)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(BuildCanonicalString(req, "123", "nonce")); got != "POST\n/test\n\n123\nnonce\n" {
			t.Errorf("canonical string without body = %q", got)
		}
		if req.Body != body {
			t.Error("absent body was replaced")
		}
	}
}

type hmacBodyRoundTripper func(*http.Request) (*http.Response, error)

func (f hmacBodyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHMACAuth_ClosesFileBody(t *testing.T) {
	const payload = `{"key":"value"}`
	file, err := os.CreateTemp(t.TempDir(), "body")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://hub.invalid/test", file)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(payload))
	auth := &HMACAuth{BrokerID: "broker", SecretKey: []byte("test-secret")}
	called := false
	base := hmacBodyRoundTripper(func(r *http.Request) (*http.Response, error) {
		called = true
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != payload {
			t.Errorf("sent body = %q, %v; want %q, nil", data, err, payload)
		}
		hash := sha256.Sum256([]byte(payload))
		canonical := "POST\n/test\n\n" + r.Header.Get(HeaderTimestamp) + "\n" + r.Header.Get(HeaderNonce) + "\n" + base64.StdEncoding.EncodeToString(hash[:])
		signature, err := base64.StdEncoding.DecodeString(r.Header.Get(HeaderSignature))
		if err != nil || !VerifyHMAC(auth.SecretKey, []byte(canonical), signature) {
			t.Error("signature does not cover the full body")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
	})
	client := NewTransport("https://hub.invalid", WithHTTPClient(&http.Client{Transport: base}), WithAuth(auth)).AuthenticatedHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !called {
		t.Fatal("request did not reach the base transport")
	}
	if _, err := file.ReadAt(make([]byte, 1), 0); !errors.Is(err, os.ErrClosed) {
		t.Errorf("original request file remains open: ReadAt error = %v, want os.ErrClosed", err)
	}
}
