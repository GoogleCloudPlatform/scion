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
	"fmt"
	"os"
	"strings"
)

// currentServiceAccountIdentity determines the namespace and ServiceAccount
// name the calling pod itself runs as, so the dialer can request a token
// for that ServiceAccount (deploy/substrate/broker.yaml grants it RBAC to do
// so). It reads namespaceFile directly, and extracts the "sub" claim
// (system:serviceaccount:<namespace>:<name>) from the JWT at tokenFile —
// the pod's own kubelet-projected token — as a cross-check and as the only
// source for the ServiceAccount name (kubelet has no plain-text file for
// that, unlike namespace).
//
// The claim is decoded, not verified: no signature check is performed here.
// That is fine — this value only selects which ServiceAccount to request a
// token FOR, and the TokenRequest API call itself is authorized (or not) by
// the API server based on the credentials the process already holds (the
// same mounted token, sent as the client-go bearer credential). A forged
// claim could at most make the CreateToken call fail with Forbidden; it
// cannot escalate privilege.
func currentServiceAccountIdentity(tokenFile, namespaceFile string) (namespace, name string, err error) {
	nsBytes, err := os.ReadFile(namespaceFile)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", namespaceFile, err)
	}
	namespace = strings.TrimSpace(string(nsBytes))
	if namespace == "" {
		return "", "", fmt.Errorf("%s is empty", namespaceFile)
	}

	tokBytes, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", tokenFile, err)
	}

	sub, err := jwtSubject(strings.TrimSpace(string(tokBytes)))
	if err != nil {
		return "", "", fmt.Errorf("parse %s: %w", tokenFile, err)
	}

	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(sub, prefix) {
		return "", "", fmt.Errorf("token subject %q is not a ServiceAccount identity", sub)
	}
	rest := strings.TrimPrefix(sub, prefix)
	ns, saName, ok := strings.Cut(rest, ":")
	if !ok || ns == "" || saName == "" {
		return "", "", fmt.Errorf("token subject %q could not be split into namespace:name", sub)
	}
	if ns != namespace {
		return "", "", fmt.Errorf("token subject namespace %q does not match %s (%q)", ns, namespaceFile, namespace)
	}
	return namespace, saName, nil
}

// jwtSubject extracts the "sub" claim from a JWT without verifying its
// signature. Not for authorization use — see currentServiceAccountIdentity.
func jwtSubject(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("malformed JWT: expected 3 dot-separated parts, got %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("unmarshal JWT claims: %w", err)
	}
	if claims.Sub == "" {
		return "", fmt.Errorf("JWT has no \"sub\" claim")
	}
	return claims.Sub, nil
}
