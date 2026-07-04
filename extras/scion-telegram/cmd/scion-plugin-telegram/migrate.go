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
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/GoogleCloudPlatform/scion/extras/scion-telegram/internal/telegram"
)

func runMigrate() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	fromPath := fs.String("from", "", "Path to source SQLite database")
	toURL := fs.String("to", "", "Postgres connection URL")
	fs.Parse(os.Args[2:])

	if *fromPath == "" || *toURL == "" {
		fmt.Fprintln(os.Stderr, "Usage: scion-plugin-telegram migrate --from <sqlite-path> --to <postgres-url>")
		os.Exit(1)
	}

	log.Info("Starting SQLite to Postgres migration", "from", *fromPath, "to", "(redacted)")

	ctx := context.Background()

	counts, err := telegram.MigrateSQLiteToPostgres(ctx, *fromPath, *toURL)
	if err != nil {
		log.Error("Migration failed", "error", err)
		os.Exit(1)
	}

	log.Info("Migration complete")
	fmt.Println("\nRow counts:")
	for table, count := range counts {
		fmt.Printf("  %-30s %d\n", table, count)
	}
}
