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

package keyring

import (
	"fmt"
	"strings"
)

// CoupledUses names every consumer of the single legacy server secret
// (design "Existing consumer map"). Changing that secret invalidates or
// strands all of them at once.
var CoupledUses = []string{
	"web session cookies (including OAuth login state)",
	"agent JWTs",
	"user access, refresh, CLI and test-login JWTs",
	"local skill-file download URLs",
	"artifact view links",
	"authorized-list cursors",
	"attachment delivery URLs",
	"conduit admission tokens",
	"relay-peer request signatures (shared-secret fallback)",
	"local secret backend values",
	"signing-key, OIDC key and conduit grant-ring backups",
}

// StartupNotice is the non-secret startup notice for k: IDs, revision and
// limits only, never key material or deadlines.
func StartupNotice(k *Keyring) string {
	if k.Source() == SourceLegacy {
		return fmt.Sprintf("server secret: legacy single-secret mode (signing id %q, encryption id %q). "+
			"This one secret keys: %s. Changing it invalidates or strands all of them together; "+
			"safe server-secret rotation is not available in this release.",
			LegacyID, LegacyID, strings.Join(CoupledUses, "; "))
	}
	return fmt.Sprintf("server secret: keyring file revision %q (experimental signing pilot): "+
		"web session cookies, agent and user JWTs, skill file download URLs, artifact view links, authorized-list cursors and "+
		"attachment delivery URLs use signing generations %v (active %q); every other purpose still uses the "+
		"unchanged %q entry, whose deadlines apply to session cookies, JWTs and those capabilities only, and encryption keeps the "+
		"imported key %q. Full server-secret rotation is not available in this release.",
		k.Revision(), k.Signing().IDs(), k.Signing().ActiveID(), LegacyID, k.Encryption().ActiveID())
}
