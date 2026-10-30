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

package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Typed ValueCipher errors (in addition to secretenvelope.ErrUnknownVersion,
// secretenvelope.ErrMalformed and secretenvelope.ErrMissingKeyID). None of them is,
// wraps or may be treated as store.ErrNotFound: a value that cannot be
// read is never missing data. No error text carries a key, a stored value
// or a plaintext.
var (
	// ErrUnknownKeyID: a version 2 value names a key ID the ring does not
	// hold. No other key is tried.
	ErrUnknownKeyID = errors.New("secret: encrypted value names an unknown key ID")
	// ErrDecrypt: no candidate key authenticated the value.
	ErrDecrypt = errors.New("secret: encrypted value failed authentication")
	// ErrPlaintextNotAdmitted: a managed (keyring file) cipher read a
	// plaintext value. Only an explicitly admitted migration reads it.
	ErrPlaintextNotAdmitted = errors.New("secret: plaintext value is not admitted")
	// ErrNoDecryptionKey: no key can read this envelope (encryption is
	// disabled, or the ring has no legacy version 1 reader).
	ErrNoDecryptionKey = errors.New("secret: no key is configured to read this encrypted value")
	// ErrNoEncryptionKey: the ring has no usable writer key.
	ErrNoEncryptionKey = errors.New("secret: no encryption key is configured")
	// ErrCipherNotConfigured: a nil or zero ValueCipher was used. It never
	// falls back to plaintext; the disabled (plaintext) mode exists only
	// through NewLegacyValueCipher("").
	ErrCipherNotConfigured = errors.New("secret: no value cipher is configured")
	// ErrWriterNotReadable: a ring that writes enc:v1: does not list its
	// active key among legacy_v1_read_ids, so it could not read back what
	// it writes.
	ErrWriterNotReadable = errors.New("secret: encryption write_format v1 requires the active key in legacy_v1_read_ids")
)

type cipherMode int

const (
	// modeInvalid is the zero value (a nil or zero ValueCipher): every
	// Encrypt and Decrypt fails with ErrCipherNotConfigured.
	modeInvalid cipherMode = iota
	// modeDisabled: legacy adapter without a server secret (ephemeral
	// development): values are stored and read as plaintext; an encrypted
	// value cannot be read.
	modeDisabled
	// modeLegacy: legacy adapter over the server secret; plaintext rows
	// pass through unchanged (pre-policy legacy behaviour, design §7).
	modeLegacy
	// modeManaged: keyring file; plaintext is refused at runtime.
	modeManaged
)

func (m cipherMode) String() string {
	switch m {
	case modeDisabled:
		return "disabled"
	case modeLegacy:
		return "legacy"
	case modeManaged:
		return "managed"
	default:
		return "unconfigured"
	}
}

// ValueCipher is the one shared encryption object for every locally
// stored Hub ciphertext (design §5): the local secret backend, signing-key
// backups, OIDC key and keyset backups and the conduit grant ring. It is
// immutable and safe for concurrent use. Key material stays inside the
// keyring; it prints, logs and marshals its mode and key IDs only.
type ValueCipher struct {
	mode cipherMode
	ring *keyring.EncryptionRing
	// nonce fills a fresh nonce; a seam for golden fixtures.
	nonce func([]byte) error
}

func randomNonce(b []byte) error {
	_, err := rand.Read(b)
	return err
}

// NewValueCipher returns the cipher for k: managed for a keyring file,
// legacy for the legacy adapter. It refuses a nil keyring or one whose
// encryption ring has no usable active key.
func NewValueCipher(k *keyring.Keyring) (*ValueCipher, error) {
	if k == nil {
		return nil, errors.New("secret: value cipher needs a keyring")
	}
	ring := k.Encryption()
	if len(ring.Active().Bytes()) == 0 {
		return nil, ErrNoEncryptionKey
	}
	if ring.WriteFormat() == keyring.WriteV1 {
		readable := false
		for _, cand := range ring.Candidates("") {
			if cand.ID == ring.ActiveID() {
				readable = true
			}
		}
		if !readable {
			return nil, ErrWriterNotReadable
		}
	}
	mode := modeLegacy
	if k.Source() == keyring.SourceFile {
		mode = modeManaged
	}
	return &ValueCipher{mode: mode, ring: ring, nonce: randomNonce}, nil
}

// NewLegacyValueCipher returns the legacy-mode cipher over the single
// server secret (keyring.Legacy), or the disabled cipher when the secret
// is empty, exactly as the local backend behaved before.
func NewLegacyValueCipher(sharedSecret string) *ValueCipher {
	if sharedSecret == "" {
		return &ValueCipher{mode: modeDisabled, nonce: randomNonce}
	}
	c, err := NewValueCipher(keyring.Legacy(sharedSecret))
	if err != nil {
		// keyring.Legacy always has a 32-byte active key.
		panic("secret: legacy keyring without an encryption key")
	}
	return c
}

// Enabled reports whether values are encrypted: false for the disabled
// cipher and for a nil or zero one (which refuses every operation).
func (c *ValueCipher) Enabled() bool {
	return c != nil && (c.mode == modeLegacy || c.mode == modeManaged)
}

// configured reports whether c may be used at all.
func (c *ValueCipher) configured() bool { return c != nil && c.mode != modeInvalid }

// Managed reports whether the cipher comes from a keyring file.
func (c *ValueCipher) Managed() bool { return c != nil && c.mode == modeManaged }

// WriterID is the active (writer) key ID, "" when disabled.
func (c *ValueCipher) WriterID() string {
	if !c.Enabled() {
		return ""
	}
	return c.ring.ActiveID()
}

// ReaderIDs are every key ID the cipher can read with, active first.
func (c *ValueCipher) ReaderIDs() []string {
	if !c.Enabled() {
		return nil
	}
	return c.ring.IDs()
}

// WriteFormat is the envelope Encrypt writes ("" when disabled).
func (c *ValueCipher) WriteFormat() keyring.WriteFormat {
	if !c.Enabled() {
		return ""
	}
	return c.ring.WriteFormat()
}

// UsesRing reports whether the cipher wraps exactly this encryption ring.
func (c *ValueCipher) UsesRing(r *keyring.EncryptionRing) bool {
	return c != nil && r != nil && c.ring == r
}

// Encrypt seals plaintext under the active key in the configured write
// format: enc:v1: (byte-compatible with EncryptValue) or
// enc:v2:<kid>: with AAD "scion-secret-v2:"+kid. The disabled cipher
// returns plaintext unchanged (legacy ephemeral development mode); a nil
// or zero cipher fails with ErrCipherNotConfigured.
func (c *ValueCipher) Encrypt(plaintext string) (string, error) {
	if !c.configured() {
		return "", ErrCipherNotConfigured
	}
	if !c.Enabled() {
		return plaintext, nil
	}
	active := c.ring.Active()
	key := active.Bytes()
	if len(key) == 0 {
		return "", ErrNoEncryptionKey
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, secretenvelope.NonceSize)
	if err := c.nonce(nonce); err != nil {
		return "", fmt.Errorf("secret: generating nonce: %w", err)
	}
	switch c.ring.WriteFormat() {
	case keyring.WriteV1:
		return secretenvelope.FormatV1Value(gcm.Seal(nonce, nonce, []byte(plaintext), nil)), nil
	case keyring.WriteV2:
		return secretenvelope.FormatV2Value(active.ID, gcm.Seal(nonce, nonce, []byte(plaintext), secretenvelope.AADV2(active.ID))), nil
	default:
		return "", errors.New("secret: unknown encryption write format")
	}
}

// Decrypt opens a stored value on a runtime path. A version 2 value is
// tried with its named key only (ErrUnknownKeyID when absent, never a
// fallback); a version 1 value with the bounded legacy_v1_read_ids. A
// plaintext value passes through in legacy and disabled mode and is
// ErrPlaintextNotAdmitted in managed mode. Every failure is a typed error;
// a failure never yields a value.
func (c *ValueCipher) Decrypt(stored string) (string, error) {
	return c.decrypt(stored, false)
}

// DecryptForMigration is Decrypt for the administrative migration reader
// (phase 6): it additionally admits plaintext, in any mode, but only for a
// migration that is running with plaintext admission enabled. It is not a
// runtime capability; TestDecryptForMigrationCallers pins its callers to
// an allow-list.
func (c *ValueCipher) DecryptForMigration(stored string, m *store.EncryptionMigration) (string, error) {
	admit := m != nil && m.State == store.EncryptionMigrationRunning && m.PlaintextReadAdmitted
	return c.decrypt(stored, admit)
}

func (c *ValueCipher) decrypt(stored string, admitPlaintext bool) (string, error) {
	if !c.configured() {
		return "", ErrCipherNotConfigured
	}
	env, err := secretenvelope.Parse(stored)
	if err != nil {
		return "", err
	}
	if env.Format == secretenvelope.FormatPlaintext {
		if admitPlaintext || c.mode != modeManaged {
			return stored, nil
		}
		return "", ErrPlaintextNotAdmitted
	}
	if !c.Enabled() {
		return "", ErrNoDecryptionKey
	}
	var (
		candidates []keyring.EncryptionKey
		aad        func(id string) []byte
	)
	switch env.Format {
	case secretenvelope.FormatV1:
		candidates = c.ring.Candidates("")
		if len(candidates) == 0 {
			return "", ErrNoDecryptionKey
		}
		aad = func(string) []byte { return nil }
	case secretenvelope.FormatV2:
		candidates = c.ring.Candidates(env.KID)
		if len(candidates) == 0 {
			return "", ErrUnknownKeyID
		}
		aad = secretenvelope.AADV2
	}
	nonce, sealed := env.Sealed[:secretenvelope.NonceSize], env.Sealed[secretenvelope.NonceSize:]
	for _, k := range candidates {
		gcm, err := newGCM(k.Bytes())
		if err != nil {
			continue
		}
		if pt, err := gcm.Open(nil, nonce, sealed, aad(k.ID)); err == nil {
			return string(pt), nil
		}
	}
	return "", ErrDecrypt
}

// Inspect parses a stored value without decrypting it.
func (c *ValueCipher) Inspect(stored string) (secretenvelope.Envelope, error) {
	return secretenvelope.Parse(stored)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		// The error text names the key length only.
		return nil, fmt.Errorf("secret: %w", err)
	}
	return cipher.NewGCM(block)
}

