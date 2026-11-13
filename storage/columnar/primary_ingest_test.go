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

package columnar_test

// Primary-workload ingestion baseline: high-volume OTLP/OTAP ingestion.
//
// Three layers, measured separately and never extrapolated into each other:
//
//   - direct: AppendBatch with already normalized typed columns. No transport, no
//     receiver conversion, no label translation. This is the storage mechanism.
//   - otap:   the real OTAP stream consumer (Arrow IPC decode, dictionary
//     resolution, label translation, Resource/Scope handling) feeding the real
//     native sink and storage. gRPC framing is excluded; the Arrow decode that
//     follows a received message is included.
//   - otlp:   the ordinary OTLP ingestion consumer translating in-memory pmetric
//     into the Prometheus model, against columnar and against stock TSDB. Requests
//     are already decoded; this is not an end-to-end network measurement.
//
// Every arm commits one ring entry per iteration and every ring entry carries its
// own timestamps, so no iteration can be a duplicate rejection. The ring therefore
// bounds the iteration count, and the harness fails rather than wrap it.
//
// Every logical record produces one gauge series and one cumulative-sum series, so
// all three layers carry the same number of stored samples for the same shape.
//
// All fixtures are synthetic and deterministic. WAL and flush state are named in
// every sub-benchmark path; the mechanism arms are WAL-off and flush-off, which is
// not durable ingestion.

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/storage/remote/otap"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/teststorage"
)

// primaryIntervalMs is the sample interval of the dense fixture: a 15 s scrape.
const primaryIntervalMs = int64(15_000)

// primaryMint is the first timestamp of ring entry 0. Entry e starts at
// primaryMint + e*span, so no two entries can collide.
const primaryMint = int64(1_700_000_000_000)

// primaryGaugeName and primarySumName are the metric names of the fixture. Every
// record is stored twice, once as a gauge and once as a monotonic sum, which is the
// pair of float kinds the native OTAP sink supports.
const (
	primaryGaugeName = "primary_gauge"
	primarySumName   = "primary_requests_total"
)

type ingestPresence int

const (
	presenceDense ingestPresence = iota
	presenceMissed
	presenceJitter
	presenceSparse
)

func (p ingestPresence) String() string {
	switch p {
	case presenceDense:
		return "dense"
	case presenceMissed:
		return "missed"
	case presenceJitter:
		return "jitter"
	case presenceSparse:
		return "sparse"
	}
	return "unknown"
}

type ingestResources int

const (
	resourceShared ingestResources = iota
	resourcePerSeries
)

func (r ingestResources) String() string {
	if r == resourcePerSeries {
		return "per-series-resources"
	}
	return "shared-resource"
}

type ingestMetadata int

const (
	metaNone ingestMetadata = iota
	metaUnchanged
	metaVersion
	metaMembership
)

func (m ingestMetadata) String() string {
	switch m {
	case metaNone:
		return "no-metadata"
	case metaUnchanged:
		return "metadata-unchanged"
	case metaVersion:
		return "metadata-version-change"
	case metaMembership:
		return "metadata-membership-change"
	}
	return "unknown"
}

// ingestShape is one logical corpus: a series-by-history shape, a presence pattern,
// a Resource layout and an identity mode.
type ingestShape struct {
	series    int
	perSeries int
	presence  ingestPresence
	resources ingestResources
	meta      ingestMetadata
	firstSeen bool
	// contentChurn changes the descriptive Resource attributes on alternate ring
	// entries without changing the Resource identity. It is a metadata version
	// change per commit, not a different series and not a different owner.
	contentChurn bool
	// intervalMs overrides the dense interval, which the sparse shape uses.
	intervalMs int64
	// commitsPerEntry splits one ring entry into several commits.
	commitsPerEntry int
}

func (s ingestShape) interval() int64 {
	if s.intervalMs > 0 {
		return s.intervalMs
	}
	return primaryIntervalMs
}

// span is the time one ring entry covers, so entries never overlap.
func (s ingestShape) span() int64 { return int64(s.perSeries+2) * s.interval() }

// String renders the shape as one benchmark-name segment. The separators are commas,
// not slashes: a slash would split the sub-benchmark name into further path levels and
// make -bench patterns that select a single arm silently match nothing.
func (s ingestShape) String() string {
	ids := "established"
	if s.firstSeen {
		ids = "first-seen"
	}
	// The content marker and the commit split are appended only when the shape uses
	// them, so an arm that does not change keeps its established name.
	split := ""
	if s.contentChurn {
		split = ",content=changing"
	}
	if s.commitsPerEntry > 1 {
		split += fmt.Sprintf(",commits=%d", s.commitsPerEntry)
	}
	return fmt.Sprintf("series=%d,samples=%d,presence=%s,%s,%s,%s%s",
		s.series, s.perSeries, s.presence, s.resources, s.meta, ids, split)
}

// ingestRecord is one logical series of one ring entry: its identity, its Resource
// association and its samples. It becomes a gauge series and a sum series.
//
// The Resource identity label is part of the series identity, exactly as the native
// OTAP sink writes it, so a record that moves to a different Resource becomes a new
// series rather than re-labelling an existing one.
type ingestRecord struct {
	labels labels.Labels
	attrs  map[string]string
	owner  columnar.OwnerKey
	ts     []int64
	vs     []float64
}

// ingestEntry is one commit's worth of logical input.
type ingestEntry struct {
	records []ingestRecord
	// samples is the number of data points the store must receive: two per
	// (record, timestamp).
	samples int
	// series is the number of metric series the entry creates.
	series int

	updates   []columnar.AssociationUpdate
	ownerUpds []columnar.Update
}

