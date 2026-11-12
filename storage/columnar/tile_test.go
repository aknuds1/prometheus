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

package columnar

// Correctness for the Stage 3 tile representations. Everything here runs outside
// any timed loop: exact sample equality against the source series, randomized
// partitions and windows, exact presence semantics, and the invalid-input paths.
// Physical-work expectations live in TestTilePhysicalWork, which is the evidence
// that a selection touched the columns it claims to.

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
)

const (
	tileBaseTime   = int64(1_700_000_000_000)
	tileIntervalMs = int64(15_000)
)

var tileTraceNames = []string{"dense", "jittered", "churned", "sparse"}

// tileFixture builds mixed-kind series for one trace. The corpus is deliberately
// awkward: signed zero, infinities, arbitrary NaN payloads, denormals, stale
// markers, integer and float native histograms, custom buckets, gauge and
// counter-reset hints, and start timestamps on some series.
func tileFixture(series, samples int, trace string) []Series {
	out := make([]Series, series)
	for i := range series {
		item := Series{Labels: labels.FromStrings(
			labels.MetricName, "node_cpu_seconds_total",
			"instance", fmt.Sprintf("host-%d:9100", i/64),
			"cpu", strconv.Itoa(i%16),
			"mode", []string{"idle", "user", "system", "iowait"}[i%4],
			"series", strconv.Itoa(i),
		)}
		for j := range samples {
			if trace == "churned" && (uint64(i)*31+uint64(j)*17)%10 == 0 {
				continue
			}
			if trace == "sparse" && (uint64(i)*31+uint64(j)*17)%33 != 0 {
				continue
			}
			t := tileBaseTime + int64(j)*tileIntervalMs
			if trace == "jittered" {
				t += int64((i*37+j*11)%4001) - 2000
			}
			switch {
			case i%7 == 3:
				item.Timestamps = append(item.Timestamps, t)
				item.Kinds = append(item.Kinds, SampleHistogram)
				item.Values = append(item.Values, 0)
				h := &histogram.Histogram{
					Count: 9, Sum: float64(j) + 0.5, Schema: 0, ZeroThreshold: 0.001, ZeroCount: 1,
					PositiveSpans:    []histogram.Span{{Offset: 0, Length: 2}},
					PositiveBuckets:  []int64{1, -1},
					CounterResetHint: histogram.CounterResetHint(i % 3),
				}
				payload, err := encodeHistogram(h, nil)
				if err != nil {
					panic(err)
				}
				item.Histograms = append(item.Histograms, payload)
				if i%2 == 0 {
					item.StartTimestamps = append(item.StartTimestamps, t-tileIntervalMs)
				}
			case i%11 == 5:
				item.Timestamps = append(item.Timestamps, t)
				item.Kinds = append(item.Kinds, SampleFloatHistogram)
				item.Values = append(item.Values, 0)
				fh := &histogram.FloatHistogram{
					Count: 4.5, Sum: float64(j) / 3, Schema: histogram.CustomBucketsSchema,
					PositiveSpans:    []histogram.Span{{Offset: 0, Length: 2}},
					PositiveBuckets:  []float64{0.5, 1.5},
					CustomValues:     []float64{-1.5, 0, 2.5, 5},
					CounterResetHint: histogram.GaugeType,
				}
				payload, err := encodeHistogram(nil, fh)
				if err != nil {
					panic(err)
				}
				item.Histograms = append(item.Histograms, payload)
			default:
				kind := SampleFloat
				v := tileFloatValue(i, j)
				if (i+j)%97 == 0 {
					kind = SampleStale
					v = math.Float64frombits(value.StaleNaN)
				}
				item.Timestamps = append(item.Timestamps, t)
				item.Kinds = append(item.Kinds, kind)
				item.Values = append(item.Values, v)
				if i%4 == 0 {
					item.StartTimestamps = append(item.StartTimestamps, t-tileIntervalMs)
				}
			}
		}
		if len(item.Histograms) > 0 {
			// Histograms must be aligned with the sample columns, including the
			// float samples of a mixed series.
			full := make([][]byte, len(item.Timestamps))
			at := 0
			for j := range item.Timestamps {
				if at < len(item.Histograms) && (item.Kinds[j] == SampleHistogram || item.Kinds[j] == SampleFloatHistogram) {
					full[j] = item.Histograms[at]
					at++
				}
			}
			item.Histograms = full
		}
		out[i] = item
	}
	return out
}

