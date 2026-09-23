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
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

const metadataPipelineDiagnosticEnv = "PROMETHEUS_METADATA_PIPELINE_DIAGNOSTICS"

type metadataPipelineMemory struct {
	AllocatedBytes, Allocations, GCPause uint64
	GC                                   uint32
}

func metadataPipelineReadMemory() metadataPipelineMemory {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return metadataPipelineMemory{m.TotalAlloc, m.Mallocs, m.PauseTotalNs, m.NumGC}
}

// metadataPipelineDiagnostics is owned by the benchmark goroutine, like the
// receiver control pipe. Observations are not atomic cross-component snapshots.
// Accounting (including stop-the-world MemStats reads) perturbs the workload.
type metadataPipelineDiagnostics struct {
	Accounting bool
	BackoffCap time.Duration
	CPUPhase   string
	Trace      bool
	Phases     []metadataPipelinePhase
	Progress   []metadataPipelineProgress
	Events     []metadataPipelineEvent
	Profiled   bool
	Traced     bool

	origin    time.Time
	active    string
	start     metadataPipelineSnapshot
	watcher   *prometheus.Registry
	output    *os.File
	profiling bool
	tracing   bool
}

type metadataPipelinePhase struct {
	Name       string
	Start, End metadataPipelineSnapshot
}

type metadataPipelineSnapshot struct {
	Started, Finished time.Duration
	CPU               metadataPipelineCPUUsage
	Memory            metadataPipelineMemory
	Receiver          metadataPipelineReceiverStats
	Progress          metadataPipelineProgress
}

type metadataPipelineProgress struct {
	Started, Finished time.Duration
	Pending           int64
	Acknowledged      int64
	HeldRequests      int64
	EnqueueRetries    float64
	// RecordsRead is incremented before decode and queue handoff, not delivery.
	RecordsRead map[string]float64
}

type metadataPipelineEvent struct {
	Name string
	At   time.Duration
}

func metadataPipelineDiagnosticsFromEnv(c metadataPipelineConfig) (*metadataPipelineDiagnostics, error) {
	mode := os.Getenv(metadataPipelineDiagnosticEnv)
	if mode == "" {
		return nil, nil
	}
	if c.Source != "native" || (c.Case != "cold" && c.Case != "backlog") {
		return nil, errors.New("phase diagnostics require native cold or backlog")
	}
	d := &metadataPipelineDiagnostics{origin: time.Now(), watcher: prometheus.NewRegistry()}
	switch mode {
	case "control":
	case "accounting":
		d.Accounting = true
		// The five-minute fixture timeout allows 30,000 polls. Overflow fails
		// explicitly rather than growing this buffer or dropping observations.
		d.Progress = make([]metadataPipelineProgress, 0, 30001)
	case "trace":
		if c.Case != "backlog" {
			return nil, errors.New("execution traces require backlog")
		}
		d.Trace = true
	default:
		phase, ok := strings.CutPrefix(mode, "cpu:")
		if !ok || !slices.Contains([]string{"initialization", "ingestion", "drain"}, phase) {
			return nil, fmt.Errorf("unknown phase diagnostics mode %q", mode)
		}
		d.CPUPhase = phase
	}
	if capValue := os.Getenv(metadataPipelineDiagnosticEnv + "_BACKOFF"); capValue != "" {
		if capValue != "5ms" || c.Case != "backlog" {
			return nil, errors.New("only backlog supports the 5ms backoff control")
		}
		d.BackoffCap = 5 * time.Millisecond
	}
	if d.CPUPhase != "" || d.Trace {
		var err error
		d.output, err = os.OpenFile(os.Getenv(metadataPipelineDiagnosticEnv+"_OUTPUT"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
	}
	return d, nil
}

// metadataPipelineDiagnosticRegisterer mirrors only the WAL record counter into
// a small registry; drain polling does not gather all TSDB/runtime collectors.
type metadataPipelineDiagnosticRegisterer struct {
	prometheus.Registerer
	watcher *prometheus.Registry
}

func (r *metadataPipelineDiagnosticRegisterer) Register(c prometheus.Collector) error {
	if err := r.Registerer.Register(c); err != nil {
		return err
	}
	if vector, ok := c.(*prometheus.CounterVec); ok {
		// CounterVec describes exactly one metric family.
		desc := make(chan *prometheus.Desc, 1)
		vector.Describe(desc)
		if strings.Contains((<-desc).String(), `fqName: "prometheus_wal_watcher_records_read_total"`) {
			return r.watcher.Register(c)
		}
	}
	return nil
}

func (r *metadataPipelineDiagnosticRegisterer) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			panic(err)
		}
	}
}

