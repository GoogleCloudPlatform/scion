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
	"errors"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/credentialmeta"
)

var (
	_ credentialmeta.Kind = CredentialKindInteractive
	_ CredentialKind      = credentialmeta.KindInteractive
)

func TestCredentialKindsAliasCanonicalContract(t *testing.T) {
	t.Parallel()

	serverKinds := []credentialmeta.Kind{
		CredentialKindInteractive,
		CredentialKindUAT,
		CredentialKindAgentJWT,
		CredentialKindFederation,
		CredentialKindBroker,
		CredentialKindDev,
	}
	if want := credentialmeta.Kinds(); !reflect.DeepEqual(serverKinds, want) {
		t.Fatalf("server credential kinds = %v, canonical kinds = %v", serverKinds, want)
	}

	_, err := credentialmeta.NewRef(credentialmeta.RefInput{Kind: CredentialKind("unknown")})
	var validationErr *credentialmeta.ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "kind" {
		t.Fatalf("unknown credential kind error = %v, want typed kind rejection", err)
	}
}