// String prints the mode and key IDs only.
func (c *ValueCipher) String() string {
	if c == nil {
		return "ValueCipher{mode:unconfigured}"
	}
	return fmt.Sprintf("ValueCipher{mode:%s writer:%s readers:%v format:%s}", c.mode, c.WriterID(), c.ReaderIDs(), c.WriteFormat())
}

// GoString prints the mode and key IDs only.
func (c *ValueCipher) GoString() string { return c.String() }

// Format prints the mode and key IDs only for every verb.
func (c *ValueCipher) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

// LogValue logs the mode and key IDs only.
func (c *ValueCipher) LogValue() slog.Value {
	if c == nil {
		return slog.GroupValue(slog.String("mode", modeInvalid.String()))
	}
	return slog.GroupValue(slog.String("mode", c.mode.String()), slog.String("writer", c.WriterID()),
		slog.Any("readers", c.ReaderIDs()), slog.String("format", string(c.WriteFormat())))
}

// MarshalJSON emits the mode and key IDs only.
func (c *ValueCipher) MarshalJSON() ([]byte, error) {
	if c == nil {
		return json.Marshal(map[string]any{"mode": modeInvalid.String()})
	}
	return json.Marshal(map[string]any{"mode": c.mode.String(), "writer": c.WriterID(),
		"readers": c.ReaderIDs(), "format": c.WriteFormat()})
}