// entryResourceAttrs returns the Resource attributes record i states in ring entry
// e. Content churn changes descriptive content on alternate entries, which keeps the
// owner identity and the series identity while stating a new Resource version.
func (s ingestShape) entryResourceAttrs(e, i int) map[string]string {
	if s.contentChurn && e%2 == 1 {
		return s.changedResourceAttrs(i)
	}
	return s.resourceAttrs(i)
}

// resourceAttrs returns the Resource attributes of record i.
func (s ingestShape) resourceAttrs(i int) map[string]string {
	if s.resources == resourceShared {
		return map[string]string{"service.name": "checkout", "service.instance.id": "instance-0"}
	}
	return map[string]string{"service.name": "checkout", "service.instance.id": "instance-" + strconv.Itoa(i)}
}

// changedResourceAttrs is the same Resource with changed descriptive attributes: an
// owner version change, not a different owner.
func (s ingestShape) changedResourceAttrs(i int) map[string]string {
	attrs := copyAttributes(s.resourceAttrs(i))
	attrs["deployment.environment"] = "prod"
	return attrs
}

// movedResourceAttrs is a different Resource identity, so a record that uses it is a
// different series with a different owner: a membership change.
func (s ingestShape) movedResourceAttrs(i int) map[string]string {
	attrs := copyAttributes(s.resourceAttrs(i))
	attrs["service.instance.id"] += "-moved"
	attrs["deployment.environment"] = "prod"
	return attrs
}

func copyAttributes(attrs map[string]string) map[string]string {
	out := make(map[string]string, len(attrs))
	maps.Copy(out, attrs)
	return out
}

// seriesLabels returns the labels of record i for ring entry e. First-seen shapes get
// a fresh identity per entry, so every append creates a new series.
func (s ingestShape) seriesLabels(e, i int, attrs map[string]string, owner columnar.OwnerKey) labels.Labels {
	id := i
	if s.firstSeen {
		id = e*s.series + i
	}
	return labels.FromStrings(
		labels.MetricName, primarySumName,
		"job", "checkout",
		"instance", attrs["service.instance.id"],
		"path", "/api/v"+strconv.Itoa(id%7),
		"code", strconv.Itoa(200+id%5),
		"series", strconv.Itoa(id),
		columnar.OTELResourceLabel, owner.Ref(),
	)
}

// entryTimestamps returns record i's timestamps in ring entry e.
func (s ingestShape) entryTimestamps(e, i int) []int64 {
	base := primaryMint + int64(e)*s.span()
	interval := s.interval()
	out := make([]int64, 0, s.perSeries)
	for j := range s.perSeries {
		switch s.presence {
		case presenceMissed:
			// One missed scrape in ten: the delta around each gap repeats, which is
			// the shape the format-10 delta-of-delta timestamp codec has to encode.
			if j%10 == 5 {
				continue
			}
			out = append(out, base+int64(j)*interval)
		case presenceJitter:
			// Per-series sub-interval offsets: no two series share a timestamp
			// vector, so no timestamp cohort can be shared.
			offset := int64((i*7919)%int(interval/2)) - interval/4
			out = append(out, base+int64(j)*interval+offset)
		default:
			out = append(out, base+int64(j)*interval)
		}
	}
	slices.Sort(out)
	return out
}

// buildIngestRing materializes ringSize entries of the shape. Entries are built
// outside the timed loop because they are fixture data, not ingestion work.
func buildIngestRing(shape ingestShape, ringSize int) []ingestEntry {
	ring := make([]ingestEntry, ringSize)
	for e := range ring {
		entry := ingestEntry{records: make([]ingestRecord, 0, shape.series)}
		for i := range shape.series {
			ts := shape.entryTimestamps(e, i)
			if len(ts) == 0 {
				continue
			}
			vs := make([]float64, len(ts))
			for j := range vs {
				vs[j] = float64(e)*1.5 + float64(j)*0.25 + float64(i%97)
			}
			attrs := shape.entryResourceAttrs(e, i)
			if shape.meta == metaMembership && e%2 == 1 && i%2 == 0 {
				// Alternate entries move half the records to a different Resource: a
				// membership change, not a descriptive version change. The identity
				// label moves with them, so each membership is its own series.
				attrs = shape.movedResourceAttrs(i)
			}
			owner := columnar.ResourceOwner(attrs)
			entry.records = append(entry.records, ingestRecord{
				labels: shape.seriesLabels(e, i, attrs, owner),
				attrs:  attrs,
				owner:  owner,
				ts:     ts,
				vs:     vs,
			})
			entry.samples += 2 * len(ts)
			entry.series += 2
		}
		entry.updates, entry.ownerUpds = shape.metadataFor(entry.records)
		ring[e] = entry
	}
	return ring
}

