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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// E.1 acceptance-criteria tests (plan §5, E.1 table). Each AC below has both
// a positive and negative behavior-level test. Mint always goes through the
// real HTTP handler (`e1MintTokenViaAPI` → `doRequest`, dev session
// credential). AC1/AC2/the legacy-row case then exercise the token through
// `UserAccessTokenService.ValidateToken` directly, the same production
// derivation path the middleware calls (review finding F4); AC4's
// `TestCredentialDecoration_AC4_MiddlewareCarriesDecorationToLogs` is the one
// test that authenticates through the real `UnifiedAuthMiddleware` end to
// end and inspects both the resulting context and rendered log output.
// ---------------------------------------------------------------------------

// e1MintTokenViaAPI mints a token via the HTTP API using the dev session
// credential, with an arbitrary request body so tests can include E.1's
// purpose/labels fields alongside the base fields.
func e1MintTokenViaAPI(t *testing.T, srv *Server, body map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/tokens", body)
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

// AC1: same human, two UATs -> distinct, immutable credential attribution.
func TestCredentialDecoration_AC1_DistinctImmutableAttribution(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("e1-ac1-p")
	ownerID := tid("e1-ac1-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	codeA, respA := e1MintTokenViaAPI(t, srv, map[string]interface{}{
		"name": "device-a", "projectId": projectID, "scopes": []string{"agent:read"},
	})
	if codeA != http.StatusCreated {
		t.Fatalf("mint device-a failed: %d %+v", codeA, respA)
	}
	codeB, respB := e1MintTokenViaAPI(t, srv, map[string]interface{}{
		"name": "device-b", "projectId": projectID, "scopes": []string{"agent:read"},
	})
	if codeB != http.StatusCreated {
		t.Fatalf("mint device-b failed: %d %+v", codeB, respB)
	}

	keyA := respA["token"].(string)
	keyB := respB["token"].(string)
	tokenIDA := respA["accessToken"].(map[string]interface{})["id"].(string)

	ctx := context.Background()
	idA, err := srv.uatService.ValidateToken(ctx, keyA)
	if err != nil {
		t.Fatalf("ValidateToken(device-a): %v", err)
	}
	idB, err := srv.uatService.ValidateToken(ctx, keyB)
	if err != nil {
		t.Fatalf("ValidateToken(device-b): %v", err)
	}

	decA, decB := idA.Decoration(), idB.Decoration()
	if decA == nil || decB == nil {
		t.Fatalf("expected both identities to carry a decoration: A=%+v B=%+v", decA, decB)
	}
	if decA.TokenID == decB.TokenID {
		t.Fatal("expected distinct token IDs for two tokens minted by the same human")
	}
	if decA.TokenName != "device-a" || decB.TokenName != "device-b" {
		t.Fatalf("token names not preserved: A=%q B=%q", decA.TokenName, decB.TokenName)
	}

	// Revoking tok-A must not affect tok-B's attribution.
	revokeRec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/tokens/"+tokenIDA+"/revoke", nil)
	if revokeRec.Code != http.StatusNoContent {
		t.Fatalf("revoke failed: %d %s", revokeRec.Code, revokeRec.Body.String())
	}

	if _, err := srv.uatService.ValidateToken(ctx, keyA); !errors.Is(err, ErrUATRevoked) {
		t.Fatalf("expected ErrUATRevoked for revoked tok-A, got %v", err)
	}

	idBAfter, err := srv.uatService.ValidateToken(ctx, keyB)
	if err != nil {
		t.Fatalf("tok-B should still validate after tok-A revoke: %v", err)
	}
	decBAfter := idBAfter.Decoration()
	if decBAfter.TokenID != decB.TokenID || decBAfter.TokenName != decB.TokenName {
		t.Fatalf("tok-B's attribution changed after tok-A was revoked: before=%+v after=%+v", decB, decBAfter)
	}

	// Negative: metadata is immutable — there is no update endpoint.
	patchRec := doRequest(t, srv, http.MethodPatch, "/api/v1/auth/tokens/"+tokenIDA,
		map[string]interface{}{"name": "renamed"})
	if patchRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for PATCH (no update endpoint), got %d", patchRec.Code)
	}
}

