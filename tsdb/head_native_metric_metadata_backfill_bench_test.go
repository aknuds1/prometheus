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
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/wlog"
	"github.com/prometheus/prometheus/util/compression"
)

// metadataBackfill describes an out-of-order backfill: series first get version
// 0 at a seed timestamp, then rounds of older samples with new versions.
// Batched rounds give each series ten consecutive older samples per
// transaction, changing version after the fifth.
type metadataBackfill struct {
	series, rounds int
	batched        bool
}

const (
	metadataBackfillSeed      = int64(1_000_000)
	metadataBackfillRoundSpan = int64(10_000)
)

func (metadataBackfill) labels(id int) labels.Labels {
	return labels.FromStrings(labels.MetricName, "backfill_seconds_total", "id", strconv.Itoa(id))
}

func (metadataBackfill) metadata(id, version int) metadata.Metadata {
	prefix := fmt.Sprintf("family %d version %d ", id%100, version)
	return metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: prefix + strings.Repeat("x", 64-len(prefix))}
}

// samples calls fn with each sample of a round's transaction for one series,
// in append order. Round 0 is the seed.
func (w metadataBackfill) samples(round int, fn func(ts int64, version int)) {
	if round == 0 {
		fn(metadataBackfillSeed, 0)
		return
	}
	start := metadataBackfillSeed - int64(round)*metadataBackfillRoundSpan
	if !w.batched {
		fn(start, round)
		return
	}
	for i := range int64(10) {
		version := 2*round - 1
		if i >= 5 {
			version = 2 * round
		}
		fn(start+i, version)
	}
}

// open returns a database with the metadata mode and an out-of-order window
// covering the backfill.
func (w metadataBackfill) open(tb testing.TB, mode string, reg prometheus.Registerer) *DB {
	opts := DefaultOptions()
	opts.EnableNativeMetadata = mode == "native"
	opts.EnableMetadataWALRecords = mode == "wal"
	opts.WALCompression = compression.Snappy
	opts.OutOfOrderTimeWindow = int64(w.rounds+1) * metadataBackfillRoundSpan
	db, err := Open(tb.TempDir(), nil, reg, opts, nil)
	require.NoError(tb, err)
	db.DisableCompactions()
	return db
}

// run appends rounds first through last, one transaction per 100 series.
func (w metadataBackfill) run(tb testing.TB, db *DB, first, last int) {
	refs := make([]storage.SeriesRef, w.series)
	for round := first; round <= last; round++ {
		for offset := 0; offset < w.series; offset += 100 {
			app := db.AppenderV2(tb.Context())
			for id := offset; id < min(offset+100, w.series); id++ {
				w.samples(round, func(ts int64, version int) {
					ref, err := app.Append(refs[id], w.labels(id), 0, ts, float64(ts), nil, nil, storage.AOptions{Metadata: w.metadata(id, version)})
					require.NoError(tb, err)
					refs[id] = ref
				})
			}
			require.NoError(tb, app.Commit())
		}
	}
}

// metadataPayload returns the decompressed bytes of Metadata records in a
// WAL directory, excluding checkpoints.
func metadataPayload(tb testing.TB, dir string) int64 {
	sr, err := wlog.NewSegmentsRangeReader(wlog.SegmentRange{Dir: dir, Last: math.MaxInt32})
	require.NoError(tb, err)
	defer sr.Close()
	var dec record.Decoder
	var n int64
	r := wlog.NewReader(sr)
	for r.Next() {
		if dec.Type(r.Record()) == record.Metadata {
			n += int64(len(r.Record()))
		}
	}
	require.NoError(tb, r.Err())
	return n
}

// BenchmarkHeadMetricMetadataBackfillWAL reports the WAL cost of metadata in an
// out-of-order backfill, after an untimed seed. It is encoding evidence:
// decompressed Metadata record bytes and compressed WAL bytes written by the
// backfill, per sample. It does not measure end-to-end forwarding.
func BenchmarkHeadMetricMetadataBackfillWAL(b *testing.B) {
	for _, batched := range []bool{false, true} {
		for _, mode := range []string{"disabled", "wal", "native"} {
			w := metadataBackfill{series: 1000, rounds: 4, batched: batched}
			b.Run(fmt.Sprintf("batched=%t/mode=%s", batched, mode), func(b *testing.B) {
				var metadataBytes, walBytes, samples float64
				for b.Loop() {
					b.StopTimer()
					reg := prometheus.NewRegistry()
					db := w.open(b, mode, reg)
					w.run(b, db, 0, 0)
					seedMetadata := metadataPayload(b, filepath.Join(db.Dir(), "wal"))
					seedWAL := walRecordBytes(b, reg)
					b.StartTimer()
					w.run(b, db, 1, w.rounds)
					b.StopTimer()
					walBytes += walRecordBytes(b, reg) - seedWAL
					require.NoError(b, db.Close())
					metadataBytes += float64(metadataPayload(b, filepath.Join(db.Dir(), "wal")) - seedMetadata)
					perSeries := 1
					if batched {
						perSeries = 10
					}
					samples += float64(w.series * w.rounds * perSeries)
				}
				b.ReportMetric(metadataBytes/samples, "metadata-B/sample")
				b.ReportMetric(walBytes/samples, "wal-B/sample")
			})
		}
	}
}

func walRecordBytes(tb testing.TB, g prometheus.Gatherer) float64 {
	families, err := g.Gather()
	require.NoError(tb, err)
	for _, family := range families {
		if family.GetName() == "prometheus_tsdb_wal_record_parts_bytes_written_total" {
			var v float64
			for _, m := range family.Metric {
				v += m.GetCounter().GetValue()
			}
			return v
		}
	}
	tb.Fatal("no WAL record bytes metric")
	return 0
}

// TestHeadMetricMetadataBackfillWAL checks that the backfill's legacy metadata
// records are exactly those its samples imply: one entry per sample whose
// metadata differs from the series' committed metadata, one record per
// transaction with entries.
func TestHeadMetricMetadataBackfillWAL(t *testing.T) {
	for _, batched := range []bool{false, true} {
		t.Run(fmt.Sprintf("batched=%t", batched), func(t *testing.T) {
			w := metadataBackfill{series: 300, rounds: 3, batched: batched}
			db := w.open(t, "wal", nil)
			w.run(t, db, 0, w.rounds)
			require.NoError(t, db.Close())
			var enc record.Encoder
			want := int64(0)
			committed := make([]int, w.series)
			for round := 0; round <= w.rounds; round++ {
				for offset := 0; offset < w.series; offset += 100 {
					entries := 0
					for id := offset; id < min(offset+100, w.series); id++ {
						last := committed[id]
						w.samples(round, func(_ int64, version int) {
							if round > 0 && version == committed[id] {
								return
							}
							m := w.metadata(id, version)
							// The seed creates series in ID order, so ID i has ref i+1.
							entry := record.RefMetadata{Ref: chunks.HeadSeriesRef(id + 1), Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: m.Help}
							want += int64(len(enc.Metadata([]record.RefMetadata{entry}, nil)) - 1)
							entries++
							last = version
						})
						committed[id] = last
					}
					if entries > 0 {
						want++
					}
				}
			}
			require.Equal(t, want, metadataPayload(t, filepath.Join(db.Dir(), "wal")))
		})
	}
}