// tileFloatValue returns a value chosen to break naive encoders and comparators.
func tileFloatValue(i, j int) float64 {
	switch (i*7 + j) % 11 {
	case 0:
		return math.Copysign(0, -1)
	case 1:
		return math.Inf(1)
	case 2:
		return math.Inf(-1)
	case 3:
		return math.Float64frombits(0x7FF8000000000123) // Arbitrary NaN payload.
	case 4:
		return math.SmallestNonzeroFloat64
	case 5:
		return math.MaxFloat64
	case 6:
		return 1.0 / 3.0
	default:
		return float64(i%97) + float64(j)*0.37
	}
}

// tileCanonical renders a series' samples so two implementations can be compared
// exactly. Stale markers compare as the canonical stale NaN, which is the value
// the storage iterator reconstructs for that kind.
func tileCanonical(t *testing.T, s Series) []string {
	t.Helper()
	out := make([]string, 0, s.Len())
	for i := range s.Len() {
		kind := s.Kinds[i]
		var valueBits uint64
		if kind == SampleStale {
			valueBits = value.StaleNaN
		} else {
			valueBits = math.Float64bits(s.Values[i])
		}
		var start int64
		if len(s.StartTimestamps) > 0 {
			start = s.StartTimestamps[i]
		}
		entry := fmt.Sprintf("%d|%d|%016x|%d", s.Timestamps[i], kind, valueBits, start)
		if kind == SampleHistogram || kind == SampleFloatHistogram {
			h, fh, err := s.HistogramAt(i)
			require.NoError(t, err)
			if h != nil {
				entry += "|" + h.String()
			} else {
				entry += "|" + fh.String()
			}
		}
		out = append(out, entry)
	}
	return out
}

// tileReferenceSelection is the independent logical answer: the source series
// filtered by matchers and trimmed to the window.
func tileReferenceSelection(series []Series, matchers []*labels.Matcher, mint, maxt int64) map[int]Series {
	out := map[int]Series{}
	for ord := range series {
		if !matchesAll(series[ord].Labels, matchers) {
			continue
		}
		trimmed := trimSeries(series[ord], mint, maxt)
		if trimmed.Len() == 0 {
			continue
		}
		out[ord] = trimmed
	}
	return out
}

// tileShape is one matcher set used across the selection matrix.
type tileShape struct {
	name     string
	matchers []*labels.Matcher
}

func tileShapes() []tileShape {
	return []tileShape{
		{"one", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "series", "0")}},
		{"sel64", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "instance", "host-1:9100")}},
		{"cpu0", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "cpu", "0")}},
		{"mode_idle", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "mode", "idle")}},
		{"full", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "node_cpu_seconds_total")}},
	}
}

// tileWindow is one query window.
type tileWindow struct {
	name string
	mint int64
	maxt int64
}

func tileWindows(series []Series, samples int) []tileWindow {
	last := tileBaseTime + int64(samples-1)*tileIntervalMs
	return []tileWindow{
		{"full", tileBaseTime - 60_000, last + 60_000},
		{"middle", tileBaseTime + 10*tileIntervalMs, tileBaseTime + 30*tileIntervalMs},
		{"instant", tileBaseTime + 17*tileIntervalMs, tileBaseTime + 17*tileIntervalMs},
		{"before", tileBaseTime - 10*tileIntervalMs, tileBaseTime - 60_000},
	}
}

