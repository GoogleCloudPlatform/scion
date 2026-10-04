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

package substrate

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeJWT builds a syntactically valid, unsigned JWT string carrying the
// given "sub" claim, for tests that only exercise claim extraction.
func fakeJWT(t *testing.T, sub string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]string{"sub": sub})
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claims)
	return header + "." + payload + ".sig"
}

func TestJWTSubject(t *testing.T) {
	tok := fakeJWT(t, "system:serviceaccount:ate-broker:scion-broker")
	sub, err := jwtSubject(tok)
	if err != nil {
		t.Fatalf("jwtSubject() error = %v", err)
	}
	if sub != "system:serviceaccount:ate-broker:scion-broker" {
		t.Errorf("jwtSubject() = %q, want the sub claim", sub)
	}
}

func TestJWTSubject_MalformedToken(t *testing.T) {
	cases := []string{"", "not-a-jwt", "a.b", "a.b.c.d"}
	for _, tc := range cases {
		if _, err := jwtSubject(tc); err == nil {
			t.Errorf("jwtSubject(%q) expected error, got nil", tc)
		}
	}
}

func TestJWTSubject_NoSubClaim(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"aud":["x"]}`))
	tok := header + "." + payload + ".sig"
	if _, err := jwtSubject(tok); err == nil {
		t.Error("jwtSubject() expected error for missing sub claim, got nil")
	}
}

func writeIdentityFiles(t *testing.T, namespace, sub string) (tokenFile, namespaceFile string) {
	t.Helper()
	dir := t.TempDir()
	tokenFile = filepath.Join(dir, "token")
	namespaceFile = filepath.Join(dir, "namespace")
	if err := os.WriteFile(tokenFile, []byte(fakeJWT(t, sub)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(namespaceFile, []byte(namespace), 0644); err != nil {
		t.Fatal(err)
	}
	return tokenFile, namespaceFile
}

func TestCurrentServiceAccountIdentity(t *testing.T) {
	tokenFile, namespaceFile := writeIdentityFiles(t, "ate-broker", "system:serviceaccount:ate-broker:scion-broker")

	ns, name, err := currentServiceAccountIdentity(tokenFile, namespaceFile)
	if err != nil {
		t.Fatalf("currentServiceAccountIdentity() error = %v", err)
	}
	if ns != "ate-broker" || name != "scion-broker" {
		t.Errorf("currentServiceAccountIdentity() = (%q, %q), want (ate-broker, scion-broker)", ns, name)
	}
}

func TestCurrentServiceAccountIdentity_NamespaceMismatch(t *testing.T) {
	// The token claims a different namespace than the mounted namespace
	// file — this should be treated as an error rather than silently
	// trusting one source over the other.
	tokenFile, namespaceFile := writeIdentityFiles(t, "ate-broker", "system:serviceaccount:other-ns:scion-broker")

	if _, _, err := currentServiceAccountIdentity(tokenFile, namespaceFile); err == nil {
		t.Error("currentServiceAccountIdentity() expected error on namespace mismatch, got nil")
	}
}

func TestCurrentServiceAccountIdentity_NotAServiceAccountToken(t *testing.T) {
	tokenFile, namespaceFile := writeIdentityFiles(t, "ate-broker", "system:node:some-node")

	if _, _, err := currentServiceAccountIdentity(tokenFile, namespaceFile); err == nil {
		t.Error("currentServiceAccountIdentity() expected error for non-ServiceAccount subject, got nil")
	}
}

func TestCurrentServiceAccountIdentity_MissingFiles(t *testing.T) {
	dir := t.TempDir()
	_, _, err := currentServiceAccountIdentity(filepath.Join(dir, "missing-token"), filepath.Join(dir, "missing-ns"))
	if err == nil {
		t.Error("currentServiceAccountIdentity() expected error for missing files, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should name the missing path, got: %v", err)
	}
}