func (d *metadataPipelineDiagnostics) sampleProgress(f *metadataPipeline, stats metadataPipelineReceiverStats) (metadataPipelineProgress, error) {
	p := metadataPipelineProgress{Started: time.Since(d.origin), Acknowledged: stats.items(), HeldRequests: stats.HeldRequests}
	if f.queue != nil {
		// Pending includes queued and in-flight samples. HeldRequests is
		// cumulative, not the number of requests currently blocked.
		p.Pending = f.pending()
		p.EnqueueRetries = testutil.ToFloat64(f.queue.metrics.enqueueRetriesTotal)
	}
	families, err := d.watcher.Gather()
	if err != nil {
		return p, err
	}
	p.RecordsRead = make(map[string]float64)
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "type" {
					p.RecordsRead[label.GetValue()] += metric.GetCounter().GetValue()
				}
			}
		}
	}
	p.Finished = time.Since(d.origin)
	return p, nil
}

func (d *metadataPipelineDiagnostics) snapshot(ctx context.Context, f *metadataPipeline) (s metadataPipelineSnapshot, err error) {
	s.Started = time.Since(d.origin)
	if d.Accounting {
		s.CPU, err = metadataPipelineCPU()
		if err != nil {
			return s, err
		}
		s.Memory = metadataPipelineReadMemory()
		s.Receiver, err = f.receiver.command(ctx, "accounting")
		if err != nil {
			return s, err
		}
		s.Progress, err = d.sampleProgress(f, s.Receiver)
	}
	s.Finished = time.Since(d.origin)
	return s, err
}

func (d *metadataPipelineDiagnostics) begin(ctx context.Context, f *metadataPipeline, name string) error {
	if d == nil {
		return nil
	}
	if d.active != "" {
		return fmt.Errorf("phase %s still active", d.active)
	}
	var err error
	d.start, err = d.snapshot(ctx, f)
	if err != nil {
		return err
	}
	if d.CPUPhase == name {
		if err := pprof.StartCPUProfile(d.output); err != nil {
			return err
		}
		d.profiling, d.Profiled = true, true
	}
	if d.Trace && name == "ingestion" {
		if err := trace.Start(d.output); err != nil {
			return err
		}
		d.tracing, d.Traced = true, true
	}
	d.active = name
	d.mark(ctx, name+"-start")
	return nil
}

func (d *metadataPipelineDiagnostics) end(ctx context.Context, f *metadataPipeline, name string) error {
	if d == nil {
		return nil
	}
	if d.active != name {
		return fmt.Errorf("ending %s while %s active", name, d.active)
	}
	d.mark(ctx, name+"-end")
	// Stop flushes the profile synchronously. Later phase timings from this
	// process must not be treated as undisturbed observations.
	if d.profiling {
		pprof.StopCPUProfile()
		d.profiling = false
	}
	if d.tracing && name == "drain" {
		trace.Stop()
		d.tracing = false
	}
	s, err := d.snapshot(ctx, f)
	if err != nil {
		return err
	}
	d.Phases = append(d.Phases, metadataPipelinePhase{Name: name, Start: d.start, End: s})
	d.active = ""
	return nil
}

func (d *metadataPipelineDiagnostics) progress(f *metadataPipeline, stats metadataPipelineReceiverStats) error {
	if d == nil || !d.Accounting || d.active != "drain" {
		return nil
	}
	if len(d.Progress) == cap(d.Progress) {
		return errors.New("drain progress capacity exhausted")
	}
	p, err := d.sampleProgress(f, stats)
	if err == nil {
		d.Progress = append(d.Progress, p)
	}
	return err
}

func (d *metadataPipelineDiagnostics) mark(ctx context.Context, name string) {
	if d == nil {
		return
	}
	d.Events = append(d.Events, metadataPipelineEvent{Name: name, At: time.Since(d.origin)})
	if d.tracing {
		trace.Log(ctx, "metadata-pipeline", name)
	}
}

func (d *metadataPipelineDiagnostics) close() error {
	if d == nil {
		return nil
	}
	if d.profiling {
		pprof.StopCPUProfile()
		d.profiling = false
	}
	if d.tracing {
		trace.Stop()
		d.tracing = false
	}
	if d.output != nil {
		err := d.output.Close()
		d.output = nil
		return err
	}
	return nil
}

