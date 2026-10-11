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

package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(data)
}

// readJSON reads JSON from request body.
func readJSON(r *http.Request, v interface{}) error {
	if r.Body == nil {
		return fmt.Errorf("empty request body")
	}
	return json.NewDecoder(r.Body).Decode(v)
}
