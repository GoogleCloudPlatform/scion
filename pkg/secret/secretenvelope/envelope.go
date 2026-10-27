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

// Package secretenvelope parses and formats the stored ciphertext envelopes of
// Hub-encrypted secret values (ptone/scion#1530 design §5):
//
//	enc:v1:<base64(nonce+ciphertext)>        key-ID-less legacy envelope
//	enc:v2:<kid>:<base64(nonce+ciphertext)>  key-ID-carrying envelope
//
// It does no cryptography and holds no keys, so the store layer can use it
// to enforce the encryption policy fence on writes. It depends on the
// standard library only. Errors never contain the stored value: they name
// the format problem and, at most, a byte offset.
package secretenvelope

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	// Prefix starts every encrypted envelope, whatever its version.
	Prefix = "enc:"
	// PrefixV1 starts a version 1 envelope.
	PrefixV1 = "enc:v1:"
	// PrefixV2 starts a version 2 envelope.
	PrefixV2 = "enc:v2:"
	// AADPrefixV2 is the additional authenticated data prefix of version
	// 2; the full AAD is AADPrefixV2 + kid.
	AADPrefixV2 = "scion-secret-v2:"
	// NonceSize is the AES-GCM nonce length both versions use.
	NonceSize = 12
	// TagSize is the AES-GCM authentication tag length.
	TagSize = 16
	// MaxKIDLen is the longest valid key ID.
	MaxKIDLen = 32
)

var (
	// ErrUnknownVersion is an "enc:" value whose version this release
	// does not know.
	ErrUnknownVersion = errors.New("envelope: unknown encrypted value version")
	// ErrMalformed is an encrypted value that does not parse.
	ErrMalformed = errors.New("envelope: malformed encrypted value")
	// ErrMissingKeyID is a version 2 value without a key ID.
	ErrMissingKeyID = errors.New("envelope: encrypted value has no key ID")
)

// Format is the envelope format of a stored value.
type Format int

const (
	// FormatPlaintext is a value without the "enc:" prefix (legacy
	// plaintext, or the empty reference-only value).
	FormatPlaintext Format = iota
	// FormatV1 is an enc:v1: value.
	FormatV1
	// FormatV2 is an enc:v2: value.
	FormatV2
)

// String names the format.
func (f Format) String() string {
	switch f {
	case FormatPlaintext:
		return "plaintext"
	case FormatV1:
		return "v1"
	case FormatV2:
		return "v2"
	default:
		return "unknown"
	}
}

// Envelope is a parsed stored value. For the encrypted formats Sealed is
// nonce || ciphertext || tag; it never holds plaintext. A plaintext value
// is not copied into the Envelope.
type Envelope struct {
	Format Format
	KID    string // FormatV2 only
	Sealed []byte
}

// ValidKID reports whether kid matches [A-Za-z0-9_-]{1,32}.
func ValidKID(kid string) bool {
	if kid == "" || len(kid) > MaxKIDLen {
		return false
	}
	for i := 0; i < len(kid); i++ {
		c := kid[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// Parse classifies a stored value. A value without the "enc:" prefix is
// FormatPlaintext. An "enc:" value with an unknown version is
// ErrUnknownVersion; a version 2 value with an empty key ID is
// ErrMissingKeyID; any other parse failure (including a bare "enc:v1:")
// is ErrMalformed. The sealed bytes must hold at least a nonce and a tag.
func Parse(stored string) (Envelope, error) {
	if !strings.HasPrefix(stored, Prefix) {
		return Envelope{Format: FormatPlaintext}, nil
	}
	switch {
	case strings.HasPrefix(stored, PrefixV1):
		sealed, err := decodeSealed(stored[len(PrefixV1):], len(PrefixV1))
		if err != nil {
			return Envelope{}, err
		}
		return Envelope{Format: FormatV1, Sealed: sealed}, nil
	case strings.HasPrefix(stored, PrefixV2):
		rest := stored[len(PrefixV2):]
		sep := strings.IndexByte(rest, ':')
		if sep < 0 {
			return Envelope{}, fmt.Errorf("%w: version 2 value has no key ID separator", ErrMalformed)
		}
		kid := rest[:sep]
		if kid == "" {
			return Envelope{}, ErrMissingKeyID
		}
		if !ValidKID(kid) {
			return Envelope{}, fmt.Errorf("%w: invalid key ID at byte %d", ErrMalformed, len(PrefixV2))
		}
		sealed, err := decodeSealed(rest[sep+1:], len(PrefixV2)+sep+1)
		if err != nil {
			return Envelope{}, err
		}
		return Envelope{Format: FormatV2, KID: kid, Sealed: sealed}, nil
	default:
		return Envelope{}, ErrUnknownVersion
	}
}

func decodeSealed(b64 string, offset int) ([]byte, error) {
	if b64 == "" {
		return nil, fmt.Errorf("%w: empty payload at byte %d", ErrMalformed, offset)
	}
	sealed, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		var cie base64.CorruptInputError
		if errors.As(err, &cie) {
			return nil, fmt.Errorf("%w: invalid base64 at byte %d", ErrMalformed, offset+int(cie))
		}
		return nil, fmt.Errorf("%w: invalid base64", ErrMalformed)
	}
	if len(sealed) < NonceSize+TagSize {
		return nil, fmt.Errorf("%w: payload too short", ErrMalformed)
	}
	return sealed, nil
}

// FormatV1Value renders a version 1 envelope.
func FormatV1Value(sealed []byte) string {
	return PrefixV1 + base64.StdEncoding.EncodeToString(sealed)
}

// FormatV2Value renders a version 2 envelope. kid must be valid.
func FormatV2Value(kid string, sealed []byte) string {
	return PrefixV2 + kid + ":" + base64.StdEncoding.EncodeToString(sealed)
}

// AADV2 is the version 2 additional authenticated data for kid.
func AADV2(kid string) []byte {
	return []byte(AADPrefixV2 + kid)
}
