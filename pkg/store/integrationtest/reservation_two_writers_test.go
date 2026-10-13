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

// Two-writer reservation test. Two separate OS processes, as two Hub
// replicas, insert broker-scoped usage reservations for the same resources at
// the same time. The unique active (limit_definition_id, resource_id) index
// must let exactly one insert win per resource, and the loser must get
// store.ErrActiveReservationExists and read the winner back with
// GetActiveReservation: a winner in another scope is a conflict, one in the
// same scope is a retry.
package integrationtest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// reservationWorkerRows is the number of resources each scenario races on.
const reservationWorkerRows = 100

func TestReservation_TwoWritersAandB_UniqueWinnerClassified(t *testing.T) {
	requirePG(t)
	dsn := enttest.NewSchemaURL(t)
	ctx := context.Background()

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	ld, err := cs.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "max_agents_per_broker_two_writers",
		ResourceType: "agent",
		Unit:         "count",
		System:       true,
	})
	require.NoError(t, err)

	for _, sc := range []struct {
		name   string
		scopes [2]string
	}{
		{"A and B", [2]string{"broker-a", "broker-b"}},
		{"same scope", [2]string{"broker-a", "broker-a"}},
	} {
		t.Run(sc.name, func(t *testing.T) {
			prefix := strings.ReplaceAll(sc.name, " ", "-") + "-"
			startAt := time.Now().Add(2 * time.Second).UnixNano()
			outputs := make([]string, 2)
			var wg sync.WaitGroup
			for w := 0; w < 2; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					out, _ := workerCommand("TestWorker_ReservationWriter", map[string]string{
						"SCION_TEST_WORKER_DSN":      dsn,
						"SCION_TEST_WORKER_LIMIT":    ld.ID,
						"SCION_TEST_WORKER_SCOPE":    sc.scopes[w],
						"SCION_TEST_WORKER_PREFIX":   prefix,
						"SCION_TEST_WORKER_COUNT":    strconv.Itoa(reservationWorkerRows),
						"SCION_TEST_WORKER_START_AT": strconv.FormatInt(startAt, 10),
					}).CombinedOutput()
					outputs[w] = string(out)
				}(w)
			}
			wg.Wait()

			rows := [2]map[int]string{parseWorkerRows(outputs[0]), parseWorkerRows(outputs[1])}
			for w := 0; w < 2; w++ {
				require.Lenf(t, rows[w], reservationWorkerRows, "worker %d reports every row\n%s", w, outputs[w])
			}

			wins := [2]int{}
			for i := 0; i < reservationWorkerRows; i++ {
				r0, r1 := rows[0][i], rows[1][i]
				var winner, loser int
				switch {
				case r0 == "CREATED" && r1 != "CREATED":
					winner, loser = 0, 1
				case r1 == "CREATED" && r0 != "CREATED":
					winner, loser = 1, 0
				default:
					t.Fatalf("row %d: exactly one writer must win, got %q and %q", i, r0, r1)
				}
				wins[winner]++
				want := "RETRY"
				if sc.scopes[0] != sc.scopes[1] {
					want = "CONFLICT " + sc.scopes[winner]
				}
				assert.Equalf(t, want, rows[loser][i], "row %d: the loser classifies the winner", i)

				held, err := cs.GetActiveReservation(ctx, ld.ID, prefix+strconv.Itoa(i))
				require.NoErrorf(t, err, "row %d: exactly one active reservation", i)
				assert.Equalf(t, sc.scopes[winner], held.ScopeID, "row %d: the active row is the winner's", i)
			}
			t.Logf("%s: worker 0 won %d rows, worker 1 won %d rows", sc.name, wins[0], wins[1])
		})
	}
}

// parseWorkerRows reads "WORKER_ROW <i> <outcome>" lines into i -> outcome.
func parseWorkerRows(out string) map[int]string {
	rows := map[int]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		rest, ok := strings.CutPrefix(line, "WORKER_ROW ")
		if !ok {
			continue
		}
		idx, outcome, ok := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		i, err := strconv.Atoi(idx)
		if err != nil {
			continue
		}
		rows[i] = outcome
	}
	return rows
}

// TestWorker_ReservationWriter is the child entrypoint of the two-writer
// reservation test. At the shared start time it inserts a broker-scoped
// reservation in its scope for each resource and reports, per resource,
// CREATED, RETRY (the winner is in its scope) or CONFLICT <winner scope>.
func TestWorker_ReservationWriter(t *testing.T) {
	dsn := os.Getenv("SCION_TEST_WORKER_DSN")
	if dsn == "" {
		t.Skip("worker entrypoint; launched only by a parent multi-process test")
	}
	limitID := os.Getenv("SCION_TEST_WORKER_LIMIT")
	scope := os.Getenv("SCION_TEST_WORKER_SCOPE")
	prefix := os.Getenv("SCION_TEST_WORKER_PREFIX")
	n, err := strconv.Atoi(os.Getenv("SCION_TEST_WORKER_COUNT"))
	require.NoError(t, err)
	startAt, err := strconv.ParseInt(os.Getenv("SCION_TEST_WORKER_START_AT"), 10, 64)
	require.NoError(t, err)
	ctx := context.Background()

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		fmt.Println("WORKER_RESULT: ERROR open:", err)
		t.Fatal(err)
	}
	defer client.Close()
	cs := entadapter.NewCompositeStore(client)

	time.Sleep(time.Until(time.Unix(0, startAt)))
	for i := 0; i < n; i++ {
		resource := prefix + strconv.Itoa(i)
		_, err := cs.CreateUsageReservation(ctx, &store.UsageReservation{
			LimitDefinitionID: limitID,
			SubjectID:         scope,
			ScopeType:         store.QuotaScopeBroker,
			ScopeID:           scope,
			ResourceID:        resource,
			Reserved:          1,
		})
		switch {
		case err == nil:
			fmt.Printf("WORKER_ROW %d CREATED\n", i)
			continue
		case !errors.Is(err, store.ErrActiveReservationExists):
			fmt.Printf("WORKER_ROW %d ERROR insert: %v\n", i, err)
			continue
		}
		held, err := cs.GetActiveReservation(ctx, limitID, resource)
		switch {
		case err != nil:
			fmt.Printf("WORKER_ROW %d ERROR read back: %v\n", i, err)
		case held.ScopeType == store.QuotaScopeBroker && held.ScopeID == scope:
			fmt.Printf("WORKER_ROW %d RETRY\n", i)
		default:
			fmt.Printf("WORKER_ROW %d CONFLICT %s\n", i, held.ScopeID)
		}
	}
	fmt.Println("WORKER_RESULT: DONE")
}
