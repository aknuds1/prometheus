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
	maxNativeMetricMetadataValues     = 128 // Limits raw metadata retained by a transaction.
	maxNativeMetricMetadataBatch      = 256 // Limits series merged under one stripe lock.
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
	ref           chunks.HeadSeriesRef
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
	// Stripes with observations, in order of first use.
	touched         []uint8
	stripeFirst     [nativeMetricMetadataStripes]nativeMetricMetadataObservationRef
	stripeLast      [nativeMetricMetadataStripes]nativeMetricMetadataObservationRef
	lastMetadata    metadata.Metadata
	lastObservation nativeMetricMetadataObservationRef
	haveLast        bool
}

func newNativeMetricMetadataAppender() *nativeMetricMetadataAppender {
	return &nativeMetricMetadataAppender{
		observations: make([]nativeMetricMetadataObservation, 0, nativeMetricMetadataStripes),
		values:       make([]nativeMetricMetadataValue, 0, maxNativeMetricMetadataValues),
		valueRefs:    make(map[metadata.Metadata]nativeMetricMetadataValueRef, maxNativeMetricMetadataValues),
		groups:       make([]nativeMetricMetadataGroup, 0, maxNativeMetricMetadataBatch),
		touched:      make([]uint8, 0, nativeMetricMetadataStripes),
	}
}

func (a *nativeMetricMetadataAppender) metadataValue(ref nativeMetricMetadataValueRef) metadata.Metadata {
	if ref&nativeMetricMetadataDirectRefMask != 0 {
		return a.directHandles[ref&^nativeMetricMetadataDirectRefMask].Value()
	}
	return a.values[ref-1].metadata
}

// metadataHandle lazily interns raw values. Commit resolves all selected
// references before taking the stripe write lock.
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
func (a *nativeMetricMetadataAppender) metadataReference(store *nativeMetricMetadataStore, ref chunks.HeadSeriesRef, m metadata.Metadata) nativeMetricMetadataValueRef {
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
		handle         unique.Handle[metadata.Metadata]
		reuseCommitted bool
	)
	stripe := store.stripe(ref)
	stripe.mtx.RLock()
	history, ok := stripe.histories[ref]
	if ok && len(history.versions) > 0 {
		handle = history.versions[len(history.versions)-1].metadata
		reuseCommitted = handle.Value() == m
	}
	stripe.mtx.RUnlock()
	if !reuseCommitted {
		handle = unique.Make(m)
	}
	valueRef := nativeMetricMetadataDirectRefMask | nativeMetricMetadataValueRef(len(a.directHandles))
	if len(a.directHandles) == cap(a.directHandles) {
		newCapacity := max(16, 2*cap(a.directHandles))
		handles := make([]unique.Handle[metadata.Metadata], len(a.directHandles), newCapacity)
		copy(handles, a.directHandles)
		a.directHandles = handles
	}
	a.directHandles = append(a.directHandles, handle)
	return valueRef
}

// observe buffers m for the series at effectiveFrom without updating committed
// metadata. Every call adds an observation, even when m is unchanged.
func (a *nativeMetricMetadataAppender) observe(store *nativeMetricMetadataStore, ref chunks.HeadSeriesRef, effectiveFrom int64, m metadata.Metadata) {
	var metadataRef nativeMetricMetadataValueRef
	if a.haveLast && a.lastMetadata == m {
		metadataRef = a.observations[a.lastObservation-1].metadataRef
	} else {
		metadataRef = a.metadataReference(store, ref, m)
	}

	// Group observations by stripe for commit.
	stripe := uint8(uint64(ref) % nativeMetricMetadataStripes)
	observationRef := nativeMetricMetadataObservationRef(len(a.observations) + 1)
	if len(a.observations) == cap(a.observations) {
		observations := make([]nativeMetricMetadataObservation, len(a.observations), 2*cap(a.observations))
		copy(observations, a.observations)
		a.observations = observations
	}
	a.observations = append(a.observations, nativeMetricMetadataObservation{
		ref:           ref,
		effectiveFrom: effectiveFrom,
		metadataRef:   metadataRef,
	})
	if a.stripeFirst[stripe] == 0 {
		a.stripeFirst[stripe] = observationRef
		a.touched = append(a.touched, stripe)
	} else {
		a.observations[a.stripeLast[stripe]-1].next = observationRef
	}
	a.stripeLast[stripe] = observationRef

	a.lastMetadata = m
	a.lastObservation = observationRef
	a.haveLast = true
}

