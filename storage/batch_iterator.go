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

package storage

import (
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

// SampleBatch is an immutable borrowed view of consecutive samples. Timestamps,
// Values, Histograms and FloatHistograms must either be empty when unused or
// align with Timestamps. Histogram entries are nil for samples of other kinds.
// The view remains valid until the iterator's next Seek or NextBatch call.
// Callers retaining arrays or histograms must copy them first.
type SampleBatch struct {
	Timestamps      []int64
	StartTimestamps []int64
	Values          []float64
	Histograms      []*histogram.Histogram
	FloatHistograms []*histogram.FloatHistogram
}

// Len returns the number of samples in the batch.
func (b SampleBatch) Len() int { return len(b.Timestamps) }

// ValueType returns the sample kind at index i.
func (b SampleBatch) ValueType(i int) chunkenc.ValueType {
	if i < len(b.Histograms) && b.Histograms[i] != nil {
		return chunkenc.ValHistogram
	}
	if i < len(b.FloatHistograms) && b.FloatHistograms[i] != nil {
		return chunkenc.ValFloatHistogram
	}
	return chunkenc.ValFloat
}

// StartTimestamp returns the start timestamp at i, or zero if unknown.
func (b SampleBatch) StartTimestamp(i int) int64 {
	if i < len(b.StartTimestamps) {
		return b.StartTimestamps[i]
	}
	return 0
}

// BatchIterator is an optional columnar counterpart to chunkenc.Iterator.
// NextBatch advances to at most maxSamples samples, exposes them through Batch,
// and scalar iterator methods address the batch's last sample. A zero or
// negative maxSamples may select an implementation-defined complete page.
// Implementations still support ordinary Next, Seek and At methods.
type BatchIterator interface {
	chunkenc.Iterator
	NextBatch(maxSamples int) bool
	Batch() SampleBatch
}

// RangeBatchIterator additionally bounds the next batch by an inclusive maximum
// timestamp. It must leave samples after maxt available for a later call.
// This optional interface supports direct matrix materialization without
// consuming future samples into the current evaluation window.
type RangeBatchIterator interface {
	BatchIterator
	NextBatchUntil(maxSamples int, maxt int64) bool
}
