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
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/prometheus/prometheus/model/labels"
)

// SampleSegment is one immutable series/time range. Its binary columns encode
// independently but share Count and timestamp bounds. Histogram payloads retain
// exact Prometheus protobuf values until decoded by the sample-kind iterator.
type SampleSegment struct {
	SeriesHash         uint64
	MinTime, MaxTime   int64
	MinValue, MaxValue float64
	Count              int

	timestampCodec integerCodec
	valueCodec     floatCodec
	kindCodec      integerCodec
	startCodec     integerCodec
	timestamps     []byte
	values         []byte
	kinds          []byte
	startTimes     []byte
	histograms     [][]byte
}

// TypedPage is a whole segment decoded into parallel typed arrays. A nil
// StartTimestamps or Histograms column means the entire optional column is
// respectively unknown/empty.
type TypedPage struct {
	Labels          labels.Labels
	Timestamps      []int64
	StartTimestamps []int64
	Kinds           []SampleKind
	Values          []float64
	Histograms      [][]byte

	kindScratch []int64
}

func (p TypedPage) Len() int { return len(p.Timestamps) }

// Validate checks alignment and point types, mirroring the block Series contract.
func (p TypedPage) Validate() error {
	return NewTypedPageSeries(p).Validate()
}

// NewSampleSegment validates and seals one typed series slice. Input timestamps
// are strictly increasing and no input array is modified by code selection.
func NewSampleSegment(ls labels.Labels, timestamps, startTimestamps []int64, kinds []SampleKind, values []float64, histograms [][]byte) (SampleSegment, error) {
	series := Series{Labels: ls, Timestamps: timestamps, StartTimestamps: startTimestamps, Kinds: kinds, Values: values, Histograms: histograms}
	if err := series.Validate(); err != nil {
		return SampleSegment{}, err
	}
	segment := SampleSegment{
		SeriesHash: stableSeriesHash(ls), Count: len(timestamps),
		histograms: cloneByteSlices(histograms),
	}
	if !slices.Contains(kinds, SampleHistogram) && !slices.Contains(kinds, SampleFloatHistogram) {
		segment.histograms = nil
	}
	if len(timestamps) == 0 {
		return segment, nil
	}
	segment.MinTime, segment.MaxTime = timestamps[0], timestamps[len(timestamps)-1]
	segment.MinValue, segment.MaxValue = values[0], values[0]
	for _, value := range values[1:] {
		segment.MinValue = min(segment.MinValue, value)
		segment.MaxValue = max(segment.MaxValue, value)
	}
	var err error
	if segment.timestamps, segment.timestampCodec, err = encodeTimestamps(timestamps); err != nil {
		return SampleSegment{}, err
	}
	if segment.values, segment.valueCodec, err = encodeFloatColumn(values); err != nil {
		return SampleSegment{}, err
	}
	allFloat, unknownStart := true, true
	for i, kind := range kinds {
		if kind < SampleFloat || kind > SampleStale {
			return SampleSegment{}, fmt.Errorf("unknown sample kind %d", kind)
		}
		allFloat = allFloat && kind == SampleFloat
		unknownStart = unknownStart && (len(startTimestamps) == 0 || startTimestamps[i] == 0)
	}
	if allFloat {
		segment.kindCodec = kindUniformFloat
	} else {
		integerKinds := make([]int64, len(kinds))
		for i, kind := range kinds {
			integerKinds[i] = int64(kind)
		}
		segment.kinds, segment.kindCodec, err = encodeIntegerColumn(integerKinds)
	}
	if err != nil {
		return SampleSegment{}, err
	}
	if unknownStart {
		segment.startTimes = nil
	} else {
		encoded, codec, encodeErr := encodeIntegerColumn(startTimestamps)
		if encodeErr != nil {
			return SampleSegment{}, encodeErr
		}
		segment.startTimes, segment.startCodec = encoded, codec
	}
	return segment, nil
}

// EncodedSize is encoded column payload size, excluding labels and Parquet
// record/framing overhead.
func (s SampleSegment) EncodedSize() int {
	size := len(s.timestamps) + len(s.values) + len(s.kinds) + len(s.startTimes)
	for _, payload := range s.histograms {
		size += len(payload)
	}
	return size
}