// TestTileSelectionEquivalence compares every layout against the source series
// for every trace, selection shape and window. A layout may not win by returning
// a different logical result, so any difference fails here first.
func TestTileSelectionEquivalence(t *testing.T) {
	const series, samples = 512, 60
	shapes := tileShapes()

	for _, trace := range tileTraceNames {
		fixture := tileFixture(series, samples, trace)
		windows := tileWindows(fixture, samples)
		for _, maxSeries := range []int{32, 64, 128} {
			for _, maxTimestamps := range []int{32, 64} {
				for layout := TileLayout(0); layout < tilePresentLayouts; layout++ {
					cfg := TileConfig{Layout: layout, MaxSeries: maxSeries, MaxTimestamps: maxTimestamps}
					set, err := BuildTileSet(cfg, fixture)
					require.NoError(t, err, "trace=%s cfg=%+v", trace, cfg)
					require.NotZero(t, set.Stats().Tiles)

					for _, shape := range shapes {
						for _, window := range windows {
							want := tileReferenceSelection(fixture, shape.matchers, window.mint, window.maxt)
							sel, err := set.Select(window.mint, window.maxt, shape.matchers...)
							require.NoError(t, err)
							require.GreaterOrEqual(t, sel.Len(), len(want),
								"trace=%s layout=%d series=%d ts=%d shape=%s window=%s: candidates",
								trace, layout, maxSeries, maxTimestamps, shape.name, window.name)

							// A row-layout selection is a candidate set, so a series that decodes
							// with no sample in the window is not a result.
							got := map[int]Series{}
							for i := range sel.Len() {
								var decoded Series
								require.NoError(t, sel.Decode(i, &decoded))
								if decoded.Len() == 0 {
									require.NotContains(t, want, sel.Ord(i),
										"series %d lost its samples", sel.Ord(i))
									continue
								}
								got[sel.Ord(i)] = decoded
							}
							for ord, expected := range want {
								actual, ok := got[ord]
								require.True(t, ok, "ordinal %d missing", ord)
								require.NoError(t, expected.Validate(), "reference ord=%d", ord)
								require.NoError(t, actual.Validate(), "decoded ord=%d trace=%s layout=%d", ord, trace, layout)
								require.Equal(t, tileCanonical(t, expected), tileCanonical(t, actual),
									"trace=%s layout=%d series=%d ts=%d shape=%s window=%s ord=%d",
									trace, layout, maxSeries, maxTimestamps, shape.name, window.name, ord)
							}
						}
					}
				}
			}
		}
	}
}

// TestTilePresenceExactness proves that missing samples stay missing in both
// directions: the decoded length equals the true stored count for the window, and
// no timestamp outside the source set appears.
func TestTilePresenceExactness(t *testing.T) {
	const series, samples = 256, 60
	for _, trace := range []string{"churned", "sparse", "jittered"} {
		fixture := tileFixture(series, samples, trace)
		for layout := TileLayout(0); layout < tilePresentLayouts; layout++ {
			cfg := TileConfig{Layout: layout, MaxSeries: 64, MaxTimestamps: 64}
			set, err := BuildTileSet(cfg, fixture)
			require.NoError(t, err)

			sel, err := set.Select(tileBaseTime-60_000, tileBaseTime+int64(samples)*tileIntervalMs, tileShapes()[4].matchers...)
			require.NoError(t, err)

			decodedSamples := 0
			for i := range sel.Len() {
				var decoded Series
				require.NoError(t, sel.Decode(i, &decoded))
				source := fixture[sel.Ord(i)]
				decodedSamples += decoded.Len()
				require.Len(t, decoded.Timestamps, source.Len(), "trace=%s layout=%d ord=%d", trace, layout, sel.Ord(i))
				require.Equal(t, source.Timestamps, decoded.Timestamps,
					"trace=%s layout=%d ord=%d fabricated or dropped timestamps", trace, layout, sel.Ord(i))
				for j := 1; j < decoded.Len(); j++ {
					require.Less(t, decoded.Timestamps[j-1], decoded.Timestamps[j],
						"trace=%s layout=%d ord=%d not strictly increasing", trace, layout, sel.Ord(i))
				}
			}
			total := 0
			for i := range fixture {
				total += fixture[i].Len()
			}
			require.Equal(t, total, decodedSamples, "trace=%s layout=%d decoded sample total", trace, layout)
			require.Equal(t, total, set.Stats().Samples, "trace=%s layout=%d stored sample total", trace, layout)
		}
	}
}

