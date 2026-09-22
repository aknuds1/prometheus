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

package tsdb

import (
	"context"
	"errors"
	"math"
	"sync"
	"unique"

	"go.uber.org/atomic"
	"golang.org/x/sync/semaphore"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/index"
)

const (
	nativeMetricMetadataStripes            = 256
	maxNativeMetricMetadataVersions        = 5
	nativeMetricMetadataPublicationPermits = math.MaxInt64
)

// ErrNativeMetadataDisabled is returned when native metadata storage is not enabled.
var ErrNativeMetadataDisabled = errors.New("native metadata is disabled; enable with --enable-feature=native-metadata")

// NativeMetricMetadataVersion is a metric metadata change point in milliseconds.
type NativeMetricMetadataVersion struct {
	EffectiveFrom int64
	Metadata      metadata.Metadata
}

// NativeMetricMetadataSeries contains native metric metadata for one series.
// Labels are immutable; callers must not modify them.
type NativeMetricMetadataSeries struct {
	Labels labels.Labels
	// Versions contains change points ordered by increasing EffectiveFrom.
	Versions []NativeMetricMetadataVersion
	// Truncated reports whether the per-series cap has ever evicted a version.
	Truncated bool
}

type nativeMetricMetadataPoint struct {
	effectiveFrom int64
	metadata      unique.Handle[metadata.Metadata]
}

// nativeMetricMetadataStripe indexes series with committed native history.
// Its mutex protects membership, not the series or their metadata. Release it
// before taking a series or Head-index lock.
type nativeMetricMetadataStripe struct {
	mtx    sync.RWMutex
	series map[chunks.HeadSeriesRef]*memSeries
}

// nativeMetricMetadataStore indexes series-owned native histories and coordinates
// their publication to senders. Index entries retain retired series until cleanup;
// they are not a substitute for revalidating membership in the Head.
type nativeMetricMetadataStore struct {
	// Keep the 64-bit atomics first for alignment on 32-bit platforms.
	// Stores are allocated individually by newNativeMetricMetadataStore.
	series    atomic.Int64
	versions  atomic.Int64
	evictions atomic.Uint64

	stripes      [nativeMetricMetadataStripes]nativeMetricMetadataStripe
	appenderPool sync.Pool
	// Commits hold one permit from before WAL logging through cache publication.
	// Senders acquire all permits for a bounded lookup batch. FIFO acquisition
	// prevents a steady stream of commits from starving senders.
	publication *semaphore.Weighted
}

func newNativeMetricMetadataStore() *nativeMetricMetadataStore {
	s := &nativeMetricMetadataStore{publication: semaphore.NewWeighted(nativeMetricMetadataPublicationPermits)}
	for i := range s.stripes {
		s.stripes[i].series = make(map[chunks.HeadSeriesRef]*memSeries)
	}
	return s
}

func (s *nativeMetricMetadataStore) stripe(ref chunks.HeadSeriesRef) *nativeMetricMetadataStripe {
	return &s.stripes[uint64(ref)%nativeMetricMetadataStripes]
}

func (s *nativeMetricMetadataStore) indexedSeries(ref chunks.HeadSeriesRef) *memSeries {
	stripe := s.stripe(ref)
	stripe.mtx.RLock()
	series := stripe.series[ref]
	stripe.mtx.RUnlock()
	return series
}

func (s *nativeMetricMetadataStore) has(ref chunks.HeadSeriesRef) bool {
	return s.indexedSeries(ref) != nil
}

func (s *nativeMetricMetadataStore) delete(refs map[storage.SeriesRef]struct{}) {
	var byStripe [nativeMetricMetadataStripes][]chunks.HeadSeriesRef
	for ref := range refs {
		headRef := chunks.HeadSeriesRef(ref)
		stripe := uint64(headRef) % nativeMetricMetadataStripes
		byStripe[stripe] = append(byStripe[stripe], headRef)
	}
	for i := range nativeMetricMetadataStripes {
		stripeRefs := byStripe[i]
		if len(stripeRefs) == 0 {
			continue
		}
		stripe := &s.stripes[i]
		var retired []*memSeries
		stripe.mtx.Lock()
		for _, ref := range stripeRefs {
			if series := stripe.series[ref]; series != nil {
				delete(stripe.series, ref)
				retired = append(retired, series)
			}
		}
		stripe.mtx.Unlock()
		// Head deletion has already excluded pending commits. Never take a
		// series lock under the index lock, and never clear retired native state:
		// an in-flight forwarding lookup may still hold its pointer.
		var versions int64
		for _, series := range retired {
			series.Lock()
			versions += int64(len(series.nativeMetadataLocked().older) + 1)
			series.Unlock()
		}
		s.series.Add(-int64(len(retired)))
		s.versions.Add(-versions)
	}
}

