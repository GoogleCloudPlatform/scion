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

package config

import (
	"errors"
	"fmt"
	"strings"
)

// ErrSettingNotWritable is matched (errors.Is) by every error that
// ValidateSettingWrite and ValidateConfigSetKey return. A key rejected
// this way would otherwise be accepted and then never read at the layer
// it was written to.
var ErrSettingNotWritable = errors.New("setting cannot be written")

// SettingNotWritableError reports a settings key that cannot be written at
// the requested layer, and why. It never carries the value being written,
// so it is safe to print for credential keys.
type SettingNotWritableError struct {
	Key    string
	Reason string
}

func (e *SettingNotWritableError) Error() string {
	return fmt.Sprintf("setting %q cannot be written: %s", e.Key, e.Reason)
}

// Is makes errors.Is(err, ErrSettingNotWritable) true.
func (e *SettingNotWritableError) Is(target error) bool {
	return target == ErrSettingNotWritable
}

// Reasons for the keys that have no place in a v1 settings file. The v1
// structs have no field for them (V1HubClientConfig has no token, apiKey or
// lastSyncedAt; VersionedSettings has no bucket section), so a write could
// only ever be dropped.
const (
	reasonHubCredential = `hub credentials are not stored in settings files; use "scion hub auth login" to store them`
	reasonHubSyncState  = "sync state is not stored in settings files; scion records it itself"
	reasonNoBucket      = "settings files have no bucket section"
	reasonGlobalOnly    = "it is read only from the global settings file; use --global to write it there"
	reasonNotConfigSet  = `it is not supported by "config set"`
)

// settingsNotStoredKeys are rejected at every layer.
var settingsNotStoredKeys = map[string]string{
	"hub.token":        reasonHubCredential,
	"hub.apiKey":       reasonHubCredential,
	"hub.lastSyncedAt": reasonHubSyncState,
	"bucket.provider":  reasonNoBucket,
	"bucket.name":      reasonNoBucket,
	"bucket.prefix":    reasonNoBucket,
}

// settingsGlobalOnlyPrefixes are rejected at the project layer. server.auth
// (the workstation dev-auth identity) is read from the global settings file
// only: by the hub's onboarding status (computeOnboardingStatus in
// pkg/hub/system_handlers.go) and by server startup (LoadGlobalConfig,
// which reads a file other than the global one only when an explicit
// --config path names it). A project's own copy is never read.
var settingsGlobalOnlyPrefixes = []string{"server.auth."}

// ValidateSettingWrite reports whether key may be written to a settings
// file at the given layer (global true for the global settings file, false
// for a project's own settings). It does no I/O, so UpdateSetting calls it
// before any read, lock or legacy-format migration: a rejected key leaves
// the file exactly as it was. The returned error matches
// ErrSettingNotWritable and does not contain the value.
//
// It covers keys whose write would be accepted and then never read at
// that layer, and the global-only keys the writer does not know (refused
// as unknown at either layer, so no layer hint points at a key no layer
// accepts). Other keys the writer does not know are still rejected later
// as unknown.
func ValidateSettingWrite(key string, global bool) error {
	if reason, ok := settingsNotStoredKeys[key]; ok {
		return &SettingNotWritableError{Key: key, Reason: reason}
	}
	for _, p := range settingsGlobalOnlyPrefixes {
		if !strings.HasPrefix(key, p) {
			continue
		}
		// The hint to use --global is only for a key the writer accepts
		// there. Any other key under the prefix is unknown at both layers,
		// and is refused as such here, before any I/O (ptone/scion#5481).
		if _, ok := versionedSettingKeys[key]; !ok {
			return fmt.Errorf("unknown or complex setting key: %s", key)
		}
		if !global {
			return &SettingNotWritableError{Key: key, Reason: reasonGlobalOnly}
		}
	}
	return nil
}

// ValidateConfigSetKey is ValidateSettingWrite plus the keys the
// "config set" command does not write even though internal callers may
// pass them to UpdateSetting (hub_connections.*, which UpdateSetting
// accepts and leaves unchanged).
func ValidateConfigSetKey(key string, global bool) error {
	if err := ValidateSettingWrite(key, global); err != nil {
		return err
	}
	if strings.HasPrefix(key, "hub_connections.") {
		return &SettingNotWritableError{Key: key, Reason: reasonNotConfigSet}
	}
	return nil
}

// IsSecretSettingKey reports whether a settings key names a credential
// (token, secret, password or API key), whose value must not be printed.
func IsSecretSettingKey(key string) bool {
	return isSecretKey(key)
}

// ValidateSettingValue rejects a value a setting does not accept, before any
// settings file is read or written. An empty value (which clears the key) is
// always accepted.
func ValidateSettingValue(key, value string) error {
	if value == "" {
		return nil
	}
	switch key {
	case "cli.mode":
		switch value {
		case "human", "agent":
			return nil
		case "assistant":
			return fmt.Errorf("cli.mode %q: the assistant mode has been removed; use human or agent", value)
		default:
			return fmt.Errorf("cli.mode %q: must be human or agent", value)
		}
	}
	return nil
}
