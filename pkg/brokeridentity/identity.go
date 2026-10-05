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

// Package brokeridentity owns the persistent local identity of a flat
// Runtime Broker instance: its immutable instance key, the locally minted
// Runtime Broker ID and runtime target ID, and the normalized execution scope
// the target is bound to (.design/flat-runtime-brokers-contract.md sections 3-5).
//
// The package has no runtime or Hub dependency: callers probe the execution
// scope and pass it in.
package brokeridentity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// SchemaVersion is the current identity file schema version.
const SchemaVersion = 1

// File and directory names inside an instance directory.
const (
	// IdentityFileName is the identity file inside <dir>.
	IdentityFileName = "identity.json"
	lockFileName     = ".lock"
	tempFilePrefix   = ".identity.json.tmp-"
	// RuntimeBrokersDirName is the directory under the global Scion dir that
	// holds one subdirectory per instance key.
	RuntimeBrokersDirName = "runtime-brokers"
)

// Runtime target types.
const (
	TargetTypeDocker     = "docker"
	TargetTypeKubernetes = "kubernetes"
)

// Errors returned by LoadOrCreate and VerifyScope. They are never followed
// by a silent re-mint.
var (
	ErrIdentityMissing            = errors.New("identity state is missing but other instance state exists")
	ErrIdentityCorrupt            = errors.New("identity file is corrupt")
	ErrIdentityKeyMismatch        = errors.New("identity file belongs to a different instance key")
	ErrRuntimeTargetTypeChanged   = errors.New("configured runtime target type differs from the recorded identity")
	ErrIdentityCollidesWithLegacy = errors.New("runtime broker ID collides with a legacy Runtime Broker identity")
	ErrExecutionScopeChanged      = errors.New("execution scope differs from the recorded identity")
	ErrExecutionScopeUnidentified = errors.New("execution scope has no identity")
)

// InstanceKeyPattern is the frozen shape of an instance key.
const InstanceKeyPattern = `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`

var instanceKeyRe = regexp.MustCompile(InstanceKeyPattern)

// ValidInstanceKey reports whether key matches InstanceKeyPattern.
func ValidInstanceKey(key string) bool { return instanceKeyRe.MatchString(key) }

// DockerScope is the normalized execution scope of a Docker target. DaemonID
// is the identity field; Endpoint is informational.
type DockerScope struct {
	DaemonID string `json:"daemonId"`
	Endpoint string `json:"endpoint,omitempty"`
}

// KubernetesScope is the normalized execution scope of a Kubernetes target
// (defined, not implemented in P1). ClusterUID and Namespace are the
// identity fields; APIServer is informational. Context aliases, kubeconfig
// paths and credentials are never recorded.
type KubernetesScope struct {
	ClusterUID string `json:"clusterUid"`
	Namespace  string `json:"namespace"`
	APIServer  string `json:"apiServer,omitempty"`
}

// ExecutionScope is the normalized execution-scope record a runtime target
// ID is bound to.
type ExecutionScope struct {
	Type       string           `json:"type"`
	Docker     *DockerScope     `json:"docker,omitempty"`
	Kubernetes *KubernetesScope `json:"kubernetes,omitempty"`
}

// Identity is the persisted identity of one flat Runtime Broker instance.
// Every field is immutable once written.
type Identity struct {
	SchemaVersion   int                         `json:"schemaVersion"`
	InstanceKey     string                      `json:"instanceKey"`
	RuntimeBrokerID string                      `json:"runtimeBrokerId"`
	RuntimeTarget   api.RuntimeTargetDescriptor `json:"runtimeTarget"`
	ExecutionScope  ExecutionScope              `json:"executionScope"`
	CreatedAt       time.Time                   `json:"createdAt"`
}

// InstanceDir returns <globalDir>/runtime-brokers/<key>.
func InstanceDir(globalDir, key string) string {
	return filepath.Join(globalDir, RuntimeBrokersDirName, key)
}

