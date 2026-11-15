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
	"time"

	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
)

// DefaultFlushSamples is the default number of buffered samples triggering a
// block flush.
//
// The flush unit is the entire head, so the threshold must comfortably exceed
// the samples ingested by one scrape round. A single scrape of a few thousand
// targets produces thousands of samples; a smaller threshold would emit one
// block per scrape, fragmenting storage into hundreds of tiny blocks and forcing
// every query to open all of them. 200k samples is roughly a few minutes of
// ingestion at that scale and keeps blocks meaningfully sized.
const (
	DefaultFlushSamples = 200_000
	// DefaultMetadataVersionCap bounds immutable and replayed owner histories.
	DefaultMetadataVersionCap = 5
)

// StorageOptions configures a Storage.
type StorageOptions struct {
	// FlushSamples is the number of samples buffered in memory before they are
	// written as a block. Zero selects DefaultFlushSamples.
	FlushSamples int

	// MetadataVersionCap bounds history per owner. Zero selects
	// DefaultMetadataVersionCap. A negative value deliberately removes the cap
	// for churn experiments and must not be used as a durable default.
	MetadataVersionCap int

	// EnableQueryTimings attributes query time to phases. It is a measurement
	// switch, not a behaviour change, and is off by default.
	EnableQueryTimings bool

	// DisableWAL leaves unflushed batches without a segment log and creates no
	// WAL directory. It is intended for explicit experiments, not safe defaults.
	DisableWAL bool
	// DisableNativeMetadata leaves the metadata catalog uninitialized. Sample-only
	// batches still work; metadata associations and owner updates return an error.
	DisableNativeMetadata bool

	// IndexedResourceAttributes selects descriptive Resource attributes for the
	// inverted index. Nil adopts configured attributes from existing blocks;
	// otherwise only this set plus identifying attributes are indexed. An explicit
	// empty slice disables inherited descriptive indexes.
	IndexedResourceAttributes []string

	// WriteOptions are passed to the block writer.
	WriteOptions WriteOptions
}

// Storage is an experimental columnar storage.
//
// Samples enter in typed per-series arrays, and metadata enters the shared
// owner/version/membership store. WAL segments replay unflushed batches and
// immutable blocks restore committed state. Compaction, retention and OOO sample
// mutation remain experiments, not production promises.
type Storage struct {
	dir  string
	opts StorageOptions

	mtx sync.Mutex
	// buffer holds buffered series by stable hash. A map keeps insertion O(1);
	// flushLocked sorts once at seal time instead of keeping an array ordered.
	buffer map[uint64]*headSeries
	// headByName narrows a head selection to the series carrying one metric name,
	// so a selector with an equality __name__ matcher does not walk every buffered
	// series. It is guarded by mtx and reset with the buffer.
	headByName    map[string][]*headSeries
	bufferN       int
	bufferMin     int64
	bufferMax     int64
	minValidTime  int64
	blocks        []*Block
	retiredBlocks []*Block
	last          map[uint64]*seriesState
	// seriesLabels deduplicates immutable label storage across blocks and
	// recovered/appended samples. Every reuse checks complete labels.
	seriesLabels map[uint64]labels.Labels
	closed       bool

	metadata          *metadataStore
	metadataPublished *metadataRelations
	metadataDirty     bool

	walSequence       uint64
	walSeg            *walSegment
	walClosedSegments []walClosedSegment
	replaying         bool
	reads             *readStats
}

// headSeries holds samples as typed columns. Histograms must be encoded before
// entering the head: the caller is allowed to reuse the source histogram.
type headSeries struct {
	hash   uint64
	labels labels.Labels
	ts     []int64
	st     []int64
	v      []float64
	kind   []SampleKind
	hist   [][]byte
}

// seriesState is the newest sample of a series, needed for immediate ordering
// detection and counter reset-hint inference without re-encoding prior samples.
type seriesState struct {
	labels labels.Labels
	t      int64
	st     int64
	count  int
	h      *histogram.Histogram
	fh     *histogram.FloatHistogram
}

