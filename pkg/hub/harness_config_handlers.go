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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/imagecheck"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// isValidHarnessConfigScope reports whether scope is an accepted harness-config
// scope for create and clone. An empty scope is valid: create defaults it to
// "global" and clone defaults it to the source harness config's scope. Any
// other value — including removed legacy scope names — is rejected outright
// rather than stored as-is. A PUT update does not take its scope from the
// request at all: it keeps the stored record's scope, scope ID and owner, so
// this validation does not apply there.
func isValidHarnessConfigScope(scope string) bool {
	switch scope {
	case "", store.HarnessConfigScopeGlobal, store.HarnessConfigScopeProject, store.HarnessConfigScopeUser:
		return true
	default:
		return false
	}
}

type imageManager interface {
	imagecheck.LocalImageExister
	PullImage(ctx context.Context, image string) error
	RemoveImage(ctx context.Context, image string) error
}

var nodeBoundProfileTypes = map[string]bool{
	"docker": true,
	"podman": true,
	"apple":  true,
}

func isNodeBoundBroker(broker *store.RuntimeBroker) bool {
	for _, p := range broker.Profiles {
		if nodeBoundProfileTypes[p.Type] {
			return true
		}
	}
	return false
}

type AggregatedImageStatusResponse struct {
	Image        string               `json:"image"`
	Registry     *RegistryImageStatus `json:"registry"`
	Brokers      []BrokerImageEntry   `json:"brokers"`
	ProxyBrokers []ProxyBrokerEntry   `json:"proxy_brokers,omitempty"`
}

type RegistryImageStatus struct {
	Image     string    `json:"image"`
	Exists    bool      `json:"exists"`
	Hash      string    `json:"hash,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

type BrokerImageEntry struct {
	BrokerID         string                  `json:"broker_id"`
	BrokerName       string                  `json:"broker_name"`
	Reachable        bool                    `json:"reachable"`
	Unsupported      bool                    `json:"unsupported,omitempty"`
	LocalShort       *BrokerImageEntityState `json:"local_short,omitempty"`
	LocalLong        *BrokerImageEntityState `json:"local_long,omitempty"`
	NewerInRegistry  bool                    `json:"newer_in_registry,omitempty"`
	ResolvedImage    string                  `json:"resolved_image,omitempty"`
	ResolutionSource string                  `json:"resolution_source,omitempty"`
}

type ProxyBrokerEntry struct {
	BrokerID   string `json:"broker_id"`
	BrokerName string `json:"broker_name"`
	Runtime    string `json:"runtime"`
}

func (s *Server) checkRegistryImage(ctx context.Context, longImage string) RegistryImageStatus {
	now := time.Now()
	if longImage == "" {
		return RegistryImageStatus{CheckedAt: now}
	}
	if imagecheck.IsBareImageName(longImage) {
		return RegistryImageStatus{
			Image:     longImage,
			CheckedAt: now,
		}
	}
	result := s.imageChecker.CheckRemoteOnly(ctx, longImage)
	return RegistryImageStatus{
		Image:     longImage,
		Exists:    result.Status == "valid",
		Hash:      result.Hash,
		CheckedAt: result.CheckedAt,
	}
}

// CreateHarnessConfigRequest is the request body for creating a harness config.
type CreateHarnessConfigRequest struct {
	Name        string              `json:"name"`
	Slug        string              `json:"slug,omitempty"`
	DisplayName string              `json:"displayName,omitempty"`
	Description string              `json:"description,omitempty"`
	Harness     string              `json:"harness"`
	Scope       string              `json:"scope"`
	ScopeID     string              `json:"scopeId,omitempty"`
	Files       []FileUploadRequest `json:"files,omitempty"`

	// Config is not settable (ptone/scion#4219): a harness-config's
	// config is derived from its config.yaml. It is decoded only so a body
	// that carries it is refused with 400 harness_config_config_in_file.
	Config json.RawMessage `json:"config,omitempty"`
}

// Error code for a harness-config create that carries config
// (ptone/scion#4219).
const (
	ErrCodeHarnessConfigConfigInFile = "harness_config_config_in_file"
	harnessConfigConfigInFileMessage = "harness config config is not settable through the API; set it in config.yaml"
)

// UpdateHarnessConfigRequest is the request body of a harness-config PUT. A
// PUT replaces only the metadata (name, slug, display name, description);
// every other field keeps its stored value (ptone/scion#4219). Unlike a
// template PUT, a body that carries other fields (config, files, ...) is not
// refused: a harness-config GET returns the config derived from config.yaml,
// so a client that PUTs back what it read must keep working. Those fields
// are ignored.
type UpdateHarnessConfigRequest struct {
	Name        string `json:"name"`
	Slug        string `json:"slug,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
}

// CreateHarnessConfigResponse is the response for harness config creation.
type CreateHarnessConfigResponse struct {
	HarnessConfig *store.HarnessConfig `json:"harnessConfig"`
	UploadURLs    []UploadURLInfo      `json:"uploadUrls,omitempty"`
	ManifestURL   string               `json:"manifestUrl,omitempty"`
	// UploadID names the staging directory the upload URLs write to. A
	// client may send it back in the finalize request (ptone/scion#4219).
	UploadID string `json:"uploadId,omitempty"`
}

// ListHarnessConfigsResponse is the response for listing harness configs.
type ListHarnessConfigsResponse struct {
	HarnessConfigs []HarnessConfigWithCapabilities `json:"harnessConfigs"`
	NextCursor     string                          `json:"nextCursor,omitempty"`
	TotalCount     int                             `json:"totalCount"`
	// TotalCountApproximate marks TotalCount as a lower bound rather than an
	// exact count (ptone/scion#1916 follow-up, C3) — see ListTemplatesResponse.
	TotalCountApproximate bool          `json:"totalCountApproximate,omitempty"`
	Capabilities          *Capabilities `json:"_capabilities,omitempty"`
}

// HarnessConfigManifest is the manifest of uploaded harness config files.
type HarnessConfigManifest struct {
	Version string               `json:"version"`
	Harness string               `json:"harness,omitempty"`
	Files   []store.TemplateFile `json:"files"`
}

