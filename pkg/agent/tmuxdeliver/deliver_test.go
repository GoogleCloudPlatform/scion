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

package tmuxdeliver_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
)

// call is one Runner.Run the fake saw.
type call struct {
	argv  []string
	stdin string
	// hasStdin tells a nil stdin from an empty one.
	hasStdin bool
	// ctxErr is the context's error when the call was made.
	ctxErr error
}

// fakeRunner records calls and fails the one whose argv[1] (the tmux verb)
// matches failOn, on its failAt-th occurrence (1-based; 0 means the first).
type fakeRunner struct {
	mu     sync.Mutex
	calls  []call
	failOn string
	failAt int
	seen   map[string]int
	out    string
}

var errStep = errors.New("step failed")

func (f *fakeRunner) Run(ctx context.Context, argv []string, stdin io.Reader) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := call{argv: append([]string(nil), argv...), ctxErr: ctx.Err()}
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		c.stdin, c.hasStdin = string(b), true
	}
	f.calls = append(f.calls, c)
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	verb := ""
	if len(argv) > 1 {
		verb = argv[1]
	}
	f.seen[verb]++
	if f.failOn != "" && verb == f.failOn {
		at := f.failAt
		if at == 0 {
			at = 1
		}
		if f.seen[verb] == at {
			return "", errStep
		}
	}
	return f.out, nil
}

func (f *fakeRunner) argvs() [][]string {
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.argv
	}
	return out
}

const buf = "scion-msg-0123abcd-7"

func keys(ks ...string) [][]string {
	var out [][]string
	for _, k := range ks {
		out = append(out, []string{"tmux", "send-keys", "-t", "scion:0", k})
	}
	return out
}

func cat(parts ...[][]string) [][]string {
	var out [][]string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var (
	load  = [][]string{{"tmux", "load-buffer", "-b", buf, "-"}}
	paste = [][]string{{"tmux", "paste-buffer", "-t", "scion:0", "-p", "-d", "-b", buf}}
)

// TestDeliver_Parity pins the command sequence of each op against what the
// broker's deliverImmediate runs today for the same input. Parity holds for
// text without control characters; TestDeliver_SanitisedText covers the rest,
// which Sanitize changes by design.
func TestDeliver_Parity(t *testing.T) {
	cases := []struct {
		name string
		req  tmuxdeliver.Request
		want [][]string
	}{
		{
			name: "paste with default submit and two extra submits",
			req:  tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, Text: "hello world", SubmitKeys: []string{"Enter"}, ExtraSubmits: 2, BufferName: buf},
			want: cat(load, paste, keys("Enter"), keys("Enter"), keys("Enter")),
		},
		{
			name: "interrupt sequence then paste, one delivery",
			req:  tmuxdeliver.Request{Op: tmuxdeliver.OpInterruptPaste, Text: "stop and do this", InterruptKeys: []string{"Escape", "C-c"}, SubmitKeys: []string{"Enter"}, ExtraSubmits: 1, BufferName: buf},
			want: cat(keys("Escape", "C-c"), load, paste, keys("Enter"), keys("Enter")),
		},
		{
			name: "empty message sends submit keys once, no extras",
			req:  tmuxdeliver.Request{Op: tmuxdeliver.OpSubmitOnly, SubmitKeys: []string{"Enter"}, ExtraSubmits: 2},
			want: keys("Enter"),
		},
		{
			name: "interrupt with empty message: keys then submit once, no extras",
			req:  tmuxdeliver.Request{Op: tmuxdeliver.OpInterruptSubmit, InterruptKeys: []string{"C-c"}, SubmitKeys: []string{"Enter"}, ExtraSubmits: 2},
			want: keys("C-c", "Enter"),
		},
		{
			name: "two submit keys per pass",
			req:  tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, Text: "two-key message", SubmitKeys: []string{"Escape", "Enter"}, ExtraSubmits: 1, BufferName: buf},
			want: cat(load, paste, keys("Escape", "Enter"), keys("Escape", "Enter")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{}
			if err := tmuxdeliver.Deliver(context.Background(), f, tc.req); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if got := f.argvs(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("commands:\n got %q\nwant %q", got, tc.want)
			}
			for _, c := range f.calls {
				isLoad := c.argv[1] == "load-buffer"
				if c.hasStdin != isLoad {
					t.Errorf("%q: stdin given = %v, want %v", c.argv, c.hasStdin, isLoad)
				}
				if isLoad && c.stdin != tc.req.Text {
					t.Errorf("load-buffer stdin = %q, want %q", c.stdin, tc.req.Text)
				}
				for _, a := range c.argv {
					if tc.req.Text != "" && strings.Contains(a, tc.req.Text) {
						t.Errorf("text appeared in argv %q", c.argv)
					}
				}
			}
		})
	}
}

