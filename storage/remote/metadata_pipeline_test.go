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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/util/compression"
)

const metadataPipelineReceiverEnv = "PROMETHEUS_METADATA_PIPELINE_RECEIVER"

// metadataPipelineConfig describes a finite trace. Step zero seeds the warm
// cases; cold initialization measures only step zero. IDs survive WAL ref changes.
type metadataPipelineConfig struct {
	Group                            string
	Case, Source                     string
	Series, Values, Sweeps           int
	Writers, Shards, Batch, Capacity int
	CommitSize, ReceiverProcs        int
	Base                             int64
	Mixed                            bool
	SweepInterval                    time.Duration
	SamplesPerSecond                 int
}

func (c metadataPipelineConfig) lastStep() int {
	if c.Case == "cold" {
		return 0
	}
	return c.Sweeps
}

func (c metadataPipelineConfig) generation(slot, step int) int {
	if c.Case != "newseries" || step <= slot%100 {
		return 0
	}
	return 1 + (step-1-slot%100)/100
}

func (c metadataPipelineConfig) firstStep(id int) int {
	if generation := id / c.Series; generation > 0 {
		return 1 + id%c.Series%100 + (generation-1)*100
	}
	return 0
}

func (c metadataPipelineConfig) version(slot, step int) int {
	switch c.Case {
	case "backlog":
		return step
	case "changes", "changes-distinct", "paced-changes":
		if step > slot%100 {
			return 1 + (step-1-slot%100)/100
		}
	}
	return 0
}

func (c metadataPipelineConfig) residentSeries() int {
	n := c.Series
	if c.Case == "newseries" {
		n += c.Sweeps * (c.Series / 100)
	}
	return n
}

func (c metadataPipelineConfig) idCapacity() int {
	if c.Case == "newseries" {
		return c.Series * (1 + (c.Sweeps+99)/100)
	}
	return c.Series
}

func metadataPipelineLabels(id int) labels.Labels {
	return labels.FromStrings(labels.MetricName, "pipeline_seconds_total", "id", strconv.Itoa(id),
		"job", "pipeline", "cluster", "benchmark", "region", "local", "environment", "test")
}

func (c metadataPipelineConfig) metadata(slot, version int) metadata.Metadata {
	prefix := fmt.Sprintf("family %d version %d ", slot%c.Values, version)
	typ := model.MetricTypeCounter
	if c.Mixed && slot%3 != 0 {
		typ = model.MetricTypeHistogram
	}
	return metadata.Metadata{Type: typ, Unit: "seconds", Help: prefix + strings.Repeat("x", 64-len(prefix))}
}

// metadataPipelineReceiver validates each series independently, allowing shard
// concurrency while retaining only sequence counters, never request histories.
type metadataPipelineReceiver struct {
	config   metadataPipelineConfig
	series   []metadataPipelineReceivedSeries
	labels   []labels.Labels
	metadata [][]metadata.Metadata
	mu       sync.Mutex
	stats    metadataPipelineReceiverStats
	gate     chan struct{}
}

type metadataPipelineReceivedSeries struct {
	sync.Mutex
	nextSample, nextExemplar int
}

type metadataPipelineReceiverStats struct {
	Address                                     string
	Error                                       string
	Samples, Histograms, Exemplars              int64
	Requests, Bytes, HeldRequests, ServiceNanos int64
	CPU                                         metadataPipelineCPUUsage
}

func (s metadataPipelineReceiverStats) items() int64 {
	return s.Samples + s.Histograms + s.Exemplars
}

