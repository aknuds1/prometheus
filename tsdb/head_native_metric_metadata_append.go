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
	"slices"
	"unique"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

const (
	nativeMetricMetadataBitsPerWord   = 64
	maxNativeMetricMetadataValues     = 128 // Limits raw metadata retained by a transaction.
	maxNativeMetricMetadataBatch      = 256 // Bounds value preparation before merging a batch.
	nativeMetricMetadataDirectRefMask = nativeMetricMetadataValueRef(1 << 31)
)

// nativeMetricMetadataValue retains raw metadata for comparisons without
// interning. When resolved is true, handle identifies the same value.
type nativeMetricMetadataValue struct {
	metadata metadata.Metadata
	handle   unique.Handle[metadata.Metadata]
	resolved bool
}

// nativeMetricMetadataValueRef selects a one-based raw value, or a zero-based
// direct handle when the high bit is set. Zero is not a value reference.
type nativeMetricMetadataValueRef uint32

// nativeMetricMetadataObservationRef is a one-based observation index.
// Zero terminates an observation chain or denotes an empty stripe.
type nativeMetricMetadataObservationRef uint32

type nativeMetricMetadataObservation struct {
	series        *memSeries
	effectiveFrom int64
	metadataRef   nativeMetricMetadataValueRef
	next          nativeMetricMetadataObservationRef
}

// nativeMetricMetadataGroup selects one series' observations with [start, end)
// indexes into nativeMetricMetadataAppender.sorted.
type nativeMetricMetadataGroup struct {
	start int
	end   int
}

// nativeMetricMetadataAppender buffers metadata observations for one transaction
// until commit. It has a single owner and is not safe for concurrent use.
// Returning it through putAppender resets it for reuse. References into its
// mutable scratch storage must not survive its return to the pool.
type nativeMetricMetadataAppender struct {
	observations  []nativeMetricMetadataObservation
	values        []nativeMetricMetadataValue
	valueRefs     map[metadata.Metadata]nativeMetricMetadataValueRef
	directHandles []unique.Handle[metadata.Metadata]
	// Observation references for the stripe being committed.
	sorted []nativeMetricMetadataObservationRef
	// Reusable merge input for the current series, with unique timestamps.
	points []nativeMetricMetadataPoint
	// Changing series selected for the current batch.
	groups []nativeMetricMetadataGroup
	// Immutable cache values shared across this transaction's series.
	shared map[unique.Handle[metadata.Metadata]]*metadata.Metadata
	// Stripes with observations, in order of first use.
	touched     []uint8
	stripeFirst [nativeMetricMetadataStripes]nativeMetricMetadataObservationRef
	stripeLast  [nativeMetricMetadataStripes]nativeMetricMetadataObservationRef
	// One bit per stripe, set when this transaction records observations for
	// multiple distinct series. Stripe sharing is expected.
	multiSeriesStripes [nativeMetricMetadataStripes / nativeMetricMetadataBitsPerWord]uint64
	lastMetadata       metadata.Metadata
	lastObservation    nativeMetricMetadataObservationRef
	haveLast           bool
}

func newNativeMetricMetadataAppender() *nativeMetricMetadataAppender {
	return &nativeMetricMetadataAppender{
		observations: make([]nativeMetricMetadataObservation, 0, nativeMetricMetadataStripes),
		values:       make([]nativeMetricMetadataValue, 0, maxNativeMetricMetadataValues),
		valueRefs:    make(map[metadata.Metadata]nativeMetricMetadataValueRef, maxNativeMetricMetadataValues),
		groups:       make([]nativeMetricMetadataGroup, 0, maxNativeMetricMetadataBatch),
		shared:       make(map[unique.Handle[metadata.Metadata]]*metadata.Metadata, maxNativeMetricMetadataValues),
		touched:      make([]uint8, 0, nativeMetricMetadataStripes),
	}
}

func (a *nativeMetricMetadataAppender) metadataValue(ref nativeMetricMetadataValueRef) metadata.Metadata {
	if ref&nativeMetricMetadataDirectRefMask != 0 {
		return a.directHandles[ref&^nativeMetricMetadataDirectRefMask].Value()
	}
	return a.values[ref-1].metadata
}

