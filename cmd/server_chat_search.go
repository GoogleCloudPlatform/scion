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

package cmd

import (
	"log"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

// chatSearchTimeout returns the configured chat search time limit, or 0 (the
// hub default) when it is unset. An invalid value is logged and ignored: a
// search limit must not keep the hub from starting.
func chatSearchTimeout(cfg *config.GlobalConfig) time.Duration {
	d, err := cfg.Hub.Chat.Search.TimeoutDuration()
	if err != nil {
		log.Printf("Warning: chat search: %v; using the default", err)
		return 0
	}
	return d
}

// webChatSearchOptions returns the web chat store options for chat search
// (ptone/scion#7586) and a func that closes what it opened (a no-op when it
// opened nothing). On a SQLite hub it opens the separate read-only search
// handle, so a slow search cannot hold the main handle's single connection;
// its pool size is the effective search cap. An in-memory database cannot be
// shared by a second pool, and an open failure must not stop the hub: both
// fall back to searching on the main handle, with a log line. A Postgres hub
// needs no option: its store bounds each search with a READ ONLY transaction
// and statement_timeout, and the search cap is clamped to a quarter of the
// shared pool, with one startup line when that lowers the configured value.
func webChatSearchOptions(cfg *config.GlobalConfig) ([]hub.WebChatStoreOption, func()) {
	noop := func() {}
	configured := cfg.Hub.Chat.Search.MaxConcurrent
	switch cfg.Database.Driver {
	case "postgres", "pgx":
		if line, ok := hub.ChatSearchCapStartupLine(configured, "postgres", cfg.Database.MaxOpenConns); ok {
			log.Print(line)
		}
		return nil, noop
	}
	if entc.SQLiteDSNIsMemory(cfg.Database.URL) {
		log.Printf("Chat search: in-memory SQLite database, searching on the main handle")
		return nil, noop
	}
	db, err := entc.OpenSQLiteSearchReader(cfg.Database.URL)
	if err != nil {
		log.Printf("Warning: chat search: opening the read-only search handle failed, searching on the main handle: %v", err)
		return nil, noop
	}
	effective, _ := hub.EffectiveChatSearchCap(configured, "sqlite", 0)
	db.SetMaxOpenConns(effective)
	log.Printf("Chat search: read-only search handle opened")
	return []hub.WebChatStoreOption{hub.WithSearchDB(db)}, func() { _ = db.Close() }
}
