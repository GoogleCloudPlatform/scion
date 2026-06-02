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

//go:build integration

// Category 7 — Multi-process. These prove the coordination primitives hold across
// SEPARATE OS PROCESSES, not just goroutines in one process — the real
// multi-replica hub topology. The parent test forks the test binary (os.Args[0])
// to run a dedicated worker entrypoint against a shared database:
//
//   - advisory-lock exclusivity: two independent processes contend for the same
//     pg_advisory_lock; exactly one wins;
//   - cross-process LISTEN/NOTIFY: a notification published by a child process is
//     delivered to a listener in the parent process.
//
// The TestWorker_* functions are those entrypoints. They no-op (skip) unless their
// SCION_TEST_WORKER_DSN env var is set, so they are inert in a normal suite run
// and only do work when launched by a parent test via workerCommand.
package integrationtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// crossProcessLockKey is a fixed advisory-lock key for the multi-process lock
// test. It is namespaced under the 0x5C10 ("SCIO") prefix like the production
// keys but uses a distinct value so it never collides with them. Each package run
// gets a fresh ephemeral database and runs this test once, so a constant is safe.
const crossProcessLockKey = int64(0x5C10FADE)

// workerCommand builds an exec.Cmd that re-invokes THIS test binary to run a
// single TestWorker_* entrypoint as a child process. SCION_TEST_POSTGRES_URL is
// stripped from the child's environment so its TestMain does not provision a
// second ephemeral database; the child talks only to the DSN passed in extraEnv.
func workerCommand(testName string, extraEnv map[string]string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$", "-test.v=true")
	env := make([]string, 0, len(os.Environ())+len(extraEnv))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SCION_TEST_POSTGRES_URL=") {
			continue
		}
		env = append(env, e)
	}
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	return cmd
}

// TestMultiProcess_AdvisoryLockExclusivity forks two worker processes that each
// try to take the SAME advisory lock and hold it. Exactly one must acquire it;
// the other must observe it held and report BLOCKED. This is the cross-process
// guarantee behind "run this maintenance job on exactly one replica".
func TestMultiProcess_AdvisoryLockExclusivity(t *testing.T) {
	requirePG(t)
	dsn := enttest.NewSchemaURL(t)

	env := map[string]string{
		"SCION_TEST_WORKER_DSN":     dsn,
		"SCION_TEST_WORKER_LOCKKEY": strconv.FormatInt(crossProcessLockKey, 10),
	}

	const procs = 2
	outputs := make([]string, procs)
	var wg sync.WaitGroup
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, _ := workerCommand("TestWorker_AdvisoryLock", env).CombinedOutput()
			outputs[i] = string(out)
		}(i)
	}
	wg.Wait()

	acquired, blocked := 0, 0
	for _, o := range outputs {
		if strings.Contains(o, "WORKER_RESULT: ACQUIRED") {
			acquired++
		}
		if strings.Contains(o, "WORKER_RESULT: BLOCKED") {
			blocked++
		}
	}
	assert.Equalf(t, 1, acquired, "exactly one process must acquire the advisory lock\n--- proc0 ---\n%s\n--- proc1 ---\n%s", outputs[0], outputs[1])
	assert.Equalf(t, 1, blocked, "exactly one process must be blocked\n--- proc0 ---\n%s\n--- proc1 ---\n%s", outputs[0], outputs[1])
}

// TestWorker_AdvisoryLock is the child entrypoint for the advisory-lock test. It
// takes the lock (if free) and holds it long enough to guarantee the sibling
// process's attempt overlaps, then reports the outcome on stdout.
func TestWorker_AdvisoryLock(t *testing.T) {
	dsn := os.Getenv("SCION_TEST_WORKER_DSN")
	if dsn == "" {
		t.Skip("worker entrypoint; launched only by a parent multi-process test")
	}
	key, err := strconv.ParseInt(os.Getenv("SCION_TEST_WORKER_LOCKKEY"), 10, 64)
	require.NoError(t, err)
	ctx := context.Background()

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		fmt.Println("WORKER_RESULT: ERROR open:", err)
		t.Fatal(err)
	}
	defer client.Close()
	cs := entadapter.NewCompositeStore(client)

	acquired, release, err := cs.TryAdvisoryLock(ctx, store.AdvisoryLockKey(key))
	if err != nil {
		fmt.Println("WORKER_RESULT: ERROR lock:", err)
		t.Fatal(err)
	}
	defer func() { _ = release() }()

	if acquired {
		fmt.Println("WORKER_RESULT: ACQUIRED")
		// Hold the lock well past sibling startup jitter so its attempt is
		// guaranteed to land while we hold it.
		time.Sleep(3 * time.Second)
		return
	}
	fmt.Println("WORKER_RESULT: BLOCKED")
}

// TestMultiProcess_CrossProcessNotify starts LISTENing in the parent, then forks a
// child process that publishes N notifications on the same channel via a separate
// connection. The parent must receive all N — proving NOTIFY crosses the
// process/connection boundary (the basis for cross-replica event delivery).
func TestMultiProcess_CrossProcessNotify(t *testing.T) {
	requirePG(t)
	dsn := enttest.NewSchemaURL(t)
	ctx := context.Background()

	// Establish the listener BEFORE forking the publisher so no notification is
	// missed (NOTIFY only reaches sessions already LISTENing at send time).
	listener := pgConnect(t, dsn)
	channel := uniqueChannel("xproc")
	_, err := listener.Exec(ctx, "LISTEN "+channel)
	require.NoError(t, err)

	const n = 50
	cmd := workerCommand("TestWorker_NotifyPublisher", map[string]string{
		"SCION_TEST_WORKER_DSN":     dsn,
		"SCION_TEST_WORKER_CHANNEL": channel,
		"SCION_TEST_WORKER_COUNT":   strconv.Itoa(n),
	})
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	require.NoError(t, cmd.Start())

	got := 0
	for got < n {
		wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		note, werr := listener.WaitForNotification(wctx)
		cancel()
		require.NoErrorf(t, werr, "received %d/%d cross-process notifications before timeout\nworker output:\n%s", got, n, out.String())
		require.Equal(t, channel, note.Channel)
		got++
	}

	require.NoErrorf(t, cmd.Wait(), "publisher process failed\nworker output:\n%s", out.String())
	assert.Contains(t, out.String(), "WORKER_RESULT: PUBLISHED")
}

// TestWorker_NotifyPublisher is the child entrypoint for the cross-process NOTIFY
// test. It connects independently and publishes N notifications on the channel.
func TestWorker_NotifyPublisher(t *testing.T) {
	dsn := os.Getenv("SCION_TEST_WORKER_DSN")
	if dsn == "" {
		t.Skip("worker entrypoint; launched only by a parent multi-process test")
	}
	channel := os.Getenv("SCION_TEST_WORKER_CHANNEL")
	n, err := strconv.Atoi(os.Getenv("SCION_TEST_WORKER_COUNT"))
	require.NoError(t, err)
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Println("WORKER_RESULT: ERROR connect:", err)
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	for i := 0; i < n; i++ {
		if _, err := conn.Exec(ctx, "SELECT pg_notify($1, $2)", channel, strconv.Itoa(i)); err != nil {
			fmt.Println("WORKER_RESULT: ERROR notify:", err)
			t.Fatal(err)
		}
	}
	fmt.Println("WORKER_RESULT: PUBLISHED")
}
