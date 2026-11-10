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
	"errors"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
)

func TestDecryptFailureKind(t *testing.T) {
	for want, err := range map[string]error{
		FailureUnknownVersion:       secretenvelope.ErrUnknownVersion,
		FailureMalformed:            fmt.Errorf("wrapped: %w", secretenvelope.ErrMalformed),
		FailureMissingKeyID:         secretenvelope.ErrMissingKeyID,
		FailureUnknownKeyID:         ErrUnknownKeyID,
		FailureDecrypt:              fmt.Errorf("decrypting secret value: %w", ErrDecrypt),
		FailurePlaintextNotAdmitted: ErrPlaintextNotAdmitted,
		FailureNoDecryptionKey:      ErrNoDecryptionKey,
		FailureUnreadable:           errors.New("something else"),
	} {
		if got := DecryptFailureKind(err); got != want {
			t.Errorf("DecryptFailureKind(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestResolveDecryptError_SortedAndTyped(t *testing.T) {
	err := newResolveDecryptError([]ResolveFailure{
		{Key: "B", Scope: "user", Kind: FailureDecrypt},
		{Key: "A", Scope: "user", Kind: FailureUnknownKeyID},
		{Key: "Z", Scope: "hub", Kind: FailureMalformed},
	})
	want := ErrResolveDecrypt.Error() + ": hub/Z (malformed), user/A (unknown_key_id), user/B (decrypt_failed)"
	if err.Error() != want {
		t.Fatalf("Error() = %q\nwant %q", err.Error(), want)
	}
	if !errors.Is(err, ErrResolveDecrypt) || errors.Is(err, ErrDecrypt) {
		t.Fatal("ResolveDecryptError must match ErrResolveDecrypt only")
	}
}
