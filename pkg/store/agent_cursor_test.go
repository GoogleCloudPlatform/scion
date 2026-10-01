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

package store

import (
	"errors"
	"testing"
	"time"
)

func TestAgentCursor_RoundTrip(t *testing.T) {
	k := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cursor := EncodeAgentCursor("updated", "desc", k, created, "agent-1", "bind123")

	decoded, err := DecodeAgentCursor(cursor, "updated", "desc", "bind123")
	if err != nil {
		t.Fatalf("DecodeAgentCursor: %v", err)
	}
	if !decoded.K.Equal(k) {
		t.Errorf("K = %v, want %v", decoded.K, k)
	}
	if !decoded.Created.Equal(created) {
		t.Errorf("Created = %v, want %v", decoded.Created, created)
	}
	if decoded.ID != "agent-1" {
		t.Errorf("ID = %q, want agent-1", decoded.ID)
	}
}

func TestAgentCursor_SortMismatchRejected(t *testing.T) {
	k := time.Now().UTC()
	cursor := EncodeAgentCursor("updated", "desc", k, k, "agent-1", "bind")
	if _, err := DecodeAgentCursor(cursor, "created", "desc", "bind"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("sort mismatch: err = %v, want ErrInvalidInput", err)
	}
}

func TestAgentCursor_DirMismatchRejected(t *testing.T) {
	k := time.Now().UTC()
	cursor := EncodeAgentCursor("updated", "desc", k, k, "agent-1", "bind")
	if _, err := DecodeAgentCursor(cursor, "updated", "asc", "bind"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("dir mismatch: err = %v, want ErrInvalidInput", err)
	}
}

func TestAgentCursor_BindingMismatchRejected(t *testing.T) {
	k := time.Now().UTC()
	cursor := EncodeAgentCursor("updated", "desc", k, k, "agent-1", "bind-a")
	if _, err := DecodeAgentCursor(cursor, "updated", "desc", "bind-b"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("binding mismatch: err = %v, want ErrInvalidInput", err)
	}
}

func TestAgentCursor_LegacyCursorRejected(t *testing.T) {
	// A legacy (pre-sort) cursor is base64("RFC3339Nano,uuid[,binding]"), with
	// no "v2" prefix segment: decoding it as a v2 cursor must fail rather
	// than silently misparse the timestamp field into the wrong slot.
	legacy := "MjAyNi0wMS0wMVQwMDowMDowMFosYWJjLGJpbmQ=" // arbitrary base64, no v2 marker
	if _, err := DecodeAgentCursor(legacy, "updated", "desc", "bind"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("legacy cursor: err = %v, want ErrInvalidInput", err)
	}
}

func TestAgentCursor_MalformedBase64Rejected(t *testing.T) {
	if _, err := DecodeAgentCursor("not-valid-base64!!!", "updated", "desc", "bind"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("malformed cursor: err = %v, want ErrInvalidInput", err)
	}
}

func TestAgentCursor_TamperedByteRejected(t *testing.T) {
	k := time.Now().UTC()
	cursor := EncodeAgentCursor("updated", "desc", k, k, "agent-1", "bind")
	tampered := "X" + cursor[1:]
	if _, err := DecodeAgentCursor(tampered, "updated", "desc", "bind"); err == nil {
		t.Fatalf("tampered cursor decoded without error")
	}
}

func TestAgentCursor_BindingMayContainCommas(t *testing.T) {
	// The binding is an opaque token (in practice a base64 hash, which never
	// contains a raw comma, but the codec must not assume that).
	k := time.Now().UTC()
	binding := "part,with,commas"
	cursor := EncodeAgentCursor("updated", "desc", k, k, "agent-1", binding)
	decoded, err := DecodeAgentCursor(cursor, "updated", "desc", binding)
	if err != nil {
		t.Fatalf("DecodeAgentCursor: %v", err)
	}
	if decoded.ID != "agent-1" {
		t.Fatalf("ID = %q, want agent-1", decoded.ID)
	}
}
