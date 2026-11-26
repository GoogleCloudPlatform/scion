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

package source

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strings"
)

// VerifyHMACSHA256Hex validates a prefixed lowercase-hex HMAC-SHA256 signature
// (e.g. "sha256=<hex>") over payload using secret in constant time.
// Shared by pkg/hub/trigger/github, pkg/hub/githubapp, and pkg/hub/trigger so
// GitHub webhook HMAC verification has a single canonical implementation.
func VerifyHMACSHA256Hex(payload []byte, signature, prefix, secret string) bool {
	if secret == "" || signature == "" {
		return false
	}
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	sigHex := signature[len(prefix):]

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expectedMAC := mac.Sum(nil)
	expectedHex := fmt.Sprintf("%x", expectedMAC)

	return hmac.Equal([]byte(sigHex), []byte(expectedHex))
}
