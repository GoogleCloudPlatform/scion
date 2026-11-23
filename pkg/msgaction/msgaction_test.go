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

package msgaction

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClassPredicates(t *testing.T) {
	cases := []struct {
		cls        Class
		terminal   bool
		answerable bool
	}{
		{ClassPending, false, true},
		{ClassClaimed, false, false},
		{ClassResolved, true, false},
		{ClassReplaced, true, false},
		{ClassWithdrawn, true, false},
		{ClassDismissed, true, false},
		{ClassExpired, true, false},
		{Class("unknown"), false, false},
	}
	for _, tc := range cases {
		if got := tc.cls.Terminal(); got != tc.terminal {
			t.Errorf("%q.Terminal() = %v, want %v", tc.cls, got, tc.terminal)
		}
		if got := tc.cls.Answerable(); got != tc.answerable {
			t.Errorf("%q.Answerable() = %v, want %v", tc.cls, got, tc.answerable)
		}
	}
}

func TestKindSpecClassifyAndConflictCode(t *testing.T) {
	secretSpec := KindSpec{
		Name:             "secret_request",
		ResolvedStatus:   "fulfilled",
		UsesClaim:        true,
		ClaimRecovery:    ClaimRecoveryToPending,
		ClaimTTL:         2 * time.Minute,
		Expires:          7 * 24 * time.Hour,
		DismissMinAge:    0,
		RepostTerminalOK: true,
		EventTypes:       []string{"claim_recovered"},
		ConflictCodes:    []string{"secret_request_key_pending"},
	}

	// Decisions: decided -> already_answered, claimed and expired not enabled.
	if got := ConflictCode(DecisionKindSpec, "decided"); got != "already_answered" {
		t.Fatalf("decision decided conflict = %q, want already_answered", got)
	}
	if got := ConflictCode(DecisionKindSpec, "replaced"); got != ConflictReplaced {
		t.Fatalf("decision replaced conflict = %q", got)
	}
	if got := ConflictCode(DecisionKindSpec, "withdrawn"); got != ConflictWithdrawn {
		t.Fatalf("decision withdrawn conflict = %q", got)
	}
	if got := ConflictCode(DecisionKindSpec, "dismissed"); got != ConflictDismissed {
		t.Fatalf("decision dismissed conflict = %q", got)
	}
	if got := ConflictCode(DecisionKindSpec, "pending"); got != "" {
		t.Fatalf("decision pending conflict = %q, want empty", got)
	}
	if got := ConflictCode(DecisionKindSpec, "claimed"); got != "" {
		t.Fatalf("decision claimed (UsesClaim=false) conflict = %q, want empty", got)
	}
	if got := ConflictCode(DecisionKindSpec, "expired"); got != "" {
		t.Fatalf("decision expired (Expires=0) conflict = %q, want empty", got)
	}
	if !DecisionKindSpec.Answerable("pending") || DecisionKindSpec.Answerable("decided") {
		t.Fatalf("unexpected DecisionKindSpec.Answerable results")
	}
	if DecisionKindSpec.Terminal("pending") || !DecisionKindSpec.Terminal("decided") || !DecisionKindSpec.Terminal("dismissed") {
		t.Fatalf("unexpected DecisionKindSpec.Terminal results")
	}

	// Secret requests: fulfilled -> already_resolved, claimed -> submitting, expired -> expired,
	// and kind-registered conflict code accepted.
	if got := ConflictCode(secretSpec, "fulfilled"); got != ConflictAlreadyResolved {
		t.Fatalf("secret fulfilled conflict = %q, want %q", got, ConflictAlreadyResolved)
	}
	if got := ConflictCode(secretSpec, "claimed"); got != ConflictSubmitting {
		t.Fatalf("secret claimed conflict = %q, want %q", got, ConflictSubmitting)
	}
	if got := ConflictCode(secretSpec, "expired"); got != ConflictExpired {
		t.Fatalf("secret expired conflict = %q, want %q", got, ConflictExpired)
	}
	if got := ConflictCode(secretSpec, "secret_request_key_pending"); got != "secret_request_key_pending" {
		t.Fatalf("kind-registered conflict code = %q, want secret_request_key_pending", got)
	}
	if secretSpec.Answerable("claimed") || secretSpec.Terminal("claimed") {
		t.Fatalf("claimed must be neither answerable nor terminal")
	}
}