// handleHarnessConfigs handles the /api/v1/harness-configs endpoint.
func (s *Server) handleHarnessConfigs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listHarnessConfigs(w, r)
	case http.MethodPost:
		s.createHarnessConfig(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// listHarnessConfigs lists harness configs with filtering.
func (s *Server) listHarnessConfigs(w http.ResponseWriter, r *http.Request) {
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()
	query := r.URL.Query()

	filter := store.HarnessConfigFilter{
		Name:        query.Get("name"),
		Scope:       query.Get("scope"),
		ScopeID:     query.Get("scopeId"),
		ProjectID:   query.Get("projectId"),
		Harness:     query.Get("harness"),
		Status:      query.Get("status"),
		ImageStatus: query.Get("image_status"),
		Search:      query.Get("search"),
	}

	// Default to active harness configs only
	if filter.Status == "" {
		filter.Status = store.HarnessConfigStatusActive
	}

	identity := GetIdentityFromContext(ctx)
	limit, err := parseAuthorizedListLimit(query.Get("limit"))
	if err != nil {
		BadRequest(w, err.Error())
		return
	}
	cursor := query.Get("cursor")
	cursorBinding := scopedCursorBinding("harness-configs", filter, identity)
	wideAccess := s.hasCatalogWideListAccess(ctx, identity, "harness_config", "harness_config.list")
	authorizeEach := identity != nil && !wideAccess
	result, err := listAuthorizedOrAll(
		ctx, identity, cursor, limit, cursorBinding, s.listCursorSealer, authorizeEach,
		func(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.HarnessConfig], error) {
			return s.store.ListHarnessConfigs(ctx, filter, opts)
		},
		harnessConfigResource,
		func(h *store.HarnessConfig) string { return authorizedListCursor(h.Created, h.ID, cursorBinding) },
		s.catalogListReadBatch(identity),
	)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	configs, nextCursor, totalCount, totalApprox := result.Items, result.NextCursor, result.TotalCount, result.TotalCountApproximate
	items := make([]HarnessConfigWithCapabilities, 0, len(configs))
	if identity == nil {
		for i := range configs {
			items = append(items, HarnessConfigWithCapabilities{HarnessConfig: configs[i]})
		}
	} else {
		resources := make([]Resource, len(configs))
		for i := range configs {
			resources[i] = harnessConfigResource(&configs[i])
		}
		for i, cap := range s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "harness_config") {
			items = append(items, HarnessConfigWithCapabilities{HarnessConfig: configs[i], Cap: cap})
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "harness_config")
	}
	writeJSON(w, http.StatusOK, ListHarnessConfigsResponse{
		HarnessConfigs:        items,
		NextCursor:            nextCursor,
		TotalCount:            totalCount,
		TotalCountApproximate: totalApprox,
		Capabilities:          scopeCap,
	})
}

// createHarnessConfig creates a harness config with optional file upload URLs.
func (s *Server) createHarnessConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateHarnessConfigRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if req.Harness == "" {
		ValidationError(w, "harness is required", nil)
		return
	}
	if carriesJSONValue(req.Config) {
		writeError(w, http.StatusBadRequest, ErrCodeHarnessConfigConfigInFile, harnessConfigConfigInFileMessage, nil)
		return
	}
	if !isValidHarnessConfigScope(req.Scope) {
		ValidationError(w, fmt.Sprintf("invalid scope %q: must be \"global\", \"project\" or \"user\"", req.Scope), nil)
		return
	}

	// SECURITY-GATE: require harness_config.create permission before any mutation.
	// Scope-aware: project-scoped requests authorize against the project parent
	// so that project-level role bindings (owner/admin/member) grant access.
	createScope := req.Scope
	if createScope == "" {
		createScope = store.HarnessConfigScopeGlobal
	}
	// A user-scope harness config belongs to the caller: its scope and
	// owner are the caller for every caller, never a request field.
	var owner UserIdentity
	if createScope == store.HarnessConfigScopeUser {
		owner = GetUserIdentityFromContext(ctx)
		if owner == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "User authentication required for user-scoped harness configs", nil)
			return
		}
		if !requireProfileWriter(w, r) {
			return
		}
		req.ScopeID = owner.ID()
	}
	if !s.authorize(w, r, harnessConfigScopeResource(createScope, req.ScopeID), ActionCreate) {
		return
	}

	if err := validateUploadFilePaths(req.Files); err != nil {
		if !writeInvalidFilePathError(w, err) {
			ValidationError(w, "files are invalid", nil)
		}
		return
	}

	slug := req.Slug
	if slug == "" {
		slug = req.Name
	}
	slug = api.Slugify(slug)
	if slug == "" {
		ValidationError(w, "slug is empty after normalization", nil)
		return
	}

	// The row carries the requested harness until its first commit derives
	// Harness and Config from config.yaml.
	hc := &store.HarnessConfig{
		ID:          api.NewUUID(),
		Name:        req.Name,
		Slug:        slug,
		DisplayName: req.DisplayName,
		Description: req.Description,
		Harness:     req.Harness,
		Scope:       req.Scope,
		ScopeID:     req.ScopeID,
		Status:      store.HarnessConfigStatusPending,
	}

	if hc.Scope == "" {
		hc.Scope = store.HarnessConfigScopeGlobal
	}
	if owner != nil {
		hc.OwnerID = owner.ID()
		hc.CreatedBy = owner.ID()
	}

	// If no files provided, mark as active immediately
	if len(req.Files) == 0 {
		hc.Status = store.HarnessConfigStatusActive
	}

	// New harness-configs start in the blob layout, with a row-unique
	// storage path (ptone/scion#4219).
	storagePath := s.harnessConfigBlobStoragePath(hc)
	hc.StoragePath = storagePath
	hc.Layout = store.HarnessConfigLayoutBlobs

	stor := s.GetStorage()
	if stor != nil {
		hc.StorageBucket = stor.Bucket()
		hc.StorageURI = storage.StorageURIForPath(stor.Bucket(), storagePath)
	}

	if err := s.store.CreateHarnessConfig(ctx, hc); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	response := CreateHarnessConfigResponse{
		HarnessConfig: hc,
	}

	// Generate staging upload URLs if files were specified and storage is
	// available. Finalize commits them.
	if len(req.Files) > 0 && stor != nil {
		uploadID := api.NewUUID()
		uploadURLs, err := s.generateHarnessConfigUploadURLs(ctx, stor, hc, req.Files, uploadID, requestBaseURL(r))
		if err == nil {
			response.UploadURLs = uploadURLs
			response.UploadID = uploadID
		}
	}

	writeJSON(w, http.StatusCreated, response)
}

// handleHarnessConfigByID handles individual harness config operations.
func (s *Server) handleHarnessConfigByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/harness-configs/")
	if path == "" {
		NotFound(w, "HarnessConfig")
		return
	}

	parts := strings.SplitN(path, "/", 2)
	hcID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	hc, err := s.store.GetHarnessConfig(r.Context(), hcID)
	if err != nil {
		// A genuinely missing config uses the same "HarnessConfig not found"
		// message authorizeHarnessConfigRoute writes on denial below
		// (ptone/scion#1916), so the two outcomes cannot be told apart by
		// message text either — including on the clone route, whose read of
		// its source is gated by this same fetch.
		writeStoreErr(w, err, "HarnessConfig")
		return
	}

	// SECURITY-GATE: CheckAccess — authorize this specific harness config
	// before any arm runs. The gate is above the switch so that no arm,
	// including one added later, is reachable without it.
	if !s.authorizeHarnessConfigRoute(w, r, hc, harnessConfigRouteAction(action, r.Method)) {
		return
	}
	if harnessConfigProfileWrite(hc, action, r.Method) && !requireProfileWriter(w, r) {
		return
	}

	switch action {
	case "":
		s.handleHarnessConfigCRUD(w, r, hc)
	case "upload":
		s.handleHarnessConfigUpload(w, r, hc)
	case "finalize":
		s.handleHarnessConfigFinalize(w, r, hc)
	case "download":
		s.handleHarnessConfigDownload(w, r, hc)
	case "clone":
		s.handleHarnessConfigClone(w, r, hc)
	case "validate":
		s.handleHarnessConfigValidate(w, r, hc)
	case "check-image":
		s.handleHarnessConfigCheckImage(w, r, hc)
	case "image-status":
		s.handleHarnessConfigImageStatus(w, r, hc)
	case "local-image":
		s.handleHarnessConfigDeleteLocalImage(w, r, hc)
	case "pull-image":
		s.handleHarnessConfigPullImage(w, r, hc)
	case "reimport":
		s.handleHarnessConfigReimport(w, r, hc)
	case "files":
		s.handleHarnessConfigFiles(w, r, hc, "")
	default:
		if strings.HasPrefix(action, "files/") {
			filePath := strings.TrimPrefix(action, "files/")
			s.handleHarnessConfigFiles(w, r, hc, filePath)
			return
		}
		NotFound(w, "HarnessConfig action")
	}
}

