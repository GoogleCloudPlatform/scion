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

// chatOperations lists the catalog operations for chat.
var chatOperations = []OperationSpec{
	// =====================================================================
	// Domain: chat — chat access (project-scoped)
	// =====================================================================
	{
		ID:          "chat.access",
		Domain:      "chat",
		Description: "Access chat threads, spaces, topics, and messages within a project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/prefs", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/prefs", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/spaces", Method: "GET"},
			// handleChatSpaceRoutes requires a sub-action after the space
			// ID; there is no bare GET "/chat/spaces/{id}".
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/spaces/{id}/threads", Method: "GET"},
			// handleChatConversationRoutes requires a sub-action after the
			// conversation key; there is no bare GET
			// "/chat/conversations/{id}".
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{id}/messages", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/topics/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/dms", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/unread-count", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/search", Method: "GET"},
			// handleChatAttachments (upload) is POST-only; only the
			// by-ID download route (handleChatAttachmentByID) is GET.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/attachments", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/attachments/{id}", Method: "GET"},
		},
		Principals:  []PrincipalKind{PrincipalUser},
		Credentials: []CredentialKind{CredentialSessionJWT},
		// The project is resolved from the addressed row: a topic key from
		// the topic, an attachment from the file (its uploader, its
		// project, or a readable message it is attached to), and a DM key
		// from its participants. A reference inside a request (reply
		// target, read marker, attachment) must belong to the same
		// conversation. On the conversation routes a refusal answers as
		// not found (Thread / Attachment not found); the reason is logged.
		ResourceResolver: "project-from-row",
		BasePermission:   "project.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden, DenialResourceNotFound},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
}
