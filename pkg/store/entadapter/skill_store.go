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
	"fmt"
	"sort"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entskill "github.com/GoogleCloudPlatform/scion/pkg/ent/skill"
	entskillversion "github.com/GoogleCloudPlatform/scion/pkg/ent/skillversion"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/Masterminds/semver/v3"
)

// SkillStore implements store.SkillStore using Ent ORM.
type SkillStore struct {
	client *ent.Client
}

// NewSkillStore creates a new Ent-backed SkillStore.
func NewSkillStore(client *ent.Client) *SkillStore {
	return &SkillStore{client: client}
}

func entSkillToStore(e *ent.Skill) *store.Skill {
	s := &store.Skill{
		ID:            e.ID.String(),
		Name:          e.Name,
		Slug:          e.Slug,
		Description:   e.Description,
		Scope:         e.Scope,
		ScopeID:       e.ScopeID,
		StorageURI:    e.StorageURI,
		StorageBucket: e.StorageBucket,
		StoragePath:   e.StoragePath,
		Status:        string(e.Status),
		OwnerID:       e.OwnerID,
		CreatedBy:     e.CreatedBy,
		UpdatedBy:     e.UpdatedBy,
		Visibility:    e.Visibility,
		Created:       e.Created,
		Updated:       e.Updated,
	}
	if e.Tags != "" {
		_ = json.Unmarshal([]byte(e.Tags), &s.Tags)
	}
	return s
}

func entSkillVersionToStore(e *ent.SkillVersion) *store.SkillVersion {
	sv := &store.SkillVersion{
		ID:                 e.ID.String(),
		SkillID:            e.SkillID,
		Version:            e.Version,
		Status:             string(e.Status),
		ContentHash:        e.ContentHash,
		PublisherID:        e.PublisherID,
		DeprecationMessage: e.DeprecationMessage,
		ReplacementURI:     e.ReplacementURI,
		DownloadCount:      e.DownloadCount,
		Created:            e.Created,
	}
	unmarshalJSONString(e.Files, &sv.Files)
	return sv
}