// AC2: actor-like labels leave identity, capabilities, and the authorization
// decision unchanged; reserved keys are rejected at mint.
func TestCredentialDecoration_AC2_LabelsNeverChangeAuthorization(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("e1-ac2-p")
	ownerID := tid("e1-ac2-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	// Negative: a reserved, actor-shaped label key is rejected at mint.
	code, resp := e1MintTokenViaAPI(t, srv, map[string]interface{}{
		"name": "bad", "projectId": projectID, "scopes": []string{"project:read"},
		"labels": map[string]string{"agent": "nightly"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for reserved label key, got %d: %+v", code, resp)
	}

	codeA, respA := e1MintTokenViaAPI(t, srv, map[string]interface{}{
		"name": "plain", "projectId": projectID, "scopes": []string{"project:read"},
	})
	if codeA != http.StatusCreated {
		t.Fatalf("plain mint failed: %d %+v", codeA, respA)
	}
	codeB, respB := e1MintTokenViaAPI(t, srv, map[string]interface{}{
		"name": "labeled", "projectId": projectID, "scopes": []string{"project:read"},
		// Plan §5 AC2 example: an accepted actor-like label and purpose.
		"purpose": "agent xyz",
		"labels":  map[string]string{"role_hint": "admin"},
	})
	if codeB != http.StatusCreated {
		t.Fatalf("labeled mint failed: %d %+v", codeB, respB)
	}

	ctx := context.Background()
	idPlain, err := srv.uatService.ValidateToken(ctx, respA["token"].(string))
	if err != nil {
		t.Fatalf("ValidateToken(plain): %v", err)
	}
	idLabeled, err := srv.uatService.ValidateToken(ctx, respB["token"].(string))
	if err != nil {
		t.Fatalf("ValidateToken(labeled): %v", err)
	}

	// Identity is unchanged: both credentials authenticate the same human.
	if idPlain.ID() != DevUserID || idLabeled.ID() != DevUserID {
		t.Fatalf("labels changed the authenticated principal: plain=%s labeled=%s", idPlain.ID(), idLabeled.ID())
	}

	res := Resource{Type: "project", ID: projectID}
	decPlain := srv.authzService.CheckAccess(ctx, idPlain, res, ActionRead)
	decLabeled := srv.authzService.CheckAccess(ctx, idLabeled, res, ActionRead)

	if !decPlain.Allowed || !decLabeled.Allowed {
		t.Fatalf("expected both decisions to allow: plain=%+v labeled=%+v", decPlain, decLabeled)
	}
	if decPlain.Reason != decLabeled.Reason ||
		decPlain.BindingID != decLabeled.BindingID ||
		decPlain.RoleName != decLabeled.RoleName ||
		decPlain.Scope != decLabeled.Scope ||
		decPlain.MatchedGrant != decLabeled.MatchedGrant ||
		decPlain.MatchedPolicy != decLabeled.MatchedPolicy ||
		decPlain.PrincipalKind != decLabeled.PrincipalKind ||
		decPlain.CredentialKind != decLabeled.CredentialKind {
		t.Fatalf("labels changed the authorization decision: plain=%+v labeled=%+v", decPlain, decLabeled)
	}
	if decPlain.CredentialID == decLabeled.CredentialID {
		t.Fatal("expected distinct credential IDs between the two tokens")
	}

	capsPlain := srv.authzService.ComputeCapabilities(ctx, idPlain, res)
	capsLabeled := srv.authzService.ComputeCapabilities(ctx, idLabeled, res)
	if !reflect.DeepEqual(capsPlain.Actions, capsLabeled.Actions) {
		t.Fatalf("labels changed computed capabilities: plain=%v labeled=%v", capsPlain.Actions, capsLabeled.Actions)
	}

	// Ancestry trust is unchanged: labels never affect whether an identity's
	// ancestry is hub-attested.
	if AncestryIsHubAttested(idPlain) != AncestryIsHubAttested(idLabeled) {
		t.Fatalf("labels changed ancestry attestation: plain=%v labeled=%v",
			AncestryIsHubAttested(idPlain), AncestryIsHubAttested(idLabeled))
	}

	// Delegation is unchanged: both identities get the same CanDelegate
	// answer for an allowed grant and a denied one. Both grants use an
	// explicit RolePermissions list rather than a real role definition, so
	// the test pins exactly one permission per case instead of depending on
	// a seeded role's full curated permission set:
	//   - allowed: "project.read", which is both a permission the project
	//     owner holds AND within the minted token's own "project:read"
	//     scope (a UAT can only delegate permissions its own scope carries,
	//     per enforceUATDelegation/intersectCredentialCaveats).
	//   - denied: "project.manage", which the owner holds as a human but
	//     which is outside the token's scope, so the UAT credential caveat
	//     denies it regardless of the underlying human's authority.
	allowedGrant := GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{"project.read"},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         projectID,
	}
	delegatePlainAllowed := srv.authzService.CanDelegate(ctx, idPlain, allowedGrant)
	delegateLabeledAllowed := srv.authzService.CanDelegate(ctx, idLabeled, allowedGrant)
	if delegatePlainAllowed.Allowed != delegateLabeledAllowed.Allowed || delegatePlainAllowed.Reason != delegateLabeledAllowed.Reason {
		t.Fatalf("labels changed an allowed CanDelegate decision: plain=%+v labeled=%+v", delegatePlainAllowed, delegateLabeledAllowed)
	}
	if !delegatePlainAllowed.Allowed {
		t.Fatalf("expected the token (scoped to project:read) to be able to delegate project.read, got %+v", delegatePlainAllowed)
	}

	deniedGrant := GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{"project.manage"},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         projectID,
	}
	delegatePlainDenied := srv.authzService.CanDelegate(ctx, idPlain, deniedGrant)
	delegateLabeledDenied := srv.authzService.CanDelegate(ctx, idLabeled, deniedGrant)
	if delegatePlainDenied.Allowed != delegateLabeledDenied.Allowed || delegatePlainDenied.Reason != delegateLabeledDenied.Reason {
		t.Fatalf("labels changed a denied CanDelegate decision: plain=%+v labeled=%+v", delegatePlainDenied, delegateLabeledDenied)
	}
	if delegatePlainDenied.Allowed {
		t.Fatalf("expected the token (scoped to project:read) to be denied delegating project.manage, which is outside its scope, got %+v", delegatePlainDenied)
	}
}

// AC3: control characters, oversized values, and unsafe metadata are
// rejected at mint under the documented schema.
func TestCredentialDecoration_AC3_MintRejectsUnsafeMetadata(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("e1-ac3-p")
	ownerID := tid("e1-ac3-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"reserved label key", map[string]interface{}{
			"name": "t1", "projectId": projectID, "scopes": []string{"agent:read"},
			"labels": map[string]string{"user_id": "x"},
		}},
		{"oversized purpose", map[string]interface{}{
			"name": "t2", "projectId": projectID, "scopes": []string{"agent:read"},
			"purpose": strings.Repeat("p", 200),
		}},
		{"control character in name", map[string]interface{}{
			"name": "bad\x00name", "projectId": projectID, "scopes": []string{"agent:read"},
		}},
		{"label value looks like a bearer token", map[string]interface{}{
			"name": "t3", "projectId": projectID, "scopes": []string{"agent:read"},
			"labels": map[string]string{"note": "has scion_pat_leak"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, resp := e1MintTokenViaAPI(t, srv, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %+v", code, resp)
			}
			if errObj, ok := resp["error"].(map[string]interface{}); ok {
				if msg, ok := errObj["message"].(string); ok {
					// The error must never echo the offending raw value.
					if strings.Contains(msg, "scion_pat_leak") || strings.Contains(msg, "\x00") {
						t.Fatalf("error message leaked the offending value: %q", msg)
					}
				}
			}
		})
	}

	// Positive control: the same request shape without unsafe metadata succeeds.
	code, resp := e1MintTokenViaAPI(t, srv, map[string]interface{}{
		"name": "ok", "projectId": projectID, "scopes": []string{"agent:read"},
		"purpose": "ci automation", "labels": map[string]string{"env": "prod"},
	})
	if code != http.StatusCreated {
		t.Fatalf("expected bounded metadata to be accepted, got %d: %+v", code, resp)
	}
}

