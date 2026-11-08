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
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sync"

	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/storage"
)

// DefaultFlushSamples is the default number of buffered samples that triggers a
// block flush.
const DefaultFlushSamples = 1000

// StorageOptions configures a Storage.
type StorageOptions struct {
	// FlushSamples is the number of samples buffered in memory before they are
	// written out as a block. Zero selects DefaultFlushSamples.
	FlushSamples int

	// WriteOptions are passed to the block writer.
	WriteOptions WriteOptions
}

// Storage is an experimental columnar storage.
//
// Appends land in an in-memory buffer. Once the buffer holds FlushSamples
// samples it is written out as a block, and the buffer is emptied. Queries see
// the buffer and the blocks together, so a caller never has to know whether data
// has been flushed yet.
//
// There is no WAL: a crash loses the buffered samples, and blocks are never
// rewritten, compacted, or deleted. This is a measurement vehicle, not a
// database.
type Storage struct {
	dir  string
	opts StorageOptions

	mtx       sync.Mutex
	buffer    map[uint64]*headSeries
	bufferN   int
	bufferMin int64
	bufferMax int64
	blocks    []*Block
	// last holds the newest sample of every series, including series whose
	// samples were already flushed to a block. The appender needs it to reject
	// out of order and duplicate samples, and to infer counter reset hints the way
	// the TSDB chunk encoding does.
	last   map[uint64]*seriesState
	closed bool
}

// seriesState is the newest sample of a series.
type seriesState struct {
	labels labels.Labels
	t      int64
	h      *histogram.Histogram
	fh     *histogram.FloatHistogram
}

// headSeries is a series in the in-memory buffer.
type headSeries struct {
	labels  labels.Labels
	samples []headSample
}

// headSample is one buffered sample.
//
// Histograms are encoded on append rather than kept as pointers, because callers
// are allowed to reuse the histogram they passed in, and because it makes the
// flush a copy rather than an encode.
type headSample struct {
	t    int64
	v    float64
	kind SampleKind
	hist []byte
}

// NewStorage opens a columnar storage in dir, adopting any blocks already there.
func NewStorage(dir string, opts StorageOptions) (*Storage, error) {
	if opts.FlushSamples <= 0 {
		opts.FlushSamples = DefaultFlushSamples
	}

	s := &Storage{
		dir:       dir,
		opts:      opts,
		buffer:    map[uint64]*headSeries{},
		last:      map[uint64]*seriesState{},
		bufferMin: math.MaxInt64,
		bufferMax: math.MinInt64,
	}

	blocks, err := ListBlocks(dir)
	switch {
	case err == nil:
		s.blocks = blocks
	case errors.Is(err, ErrNoBlocks):
	default:
		return nil, err
	}
	return s, nil
}

// AppenderV2 returns an appender for the storage.
func (s *Storage) AppenderV2(ctx context.Context) storage.AppenderV2 {
	return s.appender(ctx)
}

// Appender returns an appender for the storage. The v1 interface exists because
// storage.Storage still requires it; both entry points share one implementation.
func (s *Storage) Appender(ctx context.Context) storage.Appender {
	return &appenderV1{appender: s.appender(ctx)}
}

func (s *Storage) appender(_ context.Context) *appender {
	return &appender{
		s:   s,
		txs: map[uint64]*headSeries{},
	}
}

// StartTime returns the oldest timestamp in storage, or math.MinInt64 when empty.
func (s *Storage) StartTime() (int64, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	if s.bufferN > 0 {
		return s.bufferMin, nil
	}
	if len(s.blocks) == 0 {
		return math.MinInt64, nil
	}
	return s.blocks[0].Meta().MinTime, nil
}

// Close releases the storage. Blocks are read on demand, so there is nothing to
// flush on close; buffered samples are discarded, as documented.
func (s *Storage) Close() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true
	return nil
}

// Flush writes the in-memory buffer out as a block.
func (s *Storage) Flush() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.flushLocked()
}

// NumBlocks returns the number of written blocks.
func (s *Storage) NumBlocks() int {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return len(s.blocks)
}

// flushLocked writes the buffer as a block and empties it.
func (s *Storage) flushLocked() error {
	if len(s.buffer) == 0 {
		return nil
	}

	keys := make([]labels.Labels, 0, len(s.buffer))
	for _, hs := range s.buffer {
		keys = append(keys, hs.labels)
	}
	slices.SortFunc(keys, labels.Compare)

	series := make([]Series, 0, len(keys))
	for _, ls := range keys {
		hs := s.buffer[ls.Hash()]
		row := Series{
			Labels:     hs.labels,
			Timestamps: make([]int64, len(hs.samples)),
			Kinds:      make([]SampleKind, len(hs.samples)),
			Values:     make([]float64, len(hs.samples)),
			Histograms: make([][]byte, len(hs.samples)),
		}
		for i, smpl := range hs.samples {
			row.Timestamps[i] = smpl.t
			row.Kinds[i] = smpl.kind
			row.Values[i] = smpl.v
			row.Histograms[i] = smpl.hist
		}
		series = append(series, row)
	}

	meta, err := WriteBlock(s.dir, series, s.opts.WriteOptions)
	if err != nil {
		return err
	}
	block, err := OpenBlock(filepath.Join(s.dir, meta.ULID))
	if err != nil {
		return err
	}

	s.blocks = append(s.blocks, block)
	s.buffer = map[uint64]*headSeries{}
	s.bufferN = 0
	s.bufferMin = math.MaxInt64
	s.bufferMax = math.MinInt64
	return nil
}

