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
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gcplog "cloud.google.com/go/logging"
	logpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// keyShapedValues are the values keyShapedError embeds; no
// sink may write any of them.
var keyShapedValues = []string{
	"sk-live-0123456789abcdefABCDEF",
	"hunter2-value",
	"pw-value-123",
	"eyJhbGciOiJIUzI1NiJ9",
}

// keyShapedError returns an error whose text holds key-shaped
// values and runs past MaxSanitizedTextBytes.
func keyShapedError() error {
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
// keyShapedError.
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

// assertNoKeyShapedValues checks out holds none of keyShapedValues.
func assertNoKeyShapedValues(t *testing.T, sink, out string) {
	t.Helper()
	for _, v := range keyShapedValues {
		if strings.Contains(out, v) {
			t.Errorf("%s output contains %q", sink, v)
		}
	}
}

// logEveryShape writes one record carrying an error as a pre-bound attr,
// a record attr, a grouped attr and a LogValuer.
func logEveryShape(h slog.Handler) {
	slog.New(h).With("bound", keyShapedError()).Error("op failed",
		"error", keyShapedError(),
		slog.Group("detail", slog.Any("inner", keyShapedError())),
		"valuer", valuerError{keyShapedError()})
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
			assertNoKeyShapedValues(t, name, string(raw))
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
			assertNoKeyShapedValues(t, tc.name, buf.String())
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
		p.attrs[string(kv.Key)] = kv.Value.String()
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
	assertNoKeyShapedValues(t, "otel", all.String())
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
				assertNoKeyShapedValues(t, name, buf.String())
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

// TestCreateBaseHandler_WritesToCurrentStdout pins that the stdout handler
// writes to os.Stdout as it is when the handler is built, so a caller that
// redirects os.Stdout first (as server start-up tests do) gets the output.
func TestCreateBaseHandler_WritesToCurrentStdout(t *testing.T) {
	t.Setenv("K_SERVICE", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	for _, useGCP := range []bool{false, true} {
		f, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
		if err != nil {
			t.Fatal(err)
		}
		prev := os.Stdout
		os.Stdout = f
		slog.New(createBaseHandler("c", useGCP, "")).Info("redirected line")
		os.Stdout = prev
		_ = f.Close()
		data, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "redirected line") {
			t.Errorf("useGCP=%v: redirected stdout = %q, want the log line", useGCP, data)
		}
	}
}

type captureLoggingServer struct {
	logpb.UnimplementedLoggingServiceV2Server
	mu      sync.Mutex
	entries []*logpb.LogEntry
}

func (s *captureLoggingServer) WriteLogEntries(_ context.Context, req *logpb.WriteLogEntriesRequest) (*logpb.WriteLogEntriesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ent := range req.GetEntries() {
		if strings.HasSuffix(ent.GetLogName(), "/logs/diagnostic-log") {
			continue
		}
		s.entries = append(s.entries, ent)
	}
	return &logpb.WriteLogEntriesResponse{}, nil
}

func (s *captureLoggingServer) takeEntries() []*logpb.LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]*logpb.LogEntry(nil), s.entries...)
	s.entries = nil
	return out
}