// metadataHandle lazily interns raw values. Commit resolves selected references
// before taking a series lock.
func (a *nativeMetricMetadataAppender) metadataHandle(ref nativeMetricMetadataValueRef) unique.Handle[metadata.Metadata] {
	if ref&nativeMetricMetadataDirectRefMask != 0 {
		return a.directHandles[ref&^nativeMetricMetadataDirectRefMask]
	}
	value := &a.values[ref-1]
	if !value.resolved {
		value.handle = unique.Make(value.metadata)
		value.resolved = true
	}
	return value.handle
}

// metadataReference returns a transaction-local reference to m. It deduplicates
// a bounded set of raw values, deferring their interning until needed. Values
// outside that set use direct handles.
func (a *nativeMetricMetadataAppender) metadataReference(series *memSeries, m metadata.Metadata) nativeMetricMetadataValueRef {
	if valueRef, ok := a.valueRefs[m]; ok {
		return valueRef
	}
	if len(a.values) < maxNativeMetricMetadataValues {
		a.values = append(a.values, nativeMetricMetadataValue{metadata: m})
		valueRef := nativeMetricMetadataValueRef(len(a.values))
		a.valueRefs[m] = valueRef
		return valueRef
	}

	// Reuse the committed handle for stable high-cardinality metadata. This
	// bounds raw transaction state without paying the interning cost again.
	var (
		committed     unique.Handle[metadata.Metadata]
		haveCommitted bool
	)
	series.Lock()
	if native := series.nativeMetadataLocked(); native != nil {
		committed = native.handle
		haveCommitted = true
	}
	series.Unlock()

	// Compare after unlocking. The handle keeps its value alive independently
	// of the series, and the comparison need not lengthen its critical section.
	handle := committed
	if !haveCommitted || committed.Value() != m {
		handle = unique.Make(m)
	}
	valueRef := nativeMetricMetadataDirectRefMask | nativeMetricMetadataValueRef(len(a.directHandles))
	a.directHandles = append(a.directHandles, handle)
	return valueRef
}

// mayHaveObservedSeries reports whether ref may have a buffered observation
// in this transaction. False is definitive; true may be a false positive.
func (a *nativeMetricMetadataAppender) mayHaveObservedSeries(ref chunks.HeadSeriesRef) bool {
	stripe := uint8(uint64(ref) % nativeMetricMetadataStripes)
	first := a.stripeFirst[stripe]
	if first == 0 {
		return false
	}
	if a.observations[first-1].series.ref == ref {
		return true
	}
	// The first observed series differs from ref. If multiple series were
	// observed in this stripe, conservatively report a possible match rather
	// than walking its observation chain.
	return a.multiSeriesStripes[stripe/nativeMetricMetadataBitsPerWord]&(uint64(1)<<(stripe%nativeMetricMetadataBitsPerWord)) != 0
}

// observe buffers m for the series at effectiveFrom without updating committed
// metadata. Every call adds an observation, even when m is unchanged.
func (a *nativeMetricMetadataAppender) observe(s *memSeries, effectiveFrom int64, m metadata.Metadata) {
	var metadataRef nativeMetricMetadataValueRef
	if a.haveLast && a.lastMetadata == m {
		metadataRef = a.observations[a.lastObservation-1].metadataRef
	} else {
		metadataRef = a.metadataReference(s, m)
	}

	// Group observations by stripe for commit.
	stripe := uint8(uint64(s.ref) % nativeMetricMetadataStripes)
	observationRef := nativeMetricMetadataObservationRef(len(a.observations) + 1)
	a.observations = append(a.observations, nativeMetricMetadataObservation{
		series:        s,
		effectiveFrom: effectiveFrom,
		metadataRef:   metadataRef,
	})
	if a.stripeFirst[stripe] == 0 {
		a.stripeFirst[stripe] = observationRef
		a.touched = append(a.touched, stripe)
	} else {
		first := a.observations[a.stripeFirst[stripe]-1]
		if first.series.ref != s.ref {
			a.multiSeriesStripes[stripe/nativeMetricMetadataBitsPerWord] |= uint64(1) << (stripe % nativeMetricMetadataBitsPerWord)
		}
		a.observations[a.stripeLast[stripe]-1].next = observationRef
	}
	a.stripeLast[stripe] = observationRef

	a.lastMetadata = m
	a.lastObservation = observationRef
	a.haveLast = true
}

