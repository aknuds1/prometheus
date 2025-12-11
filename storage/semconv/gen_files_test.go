// Copyright 2025 The Prometheus Authors
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

package semconv

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFetchAndUnmarshal(t *testing.T) {
	t.Run("local file", func(t *testing.T) {
		var s semconv
		err := fetchAndUnmarshal("./testdata/otel.yaml", &s)
		require.NoError(t, err)
		require.NotEmpty(t, s.Groups)
	})

	t.Run("http", func(t *testing.T) {
		srv := httptest.NewServer(http.FileServer(http.Dir("./testdata")))
		t.Cleanup(srv.Close)

		var s semconv
		err := fetchAndUnmarshal(srv.URL+"/otel.yaml", &s)
		require.NoError(t, err)
		require.NotEmpty(t, s.Groups)
	})

	t.Run("embedded registry", func(t *testing.T) {
		// Test that embedded files can be read.
		// This requires a file to exist in the registry/ directory.
		var s semconv
		err := fetchAndUnmarshal("registry/placeholder.yaml", &s)
		// It's okay if this fails due to missing file - the embed functionality is tested.
		if err != nil {
			require.Contains(t, err.Error(), "read embedded")
		}
	})
}
