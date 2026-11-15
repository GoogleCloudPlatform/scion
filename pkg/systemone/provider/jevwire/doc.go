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

// Package jevwire implements the shared codec for the Jev wire protocol family
// (POST /v1/systemone and optional GET /v1/models) and registers the TypeSafe
// hosted reference profile ("typesafe") in pkg/systemone/provider.
//
// Profiles share wire framing while declaring distinct implementation names,
// capability limits, confidence metric namespaces, truncation rules, and model
// alias tables (provider-design §7.1, §7.4).
//
// Contract notes and unverified vendor documentation items (U1–U10, provider-design §7.4):
//   - U1 (alias echo): TypeSafe models.md states the response model is the exact
//     version behind an alias, whereas OpenAPI 0.2.0 examples echo "jev-latest".
//     ResolvedModel is recorded verbatim and NoteModelAliasReported is added
//     whenever it matches a known alias ("jev-latest", "jev-preview").
//   - U2 (401/429/529 error body shapes): only 400 and 422 error schemas are
//     published in OpenAPI 0.2.0; error bodies are never copied into provenance,
//     and 400 bodies are inspected solely for the "max_tokens_exceeded" token.
//   - U3 (probability sum tolerance): distributions must match declared option
//     IDs without repair or renormalization and sum to 1 ± 1e-3.
//   - U4 (confidence formula): ChoiceAnswer "confidence" is recorded solely as
//     native metric "typesafe.confidence" (Source "provider-doc"); formula
//     deviation from (n*p_max-1)/(n-1) is noted as NoteConfidenceFormulaDeviation
//     rather than treated as a validation failure.
//   - U5–U10 (/v1/models billing, retention terms, org rate limits, latency,
//     option-order lean, and adversarial state injection) remain unverified
//     documentation claims pending live probe and replay evaluation.
package jevwire
