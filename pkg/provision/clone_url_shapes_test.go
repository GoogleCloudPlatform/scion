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

package provision

import (
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// gitCloneOutput returns output a failed git clone of rawURL may print:
// the URL as git prints it, and as given.
func gitCloneOutput(rawURL string) []string {
	return []string{
		"Cloning into 'repo'...\nfatal: unable to access '" + urlredacttest.GitEcho(rawURL) + "': The requested URL returned error: 403",
		"fatal: repository '" + rawURL + "' not found",
	}
}

// No part of the credential of any shape shows in a clone error, built
// from the clone URL and git's output.
func TestCloneURLRedactors_Shapes(t *testing.T) {
	for _, s := range urlredacttest.Shapes {
		if got := redactCloneURL(s.URL); urlredacttest.SecretFragment(got, s.Secret) != "" {
			t.Errorf("%s: redactCloneURL(%q) = %q shows part of the secret", s.Name, s.URL, got)
		}
		for _, out := range gitCloneOutput(s.URL) {
			err := cloneError(s.URL, out, errors.New("exit status 128"), false, "")
			if urlredacttest.SecretFragment(err.Error(), s.Secret) != "" {
				t.Errorf("%s: cloneError(%q) = %q shows part of the secret", s.Name, out, err)
			}
		}
	}
}

// An ordinary or token-bearing clone URL keeps its host and path, with
// only the credential (and the query and fragment) removed.
func TestCloneURLRedactors_KeepHostAndPath(t *testing.T) {
	for _, k := range urlredacttest.KeptURLs {
		if got := redactCloneURL(k.URL); got != k.Want {
			t.Errorf("%s: redactCloneURL(%q) = %q, want %q", k.Name, k.URL, got, k.Want)
		}
		out := "fatal: repository '" + k.URL + "' not found"
		if got, want := sanitizeCloneOutput(out, k.URL), "fatal: repository '"+k.Want+"' not found"; got != want {
			t.Errorf("%s: sanitizeCloneOutput(%q) = %q, want %q", k.Name, out, got, want)
		}
		for _, out := range gitCloneOutput(k.URL) {
			msg := cloneError(k.URL, out, errors.New("exit status 128"), true, "").Error()
			if k.Secret != "" && urlredacttest.SecretFragment(msg, k.Secret) != "" {
				t.Errorf("%s: cloneError(%q) = %q shows part of the secret", k.Name, out, msg)
			}
			if !strings.HasPrefix(msg, "git clone "+k.Want+": ") {
				t.Errorf("%s: cloneError(%q) = %q, want it to name %s", k.Name, out, msg, k.Want)
			}
		}
	}
}

// An scp-style clone URL keeps its host and path, and its user name is
// not masked elsewhere in git's output.
func TestCloneURLRedactors_SCP(t *testing.T) {
	const raw = "git@github.com:acme/repo.git"
	if got, want := redactCloneURL(raw), "github.com:acme/repo.git"; got != want {
		t.Errorf("redactCloneURL(%q) = %q, want %q", raw, got, want)
	}
	out := "fatal: '" + raw + "' does not appear to be a git repository"
	if got, want := sanitizeCloneOutput(out, raw), "fatal: 'github.com:acme/repo.git' does not appear to be a git repository"; got != want {
		t.Errorf("sanitizeCloneOutput(%q) = %q, want %q", out, got, want)
	}
}
