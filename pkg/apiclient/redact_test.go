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
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// closedServerURL returns the URL of a server that is already closed, with
// the user info userinfo.
func closedServerURL(t *testing.T, userinfo string) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return strings.Replace(srv.URL, "http://", "http://"+userinfo+"@", 1)
}

func assertNoURLSecrets(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", what)
	}
	for _, s := range []string{"s3cret", "tok3n"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("%s: error %q shows %q", what, err.Error(), s)
		}
	}
	if !strings.Contains(err.Error(), "xxxxx@") {
		t.Errorf("%s: error %q does not show the masked user info", what, err.Error())
	}
}

// Transport errors quote the request URL; the hub client shows it without
// user info, whatever the request path.
func TestTransportErrors_RedactURLUserInfo(t *testing.T) {
	ctx := context.Background()
	for _, userinfo := range []string{"tok3n", "alice:s3cret"} {
		tr := NewTransport(closedServerURL(t, userinfo))
		_, err := tr.Get(ctx, "/healthz", nil)
		assertNoURLSecrets(t, "Get "+userinfo, err)
		_, err = tr.Post(ctx, "/api/v1/x", map[string]string{"a": "b"}, nil)
		assertNoURLSecrets(t, "Post "+userinfo, err)
		_, err = tr.PostNoRetry(ctx, "/api/v1/x", nil, nil)
		assertNoURLSecrets(t, "PostNoRetry "+userinfo, err)
		_, err = tr.Delete(ctx, "/api/v1/x", nil)
		assertNoURLSecrets(t, "Delete "+userinfo, err)
	}
}

// Errors from building a request show the base URL without user info.
func TestTransportRequestErrors_RedactURLUserInfo(t *testing.T) {
	ctx := context.Background()
	tr := NewTransport("http://tok3n@hub example.com")
	_, err := tr.Get(ctx, "/healthz", nil)
	assertNoURLSecrets(t, "Get", err)
	_, err = tr.Delete(ctx, "/x", nil)
	assertNoURLSecrets(t, "Delete", err)
	_, err = tr.Put(ctx, "/x", nil, nil)
	assertNoURLSecrets(t, "Put", err)
}

func TestRedactURLError(t *testing.T) {
	inner := &url.Error{Op: "Get", URL: "https://tok3n@hub.example.com/x", Err: errors.New("refused")}
	// As the transport does: redact the *url.Error, then wrap it.
	err := fmt.Errorf("wrapped: %w", redactURLError(inner))
	if want := `wrapped: Get "https://xxxxx@hub.example.com/x": refused`; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	plain := errors.New("no URL error")
	if got := redactURLError(plain); got != plain {
		t.Errorf("an error without a url.Error changed: %v", got)
	}
	for in, want := range map[string]string{
		"https://hub.example.com/x":              "https://hub.example.com/x",
		"https://alice:s3cret@hub.example.com/x": "https://alice:xxxxx@hub.example.com/x",
		"https://tok3n@hub.example.com":          "https://xxxxx@hub.example.com",
		"https://a@s3cret@hub example.com/%zz":   "https://xxxxx@hub example.com/%zz",
	} {
		if got := redactURLUserInfo(in); got != want {
			t.Errorf("redactURLUserInfo(%q) = %q, want %q", in, got, want)
		}
	}
}

// A URL with a user name and no password.
func TestTransportGet_RedactsUserName(t *testing.T) {
	_, err := NewTransport(closedServerURL(t, "tok3n")).Get(context.Background(), "/healthz", nil)
	if err == nil || strings.Contains(err.Error(), "tok3n") || !strings.Contains(err.Error(), `"http://xxxxx@127.0.0.1:`) {
		t.Errorf("error = %v, want the URL without its user name", err)
	}
}

// Errors from building a request show the base URL without user info.
func TestTransportRequestErrors_BaseURL(t *testing.T) {
	const password = "s3cr@tp4ss"
	_, err := NewTransport("http://user:"+password+"@hub.example.com:99999x").Get(context.Background(), "/healthz", nil)
	if err == nil || !strings.Contains(err.Error(), "http://xxxxx@hub.example.com:99999x") {
		t.Fatalf("error = %v, want the URL without its user info", err)
	}
	for i := 0; i+4 <= len(password); i++ {
		if strings.Contains(err.Error(), password[i:i+4]) {
			t.Errorf("error %q shows %q", err.Error(), password[i:i+4])
		}
	}
}