// Seam test C1: ClaimRecovery=None is never recovered, while ToPending recovers
// only after ClaimTTL elapses.
func TestKindSpec_ClaimRecoverySeam(t *testing.T) {
	base := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

	execSpec := KindSpec{
		Name:           "exec_action",
		ResolvedStatus: "approved",
		UsesClaim:      true,
		ClaimRecovery:  ClaimRecoveryNone,
		ClaimTTL:       2 * time.Minute,
	}
	// Even hours past ClaimTTL, ClaimRecoveryNone is never recovered.
	if execSpec.ShouldRecoverClaim(base, base.Add(24*time.Hour)) {
		t.Fatalf("ClaimRecovery=None must never be recovered")
	}

	// Zero-value ClaimRecovery is also never recovered.
	zeroRecovery := KindSpec{
		Name:      "exec_action",
		UsesClaim: true,
		ClaimTTL:  2 * time.Minute,
	}
	if zeroRecovery.ShouldRecoverClaim(base, base.Add(24*time.Hour)) {
		t.Fatalf("zero-value ClaimRecovery must never be recovered")
	}

	// ClaimRecoveryToPending recovers at and after ClaimTTL, never before.
	secretSpec := KindSpec{
		Name:          "secret_request",
		UsesClaim:     true,
		ClaimRecovery: ClaimRecoveryToPending,
		ClaimTTL:      2 * time.Minute,
	}
	if secretSpec.ShouldRecoverClaim(base, base.Add(2*time.Minute-time.Second)) {
		t.Fatalf("claim before ClaimTTL must not be recovered")
	}
	if !secretSpec.ShouldRecoverClaim(base, base.Add(2*time.Minute)) {
		t.Fatalf("claim at ClaimTTL must be recovered")
	}
	if secretSpec.ShouldRecoverClaim(time.Time{}, base.Add(10*time.Minute)) {
		t.Fatalf("zero claimedAt must not be recovered")
	}
}

// Seam test C3: kind-registered event types and conflict codes are accepted
// alongside core sets, while unregistered values are rejected.
func TestKindSpec_KindRegisteredEventTypesAndConflictCodes(t *testing.T) {
	execSpec := KindSpec{
		Name:           "exec_action",
		ResolvedStatus: "approved",
		UsesClaim:      true,
		ClaimRecovery:  ClaimRecoveryNone,
		EventTypes:     []string{"exec_dispatched", "exec_completed"},
		ConflictCodes:  []string{"exec_container_unavailable"},
	}

	// Core event types + resolved status + kind-registered event types accepted.
	for _, ev := range []string{
		EventRequested, EventReplaced, EventWithdrawn, EventResolved,
		EventDismissed, EventExpired, EventRefused,
		"approved", "exec_dispatched", "exec_completed",
	} {
		if !execSpec.AcceptsEventType(ev) {
			t.Errorf("AcceptsEventType(%q) = false, want true", ev)
		}
	}
	for _, bad := range []string{"", "claim_recovered", "fulfilled", "unknown_event"} {
		if execSpec.AcceptsEventType(bad) {
			t.Errorf("AcceptsEventType(%q) = true, want false", bad)
		}
	}

	// Core conflict codes + kind-registered conflict codes accepted.
	for _, code := range []string{
		ConflictReplaced, ConflictWithdrawn, ConflictDismissed,
		ConflictExpired, ConflictAlreadyResolved, ConflictSubmitting,
		"exec_container_unavailable",
	} {
		if !execSpec.AcceptsConflictCode(code) {
			t.Errorf("AcceptsConflictCode(%q) = false, want true", code)
		}
	}
	if got := ConflictCode(execSpec, "exec_container_unavailable"); got != "exec_container_unavailable" {
		t.Errorf("ConflictCode(exec_container_unavailable) = %q", got)
	}
	for _, bad := range []string{"", "already_answered", "bogus_code"} {
		if execSpec.AcceptsConflictCode(bad) {
			t.Errorf("AcceptsConflictCode(%q) = true, want false", bad)
		}
	}
	if !DecisionKindSpec.AcceptsConflictCode("already_answered") {
		t.Errorf("DecisionKindSpec must accept already_answered")
	}

	// DetailKeys allow-list fails closed.
	if execSpec.AcceptsDetailKey("exec_project_id") {
		t.Errorf("empty DetailKeys must fail closed")
	}
	execSpec.DetailKeys = []string{"exec_project_id", "exit_code"}
	if !execSpec.AcceptsDetailKey("exec_project_id") || !execSpec.AcceptsDetailKey("exit_code") {
		t.Errorf("registered DetailKeys must be accepted")
	}
	if execSpec.AcceptsDetailKey("secret_value") || execSpec.AcceptsDetailKey("") {
		t.Errorf("unlisted or empty detail key must be rejected")
	}

	// Registry lookup: DecisionKindSpec pre-registered; RegisterKind / UnregisterKind work.
	if got, ok := LookupKind(DecisionKindSpec.Name); !ok || got.Name != DecisionKindSpec.Name {
		t.Fatalf("DecisionKindSpec must be pre-registered, got %+v, %v", got, ok)
	}
	RegisterKind(execSpec)
	t.Cleanup(func() { UnregisterKind(execSpec.Name) })
	if got, ok := LookupKind("exec_action"); !ok || !got.AcceptsDetailKey("exec_project_id") {
		t.Fatalf("LookupKind(exec_action) failed: %+v, %v", got, ok)
	}
	UnregisterKind("exec_action")
	if _, ok := LookupKind("exec_action"); ok {
		t.Fatalf("UnregisterKind(exec_action) did not remove kind")
	}

	// Duplicate registration panics to prevent silent allow-list overwrites.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("RegisterKind on duplicate name %q did not panic", DecisionKindSpec.Name)
			}
		}()
		RegisterKind(DecisionKindSpec)
	}()
}

