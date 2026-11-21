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

// Package source defines the provider-free interfaces and record types for
// app-shaped external trigger sources (such as GitHub Apps, Jira, or GitLab).
//
// It intentionally imports only the Go standard library and never imports any
// provider package (such as pkg/hub/trigger/github or pkg/hub/githubapp) or
// pkg/hub. An import-boundary guard test enforces this invariant.
//
// See Triggers Stage 3 design §16 item 17 and app-shaped-source-seams.md §1–§2.
package source