// TestDeliver_SanitisedText: the text loaded on stdin is Sanitize(text).
func TestDeliver_SanitisedText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\x1b[201~b", "a[201~b"},
		{"line\r\n", "line\n"},
		{"x\u009by", "xy"},
		{"ok\x7f", "ok"},
	}
	for _, tc := range cases {
		f := &fakeRunner{}
		req := tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, Text: tc.in, SubmitKeys: []string{"Enter"}, BufferName: buf}
		if err := tmuxdeliver.Deliver(context.Background(), f, req); err != nil {
			t.Fatalf("Deliver(%q): %v", tc.in, err)
		}
		if got := f.calls[0].stdin; got != tc.want {
			t.Errorf("Deliver(%q) loaded %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDeliver_EmptyAfterSanitize sends nothing for text that is only control
// characters.
func TestDeliver_EmptyAfterSanitize(t *testing.T) {
	f := &fakeRunner{}
	req := tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, Text: "\x1b\x07\r", SubmitKeys: []string{"Enter"}, BufferName: buf}
	if err := tmuxdeliver.Deliver(context.Background(), f, req); !errors.Is(err, tmuxdeliver.ErrEmptyAfterSanitize) {
		t.Fatalf("Deliver = %v, want ErrEmptyAfterSanitize", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %q, want nothing", f.argvs())
	}
}

// TestDeliver_RequestShape rejects malformed requests before running
// anything.
func TestDeliver_RequestShape(t *testing.T) {
	cases := []struct {
		name string
		req  tmuxdeliver.Request
		want error
	}{
		{"unknown op", tmuxdeliver.Request{Op: "type", Text: "x", SubmitKeys: []string{"Enter"}, BufferName: buf}, tmuxdeliver.ErrInvalidOp},
		{"paste without text", tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, SubmitKeys: []string{"Enter"}, BufferName: buf}, tmuxdeliver.ErrTextRequired},
		{"submit with text", tmuxdeliver.Request{Op: tmuxdeliver.OpSubmitOnly, Text: "x", SubmitKeys: []string{"Enter"}}, tmuxdeliver.ErrTextForbidden},
		{"no submit keys", tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, Text: "x", BufferName: buf}, tmuxdeliver.ErrNoSubmitKeys},
		{"interrupt without keys", tmuxdeliver.Request{Op: tmuxdeliver.OpInterruptSubmit, SubmitKeys: []string{"Enter"}}, tmuxdeliver.ErrNoInterrupt},
		{"stray interrupt keys", tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, Text: "x", InterruptKeys: []string{"C-c"}, SubmitKeys: []string{"Enter"}, BufferName: buf}, tmuxdeliver.ErrStrayKeys},
		{"paste without buffer", tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, Text: "x", SubmitKeys: []string{"Enter"}}, tmuxdeliver.ErrNoBufferName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{}
			if err := tmuxdeliver.Deliver(context.Background(), f, tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("Deliver = %v, want %v", err, tc.want)
			}
			if len(f.calls) != 0 {
				t.Errorf("ran %q, want nothing", f.argvs())
			}
		})
	}
}

// TestDeliver_HarnessKeysOnExecPath: Deliver itself does not apply the route
// key allowlist, so the exec path keeps a harness config's own keys.
func TestDeliver_HarnessKeysOnExecPath(t *testing.T) {
	f := &fakeRunner{}
	req := tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, Text: "x", SubmitKeys: []string{"KPEnter"}, ExtraSubmits: 5, BufferName: buf}
	if err := tmuxdeliver.Deliver(context.Background(), f, req); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if err := tmuxdeliver.CheckRoute(req); err == nil {
		t.Error("CheckRoute accepted keys and counts outside the route bounds")
	}
}