// metadataFor returns the association and owner updates of one entry. Associations
// are stated once per series and entry, which is what re-observing unchanged metadata
// costs; an owner version change is a single owner-scoped update, because it changes
// descriptive content without changing any membership.
func (s ingestShape) metadataFor(records []ingestRecord) ([]columnar.AssociationUpdate, []columnar.Update) {
	if s.meta == metaNone {
		return nil, nil
	}
	updates := make([]columnar.AssociationUpdate, 0, len(records))
	for _, rec := range records {
		updates = append(updates, columnar.AssociationUpdate{
			Labels: rec.labels,
			Update: columnar.Update{
				Owner:         rec.owner,
				Content:       columnar.Content{Attributes: rec.attrs},
				EffectiveFrom: rec.ts[0],
			},
		})
	}
	if s.meta != metaVersion {
		return updates, nil
	}
	mid := records[0].ts[len(records[0].ts)/2]
	var ownerUpdates []columnar.Update
	seen := map[columnar.OwnerKey]struct{}{}
	for _, rec := range records {
		if _, done := seen[rec.owner]; done {
			continue
		}
		seen[rec.owner] = struct{}{}
		ownerUpdates = append(ownerUpdates, columnar.Update{
			Owner:         rec.owner,
			Content:       columnar.Content{Attributes: s.changedResourceAttrs(0)},
			EffectiveFrom: mid,
		})
	}
	return updates, ownerUpdates
}

// primaryRingSize is the number of distinct commits one benchmark may perform.
func primaryRingSize(tb testing.TB) int {
	tb.Helper()
	v := os.Getenv("PRIMARY_INGEST_RING")
	if v == "" {
		return 8
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		tb.Fatalf("PRIMARY_INGEST_RING=%q is not a positive integer", v)
	}
	return n
}

// primaryRingFor bounds the ring for the largest shapes: their iteration count is
// deliberately smaller and is reported rather than extrapolated.
func primaryRingFor(tb testing.TB, entrySamples int) int {
	ring := primaryRingSize(tb)
	if entrySamples >= 800_000 && ring > 4 {
		return 4
	}
	return ring
}

// primaryRingForReceiver bounds the ring for the receiver arms, which are roughly two
// orders of magnitude more expensive per sample than the direct arms at this HEAD. The
// smaller iteration count is reported with the result.
func primaryRingForReceiver(tb testing.TB, entrySamples int) int {
	ring := primaryRingSize(tb)
	if entrySamples >= 800_000 && ring > 2 {
		return 2
	}
	return ring
}

// requireRing fails instead of wrapping the ring: a wrapped ring would re-append
// identical samples and measure duplicate rejection rather than ingestion.
func requireRing(tb testing.TB, b *testing.B, ring int) {
	tb.Helper()
	if b.N > ring {
		b.Fatalf("b.N=%d exceeds the %d-entry ring; lower -benchtime to %dx so no iteration repeats a commit",
			b.N, ring, ring)
	}
}

// primaryShapes is the executed shape matrix, fixed here before any timing so a
// shape cannot be added afterwards to fit a result.
func primaryShapes() []struct {
	name  string
	shape ingestShape
} {
	return []struct {
		name  string
		shape ingestShape
	}{
		{"A-100x1000", ingestShape{series: 100, perSeries: 1_000}},
		{"B-1000x100", ingestShape{series: 1_000, perSeries: 100}},
		{"C-10000x10", ingestShape{series: 10_000, perSeries: 10}},
		{"B-missed", ingestShape{series: 1_000, perSeries: 100, presence: presenceMissed}},
		{"B-jitter", ingestShape{series: 1_000, perSeries: 100, presence: presenceJitter}},
		{"B-sparse", ingestShape{series: 1_000, perSeries: 10, intervalMs: 10 * primaryIntervalMs}},
		{"D-5000x100", ingestShape{series: 5_000, perSeries: 100}},
	}
}

// directRing converts the ring into AppendBatch commits. An entry may be split into
// shape.commitsPerEntry commits to measure messages per commit.
func directRing(shape ingestShape, entries []ingestEntry) [][]columnar.Batch {
	out := make([][]columnar.Batch, len(entries))
	for e, entry := range entries {
		out[e] = splitEntry(shape, entry)
	}
	return out
}

// splitEntry converts one entry into one or more commits and attaches the entry's
// metadata to the first commit. Records are dealt round-robin so every commit
// carries a spread of identities rather than one contiguous range.
func splitEntry(shape ingestShape, entry ingestEntry) []columnar.Batch {
	perCommit := max(shape.commitsPerEntry, 1)
	batches := make([]columnar.Batch, perCommit)
	for i, rec := range entry.records {
		target := i % perCommit
		gaugeLabels := labels.NewBuilder(rec.labels).Set(labels.MetricName, primaryGaugeName).Labels()
		batches[target].Series = append(batches[target].Series,
			columnar.BatchSeries{
				Labels:     gaugeLabels,
				Timestamps: rec.ts,
				Kinds:      repeatKind(columnar.SampleFloat, len(rec.ts)),
				Values:     rec.vs,
			},
			columnar.BatchSeries{
				Labels:     rec.labels,
				Timestamps: rec.ts,
				Kinds:      repeatKind(columnar.SampleFloat, len(rec.ts)),
				Values:     sumValues(rec.vs),
			})
	}
	batches[0].Updates = entry.updates
	batches[0].OwnerUpdates = entry.ownerUpds
	return batches
}

// sumValues converts gauge values to counter values, so the two series of a record
// never hold identical numbers.
func sumValues(vs []float64) []float64 {
	out := make([]float64, len(vs))
	for i, v := range vs {
		out[i] = v * 100
	}
	return out
}

func repeatKind(k columnar.SampleKind, n int) []columnar.SampleKind {
	out := make([]columnar.SampleKind, n)
	for i := range out {
		out[i] = k
	}
	return out
}

