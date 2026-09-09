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

// Package authzop defines the authorization operation contract vocabulary.
//
// An authorization operation is the unit of audit: not a permission string,
// URL pattern, handler file, or UI control. Each OperationSpec declares the
// complete proof obligation for one security-meaningful action, including
// entry points, admitted principals, base permissions, typed security effects,
// delegation requirements, target-governance rules, post-state invariants,
// audit obligations, and stable public denial codes.
//
// This package is intentionally dependency-light. It imports nothing from
// pkg/hub, pkg/store, or any domain package, so handlers, catalog code, and
// reference slices can consume it without import cycles. Domain-specific
// governance implementations live in their respective packages and reference
// the vocabulary defined here.
//
// All types use deterministic validation with fail-closed invalid-spec
// behavior: an OperationSpec that fails Validate() must not be accepted
// into any catalog or enforcement path.
package authzop
