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
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	// SchemaVersion is the only accepted keyring file schema.
	SchemaVersion = 1
	// MaxEntries is the most entries a ring may hold: active, previous
	// and staged successor.
	MaxEntries = 3
	// MaxLegacyReadIDs bounds each legacy candidate list.
	MaxLegacyReadIDs = 3
	// MinSigningSecretBytes is the shortest accepted new signing secret.
	// The imported "legacy" entry keeps its exact bytes, however short.
	MinSigningSecretBytes = 32
	// EncryptionKeyBytes is the exact raw length of an encryption key.
	EncryptionKeyBytes = 32
	// MaxFileBytes bounds the keyring file read.
	MaxFileBytes = 64 << 10
)

var (
	// keyPattern is the only accepted spelling of a JSON object key: the
	// schema's field names are lower-case snake_case, and encoding/json
	// would otherwise bind "Secret", "SCHEMA" or "\u017fecret" to the same
	// field case-insensitively, bypassing the unknown-field and duplicate
	// checks.
	keyPattern      = regexp.MustCompile(`^[a-z0-9_]+$`)
	idPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	revisionPattern = regexp.MustCompile(`^[A-Za-z0-9._:+/@-]{1,128}$`)
)

// fileV1 is the version-1 keyring file. Pointer and RawMessage fields
// distinguish an absent required field from a zero value.
type fileV1 struct {
	Schema     *int          `json:"schema"`
	Revision   *string       `json:"revision"`
	Signing    *signingV1    `json:"signing"`
	Encryption *encryptionV1 `json:"encryption"`
}

type signingV1 struct {
	Active        *string        `json:"active"`
	Emit          *string        `json:"emit"`
	Keys          []signingKeyV1 `json:"keys"`
	LegacyReadIDs *[]string      `json:"legacy_read_ids"`
}

type signingKeyV1 struct {
	ID          *string         `json:"id"`
	Secret      *string         `json:"secret"`
	SignUntil   json.RawMessage `json:"sign_until"`
	VerifyUntil json.RawMessage `json:"verify_until"`
}

type encryptionV1 struct {
	Active          *string           `json:"active"`
	WriteFormat     *string           `json:"write_format"`
	Keys            []encryptionKeyV1 `json:"keys"`
	LegacyV1ReadIDs *[]string         `json:"legacy_v1_read_ids"`
}

type encryptionKeyV1 struct {
	ID     *string `json:"id"`
	KeyB64 *string `json:"key_b64"`
}

// LoadOptions adjusts LoadFileWith.
type LoadOptions struct {
	// AllowWorldReadable admits a file whose world-read bit (0o004) is set,
	// with a WARN. It never admits world write or execute (0o003). It is the
	// operator override server.keyring_file_allow_world_readable.
	AllowWorldReadable bool
}

// LoadFile is LoadFileWith with the default (strict) options.
func LoadFile(path string) (*Keyring, error) {
	return LoadFileWith(path, LoadOptions{})
}

// LoadFileWith reads and validates a version-1 keyring file. The path may
// be a symlink (as Kubernetes secret mounts are) but must resolve to a
// regular file of at most MaxFileBytes; anything else is refused before it
// is opened. File permissions:
//   - any world-write or world-execute bit (0o003): refused;
//   - the world-read bit (0o004): refused unless opts.AllowWorldReadable,
//     which loads the file with a WARN;
//   - group bits (0o070): loaded with a WARN.
//
// Errors and WARNs name the path and mode only; no error carries key
// material. Mount the file with mode 0400 (0440 when a group must read it).
func LoadFileWith(path string, opts LoadOptions) (*Keyring, error) {
	// Stat before Open: a FIFO or device would block or misbehave in Open.
	pre, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("keyring file: %w", err)
	}
	if !pre.Mode().IsRegular() {
		return nil, errors.New("keyring file: not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("keyring file: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("keyring file: %w", err)
	}
	if !os.SameFile(pre, st) || !st.Mode().IsRegular() {
		return nil, errors.New("keyring file: changed while being opened")
	}
	perm := st.Mode().Perm()
	mode := fmt.Sprintf("%#o", perm)
	if perm&0o003 != 0 {
		return nil, fmt.Errorf("keyring file %s is writable or executable by others (mode %s); mount it with mode 0400 (0440 when a group must read it)", path, mode)
	}
	if perm&0o004 != 0 {
		if !opts.AllowWorldReadable {
			return nil, fmt.Errorf("keyring file %s is readable by others (mode %s); mount it with mode 0400 (0440 when a group must read it), or set server.keyring_file_allow_world_readable", path, mode)
		}
		slog.Warn("keyring file is readable by others (allowed by server.keyring_file_allow_world_readable); prefer mode 0400 or 0440",
			"path", path, "mode", mode)
	}
	if perm&0o070 != 0 {
		slog.Warn("keyring file is accessible to its group; restrict it to the server user (mode 0400) unless a group must read it",
			"path", path, "mode", mode)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("keyring file: read failed: %w", err)
	}
	return Parse(data)
}