// Seam test C4: a row needs no message anchor; identity is (asker, key, version).
func TestAnchor_Optional(t *testing.T) {
	var empty Anchor
	if !empty.IsZero() {
		t.Fatalf("empty Anchor.IsZero() = false, want true")
	}
	anchored := Anchor{MessageID: "msg-1", BlockIndex: 2}
	if anchored.IsZero() {
		t.Fatalf("populated Anchor.IsZero() = true, want false")
	}
	srAnchor := Anchor{MessageID: "sr-msg-1"}
	if srAnchor.IsZero() || srAnchor.BlockIndex != 0 {
		t.Fatalf("secret-request Anchor unexpected: %+v", srAnchor)
	}

	// Verify SecretRequestKindSpec is pre-registered and classifies fulfilling/fulfilled cleanly.
	spec, ok := LookupKind(KindSecretRequest)
	if !ok {
		t.Fatalf("LookupKind(%q) = false, want true", KindSecretRequest)
	}
	if got := ConflictCode(spec, SecretRequestClaimedStatus); got != ConflictSubmitting {
		t.Fatalf("ConflictCode(fulfilling) = %q, want %q", got, ConflictSubmitting)
	}
	if got := ConflictCode(spec, SecretRequestResolvedStatus); got != ConflictAlreadyFulfilled {
		t.Fatalf("ConflictCode(fulfilled) = %q, want %q", got, ConflictAlreadyFulfilled)
	}
	if !spec.AcceptsEventType(EventFulfilled) || !spec.AcceptsEventType(EventClaimRecovered) {
		t.Fatalf("SecretRequestKindSpec must accept fulfilled and claim_recovered")
	}
	for _, k := range []string{"secret_type", "target", "secret_version"} {
		if !spec.AcceptsDetailKey(k) {
			t.Errorf("SecretRequestKindSpec must allow detail key %q", k)
		}
	}
	if spec.AcceptsDetailKey("value") {
		t.Errorf("SecretRequestKindSpec must reject detail key \"value\"")
	}

	// PlanRepost and Head (with ResolvedAt) operate without any message anchor.
	resolvedAt := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	execSpec := KindSpec{
		Name:                    "exec_action",
		ResolvedStatus:          "approved",
		RepostTerminalOK:        true,
		ReplaceResolvedOnUpdate: false,
	}
	plan, err := PlanRepost(execSpec, &Head{
		RowID:      "row-1",
		Key:        "run-tests",
		Version:    1,
		Status:     "approved",
		ResolvedAt: &resolvedAt,
	}, false)
	if err != nil || plan.NextVersion != 2 || plan.ReplacesRowID != "row-1" || plan.MarkReplaced {
		t.Fatalf("anchorless repost after approved: got %+v, %v", plan, err)
	}
}