func (r *metadataPipelineReceiver) validate(req *writev2.Request) (samples, histograms, exemplars int64, err error) {
	builder := labels.NewScratchBuilder(6)
	for _, series := range req.Timeseries {
		lset, e := series.ToLabels(&builder, req.Symbols)
		if e != nil {
			return 0, 0, 0, e
		}
		id, e := strconv.Atoi(lset.Get("id"))
		if e != nil || id < 0 || id >= len(r.series) || !labels.Equal(lset, r.labels[id]) {
			return 0, 0, 0, fmt.Errorf("unexpected labels %s", lset)
		}
		md, e := series.ToMetadata(req.Symbols)
		if e != nil {
			return 0, 0, 0, e
		}
		check := func(timestamp int64, value float64, isExemplar bool) error {
			if timestamp < r.config.Base || (timestamp-r.config.Base)%15000 != 0 {
				return fmt.Errorf("unexpected timestamp %d", timestamp)
			}
			step := int((timestamp - r.config.Base) / 15000)
			slot := id % r.config.Series
			if step > r.config.lastStep() || id/r.config.Series != r.config.generation(slot, step) || value != float64(step+1) {
				return fmt.Errorf("unexpected sample id=%d step=%d value=%g", id, step, value)
			}
			want := r.metadata[slot][r.config.version(slot, step)]
			if r.config.Source == "disabled" {
				want = metadata.Metadata{Type: model.MetricTypeUnknown}
			}
			if md != want {
				return fmt.Errorf("metadata id=%d step=%d: got %+v, want %+v", id, step, md, want)
			}
			state := &r.series[id]
			state.Lock()
			defer state.Unlock()
			next := &state.nextSample
			if isExemplar {
				next = &state.nextExemplar
			}
			if step != *next {
				return fmt.Errorf("missing, duplicate, or reordered item id=%d: got step=%d, want=%d", id, step, *next)
			}
			*next++
			return nil
		}
		for _, sample := range series.Samples {
			if r.config.Mixed && id%r.config.Series%3 != 0 {
				return 0, 0, 0, errors.New("expected histogram, got float sample")
			}
			if e := check(sample.Timestamp, sample.Value, false); e != nil {
				return 0, 0, 0, e
			}
			samples++
		}
		for _, h := range series.Histograms {
			slot := id % r.config.Series
			if !r.config.Mixed || slot%3 == 0 || h.IsFloatHistogram() != (slot%3 == 2) {
				return 0, 0, 0, errors.New("unexpected histogram kind")
			}
			fh := h.ToFloatHistogram()
			if fh.Count != fh.Sum || fh.ZeroCount != fh.Count || len(fh.PositiveBuckets)+len(fh.NegativeBuckets) != 0 {
				return 0, 0, 0, errors.New("unexpected histogram contents")
			}
			if e := check(h.Timestamp, h.Sum, false); e != nil {
				return 0, 0, 0, e
			}
			histograms++
		}
		for _, ex := range series.Exemplars {
			if !r.config.Mixed || id%r.config.Series%3 != 0 || len(ex.LabelsRefs) != 2 || int(ex.LabelsRefs[0]) >= len(req.Symbols) || int(ex.LabelsRefs[1]) >= len(req.Symbols) || req.Symbols[ex.LabelsRefs[0]] != "trace_id" || req.Symbols[ex.LabelsRefs[1]] != "trace" {
				return 0, 0, 0, errors.New("unexpected exemplar")
			}
			if e := check(ex.Timestamp, ex.Value, true); e != nil {
				return 0, 0, 0, e
			}
			exemplars++
		}
	}
	if samples+histograms+exemplars == 0 {
		return 0, 0, 0, errors.New("empty delivery")
	}
	return samples, histograms, exemplars, nil
}

func (r *metadataPipelineReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	begin := time.Now()
	body, err := io.ReadAll(io.LimitReader(req.Body, 32<<20))
	var samples, histograms, exemplars int64
	if err == nil {
		var decoded []byte
		decoded, err = compression.Decode(compression.Snappy, body, nil)
		if err == nil {
			var request writev2.Request
			if err = request.Unmarshal(decoded); err == nil {
				samples, histograms, exemplars, err = r.validate(&request)
			}
		}
	}
	r.mu.Lock()
	r.stats.ServiceNanos += time.Since(begin).Nanoseconds()
	if err != nil && r.stats.Error == "" {
		r.stats.Error = err.Error()
	}
	gate := r.gate
	if gate != nil {
		r.stats.HeldRequests++
	}
	r.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if gate != nil {
		select {
		case <-gate:
		case <-req.Context().Done():
			return
		}
	}
	w.Header().Set("X-Prometheus-Remote-Write-Samples-Written", strconv.FormatInt(samples, 10))
	w.Header().Set("X-Prometheus-Remote-Write-Histograms-Written", strconv.FormatInt(histograms, 10))
	w.Header().Set("X-Prometheus-Remote-Write-Exemplars-Written", strconv.FormatInt(exemplars, 10))
	w.WriteHeader(http.StatusNoContent)
	r.mu.Lock()
	r.stats.Samples += samples
	r.stats.Histograms += histograms
	r.stats.Exemplars += exemplars
	r.stats.Bytes += int64(len(body))
	r.stats.Requests++
	r.mu.Unlock()
}

// TestRemoteWriteMetadataPipelineReceiver is the test binary's subprocess entry
// point. Control traffic uses pipes, separate from the measured RW2 connection.
func TestRemoteWriteMetadataPipelineReceiver(t *testing.T) {
	encoded := os.Getenv(metadataPipelineReceiverEnv)
	if encoded == "" {
		t.Skip("receiver subprocess only")
	}
	var c metadataPipelineConfig
	require.NoError(t, json.Unmarshal([]byte(encoded), &c))
	r := &metadataPipelineReceiver{config: c, series: make([]metadataPipelineReceivedSeries, c.idCapacity())}
	r.labels, r.metadata = metadataPipelineInputs(c)
	for id := range r.series {
		r.series[id].nextSample = c.firstStep(id)
		r.series[id].nextExemplar = c.firstStep(id)
	}
	server := httptest.NewServer(r)
	defer server.Close()
	r.stats.Address = server.URL
	encoder, decoder := json.NewEncoder(os.Stdout), json.NewDecoder(os.Stdin)
	require.NoError(t, encoder.Encode(r.stats))
	for {
		var command string
		if err := decoder.Decode(&command); err != nil {
			require.ErrorIs(t, err, io.EOF)
			command = "stop"
		}
		r.mu.Lock()
		switch command {
		case "hold":
			if r.gate == nil {
				r.gate = make(chan struct{})
			}
		case "release", "stop":
			if r.gate != nil {
				close(r.gate)
				r.gate = nil
			}
		case "stats":
		default:
			r.stats.Error = "unknown control command: " + command
		}
		stats := r.stats
		r.mu.Unlock()
		var err error
		stats.CPU, err = metadataPipelineCPU()
		require.NoError(t, err)
		require.NoError(t, encoder.Encode(stats))
		if command == "stop" {
			return
		}
	}
}

