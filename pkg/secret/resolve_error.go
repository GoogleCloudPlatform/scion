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
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
)

// ErrResolveDecrypt matches (errors.Is) a *ResolveDecryptError: secret
// resolution failed because stored values could not be decrypted.
var ErrResolveDecrypt = errors.New("secret resolution failed: stored secret values could not be decrypted")

// Stable classifications of a decrypt failure (ResolveFailure.Kind). They
// never carry decoder text, key material or values.
const (
	FailureUnknownVersion       = "unknown_version"
	FailureMalformed            = "malformed"
	FailureMissingKeyID         = "missing_key_id"
	FailureUnknownKeyID         = "unknown_key_id"
	FailureDecrypt              = "decrypt_failed"
	FailurePlaintextNotAdmitted = "plaintext_not_admitted"
	FailureNoDecryptionKey      = "no_decryption_key"
	FailureUnreadable           = "unreadable"
)

// DecryptFailureKind classifies a ValueCipher or envelope error.
func DecryptFailureKind(err error) string {
	switch {
	case errors.Is(err, secretenvelope.ErrUnknownVersion):
		return FailureUnknownVersion
	case errors.Is(err, secretenvelope.ErrMalformed):
		return FailureMalformed
	case errors.Is(err, secretenvelope.ErrMissingKeyID):
		return FailureMissingKeyID
	case errors.Is(err, ErrUnknownKeyID):
		return FailureUnknownKeyID
	case errors.Is(err, ErrDecrypt):
		return FailureDecrypt
	case errors.Is(err, ErrPlaintextNotAdmitted):
		return FailurePlaintextNotAdmitted
	case errors.Is(err, ErrNoDecryptionKey):
		return FailureNoDecryptionKey
	default:
		return FailureUnreadable
	}
}

// ResolveFailure identifies one secret Resolve could not decrypt: its key
// name, scope and failure classification. No value, no key material.
type ResolveFailure struct {
	Key   string
	Scope string
	Kind  string
}

// ResolveDecryptError is the typed error of a resolution that met
// undecryptable stored values. It names the affected secret keys so an
// operator can act, never their values.
type ResolveDecryptError struct {
	Failures []ResolveFailure // sorted by scope, key
}

func newResolveDecryptError(f []ResolveFailure) *ResolveDecryptError {
	out := append([]ResolveFailure(nil), f...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Key < out[j].Key
	})
	return &ResolveDecryptError{Failures: out}
}

// Error lists scope/key (classification) for every failure.
func (e *ResolveDecryptError) Error() string {
	parts := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		parts[i] = fmt.Sprintf("%s/%s (%s)", f.Scope, f.Key, f.Kind)
	}
	return fmt.Sprintf("%s: %s", ErrResolveDecrypt.Error(), strings.Join(parts, ", "))
}

// Is matches ErrResolveDecrypt.
func (e *ResolveDecryptError) Is(target error) bool { return target == ErrResolveDecrypt }
