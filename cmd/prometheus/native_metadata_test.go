// Copyright The Prometheus Authors
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

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/agent"
)

func TestReadyStorageLookupNativeMetricMetadata(t *testing.T) {
	for _, mode := range []string{"not ready", "agent", "disabled", "enabled"} {
		t.Run(mode, func(t *testing.T) {
			s := &readyStorage{}
			var ref storage.SeriesRef
			switch mode {
			case "agent":
				s.Set(&agent.DB{}, 0)
			case "disabled", "enabled":
				opts := tsdb.DefaultOptions()
				opts.EnableNativeMetadata = mode == "enabled"
				db, err := tsdb.Open(t.TempDir(), nil, nil, opts, nil)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				s.Set(db, 0)
				app := db.AppenderV2(t.Context())
				ref, err = app.Append(0, labels.FromStrings(labels.MetricName, "metric"), 0, 100, 1, nil, nil, storage.AOptions{Metadata: metadata.Metadata{Help: "native"}})
				require.NoError(t, err)
				require.NoError(t, app.Commit())
			}
			lookups := []storage.NativeMetricMetadataLookup{{Ref: ref, Timestamp: 100, Metadata: &metadata.Metadata{Help: "stale"}}}
			require.NoError(t, s.LookupNativeMetricMetadata(t.Context(), lookups))
			if mode == "enabled" {
				require.Equal(t, "native", lookups[0].Metadata.Help)
			} else {
				require.Nil(t, lookups[0].Metadata)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			require.ErrorIs(t, s.LookupNativeMetricMetadata(ctx, lookups), context.Canceled)
		})
	}
}