// reset clears presence and current series/version counts after replacing Head
// series, while preserving cumulative evictions. The caller must exclude
// concurrent store mutations.
func (s *nativeMetricMetadataStore) reset() {
	for i := range s.stripes {
		stripe := &s.stripes[i]
		stripe.mtx.Lock()
		stripe.series = make(map[chunks.HeadSeriesRef]*memSeries)
		stripe.mtx.Unlock()
	}
	s.series.Store(0)
	s.versions.Store(0)
}

// nativeSeriesMetadata owns a series' committed history, with its newest point
// inline and up to four chronological older points. Adjacent values differ;
// truncated remains set after an eviction, even if the history later collapses.
// The series lock protects updates. Forwarding may read native state under the
// publication barrier; deletion must therefore leave retired history unchanged.
type nativeSeriesMetadata struct {
	metadata      *metadata.Metadata
	effectiveFrom int64
	handle        unique.Handle[metadata.Metadata]
	older         []nativeMetricMetadataPoint
	truncated     bool
}

// mergeLocked merges non-empty, strictly timestamp-ordered observations. It
// returns the change in retained versions and the number evicted. The caller
// holds the series lock and must update metadata to match the resulting handle.
func (n *nativeSeriesMetadata) mergeLocked(observations []nativeMetricMetadataPoint) (versionDelta, evictions int) {
	oldCount := 0
	if n.handle != (unique.Handle[metadata.Metadata]{}) {
		oldCount = len(n.older) + 1
	}
	if oldCount == 0 || observations[0].effectiveFrom >= n.effectiveFrom {
		for _, observation := range observations {
			switch {
			case n.handle == (unique.Handle[metadata.Metadata]{}):
				n.effectiveFrom, n.handle = observation.effectiveFrom, observation.metadata
			case observation.effectiveFrom == n.effectiveFrom:
				n.handle = observation.metadata
				if last := len(n.older) - 1; last >= 0 && n.older[last].metadata == n.handle {
					n.effectiveFrom = n.older[last].effectiveFrom
					n.older[last] = nativeMetricMetadataPoint{}
					n.older = n.older[:last]
				}
			case observation.metadata == n.handle:
				// A repeated value does not advance the change point.
			default:
				previous := nativeMetricMetadataPoint{effectiveFrom: n.effectiveFrom, metadata: n.handle}
				if len(n.older) == maxNativeMetricMetadataVersions-1 {
					copy(n.older, n.older[1:])
					n.older[len(n.older)-1] = previous
					evictions++
				} else {
					if len(n.older) == cap(n.older) {
						grown := make([]nativeMetricMetadataPoint, len(n.older), max(1, 2*cap(n.older)))
						copy(grown, n.older)
						n.older = grown
					}
					n.older = append(n.older, previous)
				}
				n.effectiveFrom, n.handle = observation.effectiveFrom, observation.metadata
			}
		}
	} else {
		var existing, retained [maxNativeMetricMetadataVersions]nativeMetricMetadataPoint
		count := copy(existing[:], n.older)
		existing[count] = nativeMetricMetadataPoint{effectiveFrom: n.effectiveFrom, metadata: n.handle}
		var versions []nativeMetricMetadataPoint
		versions, evictions = mergeOverlappingNativeMetricMetadata(existing[:count+1], observations, retained[:0])
		olderCount := len(versions) - 1
		if cap(n.older) < olderCount {
			capacity := 1
			for capacity < olderCount {
				capacity *= 2
			}
			n.older = make([]nativeMetricMetadataPoint, olderCount, capacity)
		} else {
			if len(n.older) > olderCount {
				clear(n.older[olderCount:])
			}
			n.older = n.older[:olderCount]
		}
		copy(n.older, versions[:olderCount])
		newest := versions[olderCount]
		n.effectiveFrom, n.handle = newest.effectiveFrom, newest.metadata
	}
	if len(n.older) == 0 {
		n.older = nil
	}
	if evictions > 0 {
		n.truncated = true
	}
	return len(n.older) + 1 - oldCount, evictions
}

// mergeOverlappingNativeMetricMetadata merges strictly timestamp-ordered inputs,
// preferring observations at equal timestamps and coalescing adjacent equal values.
// It leaves both inputs unchanged and appends retained points to versions,
// returning that slice and the number evicted by the version cap.
func mergeOverlappingNativeMetricMetadata(existing, observations, versions []nativeMetricMetadataPoint) ([]nativeMetricMetadataPoint, int) {
	var retained [maxNativeMetricMetadataVersions]nativeMetricMetadataPoint
	start, count, evictions := 0, 0, 0
	var lastMetadata unique.Handle[metadata.Metadata]
	haveLastMetadata := false

	appendPoint := func(point nativeMetricMetadataPoint) {
		if haveLastMetadata && lastMetadata == point.metadata {
			return
		}
		haveLastMetadata = true
		lastMetadata = point.metadata

		if count < len(retained) {
			retained[(start+count)%len(retained)] = point
			count++
			return
		}
		retained[start] = point
		start = (start + 1) % len(retained)
		evictions++
	}

	for i, j := 0, 0; i < len(existing) || j < len(observations); {
		switch {
		case i == len(existing):
			appendPoint(observations[j])
			j++
		case j == len(observations):
			appendPoint(existing[i])
			i++
		case existing[i].effectiveFrom < observations[j].effectiveFrom:
			appendPoint(existing[i])
			i++
		case existing[i].effectiveFrom > observations[j].effectiveFrom:
			appendPoint(observations[j])
			j++
		default:
			appendPoint(observations[j])
			i++
			j++
		}
	}

	for i := range count {
		versions = append(versions, retained[(start+i)%len(retained)])
	}
	return versions, evictions
}