type metadataPipelineReceiverProcess struct {
	encoder *json.Encoder
	stdin   io.WriteCloser
	replies chan metadataPipelineReceiverStats
	done    chan error
	readErr error // Published by closing replies; repeated reads must not consume it.
	cancel  context.CancelFunc
	address string
}

func startMetadataPipelineReceiver(ctx context.Context, c metadataPipelineConfig) (*metadataPipelineReceiverProcess, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRemoteWriteMetadataPipelineReceiver$", "-test.timeout=10m")
	cmd.Env = append(os.Environ(), metadataPipelineReceiverEnv+"="+string(encoded), "GOMAXPROCS="+strconv.Itoa(c.ReceiverProcs))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	p := &metadataPipelineReceiverProcess{encoder: json.NewEncoder(stdin), stdin: stdin, cancel: cancel, replies: make(chan metadataPipelineReceiverStats, 1), done: make(chan error, 1)}
	go func() {
		decoder := json.NewDecoder(stdout)
		for {
			var stats metadataPipelineReceiverStats
			if err := decoder.Decode(&stats); err != nil {
				buffered, _ := io.ReadAll(io.LimitReader(decoder.Buffered(), 4096))
				p.readErr = fmt.Errorf("receiver protocol: %w; output: %s", err, buffered)
				break
			}
			select {
			case p.replies <- stats:
			case <-ctx.Done():
			}
		}
		close(p.replies)
		p.done <- cmd.Wait()
	}()
	stats, err := p.receive(ctx)
	if err != nil {
		cancel()
		_ = stdin.Close()
		<-p.done
		return nil, err
	}
	p.address = stats.Address
	return p, nil
}

func (p *metadataPipelineReceiverProcess) receive(ctx context.Context) (metadataPipelineReceiverStats, error) {
	select {
	case stats, ok := <-p.replies:
		if !ok {
			return stats, p.readErr
		}
		if stats.Error != "" {
			return stats, errors.New(stats.Error)
		}
		return stats, nil
	case <-ctx.Done():
		return metadataPipelineReceiverStats{}, ctx.Err()
	}
}

func (p *metadataPipelineReceiverProcess) command(ctx context.Context, command string) (metadataPipelineReceiverStats, error) {
	if err := p.encoder.Encode(command); err != nil {
		return metadataPipelineReceiverStats{}, err
	}
	return p.receive(ctx)
}

func (p *metadataPipelineReceiverProcess) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, commandErr := p.command(ctx, "stop")
	_ = p.stdin.Close()
	select {
	case err := <-p.done:
		p.cancel()
		return errors.Join(commandErr, err)
	case <-ctx.Done():
		p.cancel()
		return errors.Join(commandErr, ctx.Err(), <-p.done)
	}
}

type metadataPipeline struct {
	config                   metadataPipelineConfig
	labels                   []labels.Labels
	metadata                 [][]metadata.Metadata
	refs                     []storage.SeriesRef
	receiver                 *metadataPipelineReceiverProcess
	registry                 *prometheus.Registry
	observer                 *prometheus.Registry
	db                       *tsdb.DB
	sender                   *Storage
	queue                    *QueueManager
	latency                  [][]time.Duration
	peak                     []int64
	lateness                 [][]time.Duration
	enqueueRetriesBeforeHold float64
}

