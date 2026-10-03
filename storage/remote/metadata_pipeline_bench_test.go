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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type metadataPipelineResult struct {
	Config                                         metadataPipelineConfig
	Diagnostic                                     bool
	Samples                                        int64
	Initialization, Ingestion, Drain, Shutdown     time.Duration
	BacklogWait, ReleaseToDrain                    time.Duration
	Completion, DBClose, Lifecycle                 time.Duration
	CPU, LifecycleCPU, ReceiverCPU                 metadataPipelineCPUUsage
	AllocatedBytes, Allocations                    uint64
	LifecycleAllocatedBytes, LifecycleAllocations  uint64
	WALBytes, LifecycleWALBytes                    float64
	SeedWALBytes                                   float64
	WALPayloadBytes                                map[string]int64
	MetadataOracle                                 bool
	UnknownEntryCounters                           map[string]float64
	SenderHeap                                     *metadataPipelineHeapAttribution `json:",omitempty"`
	Requests, RequestBytes, ReceiverServiceNanos   int64
	OutstandingAtWriterEnd, PeakSampledQueue       int64
	ResidentSeries                                 uint64
	SeededHeap, DrainedHeap, BacklogHeap           uint64
	Transactions, ScheduledTransactions            int
	TransactionP50, TransactionP95, TransactionP99 float64
	ScheduleP50, ScheduleP99, ScheduleMax          float64
	SweepsLateByInterval                           int64
	TransactionsLateByInterval                     int64
	Restart                                        *metadataPipelineRestart `json:",omitempty"`
}

// BenchmarkRemoteWriteMetadataPipeline includes ingestion, WAL reading, metadata
// population/lookup, and acknowledged HTTP forwarding. One operation is a finite
// trace, not a sample. See metadata_pipeline_bench.md for boundaries and controls.
func BenchmarkRemoteWriteMetadataPipeline(b *testing.B) {
	series := metadataPipelineSetting(b, "PROMETHEUS_METADATA_PIPELINE_SERIES", 10000)
	sweeps := metadataPipelineSetting(b, "PROMETHEUS_METADATA_PIPELINE_SWEEPS", 200)
	receiverProcs := metadataPipelineSetting(b, "PROMETHEUS_METADATA_PIPELINE_RECEIVER_PROCS", 2)
	// The checkpoint covers about two thirds of the segments, so unchanged steps
	// must follow the metadata history: 30 leaves WAL records a margin of 11
	// segments at 10,000 series.
	restartStep := metadataPipelineSetting(b, "PROMETHEUS_METADATA_PIPELINE_RESTART_STEP", 30)
	require.Zero(b, series%100, "series count must be a multiple of 100")
	require.LessOrEqual(b, sweeps, 400, "changing traces must fit the five-version native history")
	for _, workload := range []string{"cold", "unchanged", "changes", "newseries", "backlog", "cardinality", "distinct", "changes-distinct", "paced-unchanged", "paced-changes", "batched", "restart"} {
		c := metadataPipelineConfig{Case: workload, Series: series, Values: 100, Sweeps: sweeps, Writers: 4, Shards: 4, CommitSize: 1000, Batch: 2000, Capacity: 10000, ReceiverProcs: receiverProcs}
		switch workload {
		case "backlog":
			c.Writers, c.Shards, c.Sweeps = 1, 1, 4
		case "cardinality":
			c.Series *= 10
		case "distinct", "changes-distinct":
			c.Values = c.Series
		case "paced-unchanged", "paced-changes":
			c.SweepInterval = 20 * time.Millisecond
		case "batched":
			// Keep 1,000 samples per transaction, as in the unbatched cases.
			c.CommitSize, c.StepsPerCommit = 100, 10
		case "restart":
			// Segments scale with the series count. Measured sweeps follow the restart.
			c.RestartStep, c.WALSegmentSize = restartStep, max(1, series/10000)*32<<10
		}
		for _, mode := range []string{"disabled", "wal", "native"} {
			c.Source = mode
			b.Run(fmt.Sprintf("case=%s/series=%d/source=%s", workload, c.Series, mode), func(b *testing.B) {
				benchmarkMetadataPipeline(b, c)
			})
		}
	}
}

