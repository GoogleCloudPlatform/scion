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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// The exec control routes (exec-schedules design "sciontool exec control
// routes", D9 = b'; .design/conduit/exec-control-routes.md). They are the
// one bounded exception to the Conduit rule "no generic exec over conduit":
// only the scheduler calls them, for exec events, after the full fire-time
// authorization.
const (
	// RouteExecStart starts one scheduled run, keyed by run_id.
	RouteExecStart = "exec-start"
	// RouteExecStatus reads a run's state. It never starts anything.
	RouteExecStatus = "exec-status"
)

// ExecVersion is the only accepted request version of both routes.
const ExecVersion = 1

// Bounds and timings of the exec routes. The argv bounds are the hub's
// payload bounds, checked again by sciontool.
const (
	MaxExecArgc       = 64
	MaxExecArgBytes   = 4096
	MaxExecArgvBytes  = 16384
	MaxExecTimeoutSec = 3600 // D10 = b
	// ExecNotAfter is how far after its send the hub sets
	// not_after_unix_ms.
	ExecNotAfter = 60 * time.Second
	// ExecClockTolerance is the fixed tolerance on not_after_unix_ms
	// (D9-S1): sciontool spawns only while now <= not_after + tolerance.
	ExecClockTolerance = 10 * time.Second
	// ExecSpawnWithin bounds receipt to spawn: a start that cannot begin
	// spawning within it is refused.
	ExecSpawnWithin = 10 * time.Second
	// ExecKillGrace is the time between SIGTERM and SIGKILL to the run's
	// process group on timeout.
	ExecKillGrace = 5 * time.Second
	// ExecWaitDelay bounds how long a finished run waits for children
	// that still hold its output pipes.
	ExecWaitDelay = 2 * time.Second
	// HubExecCallTimeout is the hub's call timeout for both routes.
	HubExecCallTimeout = 30 * time.Second
)

// ExecStartRequest is the exec-start request body.
type ExecStartRequest struct {
	V              int      `json:"v"`
	RunID          string   `json:"run_id"`
	ScheduleID     string   `json:"schedule_id,omitempty"`
	Argv           []string `json:"argv"`
	TimeoutS       int      `json:"timeout_s"`
	NotAfterUnixMS int64    `json:"not_after_unix_ms"`
}

// ExecStatusRequest is the exec-status request body.
type ExecStatusRequest struct {
	V     int    `json:"v"`
	RunID string `json:"run_id"`
}

// Fields an exec FieldError names (FieldBody, FieldV and FieldNotAfter
// are shared with deliver-message).
const (
	FieldRunID      = "run_id"
	FieldScheduleID = "schedule_id"
	FieldArgv       = "argv"
	FieldTimeoutS   = "timeout_s"
)

// States in an exec answer.
const (
	ExecStateRunning  = "running"
	ExecStateFinished = "finished"
	ExecStateUnknown  = "unknown"
)

// Outcomes in an exec answer. The first four are the final outcomes of a
// run (exec-status, or a replayed exec-start of a finished run); the
// others are exec-start refusals, where nothing started.
const (
	ExecOutcomeOK          = "ok"
	ExecOutcomeNonzero     = "nonzero"
	ExecOutcomeTimeout     = "timeout"
	ExecOutcomeStartFailed = "start_failed"

	ExecOutcomeOverlap            = "overlap"
	ExecOutcomeBusy               = "busy"
	ExecOutcomeExpiredBeforeStart = "expired_before_start"
	ExecOutcomeMismatch           = "mismatch"
	ExecOutcomeInvalid            = OutcomeInvalid
)

// uuidPattern is the canonical, lower-case UUID text form.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidRunID reports whether id is a canonical lower-case UUID, the form
// of run and schedule ids.
func ValidRunID(id string) bool { return uuidPattern.MatchString(id) }

// ExecFieldError reports the first exec request field that is malformed
// or outside the route bounds. Its message names the field only, never a
// value.
type ExecFieldError struct {
	Field string
}

func (e *ExecFieldError) Error() string {
	return "exec control: invalid field " + e.Field
}

func execFieldError(field string) error { return &ExecFieldError{Field: field} }

// decodeStrict decodes body into v: unknown fields, a second JSON value or
// trailing data are an ExecFieldError on FieldBody.
func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return execFieldError(FieldBody)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return execFieldError(FieldBody)
	}
	return nil
}