func newMetadataPipeline(ctx context.Context, c metadataPipelineConfig) (*metadataPipeline, error) {
	if c.Series <= 0 || c.Writers <= 0 || c.CommitSize <= 0 {
		return nil, errors.New("series, writers, and commit size must be positive")
	}
	if c.SamplesPerSecond < 0 || (c.SamplesPerSecond > 0 && (c.SweepInterval != 0 || c.Series%c.Writers != 0)) {
		return nil, errors.New("transaction pacing requires equal writer partitions and no sweep pacing")
	}
	f := &metadataPipeline{
		config: c, registry: prometheus.NewRegistry(), observer: prometheus.NewRegistry(), refs: make([]storage.SeriesRef, c.Series),
		latency: make([][]time.Duration, c.Writers), peak: make([]int64, c.Writers),
		lateness: make([][]time.Duration, c.Writers),
	}
	f.labels, f.metadata = metadataPipelineInputs(c)
	for w := range f.latency {
		if c.Group != "" {
			// Keep instrumentation storage fixed across seed, backlog, and drain
			// heap checkpoints, including the single-writer seed phase.
			transactions := ((c.Series*(w+1)/c.Writers - c.Series*w/c.Writers + c.CommitSize - 1) / c.CommitSize) * c.Sweeps
			capacity := transactions
			if w == 0 {
				capacity = max(capacity, (c.Series+c.CommitSize-1)/c.CommitSize)
			}
			f.latency[w] = make([]time.Duration, 0, capacity)
			if c.SamplesPerSecond > 0 {
				f.lateness[w] = make([]time.Duration, 0, transactions)
			}
		} else {
			f.latency[w] = make([]time.Duration, 0, (c.Series/c.Writers/c.CommitSize+1)*(c.lastStep()+1))
		}
	}
	var err error
	f.receiver, err = startMetadataPipelineReceiver(ctx, c)
	return f, err
}

func metadataPipelineInputs(c metadataPipelineConfig) ([]labels.Labels, [][]metadata.Metadata) {
	lsets := make([]labels.Labels, c.idCapacity())
	for id := range lsets {
		lsets[id] = metadataPipelineLabels(id)
	}
	values := make([][]metadata.Metadata, c.Series)
	shared := make(map[[2]int][]metadata.Metadata)
	for slot := range values {
		key := [2]int{slot % c.Values, 0}
		if c.Mixed && slot%3 != 0 {
			key[1] = 1
		}
		if shared[key] == nil {
			shared[key] = make([]metadata.Metadata, 5)
			for version := range 5 {
				shared[key][version] = c.metadata(slot, version)
			}
		}
		values[slot] = shared[key]
	}
	return lsets, values
}

func (f *metadataPipeline) open(dir string) error {
	c := f.config
	opts := tsdb.DefaultOptions()
	opts.EnableNativeMetadata = c.Source == "native"
	opts.EnableMetadataWALRecords = c.Source == "wal"
	opts.WALCompression = compression.Snappy
	opts.EnableExemplarStorage, opts.MaxExemplars = c.Mixed, int64(c.Series*2)
	var err error
	f.db, err = tsdb.Open(dir, nil, f.registry, opts, nil)
	if err != nil {
		return err
	}
	f.db.DisableCompactions()
	var reader storage.NativeMetricMetadataReader
	if c.Source == "native" {
		reader = f.db
	}
	f.sender = NewStorage(nil, f.registry, f.db.StartTime, dir, 5*time.Second, nil, false, reader)
	rw := baseRemoteWriteConfig(f.receiver.address)
	rw.ProtobufMessage = remoteapi.WriteV2MessageType
	rw.SendExemplars, rw.SendNativeHistograms = c.Mixed, c.Mixed
	rw.MetadataConfig.Send = false
	rw.QueueConfig.MinShards, rw.QueueConfig.MaxShards = c.Shards, c.Shards
	rw.QueueConfig.MaxSamplesPerSend, rw.QueueConfig.Capacity = c.Batch, c.Capacity
	rw.QueueConfig.BatchSendDeadline = model.Duration(100 * time.Millisecond)
	if err := f.sender.ApplyConfig(&config.Config{RemoteWriteConfigs: []*config.RemoteWriteConfig{rw}}); err != nil {
		return err
	}
	// Configuration is now fixed. Only observe counters; never lock queue internals
	// to control when the watcher reads metadata or samples.
	for _, queue := range f.sender.rws.queues {
		f.queue = queue
	}
	// Retain collectors after sender shutdown so validation and metric collection
	// need not run inside the measured interval.
	m := f.queue.metrics
	f.observer.MustRegister(m.failedSamplesTotal, m.failedHistogramsTotal, m.failedExemplarsTotal,
		m.retriedSamplesTotal, m.retriedHistogramsTotal, m.retriedExemplarsTotal,
		m.droppedSamplesTotal, m.droppedHistogramsTotal, m.droppedExemplarsTotal)
	f.db.SetWriteNotified(f.sender)
	return nil
}

func (f *metadataPipeline) pending() int64 {
	return f.queue.shards.enqueuedSamples.Load() + f.queue.shards.enqueuedHistograms.Load() + f.queue.shards.enqueuedExemplars.Load()
}

func (f *metadataPipeline) holdReceiver(ctx context.Context) error {
	f.enqueueRetriesBeforeHold = testutil.ToFloat64(f.queue.metrics.enqueueRetriesTotal)
	_, err := f.receiver.command(ctx, "hold")
	return err
}