func TestMetadataPipelineDiagnostics(t *testing.T) {
	c := metadataPipelineConfig{Source: "native", Case: "backlog", Series: 300, Values: 100, Sweeps: 4, Writers: 1, Shards: 1, Batch: 20, Capacity: 100, CommitSize: 50, ReceiverProcs: 2, Base: time.Now().Add(time.Hour).UnixMilli()}
	t.Run("off", func(t *testing.T) {
		t.Setenv(metadataPipelineDiagnosticEnv, "")
		d, err := metadataPipelineDiagnosticsFromEnv(c)
		require.NoError(t, err)
		require.Nil(t, d)
		require.NoError(t, d.close())
		require.NoError(t, d.begin(t.Context(), nil, "unused"))
	})
	t.Run("accounting and isolated backoff", func(t *testing.T) {
		for _, capValue := range []string{"5ms", ""} {
			t.Run("cap="+capValue, func(t *testing.T) {
				t.Setenv(metadataPipelineDiagnosticEnv, "accounting")
				t.Setenv(metadataPipelineDiagnosticEnv+"_BACKOFF", capValue)
				d, err := metadataPipelineDiagnosticsFromEnv(c)
				require.NoError(t, err)
				f, err := newMetadataPipeline(t.Context(), c)
				require.NoError(t, err)
				defer func() { require.NoError(t, f.close()) }()
				f.diagnostics = d
				require.NoError(t, d.begin(t.Context(), f, "initialization"))
				require.NoError(t, f.open(t.TempDir()))
				if capValue != "" {
					require.Equal(t, 5*time.Millisecond, time.Duration(f.queue.cfg.MaxBackoff))
					require.Equal(t, f.queue.cfg.MinBackoff, f.queue.cfg.MaxBackoff)
				} else {
					require.Equal(t, 5*time.Second, time.Duration(f.queue.cfg.MaxBackoff))
				}
				require.NoError(t, f.append(t.Context(), 0, 0))
				require.NoError(t, d.end(t.Context(), f, "initialization"))
				require.NoError(t, d.begin(t.Context(), f, "drain"))
				_, err = f.drain(t.Context(), f.expectedItems(1))
				require.NoError(t, err)
				require.NoError(t, d.end(t.Context(), f, "drain"))
				require.NotEmpty(t, d.Progress)
				require.Positive(t, d.Progress[len(d.Progress)-1].RecordsRead["samples"])
				require.Zero(t, d.Progress[len(d.Progress)-1].Pending)
				require.Greater(t, d.Phases[0].End.Memory.AllocatedBytes, d.Phases[0].Start.Memory.AllocatedBytes)
				require.NotNil(t, d.Phases[0].End.Receiver.Memory)
				d.Progress = d.Progress[:cap(d.Progress)]
				d.active = "drain"
				require.ErrorContains(t, d.progress(f, metadataPipelineReceiverStats{}), "capacity exhausted")
				cancelled, cancel := context.WithCancel(t.Context())
				cancel()
				_, err = d.snapshot(cancelled, f)
				require.ErrorIs(t, err, context.Canceled)
			})
		}
	})
	t.Run("profile errors and cleanup", func(t *testing.T) {
		t.Setenv(metadataPipelineDiagnosticEnv, "cpu:ingestion")
		t.Setenv(metadataPipelineDiagnosticEnv+"_OUTPUT", filepath.Join(t.TempDir(), "missing", "cpu.pprof"))
		_, err := metadataPipelineDiagnosticsFromEnv(c)
		require.Error(t, err)
		t.Setenv(metadataPipelineDiagnosticEnv+"_OUTPUT", filepath.Join(t.TempDir(), "cpu.pprof"))
		d, err := metadataPipelineDiagnosticsFromEnv(c)
		require.NoError(t, err)
		defer d.close()
		require.NoError(t, d.begin(t.Context(), nil, "ingestion"))
		require.Error(t, d.begin(t.Context(), nil, "drain"))
		require.Error(t, d.end(t.Context(), nil, "drain"))
		require.NoError(t, d.close())
		require.False(t, d.profiling)
		require.Nil(t, d.output)
		require.NoError(t, d.close())
		_, err = metadataPipelineDiagnosticsFromEnv(c)
		require.ErrorIs(t, err, os.ErrExist)
	})
	t.Run("profiler ownership", func(t *testing.T) {
		for _, mode := range []string{"cpu:ingestion", "trace"} {
			t.Run(mode, func(t *testing.T) {
				t.Setenv(metadataPipelineDiagnosticEnv, mode)
				t.Setenv(metadataPipelineDiagnosticEnv+"_OUTPUT", filepath.Join(t.TempDir(), "profile"))
				d, err := metadataPipelineDiagnosticsFromEnv(c)
				require.NoError(t, err)
				defer d.close()
				if mode == "trace" {
					require.NoError(t, trace.Start(io.Discard))
					defer trace.Stop()
				} else {
					require.NoError(t, pprof.StartCPUProfile(io.Discard))
					defer pprof.StopCPUProfile()
				}
				require.Error(t, d.begin(t.Context(), nil, "ingestion"))
				require.False(t, d.tracing)
				require.False(t, d.profiling)
				require.NoError(t, d.close())
				if mode == "trace" {
					require.True(t, trace.IsEnabled(), "cleanup must not stop another owner's trace")
				} else {
					require.Error(t, pprof.StartCPUProfile(io.Discard), "cleanup must not stop another owner's profile")
				}
			})
		}
	})
}
