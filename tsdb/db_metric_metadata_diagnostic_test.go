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

// This file is a diagnostic fixture, not a production benchmark proposal.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"golang.org/x/sys/unix"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/wlog"
	"github.com/prometheus/prometheus/util/compression"
)

const (
	metadataDiagnosticBuild = "ACTE"
	metadataDiagnosticBase  = int64(1801300000000)
	metadataDiagnosticEnv   = "PROMETHEUS_METADATA_DIAGNOSTIC_"
)

type metadataDiagnosticConfig struct {
	Mode, Pattern                            string
	Series, Values, Sweeps, CommitSize, Rate int
	// HelpBytes sets the Help length; zero selects the original 64 bytes.
	HelpBytes int `json:",omitempty"`
}

func (c metadataDiagnosticConfig) helpBytes() int {
	if c.HelpBytes == 0 {
		return 64
	}
	return c.HelpBytes
}

func (c metadataDiagnosticConfig) validate() error {
	if c.Mode != "native" && c.Mode != "wal" && c.Mode != "off" {
		return fmt.Errorf("unknown metadata mode %q", c.Mode)
	}
	if c.Pattern != "reused" && c.Pattern != "observation" {
		return fmt.Errorf("unknown backing pattern %q", c.Pattern)
	}
	if c.HelpBytes < 0 || c.helpBytes() < len(fmt.Sprintf("family %d version 0 ", c.Values-1)) {
		return fmt.Errorf("help too short for values: %+v", c)
	}
	if c.Series <= 0 || c.Values <= 0 || c.Values > c.Series || c.Sweeps <= 0 || c.CommitSize <= 0 || c.Series%c.CommitSize != 0 || c.Rate <= 0 {
		return fmt.Errorf("invalid fixed work: %+v", c)
	}
	if metadataDiagnosticBuild != "ACTE" && c.Mode != "native" {
		return errors.New("bypass builds are only valid for native diagnostics")
	}
	return nil
}

type metadataDiagnosticFixture struct {
	config           metadataDiagnosticConfig
	db               *DB
	dir              string
	labels           []labels.Labels
	values, inputs   []metadata.Metadata
	refs             []storage.SeriesRef
	lateTransactions int
	maxLateness      time.Duration
}

func newMetadataDiagnosticFixture(tb testing.TB, c metadataDiagnosticConfig) *metadataDiagnosticFixture {
	tb.Helper()
	if err := c.validate(); err != nil {
		tb.Fatal(err)
	}
	f := &metadataDiagnosticFixture{config: c, dir: tb.TempDir(), labels: make([]labels.Labels, c.Series), values: make([]metadata.Metadata, c.Values), inputs: make([]metadata.Metadata, c.CommitSize), refs: make([]storage.SeriesRef, c.Series)}
	for i := range f.labels {
		f.labels[i] = labels.FromStrings(labels.MetricName, "pipeline_seconds_total", "id", strconv.Itoa(i), "job", "pipeline", "cluster", "benchmark", "region", "local", "environment", "test")
	}
	for i := range f.values {
		prefix := fmt.Sprintf("family %d version 0 ", i)
		f.values[i] = metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: prefix + strings.Repeat("x", c.helpBytes()-len(prefix))}
	}
	opts := DefaultOptions()
	opts.EnableNativeMetadata = c.Mode == "native"
	opts.EnableMetadataWALRecords = c.Mode == "wal"
	opts.WALCompression = compression.Snappy
	opts.EnableExemplarStorage = false
	var err error
	f.db, err = Open(f.dir, nil, prometheus.NewRegistry(), opts, nil)
	if err != nil {
		tb.Fatal(err)
	}
	f.db.DisableCompactions()
	tb.Cleanup(func() {
		if f.db != nil {
			if err := f.db.Close(); err != nil {
				tb.Error(err)
			}
			f.db = nil
		}
	})
	return f
}