// BenchmarkRemoteWriteMetadataPipelineScale separates history-preserving,
// equal-work, and held-backlog traces. Capacity traces are unpaced diagnostics.
func BenchmarkRemoteWriteMetadataPipelineScale(b *testing.B) {
	receiverProcs := metadataPipelineSetting(b, "PROMETHEUS_METADATA_PIPELINE_RECEIVER_PROCS", 2)
	for _, group := range []string{"history", "equal-work", "backlog", "capacity"} {
		for _, workload := range []string{"unchanged", "changes"} {
			if (group == "equal-work" && workload != "unchanged") || (group == "backlog" && workload != "changes") {
				continue
			}
			for _, sharing := range []string{"shared", "distinct"} {
				for _, series := range []int{10000, 100000} {
					if (group == "equal-work" || group == "capacity") && series != 100000 {
						continue
					}
					c := metadataPipelineConfig{Group: group, Case: workload, Series: series, Values: 100, Sweeps: 200, Writers: 4, Shards: 4, CommitSize: 500, Batch: 2000, Capacity: 10000, ReceiverProcs: receiverProcs, SamplesPerSecond: 500000}
					if sharing == "distinct" {
						c.Values = series
					}
					switch group {
					case "equal-work":
						c.Sweeps = 20
					case "backlog":
						c.Case, c.Writers, c.Shards, c.Sweeps, c.CommitSize, c.SamplesPerSecond = "backlog", 1, 1, 4, 1000, 0
					case "capacity":
						c.SamplesPerSecond = 0
					}
					for _, mode := range []string{"disabled", "wal", "native"} {
						c.Source = mode
						b.Run(fmt.Sprintf("group=%s/case=%s/values=%s/series=%d/source=%s", group, workload, sharing, series, mode), func(b *testing.B) {
							benchmarkMetadataPipeline(b, c)
						})
					}
				}
			}
		}
	}
}

func metadataPipelineSetting(b *testing.B, name string, fallback int) int {
	b.Helper()
	if value := os.Getenv(name); value != "" {
		n, err := strconv.Atoi(value)
		require.NoError(b, err)
		require.Positive(b, n)
		return n
	}
	return fallback
}

func benchmarkMetadataPipeline(b *testing.B, c metadataPipelineConfig) {
	b.Helper()
	b.ReportAllocs()
	b.StopTimer()
	metrics := map[string]float64{}
	for range b.N {
		c.Base = time.Now().Add(time.Hour).UnixMilli()
		r := measureMetadataPipeline(b, c)
		n := float64(r.Samples)
		metrics["samples/s"] += n / r.Completion.Seconds()
		metrics["samples/op"] += n
		metrics["alloc-B/sample"] += float64(r.AllocatedBytes) / n
		metrics["allocs/sample"] += float64(r.Allocations) / n
		metrics["wal-B/sample"] += r.WALBytes / n
		metrics["wire-B/sample"] += float64(r.RequestBytes) / n
		metrics["drain-ms/op"] += float64(r.Drain) / float64(time.Millisecond)
		metrics["txn-p99-ns"] += r.TransactionP99
		if c.SweepInterval > 0 || c.SamplesPerSecond > 0 {
			metrics["ingested-samples/s"] += n / r.Ingestion.Seconds()
			metrics["schedule-p99-ns"] += r.ScheduleP99
			metrics["schedule-max-ns"] += r.ScheduleMax
			if c.SamplesPerSecond > 0 {
				metrics["late-transactions/op"] += float64(r.TransactionsLateByInterval)
			} else {
				metrics["late-sweeps/op"] += float64(r.SweepsLateByInterval)
			}
		}
		if r.CPU.Available {
			metrics["cpu-ns/sample"] += float64(r.CPU.User+r.CPU.System) / n
		}
		if restart := r.Restart; restart != nil {
			metrics["truncate-ms/op"] += float64(restart.Truncation) / float64(time.Millisecond)
			metrics["replay-ms/op"] += float64(restart.Replay) / float64(time.Millisecond)
			metrics["checkpoint-B/op"] += float64(restart.CheckpointBytes)
			metrics["truncate-alloc-B/op"] += float64(restart.TruncationAllocatedBytes)
		}
		encoded, err := json.Marshal(r)
		require.NoError(b, err)
		b.Logf("metadata-pipeline-result: %s", encoded)
	}
	for name, value := range metrics {
		b.ReportMetric(value/float64(b.N), name)
	}
}

