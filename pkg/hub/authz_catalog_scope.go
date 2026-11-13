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
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// catalogWriteAction maps a CRUD action to its global-catalog twin when the
// target catalog record lives in the hub (global) catalog. Project- and
// user-scoped records keep their plain action unchanged: always key off the
// stored scope, never parentlessness (user-scoped records are also parentless
// and must not require *_global permissions).
//
// Phase 1 covers skill.*_global twins (create_global, update_global,
// delete_global), while keeping the interim core-scope mapping (create ->
// create_global, update/delete -> plain) until core retirement lands.
// Template twins are added in Phase 2.
func catalogWriteAction(kind, scope string, a Action) Action {
	switch kind {
	case permissions.ResourceSkill:
		switch scope {
		case store.SkillScopeGlobal:
			switch a {
			case ActionCreate:
				return ActionCreateGlobal
			case ActionUpdate:
				return ActionUpdateGlobal
			case ActionDelete:
				return ActionDeleteGlobal
			}
		case store.SkillScopeCore:
			if a == ActionCreate {
				return ActionCreateGlobal
			}
		}
	case permissions.ResourceTemplate:
		if scope == store.TemplateScopeGlobal {
			switch a {
			case ActionCreate:
				return ActionCreateGlobal
			case ActionUpdate:
				return ActionUpdateGlobal
			case ActionDelete:
				return ActionDeleteGlobal
			}
		}
	}
	return a
}
