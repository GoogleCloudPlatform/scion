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

package hub

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactAndBoundCommandOutput_Patterns(t *testing.T) {
	pemKey := "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEAtestkeydata12345\n-----END RSA PRIVATE KEY-----"
	ghp := "ghp_1234567890abcdef1234567890abcdef1234"
	gho := "gho_abcdef1234567890abcdef1234567890abcd"
	ghs := "ghs_9876543210fedcba9876543210fedcba9876"
	ghPat := "github_pat_11AAAAAA0123456789_abcdefghijklmnop"
	awsAKIA := "AKIAIOSFODNN7EXAMPLE"
	awsASIA := "ASIAIOSFODNN7EXAMPLE"
	googleTok := "ya29.a0AfH6SMB_example_google_oauth_token_12345"
	slackBot := "xoxb-1234567890-abcdefghij-secret"
	slackUser := "xoxp-1234567890-abcdefghij-secret"
	jwtTok := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	bearerTok := "super-secret-bearer-token-9999"

	input := strings.Join([]string{
		"ok line",
		pemKey,
		"github: " + ghp + " " + gho + " " + ghs + " " + ghPat,
		"aws: " + awsAKIA + " " + awsASIA,
		"gcp: " + googleTok,
		"slack: " + slackBot + " " + slackUser,
		"jwt: " + jwtTok,
		"Authorization: Bearer " + bearerTok,
		"DB_PASSWORD=my-db-pass",
		`api_secret: "quoted-secret-val"`,
		`{"access_token": "json-tok-val"}`,
	}, "\n")

	tail, count := redactAndBoundCommandOutput([]byte(input))
	assert.GreaterOrEqual(t, count, 14)
	assert.Contains(t, tail, "ok line")
	assert.Contains(t, tail, "[REDACTED]")
	assert.Contains(t, tail, "Authorization: Bearer [REDACTED]")
	assert.Contains(t, tail, "DB_PASSWORD=[REDACTED]")
	assert.Contains(t, tail, "api_secret: [REDACTED]")
	assert.Contains(t, tail, `"access_token": [REDACTED]`)

	for _, secret := range []string{
		"MIIEpAIBAAKCAQEAtestkeydata12345",
		ghp, gho, ghs, ghPat,
		awsAKIA, awsASIA,
		googleTok,
		slackBot, slackUser,
		jwtTok,
		bearerTok,
		"my-db-pass",
		"quoted-secret-val",
		"json-tok-val",
	} {
		assert.NotContains(t, tail, secret, "secret %q must be redacted", secret)
	}
}

func TestRedactAndBoundCommandOutput_TruncatedPEMAndNoDoubleCount(t *testing.T) {
	input := "GITHUB_TOKEN=ghp_1234567890abcdef1234567890abcdef1234\n" +
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig123456\n" +
		"-----BEGIN OPENSSH PRIVATE KEY-----\ntruncated-key-body-without-end"

	tail, count := redactAndBoundCommandOutput([]byte(input))
	assert.Equal(t, 3, count, "each secret should be counted once")
	assert.NotContains(t, tail, "ghp_")
	assert.NotContains(t, tail, "eyJ")
	assert.NotContains(t, tail, "truncated-key-body-without-end")
	assert.Contains(t, tail, "GITHUB_TOKEN=[REDACTED]")
	assert.Contains(t, tail, "Authorization: Bearer [REDACTED]")
}

func TestRedactAndBoundCommandOutput_UTF8BoundaryAndBoundarySecret(t *testing.T) {
	// Multi-byte UTF-8 rune ("é" = 2 bytes, "€" = 3 bytes, "🚀" = 4 bytes).
	// Place a secret straddling the 4096-byte boundary from the end so
	// redaction must happen BEFORE slicing.
	prefix := strings.Repeat("🚀", 1200) // 4800 bytes
	secret := "ghp_boundarysecret1234567890abcdef1234"
	suffix := strings.Repeat("€", 1360) // 4080 bytes -> secret sits just before/at the 4 KiB boundary
	raw := []byte(prefix + secret + suffix)

	tail, count := redactAndBoundCommandOutput(raw)
	require.Equal(t, 1, count)
	assert.LessOrEqual(t, len(tail), commandOutputShareMaxBytes)
	assert.True(t, utf8.ValidString(tail), "tail must be valid UTF-8")
	assert.NotContains(t, tail, "boundarysecret")
	assert.NotContains(t, tail, "ghp_")
	assert.True(t, strings.HasSuffix(tail, "€€€"), "tail should preserve the end of output")
}
