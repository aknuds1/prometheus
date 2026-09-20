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
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type metadataPipelineResult struct {
	Config                                         metadataPipelineConfig
	Diagnostic                                     bool
	Samples                                        int64
	Initialization, Ingestion, Drain, Shutdown     time.Duration
	Completion, DBClose, Lifecycle                 time.Duration
	CPU, LifecycleCPU, ReceiverCPU                 metadataPipelineCPUUsage
	AllocatedBytes, Allocations                    uint64
	LifecycleAllocatedBytes, LifecycleAllocations  uint64
	WALBytes, LifecycleWALBytes                    float64
	Requests, RequestBytes, ReceiverServiceNanos   int64
	OutstandingAtWriterEnd, PeakSampledQueue       int64
	ResidentSeries                                 uint64
	DrainedHeap, BacklogHeap                       uint64
	TransactionP50, TransactionP95, TransactionP99 float64
}

// BenchmarkRemoteWriteMetadataPipeline includes ingestion, WAL reading, metadata
// population/lookup, and acknowledged HTTP forwarding. One operation is a finite
// trace, not a sample. See metadata_pipeline_bench.md for boundaries and controls.
func BenchmarkRemoteWriteMetadataPipeline(b *testing.B) {
	setting := func(name string, fallback int) int {
		if value := os.Getenv(name); value != "" {
			n, err := strconv.Atoi(value)
			require.NoError(b, err)
			require.Positive(b, n)
			return n
		}
		return fallback
	}
	series := setting("PROMETHEUS_METADATA_PIPELINE_SERIES", 10000)
	sweeps := setting("PROMETHEUS_METADATA_PIPELINE_SWEEPS", 200)
	receiverProcs := setting("PROMETHEUS_METADATA_PIPELINE_RECEIVER_PROCS", 2)
	require.Zero(b, series%100, "series count must be a multiple of 100")
	require.LessOrEqual(b, sweeps, 400, "changing traces must fit the five-version native history")
	for _, workload := range []string{"cold", "unchanged", "changes", "newseries", "backlog", "cardinality", "distinct"} {
		c := metadataPipelineConfig{Case: workload, Series: series, Values: 100, Sweeps: sweeps, Writers: 4, Shards: 4, CommitSize: 1000, Batch: 2000, Capacity: 10000, ReceiverProcs: receiverProcs}
		switch workload {
		case "backlog":
			c.Writers, c.Shards, c.Sweeps = 1, 1, 4
		case "cardinality":
			c.Series *= 10
		case "distinct":
			c.Values = c.Series
		}
		for _, mode := range []string{"disabled", "wal", "native"} {
			c.Source = mode
			b.Run(fmt.Sprintf("case=%s/series=%d/source=%s", workload, c.Series, mode), func(b *testing.B) {
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
					if r.CPU.Available {
						metrics["cpu-ns/sample"] += float64(r.CPU.User+r.CPU.System) / n
					}
					encoded, err := json.Marshal(r)
					require.NoError(b, err)
					b.Logf("metadata-pipeline-result: %s", encoded)
				}
				for name, value := range metrics {
					b.ReportMetric(value/float64(b.N), name)
				}
			})
		}
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
		beforeMetrics, err := f.metrics()
		require.NoError(b, err)
		beforeWAL = beforeMetrics["prometheus_tsdb_wal_record_parts_bytes_written_total"]
		for i := range f.latency {
			f.latency[i] = f.latency[i][:0]
			f.peak[i] = 0
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
	first, last := 1, c.Sweeps
	if c.Case == "cold" {
		first, last = 0, 0
	}
	r.Samples = int64(c.Series) * int64(last-first+1)
	ingestStart := time.Now()
	require.NoError(b, f.append(ctx, first, last))
	writerEnd := time.Now()
	r.Ingestion = writerEnd.Sub(ingestStart)
	stats, err := f.receiver.command(ctx, "stats")
	require.NoError(b, err)
	r.OutstandingAtWriterEnd = f.expectedItems(last+1) - stats.items()
	if c.Case == "backlog" {
		require.NoError(b, f.awaitBacklog(ctx))
		// More samples than the queue and one decoded WAL record can hold must
		// remain unread. This exercises historical lookup, not just HTTP delay.
		require.Greater(b, r.Samples-f.pending(), int64(c.CommitSize+256))
		if r.Diagnostic {
			r.BacklogHeap = metadataPipelineRetainedHeap(f)
		}
		_, err = f.receiver.command(ctx, "release")
		require.NoError(b, err)
	}
	stats, err = f.drain(ctx, f.expectedItems(last+1))
	require.NoError(b, err)
	r.Drain = time.Since(writerEnd)
	if r.Diagnostic {
		r.DrainedHeap = metadataPipelineRetainedHeap(f)
	}
	stopStart := time.Now()
	require.NoError(b, f.closeSender())
	r.Shutdown = time.Since(stopStart)
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
	require.Zero(b, metrics["prometheus_tsdb_head_native_metric_metadata_version_evictions_total"])
	r.LifecycleWALBytes = metrics["prometheus_tsdb_wal_record_parts_bytes_written_total"]
	r.WALBytes = r.LifecycleWALBytes - beforeWAL
	for name, value := range metrics {
		if strings.HasPrefix(name, "prometheus_remote_storage_") && (strings.HasSuffix(name, "_failed_total") || strings.HasSuffix(name, "_dropped_total") || strings.HasSuffix(name, "_retried_total")) {
			require.Zero(b, value, name)
		}
	}
	closeStart := time.Now()
	require.NoError(b, f.db.Close())
	f.db = nil
	r.DBClose = time.Since(closeStart)
	r.Lifecycle = time.Since(lifecycleStart)
	lifecycleEndCPU, err := metadataPipelineCPU()
	require.NoError(b, err)
	r.LifecycleCPU = lifecycleEndCPU.sub(lifecycleCPU)
	runtime.ReadMemStats(&afterMemory)
	r.LifecycleAllocatedBytes, r.LifecycleAllocations = afterMemory.TotalAlloc-lifecycleMemory.TotalAlloc, afterMemory.Mallocs-lifecycleMemory.Mallocs
	var latencies []time.Duration
	for i := range f.latency {
		latencies = append(latencies, f.latency[i]...)
		r.PeakSampledQueue = max(r.PeakSampledQueue, f.peak[i])
	}
	r.TransactionP50 = metadataPipelinePercentile(latencies, 50)
	r.TransactionP95 = metadataPipelinePercentile(latencies, 95)
	r.TransactionP99 = metadataPipelinePercentile(latencies, 99)
	return r
}
