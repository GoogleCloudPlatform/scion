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

package cmd

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// brokerStorageBucket returns the GCS bucket the runtime broker falls back
// to when a workspace transfer request names none: the server's
// storage.bucket (or --storage-bucket), unless storage is the local
// provider, whose bucket is not a GCS bucket. A hub sends the bucket with
// each create that carries a workspace upload, so this only matters for
// requests from hubs that predate that (ptone/scion#3422).
func brokerStorageBucket(storage config.StorageConfig) string {
	if strings.EqualFold(storage.Provider, "local") {
		return ""
	}
	return storage.Bucket
}