// harnessConfigRouteAction returns the baseline permission a request on
// /api/v1/harness-configs/{id}[/action] needs on the config itself.
//
// Reads need ActionRead. Writes need ActionUpdate, except for the three arms
// that already carry a stricter, scope-aware check of their own in the
// handler: delete (harness_config delete on the owning scope), reimport
// (create on the owning scope) and clone (create on the destination). For
// those the baseline is ActionRead on the config, and the handler's own
// check still applies on top.
//
// Like projectWorkspaceAction, any method that is not a plain read is
// treated as a write.
func harnessConfigRouteAction(action, method string) Action {
	switch {
	case action == "" && method == http.MethodDelete,
		action == "reimport",
		action == "clone":
		return ActionRead
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ActionRead
	default:
		return ActionUpdate
	}
}

// harnessConfigProfileWrite reports whether a request on
// /api/v1/harness-configs/{id}[/action] writes a user-scope harness config,
// a profile resource: PUT, PATCH or DELETE on the config, upload, finalize,
// reimport, or a file write. Clone writes its destination, which its handler
// checks; the image actions act on brokers, not on the config.
func harnessConfigProfileWrite(hc *store.HarnessConfig, action, method string) bool {
	if hc.Scope != store.HarnessConfigScopeUser {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	switch {
	case action == "", action == "upload", action == "finalize", action == "reimport",
		action == "files", strings.HasPrefix(action, "files/"):
		return true
	}
	return false
}

// authorizeHarnessConfigRoute applies the dispatcher gate. Runtime brokers
// read harness configs during agent creation over HMAC auth; they are not
// user principals, so the authorization kernel cannot evaluate them. They are
// admitted for reads only, and the HMAC credential is the trust basis — but
// scoped by brokerMayReadCatalogResource to the projects the broker serves
// plus the hub-wide catalog, not every project's or user's private harness
// config.
//
// A read denial reads as 404 (ptone/scion#1916), matching the skill fix: a
// harness config a caller may not read (get, download, validate, files,
// image-status, check-image — every route action harnessConfigRouteAction
// maps to ActionRead) must not be distinguishable from one that does not
// exist. Every non-read action keeps the existing 403 behavior.
//
// A global-scope config is likewise allowed outright for an agent identity,
// ahead of the ordinary authorizeRead/CheckAccess call — see the matching
// comment on authorizeTemplateReadRoute (template_handlers.go) for why this
// lives here rather than in the shared authorization kernel.
func (s *Server) authorizeHarnessConfigRoute(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig, action Action) bool {
	if action == ActionRead {
		if broker := GetBrokerIdentityFromContext(r.Context()); broker != nil {
			if s.brokerMayReadCatalogResource(r.Context(), broker, hc.Scope, hc.ScopeID) {
				return true
			}
			NotFound(w, "HarnessConfig")
			return false
		}
		if hc.Scope == store.HarnessConfigScopeGlobal {
			if agent := GetAgentIdentityFromContext(r.Context()); agent != nil {
				return true
			}
		}
		return s.authorizeRead(w, r, harnessConfigResource(hc), "HarnessConfig")
	}
	return s.authorize(w, r, harnessConfigResource(hc), action)
}

// handleHarnessConfigCRUD handles basic harness config CRUD operations.
func (s *Server) handleHarnessConfigCRUD(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	switch r.Method {
	case http.MethodGet:
		s.getHarnessConfig(w, r, hc)
	case http.MethodPut:
		s.updateHarnessConfig(w, r, hc)
	case http.MethodPatch:
		s.patchHarnessConfig(w, r, hc)
	case http.MethodDelete:
		s.deleteHarnessConfig(w, r, hc)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete)
	}
}

func (s *Server) getHarnessConfig(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()

	// Authorization (including the read-only broker exemption) is enforced
	// once, above the switch, in handleHarnessConfigByID.

	resp := HarnessConfigWithCapabilities{HarnessConfig: *hc}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, harnessConfigResource(hc))
	}

	writeJSON(w, http.StatusOK, resp)
}

// harnessConfigImage is the image the image handlers show and check: the
// built image when the build executor has set one, otherwise config.yaml's
// image (effectiveHarnessConfigImage).
func (s *Server) harnessConfigImage(hc *store.HarnessConfig) string {
	return effectiveHarnessConfigImage(hc)
}

// extractHarnessConfigEntryFromStorage downloads and parses hc's stored
// config.yaml (its blob, or its path object for a legacy row), returning
// ok=false if it can't be downloaded or parsed.
func (s *Server) extractHarnessConfigEntryFromStorage(ctx context.Context, stor storage.Storage, hc *store.HarnessConfig) (config.HarnessConfigEntry, bool) {
	data, err := s.readHarnessConfigFile(ctx, stor, hc, harnessConfigYAMLPath, maxHarnessConfigFileSize)
	if err != nil {
		return config.HarnessConfigEntry{}, false
	}
	entry, err := config.ParseHarnessConfigYAML(data)
	if err != nil {
		return config.HarnessConfigEntry{}, false
	}
	return entry, true
}

func (s *Server) updateHarnessConfig(w http.ResponseWriter, r *http.Request, existing *store.HarnessConfig) {
	ctx := r.Context()

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}

	var req UpdateHarnessConfigRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// A PUT replaces only the metadata. Everything else keeps its stored
	// value: identity, scope and ownership, the files and what is derived
	// from them (managed by the commit path), storage, lifecycle, image
	// state and provenance. The store's metadata update writes none of the
	// content columns either, so a PUT from a stale read cannot revert a
	// commit (ptone/scion#4219).
	hc := *existing
	hc.Name = req.Name
	hc.Slug = req.Slug
	hc.DisplayName = req.DisplayName
	hc.Description = req.Description
	hc.UpdatedBy = identity.ID()
	if hc.Slug == "" {
		hc.Slug = api.Slugify(hc.Name)
	}

	if err := s.store.UpdateHarnessConfig(ctx, &hc); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, hc)
}