// NewStorage opens a columnar storage in dir, adopting existing blocks.
func NewStorage(dir string, opts StorageOptions) (*Storage, error) {
	if opts.FlushSamples <= 0 {
		opts.FlushSamples = DefaultFlushSamples
	}
	if opts.MetadataVersionCap == 0 {
		opts.MetadataVersionCap = DefaultMetadataVersionCap
	} else if opts.MetadataVersionCap < 0 {
		opts.MetadataVersionCap = 0
	}
	s := &Storage{
		dir:          dir,
		opts:         opts,
		last:         map[uint64]*seriesState{},
		buffer:       map[uint64]*headSeries{},
		headByName:   map[string][]*headSeries{},
		seriesLabels: map[uint64]labels.Labels{},
		bufferMin:    math.MaxInt64,
		bufferMax:    math.MinInt64,
		minValidTime: math.MinInt64,
		reads:        &readStats{timings: &queryTimings{}},
	}
	s.reads.timings.enabled.Store(opts.EnableQueryTimings)
	if !opts.DisableNativeMetadata {
		s.metadata = newMetadataStore(opts.MetadataVersionCap)
		s.metadata.reads = s.reads
	}

	blocks, err := ListBlocks(dir)
	switch {
	case err == nil:
		s.blocks = blocks
	case errors.Is(err, ErrNoBlocks):
	default:
		return nil, err
	}
	inheritedAttributes := []string(nil)
	hasExplicitIndexConfig := false
	for _, block := range s.blocks {
		block.reads = s.reads
		if err := s.reuseBlockLabels(block); err != nil {
			return nil, fmt.Errorf("interning series labels from block %s: %w", block.meta.ULID, err)
		}
		if err := s.restoreSeriesTail(block); err != nil {
			return nil, fmt.Errorf("recovering samples from block %s: %w", block.meta.ULID, err)
		}
		if s.metadata != nil && block.meta.Metadata != nil {
			relations, err := block.readMetadataRelations(context.Background())
			if err != nil {
				return nil, fmt.Errorf("recovering metadata from block %s: %w", block.meta.ULID, err)
			}
			if err := s.metadata.restoreRelations(relations); err != nil {
				return nil, fmt.Errorf("reconciling metadata from block %s: %w", block.meta.ULID, err)
			}
		}
		s.walSequence = max(s.walSequence, block.meta.WALSequence)
		if opts.IndexedResourceAttributes == nil && block.meta.Metadata != nil && block.meta.Metadata.IndexedAttributes != nil {
			inheritedAttributes = slices.Clone(block.meta.Metadata.IndexedAttributes)
			hasExplicitIndexConfig = true
		}
	}
	if s.metadata != nil {
		attributes := opts.IndexedResourceAttributes
		if hasExplicitIndexConfig {
			attributes = inheritedAttributes
		}
		s.setIndexedResourceAttrsLocked(attributes)
		published := s.metadata.snapshot()
		s.metadataPublished = &published
	}
	if !opts.DisableWAL {
		s.replaying = true
		err = s.replayWAL(context.Background())
		s.replaying = false
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Storage) AppenderV2(context.Context) storage.AppenderV2 {
	return s.appender()
}

// Appender implements the older storage interface through a single V2 adapter.
func (s *Storage) Appender(context.Context) storage.Appender {
	return &appenderV1{appender: s.appender()}
}

func (s *Storage) appender() *appender {
	return &appender{s: s, txs: map[uint64]*headSeries{}}
}

// StartTime returns the oldest timestamp in storage, or math.MinInt64 when empty.
func (s *Storage) StartTime() (int64, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.bufferN > 0 {
		return s.bufferMin, nil
	}
	for _, block := range s.blocks {
		if block.meta.NumSamples > 0 {
			return block.Meta().MinTime, nil
		}
	}
	return math.MinInt64, nil
}

// Close marks storage closed. Blocks are read on demand; no block state needs
// teardown at this stage of the experiment.
func (s *Storage) Close() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	for _, block := range s.blocks {
		errs = append(errs, block.Close())
	}
	for _, block := range s.retiredBlocks {
		errs = append(errs, block.Close())
	}
	s.retiredBlocks = nil
	// Flush the open WAL segment so every acknowledged record is complete and
	// its footer is written.
	errs = append(errs, s.rotateWALSegment())
	return errors.Join(errs...)
}

// closeRetired releases cached readers for blocks no longer published.
func (s *Storage) closeRetired(blocks []*Block) {
	for _, block := range blocks {
		s.retiredBlocks = append(s.retiredBlocks, block)
	}
}

