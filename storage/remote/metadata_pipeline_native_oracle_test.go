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

package remote

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/wlog"
	"github.com/prometheus/prometheus/util/compression"
)

// This build logs native metadata merge groups and checkpoints overrides.
func init() {
	metadataPipelineMetadataOracle = func(c metadataPipelineConfig, w metadataPipelineWALRecords, refs map[int]chunks.HeadSeriesRef, fromAppend int) error {
		if c.Source != "native" {
			return metadataPipelineLegacyMetadataOracle(c, w, refs, fromAppend)
		}
		want, err := metadataPipelineNativeEntries(c, refs, fromAppend)
		if err != nil {
			return err
		}
		got, err := w.decodeNative(w.ids)
		if err != nil {
			return err
		}
		if c.Mixed {
			want.records = len(w.metadata)
		}
		return want.check("WAL", got, len(w.metadata), w.payload())
	}
	metadataPipelineCheckpointOracle = func(c metadataPipelineConfig, w metadataPipelineWALRecords) error {
		if c.Source != "native" {
			return metadataPipelineLegacyCheckpointOracle(c, w)
		}
		// Every series keeps its whole history, as one untruncated override.
		var want metadataPipelineExpected
		var enc record.Encoder
		refs := w.refs()
		for slot := range c.Series {
			entry := record.RefNativeMetadata{Ref: refs[slot], Kind: record.NativeMetadataOverride}
			for step := 0; step <= metadataPipelineRestartHistory; step++ {
				entry.Points = append(entry.Points, metadataPipelineNativePoint(c, slot, step))
			}
			want.add(metadataPipelineNativeKey(slot, entry), len(enc.NativeMetadata([]record.RefNativeMetadata{entry}, nil))-1)
		}
		got, err := w.decodeNative(w.ids)
		if err != nil {
			return err
		}
		want.records = len(w.metadata)
		return want.check("checkpoint", got, len(w.metadata), w.payload())
	}
}

func metadataPipelineNativePoint(c metadataPipelineConfig, slot, step int) record.RefNativeMetadataPoint {
	m := c.metadata(slot, c.version(slot, step))
	return record.RefNativeMetadataPoint{EffectiveFrom: c.timestamp(step), Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: m.Help}
}

func metadataPipelineNativeKey(id int, e record.RefNativeMetadata) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d|%d|%t", id, e.Kind, e.Truncated)
	for _, p := range e.Points {
		fmt.Fprintf(&b, "|%d,%d,%s,%s", p.EffectiveFrom, p.Type, p.Unit, p.Help)
	}
	return b.String()
}

// metadataPipelineNativeEntries models the merge groups Commit logs. A
// series' samples in a transaction are observed from its first sample whose
// metadata differs from the series' committed native version, or from its
// first sample if it has none; an observed sample keeps later samples of the
// series in that transaction observed. Groups hold one point per observed
// timestamp. Replay seeds each series with its newest version.
//
// Observations of another series in the same stripe can make a sample with
// unchanged metadata observed. Such a group is stable and logs nothing, and in
// these traces a series' observed samples are contiguous, so the model
// ignores stripes.
func metadataPipelineNativeEntries(c metadataPipelineConfig, refs map[int]chunks.HeadSeriesRef, fromAppend int) (metadataPipelineExpected, error) {
	var e metadataPipelineExpected
	var enc record.Encoder
	committed := map[int]int{}
	var err error
	for i, a := range c.appends() {
		c.transactions(a, func(samples []metadataPipelineSample) {
			entries := 0
			for start := 0; start < len(samples); {
				end := start + 1
				for end < len(samples) && samples[end].id == samples[start].id {
					end++
				}
				run := samples[start:end]
				start = end
				id := run[0].id
				first := 0
				if v, ok := committed[id]; ok {
					for first < len(run) && c.version(run[first].slot, run[first].step) == v {
						first++
					}
				}
				if first == len(run) {
					continue
				}
				last := run[len(run)-1]
				committed[id] = c.version(last.slot, last.step)
				if i < fromAppend {
					continue
				}
				ref, ok := refs[id]
				if !ok {
					err = errors.Join(err, fmt.Errorf("no series record for id %d", id))
					continue
				}
				entry := record.RefNativeMetadata{Ref: ref, Kind: record.NativeMetadataGroup}
				for _, s := range run[first:] {
					entry.Points = append(entry.Points, metadataPipelineNativePoint(c, s.slot, s.step))
				}
				e.add(metadataPipelineNativeKey(id, entry), len(enc.NativeMetadata([]record.RefNativeMetadata{entry}, nil))-1)
				entries++
			}
			if entries > 0 {
				e.records++
			}
		})
	}
	return e, err
}

