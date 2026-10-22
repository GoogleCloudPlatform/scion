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
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// No part of the credential of any shape shows in the URL a failed fetch
// logs. Only the url attribute is checked here: it is what redactURL
// writes. The fetch fails before any connection (user info, or a host
// that is not allowed).
func TestRedactURL_Shapes(t *testing.T) {
	for _, s := range urlredacttest.Shapes {
		if got := redactURL(s.URL); urlredacttest.SecretFragment(got, s.Secret) != "" {
			t.Errorf("%s: redactURL(%q) = %q shows part of the secret", s.Name, s.URL, got)
		}
		var logs bytes.Buffer
		f := New(Config{Timeout: 5 * time.Second, AllowedHosts: []string{"img.example.com"}, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
		if _, err := f.FetchBytes(context.Background(), s.URL); err == nil {
			t.Fatalf("%s: fetch of %q succeeded", s.Name, s.URL)
		}
		var rec struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(logs.Bytes(), &rec); err != nil || rec.URL == "" {
			t.Fatalf("%s: no url in the log record %q (%v)", s.Name, logs.String(), err)
		}
		if urlredacttest.SecretFragment(rec.URL, s.Secret) != "" {
			t.Errorf("%s: logged url %q shows part of the secret", s.Name, rec.URL)
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
