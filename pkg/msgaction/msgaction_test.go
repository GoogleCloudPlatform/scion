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
		Expires:          7 * 24 * time.Hour,
		DismissMinAge:    0,
		RepostTerminalOK: true,
	}

	// Decisions: decided -> already_answered, claimed not enabled.
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
	if !DecisionKindSpec.Answerable("pending") || DecisionKindSpec.Answerable("decided") {
		t.Fatalf("unexpected DecisionKindSpec.Answerable results")
	}
	if DecisionKindSpec.Terminal("pending") || !DecisionKindSpec.Terminal("decided") || !DecisionKindSpec.Terminal("dismissed") {
		t.Fatalf("unexpected DecisionKindSpec.Terminal results")
	}

	// Secret requests: fulfilled -> already_resolved, claimed -> submitting, expired -> expired.
	if got := ConflictCode(secretSpec, "fulfilled"); got != ConflictAlreadyResolved {
		t.Fatalf("secret fulfilled conflict = %q, want %q", got, ConflictAlreadyResolved)
	}
	if got := ConflictCode(secretSpec, "claimed"); got != ConflictSubmitting {
		t.Fatalf("secret claimed conflict = %q, want %q", got, ConflictSubmitting)
	}
	if got := ConflictCode(secretSpec, "expired"); got != ConflictExpired {
		t.Fatalf("secret expired conflict = %q, want %q", got, ConflictExpired)
	}
	if secretSpec.Answerable("claimed") || secretSpec.Terminal("claimed") {
		t.Fatalf("claimed must be neither answerable nor terminal")
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

	// Decided with update -> v2, replaces r1, MarkReplaced=true (decisions transition decided->replaced on --update).
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

func TestPlanRepost_TerminalOK(t *testing.T) {
	secretSpec := KindSpec{
		Name:             "secret_request",
		ResolvedStatus:   "fulfilled",
		UsesClaim:        true,
		RepostTerminalOK: true,
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

func TestParseWithdrawRequest(t *testing.T) {
	opts := WithdrawParseOptions{MaxKeys: 10, KeyLabel: "decision key"}

	req, ferr := ParseWithdrawRequest(strings.NewReader(`{"keys":[" D234 ","1"],"wholeSet":true,"reason":"  done  "}`), opts)
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if len(req.Keys) != 2 || req.Keys[0] != "D234" || req.Keys[1] != "1" || !req.WholeSet || req.Reason != "done" {
		t.Fatalf("unexpected parsed withdraw: %+v", req)
	}

	// Duplicate case-insensitively.
	_, ferr = ParseWithdrawRequest(strings.NewReader(`{"keys":["D234","d234"]}`), opts)
	if ferr == nil || ferr.Field != "keys" || !strings.Contains(ferr.Message, "duplicate decision key") {
		t.Fatalf("expected duplicate error, got %+v", ferr)
	}

	// Empty keys.
	_, ferr = ParseWithdrawRequest(strings.NewReader(`{"keys":[]}`), opts)
	if ferr == nil || ferr.Field != "keys" {
		t.Fatalf("expected empty keys error, got %+v", ferr)
	}

	// Blank key element.
	_, ferr = ParseWithdrawRequest(strings.NewReader(`{"keys":["   "]}`), opts)
	if ferr == nil || ferr.Field != "keys" {
		t.Fatalf("expected blank key error, got %+v", ferr)
	}

	// Too-long reason.
	longReason := strings.Repeat("a", MaxReasonRunes+1)
	_, ferr = ParseWithdrawRequest(strings.NewReader(`{"keys":["D1"],"reason":"`+longReason+`"}`), opts)
	if ferr == nil || ferr.Field != "reason" {
		t.Fatalf("expected reason too long error, got %+v", ferr)
	}

	// Malformed JSON.
	_, ferr = ParseWithdrawRequest(strings.NewReader(`{bad`), opts)
	if ferr == nil || !ferr.InvalidJSON {
		t.Fatalf("expected invalid JSON error, got %+v", ferr)
	}
}

func TestParseDismissRequest(t *testing.T) {
	// Empty body is allowed.
	req, ferr := ParseDismissRequest(strings.NewReader(""), DismissParseOptions{})
	if ferr != nil || len(req.RowIDs) != 0 || req.Reason != "" {
		t.Fatalf("empty body: got %+v, %v", req, ferr)
	}

	// Default rowIds field.
	req, ferr = ParseDismissRequest(strings.NewReader(`{"rowIds":["r1","r2"],"reason":"  nope ","surface":"native","idempotencyKey":"k1"}`), DismissParseOptions{MaxRowIDs: 10})
	if ferr != nil || len(req.RowIDs) != 2 || req.Reason != "nope" || req.Surface != "native" || req.IdempotencyKey != "k1" {
		t.Fatalf("default dismiss parse: got %+v, %v", req, ferr)
	}

	// Custom RowIDsField ("decisionIds") for decisions wire compatibility.
	req, ferr = ParseDismissRequest(strings.NewReader(`{"decisionIds":["d1"],"reason":"later"}`), DismissParseOptions{
		RowIDsField: "decisionIds",
		MaxRowIDs:   10,
		NormalizeSurface: func(raw string) (string, bool) {
			if raw == "" {
				return "native", true
			}
			return raw, raw == "native"
		},
	})
	if ferr != nil || len(req.RowIDs) != 1 || req.RowIDs[0] != "d1" || req.Surface != "native" {
		t.Fatalf("custom decisionIds parse: got %+v, %v", req, ferr)
	}

	// Too many row IDs.
	_, ferr = ParseDismissRequest(strings.NewReader(`{"decisionIds":["1","2","3"]}`), DismissParseOptions{
		RowIDsField: "decisionIds",
		MaxRowIDs:   2,
	})
	if ferr == nil || ferr.Field != "decisionIds" {
		t.Fatalf("expected max row ids error, got %+v", ferr)
	}

	// Idempotency key too long.
	longKey := strings.Repeat("x", MaxIdempotencyKeyLen+1)
	_, ferr = ParseDismissRequest(strings.NewReader(`{"idempotencyKey":"`+longKey+`"}`), DismissParseOptions{})
	if ferr == nil || ferr.Field != "idempotencyKey" {
		t.Fatalf("expected idempotencyKey error, got %+v", ferr)
	}

	// Invalid surface.
	_, ferr = ParseDismissRequest(strings.NewReader(`{"surface":"bogus"}`), DismissParseOptions{
		NormalizeSurface: func(string) (string, bool) { return "", false },
		SurfaceError:     func(raw string) string { return "bad surface " + raw },
	})
	if ferr == nil || ferr.Field != "surface" || ferr.Message != "bad surface bogus" {
		t.Fatalf("expected surface error, got %+v", ferr)
	}
}
