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

package config

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/integrationconfig"
)

// PostgresConfigProvider implements IntegrationConfigProvider backed by the
// integration_configs Ent table. Used for Mode 3 (HA) integrations where
// config is stored in Postgres rather than local YAML files.
type PostgresConfigProvider struct {
	client      *ent.Client
	integration string
	updatedBy   string
}

// NewPostgresConfigProvider creates a new PostgresConfigProvider for the given
// integration name (e.g. "discord", "telegram").
func NewPostgresConfigProvider(client *ent.Client, integration string) *PostgresConfigProvider {
	return &PostgresConfigProvider{
		client:      client,
		integration: integration,
	}
}

// SetUpdatedBy sets the user ID that will be recorded as the updater on the
// next Save call.
func (p *PostgresConfigProvider) SetUpdatedBy(userID string) {
	p.updatedBy = userID
}

// Load queries the integration_configs table for the named integration and
// returns the config JSON as a map[string]string.
func (p *PostgresConfigProvider) Load(ctx context.Context) (map[string]string, error) {
	row, err := p.client.IntegrationConfig.
		Query().
		Where(integrationconfig.IntegrationEQ(p.integration)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return make(map[string]string), nil
		}
		return nil, fmt.Errorf("load integration config for %s: %w", p.integration, err)
	}

	var result map[string]string
	if err := json.Unmarshal([]byte(row.Config), &result); err != nil {
		return nil, fmt.Errorf("unmarshal integration config for %s: %w", p.integration, err)
	}
	return result, nil
}

// Save upserts the config map as JSON into the integration_configs table.
func (p *PostgresConfigProvider) Save(ctx context.Context, config map[string]string) error {
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal integration config for %s: %w", p.integration, err)
	}

	create := p.client.IntegrationConfig.
		Create().
		SetIntegration(p.integration).
		SetConfig(string(data))

	if p.updatedBy != "" {
		create = create.SetUpdatedBy(p.updatedBy)
	}

	return create.
		OnConflict(
			sql.ConflictColumns(integrationconfig.FieldIntegration),
		).
		Update(func(u *ent.IntegrationConfigUpsert) {
			u.SetConfig(string(data))
			u.SetUpdateTime(time.Now())
			if p.updatedBy != "" {
				u.SetUpdatedBy(p.updatedBy)
			}
		}).
		Exec(ctx)
}
