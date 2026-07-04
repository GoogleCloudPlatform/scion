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

package hub

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestAdminSignalIntegration_PublishReceiveRoundTrip(t *testing.T) {
	dsn := os.Getenv("SCION_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set SCION_TEST_POSTGRES_DSN to run admin signal integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	listener := NewAdminSignalListener(ctx, dsn, slog.Default())
	defer listener.Close()

	received := make(chan AdminSignal, 1)
	listener.OnSignal(func(s AdminSignal) {
		received <- s
	})

	// Give the listener time to connect and LISTEN.
	time.Sleep(500 * time.Millisecond)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	sent := AdminSignal{
		Integration: "discord",
		Kind:        "update",
		ID:          "test-round-trip-id",
	}
	if err := PublishAdminSignalDB(ctx, db, sent); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-received:
		if got.Integration != sent.Integration {
			t.Errorf("Integration: got %q, want %q", got.Integration, sent.Integration)
		}
		if got.Kind != sent.Kind {
			t.Errorf("Kind: got %q, want %q", got.Kind, sent.Kind)
		}
		if got.ID != sent.ID {
			t.Errorf("ID: got %q, want %q", got.ID, sent.ID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for admin signal delivery")
	}
}
