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
	"encoding/json"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entharnessconfig "github.com/GoogleCloudPlatform/scion/pkg/ent/harnessconfig"
	enttemplate "github.com/GoogleCloudPlatform/scion/pkg/ent/template"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TemplateStore implements store.TemplateStore and store.HarnessConfigStore
// using Ent ORM.
//
// Both entities use (scope, scope_id) polymorphic addressing rather than FK
// edges so that global/unscoped rows port cleanly. The structured config and
// file-manifest columns are stored as raw JSON strings, matching the legacy
// SQLite layout. Subscription templates, although a sibling notification-domain
// table, are intentionally NOT handled here — they are owned by NotificationStore.
type TemplateStore struct {
	client *ent.Client
}

// NewTemplateStore creates a new Ent-backed TemplateStore.
func NewTemplateStore(client *ent.Client) *TemplateStore {
	return &TemplateStore{client: client}
}

// marshalJSONString serializes a value to a JSON string for storage in a TEXT
// column. A nil value yields an empty string. This mirrors the SQLite adapter's
// marshalJSON helper so both backends round-trip identically.
func marshalJSONString(v interface{}) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}

// unmarshalJSONString deserializes a JSON string into v. An empty string is a
// no-op, leaving v at its zero value.
func unmarshalJSONString[T any](data string, v *T) {
	if data == "" {
		return
	}
	_ = json.Unmarshal([]byte(data), v)
}

// =============================================================================
// Template operations
// =============================================================================

// entTemplateRowToStore converts an Ent Template entity to a store.Template model.
func entTemplateRowToStore(e *ent.Template) *store.Template {
	t := &store.Template{
		ID:                   e.ID.String(),
		Name:                 e.Name,
		Slug:                 e.Slug,
		DisplayName:          e.DisplayName,
		Description:          e.Description,
		Harness:              e.Harness,
		DefaultHarnessConfig: e.DefaultHarnessConfig,
		Image:                e.Image,
		ContentHash:          e.ContentHash,
		Scope:                e.Scope,
		ScopeID:              e.ScopeID,
		ProjectID:            e.ProjectID,
		StorageURI:           e.StorageURI,
		StorageBucket:        e.StorageBucket,
		StoragePath:          e.StoragePath,
		BaseTemplate:         e.BaseTemplate,
		SourceURL:            e.SourceURL,
		Status:               string(e.Status),
		OwnerID:              e.OwnerID,
		CreatedBy:            e.CreatedBy,
		UpdatedBy:            e.UpdatedBy,
		Visibility:           e.Visibility,
		Created:              e.Created,
		Updated:              e.Updated,
	}
	unmarshalJSONString(e.Config, &t.Config)
	unmarshalJSONString(e.Files, &t.Files)
	return t
}