// appender buffers appends in a transaction and applies them on Commit.
type appender struct {
	s *Storage

	txs   map[uint64]*headSeries
	order []uint64
	done  bool
}

func (a *appender) Append(_ storage.SeriesRef, ls labels.Labels, st, t int64, v float64, h *histogram.Histogram, fh *histogram.FloatHistogram, _ storage.AOptions) (storage.SeriesRef, error) {
	if a.done {
		return 0, errors.New("columnar: appender used after Commit or Rollback")
	}

	// Start timestamps are not implemented. Silently accepting them would change
	// what query results mean, so refuse instead.
	if st != 0 {
		return 0, fmt.Errorf("%w: start timestamps", errUnsupported)
	}

	hs, err := a.seriesFor(ls)
	if err != nil {
		return 0, err
	}

	prev, hasPrev := a.previousLocked(ls, hs)
	if hasPrev {
		switch {
		case t == prev.t:
			return 0, storage.ErrDuplicateSampleForTimestamp
		case t < prev.t:
			return 0, storage.ErrOutOfOrderSample
		}
	}

	smpl := headSample{t: t, v: v, kind: SampleFloat}
	if value.IsStaleNaN(v) {
		// Record staleness explicitly; see SampleStale.
		smpl.kind = SampleStale
	}
	if h != nil || fh != nil {
		h, fh = withInferredResetHint(prev, h, fh)
		encoded, err := encodeHistogram(h, fh)
		if err != nil {
			return 0, err
		}
		smpl.kind = SampleFloatHistogram
		if h != nil {
			smpl.kind = SampleHistogram
		}
		smpl.hist = encoded
	}

	hs.samples = append(hs.samples, smpl)
	return refFor(ls), nil
}

// withInferredResetHint fills in a counter reset hint when the caller did not
// provide one, matching what the TSDB chunk encoding does on append.
//
// The hint is not cosmetic: PromQL aggregation and rate() derive results and
// warnings from it, so leaving every hint at unknown changes query results. Only
// counter-like histograms get an inferred hint; gauge histograms keep theirs.
func withInferredResetHint(prev seriesState, h *histogram.Histogram, fh *histogram.FloatHistogram) (*histogram.Histogram, *histogram.FloatHistogram) {
	var hint histogram.CounterResetHint
	if fh != nil {
		hint = fh.CounterResetHint
	} else {
		hint = h.CounterResetHint
	}
	if hint != histogram.UnknownCounterReset {
		return h, fh
	}

	var prevFH *histogram.FloatHistogram
	switch {
	case prev.fh != nil:
		prevFH = prev.fh
	case prev.h != nil:
		prevFH = prev.h.ToFloat(nil)
	default:
		// No previous histogram to compare against, so no inference possible.
		return h, fh
	}

	// Compare as float histograms: DetectReset only needs relative counts, and
	// converting avoids implementing the same check twice.
	var curFH *histogram.FloatHistogram
	if fh != nil {
		curFH = fh
	} else {
		curFH = h.ToFloat(nil)
	}
	if curFH.DetectReset(prevFH) {
		hint = histogram.CounterReset
	} else {
		hint = histogram.NotCounterReset
	}

	if fh != nil {
		// Copy before mutating: the caller owns the histogram it passed in.
		cp := fh.Copy()
		cp.CounterResetHint = hint
		return nil, cp
	}
	cp := h.Copy()
	cp.CounterResetHint = hint
	return cp, nil
}

// previousLocked returns the newest sample of ls, looking in the transaction first
// and then in the committed state.
func (a *appender) previousLocked(ls labels.Labels, tx *headSeries) (seriesState, bool) {
	if len(tx.samples) > 0 {
		last := tx.samples[len(tx.samples)-1]
		state := seriesState{labels: ls, t: last.t}
		if last.kind != SampleFloat {
			h, fh, err := last.decode()
			if err == nil {
				state.h, state.fh = h, fh
			}
		}
		return state, true
	}
	if st, ok := a.s.last[ls.Hash()]; ok {
		return *st, true
	}
	return seriesState{}, false
}

// decode returns the histograms of the sample.
func (s headSample) decode() (*histogram.Histogram, *histogram.FloatHistogram, error) {
	var hp prompb.Histogram
	if err := hp.Unmarshal(s.hist); err != nil {
		return nil, nil, err
	}
	if hp.IsFloatHistogram() {
		return nil, hp.ToFloatHistogram(), nil
	}
	return hp.ToIntHistogram(), nil, nil
}

