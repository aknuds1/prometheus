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
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"slices"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
)

// The attribution mode repeats a pipeline case's whole lifecycle within one
// benchmark iteration, so that one CPU profile can span many repetitions. Each
// repetition gets a fresh pipeline and an empty metadata interner, but the
// repetitions share one process and its runtime state.
const (
	// metadataPipelineRepeatEnv sets the number of repetitions; unset means the
	// ordinary benchmark.
	metadataPipelineRepeatEnv = "PROMETHEUS_METADATA_PIPELINE_REPEAT"
	// metadataPipelineProfileEnv names a new file for one CPU profile spanning
	// every repetition; unset means no profile.
	metadataPipelineProfileEnv = "PROMETHEUS_METADATA_PIPELINE_CPU_PROFILE"
	// metadataPipelineSeedOnlyEnv, set to 1, repeats the lifecycle without its
	// measured steps.
	metadataPipelineSeedOnlyEnv = "PROMETHEUS_METADATA_PIPELINE_SEED_ONLY"
)

// metadataPipelineMarks records phase boundaries while the attribution mode
// runs, and is nil otherwise.
var metadataPipelineMarks *metadataPipelineAttribution

// metadataPipelineResetHook, if set by a test, runs after each repetition's
// interner reset.
var metadataPipelineResetHook func()

// metadataPipelineAttribution records the process's CPU at named phase
// boundaries of one repetition.
type metadataPipelineAttribution struct {
	marks []metadataPipelineMark
	err   error
}

// metadataPipelineMark is a phase boundary: the process's cumulative user and
// system CPU when it was reached.
type metadataPipelineMark struct {
	Name string
	CPU  time.Duration
}

// metadataPipelineAttributionSummary describes the window of one repeated run.
// Its CPU readings are cumulative process CPU, like the marks.
type metadataPipelineAttributionSummary struct {
	Repeat    int
	SeedOnly  bool
	Profile   string
	ProfileHz int
	// StartCPU and EndCPU bound the window. The first repetition's between
	// phase starts at StartCPU, so the window has no start gap.
	StartCPU, EndCPU time.Duration
	Wall             time.Duration
	// Phases totals each phase over the repetitions. A phase is named by the
	// mark that starts it, apart from "between", which ends at a creation mark.
	Phases map[string]time.Duration
	// EndGap runs from the last repetition's end mark to EndCPU.
	EndGap time.Duration
	// Residual is the window's CPU less every phase and the end gap.
	Residual time.Duration
	// For the tests: the values each repetition's interner held right after
	// its reset and at the repetition's end, and the collections forced in
	// the window. Sizes, not interners, so that nothing retains a superseded
	// interner.
	internerStart, internerEnd []int
	collections                uint64
}

// Mark names, in the order a repetition reaches them. The seed-only lifecycle
// has no measured phase.
var (
	metadataPipelineMarkNames         = []string{"creation", "setup", "seeding", "measured", "shutdown", "post", "end"}
	metadataPipelineSeedOnlyMarkNames = []string{"creation", "setup", "seeding", "shutdown", "post", "end"}
)

func (a *metadataPipelineAttribution) mark(name string) {
	if a == nil {
		return
	}
	cpu, err := metadataPipelineCPU()
	if err != nil {
		a.err = errors.Join(a.err, err)
		return
	}
	a.markAt(name, cpu)
}

// markAt records a boundary at a CPU reading already taken, so that a phase
// can share its boundary with a figure the benchmark reports.
func (a *metadataPipelineAttribution) markAt(name string, cpu metadataPipelineCPUUsage) {
	if a == nil {
		return
	}
	if !cpu.Available {
		a.err = errors.Join(a.err, errors.New("process CPU is unavailable"))
		return
	}
	a.marks = append(a.marks, metadataPipelineMark{Name: name, CPU: cpu.User + cpu.System})
}

func metadataPipelineProcessCPU() (time.Duration, error) {
	cpu, err := metadataPipelineCPU()
	if err == nil && !cpu.Available {
		err = errors.New("process CPU is unavailable")
	}
	return cpu.User + cpu.System, err
}

// benchmarkMetadataPipelineAttribution runs the attribution mode and logs each
// repetition's result, then the window's summary.
func benchmarkMetadataPipelineAttribution(b *testing.B, c metadataPipelineConfig, repeat int) {
	b.Helper()
	results, summary := runMetadataPipelineAttribution(b, c, repeat, os.Getenv(metadataPipelineSeedOnlyEnv) == "1", os.Getenv(metadataPipelineProfileEnv))
	for _, r := range results {
		encoded, err := json.Marshal(r)
		require.NoError(b, err)
		b.Logf("metadata-pipeline-result: %s", encoded)
	}
	encoded, err := json.Marshal(summary)
	require.NoError(b, err)
	b.Logf("metadata-pipeline-attribution: %s", encoded)
	b.ReportMetric(float64(repeat), "repetitions/op")
}