func TestCredentialDecoration_MintPreservesValidationErrorMessage(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("e1-error-contract-p")
	ownerID := tid("e1-error-contract-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	code, resp := e1MintTokenViaAPI(t, srv, map[string]interface{}{
		"name": strings.Repeat("n", uatMaxNameBytes+1), "projectId": projectID, "scopes": []string{"agent:read"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %+v", code, resp)
	}
	errorBody, ok := resp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("response missing structured error: %+v", resp)
	}
	if got, want := errorBody["message"], "invalid name: must be at most 128 bytes"; got != want {
		t.Fatalf("validation message = %q, want %q", got, want)
	}
}

// TestCredentialDecoration_AC_MetadataAuditAndAPIEcho closes review-3 finding
// 2 (non-blocking): the credential_create mutation-audit AfterSummary must
// record has_purpose/label_keys, and never the purpose/label values
// themselves (plan §2.4); and the token create/list API must echo
// purpose/labels when set, and omit them (not render empty strings/maps)
// when not.
func TestCredentialDecoration_AC_MetadataAuditAndAPIEcho(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("e1-metadata-echo-p")
	ownerID := tid("e1-metadata-echo-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	// --- With metadata ---
	codeWith, respWith := e1MintTokenViaAPI(t, srv, map[string]interface{}{
		"name": "with-metadata", "projectId": projectID, "scopes": []string{"agent:read"},
		"purpose": "ci automation", "labels": map[string]string{"env": "prod"},
	})
	if codeWith != http.StatusCreated {
		t.Fatalf("mint with-metadata failed: %d %+v", codeWith, respWith)
	}

	// (a) Audit: has_purpose/label_keys present, values absent.
	ctx := context.Background()
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "credential_create",
		Limit:        50,
	})
	if err != nil {
		t.Fatalf("failed to list mutation audits: %v", err)
	}
	// AfterSummary carries the token's server-assigned ID, not its name, so
	// identify the with-metadata record by the presence of label_keys (only
	// appendCredentialMetadataAuditFields adds that key, and only when
	// purpose or labels were supplied).
	var withMetadataAudit *store.MutationAuditRecord
	for _, a := range audits {
		if strings.Contains(a.AfterSummary, `"label_keys"`) {
			withMetadataAudit = a
			break
		}
	}
	if withMetadataAudit == nil {
		t.Fatalf("no credential_create mutation audit with metadata fields found among %d records", len(audits))
	}
	var afterFields map[string]interface{}
	if err := json.Unmarshal([]byte(withMetadataAudit.AfterSummary), &afterFields); err != nil {
		t.Fatalf("AfterSummary is not valid JSON: %v: %q", err, withMetadataAudit.AfterSummary)
	}
	if hasPurpose, _ := afterFields["has_purpose"].(bool); !hasPurpose {
		t.Errorf("expected has_purpose:true in AfterSummary, got %v", afterFields["has_purpose"])
	}
	labelKeys, ok := afterFields["label_keys"].([]interface{})
	if !ok || len(labelKeys) != 1 || labelKeys[0] != "env" {
		t.Errorf(`expected label_keys:["env"] in AfterSummary, got %v`, afterFields["label_keys"])
	}
	if strings.Contains(withMetadataAudit.AfterSummary, "ci automation") || strings.Contains(withMetadataAudit.AfterSummary, "prod") {
		t.Fatalf("AfterSummary leaked a purpose/label VALUE, not just its presence: %q", withMetadataAudit.AfterSummary)
	}

	// (b) API echo: POST response carries purpose/labels.
	accessToken, ok := respWith["accessToken"].(map[string]interface{})
	if !ok {
		t.Fatalf("response missing accessToken object: %+v", respWith)
	}
	if got, _ := accessToken["purpose"].(string); got != "ci automation" {
		t.Errorf("expected accessToken.purpose=%q, got %v", "ci automation", accessToken["purpose"])
	}
	labelsWire, ok := accessToken["labels"].(map[string]interface{})
	if !ok || labelsWire["env"] != "prod" {
		t.Errorf(`expected accessToken.labels={"env":"prod"}, got %v`, accessToken["labels"])
	}

	// --- Without metadata: purpose/labels keys must be entirely absent
	// (omitempty), not rendered as "" / {} / null.
	codeWithout, respWithout := e1MintTokenViaAPI(t, srv, map[string]interface{}{
		"name": "no-metadata", "projectId": projectID, "scopes": []string{"agent:read"},
	})
	if codeWithout != http.StatusCreated {
		t.Fatalf("mint no-metadata failed: %d %+v", codeWithout, respWithout)
	}
	accessTokenNoMeta, ok := respWithout["accessToken"].(map[string]interface{})
	if !ok {
		t.Fatalf("response missing accessToken object: %+v", respWithout)
	}
	if _, present := accessTokenNoMeta["purpose"]; present {
		t.Errorf("expected no purpose key for a no-metadata token, got %v", accessTokenNoMeta["purpose"])
	}
	if _, present := accessTokenNoMeta["labels"]; present {
		t.Errorf("expected no labels key for a no-metadata token, got %v", accessTokenNoMeta["labels"])
	}

	// (b, continued) GET list echoes the same shape.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/auth/tokens", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list tokens failed: %d %s", rec.Code, rec.Body.String())
	}
	var listResp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("failed to parse list response: %v", err)
	}
	items, ok := listResp["items"].([]interface{})
	if !ok {
		t.Fatalf("list response missing items array: %+v", listResp)
	}
	var foundWith, foundWithout bool
	for _, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		switch item["name"] {
		case "with-metadata":
			foundWith = true
			if got, _ := item["purpose"].(string); got != "ci automation" {
				t.Errorf("list: expected purpose=%q for with-metadata, got %v", "ci automation", item["purpose"])
			}
		case "no-metadata":
			foundWithout = true
			if _, present := item["purpose"]; present {
				t.Errorf("list: expected no purpose key for no-metadata, got %v", item["purpose"])
			}
			if _, present := item["labels"]; present {
				t.Errorf("list: expected no labels key for no-metadata, got %v", item["labels"])
			}
		}
	}
	if !foundWith || !foundWithout {
		t.Fatalf("list response did not contain both minted tokens: foundWith=%v foundWithout=%v, items=%+v", foundWith, foundWithout, items)
	}
}