// seriesFor returns the transaction-local series for ls, creating it if needed.
func (a *appender) seriesFor(ls labels.Labels) (*headSeries, error) {
	hash := ls.Hash()
	if hs, ok := a.txs[hash]; ok {
		return hs, nil
	}
	hs := &headSeries{labels: ls}
	a.txs[hash] = hs
	a.order = append(a.order, hash)
	return hs, nil
}

// refFor derives a stable series reference from the labels.
func refFor(ls labels.Labels) storage.SeriesRef {
	// Zero means "unknown" to callers, so it must not be returned.
	if ref := storage.SeriesRef(ls.Hash()); ref != 0 {
		return ref
	}
	return 1
}

// Commit applies the transaction to the buffer, flushing a block when the buffer
// is large enough.
func (a *appender) Commit() error {
	if a.done {
		return errors.New("columnar: appender used after Commit or Rollback")
	}
	a.done = true

	s := a.s
	s.mtx.Lock()
	defer s.mtx.Unlock()

	if s.closed {
		return errors.New("columnar: storage is closed")
	}

	for _, hash := range a.order {
		tx := a.txs[hash]
		if len(tx.samples) == 0 {
			continue
		}
		hs, ok := s.buffer[hash]
		if !ok {
			hs = &headSeries{labels: tx.labels}
			s.buffer[hash] = hs
		}
		hs.samples = append(hs.samples, tx.samples...)
		s.bufferN += len(tx.samples)
		if last := tx.samples[len(tx.samples)-1]; true {
			state := &seriesState{labels: tx.labels, t: last.t}
			if last.kind != SampleFloat {
				if h, fh, err := last.decode(); err == nil {
					state.h, state.fh = h, fh
				}
			}
			s.last[hash] = state
		}
		for _, smpl := range tx.samples {
			s.bufferMin = min(s.bufferMin, smpl.t)
			s.bufferMax = max(s.bufferMax, smpl.t)
		}
	}

	if s.bufferN >= s.opts.FlushSamples {
		return s.flushLocked()
	}
	return nil
}

// Rollback discards the transaction.
func (a *appender) Rollback() error {
	if a.done {
		return errors.New("columnar: appender used after Commit or Rollback")
	}
	a.done = true
	a.txs = nil
	a.order = nil
	return nil
}

// errUnsupported marks functionality this prototype deliberately does not
// implement.
var errUnsupported = errors.New("columnar: unsupported by prototype")

// AppendHistogram adds a histogram sample. It exists for the v1 appender
// interface; AppenderV2 callers pass histograms to Append.
func (a *appender) AppendHistogram(ref storage.SeriesRef, ls labels.Labels, t int64, h *histogram.Histogram, fh *histogram.FloatHistogram) (storage.SeriesRef, error) {
	return a.Append(ref, ls, 0, t, 0, h, fh, storage.AOptions{})
}

// AppendHistogramSTZeroSample is not implemented.
func (*appender) AppendHistogramSTZeroSample(storage.SeriesRef, labels.Labels, int64, int64, *histogram.Histogram, *histogram.FloatHistogram) (storage.SeriesRef, error) {
	return 0, fmt.Errorf("%w: histogram start timestamp zero samples", errUnsupported)
}

// AppendSTZeroSample is not implemented.
func (*appender) AppendSTZeroSample(_ storage.SeriesRef, _ labels.Labels, _, _ int64) (storage.SeriesRef, error) {
	return 0, fmt.Errorf("%w: start timestamp zero samples", errUnsupported)
}

// AppendExemplar accepts and drops exemplars: the block format has no exemplar
// column yet, and failing the append would fail every scrape that carries one.
func (a *appender) AppendExemplar(_ storage.SeriesRef, ls labels.Labels, _ exemplar.Exemplar) (storage.SeriesRef, error) {
	if a.done {
		return 0, errors.New("columnar: appender used after Commit or Rollback")
	}
	return refFor(ls), nil
}

// UpdateMetadata accepts and drops metadata, for the same reason as exemplars.
func (a *appender) UpdateMetadata(_ storage.SeriesRef, ls labels.Labels, _ metadata.Metadata) (storage.SeriesRef, error) {
	if a.done {
		return 0, errors.New("columnar: appender used after Commit or Rollback")
	}
	return refFor(ls), nil
}

// SetOptions is a no-op: this prototype has no out-of-order window.
func (*appender) SetOptions(*storage.AppendOptions) {}

// appenderV1 adapts the appender to the v1 interface, which storage.Storage still
// requires. The v1 Append signature has no start timestamp and no histogram
// parameters, so it forwards floats only.
type appenderV1 struct {
	*appender
}

func (a *appenderV1) Append(_ storage.SeriesRef, ls labels.Labels, t int64, v float64) (storage.SeriesRef, error) {
	return a.appender.Append(0, ls, 0, t, v, nil, nil, storage.AOptions{})
}
