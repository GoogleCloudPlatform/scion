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
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"
)

// Target is the tmux target every step addresses: the single window every
// agent harness runs in. It is a constant, never taken from a request.
const Target = "scion:0"

// BufferPrefix starts every per-delivery tmux buffer name.
const BufferPrefix = "scion-msg"

// Runner runs one command in the agent container. argv is passed as is, with
// no shell; stdin, when not nil, is streamed to the command's standard input.
// It returns the command's standard output.
//
// The Runtime Broker's Runner runs argv through the runtime's exec; the
// sciontool Runner runs it locally.
type Runner interface {
	Run(ctx context.Context, argv []string, stdin io.Reader) (string, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, argv []string, stdin io.Reader) (string, error)

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, argv []string, stdin io.Reader) (string, error) {
	return f(ctx, argv, stdin)
}

// LoadBufferArgv loads text from stdin into the named tmux buffer. The text
// goes through stdin, not argv: tmux caps one command's argv at 16 KB, and a
// coalesced batch of messages can exceed that (ptone/scion#2256).
func LoadBufferArgv(bufName string) []string {
	return []string{"tmux", "load-buffer", "-b", bufName, "-"}
}

// PasteBufferArgv pastes the named buffer into Target with bracketed paste
// (-p), deleting the buffer on success (-d). Bracketed paste makes the
// harness treat the text as pasted, not typed, so characters such as '!' are
// not read as hotkeys.
func PasteBufferArgv(bufName string) []string {
	return []string{"tmux", "paste-buffer", "-t", Target, "-p", "-d", "-b", bufName}
}

// SendKeyArgv sends one tmux key name to Target.
func SendKeyArgv(key string) []string {
	return []string{"tmux", "send-keys", "-t", Target, key}
}

// DeleteBufferArgv deletes the named buffer. It cleans up after a failed
// paste, since paste-buffer's -d does not run when the paste fails.
func DeleteBufferArgv(bufName string) []string {
	return []string{"tmux", "delete-buffer", "-b", bufName}
}

// deliveryIDPattern is the shape of the id in a buffer name: the
// deliver-message route's delivery_id, or the broker's per-process nonce.
var deliveryIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// BufferName returns the tmux buffer name for one delivery:
// "scion-msg-<id>-<n>". Every step of a delivery uses the same name
// (ptone/scion#2265: a shared fixed name lets one delivery's paste consume
// another's buffer).
func BufferName(id string, n uint64) (string, error) {
	if !deliveryIDPattern.MatchString(id) {
		return "", fmt.Errorf("tmuxdeliver: invalid buffer id %q", id)
	}
	return BufferPrefix + "-" + id + "-" + strconv.FormatUint(n, 10), nil
}

// Op is one delivery shape. Each matches what the broker's delivery does
// today for the same input (design v2.22 §1.2, §3.1).
type Op string

const (
	// OpPaste pastes the text, then sends the submit keys and the extra
	// submits.
	OpPaste Op = "paste"
	// OpInterruptPaste sends the interrupt keys, then does what OpPaste
	// does, in one delivery.
	OpInterruptPaste Op = "interrupt_paste"
	// OpSubmitOnly sends the submit keys once, with no text and no extra
	// submits: an empty message, used to answer confirmations.
	OpSubmitOnly Op = "submit_only"
	// OpInterruptSubmit sends the interrupt keys, then the submit keys once:
	// an interrupt with an empty message.
	OpInterruptSubmit Op = "interrupt_submit"
)

// pastes reports whether op pastes text.
func (op Op) pastes() bool { return op == OpPaste || op == OpInterruptPaste }

// interrupts reports whether op sends interrupt keys.
func (op Op) interrupts() bool { return op == OpInterruptPaste || op == OpInterruptSubmit }

// Valid reports whether op is one of the four ops.
func (op Op) Valid() bool {
	switch op {
	case OpPaste, OpInterruptPaste, OpSubmitOnly, OpInterruptSubmit:
		return true
	}
	return false
}

