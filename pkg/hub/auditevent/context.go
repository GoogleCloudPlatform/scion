// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auditevent

import (
	"context"

	"github.com/google/uuid"
)

// AuditOperationContext carries the correlation shared by every audit event
// from one logical operation.
type AuditOperationContext struct {
	CorrelationID string `json:"correlation_id"`
}

type operationContextKey struct{}

// NewOperationContext validates a trusted request or restored operation ID.
func NewOperationContext(correlationID string) (AuditOperationContext, error) {
	if err := validateBoundedString("correlation_id", correlationID, 128); err != nil {
		return AuditOperationContext{}, err
	}
	return AuditOperationContext{CorrelationID: correlationID}, nil
}

// StartOperation creates a fresh context for work that has no trusted request
// ID. It must be called once at operation ingress, never by an event builder.
func StartOperation() AuditOperationContext {
	return AuditOperationContext{CorrelationID: uuid.NewString()}
}

// ContextWithOperation stores an operation context on ctx.
func ContextWithOperation(ctx context.Context, operation AuditOperationContext) context.Context {
	return context.WithValue(ctx, operationContextKey{}, operation)
}

// OperationFromContext retrieves the operation context stored on ctx.
func OperationFromContext(ctx context.Context) (AuditOperationContext, bool) {
	if ctx == nil {
		return AuditOperationContext{}, false
	}
	operation, ok := ctx.Value(operationContextKey{}).(AuditOperationContext)
	if !ok || operation.CorrelationID == "" {
		return AuditOperationContext{}, false
	}
	return operation, true
}

func requireOperation(ctx context.Context) (AuditOperationContext, error) {
	operation, ok := OperationFromContext(ctx)
	if !ok {
		return AuditOperationContext{}, invalid("operation_context", "is required")
	}
	if _, err := NewOperationContext(operation.CorrelationID); err != nil {
		return AuditOperationContext{}, err
	}
	return operation, nil
}