func TestPlanRepost_Decisions(t *testing.T) {
	// Fresh key -> v1.
	plan, err := PlanRepost(DecisionKindSpec, nil, false)
	if err != nil || plan.NextVersion != 1 || plan.ReplacesRowID != "" || plan.MarkReplaced {
		t.Fatalf("fresh key: got %+v, %v", plan, err)
	}

	// Pending without update -> ErrKeyPending.
	pendingHead := &Head{RowID: "r1", Key: "D234", Version: 1, Status: "pending"}
	if _, err := PlanRepost(DecisionKindSpec, pendingHead, false); !errors.Is(err, ErrKeyPending) {
		t.Fatalf("pending without update: got %v, want ErrKeyPending", err)
	}

	// Decided/withdrawn/dismissed without update -> ErrKeyExists (RepostTerminalOK=false).
	for _, st := range []string{"decided", "withdrawn", "dismissed", "replaced"} {
		h := &Head{RowID: "r1", Key: "D234", Version: 1, Status: st}
		if _, err := PlanRepost(DecisionKindSpec, h, false); !errors.Is(err, ErrKeyExists) {
			t.Fatalf("%s without update: got %v, want ErrKeyExists", st, err)
		}
	}

	// Pending with update -> v2, replaces r1, MarkReplaced=true.
	plan, err = PlanRepost(DecisionKindSpec, pendingHead, true)
	if err != nil || plan.NextVersion != 2 || plan.DisplayKey != "D234" || plan.ReplacesRowID != "r1" || !plan.MarkReplaced {
		t.Fatalf("pending with update: got %+v, %v", plan, err)
	}

	// Decided with update -> v2, replaces r1, MarkReplaced=true (ReplaceResolvedOnUpdate=true).
	decidedHead := &Head{RowID: "r1", Key: "D234", Version: 2, Status: "decided"}
	plan, err = PlanRepost(DecisionKindSpec, decidedHead, true)
	if err != nil || plan.NextVersion != 3 || plan.ReplacesRowID != "r1" || !plan.MarkReplaced {
		t.Fatalf("decided with update: got %+v, %v", plan, err)
	}

	// Withdrawn / dismissed with update -> v2, links ReplacesRowID, MarkReplaced=false (keeps terminal status).
	for _, st := range []string{"withdrawn", "dismissed"} {
		h := &Head{RowID: "r1", Key: "D234", Version: 1, Status: st}
		plan, err = PlanRepost(DecisionKindSpec, h, true)
		if err != nil || plan.NextVersion != 2 || plan.ReplacesRowID != "r1" || plan.MarkReplaced {
			t.Fatalf("%s with update: got %+v, %v", st, plan, err)
		}
	}

	// Already replaced head -> v+1, no ReplacesRowID.
	repHead := &Head{RowID: "r1", Key: "D234", Version: 2, Status: "replaced", ReplacedBy: "r2"}
	plan, err = PlanRepost(DecisionKindSpec, repHead, true)
	if err != nil || plan.NextVersion != 3 || plan.ReplacesRowID != "" || plan.MarkReplaced {
		t.Fatalf("replaced head with update: got %+v, %v", plan, err)
	}
}

func TestPlanRepost_SplitTerminalAndReplaceFlags(t *testing.T) {
	// RepostTerminalOK=false with ReplaceResolvedOnUpdate=false: requires --update
	// for resolved prior, yet preserves the prior's resolved status when updated.
	spec := KindSpec{
		Name:                    "custom",
		ResolvedStatus:          "approved",
		RepostTerminalOK:        false,
		ReplaceResolvedOnUpdate: false,
	}
	h := &Head{RowID: "r1", Key: "K1", Version: 1, Status: "approved"}
	if _, err := PlanRepost(spec, h, false); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("without update: got %v, want ErrKeyExists", err)
	}
	plan, err := PlanRepost(spec, h, true)
	if err != nil || plan.NextVersion != 2 || plan.ReplacesRowID != "r1" || plan.MarkReplaced {
		t.Fatalf("with update (ReplaceResolvedOnUpdate=false): got %+v, %v", plan, err)
	}
}

func TestPlanRepost_TerminalOK(t *testing.T) {
	secretSpec := KindSpec{
		Name:                    "secret_request",
		ResolvedStatus:          "fulfilled",
		UsesClaim:               true,
		Expires:                 7 * 24 * time.Hour,
		RepostTerminalOK:        true,
		ReplaceResolvedOnUpdate: false,
	}

	// Pending and claimed refuse without update.
	for _, st := range []string{"pending", "claimed"} {
		h := &Head{RowID: "r1", Key: "API_KEY", Version: 1, Status: st}
		if _, err := PlanRepost(secretSpec, h, false); !errors.Is(err, ErrKeyPending) {
			t.Fatalf("%s without update: got %v, want ErrKeyPending", st, err)
		}
	}

	// Terminal prior versions auto-increment without update and keep their terminal status.
	for _, st := range []string{"fulfilled", "withdrawn", "dismissed", "expired"} {
		h := &Head{RowID: "r1", Key: "API_KEY", Version: 1, Status: st}
		plan, err := PlanRepost(secretSpec, h, false)
		if err != nil || plan.NextVersion != 2 || plan.ReplacesRowID != "r1" || plan.MarkReplaced {
			t.Fatalf("%s without update: got %+v, %v", st, plan, err)
		}
	}
}