// newPrimaryStorage opens the columnar store of the mechanism arms: WAL off and a
// flush threshold far above the fixture.
func newPrimaryStorage(tb testing.TB) *columnar.Storage {
	tb.Helper()
	st, err := columnar.NewStorage(tb.TempDir(), columnar.StorageOptions{
		FlushSamples: 1 << 40,
		DisableWAL:   true,
	})
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = st.Close() })
	return st
}

// openPrimaryTSDB opens the stock control with the WAL disabled, so the head append
// path is the measured mechanism on both arms.
func openPrimaryTSDB(tb testing.TB) *teststorage.TestStorage {
	tb.Helper()
	st := teststorage.New(tb, func(o *tsdb.Options) { o.WALSegmentSize = -1 })
	tb.Cleanup(func() { _ = st.Close() })
	return st
}

// reportEntryMetrics publishes the per-entry accounting of an arm after StopTimer,
// because ReportMetric before ResetTimer is discarded on this Go version.
func reportEntryMetrics(b *testing.B, entrySamples, entrySeries int) {
	b.ReportMetric(float64(entrySamples), "samples/op")
	b.ReportMetric(float64(entrySeries), "series/op")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(int64(b.N)*int64(entrySamples)), "ns/sample")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(int64(b.N)*int64(entrySeries)), "ns/series")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N), "ns/batch")
	b.ReportMetric(heapAllocMiB(), "heap-MiB")
}

// BenchmarkPrimaryIngestDirect measures the storage batch mechanism: already
// normalized typed columns, one AppendBatch per ring entry, WAL and flush off.
func BenchmarkPrimaryIngestDirect(b *testing.B) {
	ringSize := primaryRingSize(b)
	for _, tc := range primaryShapes() {
		for _, firstSeen := range []bool{false, true} {
			shape := tc.shape
			shape.firstSeen = firstSeen
			entries := buildIngestRing(shape, ringSize)
			ring := primaryRingFor(b, entries[0].samples)
			entries = entries[:ring]
			batches := directRing(shape, entries)

			b.Run(fmt.Sprintf("%s/%s", tc.name, shape), func(b *testing.B) {
				requireRing(b, b, ring)
				st := newPrimaryStorage(b)

				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					for _, batch := range batches[i] {
						if err := st.AppendBatch(context.Background(), batch); err != nil {
							b.Fatalf("AppendBatch: %v", err)
						}
					}
				}
				b.StopTimer()

				reportEntryMetrics(b, entries[0].samples, entries[0].series)
				stats := st.ReadStats()
				b.ReportMetric(float64(stats.CommitCount), "commits")
				b.ReportMetric(float64(stats.HashCallsPrepare), "prepare-hashes")
				b.ReportMetric(float64(stats.HashCallsSeriesFor), "appender-hashes")
				b.ReportMetric(float64(stats.SealCount), "seals")
			})
		}
	}
}

// BenchmarkPrimaryIngestDirectWAL is the same shape with the WAL enabled, so
// publication is measured separately from the head path. /tmp is tmpfs: this is
// in-process protocol and CPU evidence, not physical-disk fsync evidence.
func BenchmarkPrimaryIngestDirectWAL(b *testing.B) {
	ringSize := primaryRingSize(b)
	shape := ingestShape{series: 1_000, perSeries: 100}
	entries := buildIngestRing(shape, ringSize)
	ring := primaryRingFor(b, entries[0].samples)
	entries = entries[:ring]
	batches := directRing(shape, entries)

	b.Run("B-1000x100/wal=on/flush=off", func(b *testing.B) {
		requireRing(b, b, ring)
		st, err := columnar.NewStorage(b.TempDir(), columnar.StorageOptions{FlushSamples: 1 << 40})
		require.NoError(b, err)
		defer func() { require.NoError(b, st.Close()) }()

		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			for _, batch := range batches[i] {
				if err := st.AppendBatch(context.Background(), batch); err != nil {
					b.Fatalf("AppendBatch: %v", err)
				}
			}
		}
		b.StopTimer()
		reportEntryMetrics(b, entries[0].samples, entries[0].series)
		b.ReportMetric(float64(st.ReadStats().CommitCount), "commits")
	})
}

// BenchmarkPrimaryIngestDirectSeal measures the commits that seal the buffer: the
// same shape with the flush threshold set to one entry.
func BenchmarkPrimaryIngestDirectSeal(b *testing.B) {
	ringSize := primaryRingSize(b)
	shape := ingestShape{series: 1_000, perSeries: 100}
	entries := buildIngestRing(shape, ringSize)
	ring := primaryRingFor(b, entries[0].samples)
	entries = entries[:ring]
	batches := directRing(shape, entries)

	b.Run("B-1000x100/wal=off/flush=every-entry", func(b *testing.B) {
		requireRing(b, b, ring)
		st, err := columnar.NewStorage(b.TempDir(), columnar.StorageOptions{
			FlushSamples: entries[0].samples,
			DisableWAL:   true,
		})
		require.NoError(b, err)
		defer func() { require.NoError(b, st.Close()) }()

		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			for _, batch := range batches[i] {
				if err := st.AppendBatch(context.Background(), batch); err != nil {
					b.Fatalf("AppendBatch: %v", err)
				}
			}
		}
		b.StopTimer()
		reportEntryMetrics(b, entries[0].samples, entries[0].series)
		stats := st.ReadStats()
		b.ReportMetric(float64(stats.CommitCount), "commits")
		b.ReportMetric(float64(stats.SealCount), "seals")
		b.ReportMetric(float64(stats.Blocks), "blocks")
		b.ReportMetric(float64(stats.SealNanos)/float64(max(stats.SealCount, 1)), "seal-ns")
	})
}