// selectBatchLocked selects changing groups and returns the next position.
// The caller must hold the stripe lock. Stable groups count toward the limit.
func (a *nativeMetricMetadataAppender) selectBatchLocked(stripe *nativeMetricMetadataStripe, position int) int {
	a.groups = a.groups[:0]
	for examined := 0; position < len(a.sorted) && examined < maxNativeMetricMetadataBatch; examined++ {
		end := position + 1
		ref := a.observations[a.sorted[position]-1].ref
		for end < len(a.sorted) && a.observations[a.sorted[end]-1].ref == ref {
			end++
		}
		if !nativeMetricMetadataGroupStableLocked(stripe, a, a.sorted[position:end]) {
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
	stripe := &s.stripes[stripeIndex]
	first := appender.stripeFirst[stripeIndex]
	// Skip stable stripes before collecting and sorting observation references.
	if nativeMetricMetadataStripeStable(stripe, appender, first) {
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
		case left.ref < right.ref:
			return -1
		case left.ref > right.ref:
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

	// Select changing groups before interning to avoid work for stable metadata.
	// Release the read lock while resolving values, then recheck selected groups
	// against current history under the write lock.
	for position := 0; position < len(appender.sorted); {
		stripe.mtx.RLock()
		position = appender.selectBatchLocked(stripe, position)
		stripe.mtx.RUnlock()
		if len(appender.groups) == 0 {
			continue
		}

		// Resolve handles without holding a stripe lock.
		for _, group := range appender.groups {
			for _, observationRef := range appender.sorted[group.start:group.end] {
				appender.metadataHandle(appender.observations[observationRef-1].metadataRef)
			}
		}

		stripe.mtx.Lock()
		for _, group := range appender.groups {
			observationRefs := appender.sorted[group.start:group.end]
			if nativeMetricMetadataGroupStableResolvedLocked(stripe, appender, observationRefs) {
				continue
			}
			appender.points = appender.points[:0]
			for _, observationRef := range observationRefs {
				observation := appender.observations[observationRef-1]
				point := nativeMetricMetadataPoint{
					effectiveFrom: observation.effectiveFrom,
					metadata:      appender.metadataHandle(observation.metadataRef),
				}
				if len(appender.points) > 0 && appender.points[len(appender.points)-1].effectiveFrom == point.effectiveFrom {
					appender.points[len(appender.points)-1] = point
					continue
				}
				appender.points = append(appender.points, point)
			}
			ref := appender.observations[observationRefs[0]-1].ref
			history, exists := stripe.histories[ref]
			s.mergeLocked(stripe, ref, history, exists, appender.points)
		}
		stripe.mtx.Unlock()
	}
}

// nativeMetricMetadataObservationStable reports whether observation repeats the
// newest committed value at the same or a later timestamp. A true result does
// not justify skipping it independently of other observations for the series.
// The caller must hold the stripe read or write lock protecting history.
func nativeMetricMetadataObservationStable(history nativeMetricMetadataHistory, appender *nativeMetricMetadataAppender, observation nativeMetricMetadataObservation) bool {
	if len(history.versions) == 0 {
		return false
	}
	last := history.versions[len(history.versions)-1]
	return observation.effectiveFrom >= last.effectiveFrom && appender.metadataValue(observation.metadataRef) == last.metadata.Value()
}

// nativeMetricMetadataGroupStableLocked requires the stripe read or write lock.
func nativeMetricMetadataGroupStableLocked(stripe *nativeMetricMetadataStripe, appender *nativeMetricMetadataAppender, observationRefs []nativeMetricMetadataObservationRef) bool {
	first := appender.observations[observationRefs[0]-1]
	history, ok := stripe.histories[first.ref]
	if !ok {
		return false
	}
	for _, observationRef := range observationRefs {
		if !nativeMetricMetadataObservationStable(history, appender, appender.observations[observationRef-1]) {
			return false
		}
	}
	return true
}

// nativeMetricMetadataGroupStableResolvedLocked requires the stripe lock and
// resolved metadata references.
func nativeMetricMetadataGroupStableResolvedLocked(stripe *nativeMetricMetadataStripe, appender *nativeMetricMetadataAppender, observationRefs []nativeMetricMetadataObservationRef) bool {
	first := appender.observations[observationRefs[0]-1]
	history, ok := stripe.histories[first.ref]
	if !ok || len(history.versions) == 0 {
		return false
	}
	last := history.versions[len(history.versions)-1]
	for _, observationRef := range observationRefs {
		observation := appender.observations[observationRef-1]
		if observation.effectiveFrom < last.effectiveFrom || appender.metadataHandle(observation.metadataRef) != last.metadata {
			return false
		}
	}
	return true
}

// nativeMetricMetadataStripeStable reports whether all buffered observations
// for stripe are stable against their series' committed histories. first must
// identify the start of the appender's observation chain for stripe.
// It takes the stripe read lock internally.
func nativeMetricMetadataStripeStable(stripe *nativeMetricMetadataStripe, appender *nativeMetricMetadataAppender, first nativeMetricMetadataObservationRef) bool {
	stripe.mtx.RLock()
	defer stripe.mtx.RUnlock()
	for observationRef := first; observationRef != 0; {
		observation := appender.observations[observationRef-1]
		history, ok := stripe.histories[observation.ref]
		if !ok || !nativeMetricMetadataObservationStable(history, appender, observation) {
			return false
		}
		observationRef = observation.next
	}
	return true
}