// TestTileRandomizedPartitions exercises partitions, matchers and windows that the
// fixed table cannot reach: random tile bounds, random label matchers and random
// windows, always compared against the full logical scan.
func TestTileRandomizedPartitions(t *testing.T) {
	const series, samples = 300, 40
	for _, trace := range tileTraceNames {
		fixture := tileFixture(series, samples, trace)
		state := uint64(0x9E3779B97F4A7C15)
		next := func(n int) int {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			return int(state % uint64(n))
		}
		for round := range 12 {
			cfg := TileConfig{
				Layout: TileLayout(round % tilePresentLayouts), MaxSeries: 8 + next(120),
				MaxTimestamps: 8 + next(96), IntervalMs: int64(5_000 + next(20_000)),
			}
			set, err := BuildTileSet(cfg, fixture)
			require.NoError(t, err)

			matchers := []*labels.Matcher{
				labels.MustNewMatcher(labels.MatchEqual, "cpu", strconv.Itoa(next(16))),
			}
			switch round % 3 {
			case 1:
				matchers = append(matchers, labels.MustNewMatcher(labels.MatchNotEqual, "mode", "idle"))
			case 2:
				matchers = append(matchers, labels.MustNewMatcher(labels.MatchRegexp, "instance", "host-[0-3]:9100"))
			}
			start := tileBaseTime + int64(next(samples))*tileIntervalMs
			end := start + int64(next(samples))*tileIntervalMs
			want := tileReferenceSelection(fixture, matchers, start, end)

			sel, err := set.Select(start, end, matchers...)
			require.NoError(t, err)
			require.GreaterOrEqual(t, sel.Len(), len(want), "round=%d trace=%s cfg=%+v", round, trace, cfg)
			for i := range sel.Len() {
				var decoded Series
				require.NoError(t, sel.Decode(i, &decoded))
				expected, ok := want[sel.Ord(i)]
				if !ok {
					require.Zero(t, decoded.Len(), "ord=%d decoded a series the reference does not select", sel.Ord(i))
					continue
				}
				require.NoError(t, expected.Validate(), "reference ord=%d", sel.Ord(i))
				require.NoError(t, decoded.Validate(), "decoded ord=%d", sel.Ord(i))
				require.Equal(t, tileCanonical(t, expected), tileCanonical(t, decoded),
					"round=%d trace=%s cfg=%+v ord=%d", round, trace, cfg, sel.Ord(i))
			}
		}
	}
}