// BenchmarkPrimaryIngestMetadata separates unchanged repeated metadata, an owner
// version change and a membership change at a fixed corpus.
func BenchmarkPrimaryIngestMetadata(b *testing.B) {
	ringSize := primaryRingSize(b)
	for _, resources := range []ingestResources{resourceShared, resourcePerSeries} {
		for _, meta := range []ingestMetadata{metaNone, metaUnchanged, metaVersion, metaMembership} {
			shape := ingestShape{series: 1_000, perSeries: 100, resources: resources, meta: meta}
			entries := buildIngestRing(shape, ringSize)
			ring := primaryRingFor(b, entries[0].samples)
			entries = entries[:ring]
			batches := directRing(shape, entries)

			b.Run(fmt.Sprintf("%s/%s", resources, meta), func(b *testing.B) {
				requireRing(b, b, ring)
				st := newPrimaryStorage(b)

				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					for _, batch := range batches[i] {
						if err := st.AppendBatch(context.Background(), batch); err != nil {
							b.Fatalf("AppendBatch: %v", err)
						}
					}
				}
				b.StopTimer()
				reportEntryMetrics(b, entries[0].samples, entries[0].series)
				b.ReportMetric(float64(len(entries[0].updates)), "associations/op")
				b.ReportMetric(float64(len(entries[0].ownerUpds)), "owner-updates/op")
			})
		}
	}
}

// BenchmarkPrimaryIngestSplit measures messages per commit: the same 100,000
// samples as one commit and as ten.
func BenchmarkPrimaryIngestSplit(b *testing.B) {
	ringSize := primaryRingSize(b)
	for _, commits := range []int{1, 10} {
		shape := ingestShape{series: 1_000, perSeries: 100, commitsPerEntry: commits}
		entries := buildIngestRing(shape, ringSize)
		ring := primaryRingFor(b, entries[0].samples)
		entries = entries[:ring]
		batches := directRing(shape, entries)

		b.Run(fmt.Sprintf("B-1000x100/commits=%d", commits), func(b *testing.B) {
			requireRing(b, b, ring)
			st := newPrimaryStorage(b)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				for _, batch := range batches[i] {
					if err := st.AppendBatch(context.Background(), batch); err != nil {
						b.Fatalf("AppendBatch: %v", err)
					}
				}
			}
			b.StopTimer()
			reportEntryMetrics(b, entries[0].samples, entries[0].series)
			b.ReportMetric(float64(st.ReadStats().CommitCount), "commits")
		})
	}
}

// BenchmarkPrimaryIngestOTAP measures the native OTAP route: one real batch per
// iteration through the real stream consumer (Arrow IPC decode and dictionary
// resolution) into the real sink and storage. Network framing is not included.
func BenchmarkPrimaryIngestOTAP(b *testing.B) {
	ringSize := primaryRingSize(b)
	for _, tc := range []struct {
		name  string
		shape ingestShape
	}{
		{"B-1000x100/shared-resource", ingestShape{series: 1_000, perSeries: 100}},
		{"B-1000x100/first-seen", ingestShape{series: 1_000, perSeries: 100, firstSeen: true}},
		{"B-1000x100/changing-content", ingestShape{series: 1_000, perSeries: 100, contentChurn: true}},
		{"B-1000x100/per-series-resources", ingestShape{series: 1_000, perSeries: 100, resources: resourcePerSeries}},
		{"B-missed", ingestShape{series: 1_000, perSeries: 100, presence: presenceMissed}},
		{"B-sparse", ingestShape{series: 1_000, perSeries: 10, intervalMs: 10 * primaryIntervalMs}},
		{"A-100x1000", ingestShape{series: 100, perSeries: 1_000}},
		{"D-5000x100", ingestShape{series: 5_000, perSeries: 100}},
	} {
		entries := buildIngestRing(tc.shape, ringSize)
		ring := primaryRingForReceiver(b, entries[0].samples)
		entries = entries[:ring]
		batches := otapRing(b, entries)

		b.Run(tc.name, func(b *testing.B) {
			requireRing(b, b, ring)
			st := newPrimaryStorage(b)
			// The standalone sink settings: UTF-8 names, no suffixes, no target_info.
			sink := columnar.NewOTAPSink(st, slog.New(slog.DiscardHandler))
			stream := otap.NewStreamConsumer(nil, otap.WithRecordSink(sink))
			defer func() { require.NoError(b, stream.Close()) }()

			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if err := stream.ConsumeBatch(context.Background(), batches[i]); err != nil {
					b.Fatalf("ConsumeBatch: %v", err)
				}
			}
			b.StopTimer()
			reportEntryMetrics(b, entries[0].samples, entries[0].series)
			b.ReportMetric(float64(st.ReadStats().CommitCount), "commits")
			b.ReportMetric(float64(st.ReadStats().HashCallsPrepare), "prepare-hashes")
			reportNativeDerivation(b, st.ReadStats().NativeDerivation)
		})
	}
}