// CreateTemplate creates a new template record.
func (s *TemplateStore) CreateTemplate(ctx context.Context, template *store.Template) error {
	uid, err := parseUUID(template.ID)
	if err != nil {
		return err
	}

	now := time.Now()
	template.Created = now
	template.Updated = now

	if template.Status == "" {
		template.Status = store.TemplateStatusActive
	}

	create := s.client.Template.Create().
		SetID(uid).
		SetName(template.Name).
		SetSlug(template.Slug).
		SetDisplayName(template.DisplayName).
		SetDescription(template.Description).
		SetHarness(template.Harness).
		SetDefaultHarnessConfig(template.DefaultHarnessConfig).
		SetImage(template.Image).
		SetConfig(marshalJSONString(template.Config)).
		SetContentHash(template.ContentHash).
		SetScope(template.Scope).
		SetScopeID(template.ScopeID).
		SetProjectID(template.ProjectID).
		SetStorageURI(template.StorageURI).
		SetStorageBucket(template.StorageBucket).
		SetStoragePath(template.StoragePath).
		SetFiles(marshalJSONString(template.Files)).
		SetBaseTemplate(template.BaseTemplate).
		SetSourceURL(template.SourceURL).
		SetStatus(enttemplate.Status(template.Status)).
		SetOwnerID(template.OwnerID).
		SetCreatedBy(template.CreatedBy).
		SetUpdatedBy(template.UpdatedBy).
		SetVisibility(template.Visibility).
		SetCreated(template.Created).
		SetUpdated(template.Updated)

	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetTemplate retrieves a template by ID.
func (s *TemplateStore) GetTemplate(ctx context.Context, id string) (*store.Template, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.Template.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entTemplateRowToStore(e), nil
}

// GetTemplateBySlug retrieves a template by its slug and scope. For the project
// scope it also matches the legacy project_id column for backwards compatibility.
func (s *TemplateStore) GetTemplateBySlug(ctx context.Context, slug, scope, scopeID string) (*store.Template, error) {
	query := s.client.Template.Query().
		Where(
			enttemplate.SlugEQ(slug),
			enttemplate.ScopeEQ(scope),
		)

	switch {
	case scope == store.TemplateScopeProject && scopeID != "":
		query.Where(enttemplate.Or(
			enttemplate.ScopeIDEQ(scopeID),
			enttemplate.ProjectIDEQ(scopeID),
		))
	case scope == store.TemplateScopeUser && scopeID != "":
		query.Where(enttemplate.ScopeIDEQ(scopeID))
	}

	e, err := query.First(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entTemplateRowToStore(e), nil
}

// UpdateTemplate updates an existing template.
func (s *TemplateStore) UpdateTemplate(ctx context.Context, template *store.Template) error {
	uid, err := parseUUID(template.ID)
	if err != nil {
		return err
	}

	template.Updated = time.Now()

	_, err = s.client.Template.UpdateOneID(uid).
		SetName(template.Name).
		SetSlug(template.Slug).
		SetDisplayName(template.DisplayName).
		SetDescription(template.Description).
		SetHarness(template.Harness).
		SetDefaultHarnessConfig(template.DefaultHarnessConfig).
		SetImage(template.Image).
		SetConfig(marshalJSONString(template.Config)).
		SetContentHash(template.ContentHash).
		SetScope(template.Scope).
		SetScopeID(template.ScopeID).
		SetProjectID(template.ProjectID).
		SetStorageURI(template.StorageURI).
		SetStorageBucket(template.StorageBucket).
		SetStoragePath(template.StoragePath).
		SetFiles(marshalJSONString(template.Files)).
		SetBaseTemplate(template.BaseTemplate).
		SetSourceURL(template.SourceURL).
		SetStatus(enttemplate.Status(template.Status)).
		SetOwnerID(template.OwnerID).
		SetUpdatedBy(template.UpdatedBy).
		SetVisibility(template.Visibility).
		SetUpdated(template.Updated).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteTemplate removes a template by ID.
func (s *TemplateStore) DeleteTemplate(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.Template.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteTemplatesByScope removes all templates for a given scope, returning the
// number of deleted records.
func (s *TemplateStore) DeleteTemplatesByScope(ctx context.Context, scope, scopeID string) (int, error) {
	n, err := s.client.Template.Delete().
		Where(
			enttemplate.ScopeEQ(scope),
			enttemplate.ScopeIDEQ(scopeID),
		).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ListTemplates returns templates matching the filter criteria.
func (s *TemplateStore) ListTemplates(ctx context.Context, filter store.TemplateFilter, opts store.ListOptions) (*store.ListResult[store.Template], error) {
	query := s.client.Template.Query()

	if filter.Name != "" {
		query.Where(enttemplate.Or(
			enttemplate.NameEQ(filter.Name),
			enttemplate.SlugEQ(filter.Name),
		))
	}
	if filter.Scope != "" {
		query.Where(enttemplate.ScopeEQ(filter.Scope))
	}
	switch {
	case filter.ScopeID != "":
		query.Where(enttemplate.Or(
			enttemplate.ScopeIDEQ(filter.ScopeID),
			enttemplate.ProjectIDEQ(filter.ScopeID),
		))
	case filter.ProjectID != "" && filter.Scope == "":
		// Project-without-scope: return global plus this project's templates.
		query.Where(enttemplate.Or(
			enttemplate.ScopeEQ(store.TemplateScopeGlobal),
			enttemplate.And(
				enttemplate.ScopeEQ(store.TemplateScopeProject),
				enttemplate.Or(
					enttemplate.ScopeIDEQ(filter.ProjectID),
					enttemplate.ProjectIDEQ(filter.ProjectID),
				),
			),
		))
	case filter.ProjectID != "":
		query.Where(enttemplate.Or(
			enttemplate.ScopeIDEQ(filter.ProjectID),
			enttemplate.ProjectIDEQ(filter.ProjectID),
		))
	}
	if filter.Harness != "" {
		query.Where(enttemplate.HarnessEQ(filter.Harness))
	}
	if filter.OwnerID != "" {
		query.Where(enttemplate.OwnerIDEQ(filter.OwnerID))
	}
	if filter.Status != "" {
		query.Where(enttemplate.StatusEQ(enttemplate.Status(filter.Status)))
	}
	if filter.Search != "" {
		query.Where(enttemplate.Or(
			enttemplate.NameContainsFold(filter.Search),
			enttemplate.DescriptionContainsFold(filter.Search),
		))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	rows, err := query.
		Order(enttemplate.ByCreated(entsql.OrderDesc())).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.Template, 0, len(rows))
	for _, e := range rows {
		items = append(items, *entTemplateRowToStore(e))
	}

	return &store.ListResult[store.Template]{
		Items:      items,
		TotalCount: totalCount,
	}, nil
}

// =============================================================================
// HarnessConfig operations
// =============================================================================

// entHarnessConfigToStore converts an Ent HarnessConfig entity to a
// store.HarnessConfig model.
func entHarnessConfigToStore(e *ent.HarnessConfig) *store.HarnessConfig {
	hc := &store.HarnessConfig{
		ID:                   e.ID.String(),
		Name:                 e.Name,
		Slug:                 e.Slug,
		DisplayName:          e.DisplayName,
		Description:          e.Description,
		Harness:              e.Harness,
		ContentHash:          e.ContentHash,
		Scope:                e.Scope,
		ScopeID:              e.ScopeID,
		StorageURI:           e.StorageURI,
		StorageBucket:        e.StorageBucket,
		StoragePath:          e.StoragePath,
		SourceURL:            e.SourceURL,
		Status:               string(e.Status),
		ImageStatus:          string(e.ImageStatus),
		ImageStatusCheckedAt: e.ImageStatusCheckedAt,
		OwnerID:              e.OwnerID,
		CreatedBy:            e.CreatedBy,
		UpdatedBy:            e.UpdatedBy,
		Visibility:           e.Visibility,
		Created:              e.Created,
		Updated:              e.Updated,
	}
	unmarshalJSONString(e.Config, &hc.Config)
	unmarshalJSONString(e.Files, &hc.Files)
	return hc
}

// CreateHarnessConfig creates a new harness config record.
func (s *TemplateStore) CreateHarnessConfig(ctx context.Context, hc *store.HarnessConfig) error {
	uid, err := parseUUID(hc.ID)
	if err != nil {
		return err
	}

	now := time.Now()
	hc.Created = now
	hc.Updated = now

	if hc.Status == "" {
		hc.Status = store.HarnessConfigStatusActive
	}
	if hc.ImageStatus == "" {
		hc.ImageStatus = store.HarnessConfigImageStatusUnknown
	}

	create := s.client.HarnessConfig.Create().
		SetID(uid).
		SetName(hc.Name).
		SetSlug(hc.Slug).
		SetDisplayName(hc.DisplayName).
		SetDescription(hc.Description).
		SetHarness(hc.Harness).
		SetConfig(marshalJSONString(hc.Config)).
		SetContentHash(hc.ContentHash).
		SetScope(hc.Scope).
		SetScopeID(hc.ScopeID).
		SetStorageURI(hc.StorageURI).
		SetStorageBucket(hc.StorageBucket).
		SetStoragePath(hc.StoragePath).
		SetFiles(marshalJSONString(hc.Files)).
		SetSourceURL(hc.SourceURL).
		SetStatus(entharnessconfig.Status(hc.Status)).
		SetImageStatus(entharnessconfig.ImageStatus(hc.ImageStatus)).
		SetOwnerID(hc.OwnerID).
		SetCreatedBy(hc.CreatedBy).
		SetUpdatedBy(hc.UpdatedBy).
		SetVisibility(hc.Visibility).
		SetCreated(hc.Created).
		SetUpdated(hc.Updated)

	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetHarnessConfig retrieves a harness config by ID.
func (s *TemplateStore) GetHarnessConfig(ctx context.Context, id string) (*store.HarnessConfig, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.HarnessConfig.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entHarnessConfigToStore(e), nil
}

// GetHarnessConfigBySlug retrieves a harness config by its slug and scope.
func (s *TemplateStore) GetHarnessConfigBySlug(ctx context.Context, slug, scope, scopeID string) (*store.HarnessConfig, error) {
	query := s.client.HarnessConfig.Query().
		Where(
			entharnessconfig.SlugEQ(slug),
			entharnessconfig.ScopeEQ(scope),
		)
	if scopeID != "" {
		query.Where(entharnessconfig.ScopeIDEQ(scopeID))
	}

	e, err := query.First(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entHarnessConfigToStore(e), nil
}

// UpdateHarnessConfig updates an existing harness config.
func (s *TemplateStore) UpdateHarnessConfig(ctx context.Context, hc *store.HarnessConfig) error {
	uid, err := parseUUID(hc.ID)
	if err != nil {
		return err
	}

	hc.Updated = time.Now()
	if hc.ImageStatus == "" {
		hc.ImageStatus = store.HarnessConfigImageStatusUnknown
	}

	update := s.client.HarnessConfig.UpdateOneID(uid).
		SetName(hc.Name).
		SetSlug(hc.Slug).
		SetDisplayName(hc.DisplayName).
		SetDescription(hc.Description).
		SetHarness(hc.Harness).
		SetConfig(marshalJSONString(hc.Config)).
		SetContentHash(hc.ContentHash).
		SetScope(hc.Scope).
		SetScopeID(hc.ScopeID).
		SetStorageURI(hc.StorageURI).
		SetStorageBucket(hc.StorageBucket).
		SetStoragePath(hc.StoragePath).
		SetFiles(marshalJSONString(hc.Files)).
		SetSourceURL(hc.SourceURL).
		SetStatus(entharnessconfig.Status(hc.Status)).
		SetImageStatus(entharnessconfig.ImageStatus(hc.ImageStatus)).
		SetOwnerID(hc.OwnerID).
		SetUpdatedBy(hc.UpdatedBy).
		SetVisibility(hc.Visibility).
		SetUpdated(hc.Updated)
	if hc.ImageStatusCheckedAt != nil {
		update.SetImageStatusCheckedAt(*hc.ImageStatusCheckedAt)
	}
	_, err = update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// UpdateHarnessConfigImageStatus updates only the image_status and
// image_status_checked_at columns for a harness config.
func (s *TemplateStore) UpdateHarnessConfigImageStatus(ctx context.Context, id, status string, checkedAt time.Time) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	existing, err := s.client.HarnessConfig.Get(ctx, uid)
	if err != nil {
		return mapError(err)
	}
	_, err = s.client.HarnessConfig.UpdateOneID(uid).
		SetImageStatus(entharnessconfig.ImageStatus(status)).
		SetImageStatusCheckedAt(checkedAt).
		SetUpdated(existing.Updated).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteHarnessConfig removes a harness config by ID.
func (s *TemplateStore) DeleteHarnessConfig(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.HarnessConfig.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteHarnessConfigsByScope removes all harness configs for a given scope,
// returning the number of deleted records.
func (s *TemplateStore) DeleteHarnessConfigsByScope(ctx context.Context, scope, scopeID string) (int, error) {
	n, err := s.client.HarnessConfig.Delete().
		Where(
			entharnessconfig.ScopeEQ(scope),
			entharnessconfig.ScopeIDEQ(scopeID),
		).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ListHarnessConfigs returns harness configs matching the filter criteria.
func (s *TemplateStore) ListHarnessConfigs(ctx context.Context, filter store.HarnessConfigFilter, opts store.ListOptions) (*store.ListResult[store.HarnessConfig], error) {
	query := s.client.HarnessConfig.Query()

	if filter.Name != "" {
		query.Where(entharnessconfig.Or(
			entharnessconfig.NameEQ(filter.Name),
			entharnessconfig.SlugEQ(filter.Name),
		))
	}
	if filter.Scope != "" {
		query.Where(entharnessconfig.ScopeEQ(filter.Scope))
	}
	switch {
	case filter.ScopeID != "":
		query.Where(entharnessconfig.ScopeIDEQ(filter.ScopeID))
	case filter.ProjectID != "" && filter.Scope == store.HarnessConfigScopeProject:
		query.Where(entharnessconfig.ScopeIDEQ(filter.ProjectID))
	case filter.ProjectID != "" && filter.Scope == "":
		// Project-without-scope: return global plus this project's configs.
		query.Where(entharnessconfig.Or(
			entharnessconfig.ScopeEQ(store.HarnessConfigScopeGlobal),
			entharnessconfig.And(
				entharnessconfig.ScopeEQ(store.HarnessConfigScopeProject),
				entharnessconfig.ScopeIDEQ(filter.ProjectID),
			),
		))
	}
	if filter.Harness != "" {
		query.Where(entharnessconfig.HarnessEQ(filter.Harness))
	}
	if filter.OwnerID != "" {
		query.Where(entharnessconfig.OwnerIDEQ(filter.OwnerID))
	}
	if filter.Status != "" {
		query.Where(entharnessconfig.StatusEQ(entharnessconfig.Status(filter.Status)))
	}
	if filter.ImageStatus != "" {
		query.Where(entharnessconfig.ImageStatusEQ(entharnessconfig.ImageStatus(filter.ImageStatus)))
	}
	if filter.Search != "" {
		query.Where(entharnessconfig.Or(
			entharnessconfig.NameContainsFold(filter.Search),
			entharnessconfig.DescriptionContainsFold(filter.Search),
		))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	rows, err := query.
		Order(entharnessconfig.ByCreated(entsql.OrderDesc())).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.HarnessConfig, 0, len(rows))
	for _, e := range rows {
		items = append(items, *entHarnessConfigToStore(e))
	}

	return &store.ListResult[store.HarnessConfig]{
		Items:      items,
		TotalCount: totalCount,
	}, nil
}
