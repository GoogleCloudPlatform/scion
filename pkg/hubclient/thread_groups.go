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

package hubclient

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// threadGroupsPath is the collection route of project-scoped chat thread
// groups (ptone/scion#4424, design section 2).
const threadGroupsPath = "/api/v1/thread-groups"

// ThreadGroupService provides operations on project-scoped chat thread
// groups: named sets of a project's group conversations (threads).
//
// A hub older than ptone/scion#5546 serves the /api/v1/thread-groups routes
// only while its thread groups experiment is on, and answers 404 on
// every one of them otherwise (as does a hub that predates the routes).
// Every method reports that case as a *ThreadGroupsNotEnabledError (see
// IsThreadGroupsNotEnabled), never as a plain not-found.
type ThreadGroupService interface {
	// List returns the project's thread groups, sorted by name. An empty
	// projectID lets the hub default it to an agent caller's project; a
	// user must pass one.
	List(ctx context.Context, projectID string) ([]ThreadGroup, error)

	// Get returns one thread group with its member threads.
	Get(ctx context.Context, id string) (*ThreadGroup, error)

	// Create creates a thread group. The hub answers 409 when the project
	// already has a group with the same name (compared case-insensitively).
	Create(ctx context.Context, req *CreateThreadGroupRequest) (*ThreadGroup, error)

	// Rename renames a thread group (409 on a name conflict).
	Rename(ctx context.Context, id, name string) (*ThreadGroup, error)

	// Delete deletes a thread group. Its threads become ungrouped and are
	// not deleted. Only a user may delete; an agent gets 403.
	Delete(ctx context.Context, id string) error

	// AddThread files a thread into a thread group, moving it out of any
	// other group of the project. It is idempotent.
	AddThread(ctx context.Context, id string, req *AddThreadGroupThreadRequest) (*ThreadGroup, error)

	// RemoveThread takes a conversation out of a thread group. It is
	// idempotent.
	RemoveThread(ctx context.Context, id, conversationID string) error
}

// threadGroupService is the implementation of ThreadGroupService.
type threadGroupService struct {
	c *client
}

// ThreadGroup is a project-scoped chat thread group as the hub returns it.
type ThreadGroup struct {
	ID            string    `json:"id"`
	ProjectID     string    `json:"projectId"`
	Name          string    `json:"name"`
	CreatedBy     string    `json:"createdBy"`
	CreatedByKind string    `json:"createdByKind"` // user | agent
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	// Threads is never nil after decoding; a group with no live threads
	// has an empty list.
	Threads []ThreadGroupThread `json:"threads"`
}

// ThreadGroupThread is one member thread of a ThreadGroup. ThreadID (the
// webchat topic ID) is empty for a group conversation with no topic (an
// external surface); Name then comes from the conversation.
type ThreadGroupThread struct {
	ConversationID string    `json:"conversationId"`
	ThreadID       string    `json:"threadId"`
	Name           string    `json:"name"`
	AddedBy        string    `json:"addedBy"`
	AddedAt        time.Time `json:"addedAt"`
}

// CreateThreadGroupRequest is the body of POST /api/v1/thread-groups. An
// empty ProjectID lets the hub default it to an agent caller's project.
type CreateThreadGroupRequest struct {
	ProjectID string `json:"projectId,omitempty"`
	Name      string `json:"name"`
}

// AddThreadGroupThreadRequest is the body of
// POST /api/v1/thread-groups/{id}/threads. Set exactly one field: a
// conversation ID, or a webchat topic (thread) ID, which the hub resolves
// within the group's project only.
type AddThreadGroupThreadRequest struct {
	ConversationID string `json:"conversationId,omitempty"`
	ThreadID       string `json:"threadId,omitempty"`
}

// threadGroupListResponse is the body of GET /api/v1/thread-groups.
type threadGroupListResponse struct {
	ThreadGroups []ThreadGroup `json:"threadGroups"`
}

// ThreadGroupsNotEnabledMessage is the error text for a hub that does not
// serve /api/v1/thread-groups: an older hub with its thread groups
// experiment off, or a hub that predates the routes.
const ThreadGroupsNotEnabledMessage = "this hub does not serve thread groups: upgrade the hub, or turn on its thread groups experiment"

// ThreadGroupsNotEnabledError reports that the hub answered 404 because it
// does not serve thread groups. Err is the hub's original error.
type ThreadGroupsNotEnabledError struct {
	Err error
}

// Error implements the error interface.
func (e *ThreadGroupsNotEnabledError) Error() string { return ThreadGroupsNotEnabledMessage }

// Unwrap returns the hub's original error.
func (e *ThreadGroupsNotEnabledError) Unwrap() error { return e.Err }

// IsThreadGroupsNotEnabled reports whether err is, or wraps, a
// *ThreadGroupsNotEnabledError.
func IsThreadGroupsNotEnabled(err error) bool {
	var target *ThreadGroupsNotEnabledError
	return errors.As(err, &target)
}

// isAPINotFound reports whether err is a hub 404.
func isAPINotFound(err error) bool {
	var apiErr *apiclient.APIError
	return errors.As(err, &apiErr) && apiErr.IsNotFound()
}