// TestDeliver_Failures pins the partial-delivery boundary: before the paste
// the error is returned as is (retryable); a failed paste deletes the buffer;
// after the paste the error is a *PartialError.
func TestDeliver_Failures(t *testing.T) {
	base := tmuxdeliver.Request{Op: tmuxdeliver.OpInterruptPaste, Text: "msg", InterruptKeys: []string{"C-c"}, SubmitKeys: []string{"Enter"}, ExtraSubmits: 2, BufferName: buf}
	cases := []struct {
		name      string
		failOn    string
		failAt    int
		partial   string // "" = not partial
		wantLast  []string
		wantCalls int
	}{
		{name: "interrupt key fails", failOn: "send-keys", failAt: 1, wantLast: []string{"tmux", "send-keys", "-t", "scion:0", "C-c"}, wantCalls: 1},
		{name: "load fails", failOn: "load-buffer", wantLast: []string{"tmux", "load-buffer", "-b", buf, "-"}, wantCalls: 2},
		{name: "paste fails, buffer deleted", failOn: "paste-buffer", wantLast: []string{"tmux", "delete-buffer", "-b", buf}, wantCalls: 4},
		{name: "submit after paste fails", failOn: "send-keys", failAt: 2, partial: tmuxdeliver.StageSubmit, wantCalls: 4},
		{name: "extra submit fails", failOn: "send-keys", failAt: 3, partial: tmuxdeliver.StageExtraSubmit, wantCalls: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{failOn: tc.failOn, failAt: tc.failAt}
			err := tmuxdeliver.Deliver(context.Background(), f, base)
			if err == nil {
				t.Fatal("Deliver = nil, want an error")
			}
			var pe *tmuxdeliver.PartialError
			isPartial := errors.As(err, &pe)
			switch {
			case tc.partial == "" && isPartial:
				t.Errorf("Deliver = %v: partial, want a plain error", err)
			case tc.partial != "" && !isPartial:
				t.Errorf("Deliver = %v: plain, want partial %s", err, tc.partial)
			case tc.partial != "" && pe.Stage != tc.partial:
				t.Errorf("stage = %q, want %q", pe.Stage, tc.partial)
			}
			if !errors.Is(err, errStep) {
				t.Errorf("Deliver = %v: does not wrap the step error", err)
			}
			if len(f.calls) != tc.wantCalls {
				t.Errorf("ran %d commands %q, want %d", len(f.calls), f.argvs(), tc.wantCalls)
			}
			if tc.wantLast != nil {
				if got := f.calls[len(f.calls)-1].argv; !reflect.DeepEqual(got, tc.wantLast) {
					t.Errorf("last command = %q, want %q", got, tc.wantLast)
				}
			}
		})
	}
}

// TestDeliver_PasteFailureCleanupIgnoresCancel: the buffer is deleted even
// when the caller's ctx is already cancelled.
func TestDeliver_PasteFailureCleanupIgnoresCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeRunner{failOn: "paste-buffer"}
	f2 := tmuxdeliver.RunnerFunc(func(c context.Context, argv []string, stdin io.Reader) (string, error) {
		if argv[1] == "paste-buffer" {
			cancel()
		}
		return f.Run(c, argv, stdin)
	})
	req := tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, Text: "x", SubmitKeys: []string{"Enter"}, BufferName: buf}
	_ = tmuxdeliver.Deliver(ctx, f2, req)
	last := f.calls[len(f.calls)-1]
	if last.argv[1] != "delete-buffer" {
		t.Fatalf("last command = %q, want delete-buffer", last.argv)
	}
	if last.ctxErr != nil {
		t.Errorf("delete-buffer ran on a cancelled context: %v", last.ctxErr)
	}
}

// TestDeliver_CancelDuringExtraSubmits is a partial delivery.
func TestDeliver_CancelDuringExtraSubmits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeRunner{}
	r := tmuxdeliver.RunnerFunc(func(c context.Context, argv []string, stdin io.Reader) (string, error) {
		out, err := f.Run(c, argv, stdin)
		if argv[1] == "paste-buffer" {
			cancel()
		}
		return out, err
	})
	req := tmuxdeliver.Request{Op: tmuxdeliver.OpPaste, Text: "x", SubmitKeys: []string{"Enter"}, ExtraSubmits: 2, ExtraSubmitDelay: time.Hour, BufferName: buf}
	err := tmuxdeliver.Deliver(ctx, r, req)
	var pe *tmuxdeliver.PartialError
	if !errors.As(err, &pe) || pe.Stage != tmuxdeliver.StageExtraSubmit || !errors.Is(err, context.Canceled) {
		t.Fatalf("Deliver = %v, want a partial extra_submit error wrapping context.Canceled", err)
	}
}

// TestBufferName validates the id and keeps the broker's existing shape.
func TestBufferName(t *testing.T) {
	got, err := tmuxdeliver.BufferName("0123abcd", 7)
	if err != nil || got != buf {
		t.Fatalf("BufferName = %q, %v; want %q", got, err, buf)
	}
	for _, id := range []string{"", "a b", "a;b", "../x", strings.Repeat("a", 65), "a\nb"} {
		if _, err := tmuxdeliver.BufferName(id, 1); err == nil {
			t.Errorf("BufferName(%q) accepted", id)
		}
	}
}
