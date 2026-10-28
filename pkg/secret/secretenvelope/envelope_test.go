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

package secretenvelope

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func sealedFixture(n int) []byte { return bytes.Repeat([]byte{0xA5}, n) }

func TestParse(t *testing.T) {
	ok := base64.StdEncoding.EncodeToString(sealedFixture(NonceSize + TagSize))
	short := base64.StdEncoding.EncodeToString(sealedFixture(NonceSize + TagSize - 1))
	tests := []struct {
		name    string
		in      string
		format  Format
		kid     string
		wantErr error
	}{
		{name: "empty is plaintext", in: "", format: FormatPlaintext},
		{name: "plaintext", in: "fake-plain-value", format: FormatPlaintext},
		{name: "plaintext lookalike without enc colon", in: "encrypted", format: FormatPlaintext},
		{name: "v1", in: PrefixV1 + ok, format: FormatV1},
		{name: "v2", in: PrefixV2 + "k_2-B:" + ok, format: FormatV2, kid: "k_2-B"},
		{name: "v2 kid of 32", in: PrefixV2 + strings.Repeat("a", 32) + ":" + ok, format: FormatV2, kid: strings.Repeat("a", 32)},
		{name: "bare enc", in: "enc:", wantErr: ErrUnknownVersion},
		{name: "unknown version", in: "enc:v3:" + ok, wantErr: ErrUnknownVersion},
		{name: "unknown version no colon", in: "enc:v1", wantErr: ErrUnknownVersion},
		{name: "bare v1", in: PrefixV1, wantErr: ErrMalformed},
		{name: "v1 bad base64", in: PrefixV1 + "!!!!", wantErr: ErrMalformed},
		{name: "v1 unpadded base64", in: PrefixV1 + strings.TrimRight(base64.StdEncoding.EncodeToString(sealedFixture(29)), "="), wantErr: ErrMalformed},
		{name: "v1 too short", in: PrefixV1 + short, wantErr: ErrMalformed},
		{name: "v2 no separator", in: PrefixV2 + "k2", wantErr: ErrMalformed},
		{name: "v2 missing kid", in: PrefixV2 + ":" + ok, wantErr: ErrMissingKeyID},
		{name: "v2 bare", in: PrefixV2, wantErr: ErrMalformed},
		{name: "v2 kid too long", in: PrefixV2 + strings.Repeat("a", 33) + ":" + ok, wantErr: ErrMalformed},
		{name: "v2 kid bad char", in: PrefixV2 + "k.2:" + ok, wantErr: ErrMalformed},
		{name: "v2 empty payload", in: PrefixV2 + "k2:", wantErr: ErrMalformed},
		{name: "v2 too short", in: PrefixV2 + "k2:" + short, wantErr: ErrMalformed},
		{name: "v2 bad base64", in: PrefixV2 + "k2:%%%%", wantErr: ErrMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, err := Parse(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Parse error = %v, want %v", err, tc.wantErr)
				}
				for _, other := range []error{ErrUnknownVersion, ErrMalformed, ErrMissingKeyID} {
					if other != tc.wantErr && errors.Is(err, other) {
						t.Fatalf("Parse error %v also matches %v", err, other)
					}
				}
				if strings.Contains(err.Error(), ok) || strings.Contains(err.Error(), "!!!!") || strings.Contains(err.Error(), "%%%%") {
					t.Fatalf("error text echoes the stored value")
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if env.Format != tc.format || env.KID != tc.kid {
				t.Fatalf("Parse = (%v, %q), want (%v, %q)", env.Format, env.KID, tc.format, tc.kid)
			}
			if env.Format == FormatPlaintext && env.Sealed != nil {
				t.Fatalf("plaintext copied into the envelope")
			}
		})
	}
}

func TestFormatRoundTrip(t *testing.T) {
	sealed := sealedFixture(40)
	v1 := FormatV1Value(sealed)
	env, err := Parse(v1)
	if err != nil || env.Format != FormatV1 || !bytes.Equal(env.Sealed, sealed) {
		t.Fatalf("v1 round trip: %v %v", env.Format, err)
	}
	v2 := FormatV2Value("kid-1", sealed)
	if !strings.HasPrefix(v2, "enc:v2:kid-1:") {
		t.Fatalf("v2 prefix: got %q", v2[:13])
	}
	env, err = Parse(v2)
	if err != nil || env.Format != FormatV2 || env.KID != "kid-1" || !bytes.Equal(env.Sealed, sealed) {
		t.Fatalf("v2 round trip: %v %q %v", env.Format, env.KID, err)
	}
	if got := string(AADV2("kid-1")); got != "scion-secret-v2:kid-1" {
		t.Fatalf("AADV2 = %q", got)
	}
}

func TestValidKID(t *testing.T) {
	for _, id := range []string{"a", "A-z_0", strings.Repeat("Z", 32), "legacy"} {
		if !ValidKID(id) {
			t.Errorf("ValidKID(%q) = false", id)
		}
	}
	for _, id := range []string{"", strings.Repeat("a", 33), "a b", "a:b", "a.b", "ä"} {
		if ValidKID(id) {
			t.Errorf("ValidKID(%q) = true", id)
		}
	}
}

func TestFormatString(t *testing.T) {
	if FormatPlaintext.String() != "plaintext" || FormatV1.String() != "v1" || FormatV2.String() != "v2" || Format(9).String() != "unknown" {
		t.Fatal("Format.String")
	}
}