// runMetadataPipelineAttribution repeats c's lifecycle, or its seed-only
// lifecycle. With a profile path, one CPU profile spans every repetition. The
// window's CPU is read right after the profile starts and right before it
// stops; repetition i's work before its creation mark, from the previous
// repetition's end mark, resets the interner and collects garbage.
func runMetadataPipelineAttribution(b *testing.B, c metadataPipelineConfig, repeat int, seedOnly bool, path string) ([]metadataPipelineResult, metadataPipelineAttributionSummary) {
	b.Helper()
	b.StopTimer()
	c.Base = metadataPipelineBase
	summary := metadataPipelineAttributionSummary{Repeat: repeat, SeedOnly: seedOnly, Profile: path}
	var out *os.File
	if path != "" {
		var err error
		out, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		require.NoError(b, err)
		require.NoError(b, pprof.StartCPUProfile(out))
		// Go's CPU profiler samples at its default rate.
		summary.ProfileHz = 100
	}
	forced := metadataPipelineForcedCollections()
	var err error
	summary.StartCPU, err = metadataPipelineProcessCPU()
	require.NoError(b, err)
	start := time.Now()
	marks := &metadataPipelineAttribution{}
	metadataPipelineMarks = marks
	defer func() { metadataPipelineMarks = nil }()
	results := make([]metadataPipelineResult, 0, repeat)
	for range repeat {
		marks.marks = marks.marks[:0]
		metadataPipelineResetInterner()
		summary.internerStart = append(summary.internerStart, metadataPipelineInternerSize())
		if metadataPipelineResetHook != nil {
			metadataPipelineResetHook()
		}
		runtime.GC()
		var r metadataPipelineResult
		if seedOnly {
			r = measureMetadataPipelineSeedOnly(b, c)
		} else {
			r = measureMetadataPipeline(b, c)
		}
		marks.mark("end")
		require.NoError(b, marks.err)
		r.Attribution = slices.Clone(marks.marks)
		results = append(results, r)
		summary.internerEnd = append(summary.internerEnd, metadataPipelineInternerSize())
	}
	summary.EndCPU, err = metadataPipelineProcessCPU()
	require.NoError(b, err)
	summary.collections = metadataPipelineForcedCollections() - forced
	summary.Wall = time.Since(start)
	if out != nil {
		pprof.StopCPUProfile()
		require.NoError(b, out.Close())
	}
	summary.account(results)
	return results, summary
}

// account totals the phases of results within the window.
func (s *metadataPipelineAttributionSummary) account(results []metadataPipelineResult) {
	s.Phases = map[string]time.Duration{}
	previous, counted := s.StartCPU, time.Duration(0)
	for _, r := range results {
		for i, m := range r.Attribution {
			name := "between"
			if i > 0 {
				name = r.Attribution[i-1].Name
			}
			s.Phases[name] += m.CPU - previous
			counted += m.CPU - previous
			previous = m.CPU
		}
	}
	s.EndGap = s.EndCPU - previous
	s.Residual = s.EndCPU - s.StartCPU - counted - s.EndGap
}

// metadataPipelineSeedOnlyConfig is c without its measured steps, for the
// receivers' and the WAL oracle's expectations.
func metadataPipelineSeedOnlyConfig(c metadataPipelineConfig) metadataPipelineConfig {
	c.Sweeps, c.ReleaseLag = 0, 0
	return c
}

