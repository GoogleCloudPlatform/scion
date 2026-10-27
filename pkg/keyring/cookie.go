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
	"errors"
	"fmt"
	"time"

	"github.com/gorilla/securecookie"
)

// Cookie codec errors. They carry no key material; the generation ID is
// not included either (cookies do not name a generation).
var (
	// ErrCookieNotActive is returned by Encode on a non-active codec: only
	// the active generation issues cookies.
	ErrCookieNotActive = errors.New("keyring: only the active signing generation encodes cookies")
	// ErrCookieIssuanceClosed is returned by Encode at or after the
	// active generation's sign_until.
	ErrCookieIssuanceClosed = errors.New("keyring: cookie issuance closed (active generation reached sign_until)")
	// ErrCookieVerificationClosed is returned by Decode at or after a
	// generation's verify_until.
	ErrCookieVerificationClosed = errors.New("keyring: cookie generation reached verify_until")
)

// CookieCodecs returns one Gorilla securecookie codec per signing
// generation, active first, then file order (design §3, Cookies): the
// wire format is unchanged and carries no key ID, so a cookie minted by
// any generation another replica holds as active can be read here.
//
// Each codec's hash and block keys are hashPurpose and blockPurpose of
// that generation. Every codec has the given max age (cookie timestamp
// window) and securecookie's default 4096-byte length limit. Each codec is
// wrapped with its generation's deadlines, evaluated with now on every
// call:
//   - Encode succeeds only on the active codec and only before its
//     sign_until; a non-active codec never encodes, so
//     securecookie.EncodeMulti cannot fall through to an old or staged
//     generation.
//   - Decode succeeds only before the codec's verify_until.
//
// A ring without a usable active generation (zero, nil or empty) is
// refused with ErrNoActiveGeneration: no codec is ever built from an empty
// secret. The returned codecs hold raw securecookie keys: keep them inside
// the cookie store and never print or reflect them.
//
// Pass the result to sessions.CookieStore.Codecs. Do not call
// CookieStore.MaxAge afterwards expecting it to reach the codecs (it only
// updates *securecookie.SecureCookie values); the max age is set here.
func CookieCodecs(r *SigningRing, hashPurpose, blockPurpose Purpose, maxAge int, now func() time.Time) ([]securecookie.Codec, error) {
	if now == nil {
		now = time.Now
	}
	hashes := r.Generations(hashPurpose)
	blocks := r.Generations(blockPurpose)
	if len(hashes) == 0 || len(hashes) != len(blocks) {
		return nil, ErrNoActiveGeneration
	}
	out := make([]securecookie.Codec, 0, len(hashes))
	for i := range hashes {
		sc := securecookie.New(hashes[i].Bytes(), blocks[i].Bytes())
		sc.MaxAge(maxAge)
		out = append(out, &deadlineCodec{
			inner:       sc,
			id:          hashes[i].ID,
			active:      i == 0,
			signUntil:   hashes[i].SignUntil,
			verifyUntil: hashes[i].VerifyUntil,
			now:         now,
		})
	}
	return out, nil
}

// deadlineCodec enforces one generation's deadlines around a
// securecookie codec.
type deadlineCodec struct {
	inner       securecookie.Codec
	id          string
	active      bool
	signUntil   *time.Time
	verifyUntil *time.Time
	now         func() time.Time
}

func (c *deadlineCodec) Encode(name string, value interface{}) (string, error) {
	if !c.active {
		return "", ErrCookieNotActive
	}
	if c.signUntil != nil && !c.now().Before(*c.signUntil) {
		return "", ErrCookieIssuanceClosed
	}
	return c.inner.Encode(name, value)
}

func (c *deadlineCodec) Decode(name, value string, dst interface{}) error {
	if c.verifyUntil != nil && !c.now().Before(*c.verifyUntil) {
		return ErrCookieVerificationClosed
	}
	return c.inner.Decode(name, value, dst)
}

// String prints the generation ID only.
func (c *deadlineCodec) String() string {
	return fmt.Sprintf("cookieCodec{id:%s active:%t}", c.id, c.active)
}

// DecodesWithActive reports whether value decodes under the active codec
// alone (codecs[0] of CookieCodecs). A session that decoded under the
// store's codec list but not under the active one was minted by another
// generation and should be re-saved, which encodes it with the active
// generation.
func DecodesWithActive(name, value string, codecs []securecookie.Codec) bool {
	if len(codecs) == 0 {
		return false
	}
	var discard map[interface{}]interface{}
	return codecs[0].Decode(name, value, &discard) == nil
}