// Flush writes the buffer out as a block.
func (s *Storage) Flush() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.flushLocked()
}

// NumBlocks returns written block count.
func (s *Storage) NumBlocks() int {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return len(s.blocks)
}

// flushLocked converts columns into block rows only at seal time.
func (s *Storage) flushLocked() error {
	// Measured over the whole call, fsync included: this is the commit-blocking
	// window, which is what a scrape actually waits on.
	sealStart := time.Now()
	defer func() {
		if s.reads == nil {
			return
		}
		d := uint64(time.Since(sealStart).Nanoseconds())
		s.reads.seals.Inc()
		s.reads.sealNanos.Add(d)
		s.reads.lastSealNanos.Store(d)
	}()
	if len(s.buffer) == 0 && !s.metadataDirty {
		return nil
	}
	series := make([]Series, 0, len(s.buffer))
	for _, hs := range s.buffer {
		series = append(series, Series{
			Labels:          hs.labels,
			Timestamps:      hs.ts,
			StartTimestamps: hs.st,
			Kinds:           hs.kind,
			Values:          hs.v,
			Histograms:      hs.hist,
		})
	}

	var metadata *metadataRelations
	var currentMetadata *metadataRelations
	if s.metadataDirty && s.metadata != nil {
		snapshot := s.metadata.snapshot()
		currentMetadata = &snapshot
		pending := snapshot.pendingSince(s.metadataPublished)
		if pending.hasChanges() {
			metadata = &pending
		}
	}
	if len(s.buffer) == 0 && metadata == nil {
		return nil
	}
	var catalog blockCatalog
	meta, err := writeBlock(s.dir, series, metadata, s.opts.WriteOptions, s.walSequence, &catalog, s.sharedSeriesLabels)
	if err != nil {
		return err
	}
	block, err := openBlock(filepath.Join(s.dir, meta.ULID), &catalog)
	if err != nil {
		return err
	}
	block.reads = s.reads
	// Labels were interned while the catalog was built.
	s.blocks = append(s.blocks, block)
	s.buffer = map[uint64]*headSeries{}
	s.headByName = map[string][]*headSeries{}
	s.bufferN = 0
	s.bufferMin = math.MaxInt64
	s.bufferMax = math.MinInt64
	s.metadataDirty = false
	if currentMetadata != nil {
		s.metadataPublished = currentMetadata
	}
	return s.discardWALThrough(s.walSequence)
}

// appenderV1 adapts the typed series appender to the legacy interface.
type appenderV1 struct{ *appender }

func (a *appenderV1) Append(_ storage.SeriesRef, ls labels.Labels, t int64, v float64) (storage.SeriesRef, error) {
	return a.Append_(ls, t, v)
}

func (a *appenderV1) Append_(ls labels.Labels, t int64, v float64) (storage.SeriesRef, error) {
	return a.appender.Append(0, ls, 0, t, v, nil, nil, storage.AOptions{})
}

// appender buffers one transaction of typed series columns.
type appender struct {
	s *Storage

	txs    map[uint64]*headSeries
	order  []uint64
	meta   []AssociationUpdate
	counts ingestCounts
	// lastMetadata is the metadata context of the previous sample, so context runs
	// can be counted without a per-sample set.
	lastMetadata     metadata.Metadata
	haveLastMetadata bool
	// metadataCache holds this transaction's per-series and per-context metadata
	// derivations. It is created on the first metadata observation and dropped at
	// Commit or Rollback, so nothing outlives the transaction.
	metadataCache ordinaryMetadataCache
	// metadataIdentities is index-aligned with meta: the pre-derived identity of each
	// staged observation, which the store reuses instead of deriving it again.
	metadataIdentities []associationIdentity
	done               bool
}

// ordinaryMetadataCache is the ordinary appender's derivation cache for one
// transaction. Both maps are keyed exactly: the series map by the verified series hash,
// whose hits are re-verified against the complete labels, and the context map by the
// complete type/unit/help metadata.
type ordinaryMetadataCache struct {
	series   map[uint64]ordinarySeriesIdentity
	contexts map[metadata.Metadata]ordinaryContentIdentity
}

