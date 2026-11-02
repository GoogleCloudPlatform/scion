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

package keyring

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Synthetic fixtures only. None of these is or was a real credential.
const (
	fixtureLegacySecret = "test-only-legacy-secret"
	fixtureGen1Secret   = "test-only-signing-generation-1-0123456789abcdef"
	fixtureGen2Secret   = "test-only-signing-generation-2-0123456789abcdef"
	fixtureOtherEncKey  = "test-only-encryption-key-32bytes" // exactly 32 bytes
	// Misplaced-secret fixtures: secret-shaped strings an operator could
	// leave in key position by dropping a field name.
	fixtureMixedCaseSecret = "Test-Only+Base64/Secret==AAAA0123456789abcdEFGH"
	fixtureLowerSecret     = "test_only_lowercase_secret_0123456789abcdef"
)

// Golden legacy derivations of fixtureLegacySecret, computed independently
// (sha256 over label + secret). They pin the byte-compatibility of the
// legacy adapter with the pre-keyring derivations in pkg/hub (web session
// cookie keys) and pkg/secret (local encryption key).
const (
	goldenSessionHash = "5c7e8467b6a3919f7eb1a581c323d8e0521b140fe72af4820b125e85ad38c904"
	goldenSessionBlk  = "9532664051afd03b3e077460d7cba0b73d4d181e6effe3299429131475946e90"
	goldenEncryption  = "216b3dfda93c482541529e7bd3362249aac73f3b32c745298ed1e08ae45482aa"
)

// sha256LabelPurpose is the web session cookie key derivation,
// sha256(label + ":" + secret), recomputed here independently of pkg/hub.
func sha256LabelPurpose(label string) Purpose {
	return NewPurpose(label, func(secret []byte) []byte {
		sum := sha256.Sum256(append([]byte(label+":"), secret...))
		return sum[:]
	})
}

var (
	sessionHash  = sha256LabelPurpose("scion-session-hash")
	sessionBlock = sha256LabelPurpose("scion-session-block")
	t0           = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
)

func legacyEncB64() string {
	return base64.StdEncoding.EncodeToString(DeriveLegacyEncryptionKey(fixtureLegacySecret))
}

// validDoc is a pilot-valid three-generation file: legacy previous (with
// bounds), gen1 active, gen2 staged.
func validDoc() map[string]any {
	return map[string]any{
		"schema":   1,
		"revision": "rev-2026-10-21.1",
		"signing": map[string]any{
			"active": "gen1",
			"emit":   "legacy",
			"keys": []any{
				map[string]any{"id": "legacy", "secret": fixtureLegacySecret,
					"sign_until": "2026-10-21T13:00:00Z", "verify_until": "2026-10-22T13:00:00Z"},
				map[string]any{"id": "gen1", "secret": fixtureGen1Secret, "sign_until": nil, "verify_until": nil},
				map[string]any{"id": "gen2", "secret": fixtureGen2Secret, "sign_until": nil, "verify_until": nil},
			},
			"legacy_read_ids": []any{"legacy"},
		},
		"encryption": map[string]any{
			"active":       "legacy",
			"write_format": "v1",
			"keys": []any{
				map[string]any{"id": "legacy", "key_b64": legacyEncB64()},
			},
			"legacy_v1_read_ids": []any{"legacy"},
		},
	}
}