// nativeMetricMetadataSnapshot owns its copied points independently of the series.
type nativeMetricMetadataSnapshot struct {
	points    [maxNativeMetricMetadataVersions]nativeMetricMetadataPoint
	count     int
	truncated bool
}

func (s *nativeMetricMetadataSnapshot) expand() []NativeMetricMetadataVersion {
	versions := make([]NativeMetricMetadataVersion, s.count)
	for i, point := range s.points[:s.count] {
		versions[i] = NativeMetricMetadataVersion{EffectiveFrom: point.effectiveFrom, Metadata: point.metadata.Value()}
	}
	return versions
}

type nativeMetricMetadataPostings struct {
	index.Postings
	store *nativeMetricMetadataStore
}

func (p *nativeMetricMetadataPostings) Next() bool {
	for p.Postings.Next() {
		if p.store.has(chunks.HeadSeriesRef(p.At())) {
			return true
		}
	}
	return false
}

func (p *nativeMetricMetadataPostings) Seek(ref storage.SeriesRef) bool {
	if !p.Postings.Seek(ref) {
		return false
	}
	if p.store.has(chunks.HeadSeriesRef(p.At())) {
		return true
	}
	return p.Next()
}

func (h *Head) nativeMetricMetadataForMatchers(ctx context.Context, matcherSets [][]*labels.Matcher, limit int) ([]NativeMetricMetadataSeries, bool, error) {
	if h.nativeMetricMetadata == nil {
		return nil, false, ErrNativeMetadataDisabled
	}

	reader, err := h.Index()
	if err != nil {
		return nil, false, err
	}
	defer reader.Close()

	postings := make([]index.Postings, 0, len(matcherSets))
	for _, matchers := range matcherSets {
		p, err := PostingsForMatchers(ctx, reader, matchers...)
		if err != nil {
			return nil, false, err
		}
		postings = append(postings, p)
	}
	// Sorting materialises every matching series before iteration, so limit
	// bounds the response and the per-series decode below but not this. That is
	// deliberate: bounding before the sort would return an arbitrary subset
	// rather than the first by label order.
	p := reader.SortedPostings(&nativeMetricMetadataPostings{
		Postings: index.Merge(ctx, postings...),
		store:    h.nativeMetricMetadata,
	})

	return h.nativeMetricMetadataForPostings(ctx, p, limit)
}

// nativeMetricMetadataForPostings consumes label-sorted references, not retained
// series pointers.
func (h *Head) nativeMetricMetadataForPostings(ctx context.Context, p index.Postings, limit int) ([]NativeMetricMetadataSeries, bool, error) {
	result := make([]NativeMetricMetadataSeries, 0)
	for p.Next() {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		ref := chunks.HeadSeriesRef(p.At())
		series := h.series.getByID(ref)
		if series == nil {
			continue
		}
		// Head lookup releases its index lock before we take the series lock.
		// GC marks a retired series under that lock, so this check revalidates
		// liveness while copying history and labels (including dedupelabels).
		series.Lock()
		native := series.nativeMetadataLocked()
		if native == nil || series.isGCed() {
			series.Unlock()
			continue
		}
		var snapshot nativeMetricMetadataSnapshot
		snapshot.count = copy(snapshot.points[:], native.older)
		snapshot.points[snapshot.count] = nativeMetricMetadataPoint{effectiveFrom: native.effectiveFrom, metadata: native.handle}
		snapshot.count++
		snapshot.truncated = native.truncated
		lset := series.lset
		series.Unlock()
		// Only a live additional row proves truncation. Do not expand its metadata.
		if limit > 0 && len(result) == limit {
			return result, true, nil
		}
		result = append(result, NativeMetricMetadataSeries{
			Labels:    lset,
			Versions:  snapshot.expand(),
			Truncated: snapshot.truncated,
		})
	}
	if err := p.Err(); err != nil {
		return nil, false, err
	}
	return result, false, nil
}

// NativeMetricMetadata returns native metric metadata from the Head in label order.
// Matcher sets are ORed, with matchers within each set ANDed. Empty sets contribute
// no matches; no sets yields no results. Matcher slices may be reordered.
//
// A non-positive limit is unlimited. The boolean reports result truncation by
// the series limit, independently of per-series history truncation.
func (db *DB) NativeMetricMetadata(ctx context.Context, matcherSets [][]*labels.Matcher, limit int) ([]NativeMetricMetadataSeries, bool, error) {
	return db.head.nativeMetricMetadataForMatchers(ctx, matcherSets, limit)
}