// seed uses real sample appends and commits. Explicit native observations let
// bypass builds start from the same state without a runtime hot-path switch.
func (f *metadataDiagnosticFixture) seed(ctx context.Context, ordinary bool) error {
	c := f.config
	for begin := 0; begin < c.Series; begin += c.CommitSize {
		app := f.db.AppenderV2(ctx)
		for i := begin; i < begin+c.CommitSize; i++ {
			m := f.values[i%c.Values]
			opts := storage.AOptions{Metadata: m}
			if c.Mode == "native" && !ordinary {
				opts.Metadata = metadata.Metadata{}
			}
			ref, err := app.Append(0, f.labels[i], 0, metadataDiagnosticBase, 1, nil, nil, opts)
			if err != nil {
				return errors.Join(err, app.Rollback())
			}
			f.refs[i] = ref
			if c.Mode == "native" && !ordinary {
				headApp := app.(dbAppenderV2).AppenderV2
				if init, ok := headApp.(*initAppenderV2); ok {
					headApp = init.app
				}
				s := f.db.head.series.getByID(chunks.HeadSeriesRef(ref))
				headApp.(*headAppenderV2).recordNativeMetricMetadata(s, metadataDiagnosticBase, m)
			}
		}
		if err := app.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (f *metadataDiagnosticFixture) prepare(begin int) {
	if f.config.Pattern == "reused" {
		return
	}
	for i := range f.inputs {
		m := f.values[(begin+i)%f.config.Values]
		f.inputs[i] = metadata.Metadata{Type: model.MetricType(strings.Clone(string(m.Type))), Unit: strings.Clone(m.Unit), Help: strings.Clone(m.Help)}
	}
}

// ingest keeps pacing and producer allocation inside CPU accounting. It does
// not collect per-transaction latency, profiles, or diagnostic path counters.
func (f *metadataDiagnosticFixture) ingest(ctx context.Context) error {
	c := f.config
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	start := time.Now()
	for step := 1; step <= c.Sweeps; step++ {
		for begin := 0; begin < c.Series; begin += c.CommitSize {
			if err := ctx.Err(); err != nil {
				return err
			}
			scheduled := (step-1)*c.Series + begin
			deadline := start.Add(time.Duration(scheduled) * time.Second / time.Duration(c.Rate))
			if delay := time.Until(deadline); delay > 0 {
				timer.Reset(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			late := max(0, time.Since(deadline))
			if late > time.Millisecond {
				f.lateTransactions++
			}
			f.maxLateness = max(f.maxLateness, late)
			f.prepare(begin)
			app := f.db.AppenderV2(ctx)
			for i := begin; i < begin+c.CommitSize; i++ {
				m := f.values[i%c.Values]
				if c.Pattern == "observation" {
					m = f.inputs[i-begin]
				}
				ref, err := app.Append(f.refs[i], f.labels[i], 0, metadataDiagnosticBase+int64(step)*15000, float64(step+1), nil, nil, storage.AOptions{Metadata: m})
				if err != nil {
					return errors.Join(err, app.Rollback())
				}
				f.refs[i] = ref
			}
			if err := app.Commit(); err != nil {
				return err
			}
			clear(f.inputs)
		}
	}
	return nil
}

type metadataDiagnosticState struct {
	NativeSeries, Versions          int64
	NativeOwnedValues, LegacySeries int
	BackingEqual                    [3]int
	Evictions                       uint64
	NativeOwnership                 string
}

func (f *metadataDiagnosticFixture) validateState(tb testing.TB) metadataDiagnosticState {
	tb.Helper()
	c, h := f.config, f.db.head
	if h.NumSeries() != uint64(c.Series) {
		tb.Fatalf("series count: %d", h.NumSeries())
	}
	r := metadataDiagnosticState{}
	owned := map[*metadata.Metadata]uint64{}
	ownership := sha256.New()
	if h.nativeMetricMetadata != nil {
		r.NativeSeries, r.Versions, r.Evictions = h.nativeMetricMetadata.series.Load(), h.nativeMetricMetadata.versions.Load(), h.nativeMetricMetadata.evictions.Load()
	}
	for i, ref := range f.refs {
		s := h.series.getByID(chunks.HeadSeriesRef(ref))
		if s == nil {
			tb.Fatalf("missing series %d", ref)
		}
		s.Lock()
		native, legacy := s.nativeMetadataLocked(), s.legacyMetadataLocked()
		pending := s.pendingCommitCount()
		s.Unlock()
		if pending != 0 {
			tb.Fatalf("pending commit on %d", ref)
		}
		m := f.values[i%c.Values]
		if c.Mode == "native" {
			if native == nil || *native.metadata != m || native.effectiveFrom != metadataDiagnosticBase || len(native.older) != 0 || native.flags.Load() != 0 {
				tb.Fatalf("invalid native state for %d", ref)
			}
			id := owned[native.metadata]
			if id == 0 {
				id = uint64(len(owned) + 1)
				owned[native.metadata] = id
			}
			var encoded [8]byte
			binary.LittleEndian.PutUint64(encoded[:], id)
			_, _ = ownership.Write(encoded[:])
			for field, pair := range [][2]string{{string(native.metadata.Type), string(m.Type)}, {native.metadata.Unit, m.Unit}, {native.metadata.Help, m.Help}} {
				if unsafe.StringData(pair[0]) == unsafe.StringData(pair[1]) {
					r.BackingEqual[field]++
				}
			}
			if h.nativeMetricMetadata.indexedSeries(s.ref) != s {
				tb.Fatalf("missing native directory membership for %d", ref)
			}
		} else if native != nil {
			tb.Fatalf("unexpected native state for %d", ref)
		}
		if c.Mode == "wal" {
			if legacy == nil || *legacy != m {
				tb.Fatalf("invalid legacy state for %d", ref)
			}
			r.LegacySeries++
		} else if legacy != nil {
			tb.Fatalf("unexpected legacy state for %d", ref)
		}
	}
	r.NativeOwnedValues = len(owned)
	if c.Mode == "native" {
		r.NativeOwnership = hex.EncodeToString(ownership.Sum(nil))
		if r.NativeSeries != int64(c.Series) || r.Versions != int64(c.Series) || r.Evictions != 0 || r.NativeOwnedValues < c.Values || r.NativeOwnedValues > c.Series {
			tb.Fatalf("invalid native totals: %+v", r)
		}
	} else if h.nativeMetricMetadata != nil {
		tb.Fatal("unexpected native store")
	}
	for _, m := range f.inputs {
		if m != (metadata.Metadata{}) {
			tb.Fatal("producer backing was not released")
		}
	}
	return r
}

func (f *metadataDiagnosticFixture) validateSamples(tb testing.TB) {
	tb.Helper()
	c := f.config
	q, err := f.db.Querier(metadataDiagnosticBase, metadataDiagnosticBase+int64(c.Sweeps)*15000)
	if err != nil {
		tb.Fatal(err)
	}
	defer q.Close()
	set := q.Select(tb.Context(), true, nil, labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "pipeline_seconds_total"))
	seen := make([]bool, c.Series)
	count := 0
	for set.Next() {
		s := set.At()
		id, err := strconv.Atoi(s.Labels().Get("id"))
		if err != nil || id < 0 || id >= c.Series || seen[id] || !labels.Equal(f.labels[id], s.Labels()) {
			tb.Fatal("invalid series labels")
		}
		seen[id] = true
		it, step := s.Iterator(nil), 0
		for typ := it.Next(); typ != chunkenc.ValNone; typ = it.Next() {
			t, v := it.At()
			if typ != chunkenc.ValFloat || t != metadataDiagnosticBase+int64(step)*15000 || v != float64(step+1) {
				tb.Fatalf("invalid sample for series %d step %d", id, step)
			}
			step++
		}
		if it.Err() != nil || step != c.Sweeps+1 {
			tb.Fatalf("sample count/error: %d %v", step, it.Err())
		}
		count++
	}
	if set.Err() != nil || count != c.Series {
		tb.Fatalf("query validation: %d %v", count, set.Err())
	}
}

type metadataDiagnosticWAL struct{ Series, Samples, Metadata int }

func (f *metadataDiagnosticFixture) validateWAL(tb testing.TB) metadataDiagnosticWAL {
	tb.Helper()
	sr, err := wlog.NewSegmentsReader(filepath.Join(f.dir, "wal"))
	if err != nil {
		tb.Fatal(err)
	}
	defer sr.Close()
	r := wlog.NewReader(sr)
	d := record.NewDecoder(nil, nil)
	c := f.config
	ids := make(map[chunks.HeadSeriesRef]int, c.Series)
	steps := make([]int, c.Series)
	seenMetadata := make([]bool, c.Series)
	var series []record.RefSeries
	var samples []record.RefSample
	var metas []record.RefMetadata
	out := metadataDiagnosticWAL{}
	for r.Next() {
		data := r.Record()
		switch d.Type(data) {
		case record.Series:
			series, err = d.Series(data, series[:0])
			if err != nil {
				tb.Fatal(err)
			}
			for _, s := range series {
				id, e := strconv.Atoi(s.Labels.Get("id"))
				_, exists := ids[s.Ref]
				if e != nil || id < 0 || id >= c.Series || exists || !labels.Equal(s.Labels, f.labels[id]) || storage.SeriesRef(s.Ref) != f.refs[id] {
					tb.Fatal("invalid WAL series")
				}
				ids[s.Ref] = id
				out.Series++
			}
		case record.Samples, record.SamplesV2:
			samples, err = d.Samples(data, samples[:0])
			if err != nil {
				tb.Fatal(err)
			}
			for _, s := range samples {
				id, ok := ids[s.Ref]
				if !ok || s.T != metadataDiagnosticBase+int64(steps[id])*15000 || s.V != float64(steps[id]+1) || s.ST != 0 {
					tb.Fatal("invalid WAL sample")
				}
				steps[id]++
				out.Samples++
			}
		case record.Metadata:
			metas, err = d.Metadata(data, metas[:0])
			if err != nil {
				tb.Fatal(err)
			}
			for _, m := range metas {
				id, ok := ids[m.Ref]
				want := f.values[id%c.Values]
				if !ok || c.Mode != "wal" || seenMetadata[id] || m.Type != record.GetMetricType(want.Type) || m.Unit != want.Unit || m.Help != want.Help {
					tb.Fatal("invalid WAL metadata")
				}
				seenMetadata[id] = true
				out.Metadata++
			}
		default:
			tb.Fatalf("unexpected WAL record %d", d.Type(data))
		}
	}
	if r.Err() != nil {
		tb.Fatal(r.Err())
	}
	if out.Series != c.Series || out.Samples != c.Series*(c.Sweeps+1) || (c.Mode == "wal" && out.Metadata != c.Series) || (c.Mode != "wal" && out.Metadata != 0) {
		tb.Fatalf("invalid WAL totals: %+v", out)
	}
	for _, n := range steps {
		if n != c.Sweeps+1 {
			tb.Fatal("invalid per-series WAL count")
		}
	}
	return out
}

type metadataDiagnosticCPU struct {
	User, System                                                     int64
	MinorFaults, MajorFaults, VoluntarySwitches, InvoluntarySwitches int64
}

func metadataDiagnosticReadCPU() (metadataDiagnosticCPU, error) {
	var r unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &r); err != nil {
		return metadataDiagnosticCPU{}, err
	}
	return metadataDiagnosticCPU{r.Utime.Nano(), r.Stime.Nano(), metadataDiagnosticCount(r.Minflt), metadataDiagnosticCount(r.Majflt), metadataDiagnosticCount(r.Nvcsw), metadataDiagnosticCount(r.Nivcsw)}, nil
}

// metadataDiagnosticCount widens rusage counters, which are 32-bit on 386.
func metadataDiagnosticCount[T int32 | int64](v T) int64 {
	return int64(v)
}

func (r metadataDiagnosticCPU) sub(b metadataDiagnosticCPU) metadataDiagnosticCPU {
	return metadataDiagnosticCPU{r.User - b.User, r.System - b.System, r.MinorFaults - b.MinorFaults, r.MajorFaults - b.MajorFaults, r.VoluntarySwitches - b.VoluntarySwitches, r.InvoluntarySwitches - b.InvoluntarySwitches}
}

type metadataDiagnosticResult struct {
	Schema, Build, Labels                                    string
	Config                                                   metadataDiagnosticConfig
	Profile                                                  bool
	Samples, Transactions                                    int
	ElapsedNS, OuterElapsedNS                                int64
	StartUnixNS, EndUnixNS, OuterStartUnixNS, OuterEndUnixNS int64
	CPU, OuterCPU                                            metadataDiagnosticCPU
	AllocatedBytes, Allocations, StartHeap, EndHeap          uint64
	StartGC, EndGC                                           uint32
	WALStart, WALEnd                                         int64
	LateTransactions                                         int
	MaxLatenessNS                                            int64
	State                                                    metadataDiagnosticState
	WAL                                                      metadataDiagnosticWAL
}

// BenchmarkDBMetricMetadataUnchangedDiagnostic executes one fixed trace in a
// fresh process. CPU accounting includes pacing and input preparation, not setup.
func BenchmarkDBMetricMetadataUnchangedDiagnostic(b *testing.B) {
	b.StopTimer()
	if b.N != 1 {
		b.Fatal("use -benchtime=1x and fresh processes for repetitions")
	}
	c := metadataDiagnosticConfig{Mode: "native", Pattern: "reused", Series: 10000, Values: 100, Sweeps: 200, CommitSize: 500, Rate: 500000}
	if raw := os.Getenv(metadataDiagnosticEnv + "CONFIG"); raw != "" {
		d := json.NewDecoder(strings.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&c); err != nil {
			b.Fatal(err)
		}
	}
	f := newMetadataDiagnosticFixture(b, c)
	if err := f.seed(b.Context(), false); err != nil {
		b.Fatal(err)
	}
	seedState := f.validateState(b)
	r := metadataDiagnosticResult{Schema: "unchanged-diagnostic-1", Build: metadataDiagnosticBuild, Labels: labels.ImplementationName, Config: c, Samples: c.Series * c.Sweeps, Transactions: c.Series * c.Sweeps / c.CommitSize}
	r.WALStart = metricMetadataBenchmarkWALPosition(b, f.db.head.wal)
	var profile *os.File
	if path := os.Getenv(metadataDiagnosticEnv + "PROFILE"); path != "" {
		var err error
		profile, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			b.Fatal(err)
		}
		defer profile.Close()
		r.Profile = true
	}
	var before, after runtime.MemStats
	b.ReportAllocs()
	b.StartTimer()
	runtime.ReadMemStats(&before)
	outerStartCPU, err := metadataDiagnosticReadCPU()
	if err != nil {
		b.Fatal(err)
	}
	outerStart := time.Now()
	if profile != nil {
		if err := pprof.StartCPUProfile(profile); err != nil {
			b.Fatal(err)
		}
	}
	startCPU, err := metadataDiagnosticReadCPU()
	if err != nil {
		if profile != nil {
			pprof.StopCPUProfile()
		}
		b.Fatal(err)
	}
	start := time.Now()
	appendErr := f.ingest(b.Context())
	end := time.Now()
	endCPU, cpuErr := metadataDiagnosticReadCPU()
	if profile != nil {
		pprof.StopCPUProfile()
	}
	outerEnd := time.Now()
	outerEndCPU, outerCPUErr := metadataDiagnosticReadCPU()
	runtime.ReadMemStats(&after)
	b.StopTimer()
	if err := errors.Join(appendErr, cpuErr, outerCPUErr); err != nil {
		b.Fatal(err)
	}
	r.ElapsedNS, r.OuterElapsedNS = end.Sub(start).Nanoseconds(), outerEnd.Sub(outerStart).Nanoseconds()
	r.StartUnixNS, r.EndUnixNS, r.OuterStartUnixNS, r.OuterEndUnixNS = start.UnixNano(), end.UnixNano(), outerStart.UnixNano(), outerEnd.UnixNano()
	r.CPU, r.OuterCPU = endCPU.sub(startCPU), outerEndCPU.sub(outerStartCPU)
	r.AllocatedBytes, r.Allocations = after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs
	r.StartHeap, r.EndHeap, r.StartGC, r.EndGC = before.HeapAlloc, after.HeapAlloc, before.NumGC, after.NumGC
	r.WALEnd = metricMetadataBenchmarkWALPosition(b, f.db.head.wal)
	r.LateTransactions, r.MaxLatenessNS = f.lateTransactions, f.maxLateness.Nanoseconds()
	r.State = f.validateState(b)
	if !reflect.DeepEqual(seedState, r.State) {
		b.Fatalf("metadata ownership changed during unchanged ingestion: before=%+v after=%+v", seedState, r.State)
	}
	f.validateSamples(b)
	if err := f.db.Close(); err != nil {
		b.Fatal(err)
	}
	f.db = nil
	r.WAL = f.validateWAL(b)
	b.ReportMetric(float64(r.CPU.User+r.CPU.System)/float64(r.Samples), "cpu-ns/sample")
	b.ReportMetric(float64(r.ElapsedNS)/float64(r.Samples), "elapsed-ns/sample")
	b.ReportMetric(float64(r.WALEnd-r.WALStart)/float64(r.Samples), "wal-B/sample")
	encoded, err := json.Marshal(r)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("unchanged-diagnostic-result: %s", encoded)
}

func TestDBMetricMetadataDiagnostic(t *testing.T) {
	modes := []string{"native", "wal", "off"}
	if metadataDiagnosticBuild != "ACTE" {
		modes = modes[:1]
	}
	for _, mode := range modes {
		for _, pattern := range []string{"reused", "observation"} {
			t.Run(mode+"/"+pattern, func(t *testing.T) {
				c := metadataDiagnosticConfig{Mode: mode, Pattern: pattern, Series: 100, Values: 10, Sweeps: 2, CommitSize: 50, Rate: 500000}
				f := newMetadataDiagnosticFixture(t, c)
				if err := f.seed(t.Context(), false); err != nil {
					t.Fatal(err)
				}
				before := f.validateState(t)
				f.prepare(0)
				original := f.values[0]
				if pattern == "observation" {
					first, duplicate := f.inputs[0], f.inputs[10]
					if first != original || duplicate != original {
						t.Fatal("cloning changed values")
					}
					for _, p := range [][3]string{{string(first.Type), string(duplicate.Type), string(original.Type)}, {first.Unit, duplicate.Unit, original.Unit}, {first.Help, duplicate.Help, original.Help}} {
						if unsafe.StringData(p[0]) == unsafe.StringData(p[1]) || unsafe.StringData(p[0]) == unsafe.StringData(p[2]) {
							t.Fatal("fresh backing aliases")
						}
					}
					clear(f.inputs)
					f.prepare(0)
					if unsafe.StringData(first.Help) == unsafe.StringData(f.inputs[0].Help) {
						t.Fatal("backing reused across transactions")
					}
					runtime.KeepAlive(first)
				}
				clear(f.inputs)
				if err := f.ingest(t.Context()); err != nil {
					t.Fatal(err)
				}
				if after := f.validateState(t); !reflect.DeepEqual(before, after) {
					t.Fatalf("metadata changed: %+v %+v", before, after)
				}
				f.validateSamples(t)
				if err := f.db.Close(); err != nil {
					t.Fatal(err)
				}
				f.db = nil
				f.validateWAL(t)
			})
		}
	}
	t.Run("seed equivalence", func(t *testing.T) {
		if metadataDiagnosticBuild != "ACTE" {
			t.Skip("ordinary API seeding is intentionally disabled in bypass builds")
		}
		c := metadataDiagnosticConfig{Mode: "native", Pattern: "reused", Series: 100, Values: 10, Sweeps: 2, CommitSize: 50, Rate: 500000}
		manual, ordinary := newMetadataDiagnosticFixture(t, c), newMetadataDiagnosticFixture(t, c)
		if err := errors.Join(manual.seed(t.Context(), false), ordinary.seed(t.Context(), true)); err != nil {
			t.Fatal(err)
		}
		if a, b := manual.validateState(t), ordinary.validateState(t); !reflect.DeepEqual(a, b) {
			t.Fatalf("seed state mismatch: %+v %+v", a, b)
		}
		if !reflect.DeepEqual(manual.refs, ordinary.refs) {
			t.Fatal("seed refs differ")
		}
	})
	t.Run("decision contract", func(t *testing.T) {
		m := metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: "stable"}
		s := &memSeries{ref: 1}
		a := &headAppenderBase{head: &Head{nativeMetricMetadata: newNativeMetricMetadataStore()}}
		s.Lock()
		defer s.Unlock()
		check := func(name string, ts int64, value *metadata.Metadata, want bool) {
			t.Helper()
			if metadataDiagnosticBuild == "decision" {
				want = false
			}
			observe, proof := a.shouldObserveNativeMetricMetadataLocked(s, ts, value)
			if observe != want || proof != nil {
				t.Fatalf("%s: observe=%v proof=%v", name, observe, proof)
			}
		}
		check("nil", 100, nil, false)
		check("missing committed state", 100, &m, true)
		s.ensureMetadataLocked().native = &nativeSeriesMetadata{metadata: cloneNativeMetricMetadata(m), effectiveFrom: 100}
		check("old timestamp", 99, &m, true)
		check("unchanged", 101, &m, false)
		changed := m
		changed.Help = "different"
		check("changed short value", 101, &changed, metadataDiagnosticBuild == "ACTE")
		long := m
		long.Help = strings.Repeat("x", 1024)
		check("changed long value", 101, &long, true)
		a.recordNativeMetricMetadata(s, 101, changed)
		check("pending reassertion", 102, &m, true)
		a.clearNativeMetricMetadata()
	})
}