// awaitBacklog waits for both a held HTTP request and actual enqueue backpressure.
// Merely withholding an acknowledgement need not block the WAL reader.
func (f *metadataPipeline) awaitBacklog(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		stats, err := f.receiver.command(ctx, "stats")
		if err != nil {
			return err
		}
		if stats.HeldRequests > 0 && testutil.ToFloat64(f.queue.metrics.enqueueRetriesTotal) > f.enqueueRetriesBeforeHold {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// metadataPipelinePacer waits for absolute deadlines without shifting late work.
type metadataPipelinePacer struct {
	start    time.Time
	interval time.Duration
	timer    *time.Timer
}

func (p *metadataPipelinePacer) wait(ctx context.Context, step int) (time.Duration, error) {
	return p.waitUntil(ctx, p.start.Add(time.Duration(step)*p.interval))
}

// waitSamples schedules each writer's cumulative sample count at its share of the
// aggregate rate. Integer division happens last to avoid accumulating rounding.
func (p *metadataPipelinePacer) waitSamples(ctx context.Context, samples, writer, writers, commitSize, rate int) (time.Duration, error) {
	offset := (time.Duration(samples)*time.Duration(writers) + time.Duration(writer)*time.Duration(commitSize)) * time.Second / time.Duration(rate)
	return p.waitUntil(ctx, p.start.Add(offset))
}

func (p *metadataPipelinePacer) waitUntil(ctx context.Context, deadline time.Time) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if delay := time.Until(deadline); delay > 0 {
		p.timer.Reset(delay)
		select {
		case <-p.timer.C:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return max(0, time.Since(deadline)), ctx.Err()
}

func (f *metadataPipeline) append(ctx context.Context, first, last int) error {
	c := f.config
	group, ctx := errgroup.WithContext(ctx)
	writers := c.Writers
	if first == 0 && last == 0 && c.Case != "cold" {
		// Seed in ID order so warm cases start with identical WAL refs and
		// shard assignments. Measured sweeps still use concurrent producers.
		writers = 1
	}
	var paceStart time.Time
	if (c.SweepInterval > 0 || c.SamplesPerSecond > 0) && first > 0 {
		paceStart = time.Now()
	}
	for writer := range writers {
		group.Go(func() error {
			var pacer *metadataPipelinePacer
			if !paceStart.IsZero() {
				pacer = &metadataPipelinePacer{
					start:    paceStart.Add(time.Duration(writer) * c.SweepInterval / time.Duration(writers)),
					interval: c.SweepInterval,
					timer:    time.NewTimer(time.Hour),
				}
				pacer.timer.Stop()
				defer pacer.timer.Stop()
			}
			begin, end := c.Series*writer/writers, c.Series*(writer+1)/writers
			for step := first; step <= last; step++ {
				if pacer != nil && c.SweepInterval > 0 {
					late, err := pacer.wait(ctx, step-first)
					if err != nil {
						return err
					}
					f.lateness[writer] = append(f.lateness[writer], late)
				}
				for offset := begin; offset < end; offset += c.CommitSize {
					if pacer != nil && c.SamplesPerSecond > 0 {
						scheduled := (step-first)*(end-begin) + offset - begin
						late, err := pacer.waitSamples(ctx, scheduled, writer, writers, c.CommitSize, c.SamplesPerSecond)
						if err != nil {
							return err
						}
						f.lateness[writer] = append(f.lateness[writer], late)
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					start := time.Now()
					app := f.db.AppenderV2(ctx)
					for slot := offset; slot < min(offset+c.CommitSize, end); slot++ {
						generation := c.generation(slot, step)
						id := slot + generation*c.Series
						if generation > 0 && step == c.firstStep(id) {
							f.refs[slot] = 0
						}
						timestamp, value := c.Base+int64(step)*15000, float64(step+1)
						var h *histogram.Histogram
						var fh *histogram.FloatHistogram
						if c.Mixed {
							switch slot % 3 {
							case 1:
								h = &histogram.Histogram{Count: uint64(step + 1), ZeroCount: uint64(step + 1), Sum: value}
							case 2:
								fh = &histogram.FloatHistogram{Count: value, ZeroCount: value, Sum: value}
							}
						}
						ref, err := app.Append(f.refs[slot], f.labels[id], 0, timestamp, value, h, fh, storage.AOptions{Metadata: f.metadata[slot][c.version(slot, step)]})
						if err == nil && c.Mixed && slot%3 == 0 {
							_, err = app.AppendExemplars(ref, labels.EmptyLabels(), []exemplar.Exemplar{{Labels: labels.FromStrings("trace_id", "trace"), Ts: timestamp, HasTs: true, Value: value}})
						}
						if err != nil {
							return errors.Join(err, app.Rollback())
						}
						f.refs[slot] = ref
					}
					if err := app.Commit(); err != nil {
						return err
					}
					f.latency[writer] = append(f.latency[writer], time.Since(start))
					f.peak[writer] = max(f.peak[writer], f.pending())
				}
			}
			return nil
		})
	}
	return group.Wait()
}

func (f *metadataPipeline) expectedItems(sweeps int) int64 {
	perSweep := f.config.Series
	if f.config.Mixed {
		perSweep += (f.config.Series + 2) / 3
	}
	return int64(perSweep) * int64(sweeps)
}

func (f *metadataPipeline) drain(ctx context.Context, expected int64) (metadataPipelineReceiverStats, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		stats, err := f.receiver.command(ctx, "stats")
		if err != nil {
			return stats, err
		}
		if stats.items() > expected {
			return stats, fmt.Errorf("received %d items, expected %d", stats.items(), expected)
		}
		if stats.items() == expected && f.pending() == 0 {
			return stats, nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return stats, fmt.Errorf("draining %d/%d items, pending %d: %w", stats.items(), expected, f.pending(), ctx.Err())
		}
	}
}

func (f *metadataPipeline) metrics() (map[string]float64, error) {
	gatherers := prometheus.Gatherers{f.registry}
	if f.sender == nil {
		gatherers = append(gatherers, f.observer)
	}
	families, err := gatherers.Gather()
	if err != nil {
		return nil, err
	}
	values := make(map[string]float64, len(families))
	for _, family := range families {
		for _, metric := range family.Metric {
			values[family.GetName()] += metric.GetCounter().GetValue() + metric.GetGauge().GetValue()
		}
	}
	return values, nil
}

func (f *metadataPipeline) closeSender() error {
	if f.sender == nil {
		return nil
	}
	f.db.SetWriteNotified(nil)
	err := f.sender.Close()
	f.sender = nil
	return err
}

func (f *metadataPipeline) close() error {
	var errs []error
	if f.receiver != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := f.receiver.command(ctx, "release")
		cancel()
		errs = append(errs, err)
	}
	errs = append(errs, f.closeSender())
	if f.db != nil {
		errs = append(errs, f.db.Close())
		f.db = nil
	}
	if f.receiver != nil {
		errs = append(errs, f.receiver.close())
		f.receiver = nil
	}
	return errors.Join(errs...)
}

// metadataPipelineCPUUsage excludes child processes. Unsupported platforms omit
// these metrics rather than substituting wall time for CPU time.
type metadataPipelineCPUUsage struct {
	User, System time.Duration
	Available    bool
}

func (u metadataPipelineCPUUsage) sub(before metadataPipelineCPUUsage) metadataPipelineCPUUsage {
	return metadataPipelineCPUUsage{User: u.User - before.User, System: u.System - before.System, Available: u.Available && before.Available}
}

func TestRemoteWriteMetadataPipeline(t *testing.T) {
	for _, mode := range []string{"disabled", "wal", "native"} {
		for _, workload := range []string{"cold", "unchanged", "changes", "changes-distinct", "paced-unchanged", "paced-changes", "newseries", "backlog", "mixed", "scale-unchanged", "scale-distinct", "scale-changes", "scale-changes-distinct", "scale-backlog", "scale-backlog-distinct"} {
			t.Run("source="+mode+"/case="+workload, func(t *testing.T) {
				c := metadataPipelineConfig{Source: mode, Case: workload, Series: 300, Values: 100, Sweeps: 4, Writers: 4, Shards: 4, Batch: 20, Capacity: 100, CommitSize: 50, ReceiverProcs: 2, Base: time.Now().Add(time.Hour).UnixMilli(), Mixed: workload == "mixed"}
				if scaleCase, ok := strings.CutPrefix(workload, "scale-"); ok {
					c.Group, c.Case = "history", scaleCase
					if strings.Contains(c.Case, "distinct") {
						c.Values = c.Series
					}
					if c.Case == "backlog-distinct" {
						c.Case = "backlog"
					}
					if c.Case == "backlog" {
						c.Group = "backlog"
					} else {
						c.SamplesPerSecond = 100000
					}
				}
				if c.Case == "backlog" {
					c.Writers, c.Shards = 1, 1
				}
				if c.Case == "changes" || c.Case == "changes-distinct" || c.Case == "paced-changes" || c.Case == "newseries" {
					c.Sweeps = 101
				}
				if workload == "changes-distinct" {
					c.Values = c.Series
				}
				if strings.HasPrefix(workload, "paced-") {
					c.SweepInterval = time.Millisecond
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				f, err := newMetadataPipeline(ctx, c)
				require.NoError(t, err)
				defer func() { require.NoError(t, f.close()) }()
				require.NoError(t, f.open(t.TempDir()))
				require.NoError(t, f.append(ctx, 0, 0))
				_, err = f.drain(ctx, f.expectedItems(1))
				require.NoError(t, err)
				if c.Group != "" {
					for w := range f.latency {
						f.latency[w] = f.latency[w][:0]
					}
				}
				if workload != "cold" {
					if c.Case == "backlog" {
						require.NoError(t, f.holdReceiver(ctx))
					}
					require.NoError(t, f.append(ctx, 1, c.Sweeps))
					if c.Case == "backlog" {
						require.NoError(t, f.awaitBacklog(ctx))
						require.Less(t, f.pending(), int64(c.Series*c.Sweeps), "must leave unread WAL work")
						_, err = f.receiver.command(ctx, "release")
						require.NoError(t, err)
					}
					_, err = f.drain(ctx, f.expectedItems(c.Sweeps+1))
					require.NoError(t, err)
					if c.SamplesPerSecond > 0 {
						for w := range c.Writers {
							transactions := (c.Series/c.Writers + c.CommitSize - 1) / c.CommitSize * c.Sweeps
							require.Len(t, f.latency[w], transactions)
							capacity := transactions
							if w == 0 {
								capacity = max(capacity, (c.Series+c.CommitSize-1)/c.CommitSize)
							}
							require.Equal(t, capacity, cap(f.latency[w]), "latency buffer must not grow")
							require.Len(t, f.lateness[w], transactions)
							require.Equal(t, transactions, cap(f.lateness[w]), "timing buffer must not grow")
						}
					}
				}
				require.Equal(t, uint64(c.residentSeries()), f.db.Head().NumSeries())
				metrics, err := f.metrics()
				require.NoError(t, err)
				require.Zero(t, metrics["prometheus_tsdb_head_native_metric_metadata_version_evictions_total"])
				for name, value := range metrics {
					if strings.HasPrefix(name, "prometheus_remote_storage_") && (strings.HasSuffix(name, "_failed_total") || strings.HasSuffix(name, "_dropped_total") || strings.HasSuffix(name, "_retried_total")) {
						require.Zero(t, value, name)
					}
				}
				require.NoError(t, f.closeSender())
				cancelled, stop := context.WithCancel(ctx)
				stop()
				require.ErrorIs(t, f.append(cancelled, 1, 1), context.Canceled)
			})
		}
	}
	t.Run("failure cleanup", func(t *testing.T) {
		for _, scenario := range []string{"invalid delivery", "cancellation while blocked"} {
			t.Run(scenario, func(t *testing.T) {
				c := metadataPipelineConfig{Source: "native", Case: "backlog", Series: 300, Values: 100, Sweeps: 4, Writers: 1, Shards: 1, Batch: 20, Capacity: 100, CommitSize: 50, ReceiverProcs: 2, Base: time.Now().Add(time.Hour).UnixMilli()}
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				f, err := newMetadataPipeline(ctx, c)
				require.NoError(t, err)
				defer func() { _ = f.close() }()
				require.NoError(t, f.open(t.TempDir()))
				require.NoError(t, f.append(ctx, 0, 0))
				_, err = f.drain(ctx, f.expectedItems(1))
				require.NoError(t, err)
				if scenario == "invalid delivery" {
					client := &http.Client{Timeout: time.Second}
					response, err := client.Post(f.receiver.address, "application/x-protobuf", strings.NewReader("invalid snappy"))
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					require.Equal(t, http.StatusBadRequest, response.StatusCode)
					_, err = f.drain(ctx, f.expectedItems(1))
					require.Error(t, err)
				} else {
					require.NoError(t, f.holdReceiver(ctx))
					require.NoError(t, f.append(ctx, 1, c.Sweeps))
					require.NoError(t, f.awaitBacklog(ctx))
					cancel()
				}
				// close joins the subprocess even when it reports an error or has
				// been killed, and closes the sender before removing the TSDB.
				require.Error(t, f.close())
				require.Nil(t, f.receiver)
				require.Nil(t, f.sender)
				require.Nil(t, f.db)
			})
		}
	})
	t.Run("closed control stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		c := metadataPipelineConfig{Case: "cold", Source: "native", Series: 100, Values: 100, ReceiverProcs: 2}
		p, err := startMetadataPipelineReceiver(ctx, c)
		require.NoError(t, err)
		defer p.stdin.Close()
		p.cancel()
		select {
		case <-p.done:
		case <-ctx.Done():
			t.Fatal("receiver did not exit")
		}
		// Both normal operation and cleanup may observe the closed stream.
		for range 2 {
			_, err := p.receive(ctx)
			require.Error(t, err)
		}
	})
}

func TestRemoteWriteMetadataPipelineValidation(t *testing.T) {
	c := metadataPipelineConfig{Source: "native", Case: "unchanged", Series: 1, Values: 1, Sweeps: 2, Base: 1000}
	for _, scenario := range []string{"valid", "duplicate", "missing", "metadata", "labels"} {
		t.Run(scenario, func(t *testing.T) {
			r := &metadataPipelineReceiver{config: c, series: make([]metadataPipelineReceivedSeries, 1)}
			r.labels, r.metadata = metadataPipelineInputs(c)
			symbols := writev2.NewSymbolTable()
			md := c.metadata(0, 0)
			lset := metadataPipelineLabels(0)
			if scenario == "labels" {
				lset = labels.FromStrings("id", "0")
			}
			var refs []uint32
			lset.Range(func(l labels.Label) { refs = append(refs, symbols.Symbolize(l.Name), symbols.Symbolize(l.Value)) })
			ts := writev2.TimeSeries{LabelsRefs: refs, Samples: []writev2.Sample{{Timestamp: c.Base, Value: 1}}, Metadata: writev2.Metadata{Type: writev2.FromMetadataType(md.Type), HelpRef: symbols.Symbolize(md.Help), UnitRef: symbols.Symbolize(md.Unit)}}
			if scenario == "missing" {
				ts.Samples[0] = writev2.Sample{Timestamp: c.Base + 15000, Value: 2}
			}
			if scenario == "metadata" {
				ts.Metadata.HelpRef = 0
			}
			req := &writev2.Request{Symbols: symbols.Symbols(), Timeseries: []writev2.TimeSeries{ts}}
			if scenario == "duplicate" {
				_, _, _, err := r.validate(req)
				require.NoError(t, err)
			}
			_, _, _, err := r.validate(req)
			if scenario == "valid" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestMetadataPipelinePacer(t *testing.T) {
	t.Run("transaction deadlines", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := metadataPipelinePacer{start: time.Now(), timer: time.NewTimer(time.Hour)}
			p.timer.Stop()
			defer p.timer.Stop()
			// Four writers offer 500K samples/s, with 500-sample transactions.
			for _, point := range []struct {
				samples, writer int
				deadline        time.Duration
			}{{0, 0, 0}, {0, 1, time.Millisecond}, {0, 2, 2 * time.Millisecond}, {0, 3, 3 * time.Millisecond}, {500, 0, 4 * time.Millisecond}, {750, 0, 6 * time.Millisecond}, {1000, 0, 8 * time.Millisecond}} {
				late, err := p.waitSamples(t.Context(), point.samples, point.writer, 4, 500, 500000)
				require.NoError(t, err)
				require.Zero(t, late)
				require.Equal(t, p.start.Add(point.deadline), time.Now())
			}
			time.Sleep(10 * time.Millisecond)
			late, err := p.waitSamples(t.Context(), 1500, 0, 4, 500, 500000)
			require.NoError(t, err)
			require.Equal(t, 6*time.Millisecond, late)
			late, err = p.waitSamples(t.Context(), 2500, 0, 4, 500, 500000)
			require.NoError(t, err)
			require.Zero(t, late)
			require.Equal(t, p.start.Add(20*time.Millisecond), time.Now())
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				_, err := p.waitSamples(ctx, 3000, 0, 4, 500, 500000)
				done <- err
			}()
			synctest.Wait()
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
			_, err = p.waitSamples(ctx, 0, 0, 4, 500, 500000)
			require.ErrorIs(t, err, context.Canceled)
		})
	})
	t.Run("invalid transaction pacing", func(t *testing.T) {
		for _, c := range []metadataPipelineConfig{
			{Series: 100, Writers: 4, CommitSize: 10, SamplesPerSecond: 500000, SweepInterval: time.Second},
			{Series: 101, Writers: 4, CommitSize: 10, SamplesPerSecond: 500000},
			{Series: 100, Writers: 4, CommitSize: 10, SamplesPerSecond: -1},
		} {
			_, err := newMetadataPipeline(t.Context(), c)
			require.Error(t, err)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		p := metadataPipelinePacer{start: time.Now().Add(5 * time.Millisecond), interval: 20 * time.Millisecond, timer: time.NewTimer(time.Hour)}
		p.timer.Stop()
		defer p.timer.Stop()
		for _, step := range []int{0, 1} {
			late, err := p.wait(t.Context(), step)
			require.NoError(t, err)
			require.Zero(t, late)
			require.Equal(t, p.start.Add(time.Duration(step)*p.interval), time.Now())
		}
		time.Sleep(50 * time.Millisecond)
		late, err := p.wait(t.Context(), 2)
		require.NoError(t, err)
		require.Equal(t, 30*time.Millisecond, late)
		late, err = p.wait(t.Context(), 3)
		require.NoError(t, err)
		require.Equal(t, 10*time.Millisecond, late)
		late, err = p.wait(t.Context(), 4)
		require.NoError(t, err)
		require.Zero(t, late)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := p.wait(ctx, 5)
			done <- err
		}()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		_, err = p.wait(ctx, 0)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func metadataPipelineRetainedHeap(f *metadataPipeline) uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	runtime.KeepAlive(f)
	return stats.HeapAlloc
}

func metadataPipelinePercentile(values []time.Duration, percentile int) float64 {
	slices.Sort(values)
	if len(values) == 0 {
		return 0
	}
	return float64(values[(len(values)-1)*percentile/100])
}
