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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	gcplog "cloud.google.com/go/logging"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// credentialShapedValues are the values credentialShapedError embeds; no
// sink may write any of them.
var credentialShapedValues = []string{
	"sk-live-0123456789abcdefABCDEF",
	"hunter2-value",
	"pw-value-123",
	"eyJhbGciOiJIUzI1NiJ9",
}

// credentialShapedError returns an error whose text holds credential-shaped
// values and runs past MaxSanitizedTextBytes.
func credentialShapedError() error {
	return fmt.Errorf("request failed: %w", errors.New(
		"env FOO_API_KEY=sk-live-0123456789abcdefABCDEF "+
			`spec {"DB_PASSWORD": "hunter2-value"} `+
			"pull https://deploy:pw-value-123@registry.example.com/img "+
			"got eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl "+
			strings.Repeat("output line\n", 150)))
}

// valuerError is a slog.LogValuer that resolves to an error.
type valuerError struct{ err error }

func (v valuerError) LogValue() slog.Value { return slog.AnyValue(v.err) }

// nilPtrError has an Error method that panics on a nil receiver.
type nilPtrError struct{ msg string }

func (e *nilPtrError) Error() string { return e.msg }

// assertSanitizedText checks got is a sanitized, truncated rendering of
// credentialShapedError.
func assertSanitizedText(t *testing.T, field string, got any) {
	t.Helper()
	s, ok := got.(string)
	if !ok {
		t.Fatalf("%s = %#v (%T), want a string", field, got, got)
	}
	for _, want := range []string{"FOO_API_KEY=[REDACTED]", `"DB_PASSWORD": [REDACTED]`, "https://[REDACTED]@registry.example.com", "...[truncated, "} {
		if !strings.Contains(s, want) {
			t.Errorf("%s lacks %q: %q", field, want, s)
		}
	}
	if len(s) > MaxSanitizedTextBytes+64 {
		t.Errorf("%s is %d bytes, want it bounded near %d", field, len(s), MaxSanitizedTextBytes)
	}
}

// assertNoCredentialValues checks out holds none of credentialShapedValues.
func assertNoCredentialValues(t *testing.T, sink, out string) {
	t.Helper()
	for _, v := range credentialShapedValues {
		if strings.Contains(out, v) {
			t.Errorf("%s output contains %q", sink, v)
		}
	}
}

// logEveryShape writes one record carrying an error as a pre-bound attr,
// a record attr, a grouped attr and a LogValuer.
func logEveryShape(h slog.Handler) {
	slog.New(h).With("bound", credentialShapedError()).Error("op failed",
		"error", credentialShapedError(),
		slog.Group("detail", slog.Any("inner", credentialShapedError())),
		"valuer", valuerError{credentialShapedError()})
}

// TestCloudLoggingSinks_RenderErrorsSanitized is the Cloud Logging
// payload guard: the main, message and artifact handlers all write every
// error-valued attribute as a sanitized, truncated string.
func TestCloudLoggingSinks_RenderErrorsSanitized(t *testing.T) {
	for name, mk := range map[string]func(hook func(gcplog.Entry)) slog.Handler{
		"cloud": func(hook func(gcplog.Entry)) slog.Handler {
			return &CloudHandler{level: slog.LevelDebug, component: "c", logHook: hook}
		},
		"message": func(hook func(gcplog.Entry)) slog.Handler {
			return &messageCloudHandler{CloudHandler: CloudHandler{level: slog.LevelDebug, component: "c", logHook: hook}}
		},
		"artifact": func(hook func(gcplog.Entry)) slog.Handler {
			return &artifactCloudHandler{CloudHandler: CloudHandler{level: slog.LevelDebug, component: "c", logHook: hook}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var entries []gcplog.Entry
			logEveryShape(mk(func(e gcplog.Entry) { entries = append(entries, e) }))
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1", len(entries))
			}
			payload, ok := entries[0].Payload.(map[string]any)
			if !ok {
				t.Fatalf("payload = %T, want a map", entries[0].Payload)
			}
			assertSanitizedText(t, "bound", payload["bound"])
			assertSanitizedText(t, "error", payload["error"])
			assertSanitizedText(t, "valuer", payload["valuer"])
			detail, ok := payload["detail"].(map[string]any)
			if !ok {
				t.Fatalf("detail = %#v, want a group map", payload["detail"])
			}
			assertSanitizedText(t, "detail.inner", detail["inner"])
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("payload does not encode: %v", err)
			}
			assertNoCredentialValues(t, name, string(raw))
		})
	}
}