func measureMetadataPipeline(b *testing.B, c metadataPipelineConfig) metadataPipelineResult {
	ctx, cancel := context.WithTimeout(b.Context(), 5*time.Minute)
	defer cancel()
	dir := b.TempDir()
	f, err := newMetadataPipeline(ctx, c)
	require.NoError(b, err)
	defer func() { require.NoError(b, f.close()) }()
	defer b.StopTimer()
	d, err := metadataPipelineDiagnosticsFromEnv(c)
	require.NoError(b, err)
	f.diagnostics = d
	defer func() { require.NoError(b, d.close()) }()
	r := metadataPipelineResult{Config: c, Diagnostic: os.Getenv("PROMETHEUS_METADATA_PIPELINE_HEAP") == "1"}
	beforeReceiver, err := f.receiver.command(ctx, "stats")
	require.NoError(b, err)
	var lifecycleMemory, beforeMemory, afterMemory runtime.MemStats
	runtime.ReadMemStats(&lifecycleMemory)
	lifecycleCPU, err := metadataPipelineCPU()
	require.NoError(b, err)
	lifecycleStart := time.Now()
	phaseStart, phaseCPU := lifecycleStart, lifecycleCPU
	beforeMemory = lifecycleMemory
	require.NoError(b, d.begin(ctx, f, "initialization"))
	if c.Case == "cold" {
		b.StartTimer()
	}
	require.NoError(b, f.open(dir))
	r.Initialization = time.Since(lifecycleStart)
	var beforeWAL float64
	if c.Case != "cold" {
		require.NoError(b, f.append(ctx, 0, 0))
		beforeReceiver, err = f.drain(ctx, f.expectedItems(1))
		require.NoError(b, err)
		if c.RestartStep > 0 {
			restart, err := f.restart(ctx, dir, r.Diagnostic)
			require.NoError(b, err)
			r.Restart = &restart
			beforeReceiver, err = f.receiver.command(ctx, "stats")
			require.NoError(b, err)
		}
		beforeMetrics, err := f.metrics()
		require.NoError(b, err)
		beforeWAL = beforeMetrics["prometheus_tsdb_wal_record_parts_bytes_written_total"]
		r.SeedWALBytes = beforeWAL
		for i := range f.latency {
			f.latency[i] = f.latency[i][:0]
			f.peak[i] = 0
		}
		if r.Diagnostic && c.Group != "" {
			r.SeededHeap = metadataPipelineRetainedHeap(f)
		}
		if c.Case == "backlog" {
			require.NoError(b, f.holdReceiver(ctx))
		}
		r.Initialization = time.Since(lifecycleStart)
		runtime.ReadMemStats(&beforeMemory)
		phaseCPU, err = metadataPipelineCPU()
		require.NoError(b, err)
		phaseStart = time.Now()
		b.StartTimer()
	}
	first, last := c.RestartStep+1, c.Sweeps
	if c.Case == "cold" {
		first, last = 0, 0
	}
	require.NoError(b, d.end(ctx, f, "initialization"))
	r.Samples = int64(c.Series) * int64(last-first+1)
	require.NoError(b, d.begin(ctx, f, "ingestion"))
	ingestStart := time.Now()
	require.NoError(b, f.append(ctx, first, last))
	writerEnd := time.Now()
	r.Ingestion = writerEnd.Sub(ingestStart)
	d.mark(ctx, "writer-complete")
	require.NoError(b, d.end(ctx, f, "ingestion"))
	require.NoError(b, d.begin(ctx, f, "backlog-wait"))
	stats, err := f.receiver.command(ctx, "stats")
	require.NoError(b, err)
	r.OutstandingAtWriterEnd = f.expectedItems(last+1) - stats.items()
	var releaseStart time.Time
	if c.Case == "backlog" {
		require.NoError(b, f.awaitBacklog(ctx))
		// More samples than the queue and one decoded WAL record can hold must
		// remain unread. This exercises historical lookup, not just HTTP delay.
		require.Greater(b, r.Samples-f.pending(), int64(c.CommitSize+256))
		if r.Diagnostic {
			r.BacklogHeap = metadataPipelineRetainedHeap(f)
		}
		require.NoError(b, d.end(ctx, f, "backlog-wait"))
		require.NoError(b, d.begin(ctx, f, "drain"))
		releaseStart = time.Now()
		r.BacklogWait = releaseStart.Sub(writerEnd)
		d.mark(ctx, "release-command-start")
		_, err = f.receiver.command(ctx, "release")
		require.NoError(b, err)
		d.mark(ctx, "release-command-complete")
	} else {
		require.NoError(b, d.end(ctx, f, "backlog-wait"))
		require.NoError(b, d.begin(ctx, f, "drain"))
	}
	stats, err = f.drain(ctx, f.expectedItems(last+1))
	require.NoError(b, err)
	drainEnd := time.Now()
	r.Drain = drainEnd.Sub(writerEnd)
	if !releaseStart.IsZero() {
		r.ReleaseToDrain = drainEnd.Sub(releaseStart)
	}
	d.mark(ctx, "drain-complete")
	require.NoError(b, d.end(ctx, f, "drain"))
	if r.Diagnostic {
		r.DrainedHeap = metadataPipelineRetainedHeap(f)
		// Attribute the heap while the queues and their state are alive.
		heap, err := metadataPipelineSenderHeap()
		require.NoError(b, err, "heap passes need -test.memprofilerate=1")
		r.SenderHeap = &heap
	}
	require.NoError(b, d.begin(ctx, f, "shutdown"))
	stopStart := time.Now()
	require.NoError(b, f.closeSender())
	r.Shutdown = time.Since(stopStart)
	require.NoError(b, d.end(ctx, f, "shutdown"))
	r.Completion = time.Since(phaseStart)
	phaseEndCPU, err := metadataPipelineCPU()
	require.NoError(b, err)
	b.StopTimer()
	runtime.ReadMemStats(&afterMemory)
	r.CPU = phaseEndCPU.sub(phaseCPU)
	r.AllocatedBytes, r.Allocations = afterMemory.TotalAlloc-beforeMemory.TotalAlloc, afterMemory.Mallocs-beforeMemory.Mallocs
	r.ReceiverCPU = stats.CPU.sub(beforeReceiver.CPU)
	r.Requests, r.RequestBytes = stats.Requests-beforeReceiver.Requests, stats.Bytes-beforeReceiver.Bytes
	r.ReceiverServiceNanos = stats.ServiceNanos - beforeReceiver.ServiceNanos
	r.ResidentSeries = f.db.Head().NumSeries()
	require.Equal(b, uint64(c.residentSeries()), r.ResidentSeries)
	metrics, err := f.metrics()
	require.NoError(b, err)
	require.NoError(b, checkMetadataPipelineMetrics(metrics))
	r.UnknownEntryCounters = metadataPipelineUnknownCounters(metrics)
	r.LifecycleWALBytes = metrics["prometheus_tsdb_wal_record_parts_bytes_written_total"]
	r.WALBytes = r.LifecycleWALBytes - beforeWAL
	require.NoError(b, d.begin(ctx, f, "db-close"))
	closeStart := time.Now()
	require.NoError(b, f.db.Close())
	f.db = nil
	r.DBClose = time.Since(closeStart)
	require.NoError(b, d.end(ctx, f, "db-close"))
	r.Lifecycle = time.Since(lifecycleStart)
	lifecycleEndCPU, err := metadataPipelineCPU()
	require.NoError(b, err)
	r.LifecycleCPU = lifecycleEndCPU.sub(lifecycleCPU)
	runtime.ReadMemStats(&afterMemory)
	r.LifecycleAllocatedBytes, r.LifecycleAllocations = afterMemory.TotalAlloc-lifecycleMemory.TotalAlloc, afterMemory.Mallocs-lifecycleMemory.Mallocs
	walDir := filepath.Join(dir, "wal")
	r.WALPayloadBytes, err = metadataPipelinePayloadBytes(walDir, 0)
	require.NoError(b, err)
	if r.Restart != nil {
		r.Restart.PostRestartPayloadBytes, err = metadataPipelinePayloadBytes(walDir, r.Restart.FirstSegment)
		require.NoError(b, err)
	}
	require.NoError(b, checkMetadataPipelineWAL(c, walDir, r.Restart))
	r.MetadataOracle = true
	var latencies []time.Duration
	var lateness []time.Duration
	for i := range f.latency {
		r.Transactions += len(f.latency[i])
		latencies = append(latencies, f.latency[i]...)
		r.PeakSampledQueue = max(r.PeakSampledQueue, f.peak[i])
		lateness = append(lateness, f.lateness[i]...)
		if c.SamplesPerSecond > 0 {
			r.ScheduledTransactions += len(f.lateness[i])
		}
		for _, late := range f.lateness[i] {
			if c.SamplesPerSecond > 0 {
				if late >= time.Second*time.Duration(c.CommitSize)*time.Duration(c.Writers)/time.Duration(c.SamplesPerSecond) {
					r.TransactionsLateByInterval++
				}
			} else if late >= c.SweepInterval {
				r.SweepsLateByInterval++
			}
		}
	}
	r.TransactionP50 = metadataPipelinePercentile(latencies, 50)
	r.TransactionP95 = metadataPipelinePercentile(latencies, 95)
	r.TransactionP99 = metadataPipelinePercentile(latencies, 99)
	r.ScheduleP50 = metadataPipelinePercentile(lateness, 50)
	r.ScheduleP99 = metadataPipelinePercentile(lateness, 99)
	r.ScheduleMax = metadataPipelinePercentile(lateness, 100)
	if d != nil {
		require.NoError(b, d.close())
		encoded, err := json.Marshal(d)
		require.NoError(b, err)
		b.Logf("metadata-pipeline-diagnostics: %s", encoded)
	}
	return r
}
