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

package version

// DowngradeFloor is the oldest release this binary lets the hub install.
// The running binary refuses older targets (below_downgrade_floor), so the
// check works without older binaries knowing about it.
//
// Bump it, in the same PR, to the first release that contains any schema
// change an older binary cannot ignore: a dropped table or column (see the
// drop block in pkg/ent/entc/client.go AutoMigrate), a new NOT NULL column
// without a default, a narrowed enum, or rewritten values. Releases older
// than the floor would run on a schema they do not understand.
//
// The value is a release tag. A nightly tag carries its date, which orders
// it against releases of other channels by their publish date.
const DowngradeFloor = "nightly-20261009"