// DecodeExecStartRequest decodes body strictly. It does not validate.
func DecodeExecStartRequest(body []byte) (*ExecStartRequest, error) {
	var r ExecStartRequest
	if err := decodeStrict(body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// DecodeExecStatusRequest decodes body strictly. It does not validate.
func DecodeExecStatusRequest(body []byte) (*ExecStatusRequest, error) {
	var r ExecStatusRequest
	if err := decodeStrict(body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ValidateExecArgv checks argv against the route bounds: 1..MaxExecArgc
// elements, argv[0] not empty, no NUL, each element at most
// MaxExecArgBytes and all together at most MaxExecArgvBytes.
func ValidateExecArgv(argv []string) error {
	if len(argv) == 0 || len(argv) > MaxExecArgc || argv[0] == "" {
		return execFieldError(FieldArgv)
	}
	total := 0
	for _, a := range argv {
		if len(a) > MaxExecArgBytes || strings.IndexByte(a, 0) >= 0 {
			return execFieldError(FieldArgv)
		}
		total += len(a)
	}
	if total > MaxExecArgvBytes {
		return execFieldError(FieldArgv)
	}
	return nil
}

// Validate checks r against the fixed schema and bounds and returns a
// *ExecFieldError naming the first field that fails, in a fixed order. It does
// not judge not_after_unix_ms against a clock; only that it is set.
func (r *ExecStartRequest) Validate() error {
	if r.V != ExecVersion {
		return execFieldError(FieldV)
	}
	if !ValidRunID(r.RunID) {
		return execFieldError(FieldRunID)
	}
	if r.ScheduleID != "" && !ValidRunID(r.ScheduleID) {
		return execFieldError(FieldScheduleID)
	}
	if err := ValidateExecArgv(r.Argv); err != nil {
		return err
	}
	if r.TimeoutS < 1 || r.TimeoutS > MaxExecTimeoutSec {
		return execFieldError(FieldTimeoutS)
	}
	if r.NotAfterUnixMS <= 0 {
		return execFieldError(FieldNotAfter)
	}
	return nil
}

// Validate checks an exec-status request.
func (r *ExecStatusRequest) Validate() error {
	if r.V != ExecVersion {
		return execFieldError(FieldV)
	}
	if !ValidRunID(r.RunID) {
		return execFieldError(FieldRunID)
	}
	return nil
}

// ExecArgvDigest is the hex sha256 of argv encoded as a canonical JSON
// array of strings: the command digest the hub records and sciontool binds
// a run_id to.
func ExecArgvDigest(argv []string) string {
	if argv == nil {
		argv = []string{}
	}
	b, _ := json.Marshal(argv) // a []string always encodes
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ExecResponse is every exec-start and exec-status answer body. A field
// that does not apply is omitted. It never carries command output, argv
// or error text.
type ExecResponse struct {
	State       string `json:"state,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	DurationMS  *int64 `json:"duration_ms,omitempty"`
	OutputBytes *int64 `json:"output_bytes,omitempty"`
	Replayed    bool   `json:"replayed,omitempty"`
	Field       string `json:"field,omitempty"`
}

// ExecResult is a decoded, schema-checked exec answer.
type ExecResult struct {
	// Status is the agent's HTTP status.
	Status int
	// State is running or finished for a run the agent knows, unknown for
	// a 404 from exec-status, and empty for an exec-start refusal.
	State string
	// Outcome is the final outcome of a finished run, or the refusal of an
	// exec-start (overlap, busy, expired_before_start, mismatch, invalid).
	Outcome     string
	ExitCode    int
	DurationMS  int64
	OutputBytes int64
	Replayed    bool
}

// Started reports whether the answer means the run_id is (or was)
// admitted: a 202 (running, or finished start_failed) or a 200 replay.
func (r ExecResult) Started() bool {
	return r.Status == http.StatusAccepted || r.Status == http.StatusOK
}

// Finished reports whether the answer carries a final run outcome.
func (r ExecResult) Finished() bool { return r.State == ExecStateFinished }

// ErrExecSchema: an answer that does not match the route's schema. A
// caller must record it as an error, never as success.
var ErrExecSchema = errors.New("exec control: answer does not match the schema")

func schemaErr(route string, status int, why string) error {
	return fmt.Errorf("%w: %s answered %d: %s", ErrExecSchema, route, status, why)
}

// decodeExecResponse decodes an answer body strictly.
func decodeExecResponse(body []byte) (ExecResponse, error) {
	var r ExecResponse
	if err := decodeStrict(body, &r); err != nil {
		return ExecResponse{}, err
	}
	return r, nil
}

// finalOutcome reports whether o is a final run outcome.
func finalOutcome(o string) bool {
	switch o {
	case ExecOutcomeOK, ExecOutcomeNonzero, ExecOutcomeTimeout, ExecOutcomeStartFailed:
		return true
	}
	return false
}

// runState checks the run-state part of an answer (running, or finished
// with a final outcome and its numbers) and fills res.
func runState(route string, status int, r ExecResponse, res *ExecResult) error {
	switch r.State {
	case ExecStateRunning:
		if r.Outcome != "" || r.ExitCode != nil || r.DurationMS != nil || r.OutputBytes != nil {
			return schemaErr(route, status, "running with an outcome")
		}
	case ExecStateFinished:
		if !finalOutcome(r.Outcome) {
			return schemaErr(route, status, "finished without a final outcome")
		}
		if r.Outcome != ExecOutcomeStartFailed && (r.ExitCode == nil || r.DurationMS == nil || r.OutputBytes == nil) {
			return schemaErr(route, status, "finished without exit_code, duration_ms and output_bytes")
		}
		if r.Outcome == ExecOutcomeOK && *r.ExitCode != 0 {
			return schemaErr(route, status, "ok with a non-zero exit code")
		}
		if r.Outcome == ExecOutcomeNonzero && *r.ExitCode == 0 {
			return schemaErr(route, status, "nonzero with exit code 0")
		}
		if r.ExitCode != nil {
			res.ExitCode = *r.ExitCode
		}
		if r.DurationMS != nil {
			res.DurationMS = *r.DurationMS
		}
		if r.OutputBytes != nil {
			res.OutputBytes = *r.OutputBytes
		}
		if res.DurationMS < 0 || res.OutputBytes < 0 {
			return schemaErr(route, status, "negative duration or byte count")
		}
	default:
		return schemaErr(route, status, "no run state")
	}
	res.State, res.Outcome = r.State, r.Outcome
	return nil
}

// ParseExecStartAnswer maps an exec-start answer. Every status and body
// the route documents decodes to an ExecResult; anything else (an unknown
// status, a body that does not decode or does not fit the status) is an
// error wrapping ErrExecSchema and must never be read as success.
func ParseExecStartAnswer(status int, body []byte) (ExecResult, error) {
	r, err := decodeExecResponse(body)
	if err != nil {
		return ExecResult{}, schemaErr(RouteExecStart, status, "malformed body")
	}
	res := ExecResult{Status: status, Replayed: r.Replayed}
	switch status {
	case http.StatusAccepted:
		// Admitted now: running, or finished start_failed when the spawn
		// itself failed (argv[0] not found, privilege drop refused).
		startFailed := r.State == ExecStateFinished && r.Outcome == ExecOutcomeStartFailed
		if (r.State != ExecStateRunning && !startFailed) || r.Replayed {
			return ExecResult{}, schemaErr(RouteExecStart, status, "want running or start_failed, not replayed")
		}
		return res, runState(RouteExecStart, status, r, &res)
	case http.StatusOK:
		if !r.Replayed {
			return ExecResult{}, schemaErr(RouteExecStart, status, "200 without replayed")
		}
		return res, runState(RouteExecStart, status, r, &res)
	case http.StatusConflict, http.StatusRequestTimeout, http.StatusBadRequest:
		want := map[int][]string{
			http.StatusConflict:       {ExecOutcomeOverlap, ExecOutcomeBusy, ExecOutcomeMismatch},
			http.StatusRequestTimeout: {ExecOutcomeExpiredBeforeStart},
			http.StatusBadRequest:     {ExecOutcomeInvalid},
		}[status]
		ok := false
		for _, o := range want {
			ok = ok || r.Outcome == o
		}
		if !ok || r.State != "" || r.Replayed || r.ExitCode != nil || r.DurationMS != nil || r.OutputBytes != nil {
			return ExecResult{}, schemaErr(RouteExecStart, status, "unexpected refusal body")
		}
		res.Outcome = r.Outcome
		return res, nil
	}
	return ExecResult{}, schemaErr(RouteExecStart, status, "unexpected status")
}

// ParseExecStatusAnswer maps an exec-status answer: 200 running or
// finished, or 404 unknown. Anything else is an error wrapping
// ErrExecSchema (a 400 included: the hub sent a malformed request).
func ParseExecStatusAnswer(status int, body []byte) (ExecResult, error) {
	r, err := decodeExecResponse(body)
	if err != nil {
		return ExecResult{}, schemaErr(RouteExecStatus, status, "malformed body")
	}
	res := ExecResult{Status: status}
	switch status {
	case http.StatusOK:
		if r.Replayed {
			return ExecResult{}, schemaErr(RouteExecStatus, status, "replayed on a status read")
		}
		return res, runState(RouteExecStatus, status, r, &res)
	case http.StatusNotFound:
		if r != (ExecResponse{State: ExecStateUnknown}) {
			return ExecResult{}, schemaErr(RouteExecStatus, status, "want state unknown only")
		}
		res.State = ExecStateUnknown
		return res, nil
	}
	return ExecResult{}, schemaErr(RouteExecStatus, status, "unexpected status")
}
