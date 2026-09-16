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

//go:build !no_embed_web

package web

import "embed"

// ClientAssets contains the built web client assets embedded at compile time.
// Build the client first (cd web && npm run build:client) before compiling
// without the no_embed_web tag.
//
//go:embed all:dist/client
var ClientAssets embed.FS

// AssetsEmbedded indicates whether client assets are embedded in the binary.
var AssetsEmbedded = true