// measureMetadataPipelineSeedOnly runs measureMetadataPipeline's lifecycle
// without its measured steps, in the same order: creation, setup, and the seed
// with its drain, metrics and any backlog hold, then the sender's shutdown, the
// checks, closing the DB and the WAL oracle. Its memory statistics are read
// where measureMetadataPipeline reads them, but not reported.
func measureMetadataPipelineSeedOnly(b *testing.B, c metadataPipelineConfig) metadataPipelineResult {
	metadataPipelineMarks.mark("creation")
	timeout := metadataPipelineSetting(b, "PROMETHEUS_METADATA_PIPELINE_TIMEOUT_MINUTES", 5)
	ctx, cancel := context.WithTimeout(b.Context(), time.Duration(timeout)*time.Minute)
	defer cancel()
	c = metadataPipelineSeedOnlyConfig(c)
	dir := b.TempDir()
	f, err := newMetadataPipeline(ctx, c)
	require.NoError(b, err)
	defer func() { require.NoError(b, f.close()) }()
	r := metadataPipelineResult{Config: c}
	_, err = f.receiver.command(ctx, "stats")
	require.NoError(b, err)
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	lifecycleCPU, err := metadataPipelineCPU()
	require.NoError(b, err)
	lifecycleStart := time.Now()
	metadataPipelineMarks.markAt("setup", lifecycleCPU)
	require.NoError(b, f.open(dir))
	metadataPipelineMarks.mark("seeding")
	require.NoError(b, f.append(ctx, 0, 0))
	_, err = f.drain(ctx, f.expectedItems(1))
	require.NoError(b, err)
	seedMetrics, err := f.metrics()
	require.NoError(b, err)
	r.SeedWALBytes = seedMetrics["prometheus_tsdb_wal_record_parts_bytes_written_total"]
	_, err = f.metrics()
	require.NoError(b, err)
	if c.Case == "backlog" {
		require.NoError(b, f.holdReceivers(ctx))
	}
	r.Initialization = time.Since(lifecycleStart)
	runtime.ReadMemStats(&memory)
	live, err := f.metrics()
	require.NoError(b, err)
	require.NoError(b, checkMetadataPipelineMetrics(live))
	r.UnknownEntryCounters = metadataPipelineUnknownCounters(live)
	stopStart := time.Now()
	metadataPipelineMarks.mark("shutdown")
	require.NoError(b, f.closeSender())
	r.Shutdown = time.Since(stopStart)
	metadataPipelineMarks.mark("post")
	runtime.ReadMemStats(&memory)
	r.ResidentSeries = f.db.Head().NumSeries()
	require.Equal(b, uint64(c.residentSeries()), r.ResidentSeries)
	metrics, err := f.metrics()
	require.NoError(b, err)
	require.NoError(b, checkMetadataPipelineMetrics(metrics))
	r.LifecycleWALBytes = metrics["prometheus_tsdb_wal_record_parts_bytes_written_total"]
	closeStart := time.Now()
	require.NoError(b, f.db.Close())
	f.db = nil
	r.DBClose = time.Since(closeStart)
	r.Lifecycle = time.Since(lifecycleStart)
	lifecycleEndCPU, err := metadataPipelineCPU()
	require.NoError(b, err)
	r.LifecycleCPU = lifecycleEndCPU.sub(lifecycleCPU)
	runtime.ReadMemStats(&memory)
	walDir := filepath.Join(dir, "wal")
	r.WALPayloadBytes, err = metadataPipelinePayloadBytes(walDir, 0)
	require.NoError(b, err)
	require.NoError(b, checkMetadataPipelineWAL(c, walDir, nil))
	r.MetadataOracle = true
	return r
}

// runMetadataPipelineAttributionOnce runs fn as a benchmark exactly once.
// testing.Benchmark calls it again with a larger b.N for as long as the timer
// reads less than the benchmark time, and the attribution mode stops the timer.
func runMetadataPipelineAttributionOnce(t *testing.T, fn func(b *testing.B)) {
	t.Helper()
	var ran bool
	testing.Benchmark(func(b *testing.B) {
		if ran {
			return
		}
		ran = true
		fn(b)
	})
	require.True(t, ran)
}