// ordinarySeriesIdentity is the immutable derivation of one exact series: the Metric
// owner the series states and an owned copy of the complete labels that identify it.
type ordinarySeriesIdentity struct {
	// labels is an owned immutable copy, so a cached observation never aliases a
	// caller's label slice and a later caller cannot mutate a staged observation.
	labels labels.Labels
	owner  OwnerKey
}

// ordinaryContentIdentity is the immutable derivation of one exact metadata context:
// the type/unit/help content and its canonical form. The store clones content when it
// creates a version, so the shared map is never stored by reference.
type ordinaryContentIdentity struct {
	content   Content
	canonical string
}

// Append implements the shared V2 appender, producing typed head arrays.
// Prometheus metadata, where supplied, is staged alongside the transaction and
// associated to the series' own metric owner, not converted into labels here.
func (a *appender) Append(ref storage.SeriesRef, ls labels.Labels, st, t int64, v float64, h *histogram.Histogram, fh *histogram.FloatHistogram, opts storage.AOptions) (storage.SeriesRef, error) {
	if a.done {
		return 0, errors.New("columnar: appender used after Commit or Rollback")
	}
	if t < a.s.minValidTime {
		return 0, fmt.Errorf("%w: timestamp %d precedes retention cutoff %d", storage.ErrOutOfBounds, t, a.s.minValidTime)
	}

	// A non-zero ref is the hash this storage returned for the series, but it is
	// treated as a hint: complete labels must agree before the reference is used,
	// so a stale, foreign or colliding reference cannot move a sample to another
	// series. Anything that does not verify is hashed exactly as before.
	var (
		hs      *headSeries
		hash    uint64
		prev    seriesState
		hasPrev bool
	)
	reused := false
	if ref != 0 {
		if h, p, hp, ok := a.resolveReference(ref, ls); ok {
			hash, prev, hasPrev, reused = h, p, hp, true
			var err error
			if hs, err = a.stagedSeries(hash, ls); err != nil {
				return 0, err
			}
			a.counts.refTrusted++
		}
	}
	if !reused {
		var err error
		if hs, hash, err = a.seriesFor(ls); err != nil {
			return 0, err
		}
		prev, hasPrev = a.previousLocked(ls, hash, hs)
	}
	if hasPrev {
		switch {
		case t == prev.t:
			return 0, storage.ErrDuplicateSampleForTimestamp
		case t < prev.t:
			return 0, storage.ErrOutOfOrderSample
		}
	}

	kind := SampleFloat
	if value.IsStaleNaN(v) {
		kind = SampleStale
	}
	if h != nil || fh != nil {
		prevHint := sampleHintState{
			first: !hasPrev,
			reset: detectsReset(prev, h, fh),
		}
		switch {
		case prev.fh != nil:
			prevHint.gauge = prev.fh.CounterResetHint == histogram.GaugeType
		case prev.h != nil:
			prevHint.gauge = prev.h.CounterResetHint == histogram.GaugeType
		}
		h, fh = withInferredResetHint(prevHint, h, fh)
		encoded, err := encodeHistogram(h, fh)
		if err != nil {
			return 0, err
		}
		kind = SampleFloatHistogram
		if h != nil {
			kind = SampleHistogram
		}
		hs.hist = append(hs.hist, encoded)
		if kind != SampleHistogram {
			kind = SampleFloatHistogram
		}
	} else {
		hs.hist = append(hs.hist, nil)
	}

	hs.ts = append(hs.ts, t)
	hs.st = append(hs.st, st)
	hs.v = append(hs.v, v)
	hs.kind = append(hs.kind, kind)
	if !opts.Metadata.IsEmpty() {
		update, identity := a.metadataObservation(ls, hash, t, opts.Metadata)
		a.meta = append(a.meta, update)
		a.metadataIdentities = append(a.metadataIdentities, identity)
	}
	if reused {
		return refForHash(hash), nil
	}
	a.counts.refForHash++
	return refFor(ls), nil
}

