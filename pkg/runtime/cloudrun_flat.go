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

package runtime

import (
	"context"
	"errors"
	"fmt"

	"cloud.google.com/go/auth"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	resourcemanagerpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
)

// Flat Cloud Run instances (amendment P5-CR). A flat cloudrun Runtime Broker
// instance runs the real CloudRunRuntime, built from the instance's own
// explicit configuration and its own credentials file: every Cloud Run
// Admin, Cloud Logging, Resource Manager and exec call uses those
// credentials, never Application Default Credentials or the metadata
// server, and project and region are never auto-detected.

// NewFlatCloudRunRuntime returns the CloudRunRuntime of a flat cloudrun
// instance. cfg must name both the project ID and the region, and no NFS
// server or export (the flat instance always mounts the host's share).
// creds is the instance's parsed credential, built once by the caller; it is
// the only credential the runtime uses.
func NewFlatCloudRunRuntime(cfg config.CloudRunConfig, creds *auth.Credentials) (*CloudRunRuntime, error) {
	if creds == nil {
		return nil, errors.New("cloudrun: a flat instance requires its own credentials")
	}
	rt, err := newFlatCloudRunRuntime(cfg, []option.ClientOption{option.WithAuthCredentials(creds)})
	if err != nil {
		return nil, err
	}
	rt.exec = cloudrun.NewIAPExecConnectorWithCredentials(creds, rt.clientOpts...)
	return rt, nil
}

// newFlatCloudRunRuntime checks cfg and builds the runtime with opts. The
// configuration counts as resolved from the start: resolveConfig never asks
// the metadata server for a flat instance.
func newFlatCloudRunRuntime(cfg config.CloudRunConfig, opts []option.ClientOption) (*CloudRunRuntime, error) {
	if cfg.ProjectID == "" || cfg.Location == "" {
		return nil, errors.New("cloudrun: a flat instance requires both project_id and region")
	}
	if cfg.NFSServer != "" || cfg.NFSExport != "" {
		return nil, errors.New("cloudrun: a flat instance always uses the host's NFS share; it takes no NFS server or export")
	}
	c := cfg
	return &CloudRunRuntime{config: &c, clientOpts: opts, resolved: true}, nil
}

// ErrProjectNumberBound is returned by BindProjectNumber when the runtime
// already has a different project number.
var ErrProjectNumberBound = errors.New("cloudrun: a different project number is already bound")

// BindProjectNumber binds the probed project number once, before activation
// (amendment P5-CR, CR3: Cloud Run Admin operations use the probed project
// number): from then on every Cloud Run Admin resource name
// (parent()) uses projects/<number>/locations/<region>, the exact probed
// scope. Only parent() reads it; the configured project ID stays in the
// runtime's configuration, for Cloud Logging, the exec connector and its
// key cache. Binding the same number again is a no-op; a different one is
// refused (ErrProjectNumberBound).
func (r *CloudRunRuntime) BindProjectNumber(number string) error {
	if number == "" {
		return errors.New("cloudrun: an empty project number cannot be bound")
	}
	r.resolveMu.Lock()
	defer r.resolveMu.Unlock()
	switch r.projectNumber {
	case "":
		r.projectNumber = number
		return nil
	case number:
		return nil
	default:
		return ErrProjectNumberBound
	}
}

// ProjectID returns the configured project ID.
func (r *CloudRunRuntime) ProjectID() string { return r.config.ProjectID }

// Location returns the configured region.
func (r *CloudRunRuntime) Location() string { return r.config.Location }

// ProbeProject reads the configured project through Resource Manager with
// the runtime's own credentials (the first read of the flat scope probe).
// The API's error is returned as is, so the caller can classify it.
func (r *CloudRunRuntime) ProbeProject(ctx context.Context) (*resourcemanagerpb.Project, error) {
	name := "projects/" + r.config.ProjectID
	if r.getProject != nil {
		return r.getProject(ctx, name)
	}
	c, err := resourcemanager.NewProjectsClient(ctx, r.clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating the Resource Manager client: %w", err)
	}
	defer func() { _ = c.Close() }()
	return c.GetProject(ctx, &resourcemanagerpb.GetProjectRequest{Name: name})
}

// ProbeListInstances lists at most one Cloud Run instance in the configured
// project and region with the runtime's own credentials (the second read
// of the flat scope probe): it proves the region and the list permission.
// An empty region is no error. The API's error is returned as is.
func (r *CloudRunRuntime) ProbeListInstances(ctx context.Context) error {
	c, err := r.client(ctx)
	if err != nil {
		return fmt.Errorf("creating the Cloud Run client: %w", err)
	}
	defer func() { _ = c.Close() }()
	it := c.ListInstances(ctx, &runpb.ListInstancesRequest{
		Parent:   fmt.Sprintf("projects/%s/locations/%s", r.config.ProjectID, r.config.Location),
		PageSize: 1,
	}, defaultCallOpts...)
	if _, err := it.Next(); err != nil && !errors.Is(err, iterator.Done) {
		return err
	}
	return nil
}