// Request is one delivery.
type Request struct {
	Op Op
	// Text is the message, required and non-empty for the paste ops and
	// absent for the others. Deliver sanitises it (Sanitize) before loading
	// it.
	Text string
	// InterruptKeys are sent first by the interrupt ops: the harness's
	// interrupt sequence, or its single interrupt key.
	InterruptKeys []string
	// SubmitKeys are sent after the paste (or alone for the submit ops), one
	// send-keys call per key.
	SubmitKeys []string
	// ExtraSubmits is how many more times SubmitKeys is sent after a paste,
	// each pass after ExtraSubmitDelay.
	ExtraSubmits     int
	ExtraSubmitDelay time.Duration
	// BufferName names the tmux buffer of a paste op (BufferName).
	BufferName string
}

// StepKind says how a step runs.
type StepKind int

const (
	// StepSendKeys sends one key.
	StepSendKeys StepKind = iota
	// StepLoadBuffer streams the text into the buffer on stdin.
	StepLoadBuffer
	// StepPasteBuffer pastes the buffer. Its success means the text is in
	// the harness's input: any later failure is a partial delivery.
	StepPasteBuffer
)

// Step is one command of a delivery.
type Step struct {
	Kind StepKind
	Argv []string
}

// Errors from Steps and Deliver for a request that cannot be delivered.
// They all happen before anything is sent.
var (
	ErrInvalidOp     = errors.New("tmuxdeliver: invalid op")
	ErrTextRequired  = errors.New("tmuxdeliver: text is required for a paste op")
	ErrTextForbidden = errors.New("tmuxdeliver: text is not allowed for a submit op")
	ErrNoSubmitKeys  = errors.New("tmuxdeliver: no submit keys")
	ErrNoInterrupt   = errors.New("tmuxdeliver: no interrupt keys for an interrupt op")
	ErrStrayKeys     = errors.New("tmuxdeliver: interrupt keys given for an op that does not interrupt")
	ErrNoBufferName  = errors.New("tmuxdeliver: no buffer name for a paste op")
	// ErrEmptyAfterSanitize: the text had only characters Sanitize removes,
	// so there is nothing to paste.
	ErrEmptyAfterSanitize = errors.New("tmuxdeliver: text is empty after sanitising")
)

// Steps returns the commands of req, in order, without the extra submits
// (Deliver runs those after the paste, with their delay). It checks the
// request's shape: the op, the text presence, the keys each op needs and the
// buffer name. It does not apply the deliver-message route bounds (the key
// allowlist and the counts; CheckRoute): the broker's exec path delivers with
// the harness config's own keys, as before. It does not sanitise the text;
// Deliver does.
func Steps(req Request) ([]Step, error) {
	if !req.Op.Valid() {
		return nil, fmt.Errorf("%w %q", ErrInvalidOp, req.Op)
	}
	if req.Op.pastes() && req.Text == "" {
		return nil, ErrTextRequired
	}
	if !req.Op.pastes() && req.Text != "" {
		return nil, ErrTextForbidden
	}
	if len(req.SubmitKeys) == 0 {
		return nil, ErrNoSubmitKeys
	}
	if req.Op.interrupts() {
		if len(req.InterruptKeys) == 0 {
			return nil, ErrNoInterrupt
		}
	} else if len(req.InterruptKeys) > 0 {
		return nil, ErrStrayKeys
	}
	if req.Op.pastes() && req.BufferName == "" {
		return nil, ErrNoBufferName
	}

	var steps []Step
	if req.Op.interrupts() {
		for _, k := range req.InterruptKeys {
			steps = append(steps, Step{Kind: StepSendKeys, Argv: SendKeyArgv(k)})
		}
	}
	if req.Op.pastes() {
		steps = append(steps,
			Step{Kind: StepLoadBuffer, Argv: LoadBufferArgv(req.BufferName)},
			Step{Kind: StepPasteBuffer, Argv: PasteBufferArgv(req.BufferName)})
	}
	for _, k := range req.SubmitKeys {
		steps = append(steps, Step{Kind: StepSendKeys, Argv: SendKeyArgv(k)})
	}
	return steps, nil
}