// metadataObservation stages one metadata observation of one sample. The Metric owner,
// the content and the series' immutable labels are derived once per exact series and
// once per exact metadata context in this transaction; every sample still states its own
// observation with its own effective time, in input order.
//
// seriesHash is the hash this transaction verified for ls, so the staged identity lets
// the store skip re-hashing the complete labels and re-canonicalizing the content.
func (a *appender) metadataObservation(ls labels.Labels, seriesHash uint64, t int64, md metadata.Metadata) (AssociationUpdate, associationIdentity) {
	ordinary := &a.counts.ordinary
	ordinary.Appends++
	if !a.haveLastMetadata || a.lastMetadata != md {
		ordinary.ContextRuns++
		a.lastMetadata, a.haveLastMetadata = md, true
	}

	series, cached := a.metadataCache.series[seriesHash]
	switch {
	case !cached:
		series = ordinarySeriesIdentity{labels: ls.Copy(), owner: MetricOwner(ls)}
		if a.metadataCache.series == nil {
			a.metadataCache.series = make(map[uint64]ordinarySeriesIdentity)
		}
		a.metadataCache.series[seriesHash] = series
		ordinary.SeriesWithMetadata++
		ordinary.OwnerDerivations++
	case !labels.Equal(series.labels, ls):
		// A hash only locates a candidate: the complete labels decide. An aliasing hash
		// is derived for this observation, never merged into the cached identity, and its
		// labels are retained exactly as the appender retained them before the reuse.
		series = ordinarySeriesIdentity{labels: ls, owner: MetricOwner(ls)}
		ordinary.OwnerDerivations++
		ordinary.AliasedSeriesHashes++
	default:
		ordinary.SeriesCacheHits++
	}

	context, cached := a.metadataCache.contexts[md]
	if !cached {
		context = ordinaryContentIdentity{
			content: Content{Attributes: map[string]string{
				"type": string(md.Type), "unit": md.Unit, "help": md.Help,
			}},
		}
		context.canonical = context.content.Canonical()
		if a.metadataCache.contexts == nil {
			a.metadataCache.contexts = make(map[metadata.Metadata]ordinaryContentIdentity)
		}
		a.metadataCache.contexts[md] = context
		ordinary.ContentBuilds++
		ordinary.ContextCanonicalizations++
	} else {
		ordinary.ContextCacheHits++
	}

	return AssociationUpdate{
			Labels: series.labels,
			Update: Update{Owner: series.owner, Content: context.content, EffectiveFrom: t},
		},
		associationIdentity{seriesHash: seriesHash, canonical: context.canonical}
}

// headSeriesPool reuses per-transaction headSeries objects. Labels are always
// overwritten before use, and sample slices are either fresh or pooled by
// appendTypedSeries.
var headSeriesPool = sync.Pool{New: func() any { return new(headSeries) }}

// seriesFor returns the transaction's series.
// seriesFor returns the transaction's series and its stable hash, so callers do
// not re-hash the same labels.
func (a *appender) seriesFor(ls labels.Labels) (*headSeries, uint64, error) {
	a.counts.seriesForHash++
	a.counts.refFallback++
	hash := stableSeriesHash(ls)
	hs, err := a.stagedSeries(hash, ls)
	if err != nil {
		return nil, 0, err
	}
	return hs, hash, nil
}

// stagedSeries returns the transaction's series for hash, creating it when the
// transaction has not seen it yet. Complete labels are compared whenever the
// hash came from outside this function, so a reference cannot alias a series.
func (a *appender) stagedSeries(hash uint64, ls labels.Labels) (*headSeries, error) {
	if hs, ok := a.txs[hash]; ok {
		if !labels.Equal(hs.labels, ls) {
			return nil, fmt.Errorf("%w for labels = %q", ErrSeriesHashCollision, ls)
		}
		return hs, nil
	}
	hs := headSeriesPool.Get().(*headSeries)
	*hs = headSeries{hash: hash, labels: ls}
	a.txs[hash] = hs
	a.order = append(a.order, hash)
	return hs, nil
}

