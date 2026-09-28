// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"net/url"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const defaultListPageSize = 50

func listOptionsFromQuery(query url.Values) store.ListOptions {
	limit := defaultListPageSize
	if parsed, err := strconv.Atoi(query.Get("limit")); err == nil && parsed > 0 {
		limit = parsed
	}

	return store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	}
}
