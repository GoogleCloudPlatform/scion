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

package agentcontrol

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
)

func TestExecArgvDigest(t *testing.T) {
	// sha256 of the canonical JSON ["echo","hi"].
	sum := sha256.Sum256([]byte(`["echo","hi"]`))
	if got, want := ExecArgvDigest([]string{"echo", "hi"}), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
	a := ExecArgvDigest([]string{"a b"})
	b := ExecArgvDigest([]string{"a", "b"})
	if a == b {
		t.Fatal("argv boundaries must change the digest")
	}
	if ExecArgvDigest(nil) != ExecArgvDigest([]string{}) {
		t.Fatal("nil and empty argv must agree")
	}
}

func TestParseExecStartAnswer(t *testing.T) {
	ok := []struct {
		status  int
		body    string
		state   string
		outcome string
		started bool
	}{
		{202, `{"state":"running"}`, ExecStateRunning, "", true},
		{202, `{"state":"finished","outcome":"start_failed"}`, ExecStateFinished, ExecOutcomeStartFailed, true},
		{200, `{"state":"running","replayed":true}`, ExecStateRunning, "", true},
		{200, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":5,"output_bytes":9,"replayed":true}`, ExecStateFinished, ExecOutcomeOK, true},
		{200, `{"state":"finished","outcome":"timeout","exit_code":143,"duration_ms":5,"output_bytes":0,"replayed":true}`, ExecStateFinished, ExecOutcomeTimeout, true},
		{409, `{"outcome":"overlap"}`, "", ExecOutcomeOverlap, false},
		{409, `{"outcome":"busy"}`, "", ExecOutcomeBusy, false},
		{409, `{"outcome":"mismatch"}`, "", ExecOutcomeMismatch, false},
		{408, `{"outcome":"expired_before_start"}`, "", ExecOutcomeExpiredBeforeStart, false},
		{400, `{"outcome":"invalid","field":"argv"}`, "", ExecOutcomeInvalid, false},
	}
	for _, c := range ok {
		res, err := ParseExecStartAnswer(c.status, []byte(c.body))
		if err != nil {
			t.Errorf("%d %s: %v", c.status, c.body, err)
			continue
		}
		if res.State != c.state || res.Outcome != c.outcome || res.Started() != c.started {
			t.Errorf("%d %s: got %+v", c.status, c.body, res)
		}
	}
	bad := []struct {
		status int
		body   string
	}{
		{202, `{"state":"running","replayed":true}`},
		{202, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":1}`},
		{202, `{}`},
		{200, `{"state":"running"}`}, // not replayed
		{200, `{"state":"finished","outcome":"ok","replayed":true}`},    // no numbers
		{200, `{"state":"finished","outcome":"weird","replayed":true}`}, // unknown outcome
		{200, `{"state":"finished","outcome":"ok","exit_code":2,"duration_ms":1,"output_bytes":1,"replayed":true}`},
		{200, `{"state":"finished","outcome":"nonzero","exit_code":0,"duration_ms":1,"output_bytes":1,"replayed":true}`},
		{200, `{"state":"running","outcome":"ok","replayed":true}`},
		{200, `{"state":"running","replayed":true,"stdout":"x"}`}, // unknown field
		{200, `{"state":"running","replayed":true} trailing`},
		{409, `{"outcome":"expired_before_start"}`},
		{409, `{"outcome":"busy","state":"running"}`},
		{408, `{"outcome":"busy"}`},
		{400, `{"outcome":"busy"}`},
		{500, `{"outcome":"busy"}`},
		{404, `{"state":"unknown"}`},
		{202, `not json`},
		{202, ``},
	}
	for _, c := range bad {
		if res, err := ParseExecStartAnswer(c.status, []byte(c.body)); !errors.Is(err, ErrExecSchema) {
			t.Errorf("%d %s: got %+v, %v; want a schema error", c.status, c.body, res, err)
		}
	}
}

func TestParseExecStatusAnswer(t *testing.T) {
	res, err := ParseExecStatusAnswer(http.StatusOK, []byte(`{"state":"finished","outcome":"nonzero","exit_code":3,"duration_ms":10,"output_bytes":2}`))
	if err != nil || !res.Finished() || res.ExitCode != 3 || res.DurationMS != 10 || res.OutputBytes != 2 {
		t.Fatalf("finished: %+v %v", res, err)
	}
	if res, err := ParseExecStatusAnswer(http.StatusOK, []byte(`{"state":"running"}`)); err != nil || res.State != ExecStateRunning {
		t.Fatalf("running: %+v %v", res, err)
	}
	if res, err := ParseExecStatusAnswer(http.StatusNotFound, []byte(`{"state":"unknown"}`)); err != nil || res.State != ExecStateUnknown {
		t.Fatalf("unknown: %+v %v", res, err)
	}
	for _, c := range []struct {
		status int
		body   string
	}{
		{200, `{"state":"running","replayed":true}`},
		{200, `{"state":"unknown"}`},
		{404, `{"state":"unknown","outcome":"ok"}`},
		{404, `page not found`},
		{400, `{"outcome":"invalid","field":"run_id"}`},
		{200, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":-1,"output_bytes":0}`},
	} {
		if res, err := ParseExecStatusAnswer(c.status, []byte(c.body)); !errors.Is(err, ErrExecSchema) {
			t.Errorf("%d %s: got %+v, %v; want a schema error", c.status, c.body, res, err)
		}
	}
}

func TestExecStartRequestValidateIsFieldOnly(t *testing.T) {
	r := ExecStartRequest{V: 1, RunID: "00000000-0000-4000-8000-000000000001", Argv: []string{"SECRET-ARG\x00"}, TimeoutS: 1, NotAfterUnixMS: 1}
	err := r.Validate()
	var fe *ExecFieldError
	if !errors.As(err, &fe) || fe.Field != FieldArgv {
		t.Fatalf("err = %v", err)
	}
	if got := err.Error(); got != "exec control: invalid field argv" {
		t.Fatalf("error text %q must name the field only", got)
	}
}
