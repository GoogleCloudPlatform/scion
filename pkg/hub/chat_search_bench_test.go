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

//go:build chatsearchbench && !no_sqlite

package hub

// Chat search load tests L1-L3 (ptone/scion#7586, design v3 section 11).
// Excluded from the default build: run them with
//
//	CHATSEARCH_BENCH_N=1000000 go test -tags chatsearchbench -run '^TestChatSearchBench' -v -timeout 60m ./pkg/hub
//
// CHATSEARCH_BENCH_N sets the fixture size (default 100000, the reduced
// form). CHATSEARCH_BENCH_DIR, when set, keeps the fixture database there
// and reuses it across runs of the same size. Each test logs its numbers as
// "BENCH ..." lines for the PR body.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	benchProjects      = 200
	benchConversations = 2000
	benchTimeout       = DefaultChatSearchTimeout
	// benchRareTerm appears in no fixture message, so an all-scope search
	// for it reads every visible web row: the incident's worst case.
	benchRareTerm = "zqxjv-rare"
	// benchP99Floor is the absolute slack added to the 10% p99 budget, so
	// sub-millisecond baselines do not fail on scheduler noise.
	benchP99Floor = 2 * time.Millisecond
)

// holdRecorder is a txdiag.Recorder that keeps the longest transaction or
// connection hold on the main handle: the measure db_tx_long_hold reports.
type holdRecorder struct {
	mu    sync.Mutex
	maxMS float64
}

func (r *holdRecorder) RecordTxHold(_ context.Context, ms float64, _, _, _ string) {
	r.mu.Lock()
	if ms > r.maxMS {
		r.maxMS = ms
	}
	r.mu.Unlock()
}
func (r *holdRecorder) RecordTxCommit(context.Context, float64, string, string) {}
func (r *holdRecorder) RecordLockWait(context.Context, float64, string, string) {}
func (r *holdRecorder) IncDiagDropped(context.Context, int64)                   {}

// takeMax returns the longest hold recorded since the last call, and resets it.
func (r *holdRecorder) takeMax() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := time.Duration(r.maxMS * float64(time.Millisecond))
	r.maxMS = 0
	return d
}

type benchFixture struct {
	holds    *holdRecorder
	main     *sql.DB
	reader   *sql.DB
	wcs      WebChatStore
	projects []string
	n        int
}

func benchN(t *testing.T) int {
	t.Helper()
	if v := os.Getenv("CHATSEARCH_BENCH_N"); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(t, err)
		return n
	}
	return 100000
}

// benchMessageText draws a message: log-normal length (median 120
// characters, p99 about 4 KB), 5% CJK and 2% with emoji.
func benchMessageText(rng *rand.Rand) string {
	// ln(120) = 4.79; sigma 1.5 puts p99 near 120 * e^(2.33*1.5) = ~3.9 KB.
	n := int(math.Exp(4.79 + 1.5*rng.NormFloat64()))
	if n < 2 {
		n = 2
	}
	if n > 16384 {
		n = 16384
	}
	words := []string{"deploy", "review", "the", "agent", "build", "failed", "merge", "please", "check", "logs", "branch", "test", "ok", "thanks", "error", "retry"}
	var b strings.Builder
	switch r := rng.Float64(); {
	case r < 0.05:
		cjk := []rune("日本語の検索テスト中文消息한국어")
		for b.Len() < n {
			b.WriteRune(cjk[rng.Intn(len(cjk))])
		}
	default:
		for b.Len() < n {
			b.WriteString(words[rng.Intn(len(words))])
			b.WriteByte(' ')
		}
		if r < 0.07 {
			b.WriteString("🙂🚀")
		}
	}
	return b.String()
}