// Parse validates a version-1 keyring document: no unknown fields or
// duplicate object keys, every required field present, at most three
// entries per ring with unique IDs, an active entry, bounded legacy
// candidate lists, consistent deadlines and valid key lengths.
func Parse(data []byte) (*Keyring, error) {
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("keyring file: larger than %d bytes", MaxFileBytes)
	}
	if err := checkJSONShape(data); err != nil {
		return nil, err
	}
	var f fileV1
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, decodeError(err, dec.InputOffset())
	}
	if f.Schema == nil {
		return nil, missing("schema")
	}
	if *f.Schema != SchemaVersion {
		return nil, fmt.Errorf("keyring file: unsupported schema %d (want %d)", *f.Schema, SchemaVersion)
	}
	if f.Revision == nil {
		return nil, missing("revision")
	}
	if !revisionPattern.MatchString(*f.Revision) {
		return nil, invalid("revision", "must match "+revisionPattern.String())
	}
	if f.Signing == nil {
		return nil, missing("signing")
	}
	if f.Encryption == nil {
		return nil, missing("encryption")
	}
	signing, err := buildSigning(f.Signing)
	if err != nil {
		return nil, err
	}
	encryption, err := buildEncryption(f.Encryption)
	if err != nil {
		return nil, err
	}
	return &Keyring{source: SourceFile, revision: *f.Revision, signing: signing, encryption: encryption}, nil
}

func buildSigning(s *signingV1) (*SigningRing, error) {
	if s.Active == nil {
		return nil, missing("signing.active")
	}
	if s.Emit == nil {
		return nil, missing("signing.emit")
	}
	emit := Emit(*s.Emit)
	if emit != EmitLegacy && emit != EmitVersioned {
		return nil, invalid("signing.emit", `must be "legacy" or "versioned"`)
	}
	if s.Keys == nil {
		return nil, missing("signing.keys")
	}
	if len(s.Keys) == 0 || len(s.Keys) > MaxEntries {
		return nil, invalid("signing.keys", fmt.Sprintf("must hold 1 to %d entries", MaxEntries))
	}
	r := &SigningRing{active: *s.Active, emit: emit}
	seen := map[string]bool{}
	firstAt := map[string]int{}
	for i, k := range s.Keys {
		field := fmt.Sprintf("signing.keys[%d]", i)
		if k.ID == nil {
			return nil, missing(field + ".id")
		}
		id := *k.ID
		if !idPattern.MatchString(id) {
			return nil, invalid(field+".id", "must match "+idPattern.String())
		}
		if seen[id] {
			return nil, fmt.Errorf("keyring file: duplicate signing key id: %s repeats signing.keys[%d]", field, firstAt[id])
		}
		seen[id] = true
		firstAt[id] = i
		if k.Secret == nil {
			return nil, missing(field + ".secret")
		}
		if id == LegacyID {
			if len(*k.Secret) == 0 {
				return nil, invalid(field+".secret", "must not be empty")
			}
		} else if len(*k.Secret) < MinSigningSecretBytes {
			return nil, invalid(field+".secret", fmt.Sprintf("must be at least %d bytes", MinSigningSecretBytes))
		}
		signUntil, err := deadline(k.SignUntil, field+".sign_until")
		if err != nil {
			return nil, err
		}
		verifyUntil, err := deadline(k.VerifyUntil, field+".verify_until")
		if err != nil {
			return nil, err
		}
		if (signUntil == nil) != (verifyUntil == nil) {
			return nil, invalid(field, "sign_until and verify_until must both be set or both be null")
		}
		if signUntil != nil && !signUntil.Before(*verifyUntil) {
			return nil, invalid(field, "sign_until must be before verify_until")
		}
		r.entries = append(r.entries, signingEntry{
			id: id, secret: newMaterial([]byte(*k.Secret)), signUntil: signUntil, verifyUntil: verifyUntil,
		})
	}
	if !seen[r.active] {
		return nil, invalid("signing.active", "must name a signing key id")
	}
	if s.LegacyReadIDs == nil {
		return nil, missing("signing.legacy_read_ids")
	}
	ids, err := candidateIDs(*s.LegacyReadIDs, seen, "signing.legacy_read_ids")
	if err != nil {
		return nil, err
	}
	r.legacyReads = ids
	return r, nil
}