func marshal(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sub(doc map[string]any, keys ...string) map[string]any {
	m := doc
	for _, k := range keys {
		m = m[k].(map[string]any)
	}
	return m
}

func signingKeyAt(doc map[string]any, i int) map[string]any {
	return sub(doc, "signing")["keys"].([]any)[i].(map[string]any)
}

// assertNoSecret fails if s contains any fixture key material.
func assertNoSecret(t *testing.T, s string) {
	t.Helper()
	for _, secret := range []string{fixtureLegacySecret, fixtureGen1Secret, fixtureGen2Secret, fixtureOtherEncKey, legacyEncB64(),
		fixtureMixedCaseSecret, fixtureLowerSecret,
		base64.StdEncoding.EncodeToString([]byte(fixtureOtherEncKey))} {
		if strings.Contains(s, secret) {
			t.Fatalf("output contains key material: %q", s)
		}
	}
	for _, golden := range []string{goldenSessionHash, goldenSessionBlk, goldenEncryption} {
		if strings.Contains(s, golden) {
			t.Fatalf("output contains derived key material: %q", s)
		}
	}
}

func TestParseValid(t *testing.T) {
	k, err := Parse(marshal(t, validDoc()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if k.Source() != SourceFile || k.Revision() != "rev-2026-10-21.1" {
		t.Fatalf("source/revision = %s/%q", k.Source(), k.Revision())
	}
	if got := k.Signing().IDs(); fmt.Sprint(got) != "[gen1 legacy gen2]" {
		t.Fatalf("signing IDs = %v, want active first then file order", got)
	}
	if k.Signing().Emit() != EmitLegacy || k.Encryption().WriteFormat() != WriteV1 {
		t.Fatal("emit/write format not parsed")
	}
	if err := k.CheckSessionPilot(); err != nil {
		t.Fatalf("CheckSessionPilot: %v", err)
	}
	if s, ok := k.Signing().LegacySecret(); !ok || s != fixtureLegacySecret {
		t.Fatal("LegacySecret did not return the exact legacy entry bytes")
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.json")
	if err := os.WriteFile(path, marshal(t, validDoc()), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, link} {
		if _, err := LoadFile(p); err != nil {
			t.Fatalf("LoadFile(%s): %v", filepath.Base(p), err)
		}
	}
	if _, err := LoadFile(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("LoadFile(dir) = %v, want not a regular file", err)
	}
	if _, err := LoadFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("LoadFile(missing) succeeded")
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, []byte(strings.Repeat(" ", MaxFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("LoadFile(big) = %v, want size refusal", err)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(d map[string]any)
		raw    string // used instead of mutate when set
		want   string
	}{
		{name: "duplicate signing id", mutate: func(d map[string]any) {
			signingKeyAt(d, 2)["id"] = "gen1"
		}, want: "duplicate signing key id: signing.keys[2] repeats signing.keys[1]"},
		{name: "duplicate encryption id", mutate: func(d map[string]any) {
			e := sub(d, "encryption")
			e["keys"] = append(e["keys"].([]any), map[string]any{"id": "legacy", "key_b64": legacyEncB64()})
		}, want: "duplicate encryption key id: encryption.keys[1] repeats encryption.keys[0]"},
		{name: "duplicate JSON key", raw: `{"schema":1,"schema":1}`, want: "duplicate JSON object key near byte"},
		{name: "upper-case key Secret", mutate: func(d map[string]any) {
			k := signingKeyAt(d, 1)
			k["Secret"] = k["secret"]
			delete(k, "secret")
		}, want: "invalid JSON object key near byte"},
		{name: "upper-case key SCHEMA", mutate: func(d map[string]any) { d["SCHEMA"] = d["schema"]; delete(d, "schema") }, want: "invalid JSON object key near byte"},
		{name: "unicode-folded key", mutate: func(d map[string]any) {
			k := signingKeyAt(d, 1)
			k["\u017fecret"] = k["secret"] // U+017F LATIN SMALL LETTER LONG S folds to s
			delete(k, "secret")
		}, want: "invalid JSON object key near byte"},
		{name: "secret plus Secret", mutate: func(d map[string]any) { signingKeyAt(d, 1)["Secret"] = fixtureGen2Secret }, want: "invalid JSON object key near byte"},
		{name: "duplicate nested JSON key", raw: `{"signing":{"active":"a","active":"b"}}`, want: "duplicate JSON object key near byte"},
		{name: "unknown top-level field", mutate: func(d map[string]any) { d["extra"] = true }, want: "unknown field near byte"},
		{name: "unknown ring field", mutate: func(d map[string]any) { sub(d, "signing")["extra"] = 1 }, want: "unknown field near byte"},
		{name: "unknown entry field", mutate: func(d map[string]any) { signingKeyAt(d, 0)["role"] = "previous" }, want: "unknown field near byte"},
		{name: "schema 2", mutate: func(d map[string]any) { d["schema"] = 2 }, want: "unsupported schema 2"},
		{name: "schema string", mutate: func(d map[string]any) { d["schema"] = "1" }, want: "wrong type"},
		{name: "schema missing", mutate: func(d map[string]any) { delete(d, "schema") }, want: "missing required field schema"},
		{name: "revision missing", mutate: func(d map[string]any) { delete(d, "revision") }, want: "missing required field revision"},
		{name: "revision empty", mutate: func(d map[string]any) { d["revision"] = "" }, want: "invalid revision"},
		{name: "revision newline", mutate: func(d map[string]any) { d["revision"] = "a\nb" }, want: "invalid revision"},
		{name: "signing missing", mutate: func(d map[string]any) { delete(d, "signing") }, want: "missing required field signing"},
		{name: "encryption missing", mutate: func(d map[string]any) { delete(d, "encryption") }, want: "missing required field encryption"},
		{name: "four signing entries", mutate: func(d map[string]any) {
			s := sub(d, "signing")
			s["keys"] = append(s["keys"].([]any), map[string]any{"id": "gen3", "secret": fixtureGen2Secret, "sign_until": nil, "verify_until": nil})
		}, want: "must hold 1 to 3 entries"},
		{name: "no signing entries", mutate: func(d map[string]any) { sub(d, "signing")["keys"] = []any{} }, want: "must hold 1 to 3 entries"},
		{name: "active unknown", mutate: func(d map[string]any) { sub(d, "signing")["active"] = "nope" }, want: "invalid signing.active"},
		{name: "active missing", mutate: func(d map[string]any) { delete(sub(d, "signing"), "active") }, want: "missing required field signing.active"},
		{name: "encryption active unknown", mutate: func(d map[string]any) { sub(d, "encryption")["active"] = "nope" }, want: "invalid encryption.active"},
		{name: "bad id chars", mutate: func(d map[string]any) { signingKeyAt(d, 2)["id"] = "gen.2" }, want: "invalid signing.keys[2].id"},
		{name: "id too long", mutate: func(d map[string]any) { signingKeyAt(d, 2)["id"] = strings.Repeat("a", 33) }, want: "invalid signing.keys[2].id"},
		{name: "emit unknown", mutate: func(d map[string]any) { sub(d, "signing")["emit"] = "both" }, want: "invalid signing.emit"},
		{name: "write format unknown", mutate: func(d map[string]any) { sub(d, "encryption")["write_format"] = "v3" }, want: "invalid encryption.write_format"},
		{name: "bad RFC3339", mutate: func(d map[string]any) { signingKeyAt(d, 0)["sign_until"] = "2026-10-21 13:00" }, want: "signing.keys[0].sign_until: must be an RFC 3339"},
		{name: "deadline number", mutate: func(d map[string]any) { signingKeyAt(d, 0)["verify_until"] = 5 }, want: `verify_until: must be an RFC 3339`},
		{name: "deadline absent", mutate: func(d map[string]any) { delete(signingKeyAt(d, 1), "sign_until") }, want: "missing required field signing.keys[1].sign_until"},
		{name: "sign_until == verify_until", mutate: func(d map[string]any) {
			signingKeyAt(d, 0)["verify_until"] = "2026-10-21T13:00:00Z"
		}, want: "sign_until must be before verify_until"},
		{name: "sign_until after verify_until", mutate: func(d map[string]any) {
			signingKeyAt(d, 0)["sign_until"] = "2026-10-23T13:00:00Z"
		}, want: "sign_until must be before verify_until"},
		{name: "half deadline", mutate: func(d map[string]any) { signingKeyAt(d, 0)["verify_until"] = nil }, want: "both be set or both be null"},
		{name: "short new secret", mutate: func(d map[string]any) { signingKeyAt(d, 1)["secret"] = "too-short" }, want: "signing.keys[1].secret: must be at least 32 bytes"},
		{name: "empty legacy secret", mutate: func(d map[string]any) { signingKeyAt(d, 0)["secret"] = "" }, want: "signing.keys[0].secret: must not be empty"},
		{name: "secret missing", mutate: func(d map[string]any) { delete(signingKeyAt(d, 1), "secret") }, want: "missing required field signing.keys[1].secret"},
		{name: "key_b64 unpadded", mutate: func(d map[string]any) {
			sub(d, "encryption")["keys"].([]any)[0].(map[string]any)["key_b64"] = strings.TrimRight(legacyEncB64(), "=")
		}, want: "must be standard padded base64"},
		{name: "key_b64 url alphabet", mutate: func(d map[string]any) {
			sub(d, "encryption")["keys"].([]any)[0].(map[string]any)["key_b64"] = base64.URLEncoding.EncodeToString([]byte{0xfb, 0xff, 0xfe, 0xfb, 0xff, 0xfe, 0xfb, 0xff, 0xfe, 0xfb, 0xff, 0xfe, 0xfb, 0xff, 0xfe, 0xfb, 0xff, 0xfe, 0xfb, 0xff, 0xfe, 0xfb, 0xff, 0xfe, 0xfb, 0xff, 0xfe, 0xfb, 0xff, 0xfe, 0xfb, 0xff})
		}, want: "must be standard padded base64"},
		{name: "key 31 bytes", mutate: func(d map[string]any) {
			sub(d, "encryption")["keys"].([]any)[0].(map[string]any)["key_b64"] = base64.StdEncoding.EncodeToString([]byte(fixtureOtherEncKey[:31]))
		}, want: "must decode to exactly 32 bytes"},
		{name: "key 33 bytes", mutate: func(d map[string]any) {
			sub(d, "encryption")["keys"].([]any)[0].(map[string]any)["key_b64"] = base64.StdEncoding.EncodeToString([]byte(fixtureOtherEncKey + "x"))
		}, want: "must decode to exactly 32 bytes"},
		{name: "legacy_read_ids unknown", mutate: func(d map[string]any) { sub(d, "signing")["legacy_read_ids"] = []any{"zzz"} }, want: "signing.legacy_read_ids[0]: names no key in the ring"},
		{name: "legacy_read_ids repeat", mutate: func(d map[string]any) { sub(d, "signing")["legacy_read_ids"] = []any{"legacy", "legacy"} }, want: "signing.legacy_read_ids[1]: repeats an earlier id"},
		{name: "legacy_read_ids too many", mutate: func(d map[string]any) {
			sub(d, "signing")["legacy_read_ids"] = []any{"legacy", "gen1", "gen2", "gen1"}
		}, want: "at most 3 ids"},
		{name: "legacy_read_ids missing", mutate: func(d map[string]any) { delete(sub(d, "signing"), "legacy_read_ids") }, want: "missing required field signing.legacy_read_ids"},
		{name: "legacy_v1_read_ids missing", mutate: func(d map[string]any) { delete(sub(d, "encryption"), "legacy_v1_read_ids") }, want: "missing required field encryption.legacy_v1_read_ids"},
		{name: "secret in key position (mixed case, base64)", raw: `{"schema":1,"revision":"r","signing":{"keys":[{"id":"gen2","` + fixtureMixedCaseSecret + `"}]}}`, want: "invalid JSON object key near byte"},
		{name: "secret as key with a value", raw: `{"schema":1,"` + fixtureMixedCaseSecret + `":1}`, want: "invalid JSON object key near byte"},
		{name: "lower-case secret in key position", raw: `{"schema":1,"signing":{"keys":[{"id":"gen2","` + fixtureLowerSecret + `"}]}}`, want: "malformed JSON"},
		{name: "lower-case secret as unknown field", raw: `{"schema":1,"` + fixtureLowerSecret + `":1}`, want: "unknown field near byte"},
		{name: "lower-case secret as duplicate key", raw: `{"` + fixtureLowerSecret + `":1,"` + fixtureLowerSecret + `":2}`, want: "duplicate JSON object key near byte"},
		{name: "secret in legacy_read_ids", mutate: func(d map[string]any) { sub(d, "signing")["legacy_read_ids"] = []any{fixtureMixedCaseSecret} }, want: "signing.legacy_read_ids[0]: names no key in the ring"},
		{name: "secret in id", mutate: func(d map[string]any) { signingKeyAt(d, 2)["id"] = fixtureMixedCaseSecret }, want: "invalid signing.keys[2].id"},
		{name: "secret in active", mutate: func(d map[string]any) { sub(d, "signing")["active"] = fixtureMixedCaseSecret }, want: "invalid signing.active"},
		{name: "secret in emit", mutate: func(d map[string]any) { sub(d, "signing")["emit"] = fixtureMixedCaseSecret }, want: "invalid signing.emit"},
		{name: "secret as a deadline", mutate: func(d map[string]any) { signingKeyAt(d, 0)["sign_until"] = fixtureMixedCaseSecret }, want: "signing.keys[0].sign_until: must be an RFC 3339"},
		{name: "secret as wrong type", mutate: func(d map[string]any) { signingKeyAt(d, 1)["secret"] = []any{fixtureMixedCaseSecret} }, want: "wrong type"},
		{name: "trailing data", raw: `{"schema":1} {"schema":1}`, want: "trailing data"},
		{name: "empty", raw: ``, want: "empty or truncated"},
		{name: "truncated", raw: `{"schema":1,`, want: "keyring file"},
		{name: "syntax error hides input", raw: `{"schema":1,"revision":` + fixtureGen1Secret + `}`, want: "malformed JSON"},
		{name: "top-level array", raw: `[]`, want: "wrong type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var data []byte
			if tc.raw != "" || tc.mutate == nil {
				data = []byte(tc.raw)
			} else {
				d := validDoc()
				tc.mutate(d)
				data = marshal(t, d)
			}
			k, err := Parse(data)
			if err == nil {
				t.Fatalf("Parse succeeded: %v", k)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
			assertNoSecret(t, err.Error())
		})
	}
}

func TestParseAcceptsShortImportedLegacySecret(t *testing.T) {
	d := validDoc()
	// The fixture legacy secret is shorter than MinSigningSecretBytes:
	// imported legacy strings keep their exact bytes.
	if len(fixtureLegacySecret) >= MinSigningSecretBytes {
		t.Fatal("fixture must be short to exercise the exemption")
	}
	if _, err := Parse(marshal(t, d)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestCheckSessionPilot(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(d map[string]any)
		want   string
	}{
		{name: "emit versioned", mutate: func(d map[string]any) { sub(d, "signing")["emit"] = "versioned" }, want: `"versioned" is not supported`},
		{name: "write v2", mutate: func(d map[string]any) { sub(d, "encryption")["write_format"] = "v2" }, want: `"v2" is not supported`},
		{name: "no legacy entry", mutate: func(d map[string]any) {
			s := sub(d, "signing")
			s["keys"] = s["keys"].([]any)[1:]
			s["legacy_read_ids"] = []any{}
		}, want: `requires a signing key with id "legacy"`},
		{name: "replaced encryption key", mutate: func(d map[string]any) {
			e := sub(d, "encryption")
			e["keys"] = []any{
				map[string]any{"id": "legacy", "key_b64": legacyEncB64()},
				map[string]any{"id": "enc2", "key_b64": base64.StdEncoding.EncodeToString([]byte(fixtureOtherEncKey))},
			}
			e["active"] = "enc2"
		}, want: "active encryption key to be the imported legacy key"},
		{name: "legacy encryption bytes differ", mutate: func(d map[string]any) {
			sub(d, "encryption")["keys"].([]any)[0].(map[string]any)["key_b64"] = base64.StdEncoding.EncodeToString([]byte(fixtureOtherEncKey))
		}, want: "active encryption key to be the imported legacy key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDoc()
			tc.mutate(d)
			k, err := Parse(marshal(t, d))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = k.CheckSessionPilot()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckSessionPilot = %v, want %q", err, tc.want)
			}
			assertNoSecret(t, err.Error())
		})
	}
	if err := Legacy(fixtureLegacySecret).CheckSessionPilot(); err != nil {
		t.Fatalf("legacy adapter: %v", err)
	}
}

func TestLegacyGolden(t *testing.T) {
	k := Legacy(fixtureLegacySecret)
	if k.Source() != SourceLegacy || k.Revision() != "" {
		t.Fatal("legacy source/revision")
	}
	s := k.Signing()
	if s.ActiveID() != LegacyID || s.Emit() != EmitLegacy {
		t.Fatal("legacy signing ring shape")
	}
	h, err := s.Active(sessionHash, t0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Active(sessionBlock, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h.Bytes()); got != goldenSessionHash {
		t.Fatalf("session hash key = %s, want golden", got)
	}
	if got := hex.EncodeToString(b.Bytes()); got != goldenSessionBlk {
		t.Fatalf("session block key = %s, want golden", got)
	}
	e := k.Encryption()
	if e.ActiveID() != LegacyID || e.WriteFormat() != WriteV1 {
		t.Fatal("legacy encryption ring shape")
	}
	if got := hex.EncodeToString(e.Active().Bytes()); got != goldenEncryption {
		t.Fatalf("encryption key = %s, want golden", got)
	}
	if c := e.Candidates(""); len(c) != 1 || c[0].ID != LegacyID {
		t.Fatalf("legacy v1 candidates = %v", c)
	}
	if c := s.Candidates(sessionHash, "", t0.AddDate(10, 0, 0)); len(c) != 1 || c[0].ID != LegacyID {
		t.Fatalf("legacy signing candidates = %v (no deadline: always eligible)", c)
	}
	if sec, ok := s.LegacySecret(); !ok || sec != fixtureLegacySecret {
		t.Fatal("legacy secret bytes changed")
	}
}

func TestSigningRingDeadlines(t *testing.T) {
	d := validDoc()
	// Make gen1 the outgoing active entry with finite bounds.
	signingKeyAt(d, 1)["sign_until"] = "2026-10-21T14:00:00Z"
	signingKeyAt(d, 1)["verify_until"] = "2026-10-21T16:00:00Z"
	sub(d, "signing")["legacy_read_ids"] = []any{"legacy", "gen1"}
	k, err := Parse(marshal(t, d))
	if err != nil {
		t.Fatal(err)
	}
	s := k.Signing()
	signEnd := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	verifyEnd := time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC)

	if a, err := s.Active(sessionHash, signEnd.Add(-time.Nanosecond)); err != nil || a.ID != "gen1" {
		t.Fatalf("Active before sign_until = %v, %v", a, err)
	}
	if _, err := s.Active(sessionHash, signEnd); !errors.Is(err, ErrIssuanceClosed) {
		t.Fatalf("Active at sign_until = %v, want ErrIssuanceClosed", err)
	}
	// Verification continues until verify_until.
	if c := s.Candidates(sessionHash, "gen1", signEnd); len(c) != 1 {
		t.Fatalf("gen1 at sign_until: %v", c)
	}
	if c := s.Candidates(sessionHash, "gen1", verifyEnd); len(c) != 0 {
		t.Fatalf("gen1 at verify_until must be ineligible: %v", c)
	}
	// Unknown kid never falls back.
	if c := s.Candidates(sessionHash, "nope", t0); len(c) != 0 {
		t.Fatalf("unknown kid: %v", c)
	}
	// ID-less: legacy_read_ids only, in order, filtered by verify_until.
	if c := s.Candidates(sessionHash, "", t0); len(c) != 2 || c[0].ID != "legacy" || c[1].ID != "gen1" {
		t.Fatalf("ID-less candidates = %v", c)
	}
	legacyVerifyEnd := time.Date(2026, 10, 22, 13, 0, 0, 0, time.UTC)
	if c := s.Candidates(sessionHash, "", verifyEnd); len(c) != 1 || c[0].ID != "legacy" {
		t.Fatalf("ID-less after gen1 verify_until = %v", c)
	}
	if c := s.Candidates(sessionHash, "", legacyVerifyEnd); len(c) != 0 {
		t.Fatalf("ID-less after all verify_until = %v", c)
	}
	// gen2 (staged) is never an ID-less candidate.
	for _, c := range s.Candidates(sessionHash, "", t0) {
		if c.ID == "gen2" {
			t.Fatal("staged generation leaked into ID-less candidates")
		}
	}
	g := s.Generations(sessionHash)
	if len(g) != 3 || g[0].ID != "gen1" || g[1].ID != "legacy" || g[2].ID != "gen2" {
		t.Fatalf("Generations = %v", g)
	}
}

// TestDeadlineNullWithWhitespace pins that a null deadline is recognised
// with surrounding whitespace. Parse cannot produce such raw bytes today
// (encoding/json passes a json.RawMessage field the value's own bytes), so
// deadline is called directly.
func TestDeadlineNullWithWhitespace(t *testing.T) {
	for _, raw := range []string{"null", " null", "null ", "\n\tnull\r\n"} {
		got, err := deadline(json.RawMessage(raw), "f")
		if err != nil || got != nil {
			t.Errorf("deadline(%q) = %v, %v; want nil, nil", raw, got, err)
		}
	}
	want := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	got, err := deadline(json.RawMessage(` "2026-10-21T14:00:00Z" `), "f")
	if err != nil || got == nil || !got.Equal(want) {
		t.Errorf("padded RFC 3339 deadline = %v, %v; want %v", got, err, want)
	}
	for _, raw := range []string{" nul l ", `"null"`, "nullx"} {
		if _, err := deadline(json.RawMessage(raw), "f"); err == nil {
			t.Errorf("deadline(%q) accepted, want an error", raw)
		}
	}
	if _, err := deadline(nil, "f"); err == nil {
		t.Error("absent deadline accepted, want an error")
	}
}

func TestRingsReturnCopies(t *testing.T) {
	k := Legacy(fixtureLegacySecret)
	a, _ := k.Signing().Active(sessionHash, t0)
	ab := a.Bytes()
	ab[0] ^= 0xff
	if hex.EncodeToString(a.Bytes()) != goldenSessionHash {
		t.Fatal("mutating the returned bytes changed the key")
	}
	b, _ := k.Signing().Active(sessionHash, t0)
	if hex.EncodeToString(b.Bytes()) != goldenSessionHash {
		t.Fatal("mutating a returned signing key changed the ring")
	}
	e := k.Encryption().Active()
	eb := e.Bytes()
	eb[0] ^= 0xff
	if hex.EncodeToString(e.Bytes()) != goldenEncryption || hex.EncodeToString(k.Encryption().Active().Bytes()) != goldenEncryption {
		t.Fatal("mutating a returned encryption key changed the ring")
	}
}

func TestKeysNeverFormatMaterial(t *testing.T) {
	k, err := Parse(marshal(t, validDoc()))
	if err != nil {
		t.Fatal(err)
	}
	sk, _ := k.Signing().Active(sessionHash, t0)
	ek := k.Encryption().Active()
	values := map[string]any{
		"SigningKey": sk, "EncryptionKey": ek,
		"*SigningRing": k.Signing(), "SigningRing": *k.Signing(),
		"*EncryptionRing": k.Encryption(), "EncryptionRing": *k.Encryption(),
		"*Keyring": k, "Keyring": *k,
		"legacy *Keyring": Legacy(fixtureLegacySecret),
		// A consumer holding keys by value in unexported fields: fmt cannot
		// call their methods there and prints the fields by reflection.
		"consumer struct": consumer{sk: sk, ek: ek, sks: []SigningKey{sk}, ring: *k.Signing()},
		"zero Keyring":    Keyring{},
	}
	// Every rendering of key material that a leak could take: raw bytes as
	// text, hex and the decimal-byte form, for the secrets, the raw
	// encryption key and the derived keys.
	var mats [][]byte
	for _, sec := range []string{fixtureLegacySecret, fixtureGen1Secret, fixtureGen2Secret} {
		mats = append(mats, []byte(sec))
	}
	mats = append(mats, ek.Bytes(), sk.Bytes(), DeriveLegacyEncryptionKey(fixtureLegacySecret))
	check := func(label, out string) {
		t.Helper()
		assertNoSecret(t, out)
		for _, m := range mats {
			for _, form := range []string{hex.EncodeToString(m), decimalBytes(m), base64.StdEncoding.EncodeToString(m), strings.Trim(decimalBytes(m), "[]")} {
				if strings.Contains(out, form) {
					t.Fatalf("%s: output contains key material: %q", label, out)
				}
			}
		}
	}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%q"} {
			check(name+" "+verb, fmt.Sprintf(verb, v))
		}
		check(name+" Sprint", fmt.Sprint(v))
		js, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: json.Marshal: %v", name, err)
		}
		check(name+" json", string(js))
		for _, h := range []string{"text", "json"} {
			var buf bytes.Buffer
			var handler slog.Handler = slog.NewTextHandler(&buf, nil)
			if h == "json" {
				handler = slog.NewJSONHandler(&buf, nil)
			}
			slog.New(handler).Info("x", "v", v, slog.Any("any", v))
			check(name+" slog "+h, buf.String())
		}
	}
	// Reflection-based printing that ignores methods and follows pointers
	// (as spew with DisableMethods, used by testify diffs, does).
	check("reflect walk", reflectDump(reflect.ValueOf(consumer{sk: sk, ek: ek, sks: []SigningKey{sk}, ring: *k.Signing()}), 0))
	check("reflect walk keyring", reflectDump(reflect.ValueOf(*k), 0))
	check("notice", StartupNotice(k))
	check("legacy notice", StartupNotice(Legacy(fixtureLegacySecret)))
	if !strings.Contains(StartupNotice(Legacy(fixtureLegacySecret)), "agent JWTs") {
		t.Fatal("legacy notice must name the coupled uses")
	}
	if !strings.Contains(StartupNotice(k), "those capabilities, admission tokens and relay-peer signatures only") {
		t.Fatal("ring notice must state the pilot limitation")
	}
	if !strings.Contains(StartupNotice(k), "conduit admission tokens and relay-peer signatures (shared-secret fallback) use signing generations") {
		t.Fatal("ring notice must name the conduit purposes that use the ring")
	}
	// The identification is still there.
	if !strings.Contains(fmt.Sprint(k.Signing()), "gen1") || !strings.Contains(fmt.Sprint(*k), "rev-2026-10-21.1") {
		t.Fatalf("identification missing: %v / %v", k.Signing(), *k)
	}
}

// decimalBytes renders b the way fmt prints a []byte with %v ("[1 2 3]"),
// the decimal-byte form a leak through reflection would take.
func decimalBytes(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = strconv.Itoa(int(c))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// consumer is a phase 2-5 style struct holding keys in unexported fields.
type consumer struct {
	sk   SigningKey
	ek   EncryptionKey
	sks  []SigningKey
	ring SigningRing
}

// reflectDump renders v the way method-blind reflection printers do:
// every field, following pointers, slices and maps, with []byte as bytes.
func reflectDump(v reflect.Value, depth int) string {
	if depth > 12 || !v.IsValid() {
		return ""
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "nil"
		}
		return reflectDump(v.Elem(), depth+1)
	case reflect.Struct:
		var b strings.Builder
		for i := 0; i < v.NumField(); i++ {
			b.WriteString(v.Type().Field(i).Name + ":" + reflectDump(v.Field(i), depth+1) + " ")
		}
		return "{" + b.String() + "}"
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			out := make([]byte, v.Len())
			for i := range out {
				out[i] = byte(v.Index(i).Uint())
			}
			return decimalBytes(out) + " " + hex.EncodeToString(out) + " " + string(out)
		}
		var b strings.Builder
		for i := 0; i < v.Len(); i++ {
			b.WriteString(reflectDump(v.Index(i), depth+1) + ",")
		}
		return "[" + b.String() + "]"
	case reflect.Map:
		var b strings.Builder
		for _, key := range v.MapKeys() {
			b.WriteString(reflectDump(key, depth+1) + "=" + reflectDump(v.MapIndex(key), depth+1) + ",")
		}
		return "map[" + b.String() + "]"
	case reflect.String:
		return v.String()
	case reflect.Func:
		return "func"
	default:
		return fmt.Sprint(v)
	}
}