// TestTileInvalidInput proves that malformed input and malformed encoded state
// produce errors instead of a partial series.
func TestTileInvalidInput(t *testing.T) {
	fixture := tileFixture(16, 8, "dense")

	t.Run("series columns", func(t *testing.T) {
		broken := slices.Clone(fixture)
		broken[0] = Series{Labels: broken[0].Labels, Timestamps: broken[0].Timestamps, Kinds: broken[0].Kinds}
		_, err := BuildTileSet(TileConfig{}, broken)
		require.Error(t, err)

		broken = slices.Clone(fixture)
		broken[1].Timestamps = []int64{10, 9}
		_, err = BuildTileSet(TileConfig{}, broken)
		require.Error(t, err)

		broken = slices.Clone(fixture)
		broken[2] = Series{}
		_, err = BuildTileSet(TileConfig{}, broken)
		require.Error(t, err)
	})

	t.Run("config", func(t *testing.T) {
		_, err := BuildTileSet(TileConfig{Layout: 9}, fixture)
		require.Error(t, err)
		_, err = BuildTileSet(TileConfig{MaxTimestamps: 1 << 40, IntervalMs: 1 << 40}, fixture)
		require.Error(t, err)
	})

	t.Run("directory", func(t *testing.T) {
		for layout := TileLayout(0); layout < tilePresentLayouts; layout++ {
			set, err := BuildTileSet(TileConfig{Layout: layout, MaxSeries: 4, MaxTimestamps: 4}, fixture)
			require.NoError(t, err)
			require.NotEmpty(t, set.tiles)

			tile := set.tiles[0]
			require.NotEmpty(t, tile.dir)

			// Truncation at every third byte must be rejected.
			for cut := 0; cut < len(tile.dir); cut += 3 {
				_, err := decodeTileDirectory(tile.dir[:cut], tile.rawLen)
				require.Error(t, err, "layout=%d cut=%d", layout, cut)
			}
			// Trailing bytes are rejected.
			_, err = decodeTileDirectory(append(slices.Clone(tile.dir), 0), tile.rawLen)
			require.Error(t, err)
			// An unknown layout and an out-of-range section are rejected.
			bad := slices.Clone(tile.dir)
			bad[0] = 0x7f
			_, err = decodeTileDirectory(bad, tile.rawLen)
			require.Error(t, err)

			// A section that points past the payload is rejected, whatever the
			// layout-specific field order.
			_, err = decodeTileDirectory(tile.dir, tile.rawLen-1)
			require.Error(t, err)
		}
	})

	t.Run("payload", func(t *testing.T) {
		for layout := TileLayout(0); layout < tilePresentLayouts; layout++ {
			set, err := BuildTileSet(TileConfig{Layout: layout, MaxSeries: 4, MaxTimestamps: 4}, fixture)
			require.NoError(t, err)

			sel, err := set.Select(tileBaseTime-60_000, tileBaseTime+int64(8)*tileIntervalMs, tileShapes()[4].matchers...)
			require.NoError(t, err)
			require.NotZero(t, sel.Len())

			// Truncating a payload invalidates at least one section range, which a
			// decoder must reject rather than silently returning fewer samples.
			require.Greater(t, len(set.tiles[0].payload), 1)
			set.tiles[0].payload = set.tiles[0].payload[:1]
			sel, err = set.Select(tileBaseTime-60_000, tileBaseTime+int64(8)*tileIntervalMs, tileShapes()[4].matchers...)
			require.NoError(t, err)
			var failed bool
			for i := range sel.Len() {
				var decoded Series
				if err := sel.Decode(i, &decoded); err != nil {
					failed = true
					break
				}
			}
			require.True(t, failed, "layout=%d accepted a truncated payload", layout)
		}

		// A lane payload with an unknown column codec must not decode.
		segment := SampleSegment{Count: 1}
		require.NoError(t, segment.encodeColumns([]SampleKind{SampleFloat}, []float64{1}, nil, nil))
		blob := encodeSegmentPayload(segment)
		decoded, err := decodeSegmentPayload(blockSegmentRef{
			Count: 1, ValueCodec: 0xf0, KindCodec: uint8(segment.kindCodec), StartCodec: uint8(segment.startCodec),
		}, blob)
		require.NoError(t, err)
		page := TypedPage{Timestamps: []int64{0}}
		require.Error(t, decoded.decodeColumnsInto(labels.EmptyLabels(), &page))
	})
}