// LoadOrCreate loads the identity of instance key from dir, or mints it on
// first boot. First boot means dir holds nothing but the lock file and
// leftover temp files. Any other missing, corrupt or mismatching state is an
// explicit error; an existing identity is never replaced.
//
// observed is the execution scope probed by the caller; on first boot it is
// recorded (it must have an identity), and on later loads it is verified
// with VerifyScope. legacyBrokerIDs lists every legacy Runtime Broker ID the
// caller can see; the instance ID must differ from all of them.
func LoadOrCreate(dir, key, targetType string, observed ExecutionScope, legacyBrokerIDs []string) (*Identity, error) {
	if !ValidInstanceKey(key) {
		return nil, fmt.Errorf("invalid instance key %q", key)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create instance directory: %w", err)
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	path := filepath.Join(dir, IdentityFileName)
	id, err := readIdentity(path)
	switch {
	case err == nil:
		// Existing identity: verify below.
	case errors.Is(err, os.ErrNotExist):
		firstBoot, ferr := isFirstBoot(dir)
		if ferr != nil {
			return nil, ferr
		}
		if !firstBoot {
			return nil, fmt.Errorf("%w: instance %q (%s); restore %s or remove the directory to register a new Runtime Broker",
				ErrIdentityMissing, key, dir, IdentityFileName)
		}
		id, err = mint(dir, path, key, targetType, observed)
		if err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	if id.InstanceKey != key {
		return nil, fmt.Errorf("%w: file has %q, directory is for %q", ErrIdentityKeyMismatch, id.InstanceKey, key)
	}
	if id.RuntimeTarget.Type != targetType {
		return nil, fmt.Errorf("%w: recorded %q, configured %q; a different target needs a different instance key",
			ErrRuntimeTargetTypeChanged, id.RuntimeTarget.Type, targetType)
	}
	if slices.Contains(legacyBrokerIDs, id.RuntimeBrokerID) {
		return nil, fmt.Errorf("%w: %s", ErrIdentityCollidesWithLegacy, id.RuntimeBrokerID)
	}
	if err := VerifyScope(id.ExecutionScope, observed); err != nil {
		return nil, err
	}
	return id, nil
}

// lockDir takes an exclusive advisory lock on <dir>/.lock.
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock instance directory: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// isFirstBoot reports whether dir holds nothing but the lock file and
// leftover temp files (which it removes).
func isFirstBoot(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read instance directory: %w", err)
	}
	var temps []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == lockFileName:
		case strings.HasPrefix(name, tempFilePrefix) && !e.IsDir():
			temps = append(temps, name)
		default:
			return false, nil
		}
	}
	for _, t := range temps {
		_ = os.Remove(filepath.Join(dir, t))
	}
	return true, nil
}

func mint(dir, path, key, targetType string, observed ExecutionScope) (*Identity, error) {
	if targetType != TargetTypeDocker && targetType != TargetTypeKubernetes {
		return nil, fmt.Errorf("unsupported runtime target type %q", targetType)
	}
	if observed.Type != targetType {
		return nil, fmt.Errorf("%w: observed scope type %q does not match target type %q",
			ErrExecutionScopeUnidentified, observed.Type, targetType)
	}
	if _, err := scopeIdentity(observed); err != nil {
		return nil, err
	}
	id := &Identity{
		SchemaVersion:   SchemaVersion,
		InstanceKey:     key,
		RuntimeBrokerID: uuid.NewString(),
		RuntimeTarget:   api.RuntimeTargetDescriptor{ID: uuid.NewString(), Type: targetType},
		ExecutionScope:  observed,
		CreatedAt:       time.Now().UTC().Truncate(time.Second),
	}
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, tempFilePrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("create identity temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write identity: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("sync identity: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	// Link never replaces an existing file: if another process won, load
	// its identity instead.
	if err := os.Link(tmpName, path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("publish identity: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return readIdentity(path)
}

func readIdentity(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrIdentityCorrupt, err)
	}
	var id Identity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrIdentityCorrupt, path, err)
	}
	if id.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: %s: unknown schemaVersion %d", ErrIdentityCorrupt, path, id.SchemaVersion)
	}
	if id.InstanceKey == "" || id.RuntimeBrokerID == "" || id.RuntimeTarget.ID == "" ||
		id.RuntimeTarget.Type == "" || id.ExecutionScope.Type == "" {
		return nil, fmt.Errorf("%w: %s: a required field is empty", ErrIdentityCorrupt, path)
	}
	if _, err := scopeIdentity(id.ExecutionScope); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrIdentityCorrupt, path, err)
	}
	return &id, nil
}
