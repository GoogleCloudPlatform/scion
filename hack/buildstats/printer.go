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

package main

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// printer writes formatted output and keeps the first write error, so the
// table renderers stay readable and the error is still reported once by
// whoever owns the output (see printer.err).
type printer struct {
	w   io.Writer
	err error
}

func newPrinter(w io.Writer) *printer { return &printer{w: w} }

func (p *printer) printf(format string, a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, a...)
	}
}

func (p *printer) println(a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintln(p.w, a...)
	}
}

// table returns a printer that writes through a tabwriter (padding 2,
// space-filled) and a done func that flushes it and folds any error back
// into p.
func (p *printer) table(flags uint) (*printer, func()) {
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', flags)
	t := &printer{w: tw}
	return t, func() {
		if t.err == nil {
			t.err = tw.Flush()
		}
		if p.err == nil {
			p.err = t.err
		}
	}
}
