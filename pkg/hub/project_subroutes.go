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

import "strings"

// projectSubRouteKind names the handler family a project sub-route
// segment dispatches to in handleProjectRoutes.
type projectSubRouteKind string

const (
	projectSubRouteMembers           projectSubRouteKind = "members"
	projectSubRouteOwnership         projectSubRouteKind = "ownership"
	projectSubRouteAgents            projectSubRouteKind = "agents"
	projectSubRouteEnv               projectSubRouteKind = "env"
	projectSubRouteSecrets           projectSubRouteKind = "secrets"
	projectSubRouteProviders         projectSubRouteKind = "providers"
	projectSubRouteSharedDirs        projectSubRouteKind = "shared-dirs"
	projectSubRouteInjectedSkills    projectSubRouteKind = "injected-skills"
	projectSubRouteGCPServiceAccount projectSubRouteKind = "gcp-service-accounts"
	projectSubRouteMessageLogs       projectSubRouteKind = "message-logs"
	projectSubRouteBroadcast         projectSubRouteKind = "broadcast"
	projectSubRouteSchedules         projectSubRouteKind = "schedules"
	projectSubRouteMetrics           projectSubRouteKind = "metrics"
	projectSubRoutePreStartHooks     projectSubRouteKind = "pre-start-hooks"
	projectSubRouteSettings          projectSubRouteKind = "settings"
	projectSubRouteMessagingPolicy   projectSubRouteKind = "messaging-policy"
	projectSubRouteTemplate          projectSubRouteKind = "template"
	projectSubRouteCatalogImport     projectSubRouteKind = "catalog-import"
	projectSubRouteWorkspace         projectSubRouteKind = "workspace"
	projectSubRouteGitHub            projectSubRouteKind = "github"
)

// projectSubRouteTable lists every first path segment after
// /api/v1/projects/{id}/ that handleProjectRoutes dispatches. It is the
// complete project sub-route list: handleProjectRoutes answers 404 for any
// other first segment before dispatching, so a new branch in the
// dispatcher needs a row here, and TestBearerDisposition_EveryRoutePatternCovered
// then requires the new segment to be catalogued or pending.
var projectSubRouteTable = map[string]projectSubRouteKind{
	"members":                  projectSubRouteMembers,
	"transfer-ownership":       projectSubRouteOwnership,
	"agents":                   projectSubRouteAgents,
	"env":                      projectSubRouteEnv,
	"secrets":                  projectSubRouteSecrets,
	"providers":                projectSubRouteProviders,
	"shared-dirs":              projectSubRouteSharedDirs,
	"injected-skills":          projectSubRouteInjectedSkills,
	"gcp-service-accounts":     projectSubRouteGCPServiceAccount,
	"message-logs":             projectSubRouteMessageLogs,
	"broadcast":                projectSubRouteBroadcast,
	"scheduled-events":         projectSubRouteSchedules,
	"schedules":                projectSubRouteSchedules,
	"metrics-summary":          projectSubRouteMetrics,
	"metrics":                  projectSubRouteMetrics,
	"pre-start-hooks":          projectSubRoutePreStartHooks,
	"settings":                 projectSubRouteSettings,
	"messaging-policy":         projectSubRouteMessagingPolicy,
	"set-template":             projectSubRouteTemplate,
	"clone":                    projectSubRouteTemplate,
	"discover-templates":       projectSubRouteCatalogImport,
	"discover-harness-configs": projectSubRouteCatalogImport,
	"import-templates":         projectSubRouteCatalogImport,
	"import-harness-configs":   projectSubRouteCatalogImport,
	"dav":                      projectSubRouteWorkspace,
	"sync":                     projectSubRouteWorkspace,
	"workspace":                projectSubRouteWorkspace,
	"github-installation":      projectSubRouteGitHub,
	"github-status":            projectSubRouteGitHub,
	"github-permissions":       projectSubRouteGitHub,
	"git-identity":             projectSubRouteGitHub,
}

// projectSubRouteListed reports whether subPath (the path after
// /api/v1/projects/{id}/) is the project resource itself or starts with a
// segment listed in projectSubRouteTable.
func projectSubRouteListed(subPath string) bool {
	if subPath == "" {
		return true
	}
	seg, _, _ := strings.Cut(subPath, "/")
	_, ok := projectSubRouteTable[seg]
	return ok
}
