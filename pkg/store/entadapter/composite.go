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

package entadapter

import (
	"context"
	"fmt"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CompositeStore is a fully Ent-backed implementation of store.Store. Every
// domain is served by a dedicated Ent sub-store; CompositeStore embeds them so
// their methods are promoted to satisfy the store.Store interface, while the
// store-level Close/Ping/Migrate operations act on the shared Ent client.
//
// There is no longer a separate raw-SQL store: all Hub state lives in a single
// Ent database.
type CompositeStore struct {
	*AgentStore
	*ProjectStore
	*UserStore
	*SecretStore
	*TemplateStore
	*NotificationStore
	*ScheduleStore
	*MaintenanceStore
	*MessageStore
	*ExternalStore
	*BrokerSecretStore
	*AllowListStore
	*GroupStore
	*PolicyStore

	client *ent.Client
}

// Compile-time assertion that CompositeStore satisfies the full store.Store
// interface purely through its embedded Ent-backed sub-stores.
var _ store.Store = (*CompositeStore)(nil)

// NewCompositeStore creates a store.Store backed entirely by the given Ent
// client. Each domain sub-store shares the same client and therefore the same
// underlying database, so cross-domain foreign keys (e.g. group -> project,
// agent -> project) resolve natively without any shadow synchronization.
func NewCompositeStore(client *ent.Client) *CompositeStore {
	return &CompositeStore{
		AgentStore:        NewAgentStore(client),
		ProjectStore:      NewProjectStore(client),
		UserStore:         NewUserStore(client),
		SecretStore:       NewSecretStore(client),
		TemplateStore:     NewTemplateStore(client),
		NotificationStore: NewNotificationStore(client),
		ScheduleStore:     NewScheduleStore(client),
		MaintenanceStore:  NewMaintenanceStore(client),
		MessageStore:      NewMessageStore(client),
		ExternalStore:     NewExternalStore(client),
		BrokerSecretStore: NewBrokerSecretStore(client),
		AllowListStore:    NewAllowListStore(client),
		GroupStore:        NewGroupStore(client),
		PolicyStore:       NewPolicyStore(client),
		client:            client,
	}
}

// Close closes the underlying Ent client.
func (c *CompositeStore) Close() error {
	return c.client.Close()
}

// Ping verifies connectivity to the underlying database.
func (c *CompositeStore) Ping(ctx context.Context) error {
	drv, ok := c.client.Driver().(*entsql.Driver)
	if !ok {
		return fmt.Errorf("ent client driver does not expose a *sql.DB for ping")
	}
	return drv.DB().PingContext(ctx)
}

// Migrate runs Ent's automatic schema migration against the shared client.
func (c *CompositeStore) Migrate(ctx context.Context) error {
	return entc.AutoMigrate(ctx, c.client)
}