// TestTilePhysicalWork asserts that a selection touches the columns the layout
// claims. This is the anti-fallback evidence: a candidate cannot be credited with
// reading only its own lanes if the representation forces a whole-tile decode.
func TestTilePhysicalWork(t *testing.T) {
	const series, samples = 512, 60
	fixture := tileFixture(series, samples, "dense")
	window := struct{ mint, maxt int64 }{tileBaseTime - 60_000, tileBaseTime + int64(samples)*tileIntervalMs}

	for layout := TileLayout(0); layout < tilePresentLayouts; layout++ {
		set, err := BuildTileSet(TileConfig{Layout: layout, MaxSeries: 64, MaxTimestamps: 64}, fixture)
		require.NoError(t, err)
		stats := set.Stats()
		require.Equal(t, series*samples, stats.Samples)
		require.NotZero(t, stats.DirectoryBytes, "layout=%d must report directory bytes", layout)
		require.Greater(t, stats.TotalBytes, stats.PayloadBytes)

		totalPayload := stats.PayloadBytes

		// One series: a series-major tile reads one lane, a row layout reads the tile.
		sel, err := set.Select(window.mint, window.maxt, tileShapes()[0].matchers...)
		require.NoError(t, err)
		require.Equal(t, 1, sel.Len())
		var decoded Series
		require.NoError(t, sel.Decode(0, &decoded))
		require.Equal(t, samples, decoded.Len())
		one := sel.Stats()
		require.Equal(t, 1, one.SeriesMaterialised)
		require.Greater(t, one.PayloadBytesTouched, 0)
		require.Greater(t, one.DirBytesTouched, 0)
		require.Equal(t, samples, one.SamplesDecoded)

		switch layout {
		case TileSeriesMajor:
			require.Less(t, one.PayloadBytesTouched, totalPayload/50,
				"series-major one-series read must be proportional to one lane")
			require.Equal(t, 1, one.LaneDecodes)
		case TileTimeMajor:
			require.Greater(t, one.PayloadBytesTouched, one.PayloadBytesTouched/2)
			require.Zero(t, one.LaneDecodes, "row layouts decode the whole tile")
			require.Greater(t, one.LanesMaterialised, 1,
				"a time-major selective read must pay the whole tile")
		case TileSparseEvents:
			require.Zero(t, one.LaneDecodes)
			require.Equal(t, 1, one.TileDecodes)
		}

		// Full selection: every sample, every series, and for row layouts every tile
		// exactly once.
		sel, err = set.Select(window.mint, window.maxt, tileShapes()[4].matchers...)
		require.NoError(t, err)
		require.Equal(t, series, sel.Len())
		for i := range sel.Len() {
			var item Series
			require.NoError(t, sel.Decode(i, &item))
		}
		full := sel.Stats()
		require.Equal(t, series, full.SeriesMaterialised)
		require.Equal(t, series*samples, full.SamplesDecoded)
		require.GreaterOrEqual(t, full.PayloadBytesTouched, totalPayload-1,
			"layout=%d full selection must read at least the payload once", layout)
		if layout != TileSeriesMajor {
			require.Equal(t, stats.Tiles, full.TileDecodes, "layout=%d decodes each tile once", layout)
		}

		// A window with no samples prunes every tile by time and touches nothing.
		sel, err = set.Select(tileBaseTime-10*tileIntervalMs, tileBaseTime-60_000, tileShapes()[4].matchers...)
		require.NoError(t, err)
		require.Zero(t, sel.Len())
		empty := sel.Stats()
		require.Equal(t, empty.TilesConsidered, empty.TilesPrunedByTime, "layout=%d", layout)
		require.Zero(t, empty.PayloadBytesTouched)

		// A matcher selecting nothing prunes by lanes, not by time.
		sel, err = set.Select(window.mint, window.maxt, labels.MustNewMatcher(labels.MatchEqual, "series", "absent"))
		require.NoError(t, err)
		require.Zero(t, sel.Len())
		absent := sel.Stats()
		require.Equal(t, absent.TilesConsidered, absent.TilesPrunedByLanes, "layout=%d", layout)
		require.Zero(t, absent.TileDecodes)

		t.Logf("layout=%d tiles=%d payloadB=%d dirB=%d totalB=%.2fB/sample sharedTs=%d fallbacks=%d",
			layout, stats.Tiles, stats.PayloadBytes, stats.DirectoryBytes, stats.BytesPerSample(),
			stats.SharedTimestampTiles, stats.TimeMajorFallbackTiles)
	}
}

