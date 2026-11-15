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

package scrape

// These tests are the executable evidence for the columnar store's scrape
// route. They run a real scrape pool against a real HTTP target, so parsing,
// target and metric relabeling, metadata forwarding, staleness markers and the
// appender commit all execute. They are test-only and change no production
// path.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

// TestColumnarScrapeRouteStoresExactSamplesAndMetadata is the scrape-route
// obligation: a real scrape reaches the columnar store and the complete
// expected samples and metadata are asserted, not inferred from wiring.
func TestColumnarScrapeRouteStoresExactSamplesAndMetadata(t *testing.T) {
	const (
		firstTime  = int64(1700000000000)
		secondTime = int64(1700000001000)
	)
	// The first scrape serves three series; the second drops the second node, so
	// the scrape cache must append a staleness marker for it.
	firstBody := fmt.Sprintf(`# HELP contract_node_memory_bytes Node memory in bytes.
# TYPE contract_node_memory_bytes gauge
contract_node_memory_bytes{node="a",dropme="x"} 104857600 %d
contract_node_memory_bytes{node="b",dropme="x"} 209715200 %d
# HELP contract_signed_zero A negative zero.
# TYPE contract_signed_zero gauge
contract_signed_zero{gauge="bits"} -0 %d
`, firstTime, firstTime, firstTime)
	secondBody := fmt.Sprintf(`# HELP contract_node_memory_bytes Node memory in bytes.
# TYPE contract_node_memory_bytes gauge
contract_node_memory_bytes{node="a",dropme="x"} 314572800 %d
# HELP contract_signed_zero A negative zero.
# TYPE contract_signed_zero gauge
contract_signed_zero{gauge="bits"} -0 %d
`, secondTime, secondTime)

	var (
		scrapeMu    sync.Mutex
		scrapeCount int
	)
	scrapeStarted := make(chan int, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		scrapeMu.Lock()
		scrapeCount++
		count := scrapeCount
		scrapeMu.Unlock()
		body := firstBody
		if count > 1 {
			body = secondBody
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = fmt.Fprint(w, body)
		select {
		case scrapeStarted <- count:
		default:
		}
	}))
	defer server.Close()

	store, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: 1_000})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	cfg := &config.ScrapeConfig{
		JobName: "contract",
		// The target states its own sample timestamps, so honor them exactly.
		HonorTimestamps: true,
		// Track staleness even for timestamped series, so the dropped series gets
		// a marker on the next scrape.
		TrackTimestampsStaleness: true,
		// A long interval keeps exactly two scrapes in the exact expectations: the
		// first is immediate, the second follows one interval later.
		Scheme:                     "http",
		ScrapeInterval:             model.Duration(2 * time.Second),
		ScrapeTimeout:              model.Duration(2 * time.Second),
		MetricNameValidationScheme: model.UTF8Validation,
		MetricNameEscapingScheme:   model.AllowUTF8,
		// A real relabeling step proves the metric relabel config runs before the
		// append: the target's "dropme" label must never reach the store.
		MetricRelabelConfigs: []*relabel.Config{{
			Action: relabel.LabelDrop,
			Regex:  relabel.MustNewRegexp("dropme"),
		}},
	}
	// Metadata forwarding is the supported production configuration (the
	// metadata-wal-records feature); without it the scrape loop attaches none.
	sp, err := newScrapePool(cfg, nil, store, 0, nil, nil, &Options{AppendMetadata: true}, newTestScrapeMetrics(t))
	require.NoError(t, err)
	defer sp.stop()

	targetURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	sp.Sync([]*targetgroup.Group{{
		Targets: []model.LabelSet{{
			model.AddressLabel:  model.LabelValue(targetURL.Host),
			model.InstanceLabel: "contract-target",
		}},
		// A discovery-supplied label proves it reaches the stored series.
		Labels: model.LabelSet{"env": "test"},
	}})
	require.Len(t, sp.ActiveTargets(), 1)

	nodeA := labels.FromStrings("__name__", "contract_node_memory_bytes",
		"env", "test", "instance", "contract-target", "job", "contract", "node", "a")
	nodeB := labels.FromStrings("__name__", "contract_node_memory_bytes",
		"env", "test", "instance", "contract-target", "job", "contract", "node", "b")
	signedZero := labels.FromStrings("__name__", "contract_signed_zero",
		"env", "test", "gauge", "bits", "instance", "contract-target", "job", "contract")

	// Query the store while the scrape loop commits. Every snapshot must be a
	// coherent prefix of the two scrapes, so the head read/commit path is
	// exercised by the real scrape route and not only by the direct appender.
	liveErr := make(chan error, 1)
	liveStop := make(chan struct{})
	var liveWG sync.WaitGroup
	liveWG.Go(func() {
		for {
			select {
			case <-liveStop:
				return
			default:
			}
			if err := verifyLiveScrapeSnapshot(store, nodeA, nodeB, signedZero, firstTime, secondTime); err != nil {
				liveErr <- err
				return
			}
		}
	})

	waitForScrape := func(want int) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case got := <-scrapeStarted:
				if got >= want {
					return
				}
			case <-deadline:
				t.Fatalf("scrape %d did not start", want)
			}
		}
	}
	waitForScrape(1)
	waitForScrape(2)
	// Stop after the second scrape so the final expectations below contain
	// exactly two scrapes. The live reader above has already read the store
	// during the scrape commits; stopping is for determinism, not to avoid a
	// concurrent read.
	sp.stop()
	close(liveStop)
	liveWG.Wait()
	select {
	case err := <-liveErr:
		require.NoError(t, err, "a query during active scraping saw an incoherent head")
	default:
	}

	// Exact samples: labels (including the target's "env" label, the dropped
	// "dropme" label and the passed-through "node" label), timestamps, float bits
	// and kinds.
	require.Equal(t,
		[]string{
			fmt.Sprintf("%d|%s|float", firstTime, columnarBits(104857600)),
			fmt.Sprintf("%d|%s|float", secondTime, columnarBits(314572800)),
		},
		columnarRenderSamples(t, store, nodeA),
		"node a must hold exactly the two scraped values")
	nodeBSamples, ok := columnarExactSeries(t, store, nodeB)
	require.True(t, ok, "the dropped series must still exist with its marker")
	require.Len(t, nodeBSamples, 2, "the dropped series must end with a staleness marker at the second scrape")
	require.Equal(t, firstTime, nodeBSamples[0].t)
	require.Equal(t, columnarBits(209715200), nodeBSamples[0].value)
	require.Equal(t, chunkenc.ValFloat, nodeBSamples[0].typ)
	// The staleness marker is appended at the scrape start time, not at the
	// target's own timestamp, so only its kind and ordering are exact.
	require.Greater(t, nodeBSamples[1].t, firstTime)
	require.Equal(t, "stale", nodeBSamples[1].value)
	require.Equal(t, chunkenc.ValFloat, nodeBSamples[1].typ)
	require.Equal(t,
		[]string{
			fmt.Sprintf("%d|%s|float", firstTime, "0x8000000000000000"),
			fmt.Sprintf("%d|%s|float", secondTime, "0x8000000000000000"),
		},
		columnarRenderSamples(t, store, signedZero),
		"negative zero must survive the scrape with its sign bit")

	// The report sample proves the scrape succeeded and was committed.
	upSeries := columnarSelect(t, store, labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "up"))
	require.Len(t, upSeries, 1, "exactly one up series for one target")
	require.Len(t, upSeries[0].samples, 2, "one up sample per scrape")
	for _, sample := range upSeries[0].samples {
		require.Equal(t, columnarBits(1), sample.value)
		require.Equal(t, chunkenc.ValFloat, sample.typ)
	}

	// Metadata forwarded by the scrape creates one Metric owner per family with
	// the exact type/help/unit content, effective at the first sample.
	for _, tc := range []struct {
		ls    labels.Labels
		want  map[string]string
		title string
	}{
		{
			ls:    nodeA,
			title: "gauge family",
			want:  map[string]string{"type": "gauge", "unit": "", "help": "Node memory in bytes."},
		},
		{
			ls:    signedZero,
			title: "signed zero family",
			want:  map[string]string{"type": "gauge", "unit": "", "help": "A negative zero."},
		},
	} {
		t.Run(tc.title, func(t *testing.T) {
			history, ok := store.MetadataHistory(columnar.MetricOwner(tc.ls))
			require.True(t, ok, "the scraped series must have a Metric metadata owner")
			require.False(t, history.Truncated)
			require.Len(t, history.Versions, 1, "unchanged metadata observed twice must stay one version")
			require.Equal(t, tc.want, history.Versions[0].Content.Attributes)
			require.Equal(t, firstTime, history.Versions[0].EffectiveFrom)
		})
	}

	// No other contract series may appear: the scrape mutation must not invent,
	// duplicate or split them, and the dropped label must not survive.
	var names []string
	for _, series := range columnarSelect(t, store, labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, "contract_.*")) {
		names = append(names, series.labels.String())
	}
	require.ElementsMatch(t, []string{nodeA.String(), nodeB.String(), signedZero.String()}, names)
}

