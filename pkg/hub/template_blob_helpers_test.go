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

package hub

import (
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// commitHash is the manifest hash of content.
func commitHash(content string) string {
	return transfer.HashBytes([]byte(content))
}

// blobObjectPath is the object path of content's blob under a blob-layout
// template's storage path.
func blobObjectPath(t *store.Template, content string) string {
	hex, _ := templateBlobHex(commitHash(content))
	return templateBlobPath(t.StoragePath, hex)
}
