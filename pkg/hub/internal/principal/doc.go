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

// Package principal holds the Scion Hub's principal types: the Identity
// interfaces and their local user implementations, request credential
// metadata (CredentialContext, CredentialDecoration, TokenBoundary), and the
// request-context accessors and setters for them.
//
// The package is internal to pkg/hub. Its exported names exist so package
// hub can use them across the package boundary; package hub re-exports what
// its callers need through aliases (zz_alias_principal.go). The context keys
// stay unexported here, so values can be stored only through the setters,
// whose callers package hub pins (TestPrincipalContextSetterFileAllowList).
//
// Ancestry attestation is not decided here: AncestryIsHubAttested stays in
// package hub, which lists the local user types of this package explicitly
// (pkg/hub/identity_attestation.go).
package principal