// TestCloudAttrValue covers the other value shapes: plain values stay
// typed, a value that does not encode as JSON becomes a string, and an
// error whose Error method panics on a nil receiver does not panic.
func TestCloudAttrValue(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	m := map[string]any{"k": "v"}
	if got := cloudAttrValue(slog.IntValue(7)); got != int64(7) {
		t.Errorf("int = %#v", got)
	}
	if got := cloudAttrValue(slog.TimeValue(now)); got != now {
		t.Errorf("time = %#v", got)
	}
	if got, ok := cloudAttrValue(slog.AnyValue(m)).(map[string]any); !ok || got["k"] != "v" {
		t.Errorf("map = %#v", got)
	}
	if got := cloudAttrValue(slog.AnyValue(func() {})); fmt.Sprintf("%T", got) != "string" {
		t.Errorf("func = %#v (%T), want its string form", got, got)
	}
	if got := cloudAttrValue(slog.AnyValue(make(chan int))); fmt.Sprintf("%T", got) != "string" {
		t.Errorf("chan = %#v (%T), want its string form", got, got)
	}
	var nilErr *nilPtrError
	if got := cloudAttrValue(slog.AnyValue(error(nilErr))); fmt.Sprintf("%T", got) != "string" {
		t.Errorf("nil-receiver error = %#v (%T), want a string", got, got)
	}
}

// TestStdoutSink_RenderErrorsSanitized is the stdout guard: where stdout
// reaches Cloud Logging (the GCP format, Cloud Run, Kubernetes) every
// error-valued attribute is a sanitized, truncated string; a local plain
// JSON log keeps the error text as is.
func TestStdoutSink_RenderErrorsSanitized(t *testing.T) {
	for _, tc := range []struct {
		name     string
		useGCP   bool
		env      map[string]string
		sanitize bool
	}{
		{"gcp format", true, nil, true},
		{"json on cloud run", false, map[string]string{"K_SERVICE": "svc"}, true},
		{"json on kubernetes", false, map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"}, true},
		{"local json", false, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("K_SERVICE", "")
			t.Setenv("KUBERNETES_SERVICE_HOST", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var buf bytes.Buffer
			logEveryShape(createBaseHandlerTo(&buf, "c", tc.useGCP, ""))
			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("decode %q: %v", buf.String(), err)
			}
			if !tc.sanitize {
				if s, _ := rec["error"].(string); !strings.Contains(s, "sk-live-0123456789abcdefABCDEF") {
					t.Errorf("local error = %q, want the error text as is", s)
				}
				return
			}
			assertSanitizedText(t, "bound", rec["bound"])
			assertSanitizedText(t, "error", rec["error"])
			assertSanitizedText(t, "valuer", rec["valuer"])
			detail, ok := rec["detail"].(map[string]any)
			if !ok {
				t.Fatalf("detail = %#v, want a group object", rec["detail"])
			}
			assertSanitizedText(t, "detail.inner", detail["inner"])
			assertNoCredentialValues(t, tc.name, buf.String())
		})
	}
}

// attrProcessor is an in-memory sdklog.Processor that records the text
// form of every exported attribute.
type attrProcessor struct {
	mu    sync.Mutex
	attrs map[string]string
}

