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

package tmuxdeliver

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// cleanupTimeout bounds the delete-buffer run after a failed paste.
const cleanupTimeout = 5 * time.Second

// PartialError reports a failure after the paste succeeded: the text is
// already in the harness's input, so the delivery must not be retried, or
// the text would be pasted twice (ptone/scion#1866).
type PartialError struct {
	// Stage is "submit" (a submit key right after the paste) or
	// "extra_submit".
	Stage string
	Err   error
}

func (e *PartialError) Error() string {
	return fmt.Sprintf("partial delivery (%s): %v", e.Stage, e.Err)
}

func (e *PartialError) Unwrap() error { return e.Err }

// Partial stages.
const (
	StageSubmit      = "submit"
	StageExtraSubmit = "extra_submit"
)

// Deliver runs req through r. The caller serialises deliveries to one
// session; Deliver does not lock.
//
// A paste op's text is sanitised first (Sanitize) and loaded on
// load-buffer's stdin. A failure before the paste returns the error as is;
// nothing is in the harness's input, so the caller may retry. If the paste
// itself fails, the buffer is deleted (best effort, on a context detached
// from ctx so a cancelled ctx does not skip it). A failure after the paste
// returns a *PartialError. The extra submits run only after a paste.
func Deliver(ctx context.Context, r Runner, req Request) error {
	text := ""
	if req.Op.pastes() {
		text = Sanitize(req.Text)
		if req.Text != "" && text == "" {
			return ErrEmptyAfterSanitize
		}
	}
	steps, err := Steps(req)
	if err != nil {
		return err
	}

	pasted := false
	for _, st := range steps {
		var stepErr error
		if st.Kind == StepLoadBuffer {
			_, stepErr = r.Run(ctx, st.Argv, strings.NewReader(text))
		} else {
			_, stepErr = r.Run(ctx, st.Argv, nil)
		}
		if stepErr != nil {
			if st.Kind == StepPasteBuffer {
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
				_, _ = r.Run(cctx, DeleteBufferArgv(req.BufferName), nil)
				cancel()
			}
			if pasted {
				return &PartialError{Stage: StageSubmit, Err: stepErr}
			}
			return stepErr
		}
		if st.Kind == StepPasteBuffer {
			pasted = true
		}
	}

	if !req.Op.pastes() {
		return nil
	}
	for range req.ExtraSubmits {
		t := time.NewTimer(req.ExtraSubmitDelay)
		select {
		case <-ctx.Done():
			t.Stop()
			return &PartialError{Stage: StageExtraSubmit, Err: ctx.Err()}
		case <-t.C:
		}
		for _, k := range req.SubmitKeys {
			if _, err := r.Run(ctx, SendKeyArgv(k), nil); err != nil {
				return &PartialError{Stage: StageExtraSubmit, Err: err}
			}
		}
	}
	return nil
}

// CheckRoute checks req against the deliver-message route bounds (design
// v2.22 §1.7, §5.3): every key on the allowlist (deny by default, exact
// match), at most MaxKeys per list, and the extra submit count and delay. The
// route refuses a request that fails, and the broker does not send one over
// the route. Deliver itself does not apply these bounds, so the exec path
// keeps the harness config's own limits.
func CheckRoute(req Request) error {
	if err := CheckKeys(FieldSubmitKeys, req.SubmitKeys); err != nil {
		return err
	}
	if err := CheckKeys(FieldInterruptKeys, req.InterruptKeys); err != nil {
		return err
	}
	if req.ExtraSubmits < 0 || req.ExtraSubmits > MaxExtraSubmits {
		return &BoundsError{Field: FieldExtraSubmits}
	}
	if req.ExtraSubmitDelay < 0 || req.ExtraSubmitDelay > MaxExtraSubmitDelay {
		return &BoundsError{Field: FieldExtraSubmitDelay}
	}
	return nil
}
