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

//go:build linux || darwin

package tsdb

import (
	"testing"

	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
)

// The W1 writer adapter for unmodified ACTE in WAL-only mode: legacy Metadata
// records, whose entries appends build. It logs no native metadata records,
// so only encoding, compression and G1 are comparable.

// w1Transaction is a staged transaction and the legacy entries its appends
// build.
type w1Transaction struct {
	txn     []w1Observation
	refs    []storage.SeriesRef
	entries []record.RefMetadata
}

type w1Staged = *w1Transaction

func w1WriterUnavailable(string) string { return "no native metadata records" }

func w1ComponentUnavailable(component, workload string) string {
	switch {
	case component == "construct":
		return "appends build legacy entries"
	case workload == "mp":
		return "legacy records hold one value per series"
	}
	return ""
}

func w1CommitMode() string { return "wal" }

func w1MetadataRecordType(t record.Type) bool { return t == record.Metadata }

func w1Stage(_ testing.TB, _ *DB, refs []storage.SeriesRef, txn []w1Observation) w1Staged {
	return &w1Transaction{txn: txn, refs: refs}
}

// w1Construct builds the legacy entries the appends build: one per changed
// series.
func w1Construct(s w1Staged) {
	s.entries = s.entries[:0]
	for _, o := range s.txn {
		s.entries = append(s.entries, record.RefMetadata{Ref: chunks.HeadSeriesRef(s.refs[o.slot]), Type: record.GetMetricType(o.m.Type), Unit: o.m.Unit, Help: o.m.Help})
	}
}

func w1Encode(s w1Staged, buf []byte) []byte {
	var enc record.Encoder
	return enc.Metadata(s.entries, buf)
}

func w1Release(*DB, w1Staged) int { return 0 }

func w1DecodeGroups(tb testing.TB, _ []byte) ([]w1Group, int) {
	tb.Fatal("no native metadata records")
	return nil, 0
}
