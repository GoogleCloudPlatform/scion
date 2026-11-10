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

package maintenance

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
)

// TestSecretMigrationExecutor_RefusesWithoutCipher checks that the
// migration never runs without the local cipher, so it can never fall back
// to copying stored at-rest forms into Secret Manager. The cipher check
// comes before any store or Secret Manager access, so neither is needed.
func TestSecretMigrationExecutor_RefusesWithoutCipher(t *testing.T) {
	gcp := secret.NewGCPBackendWithClient(nil, nil, "test-project", "test-hub-id")
	executor := NewSecretMigrationExecutor(nil, gcp, nil)

	var log bytes.Buffer
	err := executor.Run(context.Background(), &log, map[string]string{})
	if !errors.Is(err, secret.ErrCipherNotConfigured) {
		t.Fatalf("err = %v, want secret.ErrCipherNotConfigured", err)
	}
}