// metadataPipelineForcedCollections returns the number of collections forced
// so far, as by runtime.GC.
func metadataPipelineForcedCollections() uint64 {
	s := []metrics.Sample{{Name: "/gc/cycles/forced:gc-cycles"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

func TestRemoteWriteMetadataPipelineAttribution(t *testing.T) {
	// A small held backlog, so each repetition is quick.
	c := metadataPipelineConfig{Case: "backlog", Series: 300, Values: 100, Sweeps: 4, Writers: 1, Shards: 1, Batch: 20, Capacity: 100, CommitSize: 50, ReceiverProcs: 2}
	for _, seedOnly := range []bool{false, true} {
		name := "full lifecycle"
		names := metadataPipelineMarkNames
		if seedOnly {
			name, names = "seed-only lifecycle", metadataPipelineSeedOnlyMarkNames
		}
		for _, source := range []string{"disabled", "wal", "native"} {
			t.Run(name+"/source="+source, func(t *testing.T) {
				c := c
				c.Source = source
				path := filepath.Join(t.TempDir(), "cpu.pprof")
				var results []metadataPipelineResult
				var summary metadataPipelineAttributionSummary
				runMetadataPipelineAttributionOnce(t, func(b *testing.B) {
					results, summary = runMetadataPipelineAttribution(b, c, 3, seedOnly, path)
				})
				// A failed run returns no results.
				require.Len(t, results, 3)
				// Each repetition starts after a collection, with a new
				// interner where the base has one.
				require.Equal(t, uint64(3), summary.collections)
				// Every repetition's interner starts empty, including after one
				// that filled it: the WAL source interns where the base has an
				// interner.
				require.Equal(t, []int{0, 0, 0}, summary.internerStart)
				require.Len(t, summary.internerEnd, 3)
				if metadataPipelineInterner() != nil && source == "wal" {
					for _, n := range summary.internerEnd {
						require.Positive(t, n)
					}
				}
				previous := summary.StartCPU
				for _, r := range results {
					require.True(t, r.MetadataOracle)
					require.Equal(t, metadataPipelineBase, r.Config.Base)
					got := make([]string, 0, len(r.Attribution))
					for _, m := range r.Attribution {
						got = append(got, m.Name)
						require.GreaterOrEqual(t, m.CPU, previous, "marks never go back")
						previous = m.CPU
					}
					require.Equal(t, names, got)
					if seedOnly {
						require.Zero(t, r.Config.Sweeps)
						require.Zero(t, r.Samples)
					} else {
						require.Positive(t, r.Samples)
						require.Positive(t, r.CPU.User+r.CPU.System)
					}
				}
				require.LessOrEqual(t, previous, summary.EndCPU)
				require.Zero(t, summary.Residual)
				require.Equal(t, summary.EndCPU-previous, summary.EndGap)
				phases := append([]string{"between"}, names[:len(names)-1]...)
				require.ElementsMatch(t, phases, slices.Collect(maps.Keys(summary.Phases)))
				if !seedOnly {
					// The timed CPU keeps its boundaries, which the measured
					// phase starts and the shutdown phase ends.
					var timed time.Duration
					for _, r := range results {
						timed += r.CPU.User + r.CPU.System
					}
					require.Equal(t, timed, summary.Phases["measured"]+summary.Phases["shutdown"])
				}
				require.Positive(t, summary.Wall)
				require.Equal(t, 100, summary.ProfileHz)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				p, err := profile.ParseData(data)
				require.NoError(t, err)
				require.Equal(t, int64(10*time.Millisecond), p.Period)
			})
		}
	}
	t.Run("interner reset", func(t *testing.T) {
		// Each repetition starts with a new interner, as a fresh process has;
		// bases without one have nothing to reset.
		before := metadataPipelineInterner()
		metadataPipelineResetInterner()
		after := metadataPipelineInterner()
		require.Zero(t, metadataPipelineInternerSize())
		if before == nil {
			require.Nil(t, after)
			return
		}
		require.NotNil(t, after)
		require.NotSame(t, before, after)
	})
	t.Run("superseded interners are reclaimed", func(t *testing.T) {
		// Each repetition's interner must become unreachable once the next
		// replaces it: nothing the attribution mode keeps may retain one, or
		// later repetitions would scan earlier caches.
		if metadataPipelineInterner() == nil {
			t.Skip("this base has no interner")
		}
		var created, reclaimed atomic.Int64
		metadataPipelineResetHook = func() {
			created.Add(1)
			runtime.SetFinalizer(metadataPipelineInterner(), func(any) { reclaimed.Add(1) })
		}
		defer func() { metadataPipelineResetHook = nil }()
		var results []metadataPipelineResult
		var summary metadataPipelineAttributionSummary
		runMetadataPipelineAttributionOnce(t, func(b *testing.B) {
			c := c
			c.Source = "wal"
			results, summary = runMetadataPipelineAttribution(b, c, 3, false, "")
		})
		require.Len(t, results, 3)
		require.Equal(t, int64(3), created.Load())
		// Supersede the last repetition's interner too.
		metadataPipelineResetInterner()
		for deadline := time.Now().Add(10 * time.Second); reclaimed.Load() < 3 && time.Now().Before(deadline); {
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
		}
		require.Equal(t, int64(3), reclaimed.Load(), "superseded interners reclaimed")
		runtime.KeepAlive(results)
		runtime.KeepAlive(summary)
	})
	t.Run("an existing profile is kept", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cpu.pprof")
		require.NoError(t, os.WriteFile(path, []byte("earlier"), 0o600))
		var results []metadataPipelineResult
		runMetadataPipelineAttributionOnce(t, func(b *testing.B) {
			c := c
			c.Source = "wal"
			results, _ = runMetadataPipelineAttribution(b, c, 1, false, path)
		})
		require.Empty(t, results)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "earlier", string(data))
	})
	t.Run("no profile without a path", func(t *testing.T) {
		var results []metadataPipelineResult
		var summary metadataPipelineAttributionSummary
		runMetadataPipelineAttributionOnce(t, func(b *testing.B) {
			c := c
			c.Source = "wal"
			results, summary = runMetadataPipelineAttribution(b, c, 1, false, "")
		})
		require.Len(t, results, 1)
		require.Empty(t, summary.Profile)
		require.Zero(t, summary.ProfileHz)
	})
}
