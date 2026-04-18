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

// Package entstore is the destination package for the Ent-backed store that
// will replace pkg/store/sqlite during the multi-phase Ent migration. At the
// moment the package only hosts the schema-coverage gate (see
// schemacoverage_test.go, build tag schemadiff); the EntStore type itself is
// introduced in Phase 4.
package entstore
