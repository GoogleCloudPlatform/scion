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

// Package cloudrunenv detects whether the process runs on a Cloud Run
// service or a Cloud Run Instance, from the environment variables the
// platform sets. It is the single definition of those checks; callers must
// not read the variables directly. It imports only the standard library.
package cloudrunenv

import "os"

const (
	// ServiceEnv is set by Cloud Run on services to the service name. It is
	// not set on Cloud Run Instances.
	ServiceEnv = "K_SERVICE"
	// InstanceEnv is set by Cloud Run on Instances. It is not set on
	// services.
	InstanceEnv = "CLOUD_RUN_INSTANCE"
)

// ServiceName returns the Cloud Run service name, or "" when the process is
// not running on a Cloud Run service.
func ServiceName() string { return ServiceNameFrom(os.Getenv) }

// IsService reports whether the process runs on a Cloud Run service.
func IsService() bool { return IsServiceFrom(os.Getenv) }

// IsInstance reports whether the process runs on a Cloud Run Instance.
func IsInstance() bool { return IsInstanceFrom(os.Getenv) }

// ServiceNameFrom is ServiceName reading the environment through getenv.
func ServiceNameFrom(getenv func(string) string) string { return getenv(ServiceEnv) }

// IsServiceFrom is IsService reading the environment through getenv.
func IsServiceFrom(getenv func(string) string) bool { return ServiceNameFrom(getenv) != "" }

// IsInstanceFrom is IsInstance reading the environment through getenv.
func IsInstanceFrom(getenv func(string) string) bool { return getenv(InstanceEnv) != "" }