// AC3 (legacy rows): a row with a control character in its name (created
// before validation existed, e.g. by direct store access) still
// authenticates, keeps the raw name pre-render, and renders it sanitized.
func TestCredentialDecoration_AC3_LegacyRowRendersSanitized(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("e1-ac3-legacy-p")
	ownerID := tid("e1-ac3-legacy-o")
	rs4Project(t, s, projectID, ownerID)

	plaintext := store.UATPrefix + "legacylegacylegacylegacylegacy1"
	hash := sha256.Sum256([]byte(plaintext))
	tok := &store.UserAccessToken{
		ID:        uuid.New().String(),
		UserID:    ownerID,
		Name:      "bad\x00name",
		Prefix:    plaintext[:UATPrefixLength],
		KeyHash:   hex.EncodeToString(hash[:]),
		ProjectID: projectID,
		Scopes:    []string{"agent:read"},
		Created:   time.Now(),
	}
	if err := s.CreateUserAccessToken(context.Background(), tok); err != nil {
		t.Fatalf("failed to insert legacy token: %v", err)
	}

	identity, err := srv.uatService.ValidateToken(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	dec := identity.Decoration()
	if dec == nil {
		t.Fatal("expected a decoration even for a legacy row with no bounded metadata")
	}
	if dec.TokenName != "bad\x00name" {
		t.Fatalf("expected the raw stored name preserved pre-render, got %q", dec.TokenName)
	}
	if dec.Purpose != "" || len(dec.Labels) != 0 {
		t.Fatalf("a legacy row must carry no purpose/labels, got %+v", dec)
	}
	if dec.Boundary.Kind != "project" || dec.Boundary.ProjectID != projectID {
		t.Fatalf("expected a project boundary for a pre-E.1 project token, got %+v", dec.Boundary)
	}

	attrs := attrMap(t, dec.LogValue().Group())
	name := attrs["name"].String()
	if strings.Contains(name, "\x00") {
		t.Fatalf("rendered name still contains a raw control character: %q", name)
	}
	if !strings.Contains(name, "�") {
		t.Fatalf("expected the control character to render as U+FFFD, got %q", name)
	}
}

// TestCredentialDecoration_AC3_LegacyRowWithBadLabelKeyRendersSanitized is
// the F11 regression test (review-2 finding 2(a)): a row with a label key
// that could never pass ValidateCredentialMetadata (control character),
// inserted directly via the store as a pre-E.1/out-of-band row would be,
// still renders with the key sanitized, not just the value.
func TestCredentialDecoration_AC3_LegacyRowWithBadLabelKeyRendersSanitized(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("e1-ac3-legacy-key-p")
	ownerID := tid("e1-ac3-legacy-key-o")
	rs4Project(t, s, projectID, ownerID)

	plaintext := store.UATPrefix + "legacykeylegacykeylegacykey12345"
	hash := sha256.Sum256([]byte(plaintext))
	tok := &store.UserAccessToken{
		ID:        uuid.New().String(),
		UserID:    ownerID,
		Name:      "legacy-key-test",
		Prefix:    plaintext[:UATPrefixLength],
		KeyHash:   hex.EncodeToString(hash[:]),
		ProjectID: projectID,
		Scopes:    []string{"agent:read"},
		Labels:    map[string]string{"bad\nkey": "v"},
		Created:   time.Now(),
	}
	if err := s.CreateUserAccessToken(context.Background(), tok); err != nil {
		t.Fatalf("failed to insert legacy token: %v", err)
	}

	identity, err := srv.uatService.ValidateToken(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	dec := identity.Decoration()
	if dec == nil {
		t.Fatal("expected a decoration even for a legacy row with an out-of-schema label key")
	}
	if dec.Labels["bad\nkey"] != "v" {
		t.Fatalf("expected the raw stored label key preserved pre-render, got %+v", dec.Labels)
	}

	attrs := attrMap(t, dec.LogValue().Group())
	labelsGroup, ok := attrs["labels"]
	if !ok {
		t.Fatalf("expected a labels group in the rendered decoration, got %+v", attrs)
	}
	labelAttrs := attrMap(t, labelsGroup.Group())
	for k := range labelAttrs {
		if strings.Contains(k, "\n") {
			t.Fatalf("rendered label key still contains a raw control character: %q", k)
		}
	}
	foundSanitized := false
	for k := range labelAttrs {
		if strings.Contains(k, "�") {
			foundSanitized = true
		}
	}
	if !foundSanitized {
		t.Fatalf("expected a label key containing U+FFFD in place of the control character, got %+v", labelAttrs)
	}
}

// AC4: no token plaintext or hash ever appears in logs for a full,
// real-middleware UAT request, and existing (pre-E.1) project UATs continue
// to work without any hub-boundary support.
func TestCredentialDecoration_AC4_NoPlaintextOrHashInLogs(t *testing.T) {
	// The capture must be installed before testServer: testServer's New()
	// call binds the Server's subsystem loggers (logging.Subsystem) to
	// whatever slog.Default() is at that moment, and that binding does not
	// follow a later slog.SetDefault swap. Capturing afterward could leave
	// logs written through a subsystem logger unobserved by buf. As a side
	// effect, buf now also covers the mint call below, not just the read
	// request -- a strictly stronger check.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv, s := testServer(t)

	// Positive control: New() unconditionally logs during construction, so
	// a capture installed before it must already have observed something.
	// This is the part that a capture-after-construct ordering bug (the
	// regression this test guards against) would silently defeat.
	requireLogCaptureLive(t, &buf, serverConstructionLogLine)

	projectID := tid("e1-ac4-p")
	ownerID := tid("e1-ac4-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	code, resp := e1MintTokenViaAPI(t, srv, map[string]interface{}{
		"name": "logtest", "projectId": projectID, "scopes": []string{"project:read"},
	})
	if code != http.StatusCreated {
		t.Fatalf("mint failed: %d %+v", code, resp)
	}
	key := resp["token"].(string)
	hash := sha256.Sum256([]byte(key))
	hashHex := hex.EncodeToString(hash[:])
	prefix := resp["accessToken"].(map[string]interface{})["prefix"].(string)

	rec := doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/projects/"+projectID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the request to succeed with an existing project UAT (no hub boundary needed), got %d: %s",
			rec.Code, rec.Body.String())
	}

	logged := buf.String()
	// Review-2 nit 5: without this, a future change to what the server logs
	// through (e.g. no longer using slog.Default()) would make every
	// !strings.Contains check below pass vacuously.
	if !strings.Contains(logged, "Request completed") {
		t.Fatalf("log capture is empty or missing the request log line; the absence assertions below would be vacuous: %q", logged)
	}
	if strings.Contains(logged, key) {
		t.Fatal("logs contain the plaintext token")
	}
	if strings.Contains(logged, hashHex) {
		t.Fatal("logs contain the token's SHA-256 hash")
	}
	if strings.Contains(logged, prefix) {
		t.Fatal("logs contain the token's visible prefix (ruling Q3: never log the prefix)")
	}
}