// reportNativeDerivation publishes the native builder's mechanism accounting after
// the timed loop, so the counters describe the whole run rather than one sample.
func reportNativeDerivation(b *testing.B, stats columnar.NativeDerivationStats) {
	sites := stats.AssocKeyHashes
	b.ReportMetric(float64(stats.Samples), "derivation-samples")
	b.ReportMetric(float64(stats.Series), "derivation-series")
	b.ReportMetric(float64(stats.Resources), "resources")
	b.ReportMetric(float64(stats.OwnersByKind[0]), "resource-owners")
	b.ReportMetric(float64(stats.OwnersByKind[2]), "metric-owners")
	b.ReportMetric(float64(stats.SeriesLookupHashes), "series-hashes")
	b.ReportMetric(float64(sites[0]+sites[1]+sites[2]+sites[3]+sites[4]), "assoc-hashes")
	b.ReportMetric(float64(stats.ContentCanonicalizations), "canonicalizations")
	b.ReportMetric(float64(stats.ContentDerivations), "content-derivations")
	b.ReportMetric(float64(stats.SchemaContentDerivations), "schema-contents")
	b.ReportMetric(float64(stats.OwnerReferences), "owner-refs")
	b.ReportMetric(float64(stats.LabelBuilds), "label-builds")
	b.ReportMetric(float64(stats.EpochOwnerChecks), "epoch-owner-checks")
	b.ReportMetric(float64(stats.EpochMembershipChecks), "epoch-membership-checks")
	b.ReportMetric(float64(stats.EmittedChangePoints), "emitted-change-points")
	b.ReportMetric(float64(stats.EmittedObservationBounds), "emitted-bounds")
	b.ReportMetric(float64(stats.FoldedObservations), "folded-observations")
	b.ReportMetric(float64(stats.EpochRestatements), "epoch-restatements")
}

// otapRing encodes the ring as OTAP batches once, outside the timed loop, with one
// producer so the stream state matches a real client's successive batches.
func otapRing(tb testing.TB, entries []ingestEntry) []*colarspb.BatchArrowRecords {
	tb.Helper()
	producer := arrow_record.NewProducer()
	tb.Cleanup(func() { require.NoError(tb, producer.Close()) })

	out := make([]*colarspb.BatchArrowRecords, len(entries))
	for e, entry := range entries {
		md := otapMetrics(entry)
		batch, err := producer.BatchArrowRecordsFromMetrics(md)
		require.NoError(tb, err)
		out[e] = batch
	}
	return out
}

// otapMetrics builds the pmetric input of one entry: one ResourceMetrics per
// Resource, carrying a gauge and a cumulative sum with the fixture's samples.
func otapMetrics(entry ingestEntry) pmetric.Metrics {
	md := pmetric.NewMetrics()
	byResource := map[string][]ingestRecord{}
	var order []string
	for _, rec := range entry.records {
		key := rec.owner.String()
		if _, ok := byResource[key]; !ok {
			order = append(order, key)
		}
		byResource[key] = append(byResource[key], rec)
	}
	for _, key := range order {
		records := byResource[key]
		rm := md.ResourceMetrics().AppendEmpty()
		for k, v := range records[0].attrs {
			rm.Resource().Attributes().PutStr(k, v)
		}
		sm := rm.ScopeMetrics().AppendEmpty()
		sm.Scope().SetName("prometheus.primary.ingest")
		sm.Scope().SetVersion("1.0.0")

		gauge := sm.Metrics().AppendEmpty()
		gauge.SetName(primaryGaugeName)
		gauge.SetUnit("1")
		gaugePoints := gauge.SetEmptyGauge().DataPoints()

		sum := sm.Metrics().AppendEmpty()
		sum.SetName(primarySumName)
		sum.SetUnit("1")
		summer := sum.SetEmptySum()
		summer.SetIsMonotonic(true)
		summer.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		sumPoints := summer.DataPoints()

		for _, rec := range records {
			seriesID := rec.labels.Get("series")
			counters := sumValues(rec.vs)
			for j, t := range rec.ts {
				ts := pcommon.Timestamp(t * int64(time.Millisecond))
				dp := gaugePoints.AppendEmpty()
				dp.SetTimestamp(ts)
				dp.SetDoubleValue(rec.vs[j])
				dp.Attributes().PutStr("series", seriesID)
				sdp := sumPoints.AppendEmpty()
				sdp.SetTimestamp(ts)
				sdp.SetIntValue(int64(counters[j]))
				sdp.Attributes().PutStr("series", seriesID)
			}
		}
	}
	return md
}

// otlpRing builds the same logical input directly as pmetric, which is what the
// ordinary OTLP consumer receives after request decoding.
func otlpRing(entries []ingestEntry) []pmetric.Metrics {
	out := make([]pmetric.Metrics, len(entries))
	for e, entry := range entries {
		out[e] = otapMetrics(entry)
	}
	return out
}

// BenchmarkPrimaryIngestOTLP measures the ordinary OTLP ingestion consumer against
// columnar and against stock TSDB. Requests are already decoded: no protobuf and no
// network are involved.
func BenchmarkPrimaryIngestOTLP(b *testing.B) {
	ringSize := primaryRingSize(b)
	for _, tc := range []struct {
		name  string
		shape ingestShape
	}{
		{"B-1000x100", ingestShape{series: 1_000, perSeries: 100}},
		{"B-sparse", ingestShape{series: 1_000, perSeries: 10, intervalMs: 10 * primaryIntervalMs}},
		{"C-10000x10", ingestShape{series: 10_000, perSeries: 10}},
	} {
		entries := buildIngestRing(tc.shape, ringSize)
		ring := primaryRingFor(b, entries[0].samples)
		entries = entries[:ring]
		metrics := otlpRing(entries)

		for _, engine := range []string{"columnar", "tsdb"} {
			b.Run(fmt.Sprintf("%s/engine=%s", tc.name, engine), func(b *testing.B) {
				requireRing(b, b, ring)
				var appendable storage.AppendableV2
				if engine == "columnar" {
					appendable = newPrimaryStorage(b)
				} else {
					appendable = openPrimaryTSDB(b)
				}
				c := remote.NewMetricsConsumer(
					slog.New(slog.DiscardHandler),
					prometheus.NewRegistry(),
					appendable,
					func() config.Config { return config.Config{OTLPConfig: config.DefaultOTLPConfig} },
					remote.OTLPOptions{LookbackDelta: 5 * time.Minute},
				)

				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					if err := c.ConsumeMetrics(context.Background(), metrics[i]); err != nil {
						b.Fatalf("ConsumeMetrics: %v", err)
					}
				}
				b.StopTimer()
				reportEntryMetrics(b, entries[0].samples, entries[0].series)
			})
		}
	}
}