func TestZeroKeyringIsSafe(t *testing.T) {
	var k Keyring
	_ = k.String()
	_ = fmt.Sprintf("%v %+v", k, &k)
	if _, err := json.Marshal(k); err != nil {
		t.Fatalf("json.Marshal(Keyring{}): %v", err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "k", k)
	var sr SigningRing
	var er EncryptionRing
	_ = fmt.Sprint(sr, er, (*SigningRing)(nil).IDs(), (*EncryptionRing)(nil).IDs())
	if (SigningKey{}).Bytes() != nil || (EncryptionKey{}).Bytes() != nil {
		t.Fatal("zero keys must have no bytes")
	}
}

// TestLoadFilePermissions: world write or execute is always refused; the
// world-read bit is refused unless AllowWorldReadable, which loads with a
// WARN; group bits load with a WARN. Errors and WARNs name the path and mode
// only, never key material.
func TestLoadFilePermissions(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	dir := t.TempDir()
	type want struct{ load, worldWarn, groupWarn bool }
	cases := []struct {
		mode  os.FileMode
		allow bool
		want  want
	}{
		{0o400, false, want{load: true}},
		{0o440, false, want{load: true, groupWarn: true}},
		{0o444, false, want{}},
		{0o644, false, want{}},
		{0o400, true, want{load: true}},
		{0o440, true, want{load: true, groupWarn: true}},
		{0o444, true, want{load: true, worldWarn: true, groupWarn: true}},
		{0o644, true, want{load: true, worldWarn: true, groupWarn: true}},
		{0o600, false, want{load: true}},
		{0o640, false, want{load: true, groupWarn: true}},
		// World write or execute: refused under both settings.
		{0o602, false, want{}}, {0o602, true, want{}},
		{0o646, false, want{}}, {0o646, true, want{}},
		{0o605, false, want{}}, {0o605, true, want{}},
		{0o607, false, want{}}, {0o607, true, want{}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%#o/allow=%v", tc.mode, tc.allow), func(t *testing.T) {
			buf.Reset()
			path := filepath.Join(dir, fmt.Sprintf("k%o-%v.json", tc.mode, tc.allow))
			if err := os.WriteFile(path, marshal(t, validDoc()), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFileWith(path, LoadOptions{AllowWorldReadable: tc.allow})
			if tc.want.load != (err == nil) {
				t.Fatalf("load=%v, err=%v", err == nil, err)
			}
			if err != nil {
				if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), fmt.Sprintf("%#o", tc.mode)) {
					t.Fatalf("error must name the path and mode: %v", err)
				}
				if tc.mode&0o003 != 0 && !strings.Contains(err.Error(), "writable or executable by others") {
					t.Fatalf("world write/execute refusal: %v", err)
				}
				assertNoSecret(t, err.Error())
			}
			logs := buf.String()
			if got := strings.Contains(logs, "readable by others (allowed"); got != tc.want.worldWarn {
				t.Fatalf("world-read WARN=%v, want %v (%s)", got, tc.want.worldWarn, logs)
			}
			if got := strings.Contains(logs, "accessible to its group"); got != tc.want.groupWarn {
				t.Fatalf("group WARN=%v, want %v (%s)", got, tc.want.groupWarn, logs)
			}
			assertNoSecret(t, logs)
		})
	}
	// LoadFile is the strict default.
	path := filepath.Join(dir, "default.json")
	if err := os.WriteFile(path, marshal(t, validDoc()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile admitted a world-readable file")
	}
}
