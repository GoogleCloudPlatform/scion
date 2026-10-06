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
	"strings"
	"testing"
	"time"
)

func TestAgentCursor_RoundTrip(t *testing.T) {
	k := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cursor := EncodeAgentCursor("updated", "desc", k, created, testCursorID, "bind123")

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
	if decoded.ID != testCursorID {
		t.Errorf("ID = %q, want %s", decoded.ID, testCursorID)
	}
}

func TestAgentCursor_SortMismatchRejected(t *testing.T) {
	k := time.Now().UTC()
	cursor := EncodeAgentCursor("updated", "desc", k, k, testCursorID, "bind")
	if _, err := DecodeAgentCursor(cursor, "created", "desc", "bind"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("sort mismatch: err = %v, want ErrInvalidInput", err)
	}
}

func TestAgentCursor_DirMismatchRejected(t *testing.T) {
	k := time.Now().UTC()
	cursor := EncodeAgentCursor("updated", "desc", k, k, testCursorID, "bind")
	if _, err := DecodeAgentCursor(cursor, "updated", "asc", "bind"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("dir mismatch: err = %v, want ErrInvalidInput", err)
	}
}

func TestAgentCursor_BindingMismatchRejected(t *testing.T) {
	k := time.Now().UTC()
	cursor := EncodeAgentCursor("updated", "desc", k, k, testCursorID, "bind-a")
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
	cursor := EncodeAgentCursor("updated", "desc", k, k, testCursorID, "bind")
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
	cursor := EncodeAgentCursor("updated", "desc", k, k, testCursorID, binding)
	decoded, err := DecodeAgentCursor(cursor, "updated", "desc", binding)
	if err != nil {
		t.Fatalf("DecodeAgentCursor: %v", err)
	}
	if decoded.ID != testCursorID {
		t.Fatalf("ID = %q, want %s", decoded.ID, testCursorID)
	}
}

// testCursorID is a well-formed agent id for cursor round trips.
const testCursorID = "6f1c2a3e-4b5d-4c6e-8f70-123456789abc"

func TestAgentCursor_NonUUIDIDRejected(t *testing.T) {
	// The binding, sort and dir all match, so only the id is wrong: a
	// non-UUID id must be rejected as invalid input before it reaches SQL.
	k := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const canonical = "6f1c2a3e-4b5d-4c6e-8f70-1a2b3c4d5e6f"
	for _, id := range []string{
		"agent-1", "", "not-a-uuid", "6f1c2a3e-4b5d-4c6e-8f70",
		// Non-canonical forms that uuid.Parse accepts.
		"urn:uuid:" + canonical,
		"{" + canonical + "}",
		strings.ReplaceAll(canonical, "-", ""),
		strings.ToUpper(canonical),
	} {
		cursor := EncodeAgentCursor("updated", "desc", k, k, id, "bind")
		if _, err := DecodeAgentCursor(cursor, "updated", "desc", "bind"); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("id %q: err = %v, want ErrInvalidInput", id, err)
		}
	}

	// The canonical lowercase dashed form is accepted unchanged.
	cursor := EncodeAgentCursor("updated", "desc", k, k, canonical, "bind")
	got, err := DecodeAgentCursor(cursor, "updated", "desc", "bind")
	if err != nil {
		t.Fatalf("canonical id: err = %v, want nil", err)
	}
	if got.ID != canonical {
		t.Errorf("canonical id: ID = %q, want %q", got.ID, canonical)
	}
}