// TestColumnarScrapeDropsExemplarsWithoutError records the exemplar defect
// through a real OpenMetrics scrape: the sample is stored and the scrape
// reports success, while the exemplar is discarded and cannot be read back.
func TestColumnarScrapeDropsExemplarsWithoutError(t *testing.T) {
	const sampleTime = int64(1700000000)
	body := fmt.Sprintf(`# HELP contract_om_counter A counter with an exemplar.
# TYPE contract_om_counter counter
contract_om_counter_total 5.5 %d.000 # {trace_id="abc"} 1.25 %d.000
# EOF
`, sampleTime, sampleTime)

	scraped := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		_, _ = fmt.Fprint(w, body)
		select {
		case scraped <- struct{}{}:
		default:
		}
	}))
	defer server.Close()

	store, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: 1_000})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	cfg := &config.ScrapeConfig{
		JobName:                    "contract-exemplar",
		HonorTimestamps:            true,
		Scheme:                     "http",
		ScrapeInterval:             model.Duration(500 * time.Millisecond),
		ScrapeTimeout:              model.Duration(500 * time.Millisecond),
		MetricNameValidationScheme: model.UTF8Validation,
		MetricNameEscapingScheme:   model.AllowUTF8,
	}
	// Metadata forwarding is the supported production configuration (the
	// metadata-wal-records feature); without it the scrape loop attaches none.
	sp, err := newScrapePool(cfg, nil, store, 0, nil, nil, &Options{AppendMetadata: true}, newTestScrapeMetrics(t))
	require.NoError(t, err)
	defer sp.stop()

	targetURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	sp.Sync([]*targetgroup.Group{{
		Targets: []model.LabelSet{{
			model.AddressLabel:  model.LabelValue(targetURL.Host),
			model.InstanceLabel: "contract-target",
		}},
	}})

	series := labels.FromStrings("__name__", "contract_om_counter_total",
		"instance", "contract-target", "job", "contract-exemplar")
	select {
	case <-scraped:
	case <-time.After(10 * time.Second):
		t.Fatal("the exemplar target was not scraped")
	}
	// Stop before reading the store for the same head read/commit race reason as
	// the sample test above.
	sp.stop()
	require.Positive(t, columnarSeriesSamples(t, store, series),
		"the sample must be stored even though its exemplar is not")

	// The store cannot serve exemplars at all: the scrape loop saw no exemplar
	// error because the appender accepts and discards the exemplar.
	_, exemplarQueryable := any(store).(storage.ExemplarQueryable)
	require.False(t, exemplarQueryable, "the columnar store exposes no exemplar query surface")

	// The scalar V1 appender accepts an exemplar and reports success; the store
	// retains nothing that can be read, which is the defect this test records.
	app := store.Appender(t.Context())
	direct := labels.FromStrings("__name__", "contract_direct_exemplar")
	ref, err := app.Append(0, direct, 1000, 1)
	require.NoError(t, err)
	ref, err = app.AppendExemplar(ref, direct, exemplar.Exemplar{
		Labels: labels.FromStrings("trace_id", "direct"), Value: 1, Ts: 1000,
	})
	require.NoError(t, err, "the appender accepts the exemplar instead of reporting an unsupported operation")
	require.NotZero(t, ref)
	require.NoError(t, app.Commit())
}

