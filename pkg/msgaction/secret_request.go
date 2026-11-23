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

package msgaction

import "time"

// Secret request kind constants and kind-registered event/conflict codes (§4.6, §4.10, §4.11).
const (
	KindSecretRequest           = "secret_request"
	SecretRequestResolvedStatus = "fulfilled"
	SecretRequestClaimedStatus  = "fulfilling"
	SecretRequestClaimTTL       = 2 * time.Minute
	SecretRequestTTL            = 7 * 24 * time.Hour

	// Surfaces accepted on fulfill and dismiss.
	SecretRequestSurfaceNative         = "native"
	SecretRequestSurfaceStandalonePage = "standalone_page"

	// Kind-registered audit event types.
	EventFulfilled      = "fulfilled"
	EventClaimRecovered = "claim_recovered"

	// Kind-registered 409 conflict codes.
	ConflictAlreadyFulfilled        = "already_fulfilled"
	ConflictSecretRequestKeyPending = "secret_request_key_pending"
	ConflictSecretRequestSubmitting = "secret_request_submitting"
	ConflictDismissChanged          = "dismiss_changed"
)

// SecretRequestKindSpec is the KindSpec for agent-to-human secret requests.
var SecretRequestKindSpec = KindSpec{
	Name:                    KindSecretRequest,
	ResolvedStatus:          SecretRequestResolvedStatus,
	ClaimedStatus:           SecretRequestClaimedStatus,
	ResolvedConflictCode:    ConflictAlreadyFulfilled,
	UsesClaim:               true,
	ClaimRecovery:           ClaimRecoveryToPending,
	ClaimTTL:                SecretRequestClaimTTL,
	Expires:                 SecretRequestTTL,
	DismissMinAge:           0,
	RepostTerminalOK:        true,
	ReplaceResolvedOnUpdate: false,
	EventTypes:              []string{EventFulfilled, EventClaimRecovered},
	ConflictCodes:           []string{ConflictSecretRequestKeyPending, ConflictSecretRequestSubmitting, ConflictDismissChanged},
	DetailKeys:              []string{"secret_type", "target", "secret_version"},
}

func init() {
	RegisterKind(SecretRequestKindSpec)
}
