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

package rebuildpreflight

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const (
	// MaxStdoutBytes is the maximum size of a valid preflight JSON report (4 MiB).
	MaxStdoutBytes = 4 * 1024 * 1024
	// CaptureHeadBytes is the head window kept by Capture (8 KiB).
	CaptureHeadBytes = 8 * 1024
	// CaptureTailBytes is the tail ring size kept by Capture (1 MiB).
	CaptureTailBytes = 1024 * 1024
	// MaxLogStderrBytes is the maximum length of redacted stderr written to the run log (64 KiB).
	MaxLogStderrBytes = 64 * 1024
)

// CappedWriter keeps up to limit+1 bytes and discards the rest while
// continuing to drain the pipe so a verbose child process cannot block on
// stdout and end as timed_out (CN4).
type CappedWriter struct {
	limit int
	buf   bytes.Buffer
}

// NewCappedWriter returns a CappedWriter that retains at most limit+1 bytes.
// If limit <= 0, MaxStdoutBytes is used.
func NewCappedWriter(limit int) *CappedWriter {
	if limit <= 0 {
		limit = MaxStdoutBytes
	}
	return &CappedWriter{limit: limit}
}

// Write retains bytes up to limit+1 and discards any remainder without error.
func (w *CappedWriter) Write(p []byte) (int, error) {
	capLimit := w.limit + 1
	if w.buf.Len() < capLimit {
		rem := capLimit - w.buf.Len()
		if len(p) <= rem {
			w.buf.Write(p)
		} else {
			w.buf.Write(p[:rem])
		}
	}
	return len(p), nil
}

// Bytes returns the retained bytes.
func (w *CappedWriter) Bytes() []byte {
	return w.buf.Bytes()
}

// Oversize reports whether more than limit bytes were written.
func (w *CappedWriter) Oversize() bool {
	return w.buf.Len() > w.limit
}

// Capture records a child process's stderr with a head window (default 8 KiB)
// and a tail ring buffer (default 1 MiB). Redacted applies the R3 ordering:
// drop the partial line at each cut, redact the DSN and credentials across
// the whole captured text, and only then truncate to the log budget at a
// line boundary.
type Capture struct {
	headCap int
	tailCap int
	logCap  int

	head       []byte
	tail       []byte
	tailPos    int
	tailFull   bool
	overflowed bool
}

// NewCapture returns a Capture with the R3 default sizes (8 KiB head,
// 1 MiB tail ring, 64 KiB redacted log budget).
func NewCapture() *Capture {
	return NewCaptureWithLimits(CaptureHeadBytes, CaptureTailBytes, MaxLogStderrBytes)
}

// NewCaptureWithLimits returns a Capture with custom window sizes (for tests).
func NewCaptureWithLimits(headCap, tailCap, logCap int) *Capture {
	if headCap <= 0 {
		headCap = CaptureHeadBytes
	}
	if tailCap <= 0 {
		tailCap = CaptureTailBytes
	}
	if logCap <= 0 {
		logCap = MaxLogStderrBytes
	}
	return &Capture{
		headCap: headCap,
		tailCap: tailCap,
		logCap:  logCap,
	}
}

// Write appends p into the head window and, once head is full, into the tail
// ring buffer.
func (c *Capture) Write(p []byte) (int, error) {
	origLen := len(p)
	if !c.overflowed {
		rem := c.headCap - len(c.head)
		if len(p) <= rem {
			c.head = append(c.head, p...)
			return origLen, nil
		}
		c.head = append(c.head, p[:rem]...)
		p = p[rem:]
		c.overflowed = true
	}
	if len(p) == 0 {
		return origLen, nil
	}
	if c.tail == nil {
		c.tail = make([]byte, 0, min(c.tailCap, 4096))
	}
	if len(p) >= c.tailCap {
		// Entire ring is overwritten by the last tailCap bytes of p.
		c.tail = append(c.tail[:0], p[len(p)-c.tailCap:]...)
		c.tailPos = 0
		c.tailFull = true
		return origLen, nil
	}
	if !c.tailFull {
		rem := c.tailCap - len(c.tail)
		if len(p) <= rem {
			c.tail = append(c.tail, p...)
			return origLen, nil
		}
		c.tail = append(c.tail, p[:rem]...)
		p = p[rem:]
		c.tailFull = true
		c.tailPos = 0
	}
	// Ring is full; overwrite starting at tailPos.
	for len(p) > 0 {
		n := copy(c.tail[c.tailPos:], p)
		c.tailPos = (c.tailPos + n) % c.tailCap
		p = p[n:]
	}
	return origLen, nil
}

// linearTail returns the tail ring bytes in chronological order.
func (c *Capture) linearTail() []byte {
	if len(c.tail) == 0 {
		return nil
	}
	if !c.tailFull || c.tailPos == 0 {
		return append([]byte(nil), c.tail...)
	}
	out := make([]byte, len(c.tail))
	n := copy(out, c.tail[c.tailPos:])
	copy(out[n:], c.tail[:c.tailPos])
	return out
}

// Redacted returns the captured stderr with R3 ordering:
//  1. Drop the partial line at each cut (head's last line when head was cut,
//     and tail's first line when cut) so no secret fragment survives a cut.
//  2. Run config.RedactDSNIn over the whole captured text.
//  3. Truncate to logCap (64 KiB) at a line boundary with "[N bytes omitted]".
func (c *Capture) Redacted(driver, dsn string) string {
	var text string
	if !c.overflowed {
		text = string(c.head)
	} else {
		headPart := dropTrailingPartialLine(c.head)
		tailRaw := c.linearTail()
		var tailPart string
		if c.tailFull || (len(c.head) > 0 && c.head[len(c.head)-1] != '\n') {
			tailPart = dropLeadingPartialLine(tailRaw)
		} else {
			tailPart = string(tailRaw)
		}
		switch {
		case headPart != "" && tailPart != "":
			text = headPart + tailPart
		case headPart != "":
			text = headPart
		default:
			text = tailPart
		}
	}

	redacted := config.RedactDSNIn(text, driver, dsn)
	return truncateAtLineBoundary(redacted, c.logCap)
}

func dropTrailingPartialLine(b []byte) string {
	idx := bytes.LastIndexByte(b, '\n')
	if idx < 0 {
		return ""
	}
	return string(b[:idx+1])
}

func dropLeadingPartialLine(b []byte) string {
	idx := bytes.IndexByte(b, '\n')
	if idx < 0 {
		return ""
	}
	return string(b[idx+1:])
}

func truncateAtLineBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Keep a head slice and a tail slice at line boundaries, separated by
	// "[N bytes omitted]".
	half := maxBytes / 2
	headEnd := strings.LastIndexByte(s[:half], '\n')
	var head string
	if headEnd >= 0 {
		head = s[:headEnd+1]
	}
	tailBudget := maxBytes - len(head)
	tailStart := len(s) - tailBudget
	if tailStart < len(head) {
		tailStart = len(head)
	}
	var tail string
	if nl := strings.IndexByte(s[tailStart:], '\n'); nl >= 0 {
		tail = s[tailStart+nl+1:]
	}
	omitted := len(s) - len(head) - len(tail)
	if omitted <= 0 {
		return s
	}
	marker := fmt.Sprintf("[%d bytes omitted]\n", omitted)
	return head + marker + tail
}