// resolveReference reports the series hash, previous-sample state and whether a
// reference can be trusted. The reference is the hash this storage returned for
// the series, so the transaction's staged series or the committed tail at that
// hash verifies it with one complete-label comparison and no hashing. ok is
// false for a zero reference, a label mismatch, or a series that is not known
// yet; the caller then hashes the labels, which preserves the previous
// behaviour for stale, foreign and zero references.
func (a *appender) resolveReference(ref storage.SeriesRef, ls labels.Labels) (hash uint64, prev seriesState, hasPrev, ok bool) {
	hash = uint64(ref)
	if tx, staged := a.txs[hash]; staged {
		if !labels.Equal(tx.labels, ls) {
			return 0, seriesState{}, false, false
		}
		if len(tx.ts) > 0 {
			return hash, stagedState(ls, tx), true, true
		}
	}
	waitStart := time.Now()
	a.s.mtx.Lock()
	if a.s.reads != nil {
		a.s.reads.observeAppendLockWait(time.Since(waitStart))
	}
	last, found := a.s.last[hash]
	a.s.mtx.Unlock()
	if found && labels.Equal(last.labels, ls) {
		return hash, *last, true, true
	}
	return 0, seriesState{}, false, false
}

// Commit applies the buffered series and metadata through the same batched
// transaction as direct OTAP ingestion and replay.
func (a *appender) Commit() error {
	if a.done {
		return errors.New("columnar: appender used after Commit or Rollback")
	}
	a.done = true

	batch := Batch{
		Updates: a.meta, Series: make([]BatchSeries, 0, len(a.order)), counts: a.counts,
		ordinaryAssociations: true, associationIdentities: a.metadataIdentities,
	}
	for _, hash := range a.order {
		tx := a.txs[hash]
		if len(tx.ts) == 0 {
			headSeriesPool.Put(tx)
			continue
		}
		batch.Series = append(batch.Series, BatchSeries{
			Labels: tx.labels, Timestamps: tx.ts, StartTimestamps: tx.st,
			Kinds: tx.kind, Values: tx.v, Histograms: tx.hist,
			hash: tx.hash, hashValid: true,
		})
		// The batch captured the slice headers and labels by value; the struct
		// itself can be recycled for the next transaction.
		headSeriesPool.Put(tx)
	}
	a.txs, a.order, a.meta = nil, nil, nil
	a.metadataIdentities, a.metadataCache = nil, ordinaryMetadataCache{}
	return a.s.AppendBatch(context.Background(), batch)
}

// Rollback discards the whole transaction without touching the head.
func (a *appender) Rollback() error {
	if a.done {
		return errors.New("columnar: appender used after Commit or Rollback")
	}
	a.done = true
	for _, hash := range a.order {
		if tx, ok := a.txs[hash]; ok {
			*tx = headSeries{}
			headSeriesPool.Put(tx)
		}
	}
	a.txs, a.order, a.meta = nil, nil, nil
	a.metadataIdentities, a.metadataCache = nil, ordinaryMetadataCache{}
	return nil
}

// AppendHistogram adds a histogram through the same reduced-codec array path.
func (a *appender) AppendHistogram(ref storage.SeriesRef, ls labels.Labels, t int64, h *histogram.Histogram, fh *histogram.FloatHistogram) (storage.SeriesRef, error) {
	return a.Append(ref, ls, 0, t, 0, h, fh, storage.AOptions{})
}

// AppendHistogramSTZeroSample stores a synthetic zero sample at st.
func (a *appender) AppendHistogramSTZeroSample(ref storage.SeriesRef, ls labels.Labels, _, st int64, h *histogram.Histogram, fh *histogram.FloatHistogram) (storage.SeriesRef, error) {
	return a.Append(ref, ls, 0, st, 0, h, fh, storage.AOptions{})
}

// AppendSTZeroSample appends zero at st, as the TSDB adapter does.
func (a *appender) AppendSTZeroSample(ref storage.SeriesRef, ls labels.Labels, _, st int64) (storage.SeriesRef, error) {
	return a.Append(ref, ls, 0, st, 0, nil, nil, storage.AOptions{})
}

// AppendExemplar is accepted but not stored in this experimental version.
func (*appender) AppendExemplar(ref storage.SeriesRef, ls labels.Labels, _ exemplar.Exemplar) (storage.SeriesRef, error) {
	return refFor(ls), nil
}

// UpdateMetadata handles the v1 MetadataUpdater contract, resolved through the
// same shared Metric owner rather than fake blobs on an unrelated series.
func (*appender) UpdateMetadata(_ storage.SeriesRef, _ labels.Labels, _ metadata.Metadata) (storage.SeriesRef, error) {
	return 0, errors.New("columnar: metadata appending needs a sample timestamp; use AppenderV2 or AppendBatch")
}

