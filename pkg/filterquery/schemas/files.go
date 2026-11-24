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

package schemas

import "github.com/GoogleCloudPlatform/scion/pkg/filterquery"

// Files is the filter query schema for the file browser view.
// Text matching keeps the existing regex -> fuzzy fallback on both client and
// server (?q= receives the text part only); key terms post-filter the merged
// results on the client.
var Files = filterquery.Schema{
	View:     "files",
	TextHelp: "Matches file path by regex, falling back to fuzzy match",
	Keys: []filterquery.Key{
		{
			Name:        "ext",
			Help:        "File extension without leading dot (e.g. ts, go)",
			Kind:        filterquery.KindString,
			Exec:        filterquery.ExecClient,
			MultiValued: false,
		},
	},
}
