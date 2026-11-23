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
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestExecCapture_DefaultWireGolden verifies A1: with no capture mode set, the
// wire request and response JSON omit capture, output_status, and output.
func TestExecCapture_DefaultWireGolden(t *testing.T) {
	req := ExecStartRequest{
		V:              ExecVersion,
		RunID:          "00000000-0000-4000-8000-000000000001",
		Argv:           []string{"echo", "hi"},
		TimeoutS:       30,
		NotAfterUnixMS: 1800000000000,
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	wantReq := `{"v":1,"run_id":"00000000-0000-4000-8000-000000000001","argv":["echo","hi"],"timeout_s":30,"not_after_unix_ms":1800000000000}`
	if string(b) != wantReq {
		t.Fatalf("default ExecStartRequest JSON = %s, want %s", b, wantReq)
	}

	code, dur, out := 0, int64(5), int64(3)
	resp := ExecResponse{
		State:       ExecStateFinished,
		Outcome:     ExecOutcomeOK,
		ExitCode:    &code,
		DurationMS:  &dur,
		OutputBytes: &out,
	}
	rb, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	wantResp := `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":5,"output_bytes":3}`
	if string(rb) != wantResp {
		t.Fatalf("default ExecResponse JSON = %s, want %s", rb, wantResp)
	}
}

func TestExecStartRequest_CaptureValidation(t *testing.T) {
	base := func(capture string) ExecStartRequest {
		return ExecStartRequest{
			V:              ExecVersion,
			RunID:          "00000000-0000-4000-8000-000000000001",
			Argv:           []string{"true"},
			TimeoutS:       10,
			NotAfterUnixMS: 1,
			Capture:        capture,
		}
	}
	for _, valid := range []string{"", ExecCaptureJSON, ExecCaptureTail} {
		r := base(valid)
		if err := r.Validate(); err != nil {
			t.Errorf("capture %q: unexpected error %v", valid, err)
		}
	}
	for _, invalid := range []string{"none", "raw", "JSON", "stdout"} {
		r := base(invalid)
		err := r.Validate()
		var fe *ExecFieldError
		if !errors.As(err, &fe) || fe.Field != FieldCapture {
			t.Errorf("capture %q: got err=%v, want ExecFieldError{Field: %q}", invalid, err, FieldCapture)
		}
	}
}

// TestParseExecAnswers_CaptureContract verifies A3 and A6: output fields are
// rejected on exec-start, on non-capture runs, on running/refusal/start_failed,
// and on invalid/oversize payloads; worst-case tail encoding fits MaxRPCBody.
func TestParseExecAnswers_CaptureContract(t *testing.T) {
	// exec-start never carries output_status or output, even on 200 finished replay.
	badStart := []struct {
		status int
		body   string
	}{
		{200, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":7,"replayed":true,"output_status":"captured","output":{"a":1}}`},
		{200, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":0,"replayed":true,"output_status":"empty"}`},
		{202, `{"state":"running","output_status":"empty"}`},
		{202, `{"state":"finished","outcome":"start_failed","output_status":"empty"}`},
		{409, `{"outcome":"busy","output_status":"empty"}`},
	}
	for _, tc := range badStart {
		if _, err := ParseExecStartAnswer(tc.status, []byte(tc.body)); !errors.Is(err, ErrExecSchema) {
			t.Errorf("ParseExecStartAnswer(%d, %s): want ErrExecSchema, got %v", tc.status, tc.body, err)
		}
	}

	// Valid exec-status answers for json and tail.
	res, err := ParseExecStatusAnswer(http.StatusOK,
		[]byte(`{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":5,"output_bytes":7,"output_status":"captured","output":{"a":1}}`),
		ExecCaptureJSON)
	if err != nil {
		t.Fatalf("valid json captured: %v", err)
	}
	if res.OutputStatus != ExecOutputCaptured || string(res.Output) != `{"a":1}` {
		t.Fatalf("got status=%q output=%q", res.OutputStatus, res.Output)
	}

	res, err = ParseExecStatusAnswer(http.StatusOK,
		[]byte(`{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":5,"output_bytes":4,"output_status":"captured","output":null}`),
		ExecCaptureJSON)
	if err != nil || string(res.Output) != "null" {
		t.Fatalf("json literal null should be valid JSON: res=%+v err=%v", res, err)
	}

	res, err = ParseExecStatusAnswer(http.StatusOK,
		[]byte(`{"state":"finished","outcome":"nonzero","exit_code":2,"duration_ms":5,"output_bytes":10,"output_status":"invalid_json"}`),
		ExecCaptureJSON)
	if err != nil || res.OutputStatus != ExecOutputInvalidJSON || len(res.Output) != 0 {
		t.Fatalf("json invalid_json: res=%+v err=%v", res, err)
	}

	res, err = ParseExecStatusAnswer(http.StatusOK,
		[]byte(`{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":5,"output_bytes":100,"output_status":"truncated","output":"hello\n"}`),
		ExecCaptureTail)
	if err != nil || res.OutputStatus != ExecOutputTruncated || string(res.Output) != "hello\n" {
		t.Fatalf("tail truncated: res=%+v err=%v", res, err)
	}

	// Oversize json (> 64 KiB).
	overJSON := `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":70000,"output_status":"captured","output":"` + strings.Repeat("a", MaxExecCaptureBytes) + `"}`
	// Oversize tail (> 64 KiB decoded).
	overTail := `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":70000,"output_status":"truncated","output":"` + strings.Repeat("a", MaxExecCaptureBytes+1) + `"}`

	badStatus := []struct {
		name    string
		status  int
		capture string
		body    string
	}{
		{"non-capture with output_status", 200, ExecCaptureNone, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":0,"output_status":"empty"}`},
		{"non-capture with output", 200, ExecCaptureNone, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":2,"output":{}}`},
		{"running with output_status", 200, ExecCaptureJSON, `{"state":"running","output_status":"empty"}`},
		{"start_failed with output_status", 200, ExecCaptureJSON, `{"state":"finished","outcome":"start_failed","output_status":"empty"}`},
		{"404 with output_status", 404, ExecCaptureJSON, `{"state":"unknown","output_status":"empty"}`},
		{"capture missing output_status", 200, ExecCaptureJSON, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":0}`},
		{"wire lost status rejected", 200, ExecCaptureJSON, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":0,"output_status":"lost"}`},
		{"unknown output_status", 200, ExecCaptureJSON, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":0,"output_status":"bogus"}`},
		{"captured missing output", 200, ExecCaptureJSON, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":1,"output_status":"captured"}`},
		{"empty with output present", 200, ExecCaptureJSON, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":0,"output_status":"empty","output":{}}`},
		{"too_large with output present", 200, ExecCaptureJSON, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":70000,"output_status":"too_large","output":{}}`},
		{"expired with output present", 200, ExecCaptureJSON, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":2,"output_status":"expired","output":{}}`},
		{"json oversize", 200, ExecCaptureJSON, overJSON},
		{"tail oversize", 200, ExecCaptureTail, overTail},
		{"tail non-string object", 200, ExecCaptureTail, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":2,"output_status":"captured","output":{"a":1}}`},
		{"tail null rejected", 200, ExecCaptureTail, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":4,"output_status":"captured","output":null}`},
		{"tail empty string rejected", 200, ExecCaptureTail, `{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":1,"output_bytes":0,"output_status":"captured","output":""}`},
	}
	for _, tc := range badStatus {
		if _, err := ParseExecStatusAnswer(tc.status, []byte(tc.body), tc.capture); !errors.Is(err, ErrExecSchema) {
			t.Errorf("%s: want ErrExecSchema, got %v", tc.name, err)
		}
	}

	// A6: worst-case tail encoding (64 KiB of \x01 -> 6x escaping = 384 KiB)
	// fits well within Conduit MaxRPCBody (768 KiB).
	worstTail := strings.Repeat("\x01", MaxExecCaptureBytes)
	encodedTail, err := json.Marshal(worstTail)
	if err != nil {
		t.Fatal(err)
	}
	code, dur, n := 0, int64(10), int64(MaxExecCaptureBytes)
	wireResp, err := json.Marshal(ExecResponse{
		State:        ExecStateFinished,
		Outcome:      ExecOutcomeOK,
		ExitCode:     &code,
		DurationMS:   &dur,
		OutputBytes:  &n,
		OutputStatus: ExecOutputTruncated,
		Output:       json.RawMessage(encodedTail),
	})
	if err != nil {
		t.Fatal(err)
	}
	const maxRPCBody = 768 * 1024
	if len(wireResp) >= maxRPCBody {
		t.Fatalf("worst-case tail response = %d bytes, must be < MaxRPCBody (%d)", len(wireResp), maxRPCBody)
	}
	parsed, err := ParseExecStatusAnswer(http.StatusOK, wireResp, ExecCaptureTail)
	if err != nil || string(parsed.Output) != worstTail {
		t.Fatalf("worst-case tail round-trip failed: err=%v len=%d", err, len(parsed.Output))
	}
}
