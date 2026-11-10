// Copyright 2026 The Prometheus Authors
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
	"sync/atomic"
	"time"
)

// queryTimings attributes query time to phases so that the absolute cost model
// can be measured on a running server. Every hook is gated on enabled, which is
// off unless --storage.columnar-query-timings is set: the counters are useful on
// their own and the timers must not change what they measure.
type queryTimings struct {
	enabled atomic.Bool

	match       atomic.Int64
	plan        atomic.Int64
	read        atomic.Int64
	decode      atomic.Int64
	drain       atomic.Int64
	selectTotal atomic.Int64

	drainCalls atomic.Uint64
}

// now reports the current time in nanoseconds, or zero when timing is off. The
// zero value is the signal used by add to do nothing.
func (t *queryTimings) now() int64 {
	if t == nil || !t.enabled.Load() {
		return 0
	}
	return time.Now().UnixNano()
}

// phase names the accumulator a duration belongs to. It is an enum rather than a
// field pointer so that a nil queryTimings can be consulted without taking the
// address of one of its fields.
type phase int

const (
	phaseMatch phase = iota
	phasePlan
	phaseRead
	phaseDecode
	phaseDrain
	phaseSelectTotal
)

// add accumulates the time elapsed since start, ignoring disabled samples.
func (t *queryTimings) add(p phase, start int64) {
	if t == nil || start == 0 {
		return
	}
	d := time.Now().UnixNano() - start
	switch p {
	case phaseMatch:
		t.match.Add(d)
	case phasePlan:
		t.plan.Add(d)
	case phaseRead:
		t.read.Add(d)
	case phaseDecode:
		t.decode.Add(d)
	case phaseDrain:
		t.drain.Add(d)
	case phaseSelectTotal:
		t.selectTotal.Add(d)
	}
}

// Enabled reports whether phase timing is switched on.
func (t *queryTimings) Enabled() bool { return t != nil && t.enabled.Load() }

// SetQueryTimings turns phase timing on or off.
func (s *Storage) SetQueryTimings(on bool) {
	if s.reads != nil && s.reads.timings != nil {
		s.reads.timings.enabled.Store(on)
	}
}
