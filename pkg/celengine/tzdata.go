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

package celengine

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"sync"
	"time"
)

//go:embed tzdata/zoneinfo.zip
var embeddedZoneinfoZip []byte

var (
	embeddedTZOnce  sync.Once
	embeddedTZBytes map[string][]byte
	embeddedTZErr   error
)

func initEmbeddedTZData() {
	zr, err := zip.NewReader(bytes.NewReader(embeddedZoneinfoZip), int64(len(embeddedZoneinfoZip)))
	if err != nil {
		embeddedTZErr = fmt.Errorf("celengine: parse embedded zoneinfo archive: %w", err)
		return
	}
	index := make(map[string][]byte, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			embeddedTZErr = fmt.Errorf("celengine: open embedded tz entry %q: %w", f.Name, err)
			return
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			embeddedTZErr = fmt.Errorf("celengine: read embedded tz entry %q: %w", f.Name, err)
			return
		}
		index[f.Name] = raw
	}
	embeddedTZBytes = index
}

// loadEmbeddedLocation resolves an IANA timezone name strictly from the
// embedded zoneinfo archive via time.LoadLocationFromTZData, without consulting
// $ZONEINFO, /usr/share/zoneinfo, or any host filesystem path.
func loadEmbeddedLocation(tz string) (*time.Location, error) {
	embeddedTZOnce.Do(initEmbeddedTZData)
	if embeddedTZErr != nil {
		return nil, embeddedTZErr
	}
	raw, ok := embeddedTZBytes[tz]
	if !ok {
		return nil, fmt.Errorf("unknown time zone %s", truncateTZForErr(tz))
	}
	return time.LoadLocationFromTZData(tz, raw)
}