// decodeNative decodes metadata records' entries with the native decoder.
func (w metadataPipelineWALRecords) decodeNative(ids map[chunks.HeadSeriesRef]int) (map[string]int, error) {
	got := map[string]int{}
	var dec record.Decoder
	for _, rec := range w.metadata {
		entries, _, err := dec.NativeMetadata(rec, nil, nil)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			id, ok := ids[e.Ref]
			if !ok {
				return nil, fmt.Errorf("metadata for unknown ref %d", e.Ref)
			}
			got[metadataPipelineNativeKey(id, e)]++
		}
	}
	return got, nil
}

func TestRemoteWriteMetadataPipelineNativeOracles(t *testing.T) {
	c := metadataPipelineConfig{Case: "restart", Source: "native", Series: 2, Values: 2, Sweeps: metadataPipelineRestartHistory + 3, RestartStep: metadataPipelineRestartHistory + 1, Writers: 1, CommitSize: 2, Base: 1000}
	writeCheckpoint := func(t *testing.T, entries []record.RefNativeMetadata) string {
		dir := t.TempDir()
		w, err := wlog.New(nil, nil, dir, compression.None)
		require.NoError(t, err)
		var enc record.Encoder
		var series []record.RefSeries
		for id := range c.Series {
			series = append(series, record.RefSeries{Ref: chunks.HeadSeriesRef(id + 1), Labels: metadataPipelineLabels(id)})
		}
		require.NoError(t, w.Log(enc.Series(series, nil)))
		require.NoError(t, w.Log(enc.NativeMetadata(entries, nil)))
		require.NoError(t, w.Close())
		return dir
	}
	full := func() []record.RefNativeMetadata {
		var entries []record.RefNativeMetadata
		for id := range c.Series {
			e := record.RefNativeMetadata{Ref: chunks.HeadSeriesRef(id + 1), Kind: record.NativeMetadataOverride}
			for step := 0; step <= metadataPipelineRestartHistory; step++ {
				e.Points = append(e.Points, metadataPipelineNativePoint(c, id, step))
			}
			entries = append(entries, e)
		}
		return entries
	}

	t.Run("checkpoint contents", func(t *testing.T) {
		_, err := checkMetadataPipelineCheckpoint(c, writeCheckpoint(t, full()))
		require.NoError(t, err)
		// Delivery after a restart sees only the newest version; the oracle must
		// still notice a lost older one, a wrong start, or a truncation flag.
		lost := full()
		lost[0].Points = lost[0].Points[1:]
		moved := full()
		moved[1].Points[2].EffectiveFrom++
		truncated := full()
		truncated[0].Truncated = true
		for name, entries := range map[string][]record.RefNativeMetadata{"lost older version": lost, "moved start": moved, "truncated": truncated, "missing series": full()[:1]} {
			_, err := checkMetadataPipelineCheckpoint(c, writeCheckpoint(t, entries))
			require.Error(t, err, name)
		}
	})

	t.Run("WAL groups", func(t *testing.T) {
		b := metadataPipelineConfig{Case: "batched", Source: "native", Series: 2, Values: 2, Sweeps: 100, Writers: 1, CommitSize: 2, StepsPerCommit: 10, Base: 1000}
		refs := map[int]chunks.HeadSeriesRef{0: 1, 1: 2}
		want, err := metadataPipelineNativeEntries(b, refs, 0)
		require.NoError(t, err)
		// The seed logs both series. Each series changes once in 100 steps,
		// both within the transaction of steps 1-10, so each group holds every
		// step from the change to the end of that batch.
		require.Equal(t, 2, want.records)
		groups := 0
		for key, n := range want.entries {
			groups += n
			// A key has two separators before its points, and one per point.
			if points := strings.Count(key, "|") - 2; strings.HasPrefix(key, "1|") && points > 1 {
				require.Equal(t, 9, points, "series 1 changes at step 2, in the batch of steps 1-10")
			}
		}
		require.Equal(t, 2+2, groups)
	})

	t.Run("unknown-entry counters", func(t *testing.T) {
		// This build registers both counters; restarts report them while the
		// sender still runs.
		c := metadataPipelineConfig{Case: "restart", Source: "native", Series: 300, Values: 100, Sweeps: 200, RestartStep: 180, WALSegmentSize: 32 << 10, Writers: 4, Shards: 4, Batch: 20, Capacity: 100, CommitSize: 50, ReceiverProcs: 2, Base: time.Now().Add(time.Hour).UnixMilli()}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f, err := newMetadataPipeline(ctx, c)
		require.NoError(t, err)
		defer func() { require.NoError(t, f.close()) }()
		dir := t.TempDir()
		require.NoError(t, f.open(dir))
		require.NoError(t, f.append(ctx, 0, 0))
		_, err = f.drain(ctx, f.expectedItems(1))
		require.NoError(t, err)
		restart, err := f.restart(ctx, dir, false)
		require.NoError(t, err)
		require.Equal(t, map[string]float64{
			"prometheus_remote_storage_native_metadata_unknown_entries_total":  0,
			"prometheus_tsdb_native_metric_metadata_unknown_wal_entries_total": 0,
		}, restart.UnknownEntryCounters)
	})

	t.Run("sender heap attribution", func(t *testing.T) {
		defer func(rate int) { runtime.MemProfileRate = rate }(runtime.MemProfileRate)
		runtime.MemProfileRate = 1
		// Values interned by earlier tests would be hits, retaining strings
		// that those tests allocated.
		defer func(i *metadataInterner) { walMetadataInterner = i }(walMetadataInterner)
		walMetadataInterner = newMetadataInterner(metadataInternerEntries, metadataInternerBytes)
		const series, help = 200, 4096
		c := metadataPipelineConfig{Case: "unchanged", Source: "native", Series: series, Values: series, HelpBytes: help, Sweeps: 2, Writers: 2, Shards: 2, Batch: 50, Capacity: 100, CommitSize: 50, ReceiverProcs: 2, Base: time.Now().Add(time.Hour).UnixMilli()}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f, err := newMetadataPipeline(ctx, c)
		require.NoError(t, err)
		defer func() { require.NoError(t, f.close()) }()
		require.NoError(t, f.open(t.TempDir()))
		require.NoError(t, f.append(ctx, 0, c.Sweeps))
		_, err = f.drain(ctx, f.expectedItems(c.Sweeps+1))
		require.NoError(t, err)
		a, err := metadataPipelineSenderHeap()
		require.NoError(t, err)
		// Native histories retain the interner's copies of the strings the
		// watcher lent it.
		decoded := a.SenderBytesByFunction["github.com/prometheus/prometheus/storage/remote.ownMetadata"]
		t.Logf("sender %d (copied from borrowed entries %d), fixture %d, other %d, profiled %d, heap %d", a.Sender, decoded, a.Fixture, a.Other, a.ProfiledInUse, a.HeapAlloc)
		require.GreaterOrEqual(t, decoded, int64(series*help))
		require.Less(t, a.Sender, int64(a.ProfiledInUse))
	})
}