func TestNormalizeReason(t *testing.T) {
	got, err := NormalizeReason("  trimmed reason  \n")
	if err != nil || got != "trimmed reason" {
		t.Fatalf("NormalizeReason = %q, %v", got, err)
	}
	exact := strings.Repeat("é", MaxReasonRunes)
	if got, err := NormalizeReason(exact); err != nil || got != exact {
		t.Fatalf("exact 500 runes failed: %v", err)
	}
	over := strings.Repeat("é", MaxReasonRunes+1)
	if _, err := NormalizeReason(over); !errors.Is(err, ErrReasonTooLong) {
		t.Fatalf("501 runes: got %v, want ErrReasonTooLong", err)
	}
}

func TestValidateWithdrawRequest(t *testing.T) {
	opts := WithdrawParseOptions{MaxKeys: 10, KeyLabel: "decision key"}

	req := &WithdrawRequest{Keys: []string{" D234 ", "1"}, WholeSet: true, Reason: "  done  "}
	if ferr := ValidateWithdrawRequest(req, opts); ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if len(req.Keys) != 2 || req.Keys[0] != "D234" || req.Keys[1] != "1" || !req.WholeSet || req.Reason != "done" {
		t.Fatalf("unexpected validated withdraw: %+v", req)
	}

	// Duplicate case-insensitively.
	ferr := ValidateWithdrawRequest(&WithdrawRequest{Keys: []string{"D234", "d234"}}, opts)
	if ferr == nil || ferr.Field != "keys" || !strings.Contains(ferr.Message, "duplicate decision key") {
		t.Fatalf("expected duplicate error, got %+v", ferr)
	}

	// Empty keys.
	ferr = ValidateWithdrawRequest(&WithdrawRequest{Keys: []string{}}, opts)
	if ferr == nil || ferr.Field != "keys" {
		t.Fatalf("expected empty keys error, got %+v", ferr)
	}

	// Blank key element.
	ferr = ValidateWithdrawRequest(&WithdrawRequest{Keys: []string{"   "}}, opts)
	if ferr == nil || ferr.Field != "keys" {
		t.Fatalf("expected blank key error, got %+v", ferr)
	}

	// Too-long reason.
	longReason := strings.Repeat("a", MaxReasonRunes+1)
	ferr = ValidateWithdrawRequest(&WithdrawRequest{Keys: []string{"D1"}, Reason: longReason}, opts)
	if ferr == nil || ferr.Field != "reason" {
		t.Fatalf("expected reason too long error, got %+v", ferr)
	}
}

func TestValidateDismissRequest(t *testing.T) {
	// Empty request is allowed.
	req := &DismissRequest{}
	if ferr := ValidateDismissRequest(req, DismissParseOptions{}); ferr != nil {
		t.Fatalf("empty request: got %v", ferr)
	}

	// Trims reason.
	req = &DismissRequest{RowIDs: []string{"r1", "r2"}, Reason: "  nope ", Surface: "native", IdempotencyKey: "k1"}
	if ferr := ValidateDismissRequest(req, DismissParseOptions{MaxRowIDs: 10}); ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if req.Reason != "nope" {
		t.Fatalf("expected trimmed reason, got %q", req.Reason)
	}

	// ValidateDismissRequest with custom RowIDsField ("decisionIds") for caller-decoded structs.
	custom := &DismissRequest{RowIDs: []string{"1", "2", "3"}}
	ferr := ValidateDismissRequest(custom, DismissParseOptions{
		RowIDsField: "decisionIds",
		MaxRowIDs:   2,
	})
	if ferr == nil || ferr.Field != "decisionIds" {
		t.Fatalf("expected max row ids error, got %+v", ferr)
	}

	// Idempotency key too long.
	longKey := strings.Repeat("x", MaxIdempotencyKeyLen+1)
	ferr = ValidateDismissRequest(&DismissRequest{IdempotencyKey: longKey}, DismissParseOptions{})
	if ferr == nil || ferr.Field != "idempotencyKey" {
		t.Fatalf("expected idempotencyKey error, got %+v", ferr)
	}

	// Invalid surface.
	ferr = ValidateDismissRequest(&DismissRequest{Surface: "bogus"}, DismissParseOptions{
		NormalizeSurface: func(string) (string, bool) { return "", false },
		SurfaceError:     func(raw string) string { return "bad surface " + raw },
	})
	if ferr == nil || ferr.Field != "surface" || ferr.Message != "bad surface bogus" {
		t.Fatalf("expected surface error, got %+v", ferr)
	}
}
