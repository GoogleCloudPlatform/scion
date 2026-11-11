/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// C1: the real layout. sciontool init runs the pipeline as root with
// HOME=/root and passes the resolved agent home; hook processes run as the
// agent user with HOME=<agent home>. Both must resolve the same outbox, the
// root side must chown everything it creates to the agent user, and the
// drainer must deliver events from both producers.
func TestAnalyticsRootInitAndAgentHookShareOutbox(t *testing.T) {
	setAnalyticsEnv(t, "antigravity")
	agentHome := filepath.Join(t.TempDir(), "home", "scion")
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	rootHome := filepath.Join(t.TempDir(), "root")
	t.Setenv("HOME", rootHome) // init's own HOME, which must NOT be used

	var mu sync.Mutex
	chowned := map[string]OutboxOwner{}
	prevEuid, prevChown := outboxGeteuid, outboxChown
	outboxGeteuid = func() int { return 0 }
	outboxChown = func(p string, uid, gid int) error {
		mu.Lock()
		defer mu.Unlock()
		name := filepath.Base(p)
		if i := strings.Index(name, ".tmp-"); strings.HasPrefix(name, ".") && i > 0 {
			name = name[1:i] // writeFileFrom chowns ".<name>.tmp-*" before the rename
		}
		chowned[name] = OutboxOwner{uid, gid}
		return nil
	}
	defer func() { outboxGeteuid, outboxChown = prevEuid, prevChown }()

	var entries []logging.Entry
	p := &Pipeline{}
	p.SetAnalyticsOutbox(agentHome, 1000, 1001)
	p.analytics.resolver = func(context.Context) (string, error) { return "hub-1", nil }
	p.analytics.start(context.Background(), &GCPExporter{analyticsSink: func(e logging.Entry) error {
		mu.Lock()
		defer mu.Unlock()
		entries = append(entries, e)
		return nil
	}})
	wantDir := filepath.Join(agentHome, ".scion", AnalyticsOutboxDirName)
	if got := p.AnalyticsOutboxDir(); got != wantDir {
		t.Fatalf("pipeline outbox = %q, want %q (agent home, not root's HOME)", got, wantDir)
	}
	if _, err := os.Stat(filepath.Join(rootHome, ".scion")); !os.IsNotExist(err) {
		t.Error("nothing may be created under root's HOME")
	}

	// Native producer (root side) appends.
	d := bareUsageDeriver(claudeUsageRule{})
	d.analytics = p.analytics.native
	feed(d, claudeLogs(claudeRecord("req_011CUabcdefghijklmnopq", "1", 1_000, fullClaudeTokens)))

	// Hook producer (agent side): resolves from its own HOME = agent home.
	t.Setenv("HOME", agentHome)
	hookHome, _ := os.UserHomeDir()
	hookSide := NewLazyUsageAnalytics(AnalyticsOutboxDirFor(hookHome), nil, telemetrycontract.UsageSourceHooks)
	if _, err := hookSide.Record(UsageObservation{SourceKind: "hook.antigravity.model_end", Status: "success", Calls: 1,
		Identity: telemetrycontract.SourceIdentityComposite, SourceEventID: CompositeSourceEventID("x"), TokenStatus: "not_reported"}); err != nil {
		t.Fatal(err)
	}
	if hookSide.Outbox().Dir() != wantDir {
		t.Fatalf("hook outbox = %q, want %q", hookSide.Outbox().Dir(), wantDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.analytics.stop(ctx)
	if len(entries) != 2 {
		t.Fatalf("delivered %d events, want 2 (native + hook)", len(entries))
	}
	for _, name := range []string{AnalyticsOutboxDirName, "index", "lock", "epoch", "staging.jsonl", "seq", "hub_id"} {
		if got, ok := chowned[name]; !ok || got != (OutboxOwner{1000, 1001}) {
			t.Errorf("%s not chowned to the agent user (got %v, %v)", name, got, ok)
		}
	}
}

// R1: a permanently rejected line is dead-lettered (kept, counted) and
// later lines are still delivered; a transient error is retried.
func TestOutboxDrainDeadLettersPermanentRejection(t *testing.T) {
	o, lines := preparedReady(t, 3)
	calls := 0
	send := func(_ context.Context, line []byte) error {
		calls++
		if string(line) == lines[1] {
			return PermanentSendError(errors.New("entry rejected"))
		}
		return nil
	}
	n, err := o.Drain(context.Background(), send)
	if err != nil || n != 2 {
		t.Fatalf("Drain = %d, %v; want 2 delivered past the poison line", n, err)
	}
	dl, _ := os.ReadFile(filepath.Join(o.Dir(), "dead_letter.jsonl"))
	if strings.TrimSpace(string(dl)) != lines[1] {
		t.Errorf("dead letter = %q", dl)
	}
	if st := o.Stats(); st.DeadLetters != 1 || st.PersistedDeadLetters != 1 || st.Delivered != 2 || st.PendingBytes != 0 {
		t.Errorf("stats = %+v", st)
	}
}

// R1: an event over the line cap is refused like an overflow: seq consumed
// (visible gap), persisted count, nothing written.
func TestOutboxOversizeRefused(t *testing.T) {
	o := testOutbox(t)
	big := draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:big", 1)
	big.SessionID = strings.Repeat("s", analyticsMaxLineBytes)
	mustAppend(t, o, big, AppendOversize)
	next := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:ok", 1), AppendWritten)
	if next.ProducerSeq != 2 || o.Stats().PersistedOversize != 1 || len(stagingLines(t, o)) != 1 {
		t.Errorf("seq %d, oversize %d, lines %d", next.ProducerSeq, o.Stats().PersistedOversize, len(stagingLines(t, o)))
	}
	if analyticsMaxLineBytes > 256<<10 {
		t.Error("line cap must stay below Cloud Logging's per-entry limit")
	}
}

func TestOutboxPinHubIDValidates(t *testing.T) {
	o := testOutbox(t)
	for _, bad := range []string{"", strings.Repeat("a", 129), "hub id", "hub\n", `{"x":1}`, "ü"} {
		if _, err := o.PinHubID(bad); err == nil {
			t.Errorf("PinHubID(%q) accepted", bad)
		}
	}
	if _, err := o.PinHubID("0b6f3c2e-1a2b-4c3d-9e8f-abcdef012345"); err != nil {
		t.Errorf("UUID hub ID rejected: %v", err)
	}
}

func TestSendAnalyticsClassifiesPermanentErrors(t *testing.T) {
	line := []byte(`{"event_ts":"2026-10-22T00:00:00Z"}`)
	for name, tc := range map[string]struct {
		sinkErr   error
		line      []byte
		permanent bool
	}{
		"invalid argument": {status.Error(codes.InvalidArgument, "too large"), line, true},
		"unavailable":      {status.Error(codes.Unavailable, "down"), line, false},
		"permission":       {status.Error(codes.PermissionDenied, "no"), line, false},
		"unparsable line":  {nil, []byte("not json"), true},
		"bad event_ts":     {nil, []byte(`{"event_ts":"x"}`), true},
	} {
		g := &GCPExporter{analyticsSink: func(logging.Entry) error { return tc.sinkErr }}
		err := g.SendAnalytics(context.Background(), tc.line)
		if IsPermanentSendError(err) != tc.permanent {
			t.Errorf("%s: err %v permanent=%v, want %v", name, err, IsPermanentSendError(err), tc.permanent)
		}
	}
}

// R2: the final drain gets at most half of the remaining stop budget, so
// the exporter shutdown that follows keeps its share even when the sink
// hangs.
func TestAnalyticsStopLeavesBudgetForExporter(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	hang := make(chan struct{})
	defer close(hang)
	a := analyticsState{outboxDir: filepath.Join(t.TempDir(), "ob"), resolver: func(context.Context) (string, error) { return "hub", nil }}
	a.start(context.Background(), &GCPExporter{analyticsSink: func(logging.Entry) error { <-hang; return nil }})
	d := bareUsageDeriver(claudeUsageRule{})
	d.analytics = a.native
	feed(d, claudeLogs(claudeRecord("req_011CUabcdefghijklmnopq", "1", 1_000, fullClaudeTokens)))

	// The sink ignores ctx; use a send func that honours it instead.
	a.drainer.send = func(ctx context.Context, _ []byte) error { <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	start := time.Now()
	a.stop(ctx)
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Errorf("final drain took %v of an 800ms budget; must leave at least half", elapsed)
	}
	if ctx.Err() != nil {
		t.Error("the caller's budget must not be exhausted by the analytics drain")
	}
}

// R3: a lock timeout loses the event but leaves persisted evidence; an I/O
// failure after the lock is held consumes the seq (visible gap).
func TestOutboxAppendFailuresLeaveEvidence(t *testing.T) {
	o := testOutbox(t)
	f, err := os.OpenFile(filepath.Join(o.Dir(), "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if _, err := o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1)); !errors.Is(err, ErrAnalyticsOutboxLocked) {
		t.Fatalf("append under a held lock = %v", err)
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	_ = f.Close()
	if st := o.Stats(); st.PersistedAppendFailed != 1 {
		t.Errorf("persisted append_failed = %d, want 1", st.PersistedAppendFailed)
	}

	// Make the staging append fail: staging.jsonl is a directory.
	if err := os.Mkdir(filepath.Join(o.Dir(), "staging.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:b", 1)); err == nil {
		t.Fatal("append into an unwritable staging file succeeded")
	}
	_ = os.Remove(filepath.Join(o.Dir(), "staging.jsonl"))
	next := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:b", 1), AppendWritten)
	if next.ProducerSeq != 2 || next.SourceOccurrence != 1 {
		t.Errorf("after I/O failure: seq %d occurrence %d; want 2 (gap) and 1 (not accepted)", next.ProducerSeq, next.SourceOccurrence)
	}
	if o.Stats().PersistedAppendFailed != 2 {
		t.Errorf("persisted append_failed = %d, want 2", o.Stats().PersistedAppendFailed)
	}
}

// R4: a lazy producer does no I/O until the first Record.
func TestLazyUsageAnalyticsOpensOnFirstRecord(t *testing.T) {
	setAnalyticsEnv(t, "opencode")
	dir := filepath.Join(t.TempDir(), "home", ".scion", AnalyticsOutboxDirName)
	a := NewLazyUsageAnalytics(dir, nil, telemetrycontract.UsageSourceHooks)
	opened := 0
	inner := a.open
	a.open = func() (*AnalyticsOutbox, error) { opened++; return inner() }
	if _, err := os.Stat(dir); !os.IsNotExist(err) || opened != 0 {
		t.Fatal("lazy producer touched the outbox before Record")
	}
	for i := 0; i < 2; i++ {
		if _, err := a.Record(UsageObservation{SourceKind: "hook.opencode.model_end", Status: "success", Calls: 1, TokenStatus: "not_reported"}); err != nil {
			t.Fatal(err)
		}
	}
	if opened != 1 {
		t.Errorf("opened %d times, want 1", opened)
	}
}

// O4: a metric increment with no reported start keeps interval_end and
// temporality (still metric-derived), omitting only interval_start.
func TestMetricObservationUnknownStart(t *testing.T) {
	setAnalyticsEnv(t, "copilot")
	o := testOutbox(t)
	a := NewUsageAnalytics(o, telemetrycontract.UsageSourceNative)
	obs := metricObservation(usageIncrement{Status: "success", Calls: 2, intervalEndNanos: 1_761_091_200_000_000_000, temporality: telemetrycontract.TemporalityCumulativeDelta}, "fp")
	if _, err := a.Record(obs); err != nil {
		t.Fatal(err)
	}
	ev := decodeUsage(t, stagingLines(t, o)[0])
	if ev.IntervalStart != "" || ev.IntervalEnd == "" || ev.Temporality != telemetrycontract.TemporalityCumulativeDelta {
		t.Errorf("interval = %q..%q %q", ev.IntervalStart, ev.IntervalEnd, ev.Temporality)
	}
}
