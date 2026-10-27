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

// ghTreeTestSingleFile is a git trees API response listing one regular,
// non-executable SKILL.md, the listing most fake GitHub servers serve.
const ghTreeTestSingleFile = `{"sha":"t","truncated":false,"tree":[{"path":"SKILL.md","mode":"100644","type":"blob","sha":"x","size":1}]}`

// ghTreeTestPath is the request path of the recursive git tree of skillPath
// at commitSHA, as ghListTree sends it (the query string is not part of a
// ServeMux pattern).
func ghTreeTestPath(owner, repo, commitSHA, skillPath string) string {
	return "/repos/" + owner + "/" + repo + "/git/trees/" + commitSHA + ":" + skillPath
}