func buildEncryption(e *encryptionV1) (*EncryptionRing, error) {
	if e.Active == nil {
		return nil, missing("encryption.active")
	}
	if e.WriteFormat == nil {
		return nil, missing("encryption.write_format")
	}
	wf := WriteFormat(*e.WriteFormat)
	if wf != WriteV1 && wf != WriteV2 {
		return nil, invalid("encryption.write_format", `must be "v1" or "v2"`)
	}
	if e.Keys == nil {
		return nil, missing("encryption.keys")
	}
	if len(e.Keys) == 0 || len(e.Keys) > MaxEntries {
		return nil, invalid("encryption.keys", fmt.Sprintf("must hold 1 to %d entries", MaxEntries))
	}
	r := &EncryptionRing{active: *e.Active, writeFormat: wf}
	seen := map[string]bool{}
	firstAt := map[string]int{}
	for i, k := range e.Keys {
		field := fmt.Sprintf("encryption.keys[%d]", i)
		if k.ID == nil {
			return nil, missing(field + ".id")
		}
		id := *k.ID
		if !idPattern.MatchString(id) {
			return nil, invalid(field+".id", "must match "+idPattern.String())
		}
		if seen[id] {
			return nil, fmt.Errorf("keyring file: duplicate encryption key id: %s repeats encryption.keys[%d]", field, firstAt[id])
		}
		seen[id] = true
		firstAt[id] = i
		if k.KeyB64 == nil {
			return nil, missing(field + ".key_b64")
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(*k.KeyB64)
		if err != nil {
			return nil, invalid(field+".key_b64", "must be standard padded base64")
		}
		if len(raw) != EncryptionKeyBytes {
			return nil, invalid(field+".key_b64", fmt.Sprintf("must decode to exactly %d bytes", EncryptionKeyBytes))
		}
		r.entries = append(r.entries, encryptionEntry{id: id, key: newMaterial(raw)})
	}
	if !seen[r.active] {
		return nil, invalid("encryption.active", "must name an encryption key id")
	}
	if e.LegacyV1ReadIDs == nil {
		return nil, missing("encryption.legacy_v1_read_ids")
	}
	ids, err := candidateIDs(*e.LegacyV1ReadIDs, seen, "encryption.legacy_v1_read_ids")
	if err != nil {
		return nil, err
	}
	r.legacyReads = ids
	return r, nil
}

func candidateIDs(ids []string, known map[string]bool, field string) ([]string, error) {
	if len(ids) > MaxLegacyReadIDs {
		return nil, invalid(field, fmt.Sprintf("must hold at most %d ids", MaxLegacyReadIDs))
	}
	seen := map[string]bool{}
	for i, id := range ids {
		if !known[id] {
			return nil, invalid(fmt.Sprintf("%s[%d]", field, i), "names no key in the ring")
		}
		if seen[id] {
			return nil, invalid(fmt.Sprintf("%s[%d]", field, i), "repeats an earlier id")
		}
		seen[id] = true
	}
	return append([]string(nil), ids...), nil
}

// deadline parses a required, nullable RFC 3339 deadline.
func deadline(raw json.RawMessage, field string) (*time.Time, error) {
	if len(raw) == 0 {
		return nil, missing(field + " (use null for no deadline)")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, invalid(field, "must be an RFC 3339 string or null")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, invalid(field, "must be an RFC 3339 string or null")
	}
	t = t.UTC()
	return &t, nil
}

func missing(field string) error {
	return fmt.Errorf("keyring file: missing required field %s", field)
}

func invalid(field, why string) error {
	return fmt.Errorf("keyring file: invalid %s: %s", field, why)
}

// decodeError maps a json decode error to a message that names a schema
// field path or a byte offset only: encoding/json's syntax and
// unknown-field errors quote input, which may be misplaced key material.
func decodeError(err error, offset int64) error {
	var typeErr *json.UnmarshalTypeError
	var synErr *json.SyntaxError
	switch {
	case errors.As(err, &typeErr):
		return fmt.Errorf("keyring file: field %s has the wrong type (want %s)", typeErr.Field, typeErr.Type)
	case errors.As(err, &synErr):
		return fmt.Errorf("keyring file: malformed JSON at byte %d", synErr.Offset)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return errors.New("keyring file: empty or truncated JSON")
	default:
		// DisallowUnknownFields: the error quotes the key, which may be
		// misplaced key material; report the position only.
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return fmt.Errorf("keyring file: unknown field near byte %d", offset)
		}
		return fmt.Errorf("keyring file: malformed JSON near byte %d", offset)
	}
}

