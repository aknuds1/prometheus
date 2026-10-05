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
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	runtimemetrics "runtime/metrics"
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
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/wlog"
	"github.com/prometheus/prometheus/util/compression"
)

const metadataPipelineReceiverEnv = "PROMETHEUS_METADATA_PIPELINE_RECEIVER"

// metadataPipelineConfig describes a finite trace. Step zero seeds the warm
// cases; cold initialization measures only step zero. IDs survive WAL ref changes.
// StepsPerCommit batches consecutive steps of each series into one transaction.
// The restart case checkpoints the WAL after RestartStep, then restarts storage
// and sender before the remaining steps. HelpBytes lengthens help strings beyond
// the default 64 bytes. Endpoints adds remote-write queues, each with its own
// receiver. In a held backlog with two endpoints, ReleaseLag releases the second
// receiver only once the first has acknowledged that many samples since the hold.
type metadataPipelineConfig struct {
	Group                            string
	Case, Source                     string
	Series, Values, Sweeps           int
	Writers, Shards, Batch, Capacity int
	CommitSize, ReceiverProcs        int
	StepsPerCommit, RestartStep      int
	WALSegmentSize                   int
	HelpBytes, Endpoints, ReleaseLag int
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
	case "restart":
		return min(step, metadataPipelineRestartHistory)
	case "changes", "changes-distinct", "paced-changes", "batched":
		if step > slot%100 {
			return 1 + (step-1-slot%100)/100
		}
	}
	return 0
}

// writers returns the number of concurrent producers appending steps first
// through last. The seed, and a checkpointed prefix, use one producer in ID
// order, so that WAL refs, shard assignments and segment cuts are identical
// across runs.
func (c metadataPipelineConfig) writers(first, last int) int {
	if (first == 0 && last == 0 && c.Case != "cold") || (c.RestartStep > 0 && last <= c.RestartStep) {
		return 1
	}
	return c.Writers
}

// metadataPipelineBase is the benchmarks' logical time of step zero,
// 2100-01-01T00:00:00Z. It is later than any run, so the WAL watcher, which
// skips samples older than its own start, sends every sample. It also starts a
// chunk range, so where a trace's chunks are cut never depends on when it runs.
const metadataPipelineBase int64 = 4102444800000

// timestamp returns the logical timestamp of step.
func (c metadataPipelineConfig) timestamp(step int) int64 {
	return c.Base + int64(step)*15000
}

// lastStepInCommit returns the final step of the transaction containing step.
// Steps after the seed are batched from step one.
func (c metadataPipelineConfig) lastStepInCommit(step int) int {
	if c.StepsPerCommit <= 1 || step == 0 {
		return step
	}
	return min((step-1)/c.StepsPerCommit*c.StepsPerCommit+c.StepsPerCommit, c.lastStep())
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
	return metadata.Metadata{Type: typ, Unit: "seconds", Help: prefix + strings.Repeat("x", max(64, c.HelpBytes)-len(prefix))}
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
	Memory                                      *metadataPipelineMemory `json:",omitempty"`
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
			switch r.config.Source {
			case "disabled":
				want = metadata.Metadata{Type: model.MetricTypeUnknown}
			case "wal":
				// Legacy records precede a transaction's samples and carry only the
				// latest value, so each sample gets its transaction's final version.
				want = r.metadata[slot][r.config.version(slot, r.config.lastStepInCommit(step))]
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
		case "stats", "accounting":
		default:
			r.stats.Error = "unknown control command: " + command
		}
		stats := r.stats
		r.mu.Unlock()
		var err error
		stats.CPU, err = metadataPipelineCPU()
		require.NoError(t, err)
		if command == "accounting" {
			memory := metadataPipelineReadMemory()
			stats.Memory = &memory
		}
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
	extraReceivers           []*metadataPipelineReceiverProcess
	registry                 *prometheus.Registry
	observer                 *prometheus.Registry
	db                       *tsdb.DB
	sender                   *Storage
	queue                    *QueueManager
	extraQueues              []*QueueManager
	latency                  [][]time.Duration
	peak                     []int64
	lateness                 [][]time.Duration
	enqueueRetriesBeforeHold []float64
	heldItems                []int64
	diagnostics              *metadataPipelineDiagnostics
}

