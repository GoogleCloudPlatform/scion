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

package api

// WarningEmptyPerAgentWorkspaceFilesIgnored is returned in the agent create
// response's warnings when workspace files are sent for an empty-per-agent
// project: the hub drops them because each agent starts in an empty
// directory. The CLI matches on this exact text to report that the files
// were ignored, so the hub and CLI must share this constant.
const WarningEmptyPerAgentWorkspaceFilesIgnored = "workspace files were ignored: this project gives each agent an empty workspace directory"
