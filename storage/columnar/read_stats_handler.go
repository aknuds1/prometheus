// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package columnar

import (
	"encoding/json"
	"net/http"
)

// NewReadStatsHandler reports the read counters as JSON. It exists so a cost
// model can be taken from a running server by differencing two samples around a
// burst of queries; scraping /metrics would add series to the corpus under test.
//
// This is a prototype debugging surface, not a stable API.
func NewReadStatsHandler(storage *Storage) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		stats := storage.ReadStats()
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(stats); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}