// SetOptions currently has no knobs; OOO is deliberately not enabled here.
func (*appender) SetOptions(*storage.AppendOptions) {}

// sampleHintState identifies the previous counter/gauge state and whether the
// sample would begin a fresh TSDB chunk. Reproducing it preserves the semantics
// of counter-reset warnings without exposing newly invented mid-series hints.
type sampleHintState struct {
	first bool
	gauge bool
	reset bool
}

func withInferredResetHint(prev sampleHintState, h *histogram.Histogram, fh *histogram.FloatHistogram) (*histogram.Histogram, *histogram.FloatHistogram) {
	var hint histogram.CounterResetHint
	if fh != nil {
		hint = fh.CounterResetHint
	} else {
		hint = h.CounterResetHint
	}

	var target histogram.CounterResetHint
	switch {
	case hint == histogram.GaugeType:
		return h, fh
	case prev.gauge:
		// TSDB starts a new chunk at the gauge-to-counter transition. Its first
		// non-gauge sample is conservatively unknown, not still a gauge.
		target = histogram.UnknownCounterReset
	case prev.first, prev.reset, hint == histogram.CounterReset:
		target = histogram.UnknownCounterReset
	default:
		target = histogram.NotCounterReset
	}
	if target == hint {
		return h, fh
	}
	if fh != nil {
		cp := fh.Copy()
		cp.CounterResetHint = target
		return nil, cp
	}
	cp := h.Copy()
	cp.CounterResetHint = target
	return cp, nil
}

// detectsReset compares histogram state as floats, which is sufficient because
// counter reset detection works through relative bucket/count values.
func detectsReset(prev seriesState, h *histogram.Histogram, fh *histogram.FloatHistogram) bool {
	var prevFH *histogram.FloatHistogram
	switch {
	case prev.fh != nil:
		prevFH = prev.fh
	case prev.h != nil:
		prevFH = prev.h.ToFloat(nil)
	default:
		return false
	}
	var curFH *histogram.FloatHistogram
	if fh != nil {
		curFH = fh
	} else {
		curFH = h.ToFloat(nil)
	}
	if prevFH.Schema != curFH.Schema || prevFH.ZeroThreshold != curFH.ZeroThreshold {
		// Not comparable => new chunk/header semantics, not a false same-chunk reset.
		return true
	}
	return curFH.DetectReset(prevFH)
}

// previousLocked finds the newest sample before the append, including samples
// already staged in the same appender transaction.
func (a *appender) previousLocked(ls labels.Labels, hash uint64, tx *headSeries) (seriesState, bool) {
	if len(tx.ts) > 0 {
		return stagedState(ls, tx), true
	}
	// Scalar appenders are used concurrently by independent scrape pools.
	// The shared committed-tail map is protected by the same lock used by
	// batch seals and lifecycle operations; transaction-local state above is not.
	waitStart := time.Now()
	a.s.mtx.Lock()
	if a.s.reads != nil {
		a.s.reads.observeAppendLockWait(time.Since(waitStart))
	}
	defer a.s.mtx.Unlock()
	if last, ok := a.s.last[hash]; ok {
		return *last, true
	}
	return seriesState{}, false
}

// stagedState reports the newest sample staged in the transaction.
func stagedState(ls labels.Labels, tx *headSeries) seriesState {
	last := tx.ts[len(tx.ts)-1]
	state := seriesState{labels: ls, t: last, st: tx.st[len(tx.st)-1]}
	if kind := tx.kind[len(tx.kind)-1]; kind != SampleFloat {
		h, fh, err := decodeSampleHistogram(tx.hist[len(tx.hist)-1])
		if err == nil {
			state.h, state.fh = h, fh
		}
	}
	return state
}

// refFor derives a stable series reference. Referenced series use the same hash
// as metadata associations; callers must not treat zero as a valid ref.
func refFor(ls labels.Labels) storage.SeriesRef {
	return refForHash(stableSeriesHash(ls))
}

// refForHash is refFor for an already computed hash, so a verified reference can
// be returned without hashing the labels again. A zero hash is never a valid
// reference.
func refForHash(hash uint64) storage.SeriesRef {
	if ref := storage.SeriesRef(hash); ref != 0 {
		return ref
	}
	return 1
}