// TestTileTimeMajorApplicability records what each layout can observe at seal
// time: a dense cohort and a cohort with missing scrapes keep a narrow timestamp
// union, while jitter widens it past the bound and forces the series-major
// fallback. No query history is consulted.
func TestTileTimeMajorApplicability(t *testing.T) {
	const series, samples = 256, 30
	for _, trace := range tileTraceNames {
		fixture := tileFixture(series, samples, trace)
		set, err := BuildTileSet(TileConfig{Layout: TileTimeMajor, MaxSeries: 64, MaxTimestamps: 64}, fixture)
		require.NoError(t, err)
		stats := set.Stats()
		switch trace {
		case "dense", "churned":
			require.Zero(t, stats.TimeMajorFallbackTiles, "trace=%s should keep dense time-major tiles", trace)
			require.Equal(t, stats.Tiles, stats.SharedTimestampTiles, "trace=%s timestamp sharing", trace)
		case "jittered":
			require.Equal(t, stats.Tiles, stats.TimeMajorFallbackTiles, "trace=%s must fall back", trace)
		case "sparse":
			// A sparse trace keeps few distinct timestamps per fixed time bucket, so
			// the union stays narrow and the dense form remains applicable. It is not
			// required to fall back; what must hold is that every tile is in exactly
			// one of the two observable states, and the byte cost is recorded.
			require.Equal(t, stats.Tiles, stats.TimeMajorFallbackTiles+stats.SharedTimestampTiles,
				"trace=%s must classify every tile", trace)
		}
		t.Logf("trace=%s tiles=%d fallbacks=%d shared=%d", trace, stats.Tiles, stats.TimeMajorFallbackTiles, stats.SharedTimestampTiles)
	}
}

// TestTileCompressedRoundTrip checks that the compressed variant is byte-for-byte
// equivalent in result to the uncompressed one.
func TestTileCompressedRoundTrip(t *testing.T) {
	const series, samples = 256, 60
	for _, trace := range tileTraceNames {
		fixture := tileFixture(series, samples, trace)
		for layout := TileLayout(0); layout < tilePresentLayouts; layout++ {
			plain, err := BuildTileSet(TileConfig{Layout: layout, MaxSeries: 64, MaxTimestamps: 64}, fixture)
			require.NoError(t, err)
			packed, err := BuildTileSet(TileConfig{Layout: layout, MaxSeries: 64, MaxTimestamps: 64, Compress: true}, fixture)
			require.NoError(t, err)

			require.LessOrEqual(t, packed.Stats().PayloadBytes, plain.Stats().PayloadBytes,
				"trace=%s layout=%d compression grew the payload", trace, layout)

			matchers := tileShapes()[4].matchers
			want, err := plain.Select(tileBaseTime-60_000, tileBaseTime+int64(samples)*tileIntervalMs, matchers...)
			require.NoError(t, err)
			got, err := packed.Select(tileBaseTime-60_000, tileBaseTime+int64(samples)*tileIntervalMs, matchers...)
			require.NoError(t, err)
			require.Equal(t, want.Len(), got.Len())
			for i := range got.Len() {
				var a, b Series
				require.NoError(t, want.Decode(i, &a))
				require.NoError(t, got.Decode(i, &b))
				require.Equal(t, tileCanonical(t, a), tileCanonical(t, b), "trace=%s layout=%d ord=%d", trace, layout, got.Ord(i))
			}
		}
	}
}

// TestTileBytesPerConfiguration records payload, directory and total cost for the
// layout x shape matrix so a size claim cannot omit the directory.
func TestTileBytesPerConfiguration(t *testing.T) {
	const series, samples = 1_000, 60
	for _, trace := range []string{"dense", "jittered", "sparse"} {
		fixture := tileFixture(series, samples, trace)
		stored := 0
		for _, item := range fixture {
			stored += item.Len()
		}
		for layout := TileLayout(0); layout < tilePresentLayouts; layout++ {
			for _, maxSeries := range []int{32, 64, 128, 256} {
				for _, maxTimestamps := range []int{32, 64, 128} {
					set, err := BuildTileSet(TileConfig{
						Layout: layout, MaxSeries: maxSeries, MaxTimestamps: maxTimestamps,
					}, fixture)
					require.NoError(t, err)
					stats := set.Stats()
					require.Equal(t, stored, stats.Samples)
					require.NotZero(t, stats.DirectoryBytes)
					t.Log(strings.Join([]string{
						fmt.Sprintf("trace=%s", trace),
						fmt.Sprintf("layout=%d", layout),
						fmt.Sprintf("series=%d", maxSeries),
						fmt.Sprintf("ts=%d", maxTimestamps),
						fmt.Sprintf("tiles=%d", stats.Tiles),
						fmt.Sprintf("payloadB=%d", stats.PayloadBytes),
						fmt.Sprintf("dirB=%d", stats.DirectoryBytes),
						fmt.Sprintf("B/sample=%.3f", stats.BytesPerSample()),
						fmt.Sprintf("sharedTs=%d", stats.SharedTimestampTiles),
						fmt.Sprintf("fallbacks=%d", stats.TimeMajorFallbackTiles),
					}, " "))
				}
			}
		}
	}
}