// selectBatch selects changing series groups and returns the next position.
// Stable groups count toward the limit; each comparison holds only its series lock.
func (a *nativeMetricMetadataAppender) selectBatch(position int) int {
	a.groups = a.groups[:0]
	for examined := 0; position < len(a.sorted) && examined < maxNativeMetricMetadataBatch; examined++ {
		end := position + 1
		series := a.observations[a.sorted[position]-1].series
		for end < len(a.sorted) && a.observations[a.sorted[end]-1].series.ref == series.ref {
			end++
		}
		series.Lock()
		stable := nativeMetricMetadataGroupStable(series.nativeMetadataLocked(), a, a.sorted[position:end])
		series.Unlock()
		if !stable {
			a.groups = append(a.groups, nativeMetricMetadataGroup{start: position, end: end})
		}
		position = end
	}
	return position
}

func (s *nativeMetricMetadataStore) getAppender() *nativeMetricMetadataAppender {
	if appender := s.appenderPool.Get(); appender != nil {
		return appender.(*nativeMetricMetadataAppender)
	}
	return newNativeMetricMetadataAppender()
}

func (s *nativeMetricMetadataStore) putAppender(appender *nativeMetricMetadataAppender) {
	for _, stripe := range appender.touched {
		appender.stripeFirst[stripe] = 0
		appender.stripeLast[stripe] = 0
	}
	appender.multiSeriesStripes = [nativeMetricMetadataStripes / nativeMetricMetadataBitsPerWord]uint64{}
	clear(appender.observations)
	appender.observations = appender.observations[:0]
	clear(appender.values)
	appender.values = appender.values[:0]
	clear(appender.valueRefs)
	clear(appender.directHandles)
	appender.directHandles = appender.directHandles[:0]
	appender.sorted = appender.sorted[:0]
	clear(appender.points[:cap(appender.points)])
	appender.points = appender.points[:0]
	appender.groups = appender.groups[:0]
	clear(appender.shared)
	appender.touched = appender.touched[:0]
	appender.lastMetadata = metadata.Metadata{}
	appender.lastObservation = 0
	appender.haveLast = false
	s.appenderPool.Put(appender)
}

func (s *nativeMetricMetadataStore) commitAppender(appender *nativeMetricMetadataAppender) {
	for _, stripeIndex := range appender.touched {
		s.commitAppenderStripe(stripeIndex, appender)
	}
}

func (s *nativeMetricMetadataStore) commitAppenderStripe(stripeIndex uint8, appender *nativeMetricMetadataAppender) {
	first := appender.stripeFirst[stripeIndex]
	// Skip stable stripes before collecting and sorting observation references.
	if nativeMetricMetadataStripeStable(appender, first) {
		return
	}

	appender.sorted = appender.sorted[:0]
	for observationRef := first; observationRef != 0; observationRef = appender.observations[observationRef-1].next {
		appender.sorted = append(appender.sorted, observationRef)
	}
	// Order by series and timestamp, then by append order so the last
	// observation wins when equal timestamps are collapsed during merging.
	compare := func(a, b nativeMetricMetadataObservationRef) int {
		left := appender.observations[a-1]
		right := appender.observations[b-1]
		switch {
		case left.series.ref < right.series.ref:
			return -1
		case left.series.ref > right.series.ref:
			return 1
		case left.effectiveFrom < right.effectiveFrom:
			return -1
		case left.effectiveFrom > right.effectiveFrom:
			return 1
		case a < b:
			return -1
		case a > b:
			return 1
		default:
			return 0
		}
	}
	if !slices.IsSortedFunc(appender.sorted, compare) {
		slices.SortFunc(appender.sorted, compare)
	}

	// Prepare immutable values without a series lock, then recheck and merge
	// under that lock. Concurrent commits may have changed the current point.
	for position := 0; position < len(appender.sorted); {
		position = appender.selectBatch(position)
		for _, group := range appender.groups {
			for _, observationRef := range appender.sorted[group.start:group.end] {
				appender.metadataHandle(appender.observations[observationRef-1].metadataRef)
			}
			last := appender.observations[appender.sorted[group.end-1]-1]
			handle := appender.metadataHandle(last.metadataRef)
			if appender.shared[handle] == nil {
				value := appender.metadataValue(last.metadataRef)
				appender.shared[handle] = &value
			}
		}
		s.commitBatch(appender)
	}
}

