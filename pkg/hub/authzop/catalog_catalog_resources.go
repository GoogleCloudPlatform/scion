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

package authzop

// catalogResourceOperations lists the catalog operations for skills, templates and harness configurations.
var catalogResourceOperations = []OperationSpec{
	// =====================================================================
	// Domain: skill — skill CRUD
	// =====================================================================
	{
		ID:          "skill.read",
		Domain:      "skill",
		Description: "Read skill definitions or list/discover skills",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}", Method: "GET"},
			// handleSkillsDiscoverDirectory (handlers_skills_discover.go)
			// is POST-only: it reads a filter/query body, it does not list
			// via query string.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/discover-directory", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "skill.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "skill.create",
		Domain:      "skill",
		Description: "Create a new skill definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-body",
		BasePermission:   "skill.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "skill.update",
		Domain:      "skill",
		Description: "Update an existing skill definition",
		EntryPoints: []EntryPoint{
			// handleSkillByID (skill_handlers.go) dispatches on r.Method:
			// PATCH, not PUT.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}", Method: "PATCH"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "skill-from-url",
		BasePermission:   "skill.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
		Bearer:           AdmitOn(BearerTargetCatalogRecord, BearerBoundaryProject, BearerBoundaryHub),
	},
	{
		ID:          "skill.delete",
		Domain:      "skill",
		Description: "Delete a skill definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "skill-from-url",
		BasePermission:   "skill.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "skill.delete",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"skill_id", "skill_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
		Bearer:      AdmitOn(BearerTargetCatalogRecord, BearerBoundaryProject, BearerBoundaryHub),
	},
	{
		ID:          "skill.register",
		Domain:      "skill",
		Description: "Register skills in a skill registry",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries/{id}", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "skill.register",
		Effects:          []SecurityEffect{EffectCreateResource, EffectUpdateResource, EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "skill.register",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"registry_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: template — template CRUD
	// =====================================================================
	{
		ID:          "template.read",
		Domain:      "template",
		Description: "Read template definitions or discover available templates",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}", Method: "GET"},
			// handleResourcesDiscover (handlers_resource_import.go) is
			// POST-only: it reads a discovery filter body.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/resources/discover", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "template.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "template.create",
		Domain:      "template",
		Description: "Create a new template or import resources",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/resources/import", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-body",
		BasePermission:   "template.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "template.update",
		Domain:      "template",
		Description: "Update an existing template definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "template-from-url",
		BasePermission:   "template.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
		Bearer:           AdmitOn(BearerTargetCatalogRecord, BearerBoundaryProject, BearerBoundaryHub),
	},
	{
		ID:          "template.delete",
		Domain:      "template",
		Description: "Delete a template definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "template-from-url",
		BasePermission:   "template.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "template.delete",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"template_id", "template_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: harnessconfig — harness configuration CRUD
	// =====================================================================
	{
		ID:          "harnessconfig.read",
		Domain:      "harnessconfig",
		Description: "Read harness configurations or list available configs",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "harness_config.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
		Bearer:           AdmitOn(BearerTargetCatalogRecord, BearerBoundaryProject, BearerBoundaryHub),
	},
	{
		ID:          "harnessconfig.create",
		Domain:      "harnessconfig",
		Description: "Create a new harness configuration",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-body",
		BasePermission:   "harness_config.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "harnessconfig.update",
		Domain:      "harnessconfig",
		Description: "Update a harness configuration",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "harnessconfig-from-url",
		BasePermission:   "harness_config.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
		Bearer:           AdmitOn(BearerTargetCatalogRecord, BearerBoundaryProject, BearerBoundaryHub),
	},
	{
		ID:          "harnessconfig.delete",
		Domain:      "harnessconfig",
		Description: "Delete a harness configuration",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "harnessconfig-from-url",
		BasePermission:   "harness_config.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "harnessconfig.delete",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"config_id", "config_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
}