// collectionErr maps a 404 from the collection route to
// *ThreadGroupsNotEnabledError. A hub that serves thread groups never
// answers 404 on the collection (a missing project is 403 and a missing
// projectId 400), so a 404 there can only come from an older hub's
// experiment gate or a hub without the route.
func collectionErr(err error) error {
	if isAPINotFound(err) {
		return &ThreadGroupsNotEnabledError{Err: err}
	}
	return err
}

// itemErr maps a 404 from a /api/v1/thread-groups/{id}... route. Such a 404
// may mean the group or thread does not exist, or that the hub does not
// serve thread groups. To tell them apart without matching message text, it
// probes the collection route with no projectId, which is 404 only on a hub
// that does not serve thread groups (otherwise it is 400 for a user, or 200
// or 403 for an agent).
// Any probe answer other than 404, including a transport failure, keeps
// the original error.
func (s *threadGroupService) itemErr(ctx context.Context, err error) error {
	if !isAPINotFound(err) {
		return err
	}
	resp, probeErr := s.c.get(ctx, threadGroupsPath, nil)
	if probeErr != nil {
		return err
	}
	if checkErr := apiclient.CheckResponse(resp); isAPINotFound(checkErr) {
		return &ThreadGroupsNotEnabledError{Err: err}
	}
	return err
}

// itemPath returns the route of thread group id.
func itemPath(id string) string {
	return threadGroupsPath + "/" + url.PathEscape(id)
}

// normalizeThreadGroup makes a decoded group's Threads non-nil.
func normalizeThreadGroup(g *ThreadGroup) *ThreadGroup {
	if g != nil && g.Threads == nil {
		g.Threads = []ThreadGroupThread{}
	}
	return g
}

// List returns the project's thread groups, sorted by name.
func (s *threadGroupService) List(ctx context.Context, projectID string) ([]ThreadGroup, error) {
	query := url.Values{}
	if projectID != "" {
		query.Set("projectId", projectID)
	}
	resp, err := s.c.getWithQuery(ctx, threadGroupsPath, query, nil)
	if err != nil {
		return nil, err
	}
	// The hub always answers the list with a body, so DecodeRequired: a
	// 204 or empty body is an error, never a silent empty list.
	result, err := apiclient.DecodeRequired[threadGroupListResponse](resp)
	if err != nil {
		return nil, collectionErr(err)
	}
	if result.ThreadGroups == nil {
		return []ThreadGroup{}, nil
	}
	for i := range result.ThreadGroups {
		normalizeThreadGroup(&result.ThreadGroups[i])
	}
	return result.ThreadGroups, nil
}

// Get returns one thread group with its member threads.
func (s *threadGroupService) Get(ctx context.Context, id string) (*ThreadGroup, error) {
	resp, err := s.c.get(ctx, itemPath(id), nil)
	if err != nil {
		return nil, err
	}
	g, err := apiclient.DecodeRequired[ThreadGroup](resp)
	if err != nil {
		return nil, s.itemErr(ctx, err)
	}
	return normalizeThreadGroup(g), nil
}

// Create creates a thread group.
func (s *threadGroupService) Create(ctx context.Context, req *CreateThreadGroupRequest) (*ThreadGroup, error) {
	resp, err := s.c.post(ctx, threadGroupsPath, req, nil)
	if err != nil {
		return nil, err
	}
	g, err := apiclient.DecodeRequired[ThreadGroup](resp)
	if err != nil {
		return nil, collectionErr(err)
	}
	return normalizeThreadGroup(g), nil
}

// Rename renames a thread group.
func (s *threadGroupService) Rename(ctx context.Context, id, name string) (*ThreadGroup, error) {
	body := map[string]string{"name": name}
	resp, err := s.c.patch(ctx, itemPath(id), body, nil)
	if err != nil {
		return nil, err
	}
	g, err := apiclient.DecodeRequired[ThreadGroup](resp)
	if err != nil {
		return nil, s.itemErr(ctx, err)
	}
	return normalizeThreadGroup(g), nil
}

// Delete deletes a thread group.
func (s *threadGroupService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.delete(ctx, itemPath(id), nil)
	if err != nil {
		return err
	}
	if err := apiclient.CheckResponse(resp); err != nil {
		return s.itemErr(ctx, err)
	}
	return nil
}

// AddThread files a thread into a thread group.
func (s *threadGroupService) AddThread(ctx context.Context, id string, req *AddThreadGroupThreadRequest) (*ThreadGroup, error) {
	resp, err := s.c.post(ctx, itemPath(id)+"/threads", req, nil)
	if err != nil {
		return nil, err
	}
	g, err := apiclient.DecodeRequired[ThreadGroup](resp)
	if err != nil {
		return nil, s.itemErr(ctx, err)
	}
	return normalizeThreadGroup(g), nil
}

// RemoveThread takes a conversation out of a thread group.
func (s *threadGroupService) RemoveThread(ctx context.Context, id, conversationID string) error {
	resp, err := s.c.delete(ctx, itemPath(id)+"/threads/"+url.PathEscape(conversationID), nil)
	if err != nil {
		return err
	}
	if err := apiclient.CheckResponse(resp); err != nil {
		return s.itemErr(ctx, err)
	}
	return nil
}