func newTestCloudLogger(t *testing.T) (*gcplog.Client, *gcplog.Logger, *captureLoggingServer, *CloudWriteStats, *bytes.Buffer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fake := &captureLoggingServer{}
	logpb.RegisterLoggingServiceV2Server(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := gcplog.NewClient(ctx, "test-project", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	stats := newCloudWriteStats(nil)
	var errBuf bytes.Buffer
	client.OnError = cloudClientOnError(stats, &errBuf)
	t.Cleanup(func() { _ = client.Close() })

	logger := client.Logger("test-log", gcplog.CommonResource(&mrpb.MonitoredResource{
		Type:   "global",
		Labels: map[string]string{"project_id": "test-project"},
	}))
	return client, logger, fake, stats, &errBuf
}

type badMarshalerWithSecret struct {
	secret string
}

func (b badMarshalerWithSecret) MarshalJSON() ([]byte, error) {
	return nil, errors.New("cannot encode value")
}

// TestCloudHandler_UnencodableAttrsReplacedEntryWritten verifies that the
// Cloud Logging slog handlers replace only the unencodable value (such as a
// NaN nested in a map attribute or an out-of-range time.Time) and still
// deliver the rest of the entry through the real Cloud Logging SDK without
// client errors or leaking hidden struct fields (ptone/scion#4779).
func TestCloudHandler_UnencodableAttrsReplacedEntryWritten(t *testing.T) {
	client, gcpLogger, fake, stats, errBuf := newTestCloudLogger(t)
	h := &CloudHandler{
		logger:    gcpLogger,
		client:    client,
		level:     slog.LevelDebug,
		component: "scion-hub",
		projectID: "test-project",
		version:   "test",
		stats:     stats,
	}
	log := slog.New(h)

	t.Run("NaN and Inf nested in map and direct", func(t *testing.T) {
		errBuf.Reset()
		callerMap := map[string]any{
			"ok":      1.5,
			"nan":     math.NaN(),
			"pos_inf": math.Inf(1),
			"neg_inf": math.Inf(-1),
			"inner": map[string]float64{
				"valid": 42,
				"bad":   math.NaN(),
			},
		}
		log.Info("stats recorded",
			"keep", "preserved",
			"metrics", callerMap,
			slog.Float64("direct_nan", math.NaN()),
			slog.Group("grp",
				slog.String("grp_ok", "yes"),
				slog.Float64("grp_nan", math.NaN()),
			),
		)
		if err := gcpLogger.Flush(); err != nil {
			t.Fatalf("Flush failed: %v (stderr: %s)", err, errBuf.String())
		}
		if stats.Failures(CloudReasonError) != 0 || errBuf.Len() != 0 {
			t.Fatalf("unexpected client error: count=%d stderr=%q", stats.Failures(CloudReasonError), errBuf.String())
		}
		entries := fake.takeEntries()
		if len(entries) != 1 {
			t.Fatalf("entries delivered = %d, want 1", len(entries))
		}
		got := entries[0].GetJsonPayload().AsMap()
		if got["message"] != "stats recorded" || got["keep"] != "preserved" {
			t.Fatalf("top-level fields lost: %#v", got)
		}
		if got["direct_nan"] != "NaN" {
			t.Errorf("direct_nan = %#v, want %q", got["direct_nan"], "NaN")
		}
		grp, ok := got["grp"].(map[string]any)
		if !ok || grp["grp_ok"] != "yes" || grp["grp_nan"] != "NaN" {
			t.Errorf("grp = %#v, want map with grp_ok=yes and grp_nan=NaN", got["grp"])
		}
		metrics, ok := got["metrics"].(map[string]any)
		if !ok {
			t.Fatalf("metrics = %#v (%T), want preserved map object", got["metrics"], got["metrics"])
		}
		if metrics["ok"] != 1.5 || metrics["nan"] != "NaN" || metrics["pos_inf"] != "+Inf" || metrics["neg_inf"] != "-Inf" {
			t.Errorf("metrics = %#v", metrics)
		}
		inner, ok := metrics["inner"].(map[string]any)
		if !ok || inner["valid"] != float64(42) || inner["bad"] != "NaN" {
			t.Errorf("metrics.inner = %#v", metrics["inner"])
		}
		if v, ok := callerMap["nan"].(float64); !ok || !math.IsNaN(v) {
			t.Errorf("caller map was mutated in place: %#v", callerMap)
		}
	})

	t.Run("out-of-range time direct and nested", func(t *testing.T) {
		errBuf.Reset()
		validTime := time.Date(2026, 10, 12, 17, 0, 0, 0, time.UTC)
		year10000 := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		negYear := time.Date(-1, 6, 15, 12, 0, 0, 0, time.UTC)
		yearZero := time.Date(0, 6, 15, 12, 0, 0, 0, time.UTC)
		badZone := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("bad", 25*3600))

		log.Info("schedule checked",
			"keep", "preserved",
			"valid_time", validTime,
			slog.Time("year_10000", year10000),
			"negative_year", negYear,
			"year_zero", yearZero,
			"bad_zone", badZone,
			"nested", map[string]any{
				"ok":         "still-here",
				"far_future": time.Unix(1<<50, 0).UTC(),
				"before_min": yearZero,
			},
			"year_zero_only_map", map[string]any{
				"ok":         "still-here",
				"before_min": yearZero,
			},
		)
		if err := gcpLogger.Flush(); err != nil {
			t.Fatalf("Flush failed: %v (stderr: %s)", err, errBuf.String())
		}
		if stats.Failures(CloudReasonError) != 0 || errBuf.Len() != 0 {
			t.Fatalf("unexpected client error: count=%d stderr=%q", stats.Failures(CloudReasonError), errBuf.String())
		}
		entries := fake.takeEntries()
		if len(entries) != 1 {
			t.Fatalf("entries delivered = %d, want 1", len(entries))
		}
		got := entries[0].GetJsonPayload().AsMap()
		if got["message"] != "schedule checked" || got["keep"] != "preserved" {
			t.Fatalf("top-level fields lost: %#v", got)
		}
		if got["valid_time"] != "2026-10-12T17:00:00Z" {
			t.Errorf("valid_time = %#v, want RFC3339 string", got["valid_time"])
		}
		for _, k := range []string{"year_10000", "negative_year", "year_zero", "bad_zone"} {
			if s, ok := got[k].(string); !ok || s == "" {
				t.Errorf("%s = %#v (%T), want non-empty string replacement", k, got[k], got[k])
			}
		}
		nested, ok := got["nested"].(map[string]any)
		if !ok || nested["ok"] != "still-here" {
			t.Fatalf("nested = %#v (%T), want preserved map object", got["nested"], got["nested"])
		}
		for _, k := range []string{"far_future", "before_min"} {
			if s, ok := nested[k].(string); !ok || s == "" {
				t.Errorf("nested.%s = %#v (%T), want non-empty string replacement", k, nested[k], nested[k])
			}
		}
		yzMap, ok := got["year_zero_only_map"].(map[string]any)
		if !ok || yzMap["ok"] != "still-here" {
			t.Fatalf("year_zero_only_map = %#v (%T), want preserved map object", got["year_zero_only_map"], got["year_zero_only_map"])
		}
		if s, ok := yzMap["before_min"].(string); !ok || s == "" || strings.HasSuffix(s, "Z") {
			t.Errorf("year_zero_only_map.before_min = %#v, want replaced non-RFC3339 string", yzMap["before_min"])
		}
	})

	t.Run("struct slices cycles and hidden fields", func(t *testing.T) {
		errBuf.Reset()
		type hiddenEmbed struct {
			Promoted float64 `json:"promoted"`
			secret   string
		}
		type sampleStruct struct {
			hiddenEmbed
			Count      int     `json:"count"`
			Ratio      float64 `json:"ratio"`
			OmitZero   string  `json:"omit_zero,omitempty"`
			Hidden     string  `json:"-"`
			unexported string
		}
		type linkedNode struct {
			Name string      `json:"name"`
			Next *linkedNode `json:"next,omitempty"`
		}
		acyclicNode := &linkedNode{Name: "head", Next: &linkedNode{Name: "tail"}}
		cyclicNode := &linkedNode{Name: "loop"}
		cyclicNode.Next = cyclicNode
		cyclic := map[string]any{"ok": "yes"}
		cyclic["self"] = cyclic

		log.Info("composite check",
			"struct_val", sampleStruct{
				hiddenEmbed: hiddenEmbed{
					Promoted: 3.5,
					secret:   "do-not-log-embed-secret",
				},
				Count:      7,
				Ratio:      math.NaN(),
				Hidden:     "do-not-log-json-dash",
				unexported: "do-not-log-unexported",
			},
			"bad_marshaler", badMarshalerWithSecret{secret: "do-not-log-marshaler-secret"},
			"slice_val", []float64{1.25, math.NaN(), 2.5},
			"acyclic_node", acyclicNode,
			"cyclic_node", cyclicNode,
			"cyclic", cyclic,
		)
		if err := gcpLogger.Flush(); err != nil {
			t.Fatalf("Flush failed: %v (stderr: %s)", err, errBuf.String())
		}
		entries := fake.takeEntries()
		if len(entries) != 1 {
			t.Fatalf("entries delivered = %d, want 1", len(entries))
		}
		got := entries[0].GetJsonPayload().AsMap()
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			"do-not-log-json-dash",
			"do-not-log-unexported",
			"do-not-log-marshaler-secret",
			"do-not-log-embed-secret",
		} {
			if strings.Contains(string(raw), forbidden) {
				t.Errorf("payload leaked hidden field %q: %s", forbidden, raw)
			}
		}
		sv, ok := got["struct_val"].(map[string]any)
		if !ok || sv["count"] != float64(7) || sv["ratio"] != "NaN" || sv["promoted"] != 3.5 {
			t.Errorf("struct_val = %#v", got["struct_val"])
		}
		if _, present := sv["omit_zero"]; present {
			t.Errorf("struct_val kept empty omitempty field: %#v", sv)
		}
		sl, ok := got["slice_val"].([]any)
		if !ok || len(sl) != 3 || sl[0] != 1.25 || sl[1] != "NaN" || sl[2] != 2.5 {
			t.Errorf("slice_val = %#v", got["slice_val"])
		}
		an, ok := got["acyclic_node"].(map[string]any)
		if !ok || an["name"] != "head" {
			t.Errorf("acyclic_node = %#v", got["acyclic_node"])
		}
		cn, ok := got["cyclic_node"].(map[string]any)
		if !ok || cn["name"] != "loop" || cn["next"] != "<cycle>" {
			t.Errorf("cyclic_node = %#v", got["cyclic_node"])
		}
		cm, ok := got["cyclic"].(map[string]any)
		if !ok || cm["ok"] != "yes" || cm["self"] != "<cycle>" {
			t.Errorf("cyclic = %#v", got["cyclic"])
		}
	})

	t.Run("dedicated message and artifact handlers and WithAttrs", func(t *testing.T) {
		for name, handler := range map[string]slog.Handler{
			"cloud-with-attrs": h.WithAttrs([]slog.Attr{
				slog.Any("bound_map", map[string]any{"ok": "yes", "bad": math.NaN()}),
			}),
			"message": &messageCloudHandler{CloudHandler: CloudHandler{
				logger: gcpLogger, client: client, level: slog.LevelDebug, component: "scion-hub", stats: stats,
			}},
			"artifact": &artifactCloudHandler{CloudHandler: CloudHandler{
				logger: gcpLogger, client: client, level: slog.LevelDebug, component: "scion-hub", stats: stats,
			}},
		} {
			t.Run(name, func(t *testing.T) {
				errBuf.Reset()
				slog.New(handler).Info("dedicated event",
					"keep", "ok",
					"nested", map[string]any{"v": math.NaN()},
					"when", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
				)
				if err := gcpLogger.Flush(); err != nil {
					t.Fatalf("Flush failed: %v (stderr: %s)", err, errBuf.String())
				}
				entries := fake.takeEntries()
				if len(entries) != 1 {
					t.Fatalf("entries = %d, want 1", len(entries))
				}
				got := entries[0].GetJsonPayload().AsMap()
				if got["keep"] != "ok" {
					t.Errorf("keep = %#v", got["keep"])
				}
				nested, ok := got["nested"].(map[string]any)
				if !ok || nested["v"] != "NaN" {
					t.Errorf("nested = %#v", got["nested"])
				}
				if s, ok := got["when"].(string); !ok || s == "" {
					t.Errorf("when = %#v (%T), want non-empty string", got["when"], got["when"])
				}
			})
		}
	})
}