func newMetadataPipeline(ctx context.Context, c metadataPipelineConfig) (*metadataPipeline, error) {
	if c.Series <= 0 || c.Writers <= 0 || c.CommitSize <= 0 {
		return nil, errors.New("series, writers, and commit size must be positive")
	}
	if c.SamplesPerSecond < 0 || (c.SamplesPerSecond > 0 && (c.SweepInterval != 0 || c.Series%c.Writers != 0)) {
		return nil, errors.New("transaction pacing requires equal writer partitions and no sweep pacing")
	}
	if c.StepsPerCommit < 0 || (c.StepsPerCommit > 1 && (c.SweepInterval != 0 || c.SamplesPerSecond != 0 || c.Group != "")) {
		return nil, errors.New("batched transactions must be unpaced and ungrouped")
	}
	if (c.Case == "restart") != (c.RestartStep > 0) || (c.RestartStep > 0 && (c.Group != "" || c.RestartStep <= metadataPipelineRestartHistory || c.RestartStep >= c.lastStep())) {
		return nil, errors.New("restarts require an ungrouped restart trace with steps after the history and after the restart")
	}
	if c.Endpoints < 0 || (c.Endpoints > 1 && c.RestartStep > 0) {
		return nil, errors.New("additional endpoints are supported only without restarts")
	}
	if c.ReleaseLag < 0 || (c.ReleaseLag > 0 && (c.Case != "backlog" || c.Endpoints != 2 || int64(c.ReleaseLag)+int64(c.Capacity*c.Shards) > int64(c.Series)*int64(c.lastStep()))) {
		return nil, errors.New("a release lag requires a held backlog with two endpoints, and must leave a queue's capacity of the trace")
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
	for range max(1, c.Endpoints) - 1 {
		if err != nil {
			break
		}
		var receiver *metadataPipelineReceiverProcess
		receiver, err = startMetadataPipelineReceiver(ctx, c)
		if receiver != nil {
			f.extraReceivers = append(f.extraReceivers, receiver)
		}
	}
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
	if err := f.openDB(dir); err != nil {
		return err
	}
	return f.openSender(dir)
}

func (f *metadataPipeline) openDB(dir string) error {
	c := f.config
	opts := tsdb.DefaultOptions()
	opts.EnableNativeMetadata = c.Source == "native"
	opts.EnableMetadataWALRecords = c.Source == "wal"
	opts.WALCompression = compression.Snappy
	opts.WALSegmentSize = c.WALSegmentSize
	opts.EnableExemplarStorage, opts.MaxExemplars = c.Mixed, int64(c.Series*2)
	var err error
	f.db, err = tsdb.Open(dir, nil, f.registry, opts, nil)
	if err != nil {
		return err
	}
	f.db.DisableCompactions()
	return nil
}

func (f *metadataPipeline) openSender(dir string) error {
	c := f.config
	var reader storage.NativeMetricMetadataReader
	if c.Source == "native" {
		reader = f.db
	}
	var reg prometheus.Registerer = f.registry
	if f.diagnostics != nil && f.diagnostics.Accounting {
		reg = &metadataPipelineDiagnosticRegisterer{Registerer: reg, watcher: f.diagnostics.watcher}
	}
	f.sender = NewStorage(nil, reg, f.db.StartTime, dir, 5*time.Second, nil, false, reader)
	rw := baseRemoteWriteConfig(f.receiver.address)
	rw.ProtobufMessage = remoteapi.WriteV2MessageType
	rw.SendExemplars, rw.SendNativeHistograms = c.Mixed, c.Mixed
	rw.MetadataConfig.Send = false
	rw.QueueConfig.MinShards, rw.QueueConfig.MaxShards = c.Shards, c.Shards
	rw.QueueConfig.MaxSamplesPerSend, rw.QueueConfig.Capacity = c.Batch, c.Capacity
	rw.QueueConfig.BatchSendDeadline = model.Duration(100 * time.Millisecond)
	if f.diagnostics != nil && f.diagnostics.BackoffCap != 0 {
		rw.QueueConfig.MinBackoff = model.Duration(f.diagnostics.BackoffCap)
		rw.QueueConfig.MaxBackoff = model.Duration(f.diagnostics.BackoffCap)
	}
	configs := []*config.RemoteWriteConfig{rw}
	for _, receiver := range f.extraReceivers {
		extra := *rw
		extra.URL = baseRemoteWriteConfig(receiver.address).URL
		configs = append(configs, &extra)
	}
	if err := f.sender.ApplyConfig(&config.Config{RemoteWriteConfigs: configs}); err != nil {
		return err
	}
	// Configuration is now fixed. Only observe counters; never lock queue internals
	// to control when the watcher reads metadata or samples.
	f.queue, f.extraQueues = nil, nil
	for _, queue := range f.sender.rws.queues {
		if queue.storeClient.Endpoint() == rw.URL.String() {
			f.queue = queue
		} else {
			f.extraQueues = append(f.extraQueues, queue)
		}
	}
	if f.queue == nil || len(f.extraQueues) != len(f.extraReceivers) {
		return errors.New("remote-write queues do not match the receivers")
	}
	// Retain collectors after sender shutdown so validation and metric collection
	// need not run inside the measured interval.
	for _, queue := range append([]*QueueManager{f.queue}, f.extraQueues...) {
		m := queue.metrics
		f.observer.MustRegister(m.failedSamplesTotal, m.failedHistogramsTotal, m.failedExemplarsTotal,
			m.retriedSamplesTotal, m.retriedHistogramsTotal, m.retriedExemplarsTotal,
			m.droppedSamplesTotal, m.droppedHistogramsTotal, m.droppedExemplarsTotal)
	}
	f.db.SetWriteNotified(f.sender)
	return nil
}

func (f *metadataPipeline) receivers() []*metadataPipelineReceiverProcess {
	return append([]*metadataPipelineReceiverProcess{f.receiver}, f.extraReceivers...)
}

func (f *metadataPipeline) queues() []*QueueManager {
	return append([]*QueueManager{f.queue}, f.extraQueues...)
}

func (f *metadataPipeline) pending() int64 {
	var pending int64
	for _, queue := range f.queues() {
		pending += metadataPipelineQueuePending(queue)
	}
	return pending
}

func metadataPipelineQueuePending(queue *QueueManager) int64 {
	return queue.shards.enqueuedSamples.Load() + queue.shards.enqueuedHistograms.Load() + queue.shards.enqueuedExemplars.Load()
}

// holdReceivers withholds every receiver's acknowledgements. A receiver's
// acknowledged items at its hold are its baseline: the hold follows the seed,
// whose deliveries the cumulative counts include.
func (f *metadataPipeline) holdReceivers(ctx context.Context) error {
	f.enqueueRetriesBeforeHold, f.heldItems = f.enqueueRetriesBeforeHold[:0], f.heldItems[:0]
	for _, queue := range f.queues() {
		f.enqueueRetriesBeforeHold = append(f.enqueueRetriesBeforeHold, testutil.ToFloat64(queue.metrics.enqueueRetriesTotal))
	}
	for _, receiver := range f.receivers() {
		stats, err := receiver.command(ctx, "hold")
		if err != nil {
			return err
		}
		f.heldItems = append(f.heldItems, stats.items())
	}
	return nil
}

// awaitBacklog waits until every receiver holds a request and every queue has
// retried an enqueue: merely withholding an acknowledgement need not block a WAL
// reader. Each queue must then leave more of the trace unread than one decoded
// record and a margin, so that samples are selected from history, not just
// delayed.
func (f *metadataPipeline) awaitBacklog(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		backlogged := true
		for i, receiver := range f.receivers() {
			stats, err := receiver.command(ctx, "stats")
			if err != nil {
				return err
			}
			backlogged = backlogged && stats.HeldRequests > 0 && testutil.ToFloat64(f.queues()[i].metrics.enqueueRetriesTotal) > f.enqueueRetriesBeforeHold[i]
		}
		if backlogged {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	samples := int64(f.config.Series) * int64(f.config.lastStep())
	for i, queue := range f.queues() {
		if unread := samples - metadataPipelineQueuePending(queue); unread <= int64(f.config.CommitSize+256) {
			return fmt.Errorf("queue %d leaves only %d of %d samples unread", i, unread, samples)
		}
	}
	return nil
}

// metadataPipelineRelease records the release of a held backlog with several
// endpoints. Deltas are acknowledged items since a receiver's hold; they are
// counted in delivered samples, not in WAL read positions.
type metadataPipelineRelease struct {
	Lag int
	// Baselines are each receiver's acknowledged items at its hold.
	Baselines []int64
	// FirstDelta is the first receiver's delta when the others are released,
	// and Overshoot is how far it exceeds Lag.
	FirstDelta, Overshoot int64
	// LaterDeltas are the other receivers' deltas at their release, which
	// must be zero.
	LaterDeltas []int64
}

// metadataPipelineReleaseDue reports whether a receiver that had acknowledged
// baseline items at its hold has acknowledged at least lag more since.
func metadataPipelineReleaseDue(baseline, current int64, lag int) bool {
	return current-baseline >= int64(lag)
}

// release releases the first receiver, then the others once the first has
// acknowledged the configured lag since its hold. It returns nil for a single
// endpoint.
func (f *metadataPipeline) release(ctx context.Context) (*metadataPipelineRelease, error) {
	// The reply's counts precede the release, so the lag starts from the hold.
	stats, err := f.receiver.command(ctx, "release")
	if err != nil || len(f.extraReceivers) == 0 {
		return nil, err
	}
	r := &metadataPipelineRelease{Lag: f.config.ReleaseLag, Baselines: slices.Clone(f.heldItems)}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !metadataPipelineReleaseDue(f.heldItems[0], stats.items(), r.Lag) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		if stats, err = f.receiver.command(ctx, "stats"); err != nil {
			return nil, err
		}
	}
	r.FirstDelta = stats.items() - f.heldItems[0]
	r.Overshoot = r.FirstDelta - int64(r.Lag)
	for i, receiver := range f.extraReceivers {
		stats, err := receiver.command(ctx, "release")
		if err != nil {
			return nil, err
		}
		delta := stats.items() - f.heldItems[i+1]
		if delta != 0 {
			return nil, fmt.Errorf("held receiver %d acknowledged %d items before its release", i+1, delta)
		}
		r.LaterDeltas = append(r.LaterDeltas, delta)
	}
	return r, nil
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
	writers := c.writers(first, last)
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
			for batch := first; batch <= last; batch += max(1, c.StepsPerCommit) {
				batchEnd := min(c.lastStepInCommit(batch), last)
				if pacer != nil && c.SweepInterval > 0 {
					late, err := pacer.wait(ctx, batch-first)
					if err != nil {
						return err
					}
					f.lateness[writer] = append(f.lateness[writer], late)
				}
				for offset := begin; offset < end; offset += c.CommitSize {
					if pacer != nil && c.SamplesPerSecond > 0 {
						scheduled := (batch-first)*(end-begin) + offset - begin
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
						for step := batch; step <= batchEnd; step++ {
							generation := c.generation(slot, step)
							id := slot + generation*c.Series
							if generation > 0 && step == c.firstStep(id) {
								f.refs[slot] = 0
							}
							timestamp, value := c.timestamp(step), float64(step+1)
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
		if err := f.diagnostics.progress(f, stats); err != nil {
			return stats, err
		}
		if stats.items() > expected {
			return stats, fmt.Errorf("received %d items, expected %d", stats.items(), expected)
		}
		done := stats.items() == expected && f.pending() == 0
		for _, receiver := range f.extraReceivers {
			extra, err := receiver.command(ctx, "stats")
			if err != nil {
				return stats, err
			}
			if extra.items() > expected {
				return stats, fmt.Errorf("an additional receiver received %d items, expected %d", extra.items(), expected)
			}
			done = done && extra.items() == expected
		}
		if done {
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
			values[family.GetName()] += metric.GetCounter().GetValue() + metric.GetGauge().GetValue() + metric.GetSummary().GetSampleSum()
		}
	}
	return values, nil
}

// checkMetadataPipelineMetrics rejects lost forwarding work, history
// evictions, and metadata entries of unknown kind.
func checkMetadataPipelineMetrics(metrics map[string]float64) error {
	if v := metrics["prometheus_tsdb_head_native_metric_metadata_version_evictions_total"]; v != 0 {
		return fmt.Errorf("%g native metadata version evictions", v)
	}
	for name, value := range metrics {
		if strings.HasPrefix(name, "prometheus_remote_storage_") && (strings.HasSuffix(name, "_failed_total") || strings.HasSuffix(name, "_dropped_total") || strings.HasSuffix(name, "_retried_total")) && value != 0 {
			return fmt.Errorf("%s = %g", name, value)
		}
	}
	for name, value := range metadataPipelineUnknownCounters(metrics) {
		if value != 0 {
			return fmt.Errorf("%s = %g", name, value)
		}
	}
	return nil
}

// metadataPipelineUnknownCounters returns the counters of metadata entries of
// unknown kind. Builds without native WAL entries have none.
func metadataPipelineUnknownCounters(metrics map[string]float64) map[string]float64 {
	counters := map[string]float64{}
	for name, value := range metrics {
		if strings.HasSuffix(name, "_unknown_entries_total") || strings.HasSuffix(name, "_unknown_wal_entries_total") {
			counters[name] = value
		}
	}
	return counters
}

// metadataPipelineRestartHistory is the last step that changes metadata in the
// restart case, filling the five-version native history.
const metadataPipelineRestartHistory = 4

// metadataPipelineRestart describes one WAL checkpoint and restart. Heap fields
// are populated only in heap passes; the peak samples live and unswept objects.
type metadataPipelineRestart struct {
	Truncation, Replay                              time.Duration
	WALTruncationSeconds, ReplaySeconds             float64
	HistorySegment, Checkpoint                      int
	FirstSegment                                    int // The first segment written after the restart.
	CheckpointBytes                                 int64
	CheckpointPayloadBytes, PostRestartPayloadBytes map[string]int64
	TruncationAllocatedBytes, TruncationAllocations uint64
	TruncationBaseHeap, TruncationPeakHeap          uint64
	ReplayedHeap                                    uint64
	PreRestartWALBytes                              float64
	UnknownEntryCounters                            map[string]float64
	// Series refs by ID, from the checkpoint.
	refs map[int]chunks.HeadSeriesRef
}

// drainNotified drains while repeating write notifications. A watcher that moves
// to a new segment reads it only when notified, so a final segment rollover would
// otherwise wait for the watcher's 15s read timeout.
func (f *metadataPipeline) drainNotified(ctx context.Context, expected int64) (metadataPipelineReceiverStats, error) {
	sender, done := f.sender, make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				sender.Notify()
			}
		}
	})
	defer wg.Wait()
	defer close(done)
	return f.drain(ctx, expected)
}

