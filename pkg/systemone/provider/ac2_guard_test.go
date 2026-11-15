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

package provider_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAC2_NoCredentialOrSecretReachableFromAdapterInputs enforces AC-2
// (runtime-design §4.3, P1 brief §7 AC-2): no credential-typed, secret-tagged,
// or free-form interface/function field is reachable from ModelRequest,
// InputField, or InstanceView.
func TestAC2_NoCredentialOrSecretReachableFromAdapterInputs(t *testing.T) {
	roots := []struct {
		name string
		typ  reflect.Type
	}{
		{name: "ModelRequest", typ: reflect.TypeOf(provider.ModelRequest{})},
		{name: "InputField", typ: reflect.TypeOf(provider.InputField{})},
		{name: "InstanceView", typ: reflect.TypeOf(provider.InstanceView{})},
	}

	for _, root := range roots {
		t.Run(root.name, func(t *testing.T) {
			violations := inspectCredentialReachability(root.typ)
			require.Empty(t, violations, "%s violated AC-2 credential separation:\n  %s",
				root.name, strings.Join(violations, "\n  "))
		})
	}
}

// TestAC2_ReflectionGuardCatchesSyntheticViolations self-tests the reflection
// walker against nested secret tags, credential field/type names, and opaque
// interface/func smuggling channels.
func TestAC2_ReflectionGuardCatchesSyntheticViolations(t *testing.T) {
	type nestedSecretTag struct {
		Value string `json:"value" secret:"true"`
	}
	type withNestedSliceTag struct {
		Items []nestedSecretTag
	}
	v1 := inspectCredentialReachability(reflect.TypeOf(withNestedSliceTag{}))
	assert.NotEmpty(t, v1, "expected violation for nested secret struct tag")

	type APIKeyCredential struct {
		Raw string
	}
	type withMapCredentialType struct {
		Creds map[string]*APIKeyCredential
	}
	v2 := inspectCredentialReachability(reflect.TypeOf(withMapCredentialType{}))
	assert.NotEmpty(t, v2, "expected violation for credential-named type")

	type withSecretFieldName struct {
		BearerToken string
	}
	v3 := inspectCredentialReachability(reflect.TypeOf(withSecretFieldName{}))
	assert.NotEmpty(t, v3, "expected violation for credential-named field")

	type withFreeFormAny struct {
		Payload any
	}
	v4 := inspectCredentialReachability(reflect.TypeOf(withFreeFormAny{}))
	assert.NotEmpty(t, v4, "expected violation for free-form interface field")

	type withCallback struct {
		HeaderFn func() string
	}
	v5 := inspectCredentialReachability(reflect.TypeOf(withCallback{}))
	assert.NotEmpty(t, v5, "expected violation for function-typed field")
}

var forbiddenCredentialTokens = []string{
	"secret",
	"credential",
	"apikey",
	"api_key",
	"token",
	"password",
	"passwd",
	"bearer",
	"authorization",
	"privatekey",
	"private_key",
	"clientsecret",
	"client_secret",
}

func inspectCredentialReachability(root reflect.Type) []string {
	visited := make(map[reflect.Type]bool)
	var violations []string

	var walk func(t reflect.Type, path string)
	walk = func(t reflect.Type, path string) {
		if t == nil {
			return
		}
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}

		if pkgPath := strings.ToLower(t.PkgPath()); pkgPath != "" {
			if strings.Contains(pkgPath, "/secret") ||
				strings.Contains(pkgPath, "oauth2") ||
				strings.Contains(pkgPath, "transportauth") {
				violations = append(violations, fmt.Sprintf("%s: forbidden credential package %q (%s)", path, t.PkgPath(), t.String()))
			}
		}
		if typeName := strings.ToLower(t.Name()); typeName != "" {
			for _, tok := range forbiddenCredentialTokens {
				if strings.Contains(typeName, tok) {
					violations = append(violations, fmt.Sprintf("%s: forbidden credential type name %q", path, t.String()))
					break
				}
			}
		}

		switch t.Kind() {
		case reflect.Interface:
			violations = append(violations, fmt.Sprintf("%s: free-form interface type %s is forbidden on adapter inputs", path, t.String()))
			return
		case reflect.Func, reflect.Chan, reflect.UnsafePointer:
			violations = append(violations, fmt.Sprintf("%s: non-data kind %s is forbidden on adapter inputs", path, t.Kind()))
			return
		case reflect.Slice, reflect.Array:
			walk(t.Elem(), path+"[]")
			return
		case reflect.Map:
			walk(t.Key(), path+"{key}")
			walk(t.Elem(), path+"{val}")
			return
		case reflect.Struct:
			if visited[t] {
				return
			}
			visited[t] = true
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				fPath := path + "." + f.Name

				if _, hasSecretTag := f.Tag.Lookup("secret"); hasSecretTag || strings.Contains(strings.ToLower(string(f.Tag)), "secret") {
					violations = append(violations, fmt.Sprintf("%s: struct tag %q carries secret marker", fPath, string(f.Tag)))
				}

				lowerField := strings.ToLower(f.Name)
				for _, tok := range forbiddenCredentialTokens {
					if strings.Contains(lowerField, tok) {
						violations = append(violations, fmt.Sprintf("%s: field name matches credential token %q", fPath, tok))
						break
					}
				}
				if lowerField == "auth" || strings.HasPrefix(lowerField, "auth_") || strings.HasSuffix(lowerField, "auth") {
					violations = append(violations, fmt.Sprintf("%s: auth-bearing field is forbidden on adapter inputs", fPath))
				}

				walk(f.Type, fPath)
			}
		}
	}

	walk(root, root.Name())
	return violations
}
