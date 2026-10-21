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

package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/util/cloudrunenv"
)

// sanitizedErrorText is the one way a log sink renders an error-valued
// attribute: the error's text through SanitizeText. fmt.Sprint guards an
// Error method that panics, such as one on a nil pointer receiver.
func sanitizedErrorText(err error) string {
	return SanitizeText(fmt.Sprint(err))
}

// cloudAttrValue returns the Cloud Logging payload value for v, a resolved
// value that is not a group: an error as its sanitized text, any other
// value that does not encode as JSON as its sanitized string form, and
// everything else unchanged.
func cloudAttrValue(v slog.Value) any {
	if v.Kind() != slog.KindAny {
		return v.Any()
	}
	a := v.Any()
	if a == nil {
		return nil
	}
	if err, ok := a.(error); ok {
		return sanitizedErrorText(err)
	}
	switch a.(type) {
	case string, bool, []string, map[string]string:
		return a // always encodes; skip the trial encoding below
	}
	if _, err := json.Marshal(a); err != nil {
		return SanitizeText(fmt.Sprint(a))
	}
	return a
}

// sanitizeErrorAttr is a slog.HandlerOptions.ReplaceAttr that renders an
// error-valued attribute as its sanitized text.
func sanitizeErrorAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindAny {
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, sanitizedErrorText(err))
		}
	}
	return a
}

// stdoutReachesCloudLogging reports whether this process's stdout is
// likely collected into Cloud Logging: the GCP log format was chosen, or
// the process runs on Cloud Run or in a Kubernetes pod, whose platforms
// collect container output. Every stdout log handler (newStdoutHandler)
// sanitizes error attributes in that case.
func stdoutReachesCloudLogging(useGCP bool) bool {
	return useGCP || cloudrunenv.IsService() || os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}

// logStdout, when set, is where the stdout log handlers write instead of
// os.Stdout. Tests set it.
var logStdout io.Writer

// stdoutWriter returns logStdout if set, else os.Stdout as it is now, so a
// handler built after a caller redirects os.Stdout writes to the redirect.
func stdoutWriter() io.Writer {
	if logStdout != nil {
		return logStdout
	}
	return os.Stdout
}

// newStdoutHandler builds a stdout log handler: the GCP format when useGCP,
// else JSON (with a component attribute when withComponent). Where stdout
// is likely collected into Cloud Logging (stdoutReachesCloudLogging), error
// attributes are rendered as sanitized strings. opts is copied, never
// modified: NewGCPHandler rewrites ReplaceAttr in place, and a caller may
// share opts with another handler.
func newStdoutHandler(w io.Writer, opts *slog.HandlerOptions, component, hubName string, useGCP, withComponent bool) slog.Handler {
	var o slog.HandlerOptions
	if opts != nil {
		o = *opts
	}
	if stdoutReachesCloudLogging(useGCP) {
		prev := o.ReplaceAttr
		o.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if prev != nil {
				a = prev(groups, a)
			}
			return sanitizeErrorAttr(groups, a)
		}
	}
	if useGCP {
		return NewGCPHandler(w, &o, component, hubName)
	}
	h := slog.NewJSONHandler(w, &o)
	if withComponent {
		return h.WithAttrs([]slog.Attr{slog.String(AttrComponent, component)})
	}
	return h
}

// WithSanitizedErrorAttrs wraps h, a handler with no ReplaceAttr hook of
// its own (the OTel log bridge, the agent-container log), so that every
// error-valued attribute it receives, including pre-bound and grouped
// ones, is already a sanitized string.
func WithSanitizedErrorAttrs(h slog.Handler) slog.Handler {
	return &sanitizingHandler{inner: h}
}

// sanitizingHandler is the handler WithSanitizedErrorAttrs returns.
type sanitizingHandler struct {
	inner slog.Handler
}

func (h *sanitizingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *sanitizingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(sanitizeAttrTree(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h *sanitizingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = sanitizeAttrTree(a)
	}
	return &sanitizingHandler{inner: h.inner.WithAttrs(out)}
}

func (h *sanitizingHandler) WithGroup(name string) slog.Handler {
	return &sanitizingHandler{inner: h.inner.WithGroup(name)}
}

// sanitizeAttrTree resolves a and renders every error value in it,
// recursing into groups, as its sanitized text.
func sanitizeAttrTree(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		members := a.Value.Group()
		out := make([]slog.Attr, len(members))
		for i, m := range members {
			out[i] = sanitizeAttrTree(m)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	return sanitizeErrorAttr(nil, a)
}