// restart writes the steps up to RestartStep after the seed, and checkpoints
// the drained WAL with the sender running. The checkpoint must include the whole
// metadata history. Restart then reopens storage and sender with fresh
// registries; replay is the timed DB open. Series refs are forgotten, as by
// restarted producers. The new watcher skips samples in segments written before
// it started. Later segments have the default size, so measured sweeps never
// wait for the watcher to notice a rollover.
func (f *metadataPipeline) restart(ctx context.Context, dir string, heap bool) (metadataPipelineRestart, error) {
	var r metadataPipelineRestart
	if err := f.append(ctx, 1, metadataPipelineRestartHistory); err != nil {
		return r, err
	}
	_, history, err := wlog.Segments(filepath.Join(dir, "wal"))
	if err != nil {
		return r, err
	}
	r.HistorySegment = history
	if err := f.append(ctx, metadataPipelineRestartHistory+1, f.config.RestartStep); err != nil {
		return r, err
	}
	if _, err := f.drainNotified(ctx, f.expectedItems(f.config.RestartStep+1)); err != nil {
		return r, err
	}
	if heap {
		r.TruncationBaseHeap = metadataPipelineRetainedHeap(f)
	}
	var stop chan struct{}
	var peak chan uint64
	if heap {
		stop, peak = make(chan struct{}), make(chan uint64, 1)
		go func() { peak <- metadataPipelineSampleHeap(stop) }()
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	err = f.db.Head().Truncate(f.config.timestamp(f.config.RestartStep / 2))
	r.Truncation = time.Since(start)
	runtime.ReadMemStats(&after)
	if heap {
		close(stop)
		r.TruncationPeakHeap = <-peak
	}
	if err != nil {
		return r, err
	}
	r.TruncationAllocatedBytes, r.TruncationAllocations = after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs
	checkpoint, index, err := wlog.LastCheckpoint(filepath.Join(dir, "wal"))
	if err != nil {
		return r, fmt.Errorf("restart requires a checkpoint: %w", err)
	}
	r.Checkpoint = index
	if index < history {
		return r, fmt.Errorf("checkpoint %d ends before the history in segment %d", index, history)
	}
	if r.refs, err = checkMetadataPipelineCheckpoint(f.config, checkpoint); err != nil {
		return r, err
	}
	entries, err := os.ReadDir(checkpoint)
	if err != nil {
		return r, err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return r, err
		}
		r.CheckpointBytes += info.Size()
	}
	if r.CheckpointPayloadBytes, err = metadataPipelinePayloadBytes(checkpoint, 0); err != nil {
		return r, err
	}
	// Queue metrics, including unknown-entry counters, unregister at shutdown.
	live, err := f.metrics()
	if err != nil {
		return r, err
	}
	if err := checkMetadataPipelineMetrics(live); err != nil {
		return r, err
	}
	r.UnknownEntryCounters = metadataPipelineUnknownCounters(live)
	if err := f.closeSender(); err != nil {
		return r, err
	}
	metrics, err := f.metrics()
	if err != nil {
		return r, err
	}
	if err := checkMetadataPipelineMetrics(metrics); err != nil {
		return r, err
	}
	for _, name := range []string{"prometheus_tsdb_checkpoint_creations_failed_total", "prometheus_tsdb_wal_truncations_failed_total", "prometheus_tsdb_checkpoint_deletions_failed_total"} {
		if metrics[name] != 0 {
			return r, fmt.Errorf("%s = %g", name, metrics[name])
		}
	}
	if metrics["prometheus_tsdb_checkpoint_creations_total"] != 1 {
		return r, fmt.Errorf("created %g checkpoints, want 1", metrics["prometheus_tsdb_checkpoint_creations_total"])
	}
	r.WALTruncationSeconds = metrics["prometheus_tsdb_wal_truncate_duration_seconds"]
	r.PreRestartWALBytes = metrics["prometheus_tsdb_wal_record_parts_bytes_written_total"]
	err = f.db.Close()
	f.db = nil
	if err != nil {
		return r, err
	}
	// TSDB collectors are never unregistered; keep the retired sender's failure
	// counters checked above out of later checks.
	f.registry, f.observer = prometheus.NewRegistry(), prometheus.NewRegistry()
	f.config.WALSegmentSize = 0
	start = time.Now()
	if err := f.openDB(dir); err != nil {
		return r, err
	}
	r.Replay = time.Since(start)
	// Opening storage starts a new segment.
	if _, r.FirstSegment, err = wlog.Segments(filepath.Join(dir, "wal")); err != nil {
		return r, err
	}
	if metrics, err = f.metrics(); err != nil {
		return r, err
	}
	r.ReplaySeconds = metrics["prometheus_tsdb_data_replay_duration_seconds"]
	if heap {
		r.ReplayedHeap = metadataPipelineRetainedHeap(f)
	}
	clear(f.refs)
	return r, f.openSender(dir)
}

