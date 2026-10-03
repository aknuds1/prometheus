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

package nativemetadata

import (
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/record"
)

// State is a history reduced from WAL entries, with native storage's truncation
// flag: set once the version cap evicts a version.
type State struct {
	History
	Truncated bool
}

// Apply applies one decoded WAL entry, whose points carry immutable values.
// A group merges in one call, as native storage merges a transaction's points
// for a series. An override replaces the history and truncation flag. A legacy
// entry replaces the history with its one point and keeps the flag. Entries of
// unknown kind, and invalid groups and overrides, apply as single-point
// overrides with their newest point; Apply reports them.
func (s *State) Apply(kind record.NativeMetadataKind, truncated bool, points []Point) (unknown bool) {
	switch kind {
	case record.NativeMetadataLegacy:
		if len(points) == 1 {
			s.Replace(points)
			return false
		}
	case record.NativeMetadataGroup:
		if len(points) > 0 && ascending(points) {
			if _, evictions := s.Merge(points); evictions > 0 {
				s.Truncated = true
			}
			return false
		}
	case record.NativeMetadataOverride:
		if len(points) <= MaxVersions && ascending(points) {
			s.Replace(points)
			s.Truncated = truncated
			return false
		}
	}
	s.Replace(points[max(0, len(points)-1):])
	s.Truncated = truncated
	return true
}

func ascending(points []Point) bool {
	for i := 1; i < len(points); i++ {
		if points[i-1].EffectiveFrom >= points[i].EffectiveFrom {
			return false
		}
	}
	return true
}

// AppendRecordPoints appends decoded points to dst, resolving each value with
// intern, which must return an immutable value equal to its argument.
func AppendRecordPoints(dst []Point, points []record.RefNativeMetadataPoint, intern func(metadata.Metadata) *metadata.Metadata) []Point {
	for _, p := range points {
		dst = append(dst, Point{EffectiveFrom: p.EffectiveFrom, Metadata: intern(metadata.Metadata{
			Type: record.ToMetricType(p.Type),
			Unit: p.Unit,
			Help: p.Help,
		})})
	}
	return dst
}

// AppendRecordPoint appends p to dst as a decoded point.
func AppendRecordPoint(dst []record.RefNativeMetadataPoint, p Point) []record.RefNativeMetadataPoint {
	return append(dst, record.RefNativeMetadataPoint{
		EffectiveFrom: p.EffectiveFrom,
		Type:          record.GetMetricType(p.Metadata.Type),
		Unit:          p.Metadata.Unit,
		Help:          p.Metadata.Help,
	})
}
