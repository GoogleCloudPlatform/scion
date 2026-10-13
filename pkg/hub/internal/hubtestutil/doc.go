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

// Package hubtestutil holds generic test helpers for pkg/hub and its
// subpackages: deterministic test IDs, in-memory SQLite, serialised schema
// migration, the TestMain memory and leak guards, hermetic git runners and
// small fault-injection and buffer types.
//
// Only test code imports it. It must never import pkg/hub
// (no_hub_import_test.go enforces this), so that future pkg/hub/<area>
// packages, whose tests cannot import pkg/hub, can use it. The pkg/hub tests
// reach these helpers under their old names through
// pkg/hub/zz_alias_hubtestutil_test.go.
//
// The declarations were moved verbatim from pkg/hub's *_helpers_test.go files
// by hack/hubshard/extract export (family
// hack/hubshard/extract/families/hubtestutil-wave1.txt); only the names that
// pkg/hub uses were exported. A separate comment-only commit then updated
// their doc comments to the exported names.
package hubtestutil
