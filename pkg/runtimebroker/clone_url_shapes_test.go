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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// cloneErrorTexts returns the error texts a failed clone of rawURL may
// produce: provisioning's own "git clone <url>" error, with git's output
// echoing the URL as git prints it.
func cloneErrorTexts(rawURL string) []string {
	return []string{
		"git clone " + rawURL + ": exit status 128",
		"git clone <redacted>: fatal: unable to access '" + urlredacttest.GitEcho(rawURL) + "': The requested URL returned error: 403",
	}
}

// No part of the credential of any shape shows in the logged clone URL or
// in a sanitized clone error.
func TestCloneURLRedactors_Shapes(t *testing.T) {
	for _, s := range urlredacttest.Shapes {
		if got := redactCloneURL(s.URL); urlredacttest.SecretFragment(got, s.Secret) != "" {
			t.Errorf("%s: redactCloneURL(%q) = %q shows part of the secret", s.Name, s.URL, got)
		}
		for _, text := range cloneErrorTexts(s.URL) {
			if got := sanitizeCloneErrorText(text, s.URL); urlredacttest.SecretFragment(got, s.Secret) != "" {
				t.Errorf("%s: sanitizeCloneErrorText(%q) = %q shows part of the secret", s.Name, text, got)
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
		text := "git clone " + k.URL + ": exit status 128"
		if got, want := sanitizeCloneErrorText(text, k.URL), "git clone "+k.Want+": exit status 128"; got != want {
			t.Errorf("%s: sanitizeCloneErrorText(%q) = %q, want %q", k.Name, text, got, want)
		}
		for _, text := range cloneErrorTexts(k.URL) {
			got := sanitizeCloneErrorText(text, k.URL)
			if k.Secret != "" && urlredacttest.SecretFragment(got, k.Secret) != "" {
				t.Errorf("%s: sanitizeCloneErrorText(%q) = %q shows part of the secret", k.Name, text, got)
			}
		}
	}
}