func (s *Server) patchHarnessConfig(w http.ResponseWriter, r *http.Request, existing *store.HarnessConfig) {
	ctx := r.Context()

	if !applyResourceMetadataPatch(w, r, resourceMetadataFields{
		Name:        &existing.Name,
		Slug:        &existing.Slug,
		DisplayName: &existing.DisplayName,
		Description: &existing.Description,
	}) {
		return
	}

	if err := s.store.UpdateHarnessConfig(ctx, existing); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) deleteHarnessConfig(w http.ResponseWriter, r *http.Request, existing *store.HarnessConfig) {
	ctx := r.Context()
	query := r.URL.Query()

	deleteFiles := query.Get("deleteFiles") == "true"

	// Authorize: check source scope for ActionDelete
	switch existing.Scope {
	case store.HarnessConfigScopeGlobal:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, harnessConfigScopeResource(store.HarnessConfigScopeGlobal, ""), ActionDelete)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to delete global resources", nil)
			return
		}
	case store.HarnessConfigScopeProject:
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			if !agentIdent.HasScope(ScopeAgentCreate) {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope", nil)
				return
			}
			if existing.ScopeID != agentIdent.ProjectID() {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only manage resources within their own project", nil)
				return
			}
		} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			decision := s.authzService.CheckAccess(ctx, userIdent,
				harnessConfigScopeResource(store.HarnessConfigScopeProject, existing.ScopeID), ActionDelete)
			if !decision.Allowed {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to delete resources in this project", nil)
				return
			}
		} else {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
	case store.HarnessConfigScopeUser:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		if existing.OwnerID != userIdent.ID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You do not have permission to delete another user's harness config", nil)
			return
		}
		// The route gate checked read on the config; deleting it needs
		// harness_config:delete, so a user access token with only the read
		// selector is refused on its own record.
		if !s.authzService.CheckAccess(ctx, userIdent, harnessConfigResource(existing), ActionDelete).Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You do not have permission to delete harness configs in your user scope", nil)
			return
		}
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Delete is not supported for this resource scope", nil)
		return
	}

	if deleteFiles && existing.StoragePath != "" {
		if stor := s.GetStorage(); stor != nil {
			_ = s.deleteHarnessConfigStorage(ctx, stor, existing)
		}
	}

	if err := s.store.DeleteHarnessConfig(ctx, existing.ID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleHarnessConfigUpload handles requests for upload URLs.
func (s *Server) handleHarnessConfigUpload(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	var req UploadRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if len(req.Files) == 0 {
		ValidationError(w, "at least one file is required", nil)
		return
	}

	// Upload URLs stage the files under a fresh upload ID; finalize hashes
	// them and commits (ptone/scion#4219). Local-storage URLs point at the
	// hub's file endpoint with ?uploadId=, which stages only.
	uploadID := api.NewUUID()
	uploadURLs, err := s.generateHarnessConfigUploadURLs(ctx, stor, hc, req.Files, uploadID, requestBaseURL(r))
	if err != nil {
		if writeInvalidFilePathError(w, err) {
			return
		}
		RuntimeError(w, "Failed to generate upload URLs: "+err.Error())
		return
	}
	if len(uploadURLs) == 0 && len(req.Files) > 0 {
		RuntimeError(w, "Failed to generate upload URLs")
		return
	}

	writeJSON(w, http.StatusOK, UploadResponse{
		UploadURLs: uploadURLs,
		UploadID:   uploadID,
	})
}

// maxRecordedSourceURLBytes caps the length of a recorded harness config
// source URL.
const maxRecordedSourceURLBytes = 2048

// isDisallowedSourceURLRune reports whether r may not appear in a recorded
// source URL: control characters and invisible formatting or line/paragraph
// separator characters (Unicode categories Cc, Cf, Zl, Zp).
func isDisallowedSourceURLRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

// invalidSourceURLMessage is the validation error for a sourceUrl that fails
// validRecordedSourceURL (finalize) or validReimportSourceURL (reimport).
var invalidSourceURLMessage = fmt.Sprintf(
	"sourceUrl must be a single-line remote URI (http://, https://, or rclone) of at most %d bytes",
	maxRecordedSourceURLBytes)

// validRecordedSourceURL reports whether s may be recorded as a harness
// config's source URL: a single-line remote URI (http://, https://, or an
// rclone URI) of at most maxRecordedSourceURLBytes, with no control or
// invisible formatting characters. Finalize and the reimport override share
// it.
func validRecordedSourceURL(s string) bool {
	return len(s) <= maxRecordedSourceURLBytes && config.IsRemoteURI(s) &&
		!strings.ContainsFunc(s, isDisallowedSourceURLRune)
}

// validReimportSourceURL reports whether a reimport sourceUrl override is
// acceptable. It applies validRecordedSourceURL, and additionally accepts the
// documented scheme-less "github.com/..." shorthand when its https:// form
// passes the same check (NormalizeTemplateSourceURL adds the scheme).
func validReimportSourceURL(s string) bool {
	if len(s) > maxRecordedSourceURLBytes {
		return false
	}
	if validRecordedSourceURL(s) {
		return true
	}
	if len(s) >= len("github.com/") && strings.EqualFold(s[:len("github.com/")], "github.com/") {
		return validRecordedSourceURL("https://" + s)
	}
	return false
}

// handleHarnessConfigFinalize finalizes a harness config after file upload.
func (s *Server) handleHarnessConfigFinalize(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	var req struct {
		Manifest *HarnessConfigManifest `json:"manifest"`
		// SourceURL optionally records where the uploaded files came from
		// (for example the URL given to 'scion harness-config install').
		// When empty, the stored source URL is left unchanged.
		SourceURL string `json:"sourceUrl,omitempty"`
		// UploadID, when set, is the uploadId of the upload response whose
		// staged files this finalize commits. Clients that omit it are
		// served by matching staged files to the manifest by hash
		// (ptone/scion#4219).
		UploadID string `json:"uploadId,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Manifest == nil || len(req.Manifest.Files) == 0 {
		ValidationError(w, "manifest with files is required", nil)
		return
	}

	sourceURL := strings.TrimSpace(req.SourceURL)
	if sourceURL != "" && !validRecordedSourceURL(sourceURL) {
		ValidationError(w, invalidSourceURLMessage, nil)
		return
	}
	if req.UploadID != "" && !validTemplateUploadID(req.UploadID) {
		ValidationError(w, "uploadId is invalid", map[string]interface{}{"field": "uploadId"})
		return
	}

	hc.Status = store.HarnessConfigStatusActive
	if sourceURL != "" {
		hc.SourceURL = sourceURL
	}
	// The commit hashes the staged uploads and moves them into blobs (400
	// on a hash mismatch, "file not found" when a file was neither staged
	// nor already stored, which older CLIs answer by re-uploading
	// everything), refuses an unusable provisioner (422,
	// ptone/scion#3133), derives Harness and Config from config.yaml and
	// writes the row with a compare-and-swap against the content hash,
	// layout and storage path read above (409 harness_config_conflict when
	// another commit won). Nothing is visible to readers before that write,
	// and files the manifest dropped keep their blobs for in-flight
	// hydrations until the blob garbage collector removes them.
	if err := s.commitHarnessConfigFiles(ctx, hc, req.Manifest.Files, harnessConfigCommitOpts{
		fromStaging: true,
		uploadID:    req.UploadID,
	}); err != nil {
		writeHarnessConfigCommitError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, hc)
}

// handleHarnessConfigCheckImage triggers an immediate image status re-check.
// POST /api/v1/harness-configs/{id}/check-image
func (s *Server) handleHarnessConfigCheckImage(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	image := s.harnessConfigImage(hc)
	if image == "" {
		writeError(w, http.StatusBadRequest, "no_image", "Harness config has no image configured", nil)
		return
	}

	registry := s.resolveImageRegistry()
	resolvedImage := config.RewriteImageRegistry(image, registry)
	slog.Info("checking image status", "id", hc.ID, "image", image,
		"resolved", config.RedactImageRegistry(resolvedImage), "registry", config.RedactImageRegistry(registry))

	if imagecheck.IsBareImageName(image) {
		status := store.HarnessConfigImageStatusUnknown
		source := "broker_check"

		if s.localImagePresent(ctx, image, "") {
			status = store.HarnessConfigImageStatusValid
			source = "local"
		}

		if status != store.HarnessConfigImageStatusValid && resolvedImage != image {
			result := s.imageChecker.CheckRemoteOnly(ctx, resolvedImage)
			if result.Status == "valid" {
				status = store.HarnessConfigImageStatusValid
				source = "registry"
			}
		}

		now := time.Now()
		if err := s.store.UpdateHarnessConfigImageStatus(ctx, hc.ID, status, now); err != nil {
			slog.Warn("failed to persist image status", "id", hc.ID, "error", err)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"image_status":            status,
			"image_status_checked_at": now,
			"source":                  source,
			"resolved_image":          image,
		})
		return
	}

	// A registry-qualified image can be present in a local image store
	// without being in the registry (built locally, never pushed), so the
	// local store is consulted before the registry. A local find is
	// reported but not persisted: the stored image status reflects registry
	// validity only (see store.HarnessConfigImageStatusValid).
	if s.localImagePresent(ctx, "", resolvedImage) {
		writeJSON(w, http.StatusOK, map[string]any{
			"image_status":            store.HarnessConfigImageStatusValid,
			"image_status_checked_at": time.Now(),
			"source":                  "local",
			"resolved_image":          resolvedImage,
		})
		return
	}

	result := s.imageChecker.CheckRemoteOnly(ctx, resolvedImage)
	slog.Info("image check result", "id", hc.ID, "status", result.Status, "source", result.Source, "error", result.Error)

	if err := s.store.UpdateHarnessConfigImageStatus(ctx, hc.ID, result.Status, result.CheckedAt); err != nil {
		slog.Warn("failed to persist image status", "id", hc.ID, "error", err)
	}

	resp := map[string]any{
		"image_status":            result.Status,
		"image_status_checked_at": result.CheckedAt,
		"source":                  result.Source,
		"resolved_image":          resolvedImage,
	}
	if result.Error != "" {
		resp["error"] = result.Error
	}
	writeJSON(w, http.StatusOK, resp)
}

// localImagePresent reports whether an image exists in at least one local
// runtime image store. It asks the same sources as the image-status endpoint:
// every dispatchable node-bound broker, through its image-status API, or, when
// there is no such broker, the hub's co-located runtime. An error from one
// broker does not mask a find on another. shortImage is checked as a
// bare local name and longImage as a registry-qualified name; either may be
// empty. It returns false when no source can be asked.
func (s *Server) localImagePresent(ctx context.Context, shortImage, longImage string) bool {
	var nodeBound []*store.RuntimeBroker
	if s.brokerClient != nil {
		brokerResult, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{Limit: 100})
		if err != nil {
			slog.Warn("image check: failed to list brokers", "error", err)
			return false
		}
		for i := range brokerResult.Items {
			b := &brokerResult.Items[i]
			if isPluginBroker(b) || !s.canDispatchToBroker(ctx, b) || !isNodeBoundBroker(b) {
				continue
			}
			nodeBound = append(nodeBound, b)
		}
	}

	if len(nodeBound) == 0 {
		mgr := s.getImageManager()
		if mgr == nil {
			return false
		}
		for _, img := range []string{shortImage, longImage} {
			if img == "" {
				continue
			}
			exists, err := mgr.ImageExists(ctx, img)
			if err != nil {
				slog.Warn("image check: local runtime check failed", "image", img, "error", err)
				continue
			}
			if exists {
				return true
			}
		}
		return false
	}

	var found atomic.Bool
	var wg sync.WaitGroup
	for _, b := range nodeBound {
		wg.Add(1)
		go func(broker *store.RuntimeBroker) {
			defer wg.Done()
			brokerCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			imgResp, err := s.brokerClient.ImageStatus(brokerCtx, broker.ID, broker.Endpoint, shortImage, longImage)
			if err != nil || imgResp == nil {
				return
			}
			if (imgResp.LocalShort != nil && imgResp.LocalShort.Exists) ||
				(imgResp.LocalLong != nil && imgResp.LocalLong.Exists) {
				found.Store(true)
			}
		}(b)
	}
	wg.Wait()
	return found.Load()
}

// handleHarnessConfigDownload returns signed URLs for downloading harness config files.
func (s *Server) handleHarnessConfigDownload(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	if len(hc.Files) == 0 {
		ValidationError(w, "harness config has no files", nil)
		return
	}

	// Every URL comes from this one row read: a blob row's URLs name
	// immutable blobs, so they keep returning this version even if a commit
	// lands while the client downloads.
	downloadURLs, manifestURL, expires, err := s.generateHarnessConfigDownloadURLs(ctx, stor, hc)
	if err != nil {
		RuntimeError(w, fmt.Sprintf("harness-config %q: %s — run 'scion harness-config validate %s' to diagnose", hc.Name, err, hc.Name))
		return
	}

	// For local storage, rewrite file:// URLs to HTTP proxy URLs: a file://
	// URL names a hub-host path a remote broker cannot read.
	// A blob row's local URLs carry the file's hash, so the raw read serves
	// that blob even after a later commit replaced the file.
	if stor.Provider() == storage.ProviderLocal {
		downloadURLs = rewriteLocalBlobDownloadURLs(downloadURLs, requestBaseURL(r), "harness-configs", hc.ID, isHarnessConfigBlobLayout(hc))
	}

	// ContentHash names the version the URLs were signed for, so a broker
	// caches the files under the hash they really have.
	writeJSON(w, http.StatusOK, DownloadResponse{
		Files:       downloadURLs,
		ManifestURL: manifestURL,
		Expires:     expires,
		ContentHash: hc.ContentHash,
	})
}

// handleHarnessConfigValidate validates a harness-config's storage consistency.
func (s *Server) handleHarnessConfigValidate(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()

	rec := harnessConfigToRecord(hc)
	rs := s.harnessConfigStore(hc.Harness)
	report, err := rs.ValidateStorage(ctx, rec)
	if err != nil {
		RuntimeError(w, fmt.Sprintf("validation failed: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, report)
}

// handleHarnessConfigClone creates a copy of a harness config.
func (s *Server) handleHarnessConfigClone(w http.ResponseWriter, r *http.Request, source *store.HarnessConfig) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	var req CloneTemplateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if !isValidHarnessConfigScope(req.Scope) {
		ValidationError(w, fmt.Sprintf("invalid scope %q: must be \"global\", \"project\" or \"user\"", req.Scope), nil)
		return
	}

	// Resolve scope ID
	scopeID := req.ScopeID
	if scopeID == "" && req.ProjectID != "" {
		scopeID = req.ProjectID
	}

	// Authorize: check destination scope for ActionCreate
	destScope := req.Scope
	if destScope == "" {
		destScope = source.Scope
	}
	if destScope == "" {
		destScope = store.HarnessConfigScopeGlobal
	}
	switch destScope {
	case store.HarnessConfigScopeGlobal:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, harnessConfigScopeResource(store.HarnessConfigScopeGlobal, ""), ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to create global resources", nil)
			return
		}
	case store.HarnessConfigScopeProject:
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			if !agentIdent.HasScope(ScopeAgentCreate) {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope", nil)
				return
			}
			if scopeID != agentIdent.ProjectID() {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only manage resources within their own project", nil)
				return
			}
		} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			decision := s.authzService.CheckAccess(ctx, userIdent,
				harnessConfigScopeResource(store.HarnessConfigScopeProject, scopeID), ActionCreate)
			if !decision.Allowed {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to create resources in this project", nil)
				return
			}
		} else {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
	case store.HarnessConfigScopeUser:
		// Mirrors handleTemplateClone's user-scope branch: scopeID must be
		// the caller's own, never left empty (which would land the clone at
		// a shared, ownerless "users//<slug>" path) or set to another user's
		// ID (which would plant a row and files under that user).
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		if !requireProfileWriter(w, r) {
			return
		}
		if scopeID == "" {
			scopeID = userIdent.ID()
		} else if scopeID != userIdent.ID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You can only clone harness configs into your own user scope", nil)
			return
		}
		// The destination is a new record in the caller's user scope: a
		// user access token needs harness_config:create on a hub boundary.
		if !s.authzService.CheckAccess(ctx, userIdent, harnessConfigUserScopeResource(userIdent), ActionCreate).Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to create harness configs in your user scope", nil)
			return
		}
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Cloning into this resource scope is not supported", nil)
		return
	}

	clone := &store.HarnessConfig{
		ID:          api.NewUUID(),
		Name:        req.Name,
		Slug:        api.Slugify(req.Name),
		DisplayName: source.DisplayName,
		Description: source.Description,
		// Harness is the fallback for a config.yaml that sets none; Config
		// is never copied: the commit below re-derives both from the
		// copied files (ptone/scion#4219).
		Harness: source.Harness,
		Scope:   destScope,
		ScopeID: scopeID,
		Status:  store.HarnessConfigStatusPending,
	}

	// For user-scoped clones, set the owner from the authenticated user
	// (mirrors handleTemplateClone).
	if clone.Scope == store.HarnessConfigScopeUser {
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			clone.OwnerID = userIdent.ID()
			clone.CreatedBy = userIdent.ID()
		}
	}

	// Detect a name collision at the destination BEFORE any storage write —
	// see the matching comment in handleTemplateClone (template_handlers.go)
	// for why this must run ahead of the copy below rather than only being
	// caught by CreateHarnessConfig's own uniqueness check, and why it is
	// only a fast path rather than a full fix for concurrent requests.
	if existing, err := s.store.GetHarnessConfigBySlug(ctx, clone.Slug, clone.Scope, clone.ScopeID); err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErrorFromErr(w, err, "")
		return
	} else if existing != nil {
		writeError(w, http.StatusConflict, "conflict", "A resource with this slug already exists in the target scope. Choose a different name.", nil)
		return
	}

	// Request-unique storage path — see the matching comment in
	// handleTemplateClone for why the deterministic (scope, scopeID, slug)
	// path alone is not concurrency-safe. The clone is always created in
	// the blob layout (ptone/scion#4219): its path is the row-unique
	// <slug>.<id>, and the commit copies each file the source references
	// into the clone's own blobs.
	clone.Layout = store.HarnessConfigLayoutBlobs
	storagePath := s.harnessConfigBlobStoragePath(clone)
	clone.StoragePath = storagePath

	stor := s.GetStorage()
	if stor != nil {
		clone.StorageBucket = stor.Bucket()
		clone.StorageURI = storage.StorageURIForPath(stor.Bucket(), storagePath)
	}

	// A clone with files is created through the commit path, which copies
	// each file the source references into the clone's blobs and
	// re-derives Harness and Config from the copied config.yaml instead of
	// copying the source's derived fields (ptone/scion#4219).
	var createErr error
	if stor != nil && len(source.Files) > 0 && source.StoragePath != "" {
		clone.Status = store.HarnessConfigStatusActive
		createErr = s.commitHarnessConfigFiles(ctx, clone, source.Files, harnessConfigCommitOpts{create: true, copyFrom: source})
	} else {
		createErr = s.store.CreateHarnessConfig(ctx, clone)
	}
	if err := createErr; err != nil {
		if stor != nil {
			_ = stor.DeletePrefix(ctx, templateBlobPrefix(storagePath))
		}
		if errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, "conflict", "A resource with this slug already exists in the target scope. Choose a different name.", nil)
			return
		}
		writeHarnessConfigCommitError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, clone)
}

// ReimportHarnessConfigRequest is the optional request body for the reimport endpoint.
type ReimportHarnessConfigRequest struct {
	SourceURL string `json:"sourceUrl,omitempty"`
}

// reimportHarnessConfigNewStore returns the reimport's newStore: the same
// config.yaml validation as an import, confined to the directory
// (loadLocalHarnessConfigDir: a linked config.yaml fails it), then the store
// that writes the source entry into the row targetID.
func (s *Server) reimportHarnessConfigNewStore(targetID string) func(dir string) (*ResourceStore, error) {
	return func(dir string) (*ResourceStore, error) {
		if _, err := loadLocalHarnessConfigDir(dir); err != nil {
			return nil, err
		}
		return s.targetHarnessConfigStore(targetID), nil
	}
}

// handleHarnessConfigReimport re-imports a harness-config from its stored
// source_url (or an override URL). POST /api/v1/harness-configs/{id}/reimport
func (s *Server) handleHarnessConfigReimport(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	if hc == nil {
		NotFound(w, "HarnessConfig")
		return
	}

	var req ReimportHarnessConfigRequest
	if r.Body != nil && r.Body != http.NoBody {
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
	}

	// A sourceUrl override gets the same validation as finalize, before it
	// is normalized or fetched.
	sourceURL := strings.TrimSpace(req.SourceURL)
	if sourceURL != "" && !validReimportSourceURL(sourceURL) {
		ValidationError(w, invalidSourceURLMessage, nil)
		return
	}
	if sourceURL == "" {
		sourceURL = hc.SourceURL
	}
	if sourceURL == "" {
		writeError(w, http.StatusBadRequest, "no_source_url",
			"No source URL stored and none provided. Use the sourceUrl field to specify one.", nil)
		return
	}

	sourceURL = config.NormalizeTemplateSourceURL(sourceURL)

	// Authorize: same as import — harness_config:create on the owning scope.
	switch hc.Scope {
	case store.HarnessConfigScopeGlobal:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, harnessConfigScopeResource(store.HarnessConfigScopeGlobal, ""), ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to reimport global resources", nil)
			return
		}
	case store.HarnessConfigScopeProject:
		if !s.authorizeProjectImport(ctx, w, hc.ScopeID, "harness-configs", "harness_config") {
			return
		}
	case store.HarnessConfigScopeUser:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		if hc.OwnerID != userIdent.ID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to reimport another user's harness config", nil)
			return
		}
		// Reimport overwrites the config's content, so it needs
		// harness_config:update on the record, not only the route gate's
		// read.
		if !s.authzService.CheckAccess(ctx, userIdent, harnessConfigResource(hc), ActionUpdate).Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to reimport harness configs in your user scope", nil)
			return
		}
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Reimport is not supported for this resource scope", nil)
		return
	}

	if s.GetStorage() == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Storage is not configured", nil)
		return
	}

	// The slug filter selects the source entry (ptone/scion#4213); the
	// target store writes it into the row with hc's ID, whatever the slug
	// of the entry or the row is by the time it is persisted
	// (ptone/scion#4219).
	kind := s.harnessConfigImportKind()
	kind.newStore = s.reimportHarnessConfigNewStore(hc.ID)
	slug := reimportTargetSlug(hc)
	run := func(progress importProgressFunc) ([]string, error) {
		return s.importFromRemoteSelected(ctx, hc.ScopeID, sourceURL, hc.Scope, kind, progress,
			func(dirs []resourceDir, skipped []skippedDir) ([]resourceDir, []skippedDir) {
				return applySlugFilter(dirs, skipped, slug)
			}, fmt.Sprintf("no harness-config in the source matches slug %q", slug))
	}

	if importAcceptsNDJSON(r) {
		s.streamImport(w, run)
		return
	}

	var failures []ImportFailure
	imported, err := run(failureCollector(&failures))
	if err != nil {
		if writeUnusableProvisionerImportError(w, err) {
			return
		}
		writeError(w, http.StatusBadRequest, "reimport_failed", err.Error(), nil)
		return
	}

	if len(imported) == 0 && len(failures) > 0 {
		reasons := make([]string, len(failures))
		for i, f := range failures {
			reasons[i] = f.Name + ": " + f.Reason
		}
		writeError(w, http.StatusBadRequest, "reimport_failed",
			"config.yaml validation failed: "+strings.Join(reasons, "; "), nil)
		return
	}

	writeJSON(w, http.StatusOK, ImportHarnessConfigsResponse{
		HarnessConfigs: imported,
		Count:          len(imported),
		Failed:         failures,
	})
}

// reimportTargetSlug returns the slug that selects hc's entry when
// reimporting it, so a source holding several harness-configs validates and
// imports only the target (ptone/scion#4213). Discovered entries are persisted
// under Slugify(name), so selection compares that against the record's slug;
// matching the record's Name instead could pick another config, since Name and
// Slug can be changed independently. There is no fallback for an empty slug:
// every API write path sets a non-empty slug, and an empty slug would match
// only entries whose names slugify to empty, so the reimport does not guess a
// different record.
func reimportTargetSlug(hc *store.HarnessConfig) string {
	return hc.Slug
}

// handleHarnessConfigImageStatus returns per-broker aggregated image status.
// GET /api/v1/harness-configs/{id}/image-status
func (s *Server) handleHarnessConfigImageStatus(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()

	image := s.harnessConfigImage(hc)
	if image == "" {
		writeError(w, http.StatusBadRequest, "no_image", "Harness config has no image configured", nil)
		return
	}

	registry := s.resolveImageRegistry()
	longImage := config.RewriteImageRegistry(image, registry)

	shortImage := image
	if !imagecheck.IsBareImageName(image) {
		shortImage = ""
	}

	registryStatus := s.checkRegistryImage(ctx, longImage)

	if s.brokerClient == nil {
		if mgr := s.getImageManager(); mgr != nil {
			entry := s.buildLocalImageEntry(ctx, mgr, shortImage, longImage, registryStatus)
			writeJSON(w, http.StatusOK, AggregatedImageStatusResponse{
				Image:    image,
				Registry: &registryStatus,
				Brokers:  []BrokerImageEntry{entry},
			})
			return
		}
		writeJSON(w, http.StatusOK, AggregatedImageStatusResponse{
			Image:    image,
			Registry: &registryStatus,
		})
		return
	}

	brokerResult, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{Limit: 100})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "broker_list_failed", fmt.Sprintf("Failed to list brokers: %v", err), nil)
		return
	}

	var nodeBound []*store.RuntimeBroker
	var proxyEntries []ProxyBrokerEntry
	for i := range brokerResult.Items {
		b := &brokerResult.Items[i]
		if isPluginBroker(b) {
			continue
		}
		if !s.canDispatchToBroker(ctx, b) {
			continue
		}
		if isNodeBoundBroker(b) {
			nodeBound = append(nodeBound, b)
		} else {
			var runtimeTypes []string
			seen := map[string]bool{}
			for _, p := range b.Profiles {
				if !seen[p.Type] {
					runtimeTypes = append(runtimeTypes, p.Type)
					seen[p.Type] = true
				}
			}
			runtime := strings.Join(runtimeTypes, ",")
			proxyEntries = append(proxyEntries, ProxyBrokerEntry{
				BrokerID: b.ID, BrokerName: b.Name, Runtime: runtime,
			})
		}
	}

	brokerEntries := make([]BrokerImageEntry, len(nodeBound))
	var wg sync.WaitGroup
	for i, broker := range nodeBound {
		wg.Add(1)
		go func(idx int, b *store.RuntimeBroker) {
			defer wg.Done()
			entry := BrokerImageEntry{
				BrokerID:   b.ID,
				BrokerName: b.Name,
			}

			brokerCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()

			imgResp, err := s.brokerClient.ImageStatus(brokerCtx, b.ID, b.Endpoint, shortImage, longImage)
			if err != nil {
				var unsupported *BrokerUnsupportedError
				if errors.As(err, &unsupported) {
					entry.Reachable = true
					entry.Unsupported = true
				} else {
					entry.Reachable = false
				}
			} else if imgResp != nil {
				entry.Reachable = true
				entry.LocalShort = imgResp.LocalShort
				entry.LocalLong = imgResp.LocalLong

				if entry.LocalLong != nil && entry.LocalLong.Exists && registryStatus.Hash != "" && entry.LocalLong.Hash != "" {
					entry.NewerInRegistry = registryStatus.Hash != entry.LocalLong.Hash
				}

				switch {
				case entry.LocalShort != nil && entry.LocalShort.Exists:
					entry.ResolvedImage = shortImage
					entry.ResolutionSource = "local_short"
				case entry.LocalLong != nil && entry.LocalLong.Exists:
					entry.ResolvedImage = longImage
					entry.ResolutionSource = "local_long"
				case registryStatus.Exists:
					entry.ResolvedImage = longImage
					entry.ResolutionSource = "remote"
				default:
					entry.ResolutionSource = "none"
				}
			}
			brokerEntries[idx] = entry
		}(i, broker)
	}
	wg.Wait()

	if mgr := s.getImageManager(); len(nodeBound) == 0 && mgr != nil {
		entry := s.buildLocalImageEntry(ctx, mgr, shortImage, longImage, registryStatus)
		brokerEntries = append(brokerEntries, entry)
	}

	resp := AggregatedImageStatusResponse{
		Image:        image,
		Registry:     &registryStatus,
		Brokers:      brokerEntries,
		ProxyBrokers: proxyEntries,
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildLocalImageEntry constructs a BrokerImageEntry using the hub's
// co-located container runtime (Docker/Podman) when no broker client is
// available. This ensures workstation-mode users see pulled image state
// and the Build Image option.
func (s *Server) buildLocalImageEntry(ctx context.Context, mgr imageManager, shortImage, longImage string, registryStatus RegistryImageStatus) BrokerImageEntry {
	result := s.imageChecker.CheckAll(ctx, shortImage, longImage)

	brokerName := "Local Runtime"
	if namer, ok := mgr.(interface{ Name() string }); ok {
		if n := namer.Name(); n != "" {
			brokerName = n
		}
	}

	entry := BrokerImageEntry{
		BrokerName: brokerName,
		Reachable:  true,
	}
	if shortImage != "" {
		entry.LocalShort = &BrokerImageEntityState{Exists: result.LocalShort.Exists, Hash: result.LocalShort.Hash}
	}
	if longImage != "" {
		entry.LocalLong = &BrokerImageEntityState{Exists: result.LocalLong.Exists, Hash: result.LocalLong.Hash}
	}

	if entry.LocalLong != nil && entry.LocalLong.Exists && registryStatus.Hash != "" && entry.LocalLong.Hash != "" {
		entry.NewerInRegistry = registryStatus.Hash != entry.LocalLong.Hash
	}

	switch {
	case entry.LocalShort != nil && entry.LocalShort.Exists:
		entry.ResolvedImage = shortImage
		entry.ResolutionSource = "local_short"
	case entry.LocalLong != nil && entry.LocalLong.Exists:
		entry.ResolvedImage = longImage
		entry.ResolutionSource = "local_long"
	case registryStatus.Exists:
		entry.ResolvedImage = longImage
		entry.ResolutionSource = "remote"
	default:
		entry.ResolutionSource = "none"
	}

	return entry
}

// handleHarnessConfigDeleteLocalImage removes the local short-form image.
// DELETE /api/v1/harness-configs/{id}/local-image?broker_id=...
func (s *Server) handleHarnessConfigDeleteLocalImage(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	if r.Method != http.MethodDelete {
		MethodNotAllowed(w, http.MethodDelete)
		return
	}

	ctx := r.Context()

	image := s.harnessConfigImage(hc)
	if image == "" || !imagecheck.IsBareImageName(image) {
		writeError(w, http.StatusBadRequest, "no_local_image", "Harness config has no short-form image to delete", nil)
		return
	}

	brokerID := r.URL.Query().Get("broker_id")
	if brokerID != "" {
		if s.brokerClient == nil {
			writeError(w, http.StatusServiceUnavailable, "no_broker_client", "Broker routing not available", nil)
			return
		}
		broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if !s.canRunBrokerImageOps(ctx, broker) {
			writeError(w, http.StatusForbidden, "forbidden", "You do not have permission to perform image operations on this broker", nil)
			return
		}
		if !isNodeBoundBroker(broker) {
			writeError(w, http.StatusBadRequest, "invalid_broker", "Image operations are only supported on node-bound brokers", nil)
			return
		}
		if err := s.brokerClient.DeleteImage(ctx, broker.ID, broker.Endpoint, image); err != nil {
			writeError(w, http.StatusInternalServerError, "remove_failed", fmt.Sprintf("Failed to remove image on broker %s: %v", broker.Name, err), nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "image": image, "broker_id": broker.ID})
		return
	}

	mgr := s.getImageManager()
	if mgr == nil {
		writeError(w, http.StatusServiceUnavailable, "no_runtime", "Container runtime not available", nil)
		return
	}

	exists, err := mgr.ImageExists(ctx, image)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "check_failed", fmt.Sprintf("Failed to check image: %v", err), nil)
		return
	}
	if !exists {
		writeJSON(w, http.StatusOK, map[string]string{"status": "not_found", "image": image})
		return
	}

	if err := mgr.RemoveImage(ctx, image); err != nil {
		writeError(w, http.StatusInternalServerError, "remove_failed", fmt.Sprintf("Failed to remove image: %v", err), nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "image": image})
}

// handleHarnessConfigPullImage pulls the latest image from the remote registry.
// POST /api/v1/harness-configs/{id}/pull-image?broker_id=...
func (s *Server) handleHarnessConfigPullImage(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	image := s.harnessConfigImage(hc)
	if image == "" {
		writeError(w, http.StatusBadRequest, "no_image", "Harness config has no image configured", nil)
		return
	}

	registry := s.resolveImageRegistry()
	pullImage := config.RewriteImageRegistry(image, registry)

	brokerID := r.URL.Query().Get("broker_id")
	if brokerID != "" {
		if s.brokerClient == nil {
			writeError(w, http.StatusServiceUnavailable, "no_broker_client", "Broker routing not available", nil)
			return
		}
		broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if !s.canRunBrokerImageOps(ctx, broker) {
			writeError(w, http.StatusForbidden, "forbidden", "You do not have permission to perform image operations on this broker", nil)
			return
		}
		if !isNodeBoundBroker(broker) {
			writeError(w, http.StatusBadRequest, "invalid_broker", "Image operations are only supported on node-bound brokers", nil)
			return
		}
		if err := s.brokerClient.PullImage(ctx, broker.ID, broker.Endpoint, pullImage); err != nil {
			writeError(w, http.StatusInternalServerError, "pull_failed", fmt.Sprintf("Failed to pull image on broker %s: %v", broker.Name, err), nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "pulled", "image": pullImage, "broker_id": broker.ID})
		return
	}

	mgr := s.getImageManager()
	if mgr == nil {
		writeError(w, http.StatusServiceUnavailable, "no_runtime", "Container runtime not available", nil)
		return
	}

	if err := mgr.PullImage(ctx, pullImage); err != nil {
		writeError(w, http.StatusInternalServerError, "pull_failed", fmt.Sprintf("Failed to pull image: %v", err), nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "pulled", "image": pullImage})
}

// harnessConfigScopeResource builds an ad hoc "harness_config" Resource for
// authorization checks that have a scope to evaluate but no concrete
// store.HarnessConfig record (e.g. authorizing a create, clone destination,
// or reimport before/without a resolved record in hand). scope should be one
// of the store.HarnessConfigScope* constants; scopeID is the owning project
// ID and is ignored outside project scope.
//
// ptone/scion#1916: every ad hoc "harness_config" Resource literal must set
// ScopeKind through this constructor (or harnessConfigResource, for a real
// record), never by hand — filterHubWideHarnessConfigGrants only narrows the
// curated hub-member/hub-viewer grant when ScopeKind is populated, so a
// hand-built literal that forgets it would fall through unfiltered. See
// TestHarnessConfigResourceLiterals_AllUseCanonicalConstructor.
func harnessConfigScopeResource(scope, scopeID string) Resource {
	r := Resource{Type: "harness_config", ScopeKind: scope}
	if scope == store.HarnessConfigScopeProject && scopeID != "" {
		r.ParentType = "project"
		r.ParentID = scopeID
	}
	return r
}

// harnessConfigUserScopeResource builds the "harness_config" Resource for a
// new harness config in the caller's own user scope (a clone or import
// destination). Like templateUserScopeResource, it takes the authenticated
// UserIdentity, so the owner always names the caller and never a request
// field.
func harnessConfigUserScopeResource(userIdent UserIdentity) Resource {
	r := harnessConfigScopeResource(store.HarnessConfigScopeUser, "")
	r.OwnerID = userIdent.ID()
	return r
}
