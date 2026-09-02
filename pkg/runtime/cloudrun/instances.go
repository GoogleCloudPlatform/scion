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

package cloudrun

import (
	"context"

	runapi "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/googleapis/gax-go/v2"
)

// InstancesAPI is the subset of the Cloud Run Instances Admin API that the
// Cloud Run runtime uses. It exists so the lifecycle (create/start/stop/
// delete/list) can be exercised in tests without GCP credentials or network
// access, mirroring the logEntryClient seam used for Cloud Logging in logs.go.
//
// The method set intentionally matches *runapi.InstancesClient so that
// googleInstancesClient is a pass-through adapter with no behavior of its own.
type InstancesAPI interface {
	GetInstance(ctx context.Context, req *runpb.GetInstanceRequest, opts ...gax.CallOption) (*runpb.Instance, error)
	CreateInstance(ctx context.Context, req *runpb.CreateInstanceRequest, opts ...gax.CallOption) (InstanceOperation, error)
	StartInstance(ctx context.Context, req *runpb.StartInstanceRequest, opts ...gax.CallOption) (InstanceOperation, error)
	StopInstance(ctx context.Context, req *runpb.StopInstanceRequest, opts ...gax.CallOption) (InstanceOperation, error)
	DeleteInstance(ctx context.Context, req *runpb.DeleteInstanceRequest, opts ...gax.CallOption) (InstanceOperation, error)
	ListInstances(ctx context.Context, req *runpb.ListInstancesRequest, opts ...gax.CallOption) InstanceIterator
	Close() error
}

// InstanceOperation is a long-running operation returned by the mutating
// Instances API calls. Create, Start, Stop and Delete all expose the same
// Wait signature, so one interface covers every lifecycle operation.
type InstanceOperation interface {
	Wait(ctx context.Context, opts ...gax.CallOption) (*runpb.Instance, error)
}

// InstanceIterator yields instances from ListInstances, returning
// iterator.Done when exhausted.
type InstanceIterator interface {
	Next() (*runpb.Instance, error)
}

// googleInstancesClient adapts *runapi.InstancesClient to InstancesAPI. The
// concrete operation and iterator types are returned as interfaces; no other
// behavior is added.
type googleInstancesClient struct {
	client *runapi.InstancesClient
}

// NewInstancesClient returns an InstancesAPI backed by the real Cloud Run
// Instances API. The caller is responsible for closing it.
func NewInstancesClient(ctx context.Context) (InstancesAPI, error) {
	client, err := runapi.NewInstancesClient(ctx)
	if err != nil {
		return nil, err
	}
	return &googleInstancesClient{client: client}, nil
}

func (c *googleInstancesClient) GetInstance(ctx context.Context, req *runpb.GetInstanceRequest, opts ...gax.CallOption) (*runpb.Instance, error) {
	return c.client.GetInstance(ctx, req, opts...)
}

func (c *googleInstancesClient) CreateInstance(ctx context.Context, req *runpb.CreateInstanceRequest, opts ...gax.CallOption) (InstanceOperation, error) {
	return c.client.CreateInstance(ctx, req, opts...)
}

func (c *googleInstancesClient) StartInstance(ctx context.Context, req *runpb.StartInstanceRequest, opts ...gax.CallOption) (InstanceOperation, error) {
	return c.client.StartInstance(ctx, req, opts...)
}

func (c *googleInstancesClient) StopInstance(ctx context.Context, req *runpb.StopInstanceRequest, opts ...gax.CallOption) (InstanceOperation, error) {
	return c.client.StopInstance(ctx, req, opts...)
}

func (c *googleInstancesClient) DeleteInstance(ctx context.Context, req *runpb.DeleteInstanceRequest, opts ...gax.CallOption) (InstanceOperation, error) {
	return c.client.DeleteInstance(ctx, req, opts...)
}

func (c *googleInstancesClient) ListInstances(ctx context.Context, req *runpb.ListInstancesRequest, opts ...gax.CallOption) InstanceIterator {
	return c.client.ListInstances(ctx, req, opts...)
}

func (c *googleInstancesClient) Close() error {
	return c.client.Close()
}
