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

// Package artifacts is the scion artifact service: published files and
// bundles with stable, versioned references, owned by a principal and homed
// in a scope.
//
// The package is self-contained so that it can be compiled into the hub
// binary today and linked into a standalone server later. It must never import
// pkg/hub or any other hub package. Everything it needs to know about the
// caller comes through the Host interface, which carries strings only, so a
// later standalone deployment can answer the same two questions over a wire
// protocol without changing this package.
//
// The hub mounts the service under /api/v1/artifacts. While the hub.artifacts
// experiment is off, every route answers 404. Phase P0 adds the scaffolding
// only, so every route also answers 404 while the experiment is on.
package artifacts
