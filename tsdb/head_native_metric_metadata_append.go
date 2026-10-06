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
	"strconv"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
)

const (
	nativeMetricMetadataBitsPerWord   = 64
	maxNativeMetricMetadataValues     = 128 // Limits raw metadata retained by a transaction.
	maxNativeMetricMetadataBatch      = 256 // Bounds value preparation before merging a batch.
	nativeMetricMetadataDirectRefMask = nativeMetricMetadataValueRef(1 << 31)
)

// nativeMetricMetadataValue retains raw metadata for comparisons without
// cloning. Once resolved, owned identifies an immutable copy of the same value.
type nativeMetricMetadataValue struct {
	metadata metadata.Metadata
	owned    *metadata.Metadata
}

// nativeMetricMetadataValueRef selects a one-based raw value, or a zero-based
// direct owned value when the high bit is set. Zero is not a value reference.
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
	observations []nativeMetricMetadataObservation
	values       []nativeMetricMetadataValue
	valueRefs    map[metadata.Metadata]nativeMetricMetadataValueRef
	directValues []*metadata.Metadata
	valueCache   *nativeMetricMetadataValueCache
	// Observation references for the stripe being committed.
	sorted []nativeMetricMetadataObservationRef
	// Reusable merge input for the current series, with unique timestamps.
	points []nativeMetricMetadataPoint
	// Changing series selected for the current batch.
	groups []nativeMetricMetadataGroup
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
	// Reusable WAL record contents for the pre-log pass. Values retain the
	// strings of the transaction's values.
	walValues  []record.NativeMetadataValue
	walEntries []record.RefCompactNativeMetadata
	walPoints  []record.CompactNativeMetadataPoint
	// One-based indices into walValues by raw value reference and by direct
	// value index; zero before a value's first use in the record.
	walRawIndices    [maxNativeMetricMetadataValues + 1]uint32
	walDirectIndices []uint32
}

func newNativeMetricMetadataAppender(cache *nativeMetricMetadataValueCache) *nativeMetricMetadataAppender {
	return &nativeMetricMetadataAppender{
		observations: make([]nativeMetricMetadataObservation, 0, nativeMetricMetadataStripes),
		values:       make([]nativeMetricMetadataValue, 0, maxNativeMetricMetadataValues),
		valueRefs:    make(map[metadata.Metadata]nativeMetricMetadataValueRef, maxNativeMetricMetadataValues),
		groups:       make([]nativeMetricMetadataGroup, 0, maxNativeMetricMetadataBatch),
		valueCache:   cache,
		touched:      make([]uint8, 0, nativeMetricMetadataStripes),
	}
}

func (a *nativeMetricMetadataAppender) metadataValue(ref nativeMetricMetadataValueRef) metadata.Metadata {
	if ref&nativeMetricMetadataDirectRefMask != 0 {
		return *a.directValues[ref&^nativeMetricMetadataDirectRefMask]
	}
	return a.values[ref-1].metadata
}

// metadataPointer lazily owns raw values. Commit resolves selected references
// before taking a series lock.
func (a *nativeMetricMetadataAppender) metadataPointer(series *memSeries, ref nativeMetricMetadataValueRef) *metadata.Metadata {
	if ref&nativeMetricMetadataDirectRefMask != 0 {
		return a.directValues[ref&^nativeMetricMetadataDirectRefMask]
	}
	value := &a.values[ref-1]
	if value.owned == nil {
		value.owned = a.resolveMetadata(series, value.metadata)
	}
	return value.owned
}

// metadataReference returns a transaction-local reference to m. It deduplicates
// a bounded set of raw values, deferring their cloning until needed. Values
// outside that set use direct owned pointers.
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

	valueRef := nativeMetricMetadataDirectRefMask | nativeMetricMetadataValueRef(len(a.directValues))
	a.directValues = append(a.directValues, a.resolveMetadata(series, m))
	return valueRef
}