// TestPrimaryIngestEquivalence proves each arm stores the whole fixture exactly once
// and that the engines agree, outside any timed loop. It records the stored shape
// each benchmark is timed against.
func TestPrimaryIngestEquivalence(t *testing.T) {
	ringSize := envInt(t, "PRIMARY_INGEST_RING", 3)
	for _, tc := range []struct {
		name  string
		shape ingestShape
	}{
		{"B-dense", ingestShape{series: 200, perSeries: 40}},
		{"B-missed", ingestShape{series: 200, perSeries: 40, presence: presenceMissed}},
		{"B-jitter", ingestShape{series: 200, perSeries: 40, presence: presenceJitter}},
		{"B-sparse", ingestShape{series: 200, perSeries: 10, intervalMs: 10 * primaryIntervalMs}},
		{"B-first-seen", ingestShape{series: 200, perSeries: 40, firstSeen: true}},
		{"B-metadata-unchanged", ingestShape{series: 200, perSeries: 40, meta: metaUnchanged}},
		{"B-metadata-version", ingestShape{series: 200, perSeries: 40, meta: metaVersion}},
		{"B-metadata-membership", ingestShape{series: 200, perSeries: 40, meta: metaMembership}},
		{"B-per-series-resources", ingestShape{series: 200, perSeries: 40, resources: resourcePerSeries}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := buildIngestRing(tc.shape, ringSize)
			direct := directRing(tc.shape, entries)
			metrics := otlpRing(entries)

			wantSamples := 0
			identities := map[string]struct{}{}
			for _, entry := range entries {
				wantSamples += entry.samples
				for _, rec := range entry.records {
					identities[rec.labels.String()] = struct{}{}
				}
			}
			require.NotZero(t, wantSamples)
			// One gauge and one sum series per distinct logical identity. First-seen
			// shapes and membership changes both add identities, established shapes
			// reuse the same ones.
			wantSeries := 2 * len(identities)

			colDirect := newPrimaryStorage(t)
			for _, batches := range direct {
				for _, batch := range batches {
					require.NoError(t, colDirect.AppendBatch(context.Background(), batch))
				}
			}

			colOTLP := newPrimaryStorage(t)
			colConsumer := remote.NewMetricsConsumer(
				slog.New(slog.DiscardHandler), prometheus.NewRegistry(), colOTLP,
				func() config.Config { return config.Config{OTLPConfig: config.DefaultOTLPConfig} },
				remote.OTLPOptions{LookbackDelta: 5 * time.Minute},
			)
			tsdbControl := openPrimaryTSDB(t)
			tsdbConsumer := remote.NewMetricsConsumer(
				slog.New(slog.DiscardHandler), prometheus.NewRegistry(), tsdbControl,
				func() config.Config { return config.Config{OTLPConfig: config.DefaultOTLPConfig} },
				remote.OTLPOptions{LookbackDelta: 5 * time.Minute},
			)
			for i := range entries {
				require.NoError(t, colConsumer.ConsumeMetrics(context.Background(), metrics[i]))
				require.NoError(t, tsdbConsumer.ConsumeMetrics(context.Background(), metrics[i]))
			}

			mint, maxt := fixtureRange(entries)
			directSeries, directSamples := countStored(t, colDirect, mint, maxt, "")
			colSeries, colSamples := countStored(t, colOTLP, mint, maxt, "primary_")
			tsdbSeries, tsdbSamples := countStored(t, tsdbControl, mint, maxt, "primary_")

			require.Equal(t, wantSeries, directSeries, "direct: one gauge and one sum series per logical identity")
			require.Equal(t, wantSamples, directSamples, "direct: every fixture data point is stored once")

			colValues := canonicalStored(t, colOTLP, mint, maxt, true)
			tsdbValues := canonicalStored(t, tsdbControl, mint, maxt, true)
			require.Equal(t, wantSeries, colSeries, "OTLP route: one gauge and one sum series per logical identity")
			require.Equal(t, tsdbValues, colValues, "columnar and TSDB store identical samples through the OTLP route")
			require.Equal(t, wantSamples, colSamples, "columnar OTLP route: every fixture data point stored once")
			require.Equal(t, colSeries, tsdbSeries, "columnar and TSDB agree on the stored series count")

			t.Logf("%s: entries=%d fixtureSamples=%d direct(series=%d samples=%d) otlpColumnar(series=%d samples=%d) otlpTSDB(series=%d samples=%d)",
				tc.name, len(entries), wantSamples, directSeries, directSamples, colSeries, colSamples, tsdbSeries, tsdbSamples)
		})
	}
}