// DecodeInto decodes every column into page. Existing buffers may be reused when
// their capacities suffice. Values and start timestamps are fully overwritten,
// and validation occurs at seal time or on enclosing block-Series assembly rather
// than redundantly for every page.
func (s SampleSegment) DecodeInto(ls labels.Labels, page *TypedPage) error {
	if page == nil {
		return errors.New("columnar: nil page destination")
	}
	var err error
	page.Labels = ls
	if page.Timestamps, err = decodeIntegerColumnInto(s.timestamps, s.Count, s.timestampCodec, page.Timestamps); err != nil {
		return fmt.Errorf("decoding timestamps: %w", err)
	}
	if len(s.startTimes) > 0 {
		page.StartTimestamps, err = decodeIntegerColumnInto(s.startTimes, s.Count, s.startCodec, page.StartTimestamps)
		if err != nil {
			return fmt.Errorf("decoding start timestamps: %w", err)
		}
	} else {
		page.StartTimestamps = nil
	}
	if page.Values, err = decodeFloatColumnInto(s.values, s.Count, s.valueCodec, page.Values); err != nil {
		return fmt.Errorf("decoding values: %w", err)
	}
	page.Kinds = reuseKinds(page.Kinds, s.Count)
	if s.kindCodec == kindUniformFloat {
		clear(page.Kinds)
	} else {
		if page.kindScratch, err = decodeIntegerColumnInto(s.kinds, s.Count, s.kindCodec, page.kindScratch); err != nil {
			return fmt.Errorf("decoding sample kinds: %w", err)
		}
		for i, kind := range page.kindScratch {
			if kind < 0 || kind > int64(SampleStale) {
				return fmt.Errorf("invalid sample kind %d", kind)
			}
			page.Kinds[i] = SampleKind(kind)
		}
	}
	page.Histograms = cloneByteSlices(s.histograms)
	return nil
}

// Decode returns an independent typed page. Reuse-oriented callers can use a
// PageIterator or call DecodeInto with retained buffers.
func (s SampleSegment) Decode(ls labels.Labels) (TypedPage, error) {
	var page TypedPage
	if err := s.DecodeInto(ls, &page); err != nil {
		return TypedPage{}, err
	}
	return page, nil
}

// PageIterator decodes segments beside chunkenc.Iterator-style scalar iteration.
// A returned page is borrowed until the iterator's next Next/Close call. Callers
// retaining or mutating it after that boundary must copy it first.
type PageIterator interface {
	Next() bool
	Page() TypedPage
	Err() error
	// Close releases reusable buffers. The page must not be used afterwards.
	Close() error
}

type segmentPageIterator struct {
	segment SampleSegment
	labels  labels.Labels
	page    *TypedPage
	err     error
	done    bool
}

// Reuse belongs to the decoded page pool, not to shared sample contents. Each
// borrowed page exists until Close, and no two outstanding iterators share one.
var segmentPagePool = sync.Pool{New: func() any { return &TypedPage{} }}

func (s SampleSegment) NewPageIterator(ls labels.Labels) PageIterator {
	page := segmentPagePool.Get().(*TypedPage)
	return &segmentPageIterator{segment: s, labels: ls, page: page}
}

func (it *segmentPageIterator) Next() bool {
	if it.done {
		return false
	}
	it.done = true
	it.err = it.segment.DecodeInto(it.labels, it.page)
	return it.err == nil
}

func (it *segmentPageIterator) Page() TypedPage {
	if it.page == nil {
		return TypedPage{}
	}
	return *it.page
}

func (it *segmentPageIterator) Err() error { return it.err }

func (it *segmentPageIterator) Close() error {
	if it.page != nil {
		it.page.Labels = labels.EmptyLabels()
		segmentPagePool.Put(it.page)
		it.page = nil
	}
	return nil
}

func cloneByteSlices(input [][]byte) [][]byte {
	if input == nil {
		return nil
	}
	out := make([][]byte, len(input))
	for i, payload := range input {
		out[i] = slices.Clone(payload)
	}
	return out
}

func reuseKinds(destination []SampleKind, count int) []SampleKind {
	if cap(destination) >= count {
		return destination[:count]
	}
	return make([]SampleKind, count)
}

// NewTypedPageSeries exposes the transition from a decoded page to the existing
// storage.Series boundary using the same arrays rather than per-sample encoding.
func NewTypedPageSeries(page TypedPage) Series {
	return Series{
		Labels: page.Labels, Timestamps: page.Timestamps, StartTimestamps: page.StartTimestamps,
		Kinds: page.Kinds, Values: page.Values, Histograms: page.Histograms,
	}
}