// metadataPipelinePayloadBytes sums decompressed record bytes by record type
// over the segments in dir from index first, excluding checkpoints. Page
// padding and compression make file sizes too coarse to compare record
// encodings.
func metadataPipelinePayloadBytes(dir string, first int) (map[string]int64, error) {
	segments, err := wlog.NewSegmentsRangeReader(wlog.SegmentRange{Dir: dir, First: first, Last: math.MaxInt32})
	if err != nil {
		return nil, err
	}
	defer segments.Close()
	var dec record.Decoder
	payloads := map[string]int64{}
	reader := wlog.NewReader(segments)
	for reader.Next() {
		rec := reader.Record()
		payloads[dec.Type(rec).String()] += int64(len(rec))
	}
	return payloads, reader.Err()
}

// metadataPipelineSampleHeap returns the peak heap object bytes observed until
// stop is closed. Unlike MemStats, reading these runtime metrics does not stop
// the world.
func metadataPipelineSampleHeap(stop <-chan struct{}) uint64 {
	samples := []runtimemetrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var peak uint64
	for {
		runtimemetrics.Read(samples)
		peak = max(peak, samples[0].Value.Uint64())
		select {
		case <-stop:
			runtimemetrics.Read(samples)
			return max(peak, samples[0].Value.Uint64())
		case <-ticker.C:
		}
	}
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
	for _, receiver := range f.extraReceivers {
		errs = append(errs, receiver.close())
	}
	f.extraReceivers = nil
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
		for _, workload := range []string{"cold", "unchanged", "changes", "changes-distinct", "paced-unchanged", "paced-changes", "newseries", "backlog", "batched", "restart", "mixed", "scale-unchanged", "scale-distinct", "scale-changes", "scale-changes-distinct", "scale-backlog", "scale-backlog-distinct"} {
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
				if c.Case == "changes" || c.Case == "changes-distinct" || c.Case == "paced-changes" || c.Case == "newseries" || c.Case == "batched" {
					c.Sweeps = 101
				}
				switch workload {
				case "batched":
					// Keep transactions at 50 samples, as in the unbatched cases.
					c.CommitSize, c.StepsPerCommit = 5, 10
				case "restart":
					// About nine minimal segments precede the checkpoint.
					c.Sweeps, c.RestartStep, c.WALSegmentSize = 200, 180, 32<<10
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
				dir := t.TempDir()
				require.NoError(t, f.open(dir))
				require.NoError(t, f.append(ctx, 0, 0))
				_, err = f.drain(ctx, f.expectedItems(1))
				require.NoError(t, err)
				if c.Group != "" {
					for w := range f.latency {
						f.latency[w] = f.latency[w][:0]
					}
				}
				var restart *metadataPipelineRestart
				if workload != "cold" {
					first := 1
					if c.RestartStep > 0 {
						r, err := f.restart(ctx, dir, true)
						restart = &r
						require.NoError(t, err)
						require.Positive(t, restart.Checkpoint)
						require.Positive(t, restart.CheckpointBytes)
						require.Positive(t, restart.CheckpointPayloadBytes["series"])
						require.Positive(t, restart.TruncationPeakHeap)
						require.Positive(t, restart.ReplayedHeap)
						first = c.RestartStep + 1
					}
					if c.Case == "backlog" {
						require.NoError(t, f.holdReceivers(ctx))
					}
					require.NoError(t, f.append(ctx, first, c.Sweeps))
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
				require.NoError(t, checkMetadataPipelineMetrics(metrics))
				require.NoError(t, f.closeSender())
				require.NoError(t, checkMetadataPipelineWAL(c, filepath.Join(dir, "wal"), restart))
				cancelled, stop := context.WithCancel(ctx)
				stop()
				require.ErrorIs(t, f.append(cancelled, 1, 1), context.Canceled)
			})
		}
	}
	t.Run("two endpoints with a held backlog", func(t *testing.T) {
		for _, mode := range []string{"disabled", "wal", "native"} {
			for _, lag := range []int{0, 10000} {
				t.Run(fmt.Sprintf("source=%s/lag=%d", mode, lag), func(t *testing.T) {
					// The distinct backlog of the scale benchmark. Each receiver
					// acknowledges the 10,000 seed samples before the hold, so a lag
					// counted from zero would release the second at once.
					c := metadataPipelineConfig{Group: "backlog", Case: "backlog", Source: mode, Series: 10000, Values: 10000, Sweeps: 4, Writers: 1, Shards: 1, Batch: 2000, Capacity: 10000, CommitSize: 1000, ReceiverProcs: 2, Endpoints: 2, ReleaseLag: lag, Base: time.Now().Add(time.Hour).UnixMilli()}
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
					require.NoError(t, f.holdReceivers(ctx))
					require.NoError(t, f.append(ctx, 1, c.Sweeps))
					require.NoError(t, f.awaitBacklog(ctx))
					release, err := f.release(ctx)
					require.NoError(t, err)
					require.Equal(t, []int64{f.expectedItems(1), f.expectedItems(1)}, release.Baselines)
					require.GreaterOrEqual(t, release.FirstDelta, int64(lag))
					require.Equal(t, release.FirstDelta-int64(lag), release.Overshoot)
					require.Equal(t, []int64{0}, release.LaterDeltas)
					_, err = f.drain(ctx, f.expectedItems(c.Sweeps+1))
					require.NoError(t, err)
					metrics, err := f.metrics()
					require.NoError(t, err)
					require.NoError(t, checkMetadataPipelineMetrics(metrics))
					require.NoError(t, f.closeSender())
					require.NoError(t, checkMetadataPipelineWAL(c, filepath.Join(dir, "wal"), nil))
				})
			}
		}
	})
	t.Run("fixed sample clock", func(t *testing.T) {
		// The head cuts a series' chunks at chunk-range boundaries as well as by
		// sample count, so a trace that straddles a boundary needs more chunks.
		// Every benchmark trace starts at the fixed base, which starts a chunk
		// range, and ends within it.
		chunkRange := tsdb.DefaultOptions().MinBlockDuration
		require.Zero(t, metadataPipelineBase%chunkRange)
		for _, sweeps := range []int{200, 4} {
			c := metadataPipelineConfig{Base: metadataPipelineBase, Sweeps: sweeps}
			require.Equal(t, c.timestamp(0)/chunkRange, c.timestamp(c.lastStep())/chunkRange)
		}
		chunksFrom := func(base int64) int {
			db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			c := metadataPipelineConfig{Base: base, Sweeps: 200}
			app := db.AppenderV2(t.Context())
			var ref storage.SeriesRef
			for step := 0; step <= c.lastStep(); step++ {
				ref, err = app.Append(ref, metadataPipelineLabels(0), 0, c.timestamp(step), float64(step+1), nil, nil, storage.AOptions{})
				require.NoError(t, err)
			}
			require.NoError(t, app.Commit())
			q, err := db.ChunkQuerier(math.MinInt64, math.MaxInt64)
			require.NoError(t, err)
			defer func() { require.NoError(t, q.Close()) }()
			set := q.Select(t.Context(), false, nil, labels.MustNewMatcher(labels.MatchEqual, "id", "0"))
			n := 0
			for set.Next() {
				it := set.At().Iterator(nil)
				for it.Next() {
					n++
				}
				require.NoError(t, it.Err())
			}
			require.NoError(t, set.Err())
			return n
		}
		fixed, straddling := chunksFrom(metadataPipelineBase), chunksFrom(metadataPipelineBase-10*time.Minute.Milliseconds())
		t.Logf("chunks of a 200-sweep series: %d from the fixed base, %d when straddling a boundary", fixed, straddling)
		require.Equal(t, fixed, chunksFrom(metadataPipelineBase+chunkRange), "the layout repeats every chunk range")
		require.Greater(t, straddling, fixed, "a straddling trace needs more chunks")
	})
	t.Run("release decision", func(t *testing.T) {
		for _, tc := range []struct {
			name              string
			baseline, current int64
			lag               int
			due               bool
		}{
			{name: "seed deliveries alone", baseline: 10000, current: 10000, lag: 10000},
			{name: "one sample short", baseline: 10000, current: 19999, lag: 10000},
			{name: "lag reached", baseline: 10000, current: 20000, lag: 10000, due: true},
			{name: "lag exceeded", baseline: 10000, current: 20500, lag: 10000, due: true},
			{name: "no lag", baseline: 10000, current: 10000, due: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				require.Equal(t, tc.due, metadataPipelineReleaseDue(tc.baseline, tc.current, tc.lag))
			})
		}
	})
	t.Run("release lag configuration", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mutate func(*metadataPipelineConfig)
			err    string
		}{
			{name: "lag leaving a queue's capacity of the trace"},
			{name: "negative lag", mutate: func(c *metadataPipelineConfig) { c.ReleaseLag = -1 }, err: "release lag"},
			{name: "lag without a held backlog", mutate: func(c *metadataPipelineConfig) { c.Group, c.Case = "", "unchanged" }, err: "release lag"},
			{name: "lag with one endpoint", mutate: func(c *metadataPipelineConfig) { c.Endpoints = 1 }, err: "release lag"},
			{name: "lag with three endpoints", mutate: func(c *metadataPipelineConfig) { c.Endpoints = 3 }, err: "release lag"},
			{name: "lag exceeding the trace less a queue's capacity", mutate: func(c *metadataPipelineConfig) { c.ReleaseLag++ }, err: "release lag"},
			{name: "two endpoints with a restart", mutate: func(c *metadataPipelineConfig) {
				c.Group, c.Case, c.Sweeps, c.RestartStep, c.ReleaseLag = "", "restart", 200, 180, 0
			}, err: "without restarts"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// 1,200 samples follow the seed, and a queue holds 100.
				c := metadataPipelineConfig{Group: "backlog", Case: "backlog", Source: "native", Series: 300, Values: 300, Sweeps: 4, Writers: 1, Shards: 1, Batch: 20, Capacity: 100, CommitSize: 50, ReceiverProcs: 2, Endpoints: 2, ReleaseLag: 1100}
				if tc.mutate != nil {
					tc.mutate(&c)
				}
				f, err := newMetadataPipeline(t.Context(), c)
				if tc.err != "" {
					require.ErrorContains(t, err, tc.err)
					return
				}
				require.NoError(t, err)
				require.NoError(t, f.close())
			})
		}
	})
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
					require.NoError(t, f.holdReceivers(ctx))
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
