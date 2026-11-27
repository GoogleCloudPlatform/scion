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

import "context"

// resolvedContainerIDKey is the context key marking an Exec/ExecWithStdin call
// whose id argument is already the concrete ContainerID returned by this
// runtime's List in the same operation.
type resolvedContainerIDKey struct{}

// WithResolvedContainerID marks ctx so container runtimes (Docker, Podman,
// Apple Container) skip their inner List re-resolution in Exec and
// ExecWithStdin. Set this only when the id passed to Exec/ExecWithStdin came
// from a List result in the same operation; callers that pass a slug or name
// must leave ctx unmarked.
//
// Runtimes that do not perform an inner List ignore the marker. Error
// classification on exec failure (ErrContainerNotFound) is unaffected.
func WithResolvedContainerID(ctx context.Context) context.Context {
	return context.WithValue(ctx, resolvedContainerIDKey{}, true)
}

// resolvedContainerID reports whether ctx was marked by WithResolvedContainerID.
func resolvedContainerID(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(resolvedContainerIDKey{}).(bool)
	return v
}