// commitBatch publishes complete per-series states. Head appenders retain their
// pending-sample reservations until after all batched accounting has finished.
func (s *nativeMetricMetadataStore) commitBatch(appender *nativeMetricMetadataAppender) {
	var addedSeries, versionDelta int64
	var evicted uint64
	for _, group := range appender.groups {
		observationRefs := appender.sorted[group.start:group.end]
		series := appender.observations[observationRefs[0]-1].series
		appender.points = appender.points[:0]
		for _, observationRef := range observationRefs {
			observation := appender.observations[observationRef-1]
			point := nativeMetricMetadataPoint{
				effectiveFrom: observation.effectiveFrom,
				metadata:      appender.metadataHandle(observation.metadataRef),
			}
			if last := len(appender.points) - 1; last >= 0 && appender.points[last].effectiveFrom == point.effectiveFrom {
				appender.points[last] = point
			} else {
				appender.points = append(appender.points, point)
			}
		}

		series.Lock()
		native := series.nativeMetadataLocked()
		if series.isGCed() || nativeMetricMetadataGroupStableResolved(native, appender.points) {
			series.Unlock()
			continue
		}
		first := native == nil
		if first {
			native = &nativeSeriesMetadata{}
			series.ensureMetadataLocked().native = native
			addedSeries++
		}
		previous := native.handle
		delta, evictions := native.mergeLocked(appender.points)
		versionDelta += int64(delta)
		evicted += uint64(evictions)
		// The resulting newest value is either the preceding newest value or
		// the final incoming value. Reuse the former's immutable pointer.
		if native.handle != previous {
			native.metadata = appender.shared[native.handle]
		}
		if first {
			stripe := s.stripe(series.ref)
			stripe.mtx.Lock()
			stripe.series[series.ref] = series
			stripe.mtx.Unlock()
		}
		series.Unlock()
	}
	// Pending samples prevent deletion until these deltas are accounted for.
	// Avoid contended per-series atomic updates, especially at the version cap.
	if addedSeries != 0 {
		s.series.Add(addedSeries)
	}
	if versionDelta != 0 {
		s.versions.Add(versionDelta)
	}
	if evicted != 0 {
		s.evictions.Add(evicted)
	}
}

// nativeMetricMetadataGroupStable compares an entire series group against one
// committed state. The caller must hold the series lock throughout the check.
func nativeMetricMetadataGroupStable(native *nativeSeriesMetadata, appender *nativeMetricMetadataAppender, refs []nativeMetricMetadataObservationRef) bool {
	if native == nil {
		return false
	}
	for _, ref := range refs {
		observation := appender.observations[ref-1]
		if observation.effectiveFrom < native.effectiveFrom || appender.metadataValue(observation.metadataRef) != *native.metadata {
			return false
		}
	}
	return true
}

func nativeMetricMetadataGroupStableResolved(native *nativeSeriesMetadata, points []nativeMetricMetadataPoint) bool {
	if native == nil {
		return false
	}
	for _, point := range points {
		if point.effectiveFrom < native.effectiveFrom || point.metadata != native.handle {
			return false
		}
	}
	return true
}

// nativeMetricMetadataStripeStable skips sorting a single-series stripe when
// all its observations repeat one committed state. Shared stripes are checked
// as series groups after sorting; independent per-observation snapshots would
// incorrectly allow intervening commits to make an A/B/A group appear stable.
func nativeMetricMetadataStripeStable(appender *nativeMetricMetadataAppender, first nativeMetricMetadataObservationRef) bool {
	series := appender.observations[first-1].series
	stripe := uint64(series.ref) % nativeMetricMetadataStripes
	if appender.multiSeriesStripes[stripe/nativeMetricMetadataBitsPerWord]&(uint64(1)<<(stripe%nativeMetricMetadataBitsPerWord)) != 0 {
		return false
	}
	series.Lock()
	defer series.Unlock()
	native := series.nativeMetadataLocked()
	if native == nil {
		return false
	}
	for ref := first; ref != 0; {
		observation := appender.observations[ref-1]
		if observation.effectiveFrom < native.effectiveFrom || appender.metadataValue(observation.metadataRef) != *native.metadata {
			return false
		}
		ref = observation.next
	}
	return true
}