func (p *attrProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func (p *attrProcessor) OnEmit(_ context.Context, r *sdklog.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attrs == nil {
		p.attrs = map[string]string{}
	}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		p.attrs[string(kv.Key)] = kv.Value.Emit()
		return true
	})
	return nil
}

func (p *attrProcessor) Shutdown(context.Context) error   { return nil }
func (p *attrProcessor) ForceFlush(context.Context) error { return nil }

// TestOTelSink_RenderErrorsSanitized is the OTel log bridge guard: every
// error-valued attribute it exports is a sanitized, truncated string.
func TestOTelSink_RenderErrorsSanitized(t *testing.T) {
	proc := &attrProcessor{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(proc))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })

	logEveryShape(NewOTelHandler("c", lp))

	proc.mu.Lock()
	defer proc.mu.Unlock()
	for _, key := range []string{"bound", "error", "valuer"} {
		assertSanitizedText(t, key, proc.attrs[key])
	}
	// The group is exported as a map; its text holds the sanitized value.
	for _, want := range []string{"FOO_API_KEY=[REDACTED]", "...[truncated, "} {
		if !strings.Contains(proc.attrs["detail"], want) {
			t.Errorf("detail lacks %q: %q", want, proc.attrs["detail"])
		}
	}
	var all strings.Builder
	for k, v := range proc.attrs {
		all.WriteString(k + "=" + v + "\n")
	}
	assertNoCredentialValues(t, "otel", all.String())
}

// TestDedicatedLoggerStdout_RenderErrorsSanitized is the stdout guard for
// the message, artifact and request loggers, which build their own stdout
// handlers: in a Kubernetes pod (stdout collected) every error attribute is
// a sanitized, truncated string, in both output formats.
func TestDedicatedLoggerStdout_RenderErrorsSanitized(t *testing.T) {
	for name, mk := range map[string]func(useGCP bool) (*slog.Logger, func(), error){
		"message": func(useGCP bool) (*slog.Logger, func(), error) {
			return NewMessageLogger(MessageLoggerConfig{Component: "c", UseGCP: useGCP})
		},
		"artifact": func(useGCP bool) (*slog.Logger, func(), error) {
			return NewArtifactLogger(ArtifactLoggerConfig{Component: "c", UseGCP: useGCP})
		},
		"request": func(useGCP bool) (*slog.Logger, func(), error) {
			return NewRequestLogger(RequestLoggerConfig{Component: "c", UseGCP: useGCP})
		},
	} {
		for _, useGCP := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/gcp=%v", name, useGCP), func(t *testing.T) {
				t.Setenv("K_SERVICE", "")
				t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
				var buf bytes.Buffer
				orig := logStdout
				logStdout = &buf
				t.Cleanup(func() { logStdout = orig })

				logger, cleanup, err := mk(useGCP)
				if err != nil {
					t.Fatal(err)
				}
				if cleanup != nil {
					t.Cleanup(cleanup)
				}
				logEveryShape(logger.Handler())

				var rec map[string]any
				if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
					t.Fatalf("decode %q: %v", buf.String(), err)
				}
				assertSanitizedText(t, "bound", rec["bound"])
				assertSanitizedText(t, "error", rec["error"])
				assertSanitizedText(t, "valuer", rec["valuer"])
				assertNoCredentialValues(t, name, buf.String())
			})
		}
	}
}

// TestNewStdoutHandler_CopiesOptions pins that the stdout handler leaves
// the caller's options untouched, since a caller may share them with
// another handler.
func TestNewStdoutHandler_CopiesOptions(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	for _, useGCP := range []bool{false, true} {
		_ = newStdoutHandler(&bytes.Buffer{}, opts, "c", "", useGCP, true)
		if opts.ReplaceAttr != nil {
			t.Fatalf("useGCP=%v: caller's ReplaceAttr was set", useGCP)
		}
	}
}