// openBenchFixture builds (or reuses) a file-backed hub database with the
// real Ent schema and n web messages over 200 projects, Zipf-distributed
// over 2000 conversations plus DMs.
func openBenchFixture(t *testing.T) *benchFixture {
	t.Helper()
	n := benchN(t)
	dir := os.Getenv("CHATSEARCH_BENCH_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	path := filepath.Join(dir, fmt.Sprintf("chatsearch-bench-%d.db", n))
	_, statErr := os.Stat(path)
	reuse := statErr == nil

	// The main handle goes through txdiag, as the hub's does, so the bench
	// reads the same hold measure db_tx_long_hold reports.
	client, err := entc.OpenSQLite("file:"+path+"?cache=shared", entc.PoolConfig{MaxOpenConns: 1, Diag: txdiag.QuietConfig()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	mon, ok := entc.DiagMonitor(client)
	require.True(t, ok, "the main handle must be opened through txdiag")
	holds := &holdRecorder{}
	mon.SetRecorder(holds)
	ctx := context.Background()
	if !reuse {
		require.NoError(t, entc.AutoMigrate(ctx, client))
	}
	main := entc.SQLDB(client.Driver())
	require.NotNil(t, main)

	rng := rand.New(rand.NewSource(7586))
	projects := make([]string, benchProjects)
	for i := range projects {
		projects[i] = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("bench-project-%d", i))).String()
	}
	if !reuse {
		start := time.Now()
		zipf := rand.NewZipf(rng, 1.2, 1, benchConversations-1)
		base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		tx, err := main.BeginTx(ctx, nil)
		require.NoError(t, err)
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO messages
 (id, project_id, sender, recipient, msg, type, urgent, broadcasted, read, dispatch_state, channel, thread_id, created)
 VALUES (?, ?, 'user:bench', 'agent:bench', ?, 'instruction', false, false, false, 'delivered', 'web', ?, ?)`)
		require.NoError(t, err)
		for i := 0; i < n; i++ {
			conv := int(zipf.Uint64())
			proj := projects[conv%benchProjects]
			thread := fmt.Sprintf("topic-%04d", conv)
			if rng.Float64() < 0.1 {
				thread = fmt.Sprintf("dm:user:u%03d:user:u%03d", conv%97, conv%89)
			}
			created := base.Add(time.Duration(i) * 3 * time.Second)
			_, err := stmt.ExecContext(ctx, uuid.New().String(), proj, benchMessageText(rng), thread, created)
			require.NoError(t, err)
		}
		require.NoError(t, stmt.Close())
		require.NoError(t, tx.Commit())
		_, err = main.ExecContext(ctx, "ANALYZE")
		require.NoError(t, err)
		t.Logf("BENCH fixture n=%d built in %v", n, time.Since(start).Round(time.Millisecond))
	}

	// The search reader is opened WITHOUT txdiag (OpenSQLiteSearchReader uses a
	// plain sql.Open), so it shares no monitor with the main handle: a search
	// connection's hold, which the design allows, never reaches holds.
	reader, err := entc.OpenSQLiteSearchReader(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	wcs := NewWebChatStore(main, "sqlite", WithSearchDB(reader))
	require.NoError(t, wcs.Init())
	return &benchFixture{holds: holds, main: main, reader: reader, wcs: wcs, projects: projects, n: n}
}

// latencies collects durations and reports quantiles.
type latencies struct {
	mu sync.Mutex
	d  []time.Duration
}

func (l *latencies) add(d time.Duration) {
	l.mu.Lock()
	l.d = append(l.d, d)
	l.mu.Unlock()
}

func (l *latencies) quantile(q float64) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), l.d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(math.Ceil(q*float64(len(s))))-1]
}

// max returns the longest duration recorded.
func (l *latencies) max() time.Duration {
	return l.quantile(1)
}

func (l *latencies) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.d)
}

// mainHandleLoad runs the L1 background load on the main handle for d: a
// 50 msg/s insert load and 200 req/s of other reads. It returns the insert
// and read latencies and the number of failed operations.
func (f *benchFixture) mainHandleLoad(t *testing.T, d time.Duration) (ins, reads *latencies, failed int64) {
	t.Helper()
	ins, reads = &latencies{}, &latencies{}
	var fails atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var wg sync.WaitGroup
	tick := func(rate int, op func()) {
		defer wg.Done()
		tk := time.NewTicker(time.Second / time.Duration(rate))
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				wg.Add(1)
				go func() { defer wg.Done(); op() }()
			}
		}
	}
	rng := rand.New(rand.NewSource(1))
	var rngMu sync.Mutex
	wg.Add(2)
	go tick(50, func() {
		rngMu.Lock()
		proj := f.projects[rng.Intn(len(f.projects))]
		rngMu.Unlock()
		start := time.Now()
		_, err := f.main.Exec(`INSERT INTO messages
 (id, project_id, sender, recipient, msg, type, urgent, broadcasted, read, dispatch_state, channel, thread_id, created)
 VALUES (?, ?, 'user:load', 'agent:load', 'load message', 'instruction', false, false, false, 'delivered', 'web', 'topic-load', ?)`,
			uuid.New().String(), proj, time.Now().UTC())
		ins.add(time.Since(start))
		if err != nil {
			fails.Add(1)
		}
	})
	go tick(200, func() {
		rngMu.Lock()
		proj := f.projects[rng.Intn(len(f.projects))]
		rngMu.Unlock()
		start := time.Now()
		rows, err := f.main.Query(`SELECT id, msg FROM messages WHERE project_id = ? ORDER BY created DESC, id DESC LIMIT 20`, proj)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			_ = rows.Close()
		}
		reads.add(time.Since(start))
		if err != nil {
			fails.Add(1)
		}
	})
	wg.Wait()
	return ins, reads, fails.Load()
}

// rareSearch runs one all-scope search for a term that matches nothing,
// under the hub's time limit, and returns its duration and error.
func (f *benchFixture) rareSearch(parent context.Context, term string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(parent, benchTimeout)
	defer cancel()
	start := time.Now()
	_, _, err := f.wcs.SearchChatMessages(ctx, ChatSearchFilter{Query: term, ProjectIDs: f.projects, Limit: 20, DMParticipantUserID: "u001"})
	return time.Since(start), err
}

func within10(got, base time.Duration) bool {
	return got <= base+base/10+benchP99Floor
}

// TestChatSearchBench_L1WriterIsolation: two concurrent rare-term all-scope
// searches, looping, do not raise insert or other-read p99 on the main
// handle by more than 10%, and no main-handle operation fails.
func TestChatSearchBench_L1WriterIsolation(t *testing.T) {
	f := openBenchFixture(t)
	const d = 20 * time.Second
	baseIns, baseReads, baseFail := f.mainHandleLoad(t, d)

	// Planted control: a transaction held about 1.5 s on the main handle must
	// show as a hold of at least 1 s, so a quiet result cannot mean a blind probe.
	f.holds.takeMax()
	tx, err := f.main.Begin()
	require.NoError(t, err)
	time.Sleep(1500 * time.Millisecond)
	require.NoError(t, tx.Commit())
	planted := f.holds.takeMax()
	t.Logf("BENCH L1 n=%d planted_hold=%v (control; want >= 1s)", f.n, planted)
	require.GreaterOrEqual(t, planted, time.Second, "the planted 1.5 s hold must register")

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var searches, timeouts, otherErr atomic.Int64
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_, err := f.rareSearch(ctx, benchRareTerm)
				searches.Add(1)
				switch {
				case err == nil:
				case isChatSearchTimeout(err):
					timeouts.Add(1)
				case errors.Is(err, context.Canceled):
				default:
					otherErr.Add(1)
				}
			}
		}()
	}
	ins, reads, fail := f.mainHandleLoad(t, d)
	cancel()
	wg.Wait()
	longestHold := f.holds.takeMax()

	t.Logf("BENCH L1 n=%d baseline insert_p99=%v read_p99=%v (ops %d/%d, failed %d)",
		f.n, baseIns.quantile(0.99), baseReads.quantile(0.99), baseIns.count(), baseReads.count(), baseFail)
	t.Logf("BENCH L1 n=%d with_search insert_p99=%v read_p99=%v (ops %d/%d, failed %d) searches=%d timeouts=%d other_errors=%d",
		f.n, ins.quantile(0.99), reads.quantile(0.99), ins.count(), reads.count(), fail, searches.Load(), timeouts.Load(), otherErr.Load())
	require.Zero(t, baseFail+fail, "no main-handle operation may fail")
	require.Zero(t, otherErr.Load(), "searches end in success or a timeout only")
	require.True(t, within10(ins.quantile(0.99), baseIns.quantile(0.99)), "insert p99 within 10%% of baseline")
	require.True(t, within10(reads.quantile(0.99), baseReads.quantile(0.99)), "read p99 within 10%% of baseline")
	// Second line for "no db_tx_long_hold over 1 s on the main handle" (design 11 L1), next to the txdiag
	// measure above: the slowest single main-handle operation during the searches, timed from before the
	// connection is acquired, so a queue behind one holder shows here too.
	slowest := ins.max()
	if r := reads.max(); r > slowest {
		slowest = r
	}
	t.Logf("BENCH L1 n=%d db_tx_long_hold_max=%v (txdiag, main handle, during searches; limit 1s)", f.n, longestHold)
	t.Logf("BENCH L1 n=%d main_handle_slowest_op=%v (second line: timed from before connection acquisition; limit 1s)", f.n, slowest)
	require.Less(t, longestHold, time.Second, "no main-handle transaction or connection held over 1 s while searches ran")
	require.Less(t, slowest, time.Second, "no main-handle operation took over 1 s while searches ran")
}

// TestChatSearchBench_L2Cancellation: a search stopped by its deadline, or
// aborted by the client mid-search, ends within 100 ms of the cut-off
// (SQLite interrupt), and the search handle is reusable at once.
func TestChatSearchBench_L2Cancellation(t *testing.T) {
	f := openBenchFixture(t)

	full, err := f.rareSearch(context.Background(), benchRareTerm)
	timedOut := err != nil && isChatSearchTimeout(err)
	t.Logf("BENCH L2 n=%d deadline: elapsed=%v timed_out=%t (limit %v)", f.n, full, timedOut, benchTimeout)
	if timedOut {
		require.Less(t, full, benchTimeout+100*time.Millisecond, "the statement stops within 100 ms of the deadline")
	} else {
		require.NoError(t, err, "a search under the limit succeeds")
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	aborted, err := f.rareSearch(ctx, benchRareTerm)
	t.Logf("BENCH L2 n=%d client_abort_at=500ms: elapsed=%v err_canceled=%t", f.n, aborted, errors.Is(err, context.Canceled))
	if err != nil {
		require.ErrorIs(t, err, context.Canceled)
		require.Less(t, aborted, 600*time.Millisecond, "an aborted statement stops within 100 ms")
	}

	start := time.Now()
	_, _, err = f.wcs.SearchChatMessages(context.Background(), ChatSearchFilter{Query: "deploy", ProjectIDs: f.projects, Limit: 20})
	t.Logf("BENCH L2 n=%d reuse common-term search: elapsed=%v", f.n, time.Since(start))
	require.NoError(t, err, "the search handle is reusable")
}

// TestChatSearchBench_L3IncidentReplay replays the 2026-11-07 shape: three
// unscoped rare-term searches 1 s apart (terms of 7, 9 and 11 characters).
// Every search ends by the limit plus 200 ms, and main-handle reads during
// the replay show no queueing (p99 within 10% of the baseline).
func TestChatSearchBench_L3IncidentReplay(t *testing.T) {
	f := openBenchFixture(t)
	const d = 6 * time.Second
	_, baseReads, _ := f.mainHandleLoad(t, d)

	terms := []string{"zqxjvkw", "zqxjvkwpy", "zqxjvkwpybn"}
	var wg sync.WaitGroup
	durs := make([]time.Duration, len(terms))
	errs := make([]error, len(terms))
	gate := newSearchGate(DefaultChatSearchMaxConcurrent)
	busy := make([]bool, len(terms))
	// Add every search before launching any, so wg.Wait cannot race an Add.
	wg.Add(len(terms))
	go func() {
		for i, term := range terms {
			go func(i int, term string) {
				defer wg.Done()
				release, ok := gate.tryAcquire(fmt.Sprintf("user-%d", i))
				if !ok {
					busy[i] = true
					return
				}
				defer release()
				durs[i], errs[i] = f.rareSearch(context.Background(), term)
			}(i, term)
			time.Sleep(time.Second)
		}
	}()
	_, reads, fail := f.mainHandleLoad(t, d)
	wg.Wait()

	for i := range terms {
		t.Logf("BENCH L3 n=%d search %d (len %d): elapsed=%v busy=%t timed_out=%t", f.n, i, len(terms[i]), durs[i], busy[i], errs[i] != nil && isChatSearchTimeout(errs[i]))
		if !busy[i] {
			require.Less(t, durs[i], benchTimeout+200*time.Millisecond)
			if errs[i] != nil {
				require.True(t, isChatSearchTimeout(errs[i]), "search %d: %v", i, errs[i])
			}
		}
	}
	t.Logf("BENCH L3 n=%d main read_p99 baseline=%v during_replay=%v failed=%d", f.n, baseReads.quantile(0.99), reads.quantile(0.99), fail)
	require.Zero(t, fail)
	require.True(t, within10(reads.quantile(0.99), baseReads.quantile(0.99)), "no queueing on the main handle")
}
