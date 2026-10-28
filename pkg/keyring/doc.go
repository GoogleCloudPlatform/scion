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

// Package keyring holds the Hub server's versioned key material: an
// immutable SigningRing (session cookies, and in later phases JWTs, signed
// URLs, cursors, admission tokens and the relay-peer fallback) and an
// immutable EncryptionRing (locally stored ciphertext), loaded once at
// startup from a version-1 keyring file (server.keyring_file) or built from
// the single legacy server secret (Legacy).
//
// Key IDs name generations; they never change derived key bytes. Each
// consumer defines a Purpose with its existing derivation, so a ring built
// from the legacy secret derives exactly the bytes the pre-ring code did.
//
// The package has no dependency on the hub, config or cmd packages. Its
// types print, log and marshal IDs only, and key bytes sit behind an
// unexported closure, so reflection-based printers (fmt through an
// unexported field, spew, testify diffs) cannot reach them either. The
// remaining limit is deliberate: bytes returned by SigningKey.Bytes,
// EncryptionKey.Bytes or SigningRing.LegacySecret are plain values, and a
// caller that formats, logs or stores them leaks them; so would unsafe
// access to unexported state. The codecs CookieCodecs returns wrap
// gorilla/securecookie values that hold their HMAC and AES keys in plain
// unexported fields: keep them inside the cookie store and never print them.
// Keys held in closures are never reflect.DeepEqual; compare by ID and Bytes.
//
// Fail-closed rule (part of the contract): a ring is usable only when its
// active ID names one of its entries. A nil, zero or empty ring, or one with
// a dangling active ID, is treated as empty by every accessor:
// SigningRing.Active returns ErrNoActiveGeneration; Generations, Candidates
// (for any kid), IDs and LegacySecret return nothing; CookieCodecs refuses;
// EncryptionRing.Active has no bytes and Candidates returns nothing. Every
// pointer-receiver accessor of SigningRing, EncryptionRing and Keyring
// accepts a nil receiver and returns zero values ("" or nil) and no
// material. The value-receiver formatting methods (String, GoString,
// Format, LogValue, MarshalJSON) are safe through fmt, slog and
// encoding/json, but a direct call on a nil pointer panics.
//
// Design: ptone/scion#1530 ("Safe rotation of the Hub server secret"),
// §1-§3. This release uses the signing ring for web session cookies
// (Phase 1) and for the hub's skill file URLs, artifact views, list cursors
// and attachment deliveries (Phase 4); every other purpose still uses the
// ring's "legacy" entry.
package keyring
