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

package remotefetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// jsonLoggerWithoutTime returns a JSON logger to w that leaves out each
// record's time: the top-level "time" attribute when it holds a time (an
// attribute of another kind under that key is still written). The time is
// the one value in a record the code under test does not write, and its
// digits (nanoseconds such as "...944958461Z") can hold a 4-byte part of a
// masked value ("8461") by chance; every other value of the record
// (message, level and attributes) is still written and checked.
func jsonLoggerWithoutTime(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
				return slog.Attr{}
			}
			return a
		},
	}))
}

// No part of the credential of any shape shows in what a failed fetch logs
// (every record and attribute of the JSON log, the url attribute that
// redactURL writes checked on its own as well) or returns (err.Error()).
// The fetch fails before any connection: on user info, the port, a host
// that is not allowed, or (without AllowedHosts) a lookup that fails or
// answers a denied address.
func TestRedactURL_Shapes(t *testing.T) {
	for _, s := range urlredacttest.Shapes {
		if got := redactURL(s.URL); urlredacttest.SecretFragment(got, s.Secret) != "" {
			t.Errorf("%s: redactURL(%q) = %q shows part of the secret", s.Name, s.URL, got)
		}
		for _, cfg := range []struct {
			name    string
			allowed []string
			answers map[string][]netip.Addr
		}{
			{"allowed hosts", []string{"img.example.com"}, nil},
			{"lookup fails", nil, nil},
			{"denied address", nil, map[string][]netip.Addr{"tKn7Qz": addrs("127.0.0.1"), "git.example.com": addrs("127.0.0.1")}},
		} {
			var logs bytes.Buffer
			f := New(Config{Timeout: 5 * time.Second, AllowedHosts: cfg.allowed, Logger: jsonLoggerWithoutTime(&logs)})
			f.resolver = &fakeResolver{answers: cfg.answers}
			_, err := f.FetchBytes(context.Background(), s.URL)
			if err == nil {
				t.Fatalf("%s, %s: fetch of %q succeeded", s.Name, cfg.name, s.URL)
			}
			var rec struct {
				URL string `json:"url"`
			}
			last := logs.Bytes()
			if i := bytes.LastIndexByte(bytes.TrimRight(last, "\n"), '\n'); i >= 0 {
				last = last[i+1:]
			}
			if err := json.Unmarshal(last, &rec); err != nil || rec.URL == "" {
				t.Fatalf("%s, %s: no url in the log record %q (%v)", s.Name, cfg.name, logs.String(), err)
			}
			if urlredacttest.SecretFragment(rec.URL, s.Secret) != "" {
				t.Errorf("%s: logged url %q shows part of the secret", s.Name, rec.URL)
			}
			for what, out := range map[string]string{"error": err.Error(), "logs": logs.String()} {
				if frag := urlredacttest.SecretFragment(out, s.Secret); frag != "" {
					t.Errorf("%s, %s: %s %q shows %q", s.Name, cfg.name, what, out, frag)
				}
			}
		}
	}
}

// An ordinary or token-bearing URL keeps its host and path.
func TestRedactURL_KeepHostAndPath(t *testing.T) {
	for _, k := range urlredacttest.KeptURLs {
		if got := redactURL(k.URL); got != k.Want {
			t.Errorf("%s: redactURL(%q) = %q, want %q", k.Name, k.URL, got, k.Want)
		}
	}
}

// A failed fetch of a URL whose host or port may be part of a credential (a
// token read as a host, a password read as a port) logs and returns no part
// of it: the Detail is hiddenDetail, the Reason is kept, and the denied
// address warning shows hiddenHost. An ordinary URL keeps its detail.
func TestFetchDetail_HostNotShown(t *testing.T) {
	const tokenHost = "https://tKn7Qz/x@img.example.com/a.png"
	for _, tc := range []struct {
		name, url, secret string
		allowed           []string
		answers           map[string][]netip.Addr
		reason            Reason
	}{
		{"host not allowed", tokenHost, "tKn7Qz/x", []string{"img.example.com"}, nil, ReasonHostNotAllowed},
		{"resolve fails", tokenHost, "tKn7Qz/x", nil, nil, ReasonResolve},
		{"no addresses", tokenHost, "tKn7Qz/x", nil, map[string][]netip.Addr{"tKn7Qz": {}}, ReasonResolve},
		{"denied address", tokenHost, "tKn7Qz/x", nil, map[string][]netip.Addr{"tKn7Qz": addrs("127.0.0.1")}, ReasonDeniedAddress},
		{"password read as a port", "https://u:8461/Wx2@img.example.com/a.png", "8461/Wx2", nil, nil, ReasonPort},
	} {
		var logs bytes.Buffer
		f := New(Config{Timeout: 5 * time.Second, AllowedHosts: tc.allowed, Logger: jsonLoggerWithoutTime(&logs)})
		f.resolver = &fakeResolver{answers: tc.answers}
		_, err := f.FetchBytes(context.Background(), tc.url)
		var fe *Error
		if !errors.As(err, &fe) {
			t.Fatalf("%s: error %v is not an *Error", tc.name, err)
		}
		if fe.Reason != tc.reason || fe.Detail != hiddenDetail {
			t.Errorf("%s: error = %q, %q; want %q, %q", tc.name, fe.Reason, fe.Detail, tc.reason, hiddenDetail)
		}
		for what, out := range map[string]string{"error": err.Error(), "logs": logs.String()} {
			if frag := urlredacttest.SecretFragment(out, tc.secret); frag != "" {
				t.Errorf("%s: %s %q shows %q", tc.name, what, out, frag)
			}
		}
		if tc.reason == ReasonDeniedAddress && !strings.Contains(logs.String(), `"host":"`+hiddenHost+`"`) {
			t.Errorf("%s: the denied address warning does not show %q: %s", tc.name, hiddenHost, logs.String())
		}
	}

	// An ordinary URL keeps its detail.
	f := New(Config{Timeout: 5 * time.Second, AllowedHosts: []string{"img.example.com"}, Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))})
	_, err := f.FetchBytes(context.Background(), "https://cdn.example.com/a.png")
	if want := `remote fetch failed: host_not_allowed: host "cdn.example.com" is not allowed`; err == nil || err.Error() != want {
		t.Errorf("ordinary URL: error %v, want %q", err, want)
	}
}