// TestCredentialDecoration_AC4_MiddlewareCarriesDecorationToLogs is the
// missing middleware-level case the reviewer identified (F4): it
// authenticates a labeled UAT through the real, production
// UnifiedAuthMiddleware (not ValidateToken called directly), captures
// CredentialDecorationFromContext from the request context the middleware
// built, logs it exactly as a real request-log consumer would
// (slog.Any("credential", d)), and asserts both the captured fields and the
// rendered log line, including that the plaintext, hash, and visible prefix
// never appear.
func TestCredentialDecoration_AC4_MiddlewareCarriesDecorationToLogs(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("e1-ac4-mw-p")
	ownerID := tid("e1-ac4-mw-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	code, resp := e1MintTokenViaAPI(t, srv, map[string]interface{}{
		"name": "mw-test", "projectId": projectID, "scopes": []string{"project:read"},
		"purpose": "agent xyz", "labels": map[string]string{"role_hint": "admin"},
	})
	if code != http.StatusCreated {
		t.Fatalf("mint failed: %d %+v", code, resp)
	}
	key := resp["token"].(string)
	hash := sha256.Sum256([]byte(key))
	hashHex := hex.EncodeToString(hash[:])
	tokenObj := resp["accessToken"].(map[string]interface{})
	tokenID := tokenObj["id"].(string)
	prefix := tokenObj["prefix"].(string)

	var logBuf bytes.Buffer
	captureLogger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var captured CredentialDecoration
	var capturedOK bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, capturedOK = CredentialDecorationFromContext(r.Context())
		captureLogger.Info("request completed", "credential", captured)
		w.WriteHeader(http.StatusOK)
	})
	// This is exactly the production carriage path: auth.go's UAT branch
	// (:~431) calls contextWithCredentialContext(credentialContextForIdentity(...)),
	// which is what CredentialDecorationFromContext reads back out.
	handler := UnifiedAuthMiddleware(srv.authConfig)(inner)

	req := httptest.NewRequest(http.MethodGet, "/mw-decoration-test", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 through UnifiedAuthMiddleware, got %d: %s", rec.Code, rec.Body.String())
	}
	if !capturedOK {
		t.Fatal("expected CredentialDecorationFromContext to find a decoration on the middleware-authenticated request context")
	}
	if captured.TokenID != tokenID || captured.TokenName != "mw-test" {
		t.Fatalf("unexpected decoration carried through the middleware: %+v", captured)
	}
	if captured.Purpose != "agent xyz" || captured.Labels["role_hint"] != "admin" {
		t.Fatalf("expected purpose/labels to carry through the middleware, got %+v", captured)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, `"role_hint":"admin"`) {
		t.Fatalf("expected the label to render under credential.labels.*, got log: %s", logged)
	}
	if !strings.Contains(logged, `"labels_source":"issuer"`) {
		t.Fatalf("expected the labels_source=issuer marker in the rendered log, got: %s", logged)
	}
	if strings.Contains(logged, key) {
		t.Fatal("rendered log output contains the plaintext token")
	}
	if strings.Contains(logged, hashHex) {
		t.Fatal("rendered log output contains the token's SHA-256 hash")
	}
	if strings.Contains(logged, prefix) {
		t.Fatal("rendered log output contains the token's visible prefix (ruling Q3: never log the prefix)")
	}
}
