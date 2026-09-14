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

package hub

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

func (s *Server) setHubSecret(ctx context.Context, name, value, description, userID string) error {
	if s.secretBackend != nil {
		_, _, err := s.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name:          name,
			Value:         value,
			SecretType:    secret.TypeVariable,
			Scope:         store.ScopeHub,
			ScopeID:       s.hubID,
			Description:   description,
			InjectionMode: "as_needed",
			CreatedBy:     userID,
			UpdatedBy:     userID,
		})
		return err
	}

	_, err := s.store.UpsertSecret(ctx, &store.Secret{
		ID:             uuid.NewString(),
		Key:            name,
		EncryptedValue: value,
		Scope:          store.ScopeHub,
		ScopeID:        s.hubID,
		SecretType:     store.SecretTypeVariable,
		Description:    description,
		Version:        1,
		CreatedBy:      userID,
		UpdatedBy:      userID,
	})
	return err
}