func (s *SkillStore) CreateSkill(ctx context.Context, skill *store.Skill) error {
	uid, err := parseUUID(skill.ID)
	if err != nil {
		return err
	}

	now := time.Now()
	skill.Created = now
	skill.Updated = now

	if skill.Status == "" {
		skill.Status = "active"
	}

	tagsJSON := ""
	if len(skill.Tags) > 0 {
		data, _ := json.Marshal(skill.Tags)
		tagsJSON = string(data)
	}

	_, err = s.client.Skill.Create().
		SetID(uid).
		SetName(skill.Name).
		SetSlug(skill.Slug).
		SetDescription(skill.Description).
		SetTags(tagsJSON).
		SetScope(skill.Scope).
		SetScopeID(skill.ScopeID).
		SetStorageURI(skill.StorageURI).
		SetStorageBucket(skill.StorageBucket).
		SetStoragePath(skill.StoragePath).
		SetStatus(entskill.Status(skill.Status)).
		SetOwnerID(skill.OwnerID).
		SetCreatedBy(skill.CreatedBy).
		SetUpdatedBy(skill.UpdatedBy).
		SetVisibility(skill.Visibility).
		SetCreated(skill.Created).
		SetUpdated(skill.Updated).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

func (s *SkillStore) GetSkill(ctx context.Context, id string) (*store.Skill, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.Skill.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entSkillToStore(e), nil
}

func (s *SkillStore) GetSkillBySlug(ctx context.Context, slug, scope, scopeID string) (*store.Skill, error) {
	query := s.client.Skill.Query().
		Where(
			entskill.SlugEQ(slug),
			entskill.ScopeEQ(scope),
			entskill.ScopeIDEQ(scopeID),
			entskill.StatusEQ(entskill.StatusActive),
		)

	e, err := query.First(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entSkillToStore(e), nil
}

func (s *SkillStore) UpdateSkill(ctx context.Context, skill *store.Skill) error {
	uid, err := parseUUID(skill.ID)
	if err != nil {
		return err
	}

	skill.Updated = time.Now()

	tagsJSON := ""
	if len(skill.Tags) > 0 {
		data, _ := json.Marshal(skill.Tags)
		tagsJSON = string(data)
	}

	_, err = s.client.Skill.UpdateOneID(uid).
		SetName(skill.Name).
		SetSlug(skill.Slug).
		SetDescription(skill.Description).
		SetTags(tagsJSON).
		SetScope(skill.Scope).
		SetScopeID(skill.ScopeID).
		SetStorageURI(skill.StorageURI).
		SetStorageBucket(skill.StorageBucket).
		SetStoragePath(skill.StoragePath).
		SetStatus(entskill.Status(skill.Status)).
		SetOwnerID(skill.OwnerID).
		SetUpdatedBy(skill.UpdatedBy).
		SetVisibility(skill.Visibility).
		SetUpdated(skill.Updated).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

func (s *SkillStore) DeleteSkill(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	_, err = s.client.Skill.UpdateOneID(uid).
		SetStatus(entskill.StatusArchived).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

func (s *SkillStore) ListSkills(ctx context.Context, filter store.SkillFilter, opts store.ListOptions) (*store.ListResult[store.Skill], error) {
	query := s.client.Skill.Query()

	if filter.Name != "" {
		query.Where(entskill.Or(
			entskill.NameEQ(filter.Name),
			entskill.SlugEQ(filter.Name),
		))
	}
	if filter.Scope != "" {
		query.Where(entskill.ScopeEQ(filter.Scope))
	}
	if filter.ScopeID != "" {
		query.Where(entskill.ScopeIDEQ(filter.ScopeID))
	}
	if filter.OwnerID != "" {
		query.Where(entskill.OwnerIDEQ(filter.OwnerID))
	}
	if filter.Status != "" {
		query.Where(entskill.StatusEQ(entskill.Status(filter.Status)))
	}
	if filter.Search != "" {
		query.Where(entskill.Or(
			entskill.NameContainsFold(filter.Search),
			entskill.DescriptionContainsFold(filter.Search),
			entskill.TagsContainsFold(filter.Search),
		))
	}
	if len(filter.Tags) > 0 {
		for _, tag := range filter.Tags {
			query.Where(entskill.TagsContainsFold(`"` + tag + `"`))
		}
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
		Order(entskill.ByCreated(entsql.OrderDesc())).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.Skill, 0, len(rows))
	for _, e := range rows {
		items = append(items, *entSkillToStore(e))
	}

	return &store.ListResult[store.Skill]{
		Items:      items,
		TotalCount: totalCount,
	}, nil
}

// Version operations

func (s *SkillStore) CreateSkillVersion(ctx context.Context, version *store.SkillVersion) error {
	uid, err := parseUUID(version.ID)
	if err != nil {
		return err
	}

	version.Created = time.Now()

	if version.Status == "" {
		version.Status = store.SkillVersionStatusDraft
	}

	_, err = s.client.SkillVersion.Create().
		SetID(uid).
		SetSkillID(version.SkillID).
		SetVersion(version.Version).
		SetStatus(entskillversion.Status(version.Status)).
		SetContentHash(version.ContentHash).
		SetFiles(marshalJSONString(version.Files)).
		SetPublisherID(version.PublisherID).
		SetDeprecationMessage(version.DeprecationMessage).
		SetReplacementURI(version.ReplacementURI).
		SetCreated(version.Created).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

func (s *SkillStore) GetSkillVersion(ctx context.Context, id string) (*store.SkillVersion, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.SkillVersion.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entSkillVersionToStore(e), nil
}

func (s *SkillStore) GetSkillVersionByNumber(ctx context.Context, skillID, version string) (*store.SkillVersion, error) {
	e, err := s.client.SkillVersion.Query().
		Where(
			entskillversion.SkillIDEQ(skillID),
			entskillversion.VersionEQ(version),
		).
		First(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entSkillVersionToStore(e), nil
}

func (s *SkillStore) ListSkillVersions(ctx context.Context, skillID string, opts store.ListOptions) (*store.ListResult[store.SkillVersion], error) {
	query := s.client.SkillVersion.Query().
		Where(entskillversion.SkillIDEQ(skillID))

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	rows, err := query.
		Order(entskillversion.ByCreated(entsql.OrderDesc())).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.SkillVersion, 0, len(rows))
	for _, e := range rows {
		items = append(items, *entSkillVersionToStore(e))
	}

	return &store.ListResult[store.SkillVersion]{
		Items:      items,
		TotalCount: totalCount,
	}, nil
}

func (s *SkillStore) UpdateSkillVersion(ctx context.Context, version *store.SkillVersion) error {
	uid, err := parseUUID(version.ID)
	if err != nil {
		return err
	}

	_, err = s.client.SkillVersion.UpdateOneID(uid).
		SetStatus(entskillversion.Status(version.Status)).
		SetContentHash(version.ContentHash).
		SetFiles(marshalJSONString(version.Files)).
		SetPublisherID(version.PublisherID).
		SetDeprecationMessage(version.DeprecationMessage).
		SetReplacementURI(version.ReplacementURI).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteSkillVersion hard-deletes a skill version record.
// Only draft versions may be deleted; attempting to delete a published,
// deprecated, or archived version returns an error.
func (s *SkillStore) DeleteSkillVersion(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	// Fetch the version first to check its status.
	e, err := s.client.SkillVersion.Get(ctx, uid)
	if err != nil {
		return mapError(err)
	}

	if e.Status != entskillversion.StatusDraft {
		return fmt.Errorf("cannot delete skill version in %q status: only draft versions can be deleted", e.Status)
	}

	if err := s.client.SkillVersion.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// ResolveSkillVersion resolves a version constraint to a specific published version.
func (s *SkillStore) ResolveSkillVersion(ctx context.Context, skillID, constraint string) (*store.SkillVersion, error) {
	// Content-addressed lookup
	if len(constraint) > 7 && constraint[:7] == "sha256:" {
		e, err := s.client.SkillVersion.Query().
			Where(
				entskillversion.SkillIDEQ(skillID),
				entskillversion.ContentHashEQ(constraint),
			).
			First(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		return entSkillVersionToStore(e), nil
	}

	// Fetch all published and deprecated versions for this skill
	rows, err := s.client.SkillVersion.Query().
		Where(
			entskillversion.SkillIDEQ(skillID),
			entskillversion.StatusIn(entskillversion.StatusPublished, entskillversion.StatusDeprecated),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, store.ErrNotFound
	}

	// Parse all versions
	type parsed struct {
		sv  *ent.SkillVersion
		ver *semver.Version
	}
	var versions []parsed
	for _, row := range rows {
		v, err := semver.NewVersion(row.Version)
		if err != nil {
			continue
		}
		versions = append(versions, parsed{sv: row, ver: v})
	}
	if len(versions) == 0 {
		return nil, store.ErrNotFound
	}

	// Sort descending
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].ver.GreaterThan(versions[j].ver)
	})

	if constraint == "latest" || constraint == "" {
		// Prefer published over deprecated for latest
		for _, v := range versions {
			if v.ver.Prerelease() == "" && v.sv.Status == entskillversion.StatusPublished {
				return entSkillVersionToStore(v.sv), nil
			}
		}
		// Fallback: if all non-prerelease versions are deprecated, return highest
		for _, v := range versions {
			if v.ver.Prerelease() == "" {
				return entSkillVersionToStore(v.sv), nil
			}
		}
		return nil, store.ErrNotFound
	}

	// Exact match — return regardless of status (pinned consumers get what they asked for)
	exactVer, err := semver.NewVersion(constraint)
	if err == nil {
		for _, v := range versions {
			if v.ver.Equal(exactVer) {
				return entSkillVersionToStore(v.sv), nil
			}
		}
		return nil, store.ErrNotFound
	}

	// Parse as constraint (^1.0, ~1.2, >=1.0 <2.0, etc.)
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return nil, fmt.Errorf("invalid version constraint %q: %w", constraint, err)
	}

	// Prefer published over deprecated for constraint-based resolution
	for _, v := range versions {
		if v.ver.Prerelease() == "" && c.Check(v.ver) && v.sv.Status == entskillversion.StatusPublished {
			return entSkillVersionToStore(v.sv), nil
		}
	}
	// Fallback: return highest matching deprecated version
	for _, v := range versions {
		if v.ver.Prerelease() == "" && c.Check(v.ver) {
			return entSkillVersionToStore(v.sv), nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *SkillStore) IncrementSkillVersionDownloadCount(ctx context.Context, versionID string) error {
	uid, err := parseUUID(versionID)
	if err != nil {
		return err
	}
	return s.client.SkillVersion.UpdateOneID(uid).AddDownloadCount(1).Exec(ctx)
}