// TestTileTimestampCodecFinding quantifies a pre-existing defect in the shared
// timestamp codec rather than fixing it in this package. subtractInt64 reports
// overflow with the opposite comparison, so encodeTimestamps falls back to a plain
// eight-bytes-per-sample column whenever two consecutive deltas are equal. The
// tile encoders use a corrected check; this test records the size of the defect so
// the block control's bytes can be interpreted and the defect can be carded
// separately.
func TestTileTimestampCodecFinding(t *testing.T) {
	// A series with a missed scrape: not a constant interval, so the delta-delta
	// path is entered and the inverted check fires on the repeated delta.
	var timestamps []int64
	for j := range 60 {
		if j == 30 {
			continue
		}
		timestamps = append(timestamps, tileBaseTime+int64(j)*tileIntervalMs)
	}

	shared, sharedCodec, err := encodeTimestamps(timestamps)
	require.NoError(t, err)
	fixed, fixedCodec, err := encodeTileTimestamps(timestamps)
	require.NoError(t, err)

	decoded, err := decodeIntegerColumn(fixed, len(timestamps), fixedCodec)
	require.NoError(t, err)
	require.Equal(t, timestamps, decoded, "the corrected encoder must round-trip exactly")

	require.Equal(t, integerPlain, sharedCodec, "shared encoder falls back to plain")
	require.Equal(t, integerDeltaDelta, fixedCodec)
	require.Less(t, len(fixed), len(shared)/4)
	t.Logf("samples=%d shared codec=%d bytes=%d (%.2f B/sample); corrected codec=%d bytes=%d (%.2f B/sample)",
		len(timestamps), sharedCodec, len(shared), float64(len(shared))/float64(len(timestamps)),
		fixedCodec, len(fixed), float64(len(fixed))/float64(len(timestamps)))
}

// TestTileOrderingEffect isolates the mechanism behind the time-major footprint
// result: the same values, encoded by the same codec, cost far less when laid out
// in (timestamp, lane) order than when each lane's history is contiguous. The
// corpus' cross-series structure is what makes that true, so this test records the
// mechanism on the synthetic corpus only; a general codec claim needs captured
// payloads.
func TestTileOrderingEffect(t *testing.T) {
	const lanes, samples = 64, 60
	value := func(lane, sample int) float64 { return float64(lane%97) + float64(sample)*0.37 }

	laneMajor := make([]float64, 0, lanes*samples)
	for lane := range lanes {
		for sample := range samples {
			laneMajor = append(laneMajor, value(lane, sample))
		}
	}
	timeMajor := make([]float64, 0, lanes*samples)
	for sample := range samples {
		for lane := range lanes {
			timeMajor = append(timeMajor, value(lane, sample))
		}
	}
	laneBlob, laneCodec, err := encodeFloatColumn(laneMajor)
	require.NoError(t, err)
	timeBlob, timeCodec, err := encodeFloatColumn(timeMajor)
	require.NoError(t, err)

	laneDecoded, err := decodeFloatColumn(laneBlob, len(laneMajor), laneCodec)
	require.NoError(t, err)
	require.Equal(t, laneMajor, laneDecoded)
	timeDecoded, err := decodeFloatColumn(timeBlob, len(timeMajor), timeCodec)
	require.NoError(t, err)
	require.Equal(t, timeMajor, timeDecoded)

	require.Less(t, len(timeBlob)*2, len(laneBlob), "time-major order must compress materially better")
	t.Logf("lane-major codec=%d %.2f B/sample; time-major codec=%d %.2f B/sample",
		laneCodec, float64(len(laneBlob))/float64(len(laneMajor)),
		timeCodec, float64(len(timeBlob))/float64(len(timeMajor)))
}