// liveScrapeSample is one sample read while scrapes are in flight.
type liveScrapeSample struct {
	t     int64
	bits  string
	stale bool
}

// verifyLiveScrapeSnapshot checks that one querier snapshot is a coherent
// prefix of what the scrape loop has committed so far. It avoids testing
// helpers that call t.FailNow so it is safe from a worker goroutine.
func verifyLiveScrapeSnapshot(store *columnar.Storage, nodeA, nodeB, signedZero labels.Labels, firstTime, secondTime int64) error {
	querier, err := store.Querier(math.MinInt64, math.MaxInt64)
	if err != nil {
		return fmt.Errorf("creating live querier: %w", err)
	}
	defer querier.Close()
	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, "contract_.*"))
	seen := 0
	for set.Next() {
		seen++
		ls := set.At().Labels()
		samples := []liveScrapeSample{}
		iterator := set.At().Iterator(nil)
		for iterator.Next() != chunkenc.ValNone {
			_, v := iterator.At()
			samples = append(samples, liveScrapeSample{
				t: iterator.AtT(), bits: columnarBits(v), stale: value.IsStaleNaN(v),
			})
		}
		if err := iterator.Err(); err != nil {
			return fmt.Errorf("iterating %s: %w", ls, err)
		}
		switch {
		case labels.Equal(ls, nodeA):
			want := []liveScrapeSample{
				{t: firstTime, bits: columnarBits(104857600)},
				{t: secondTime, bits: columnarBits(314572800)},
			}
			if err := requireLivePrefix(ls, samples, want); err != nil {
				return err
			}
		case labels.Equal(ls, nodeB):
			first := liveScrapeSample{t: firstTime, bits: columnarBits(209715200)}
			if len(samples) > 0 && samples[0] != first {
				return fmt.Errorf("series %s first sample is %+v, want %+v", ls, samples[0], first)
			}
			if len(samples) == 2 {
				if !samples[1].stale || samples[1].t <= firstTime {
					return fmt.Errorf("series %s has an unexpected second sample %+v", ls, samples[1])
				}
			} else if len(samples) > 2 {
				return fmt.Errorf("series %s has %d samples, want at most 2", ls, len(samples))
			}
		case labels.Equal(ls, signedZero):
			want := []liveScrapeSample{
				{t: firstTime, bits: "0x8000000000000000"},
				{t: secondTime, bits: "0x8000000000000000"},
			}
			if err := requireLivePrefix(ls, samples, want); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected live contract series %s", ls)
		}
	}
	if err := set.Err(); err != nil {
		return fmt.Errorf("selecting live contract series: %w", err)
	}
	if seen > 3 {
		return fmt.Errorf("live selection returned %d contract series, want at most 3", seen)
	}
	return nil
}

