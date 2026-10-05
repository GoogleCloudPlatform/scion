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
	"sync"
)

// settingsFileMu serialises read-modify-write cycles on settings files
// within this process (ptone/scion#3047, in-process part). It is one mutex
// for every settings file rather than one per path: settings.yaml,
// settings.yml and symlinks can name the same file, and writes are rare
// enough that a single lock costs nothing.
//
// It does not coordinate with other processes (a CLI command writing the
// same file while the server runs); that part of ptone/scion#3047 remains open.
var settingsFileMu sync.Mutex

// LockSettingsFile takes the process-wide settings-file lock and returns the
// function that releases it. Hold it from the read of a settings file to the
// write (rename) of its replacement, so a concurrent writer's change is not
// lost.
//
// UpdateVersionedSetting and SaveVersionedSettings take it themselves, so a
// caller must not hold it when calling them (the lock is not re-entrant).
//
// Lock order: the settings-file lock is taken before any database write and
// may be held across database writes (the workstation server-config PUT
// holds it from reading settings.yaml, across its hub_settings writes, to
// the rename). Code holding a database transaction, or the hub server's
// s.mu, must therefore never take the settings-file lock.
func LockSettingsFile() (unlock func()) {
	settingsFileMu.Lock()
	return settingsFileMu.Unlock
}

// ErrSkipSave, returned by a LoadModifySaveVersionedSettings modify
// function, ends the cycle without writing and without error.
var ErrSkipSave = errors.New("settings: nothing to save")

// LoadModifySaveVersionedSettings loads the settings file in dir, applies
// modify, and saves the result, holding the settings-file lock from the
// read to the write so a concurrent in-process writer's change is not lost
// by the whole-struct rewrite. modify may return ErrSkipSave to write
// nothing. It must not call UpdateVersionedSetting, SaveVersionedSettings
// or LoadModifySaveVersionedSettings (the lock is not re-entrant).
//
// The save is the struct round-trip SaveVersionedSettings makes, so
// comments and unknown keys in the file are not kept; prefer
// UpdateVersionedSetting or PrepareSettingsPathEdits for single keys.
func LoadModifySaveVersionedSettings(dir string, modify func(*VersionedSettings) error) error {
	unlock := LockSettingsFile()
	defer unlock()
	vs, err := LoadSingleFileVersioned(dir)
	if err != nil {
		return err
	}
	if err := modify(vs); err != nil {
		if errors.Is(err, ErrSkipSave) {
			return nil
		}
		return err
	}
	return writeVersionedSettingsFile(dir, newSettingsFilePath(dir), vs)
}