// checkJSONShape rejects malformed JSON, trailing data after the document
// and duplicate keys in any object (encoding/json silently keeps the last
// duplicate, which would let two deliveries of "the same" file differ).
func checkJSONShape(data []byte) error {
	type frame struct {
		object  bool
		wantKey bool
		keys    map[string]bool
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var stack []*frame
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
	started := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return decodeError(err, dec.InputOffset())
		}
		if started && len(stack) == 0 {
			return errors.New("keyring file: trailing data after the JSON document")
		}
		started = true
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				stack = append(stack, &frame{object: true, wantKey: true, keys: map[string]bool{}})
			case '[':
				stack = append(stack, &frame{})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
		case string:
			if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].wantKey {
				top := stack[n-1]
				// Keys are never quoted back: a misplaced value (an
				// operator who left out a field name) arrives here as a key.
				if !keyPattern.MatchString(v) {
					return fmt.Errorf("keyring file: invalid JSON object key near byte %d: keys are lower-case snake_case", dec.InputOffset())
				}
				if top.keys[v] {
					return fmt.Errorf("keyring file: duplicate JSON object key near byte %d", dec.InputOffset())
				}
				top.keys[v] = true
				top.wantKey = false
				continue
			}
			valueDone()
		default:
			valueDone()
		}
	}
	if !started {
		return errors.New("keyring file: empty or truncated JSON")
	}
	return nil
}

// CheckSessionPilot enforces this release's limits on a keyring file
// (design Phase 1): only web session cookies use the signing ring, so
// every other purpose keeps the unchanged legacy material from the
// "legacy" signing entry, and the imported encryption entry must be kept
// unchanged as the active key. It refuses emit "versioned" and
// write_format "v2", whose readers do not exist yet.
func (k *Keyring) CheckSessionPilot() error {
	if k.source != SourceFile {
		return nil
	}
	if k.signing.emit != EmitLegacy {
		return errors.New(`keyring file: signing.emit "versioned" is not supported in this release; use "legacy"`)
	}
	if k.encryption.writeFormat != WriteV1 {
		return errors.New(`keyring file: encryption.write_format "v2" is not supported in this release; use "v1"`)
	}
	legacy, ok := k.signing.LegacySecret()
	if !ok {
		return fmt.Errorf("keyring file: this release requires a signing key with id %q holding the legacy server secret: every purpose not yet on the signing ring still uses it", LegacyID)
	}
	want := DeriveLegacyEncryptionKey(legacy)
	if subtle.ConstantTimeCompare(k.encryption.Active().Bytes(), want) != 1 {
		return fmt.Errorf("keyring file: this release requires the active encryption key to be the imported legacy key (derived from the %q signing secret), kept unchanged", LegacyID)
	}
	return nil
}