// resolveMetadata reuses immutable values still owned by the series before
// consulting the shared cache. The caller must not hold the series lock.
func (a *nativeMetricMetadataAppender) resolveMetadata(series *memSeries, m metadata.Metadata) *metadata.Metadata {
	var retained [maxNativeMetricMetadataVersions]*metadata.Metadata
	var count int
	series.Lock()
	if native := series.nativeMetadataLocked(); native != nil {
		retained[0] = native.Metadata
		count = 1
		for _, point := range native.Older {
			retained[count] = point.Metadata
			count++
		}
	}
	series.Unlock()

	// Pointers keep the immutable values alive across concurrent truncation or
	// deletion. Reusing a value does not reuse its observation's timestamp.
	for _, value := range retained[:count] {
		if len(value.Type) != len(m.Type) || len(value.Unit) != len(m.Unit) || len(value.Help) != len(m.Help) {
			continue
		}
		// Reject long common-prefix misses cheaply; matching suffixes are not
		// proof of equality. Short strings go straight to the full comparison.
		const minSuffixCheckBytes, suffixBytes = 64, 8
		if len(m.Help) >= minSuffixCheckBytes && value.Help[len(value.Help)-suffixBytes:] != m.Help[len(m.Help)-suffixBytes:] {
			continue
		}
		if *value == m {
			return value
		}
	}
	return a.valueCache.resolve(m)
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

// sortStripe collects a stripe's observation chain into sorted, ordered by
// series and timestamp, then by append order so the last observation wins when
// equal timestamps are collapsed during merging.
func (a *nativeMetricMetadataAppender) sortStripe(first nativeMetricMetadataObservationRef) {
	a.sorted = a.sorted[:0]
	for observationRef := first; observationRef != 0; observationRef = a.observations[observationRef-1].next {
		a.sorted = append(a.sorted, observationRef)
	}
	compare := func(x, y nativeMetricMetadataObservationRef) int {
		left := a.observations[x-1]
		right := a.observations[y-1]
		switch {
		case left.series.ref < right.series.ref:
			return -1
		case left.series.ref > right.series.ref:
			return 1
		case left.effectiveFrom < right.effectiveFrom:
			return -1
		case left.effectiveFrom > right.effectiveFrom:
			return 1
		case x < y:
			return -1
		case x > y:
			return 1
		default:
			return 0
		}
	}
	if !slices.IsSortedFunc(a.sorted, compare) {
		slices.SortFunc(a.sorted, compare)
	}
}

// appendWALRecord returns the contents of a compact record of one merge group
// entry per series whose observations the commit stability checks find
// unstable before logging, with the points commit merges: deduplicated per
// timestamp, the last observation winning. Points index the returned values,
// which hold each value reference used once, in order of first use: the
// caller's raw values, or owned values beyond the transaction's bounded set.
// The results are valid until the appender is reset.
// Without concurrent writers to a series, commit merges exactly these groups.
func (a *nativeMetricMetadataAppender) appendWALRecord() ([]record.NativeMetadataValue, []record.RefCompactNativeMetadata) {
	a.walValues, a.walEntries, a.walPoints = a.walValues[:0], a.walEntries[:0], a.walPoints[:0]
	clear(a.walRawIndices[:len(a.values)+1])
	a.walDirectIndices = slices.Grow(a.walDirectIndices[:0], len(a.directValues))[:len(a.directValues)]
	clear(a.walDirectIndices)
	for _, stripe := range a.touched {
		first := a.stripeFirst[stripe]
		if nativeMetricMetadataStripeStable(a, first) {
			continue
		}
		a.sortStripe(first)
		for position := 0; position < len(a.sorted); {
			end := position + 1
			series := a.observations[a.sorted[position]-1].series
			for end < len(a.sorted) && a.observations[a.sorted[end]-1].series.ref == series.ref {
				end++
			}
			series.Lock()
			stable := nativeMetricMetadataGroupStable(series.nativeMetadataLocked(), a, a.sorted[position:end])
			series.Unlock()
			if !stable {
				// Points hold value references until the group is reduced, so
				// that replaced points add no values.
				start := len(a.walPoints)
				for _, observationRef := range a.sorted[position:end] {
					observation := a.observations[observationRef-1]
					point := record.CompactNativeMetadataPoint{EffectiveFrom: observation.effectiveFrom, Value: uint32(observation.metadataRef)}
					if last := len(a.walPoints) - 1; last >= start && a.walPoints[last].EffectiveFrom == point.EffectiveFrom {
						a.walPoints[last] = point
					} else {
						a.walPoints = append(a.walPoints, point)
					}
				}
				for i := start; i < len(a.walPoints); i++ {
					a.walPoints[i].Value = a.walValueIndex(nativeMetricMetadataValueRef(a.walPoints[i].Value))
				}
				a.walEntries = append(a.walEntries, record.RefCompactNativeMetadata{
					Ref: series.ref, Kind: record.NativeMetadataGroup,
					Points: a.walPoints[start:len(a.walPoints):len(a.walPoints)],
				})
			}
			position = end
		}
	}
	return a.walValues, a.walEntries
}

// walValueIndex returns the index of ref's value in the WAL record's values,
// adding the value on the reference's first use.
func (a *nativeMetricMetadataAppender) walValueIndex(ref nativeMetricMetadataValueRef) uint32 {
	var index *uint32
	if ref&nativeMetricMetadataDirectRefMask != 0 {
		index = &a.walDirectIndices[ref&^nativeMetricMetadataDirectRefMask]
	} else {
		index = &a.walRawIndices[ref]
	}
	if *index == 0 {
		m := a.metadataValue(ref)
		a.walValues = append(a.walValues, record.NativeMetadataValue{Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: m.Help})
		*index = uint32(len(a.walValues))
	}
	return *index - 1
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
		a := appender.(*nativeMetricMetadataAppender)
		a.valueCache = s.values
		return a
	}
	return newNativeMetricMetadataAppender(s.values)
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
	clear(appender.directValues)
	appender.directValues = appender.directValues[:0]
	appender.valueCache = nil
	appender.sorted = appender.sorted[:0]
	clear(appender.points[:cap(appender.points)])
	appender.points = appender.points[:0]
	appender.groups = appender.groups[:0]
	appender.touched = appender.touched[:0]
	appender.lastMetadata = metadata.Metadata{}
	appender.lastObservation = 0
	appender.haveLast = false
	clear(appender.walValues)
	appender.walValues = appender.walValues[:0]
	clear(appender.walEntries)
	appender.walEntries = appender.walEntries[:0]
	appender.walPoints = appender.walPoints[:0]
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

	appender.sortStripe(first)

	// Prepare immutable values without a series lock, then recheck and merge
	// under that lock. Concurrent commits may have changed the current point.
	for position := 0; position < len(appender.sorted); {
		position = appender.selectBatch(position)
		for _, group := range appender.groups {
			for _, observationRef := range appender.sorted[group.start:group.end] {
				observation := appender.observations[observationRef-1]
				appender.metadataPointer(observation.series, observation.metadataRef)
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
				EffectiveFrom: observation.effectiveFrom,
				Metadata:      appender.metadataPointer(series, observation.metadataRef),
			}
			if last := len(appender.points) - 1; last >= 0 && appender.points[last].EffectiveFrom == point.EffectiveFrom {
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
			native = createNativeMetadataLocked(series)
			addedSeries++
		}
		delta, evictions := native.mergeLocked(appender.points)
		versionDelta += int64(delta)
		evicted += uint64(evictions)
		if first {
			s.publishLocked(series)
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

// createNativeMetadataLocked attaches empty native state to a series without
// any. The caller holds the series lock, merges, and then publishes the series.
func createNativeMetadataLocked(series *memSeries) *nativeSeriesMetadata {
	if strconv.IntSize == 64 && series.metadata.Load() == nil {
		// Keep native-first state next to its sidecar in one allocation.
		// Combining them on 32-bit builds would increase allocator bytes.
		allocation := &struct {
			sidecar memSeriesMetadata
			native  nativeSeriesMetadata
		}{}
		allocation.sidecar.native = &allocation.native
		series.metadata.Store(&allocation.sidecar)
		return &allocation.native
	}
	// Never replace a sidecar already published by legacy metadata.
	native := &nativeSeriesMetadata{}
	series.ensureMetadataLocked().native = native
	return native
}

// nativeMetricMetadataGroupStable compares an entire series group against one
// committed state. The caller must hold the series lock throughout the check.
func nativeMetricMetadataGroupStable(native *nativeSeriesMetadata, appender *nativeMetricMetadataAppender, refs []nativeMetricMetadataObservationRef) bool {
	if native == nil {
		return false
	}
	for _, ref := range refs {
		observation := appender.observations[ref-1]
		if observation.effectiveFrom < native.EffectiveFrom || appender.metadataValue(observation.metadataRef) != *native.Metadata {
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
		if point.EffectiveFrom < native.EffectiveFrom || !equalNativeMetricMetadata(point.Metadata, native.Metadata) {
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
		if observation.effectiveFrom < native.EffectiveFrom || appender.metadataValue(observation.metadataRef) != *native.Metadata {
			return false
		}
		ref = observation.next
	}
	return true
}