// requireLivePrefix requires samples to be a prefix of want.
func requireLivePrefix(ls labels.Labels, samples, want []liveScrapeSample) error {
	if len(samples) > len(want) {
		return fmt.Errorf("series %s has %d samples, want at most %d", ls, len(samples), len(want))
	}
	for i, sample := range samples {
		if sample != want[i] {
			return fmt.Errorf("series %s sample %d is %+v, want %+v", ls, i, sample, want[i])
		}
	}
	return nil
}

// columnarSeriesSamples counts retained samples for one exact label set.
func columnarSeriesSamples(t *testing.T, store *columnar.Storage, ls labels.Labels) int {
	t.Helper()
	samples, ok := columnarExactSeries(t, store, ls)
	if !ok {
		return 0
	}
	return len(samples)
}

// columnarExactSeries returns the samples of one exact series.
func columnarExactSeries(t *testing.T, store *columnar.Storage, ls labels.Labels) ([]columnarSample, bool) {
	t.Helper()
	for _, series := range columnarSelect(t, store, labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, ls.Get(labels.MetricName))) {
		if labels.Equal(series.labels, ls) {
			return series.samples, true
		}
	}
	return nil, false
}

// columnarRenderSamples renders every sample of one exact series as
// timestamp|value-bits|kind. A staleness marker is rendered as "stale" instead
// of its NaN payload.
func columnarRenderSamples(t *testing.T, store *columnar.Storage, ls labels.Labels) []string {
	t.Helper()
	samples, ok := columnarExactSeries(t, store, ls)
	if !ok {
		return nil
	}
	out := []string{}
	for _, sample := range samples {
		out = append(out, fmt.Sprintf("%d|%s|%s", sample.t, sample.value, sample.typ))
	}
	return out
}

// columnarSample is one rendered sample.
type columnarSample struct {
	t     int64
	value string
	typ   chunkenc.ValueType
}

// columnarSeries is one rendered series.
type columnarSeries struct {
	labels  labels.Labels
	samples []columnarSample
}

// columnarSelect renders every series matching a matcher through the exported
// query surface. A staleness marker renders as "stale" instead of its NaN bits.
func columnarSelect(t *testing.T, store *columnar.Storage, matcher *labels.Matcher) []columnarSeries {
	t.Helper()
	querier, err := store.Querier(math.MinInt64, math.MaxInt64)
	require.NoError(t, err)
	defer func() { require.NoError(t, querier.Close()) }()
	set := querier.Select(t.Context(), true, nil, matcher)
	out := []columnarSeries{}
	for set.Next() {
		series := columnarSeries{labels: set.At().Labels()}
		iterator := set.At().Iterator(nil)
		for typ := iterator.Next(); typ != chunkenc.ValNone; typ = iterator.Next() {
			sample := columnarSample{t: iterator.AtT(), typ: typ}
			switch typ {
			case chunkenc.ValFloat:
				_, v := iterator.At()
				sample.value = columnarBits(v)
				if value.IsStaleNaN(v) {
					sample.value = "stale"
				}
			case chunkenc.ValHistogram, chunkenc.ValFloatHistogram:
				sample.value = "histogram"
			default:
				t.Fatalf("unexpected value type %v", typ)
			}
			series.samples = append(series.samples, sample)
		}
		require.NoError(t, iterator.Err())
		out = append(out, series)
	}
	require.NoError(t, set.Err())
	return out
}

// columnarBits renders an exact IEEE-754 bit pattern.
func columnarBits(v float64) string { return fmt.Sprintf("%#016x", math.Float64bits(v)) }
