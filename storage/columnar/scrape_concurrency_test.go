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

package columnar

import (
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
)

func TestConcurrentScalarAppendersShareCommittedTail(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 1_000_000, DisableWAL: true, DisableNativeMetadata: true})
	require.NoError(t, err)
	defer s.Close()

	const workers, samples = 64, 20
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Go(func() {
			ls := labels.FromStrings(labels.MetricName, "concurrent_scrape", "target", strconv.Itoa(worker))
			app := s.AppenderV2(t.Context())
			for sample := range samples {
				_, err := app.Append(0, ls, 0, int64(sample*1000), float64(sample), nil, nil, storage.AOptions{})
				if err != nil {
					errs <- err
					return
				}
			}
			errs <- app.Commit()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	selected, err := s.SelectSeries(t.Context(), 0, int64(samples*1000), nil, 0)
	require.NoError(t, err)
	require.Len(t, selected, workers)
	for _, item := range selected {
		require.Equal(t, samples, item.Len())
	}
}
