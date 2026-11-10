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
	"context"
	"errors"
	"fmt"
	"strings"
)

// HoldPending implements PendingHolder. The local backend holds the value
// inline: the reference is the value sealed by the backend's ValueCipher
// (the same envelope as a stored secret value), so the pending row itself
// carries it and nothing else is stored.
func (b *LocalBackend) HoldPending(_ context.Context, _ string, value []byte) (string, error) {
	if len(value) == 0 {
		return "", errors.New("pending secret value is empty")
	}
	ref, err := b.cipher.Encrypt(string(value))
	if err != nil {
		return "", fmt.Errorf("encrypting pending secret value: %w", err)
	}
	return ref, nil
}

// ReadPending implements PendingHolder: it opens a reference written by
// HoldPending. A Secret Manager reference is refused rather than read as a
// plaintext value.
func (b *LocalBackend) ReadPending(_ context.Context, ref string) ([]byte, error) {
	if ref == "" || strings.HasPrefix(ref, "gcpsm:") {
		return nil, errors.New("pending secret reference is not a local reference")
	}
	pt, err := b.cipher.Decrypt(ref)
	if err != nil {
		return nil, fmt.Errorf("decrypting pending secret value: %w", err)
	}
	return []byte(pt), nil
}

// DropPending implements PendingHolder. The local backend holds the value
// on the pending row, so deleting the row releases it; there is nothing
// else to drop.
func (b *LocalBackend) DropPending(context.Context, string) error {
	return nil
}

// HoldPending implements PendingHolder. Not yet supported on Secret Manager.
func (b *GCPBackend) HoldPending(context.Context, string, []byte) (string, error) {
	return "", ErrPendingNotImplemented
}

// ReadPending implements PendingHolder. Not yet supported on Secret Manager.
func (b *GCPBackend) ReadPending(context.Context, string) ([]byte, error) {
	return nil, ErrPendingNotImplemented
}

// DropPending implements PendingHolder. Not yet supported on Secret Manager.
func (b *GCPBackend) DropPending(context.Context, string) error {
	return ErrPendingNotImplemented
}