// TestPrimaryIngestNativeOTAPEquivalence proves the native OTAP route stores the whole
// fixture exactly once, that its stored samples match the direct arm's, and that no
// sample is skipped silently.
func TestPrimaryIngestNativeOTAPEquivalence(t *testing.T) {
	ringSize := envInt(t, "PRIMARY_INGEST_RING", 3)
	for _, tc := range []struct {
		name  string
		shape ingestShape
	}{
		{"B-dense", ingestShape{series: 200, perSeries: 40}},
		{"B-missed", ingestShape{series: 200, perSeries: 40, presence: presenceMissed}},
		{"B-per-series-resources", ingestShape{series: 200, perSeries: 40, resources: resourcePerSeries}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := buildIngestRing(tc.shape, ringSize)
			batches := otapRing(t, entries)

			st := newPrimaryStorage(t)
			sink := columnar.NewOTAPSink(st, slog.New(slog.DiscardHandler))
			stream := otap.NewStreamConsumer(nil, otap.WithRecordSink(sink))
			t.Cleanup(func() { require.NoError(t, stream.Close()) })
			for i := range batches {
				require.NoError(t, stream.ConsumeBatch(context.Background(), batches[i]),
					"the native route must accept every supported sample")
			}

			wantSamples := 0
			for _, entry := range entries {
				wantSamples += entry.samples
			}
			mint, maxt := fixtureRange(entries)
			series, samples := countStored(t, st, mint, maxt, "")
			require.Equal(t, entries[0].series, series, "one gauge and one sum series per logical record")
			require.Equal(t, wantSamples, samples, "every fixture data point is stored once; none skipped")

			// Complete sample comparison against the direct arm, label-free: the sink
			// renames metrics, so the timestamps and values are what must agree.
			direct := newPrimaryStorage(t)
			for _, batch := range directRing(tc.shape, entries) {
				for _, b := range batch {
					require.NoError(t, direct.AppendBatch(context.Background(), b))
				}
			}
			require.Equal(t, sampleDigest(t, direct, mint, maxt, false), sampleDigest(t, st, mint, maxt, true),
				"native OTAP route stores the same timestamps and values as the direct route")
			t.Logf("%s: entries=%d fixtureSamples=%d nativeOTAP(series=%d samples=%d)", tc.name, len(entries), wantSamples, series, samples)
		})
	}
}

// fixtureRange covers every entry's timestamps.
func fixtureRange(entries []ingestEntry) (int64, int64) {
	mint, maxt := int64(math.MaxInt64), int64(math.MinInt64)
	for _, entry := range entries {
		for _, rec := range entry.records {
			mint = min(mint, rec.ts[0])
			maxt = max(maxt, rec.ts[len(rec.ts)-1])
		}
	}
	return mint, maxt
}

// countStored drains a storage and returns the selected series and sample counts.
// A non-empty namePrefix restricts the count to the fixture's own metric names, so a
// series an engine adds on its own (the OTLP translator's target_info) is reported
// separately instead of being silently added to the fixture total.
func countStored(tb testing.TB, q storage.Queryable, mint, maxt int64, namePrefix string) (int, int) {
	tb.Helper()
	querier, err := q.Querier(mint, maxt)
	require.NoError(tb, err)
	defer func() { require.NoError(tb, querier.Close()) }()
	matcher := labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+")
	if namePrefix != "" {
		matcher = labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, "^"+namePrefix+".*")
	}
	return drainSelection(tb, querier, []*labels.Matcher{matcher})
}

// canonicalStored renders complete stored samples per series. When skipTargetInfo is
// set, the translator's target_info stream is excluded: it has no fixture
// counterpart in the direct arm.
func canonicalStored(tb testing.TB, q storage.Queryable, mint, maxt int64, skipTargetInfo bool) []string {
	tb.Helper()
	querier, err := q.Querier(mint, maxt)
	require.NoError(tb, err)
	defer func() { require.NoError(tb, querier.Close()) }()

	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+"))
	var out []string
	for set.Next() {
		s := set.At()
		if skipTargetInfo && s.Labels().Get(labels.MetricName) == "target_info" {
			continue
		}
		var b strings.Builder
		b.WriteString(s.Labels().String())
		it := s.Iterator(nil)
		for vt := it.Next(); vt != chunkenc.ValNone; vt = it.Next() {
			ts, v := it.At()
			fmt.Fprintf(&b, " %d=%s", ts, strconv.FormatFloat(v, 'g', -1, 64))
		}
		require.NoError(tb, it.Err())
		out = append(out, b.String())
	}
	require.NoError(tb, set.Err())
	sort.Strings(out)
	return out
}

// sampleDigest renders every stored (timestamp, value) pair as a sorted multiset, so
// two arms with different label translation can still be compared for completeness.
func sampleDigest(tb testing.TB, q storage.Queryable, mint, maxt int64, skipTargetInfo bool) []string {
	tb.Helper()
	querier, err := q.Querier(mint, maxt)
	require.NoError(tb, err)
	defer func() { require.NoError(tb, querier.Close()) }()

	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+"))
	var out []string
	for set.Next() {
		s := set.At()
		if skipTargetInfo && s.Labels().Get(labels.MetricName) == "target_info" {
			continue
		}
		it := s.Iterator(nil)
		for vt := it.Next(); vt != chunkenc.ValNone; vt = it.Next() {
			ts, v := it.At()
			out = append(out, strconv.FormatInt(ts, 10)+"="+strconv.FormatFloat(v, 'g', -1, 64))
		}
		require.NoError(tb, it.Err())
	}
	require.NoError(tb, set.Err())
	sort.Strings(out)
	return out
}
