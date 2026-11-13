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

// Tile-local sample representations, for the Stage 3 offline bake-off.
//
// A tile bounds a set of local lanes (dense ordinals into this tile set's series
// list) and one time bucket. Three fixed layouts are implemented:
//
//   - TileSeriesMajor: every lane has its own byte range inside the tile, so a
//     selective read pays for its own lanes and nothing else. Timestamps are
//     shared by the tile when every lane carries the identical vector.
//   - TileTimeMajor: one shared timestamp vector, a per-timestamp lane presence
//     bitmap and the values in (timestamp, lane) order. Dense exact cohorts and
//     cohorts with missing scrapes are cheap; jittered cohorts are not, so a tile
//     whose timestamp union exceeds its bound is written series-major instead and
//     counted as a fallback. That decision uses only data already in the tile.
//   - TileSparseEvents: one event per sample ordered by (timestamp, lane), with a
//     lane column. No cohort, no presence bitmap, no per-lane framing.
//
// This is an experiment, not a storage format: nothing here touches Storage,
// blocks, the WAL or FormatVersion, tiles carry no series identity hash and no
// label catalog, and the only exported entry points are the ones the offline
// harness and the vertical-slice adapter use. Ownership is stated where it
// matters: BuildTileSet copies what it retains, and every selected lane is
// decoded into caller-provided Series columns.
//
// Missing samples are represented by absence, never by a fabricated point: the
// presence bitmap of a time-major tile and the event list of a sparse tile are
// authoritative, and a lane's decoded length always equals its true sample count
// in the queried window.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"

	"github.com/prometheus/prometheus/model/labels"
)

// TileLayout selects a tile's physical sample representation.
type TileLayout uint8

const (
	// TileSeriesMajor stores one independently addressable payload per lane.
	TileSeriesMajor TileLayout = iota
	// TileTimeMajor stores a shared timestamp vector, a lane presence bitmap per
	// timestamp and values in (timestamp, lane) order.
	TileTimeMajor
	// TileSparseEvents stores one event per sample ordered by (timestamp, lane).
	TileSparseEvents
)

// Default tile bounds. They are experiment inputs, not a format decision.
const (
	DefaultTileSeries     = 64
	DefaultTileTimestamps = 64
)

// tilePresentLayouts is the number of implemented layouts, used to size fixture
// loops in tests without forgetting one.
const tilePresentLayouts = 3

// tileDirVersion guards the directory encoding of this experiment.
const tileDirVersion = 1

// tileUnionFactor bounds when a time-major tile is applicable: the timestamp
// union must not exceed MaxTimestamps*tileUnionFactor/4. A jittered cohort
// widens its union by the number of lanes, so it falls back to series-major.
const tileUnionFactor = 5

// TileConfig configures one tile set.
type TileConfig struct {
	// Layout is the requested representation.
	Layout TileLayout
	// MaxSeries bounds the lanes per tile. Zero selects DefaultTileSeries.
	MaxSeries int
	// MaxTimestamps bounds the timestamps per tile. Zero selects
	// DefaultTileTimestamps.
	MaxTimestamps int
	// IntervalMs is the nominal sample interval, used only to size a tile's time
	// bucket. Zero selects 15s.
	IntervalMs int64
	// Compress compresses each series-major lane payload independently, and each
	// time-major or sparse tile payload as a whole, using the same zstd settings
	// as the block writer. Per-lane compression preserves series-major random
	// access; a row layout is a whole-tile scan either way.
	Compress bool
}

func (c TileConfig) withDefaults() TileConfig {
	if c.MaxSeries <= 0 {
		c.MaxSeries = DefaultTileSeries
	}
	if c.MaxTimestamps <= 0 {
		c.MaxTimestamps = DefaultTileTimestamps
	}
	if c.IntervalMs <= 0 {
		c.IntervalMs = 15_000
	}
	return c
}

// TileStats reports the physical cost of a tile set. PayloadBytes and
// DirectoryBytes are both real: the directory is the serialized structure the
// reader decodes, not an estimate.
type TileStats struct {
	Layout                 TileLayout
	Tiles                  int
	Series                 int
	Samples                int
	PayloadBytes           int
	DirectoryBytes         int
	TotalBytes             int
	TimeMajorFallbackTiles int
	SharedTimestampTiles   int
	CompressedTiles        int
}

// BytesPerSample is the total (payload + directory) cost per stored sample.
func (s TileStats) BytesPerSample() float64 {
	if s.Samples == 0 {
		return 0
	}
	return float64(s.TotalBytes) / float64(s.Samples)
}

// TileSet is an immutable set of encoded tiles over a fixed series list.
type TileSet struct {
	cfg    TileConfig
	labels []labels.Labels
	tiles  []encodedTile
	stats  TileStats
}

// tileSection addresses one encoded column inside a tile payload.
type tileSection struct {
	off    int
	length int
}

func (s tileSection) empty() bool { return s.length == 0 }

// tileLanePayload is the series-major directory entry of one lane.
type tileLanePayload struct {
	ord     int32
	count   int
	tsOff   int
	tsLen   int
	tsCodec integerCodec
	payload tileSection
	// valueCodec, kindCodec and startCodec index decodeSegmentPayload, so a lane
	// reuses the proven per-segment column decoders.
	valueCodec floatCodec
	kindCodec  integerCodec
	startCodec integerCodec
	compressed bool
	minT, maxT int64
}

// tileLaneRef locates one lane inside one encoded tile.
type tileLaneRef struct {
	tile int32
	lane int32
}

// encodedTile is one tile's directory plus payload. The layout field may differ
// from the requested layout for a time-major fallback tile.
type encodedTile struct {
	index     int
	layout    TileLayout
	ords      []int32
	minT      int64
	maxT      int64
	count     int
	sharedTs  bool
	tsSection tileSection
	tsCount   int
	tsCodec   integerCodec

	// Series-major lanes.
	lanes []tileLanePayload

	// Time-major and sparse sections.
	rowCount    int
	wordsPerRow int
	rowBits     tileSection
	values      tileSection
	valuesCodec floatCodec
	kinds       tileSection
	kindsCodec  integerCodec
	starts      tileSection
	startsCodec integerCodec
	histograms  tileSection
	eventLanes  tileSection
	lanesCodec  integerCodec
	allFloat    bool
	// hasHistograms reports that this tile carries histogram samples, so the
	// decoder knows the histogram column must exist and must be aligned.
	hasHistograms bool
	compressed    bool

	dir     []byte
	payload []byte
	// rawLen is the length of the payload the directory addresses. A row layout
	// compresses its payload as one blob, so section offsets refer to the expanded
	// bytes; a series-major tile compresses each lane separately and rawLen equals
	// the stored length.
	rawLen int
}

// BuildTileSet encodes series into tiles. The input is not retained and is not
// modified: tiles own their encoded bytes, and lane ordinals are the input
// positions, so callers must keep their own series list for label lookup.
func BuildTileSet(cfg TileConfig, series []Series) (*TileSet, error) {
	cfg = cfg.withDefaults()
	if cfg.Layout > TileSparseEvents {
		return nil, fmt.Errorf("columnar: unknown tile layout %d", cfg.Layout)
	}
	set := &TileSet{cfg: cfg, stats: TileStats{Layout: cfg.Layout}}
	if len(series) == 0 {
		return set, nil
	}
	width := int64(cfg.MaxTimestamps) * cfg.IntervalMs
	if cfg.MaxTimestamps > 0 && width/cfg.IntervalMs != int64(cfg.MaxTimestamps) {
		return nil, errors.New("columnar: tile time bucket overflows int64")
	}

	base := int64(math.MaxInt64)
	set.labels = make([]labels.Labels, len(series))
	for i := range series {
		if err := series[i].Validate(); err != nil {
			return nil, fmt.Errorf("series %d: %w", i, err)
		}
		set.labels[i] = series[i].Labels
		if n := series[i].Len(); n > 0 {
			base = min(base, series[i].Timestamps[0])
		}
	}
	set.stats.Series = len(series)

	for cohort := 0; cohort < len(series); cohort += cfg.MaxSeries {
		end := min(cohort+cfg.MaxSeries, len(series))
		buckets := map[int64][]tileLaneRange{}
		var order []int64
		for ord := cohort; ord < end; ord++ {
			item := series[ord]
			for from := 0; from < item.Len(); {
				bucket := (item.Timestamps[from] - base) / width
				to := from + 1
				for to < item.Len() && (item.Timestamps[to]-base)/width == bucket {
					to++
				}
				if _, seen := buckets[bucket]; !seen {
					order = append(order, bucket)
				}
				buckets[bucket] = append(buckets[bucket], tileLaneRange{ord: int32(ord), from: from, to: to})
				from = to
			}
		}
		slices.Sort(order)
		for _, bucket := range order {
			lanes := buckets[bucket]
			tile, err := buildEncodedTile(cfg, series, lanes)
			if err != nil {
				return nil, fmt.Errorf("tile %d: %w", len(set.tiles), err)
			}
			set.tiles = append(set.tiles, tile)
		}
	}

	slices.SortStableFunc(set.tiles, func(a, b encodedTile) int {
		switch {
		case a.minT < b.minT:
			return -1
		case a.minT > b.minT:
			return 1
		case a.ords[0] < b.ords[0]:
			return -1
		case a.ords[0] > b.ords[0]:
			return 1
		default:
			return 0
		}
	})
	for i := range set.tiles {
		set.tiles[i].index = i
		set.stats.Tiles++
		set.stats.Samples += set.tiles[i].count
		set.stats.PayloadBytes += len(set.tiles[i].payload)
		set.stats.DirectoryBytes += len(set.tiles[i].dir)
		if set.tiles[i].layout != cfg.Layout {
			set.stats.TimeMajorFallbackTiles++
		}
		if set.tiles[i].sharedTs {
			set.stats.SharedTimestampTiles++
		}
		if set.tiles[i].compressed {
			set.stats.CompressedTiles++
		}
	}
	set.stats.TotalBytes = set.stats.PayloadBytes + set.stats.DirectoryBytes
	return set, nil
}

// tileLaneRange is one lane's sample range inside the input series list.
type tileLaneRange struct {
	ord  int32
	from int
	to   int
}

func buildEncodedTile(cfg TileConfig, series []Series, lanes []tileLaneRange) (encodedTile, error) {
	tile := encodedTile{
		layout: cfg.Layout, ords: make([]int32, len(lanes)),
		minT: math.MaxInt64, maxT: math.MinInt64,
	}
	for i, lane := range lanes {
		tile.ords[i] = lane.ord
		tile.count += lane.to - lane.from
		tile.minT = min(tile.minT, series[lane.ord].Timestamps[lane.from])
		tile.maxT = max(tile.maxT, series[lane.ord].Timestamps[lane.to-1])
	}
	if len(lanes) == 0 {
		return tile, errors.New("empty tile")
	}

	union := unionTimestamps(series, lanes)
	tile.sharedTs = lanesShareTimestamps(series, lanes, union)

	var (
		built encodedTile
		err   error
	)
	switch cfg.Layout {
	case TileTimeMajor:
		if len(union) > cfg.MaxTimestamps*tileUnionFactor/4 {
			// Documented seal-time fallback: a jittered or over-wide cohort cannot
			// amortise a shared vector, so this tile keeps per-lane addressing.
			tile.layout = TileSeriesMajor
			built, err = buildSeriesMajorTile(tile, cfg, series, lanes, union)
		} else {
			built, err = buildTimeMajorTile(tile, cfg, series, lanes, union)
		}
	case TileSparseEvents:
		built, err = buildSparseTile(tile, cfg, series, lanes)
	default:
		built, err = buildSeriesMajorTile(tile, cfg, series, lanes, union)
	}
	if err != nil {
		return encodedTile{}, err
	}
	// Serialise the addressable structure once and read it back. The query path
	// therefore addresses lanes through the encoded directory rather than through
	// build-time structs, and the directory bytes reported by Stats are real.
	dir, err := encodeTileDirectory(built)
	if err != nil {
		return encodedTile{}, err
	}
	parsed, err := decodeTileDirectory(dir, built.rawLen)
	if err != nil {
		return encodedTile{}, err
	}
	parsed.payload = built.payload
	return parsed, nil
}

// unionTimestamps returns the sorted distinct timestamps of a tile's lanes.
func unionTimestamps(series []Series, lanes []tileLaneRange) []int64 {
	var union []int64
	if len(lanes) == 1 {
		item := series[lanes[0].ord]
		union = append(union, item.Timestamps[lanes[0].from:lanes[0].to]...)
		return union
	}
	// Merge the sorted lane vectors. Lane count is bounded by the tile bound, so a
	// repeated linear scan with a per-lane cursor is cheaper than a heap.
	cursors := make([]int, len(lanes))
	total := 0
	for _, lane := range lanes {
		total += lane.to - lane.from
	}
	union = make([]int64, 0, total)
	for {
		next := int64(math.MaxInt64)
		for i, lane := range lanes {
			item := series[lane.ord]
			if at := lane.from + cursors[i]; at < lane.to {
				next = min(next, item.Timestamps[at])
			}
		}
		if next == math.MaxInt64 {
			return union
		}
		if len(union) == 0 || union[len(union)-1] != next {
			union = append(union, next)
		}
		for i, lane := range lanes {
			item := series[lane.ord]
			if at := lane.from + cursors[i]; at < lane.to && item.Timestamps[at] == next {
				cursors[i]++
			}
		}
	}
}

func lanesShareTimestamps(series []Series, lanes []tileLaneRange, union []int64) bool {
	for _, lane := range lanes {
		item := series[lane.ord]
		if lane.to-lane.from != len(union) {
			return false
		}
		if !slices.Equal(item.Timestamps[lane.from:lane.to], union) {
			return false
		}
	}
	return len(union) > 0
}

// tileSectionBytes returns a section's bytes, rejecting a range outside the
// payload so a damaged or truncated payload produces an error rather than a panic.
func tileSectionBytes(payload []byte, section tileSection) ([]byte, error) {
	if section.length == 0 {
		return nil, nil
	}
	if section.off < 0 || section.length < 0 || section.off > len(payload) || section.length > len(payload)-section.off {
		return nil, errors.New("columnar: tile section outside payload")
	}
	return payload[section.off : section.off+section.length], nil
}

// encodeTileTimestamps is encodeTimestamps with a corrected overflow check.
//
// The shared helper tests subtractInt64's overflow sense backwards, so it falls
// back to a plain 8-byte-per-sample column whenever any two consecutive deltas are
// equal - which is the common case. That defect is pre-existing and is not fixed
// here (it would change format-10 bytes and is a separate card), but a tile
// candidate must not be credited for avoiding it, so tiles use this encoder and
// the effect on the block control is measured separately in
// TestTileTimestampCodecFinding.
func encodeTileTimestamps(values []int64) ([]byte, integerCodec, error) {
	if len(values) == 0 {
		return nil, integerPlain, nil
	}
	if constantInterval(values) {
		out := appendSigned(values[0])
		if len(values) > 1 {
			out = timestampVarints(out, values[1]-values[0])
		}
		return out, integerConstantInterval, nil
	}
	out := appendSigned(values[0])
	previousDelta := values[1] - values[0]
	out = timestampVarints(out, previousDelta)
	for i := 2; i < len(values); i++ {
		delta := values[i] - values[i-1]
		difference := delta - previousDelta
		if (delta^previousDelta)&(delta^difference) < 0 {
			return encodeIntPlain(values), integerPlain, nil
		}
		out = timestampVarints(out, difference)
		previousDelta = delta
	}
	if len(out) > len(values)*8 {
		return encodeIntPlain(values), integerPlain, nil
	}
	return out, integerDeltaDelta, nil
}

// appendTileSection appends p to the payload and returns its address.
func appendTileSection(payload []byte, p []byte) ([]byte, tileSection) {
	if len(p) == 0 {
		return payload, tileSection{}
	}
	section := tileSection{off: len(payload), length: len(p)}
	return append(payload, p...), section
}

// compressTileSection compresses p when configured and reports whether it did.
func compressTileSection(p []byte, compress bool) ([]byte, bool) {
	if !compress || len(p) < 64 {
		return p, false
	}
	blob, codec := compressPayload(p)
	return blob, codec == payloadCompressionZstd
}

func buildSeriesMajorTile(tile encodedTile, cfg TileConfig, series []Series, lanes []tileLaneRange, union []int64) (encodedTile, error) {
	var payload []byte
	if tile.sharedTs {
		encoded, codec, err := encodeTileTimestamps(union)
		if err != nil {
			return encodedTile{}, err
		}
		payload, tile.tsSection = appendTileSection(payload, encoded)
		tile.tsCount = len(union)
		tile.tsCodec = codec
	}
	tile.lanes = make([]tileLanePayload, len(lanes))
	for i, lane := range lanes {
		item := series[lane.ord]
		var starts []int64
		if len(item.StartTimestamps) > 0 {
			starts = item.StartTimestamps[lane.from:lane.to]
		}
		var histograms [][]byte
		if len(item.Histograms) > 0 {
			histograms = item.Histograms[lane.from:lane.to]
		}
		segment := SampleSegment{Count: lane.to - lane.from}
		if err := segment.encodeColumns(
			item.Kinds[lane.from:lane.to], item.Values[lane.from:lane.to], starts, histograms,
		); err != nil {
			return encodedTile{}, err
		}
		blob, compressed := compressTileSection(encodeSegmentPayload(segment), cfg.Compress)
		entry := tileLanePayload{
			ord: lane.ord, count: lane.to - lane.from, valueCodec: segment.valueCodec,
			kindCodec: segment.kindCodec, startCodec: segment.startCodec, compressed: compressed,
			minT: item.Timestamps[lane.from], maxT: item.Timestamps[lane.to-1],
		}
		payload, entry.payload = appendTileSection(payload, blob)
		if !tile.sharedTs {
			encoded, codec, err := encodeTileTimestamps(item.Timestamps[lane.from:lane.to])
			if err != nil {
				return encodedTile{}, err
			}
			var section tileSection
			payload, section = appendTileSection(payload, encoded)
			entry.tsOff, entry.tsLen, entry.tsCodec = section.off, section.length, codec
		}
		tile.lanes[i] = entry
	}
	tile.payload = payload
	tile.rawLen = len(payload)
	return tile, nil
}

func buildTimeMajorTile(tile encodedTile, cfg TileConfig, series []Series, lanes []tileLaneRange, union []int64) (encodedTile, error) {
	rowCount := len(union)
	wordsPerRow := (len(lanes) + 63) / 64
	rowBits := make([]uint64, rowCount*wordsPerRow)
	values := make([]float64, 0, tile.count)
	kinds := make([]SampleKind, 0, tile.count)
	// starts and histograms are indexed by present-sample position, exactly like
	// values, so a lane without those columns cannot shift a later sample's state.
	starts := make([]int64, tile.count)
	histograms := make([][]byte, tile.count)
	anyStart, hasStarts, hasHistograms := false, false, false
	allFloat := true

	next := 0
	cursors := make([]int, len(lanes))
	for row, ts := range union {
		for l, lane := range lanes {
			item := series[lane.ord]
			at := lane.from + cursors[l]
			if at >= lane.to || item.Timestamps[at] != ts {
				continue
			}
			rowBits[row*wordsPerRow+l/64] |= uint64(1) << uint(l%64)
			values = append(values, item.Values[at])
			kinds = append(kinds, item.Kinds[at])
			allFloat = allFloat && item.Kinds[at] == SampleFloat
			if len(item.StartTimestamps) > 0 {
				starts[next] = item.StartTimestamps[at]
				hasStarts = true
				anyStart = anyStart || starts[next] != 0
			}
			if len(item.Histograms) > 0 {
				histograms[next] = item.Histograms[at]
				hasHistograms = true
			}
			next++
			cursors[l]++
		}
	}
	if !hasStarts {
		starts = nil
	}
	if !hasHistograms {
		histograms = nil
	}
	if len(values) != tile.count {
		return encodedTile{}, fmt.Errorf("time-major tile holds %d samples, expected %d", len(values), tile.count)
	}

	encoded, codec, err := encodeTileTimestamps(union)
	if err != nil {
		return encodedTile{}, err
	}
	var payload []byte
	payload, tile.tsSection = appendTileSection(payload, encoded)
	tile.tsCount, tile.tsCodec, tile.sharedTs = rowCount, codec, true

	raw := make([]byte, rowCount*wordsPerRow*8)
	for i, word := range rowBits {
		binary.LittleEndian.PutUint64(raw[i*8:], word)
	}
	payload, tile.rowBits = appendTileSection(payload, raw)

	encoded, tile.valuesCodec, err = encodeFloatColumn(values)
	if err != nil {
		return encodedTile{}, err
	}
	payload, tile.values = appendTileSection(payload, encoded)

	tile.allFloat = allFloat
	if !allFloat {
		integerKinds := make([]int64, len(kinds))
		for i, kind := range kinds {
			integerKinds[i] = int64(kind)
		}
		encoded, tile.kindsCodec, err = encodeIntegerColumn(integerKinds)
		if err != nil {
			return encodedTile{}, err
		}
		payload, tile.kinds = appendTileSection(payload, encoded)
	}
	if anyStart {
		encoded, tile.startsCodec, err = encodeIntegerColumn(starts)
		if err != nil {
			return encodedTile{}, err
		}
		payload, tile.starts = appendTileSection(payload, encoded)
	}
	if hasHistograms {
		typed, err := encodeTypedHistograms(kinds, histograms)
		if err != nil {
			return encodedTile{}, err
		}
		if typed.count > 0 {
			tile.hasHistograms = true
			payload, tile.histograms = appendTileSection(payload, encodeHistogramFrame(nil, typed))
		}
	}

	tile.rowCount, tile.wordsPerRow = rowCount, wordsPerRow
	blob, compressed := compressTileSection(payload, cfg.Compress)
	if compressed {
		tile.compressed = true
	}
	tile.payload = blob
	tile.rawLen = len(payload)
	return tile, nil
}

func buildSparseTile(tile encodedTile, cfg TileConfig, series []Series, lanes []tileLaneRange) (encodedTile, error) {
	type event struct {
		t    int64
		lane int32
		at   int
	}
	events := make([]event, 0, tile.count)
	for l, lane := range lanes {
		for at := lane.from; at < lane.to; at++ {
			events = append(events, event{t: series[lane.ord].Timestamps[at], lane: int32(l), at: at})
		}
	}
	slices.SortStableFunc(events, func(a, b event) int {
		switch {
		case a.t < b.t:
			return -1
		case a.t > b.t:
			return 1
		case a.lane < b.lane:
			return -1
		case a.lane > b.lane:
			return 1
		default:
			return 0
		}
	})

	timestamps := make([]int64, len(events))
	laneColumn := make([]int64, len(events))
	values := make([]float64, len(events))
	kinds := make([]SampleKind, len(events))
	starts := make([]int64, len(events))
	histograms := make([][]byte, len(events))
	anyStart, allFloat := false, true
	for i, event := range events {
		lane := lanes[event.lane]
		item := series[lane.ord]
		timestamps[i] = event.t
		laneColumn[i] = int64(event.lane)
		values[i] = item.Values[event.at]
		kinds[i] = item.Kinds[event.at]
		allFloat = allFloat && item.Kinds[event.at] == SampleFloat
		if len(item.StartTimestamps) > 0 {
			starts[i] = item.StartTimestamps[event.at]
			anyStart = anyStart || starts[i] != 0
		}
		if len(item.Histograms) > 0 {
			histograms[i] = item.Histograms[event.at]
		}
	}

	var payload []byte
	encoded, codec, err := encodeTileTimestamps(timestamps)
	if err != nil {
		return encodedTile{}, err
	}
	payload, tile.tsSection = appendTileSection(payload, encoded)
	tile.tsCount, tile.tsCodec = len(timestamps), codec
	encoded, tile.lanesCodec, err = encodeIntegerColumn(laneColumn)
	if err != nil {
		return encodedTile{}, err
	}
	payload, tile.eventLanes = appendTileSection(payload, encoded)
	encoded, tile.valuesCodec, err = encodeFloatColumn(values)
	if err != nil {
		return encodedTile{}, err
	}
	payload, tile.values = appendTileSection(payload, encoded)
	tile.allFloat = allFloat
	if !allFloat {
		integerKinds := make([]int64, len(kinds))
		for i, kind := range kinds {
			integerKinds[i] = int64(kind)
		}
		encoded, tile.kindsCodec, err = encodeIntegerColumn(integerKinds)
		if err != nil {
			return encodedTile{}, err
		}
		payload, tile.kinds = appendTileSection(payload, encoded)
	}
	if anyStart {
		encoded, tile.startsCodec, err = encodeIntegerColumn(starts)
		if err != nil {
			return encodedTile{}, err
		}
		payload, tile.starts = appendTileSection(payload, encoded)
	}
	if !allFloat {
		typed, err := encodeTypedHistograms(kinds, histograms)
		if err != nil {
			return encodedTile{}, err
		}
		if typed.count > 0 {
			tile.hasHistograms = true
			payload, tile.histograms = appendTileSection(payload, encodeHistogramFrame(nil, typed))
		}
	}

	tile.rowCount = len(events)
	blob, compressed := compressTileSection(payload, cfg.Compress)
	if compressed {
		tile.compressed = true
	}
	tile.payload = blob
	tile.rawLen = len(payload)
	return tile, nil
}

// Directory flags.
const (
	tileFlagSharedTimestamps uint8 = 1 << 0
	tileFlagCompressed       uint8 = 1 << 1
	tileFlagAllFloat         uint8 = 1 << 2
	tileFlagAllStartsZero    uint8 = 1 << 3
	tileFlagHasHistograms    uint8 = 1 << 4
)

// encodeTileDirectory serialises the addressable structure of a tile. The
// reader parses exactly this, so the reported directory bytes are the bytes a
// query must read to address a lane.
func encodeTileDirectory(tile encodedTile) ([]byte, error) {
	buf := make([]byte, 0, 64+len(tile.lanes)*16)
	buf = append(buf, byte(tile.layout))
	var flags uint8
	if tile.sharedTs {
		flags |= tileFlagSharedTimestamps
	}
	if tile.compressed {
		flags |= tileFlagCompressed
	}
	if tile.allFloat {
		flags |= tileFlagAllFloat
	}
	if tile.starts.empty() {
		flags |= tileFlagAllStartsZero
	}
	if tile.hasHistograms {
		flags |= tileFlagHasHistograms
	}
	buf = append(buf, flags)
	buf = binary.AppendVarint(buf, tile.minT)
	buf = binary.AppendVarint(buf, tile.maxT)
	buf = binary.AppendUvarint(buf, uint64(len(tile.ords)))
	buf = binary.AppendUvarint(buf, uint64(tile.count))
	if tile.sharedTs {
		buf = binary.AppendUvarint(buf, uint64(tile.tsCount))
		buf = append(buf, byte(tile.tsCodec))
		buf = appendSection(buf, tile.tsSection)
	}

	switch tile.layout {
	case TileSeriesMajor:
		for _, lane := range tile.lanes {
			buf = binary.AppendVarint(buf, int64(lane.ord))
			buf = binary.AppendUvarint(buf, uint64(lane.count))
			buf = binary.AppendVarint(buf, lane.minT)
			buf = binary.AppendVarint(buf, lane.maxT)
			if !tile.sharedTs {
				buf = binary.AppendUvarint(buf, uint64(lane.tsOff))
				buf = binary.AppendUvarint(buf, uint64(lane.tsLen))
				buf = append(buf, byte(lane.tsCodec))
			}
			buf = appendSection(buf, lane.payload)
			buf = append(buf, byte(lane.valueCodec), byte(lane.kindCodec), byte(lane.startCodec))
			if lane.compressed {
				buf = append(buf, 1)
			} else {
				buf = append(buf, 0)
			}
		}
	case TileTimeMajor:
		for _, ord := range tile.ords {
			buf = binary.AppendVarint(buf, int64(ord))
		}
		buf = binary.AppendUvarint(buf, uint64(tile.rowCount))
		buf = binary.AppendUvarint(buf, uint64(tile.wordsPerRow))
		for _, section := range []tileSection{tile.rowBits, tile.values, tile.kinds, tile.starts, tile.histograms} {
			buf = appendSection(buf, section)
		}
		buf = append(buf, byte(tile.valuesCodec), byte(tile.kindsCodec), byte(tile.startsCodec))
	case TileSparseEvents:
		for _, ord := range tile.ords {
			buf = binary.AppendVarint(buf, int64(ord))
		}
		buf = binary.AppendUvarint(buf, uint64(tile.rowCount))
		buf = binary.AppendUvarint(buf, uint64(tile.tsCount))
		buf = append(buf, byte(tile.tsCodec))
		for _, section := range []tileSection{tile.tsSection, tile.eventLanes, tile.values, tile.kinds, tile.starts, tile.histograms} {
			buf = appendSection(buf, section)
		}
		buf = append(buf, byte(tile.lanesCodec), byte(tile.valuesCodec), byte(tile.kindsCodec), byte(tile.startsCodec))
	default:
		return nil, fmt.Errorf("columnar: unknown tile layout %d", tile.layout)
	}
	return buf, nil
}

func appendSection(buf []byte, section tileSection) []byte {
	buf = binary.AppendUvarint(buf, uint64(section.off))
	return binary.AppendUvarint(buf, uint64(section.length))
}

// decodeTileDirectory parses a directory produced by encodeTileDirectory and
// rejects offsets or lengths outside the payload, unknown fields and trailing
// bytes.
func decodeTileDirectory(dir []byte, payloadLen int) (encodedTile, error) {
	reader := tileReader{dir: dir, payloadLen: payloadLen}
	var tile encodedTile
	layout, err := reader.byte()
	if err != nil {
		return tile, err
	}
	if layout > byte(TileSparseEvents) {
		return tile, fmt.Errorf("columnar: unknown tile layout %d", layout)
	}
	tile.layout = TileLayout(layout)
	flags, err := reader.byte()
	if err != nil {
		return tile, err
	}
	tile.sharedTs = flags&tileFlagSharedTimestamps != 0
	tile.compressed = flags&tileFlagCompressed != 0
	tile.allFloat = flags&tileFlagAllFloat != 0
	tile.hasHistograms = flags&tileFlagHasHistograms != 0
	if tile.minT, err = reader.varint(); err != nil {
		return tile, err
	}
	if tile.maxT, err = reader.varint(); err != nil {
		return tile, err
	}
	numLanes, err := reader.uvarint()
	if err != nil {
		return tile, err
	}
	if numLanes > 1<<20 {
		return tile, errors.New("columnar: tile directory declares too many lanes")
	}
	count, err := reader.uvarint()
	if err != nil {
		return tile, err
	}
	tile.count = int(count)
	if tile.sharedTs {
		tsCount, err := reader.uvarint()
		if err != nil {
			return tile, err
		}
		tile.tsCount = int(tsCount)
		codec, err := reader.byte()
		if err != nil {
			return tile, err
		}
		tile.tsCodec = integerCodec(codec)
		if tile.tsSection, err = reader.section(); err != nil {
			return tile, err
		}
	}

	tile.ords = make([]int32, numLanes)
	switch tile.layout {
	case TileSeriesMajor:
		tile.lanes = make([]tileLanePayload, numLanes)
		for i := range tile.lanes {
			ord, err := reader.varint()
			if err != nil {
				return tile, err
			}
			laneCount, err := reader.uvarint()
			if err != nil {
				return tile, err
			}
			minT, err := reader.varint()
			if err != nil {
				return tile, err
			}
			maxT, err := reader.varint()
			if err != nil {
				return tile, err
			}
			lane := tileLanePayload{ord: int32(ord), count: int(laneCount), minT: minT, maxT: maxT}
			if !tile.sharedTs {
				off, err := reader.uvarint()
				if err != nil {
					return tile, err
				}
				length, err := reader.uvarint()
				if err != nil {
					return tile, err
				}
				codec, err := reader.byte()
				if err != nil {
					return tile, err
				}
				lane.tsOff, lane.tsLen, lane.tsCodec = int(off), int(length), integerCodec(codec)
			}
			if lane.payload, err = reader.section(); err != nil {
				return tile, err
			}
			codecs, err := reader.bytes(3)
			if err != nil {
				return tile, err
			}
			lane.valueCodec, lane.kindCodec, lane.startCodec = floatCodec(codecs[0]), integerCodec(codecs[1]), integerCodec(codecs[2])
			compressed, err := reader.byte()
			if err != nil {
				return tile, err
			}
			lane.compressed = compressed == 1
			tile.lanes[i] = lane
			tile.ords[i] = lane.ord
		}
	case TileTimeMajor:
		for i := range tile.ords {
			ord, err := reader.varint()
			if err != nil {
				return tile, err
			}
			tile.ords[i] = int32(ord)
		}
		rowCount, err := reader.uvarint()
		if err != nil {
			return tile, err
		}
		wordsPerRow, err := reader.uvarint()
		if err != nil {
			return tile, err
		}
		tile.rowCount, tile.wordsPerRow = int(rowCount), int(wordsPerRow)
		sections := make([]tileSection, 5)
		for i := range sections {
			if sections[i], err = reader.section(); err != nil {
				return tile, err
			}
		}
		tile.rowBits, tile.values, tile.kinds, tile.starts, tile.histograms =
			sections[0], sections[1], sections[2], sections[3], sections[4]
		codecs, err := reader.bytes(3)
		if err != nil {
			return tile, err
		}
		tile.valuesCodec, tile.kindsCodec, tile.startsCodec = floatCodec(codecs[0]), integerCodec(codecs[1]), integerCodec(codecs[2])
	case TileSparseEvents:
		for i := range tile.ords {
			ord, err := reader.varint()
			if err != nil {
				return tile, err
			}
			tile.ords[i] = int32(ord)
		}
		rowCount, err := reader.uvarint()
		if err != nil {
			return tile, err
		}
		tile.rowCount = int(rowCount)
		tsCount, err := reader.uvarint()
		if err != nil {
			return tile, err
		}
		tile.tsCount = int(tsCount)
		codec, err := reader.byte()
		if err != nil {
			return tile, err
		}
		tile.tsCodec = integerCodec(codec)
		sections := make([]tileSection, 6)
		for i := range sections {
			if sections[i], err = reader.section(); err != nil {
				return tile, err
			}
		}
		tile.tsSection, tile.eventLanes, tile.values, tile.kinds, tile.starts, tile.histograms =
			sections[0], sections[1], sections[2], sections[3], sections[4], sections[5]
		codecs, err := reader.bytes(4)
		if err != nil {
			return tile, err
		}
		tile.lanesCodec, tile.valuesCodec = integerCodec(codecs[0]), floatCodec(codecs[1])
		tile.kindsCodec, tile.startsCodec = integerCodec(codecs[2]), integerCodec(codecs[3])
	}
	if reader.err != nil {
		return tile, reader.err
	}
	if reader.pos != len(dir) {
		return tile, errors.New("columnar: trailing bytes in tile directory")
	}
	if tile.rowCount > payloadLen+1 || tile.tsCount > payloadLen+1 {
		return tile, errors.New("columnar: tile directory declares more rows than the payload can hold")
	}
	tile.dir = dir
	tile.rawLen = payloadLen
	return tile, nil
}

// tileReader parses a tile directory.
type tileReader struct {
	dir        []byte
	pos        int
	err        error
	payloadLen int
}

func (r *tileReader) byte() (byte, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.pos >= len(r.dir) {
		r.err = errors.New("columnar: truncated tile directory")
		return 0, r.err
	}
	v := r.dir[r.pos]
	r.pos++
	return v, nil
}

func (r *tileReader) bytes(n int) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	if n < 0 || r.pos+n > len(r.dir) {
		r.err = errors.New("columnar: truncated tile directory")
		return nil, r.err
	}
	v := r.dir[r.pos : r.pos+n]
	r.pos += n
	return v, nil
}

func (r *tileReader) uvarint() (uint64, error) {
	if r.err != nil {
		return 0, r.err
	}
	v, n := binary.Uvarint(r.dir[r.pos:])
	if n <= 0 {
		r.err = errors.New("columnar: invalid varint in tile directory")
		return 0, r.err
	}
	r.pos += n
	return v, nil
}

func (r *tileReader) varint() (int64, error) {
	if r.err != nil {
		return 0, r.err
	}
	v, n := binary.Varint(r.dir[r.pos:])
	if n <= 0 {
		r.err = errors.New("columnar: invalid varint in tile directory")
		return 0, r.err
	}
	r.pos += n
	return v, nil
}

// section reads an (offset, length) pair and rejects a range outside the payload.
func (r *tileReader) section() (tileSection, error) {
	off, err := r.uvarint()
	if err != nil {
		return tileSection{}, err
	}
	length, err := r.uvarint()
	if err != nil {
		return tileSection{}, err
	}
	if off > uint64(r.payloadLen) || length > uint64(r.payloadLen)-off {
		r.err = errors.New("columnar: tile section outside payload")
		return tileSection{}, r.err
	}
	return tileSection{off: int(off), length: int(length)}, nil
}

// Stats returns the physical cost of the tile set.
func (ts *TileSet) Stats() TileStats { return ts.stats }

// Config returns the effective configuration.
func (ts *TileSet) Config() TileConfig { return ts.cfg }

// SeriesCount returns the number of addressable ordinals.
func (ts *TileSet) SeriesCount() int { return len(ts.labels) }

// Labels returns the labels of an ordinal. The returned slice is shared with the
// tile set and must not be modified.
func (ts *TileSet) Labels(ord int) labels.Labels { return ts.labels[ord] }

// TileSelectionStats is the physical work of one selection and its decoding.
// The counters are the evidence a wall-clock result cannot provide: in
// particular PayloadBytesTouched is what the layout physically had to read for
// the lanes it answered, not what it wanted to read.
type TileSelectionStats struct {
	TilesConsidered    int
	TilesPrunedByTime  int
	TilesPrunedByLanes int
	// SeriesPrunedByTime counts label-matching series with no sample in the window.
	SeriesPrunedByTime  int
	TilesTouched        int
	TileDecodes         int
	LaneDecodes         int
	LanesMaterialised   int
	SeriesMaterialised  int
	SamplesDecoded      int
	PayloadBytesTouched int
	DirBytesTouched     int
}

// TileSelection is the result of a matcher and time-range selection. It owns the
// decoded tiles it caches while it is used; the caller must not reuse it after
// the next selection on the same tile set.
type TileSelection struct {
	set   *TileSet
	mint  int64
	maxt  int64
	ords  []int
	refs  [][]tileLaneRef
	stats TileSelectionStats
	// cache holds decoded row-layout tiles, keyed by tile index. Series-major
	// tiles are decoded per lane and never cached: they need no cross-lane state.
	cache   map[int]*decodedTile
	cacheTs map[int][]int64
	// dirRead tracks tiles whose directory this selection has already addressed,
	// so directory bytes are counted once per tile and not once per lane.
	dirRead map[int]struct{}
}

// Select resolves matchers against the tile set's series list and the window
// against tile and lane time bounds. Tiles that cannot contribute are pruned and
// counted; series that cannot contribute are dropped. No sample is decoded here:
// decoding is the caller's explicit next step, so a selection cannot smuggle
// precomputed output into a timed operation.
//
// A row layout keeps only tile-level bounds, so its selection is a candidate set:
// a lane can pass the tile bound and still hold no sample inside the window. Such
// a series decodes with zero samples and the storage adapter omits it, exactly as
// the block reader omits a series whose trimmed length is zero.
func (ts *TileSet) Select(mint, maxt int64, matchers ...*labels.Matcher) (*TileSelection, error) {
	selected := make([]bool, len(ts.labels))
	for _, matcher := range matchers {
		if matcher == nil {
			return nil, errors.New("columnar: nil label matcher")
		}
	}
	ords := make([]int, 0, 16)
	for ord := range ts.labels {
		if !matchesAll(ts.labels[ord], matchers) {
			continue
		}
		selected[ord] = true
		ords = append(ords, ord)
	}
	sel := &TileSelection{
		set: ts, mint: mint, maxt: maxt, ords: ords,
		refs:    make([][]tileLaneRef, len(ords)),
		cache:   map[int]*decodedTile{},
		cacheTs: map[int][]int64{},
		dirRead: map[int]struct{}{},
	}
	position := make(map[int32]int, len(ords))
	for i, ord := range ords {
		position[int32(ord)] = i
	}
	hadRef := make([]bool, len(ords))
	for i := range ts.tiles {
		tile := &ts.tiles[i]
		sel.stats.TilesConsidered++
		if tile.maxT < mint || tile.minT > maxt {
			sel.stats.TilesPrunedByTime++
			continue
		}
		hits := 0
		for lane, ord := range tile.ords {
			at, ok := position[ord]
			if !ok {
				continue
			}
			if tile.layout == TileSeriesMajor {
				// A lane carries its own time bounds, so a series whose samples all
				// fall outside the window is not a candidate at all.
				entry := &tile.lanes[lane]
				if entry.maxT < mint || entry.minT > maxt {
					continue
				}
			}
			sel.refs[at] = append(sel.refs[at], tileLaneRef{tile: int32(i), lane: int32(lane)})
			hadRef[at] = true
			hits++
		}
		if hits == 0 {
			sel.stats.TilesPrunedByLanes++
		}
	}
	// A series that matched the labels but holds no sample in the window is not a
	// result: the storage boundary omits it rather than returning an empty series.
	keptOrds := sel.ords[:0]
	keptRefs := sel.refs[:0]
	for i, ord := range sel.ords {
		if !hadRef[i] {
			sel.stats.SeriesPrunedByTime++
			continue
		}
		keptOrds = append(keptOrds, ord)
		keptRefs = append(keptRefs, sel.refs[i])
	}
	sel.ords, sel.refs = keptOrds, keptRefs

	order := make([]int, len(sel.ords))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return labels.Compare(ts.labels[sel.ords[a]], ts.labels[sel.ords[b]])
	})
	sortedOrds := make([]int, len(order))
	sortedRefs := make([][]tileLaneRef, len(order))
	for i, at := range order {
		sortedOrds[i] = sel.ords[at]
		sortedRefs[i] = sel.refs[at]
	}
	sel.ords, sel.refs = sortedOrds, sortedRefs
	return sel, nil
}

// Len returns the number of selected series.
func (sel *TileSelection) Len() int { return len(sel.ords) }

// Ord returns the global ordinal of selected position i.
func (sel *TileSelection) Ord(i int) int { return sel.ords[i] }

// Labels returns the labels of selected position i.
func (sel *TileSelection) Labels(i int) labels.Labels { return sel.set.labels[sel.ords[i]] }

// Stats returns the accumulated physical work.
func (sel *TileSelection) Stats() TileSelectionStats { return sel.stats }

// ResetStats clears the accumulated physical work without dropping decoded
// tiles, so a harness can attribute a second operation separately.
func (sel *TileSelection) ResetStats() { sel.stats = TileSelectionStats{} }

// Decode appends the samples of selected position i within the selection window
// to dst and sets dst.Labels. Absent samples stay absent: the decoded length is
// the true number of stored samples in the window, and no point is interpolated,
// zero-filled or invented from presence metadata.
//
// The caller owns dst's columns after the call; the tile set retains only its
// encoded bytes and any decoded tile cached inside the selection.
func (sel *TileSelection) Decode(i int, dst *Series) error {
	if i < 0 || i >= len(sel.ords) {
		return fmt.Errorf("columnar: selected series index %d out of range", i)
	}
	ord := int32(sel.ords[i])
	*dst = Series{Labels: sel.set.labels[ord]}
	for _, ref := range sel.refs[i] {
		tile := &sel.set.tiles[ref.tile]
		if tile.layout == TileSeriesMajor {
			if err := sel.decodeSeriesMajorLane(tile, ref.lane, dst); err != nil {
				return err
			}
			continue
		}
		decoded, err := sel.decodeRowTile(tile)
		if err != nil {
			return err
		}
		if err := appendDecodedLane(dst, decoded, ref.lane, sel.mint, sel.maxt); err != nil {
			return err
		}
	}
	// SamplesDecoded counts the samples actually copied into the result, whichever
	// layout produced them, so it can be compared with the logical expectation.
	sel.stats.SamplesDecoded += dst.Len()
	sel.stats.SeriesMaterialised++
	return nil
}

// decodeSeriesMajorLane decodes exactly one lane payload plus, when shared, the
// tile's timestamp vector.
func (sel *TileSelection) decodeSeriesMajorLane(tile *encodedTile, lane int32, dst *Series) error {
	entry := &tile.lanes[lane]
	if entry.count == 0 {
		return nil
	}
	blob, err := tileSectionBytes(tile.payload, entry.payload)
	if err != nil {
		return err
	}
	sel.stats.PayloadBytesTouched += len(blob)
	sel.touchDirectory(tile)
	if entry.compressed {
		expanded, err := decompressPayload(blockSegmentRef{Compression: payloadCompressionZstd}, blob)
		if err != nil {
			return fmt.Errorf("columnar: decompressing tile lane: %w", err)
		}
		blob = expanded
	}
	segment, err := decodeSegmentPayload(blockSegmentRef{
		Count: entry.count, MinTime: entry.minT, MaxTime: entry.maxT,
		ValueCodec: uint8(entry.valueCodec), KindCodec: uint8(entry.kindCodec), StartCodec: uint8(entry.startCodec),
	}, blob)
	if err != nil {
		return fmt.Errorf("columnar: decoding tile lane: %w", err)
	}

	var timestamps []int64
	if tile.sharedTs {
		shared, err := sel.sharedTimestamps(tile)
		if err != nil {
			return err
		}
		if len(shared) != entry.count {
			return fmt.Errorf("columnar: shared timestamp vector holds %d samples for a %d-sample lane", len(shared), entry.count)
		}
		timestamps = shared
	} else {
		encoded, err := tileSectionBytes(tile.payload, tileSection{off: entry.tsOff, length: entry.tsLen})
		if err != nil {
			return err
		}
		sel.stats.PayloadBytesTouched += len(encoded)
		if timestamps, err = decodeIntegerColumn(encoded, entry.count, entry.tsCodec); err != nil {
			return fmt.Errorf("columnar: decoding tile lane timestamps: %w", err)
		}
	}

	page := TypedPage{Labels: dst.Labels, Timestamps: timestamps}
	if err := segment.decodeColumnsInto(dst.Labels, &page); err != nil {
		return fmt.Errorf("columnar: decoding tile lane columns: %w", err)
	}
	sel.stats.LaneDecodes++
	appendPageWindow(dst, page, sel.mint, sel.maxt)
	return nil
}

// touchDirectory counts a tile's directory bytes once per selection.
func (sel *TileSelection) touchDirectory(tile *encodedTile) {
	if _, ok := sel.dirRead[tile.index]; ok {
		return
	}
	sel.dirRead[tile.index] = struct{}{}
	sel.stats.TilesTouched++
	sel.stats.DirBytesTouched += len(tile.dir)
}

// decodedTile is a row-layout tile decoded into per-lane columns. Building it
// materialises every lane of the tile, which is exactly the read amplification a
// selective query pays on these layouts and must therefore be counted.
type decodedTile struct {
	perLane []Series
	ords    []int32
}

func (sel *TileSelection) decodeRowTile(tile *encodedTile) (*decodedTile, error) {
	if cached, ok := sel.cache[tile.index]; ok {
		return cached, nil
	}
	sel.stats.TileDecodes++
	sel.stats.PayloadBytesTouched += len(tile.payload)
	sel.touchDirectory(tile)

	payload := tile.payload
	if tile.compressed {
		expanded, err := decompressPayload(blockSegmentRef{Compression: payloadCompressionZstd}, payload)
		if err != nil {
			return nil, fmt.Errorf("columnar: decompressing tile: %w", err)
		}
		payload = expanded
	}
	rows := tile.tsCount
	tsBytes, err := tileSectionBytes(payload, tile.tsSection)
	if err != nil {
		return nil, err
	}
	timestamps, err := decodeIntegerColumn(tsBytes, rows, tile.tsCodec)
	if err != nil {
		return nil, fmt.Errorf("columnar: decoding tile timestamps: %w", err)
	}
	total := 0
	if tile.layout == TileTimeMajor {
		total = countPresence(tile, payload)
	} else {
		total = rows
	}
	valueBytes, err := tileSectionBytes(payload, tile.values)
	if err != nil {
		return nil, err
	}
	values, err := decodeFloatColumn(valueBytes, total, tile.valuesCodec)
	if err != nil {
		return nil, fmt.Errorf("columnar: decoding tile values: %w", err)
	}
	kinds := make([]SampleKind, total)
	if tile.allFloat {
		for i := range kinds {
			kinds[i] = SampleFloat
		}
	} else {
		kindBytes, err := tileSectionBytes(payload, tile.kinds)
		if err != nil {
			return nil, err
		}
		integerKinds, err := decodeIntegerColumn(kindBytes, total, tile.kindsCodec)
		if err != nil {
			return nil, fmt.Errorf("columnar: decoding tile kinds: %w", err)
		}
		for i, kind := range integerKinds {
			kinds[i] = SampleKind(kind)
		}
	}
	var starts []int64
	if !tile.starts.empty() {
		startBytes, err := tileSectionBytes(payload, tile.starts)
		if err != nil {
			return nil, err
		}
		if starts, err = decodeIntegerColumn(startBytes, total, tile.startsCodec); err != nil {
			return nil, fmt.Errorf("columnar: decoding tile start timestamps: %w", err)
		}
	}
	var histograms [][]byte
	if tile.hasHistograms {
		histBytes, err := tileSectionBytes(payload, tile.histograms)
		if err != nil {
			return nil, err
		}
		typed, rest, err := decodeHistogramFrame(histBytes)
		if err != nil {
			return nil, fmt.Errorf("columnar: decoding tile histograms: %w", err)
		}
		// The frame does not carry its own row count; the kinds column is
		// authoritative for how many histogram samples the tile holds.
		typed.count = countHistogramKinds(kinds)
		if len(rest) != 0 {
			return nil, errors.New("columnar: trailing bytes in tile histogram frame")
		}
		if histograms, err = typed.decode(kinds); err != nil {
			return nil, fmt.Errorf("columnar: decoding tile histograms: %w", err)
		}
		if len(histograms) != total {
			return nil, fmt.Errorf("columnar: tile histogram column holds %d samples, expected %d", len(histograms), total)
		}
	}

	decoded := &decodedTile{perLane: make([]Series, len(tile.ords)), ords: tile.ords}
	for lane := range decoded.perLane {
		decoded.perLane[lane].Labels = sel.set.labels[tile.ords[lane]]
	}
	sel.stats.LanesMaterialised += len(tile.ords)

	switch tile.layout {
	case TileTimeMajor:
		words := tile.wordsPerRow
		raw, err := tileSectionBytes(payload, tile.rowBits)
		if err != nil {
			return nil, err
		}
		if len(raw) < rows*words*8 {
			return nil, errors.New("columnar: tile presence bitmap is short")
		}
		at := 0
		for row := range rows {
			for lane := range len(tile.ords) {
				word := binary.LittleEndian.Uint64(raw[(row*words+lane/64)*8:])
				if word&(uint64(1)<<uint(lane%64)) == 0 {
					continue
				}
				if at >= total {
					return nil, errors.New("columnar: tile holds more present samples than its columns")
				}
				appendSample(&decoded.perLane[lane], timestamps[row], kinds[at], values[at], starts, histograms, at)
				at++
			}
		}
		if at != total {
			return nil, fmt.Errorf("columnar: tile presence bitmap marks %d samples, columns hold %d", at, total)
		}
	case TileSparseEvents:
		laneBytes, err := tileSectionBytes(payload, tile.eventLanes)
		if err != nil {
			return nil, err
		}
		lanes, err := decodeIntegerColumn(laneBytes, rows, tile.lanesCodec)
		if err != nil {
			return nil, fmt.Errorf("columnar: decoding tile lane column: %w", err)
		}
		for i := range rows {
			lane := lanes[i]
			if lane < 0 || int(lane) >= len(tile.ords) {
				return nil, fmt.Errorf("columnar: tile event %d references lane %d", i, lane)
			}
			appendSample(&decoded.perLane[lane], timestamps[i], kinds[i], values[i], starts, histograms, i)
		}
	}
	sel.cache[tile.index] = decoded
	return decoded, nil
}

func appendSample(dst *Series, t int64, kind SampleKind, value float64, starts []int64, histograms [][]byte, at int) {
	dst.Timestamps = append(dst.Timestamps, t)
	dst.Kinds = append(dst.Kinds, kind)
	dst.Values = append(dst.Values, value)
	if starts != nil {
		dst.StartTimestamps = append(dst.StartTimestamps, starts[at])
	}
	if histograms != nil {
		dst.Histograms = append(dst.Histograms, histograms[at])
	}
}

// countPresence counts set bits in a time-major presence bitmap.
func countPresence(tile *encodedTile, payload []byte) int {
	raw, err := tileSectionBytes(payload, tile.rowBits)
	if err != nil {
		return 0
	}
	count := 0
	for i := 0; i+8 <= len(raw); i += 8 {
		count += bits.OnesCount64(binary.LittleEndian.Uint64(raw[i:]))
	}
	return count
}

// sharedTimestamps decodes a tile's shared timestamp vector once per selection.
func (sel *TileSelection) sharedTimestamps(tile *encodedTile) ([]int64, error) {
	if cached, ok := sel.cacheTs[tile.index]; ok {
		return cached, nil
	}
	encoded, err := tileSectionBytes(tile.payload, tile.tsSection)
	if err != nil {
		return nil, err
	}
	sel.stats.PayloadBytesTouched += len(encoded)
	timestamps, err := decodeIntegerColumn(encoded, tile.tsCount, tile.tsCodec)
	if err != nil {
		return nil, fmt.Errorf("columnar: decoding shared tile timestamps: %w", err)
	}
	sel.cacheTs[tile.index] = timestamps
	return timestamps, nil
}

// appendDecodedLane copies one lane of a decoded tile within the window. Rows are
// the true stored samples, so nothing is fabricated for a missing point.
func appendDecodedLane(dst *Series, decoded *decodedTile, lane int32, mint, maxt int64) error {
	if int(lane) >= len(decoded.perLane) {
		return fmt.Errorf("columnar: lane %d outside decoded tile", lane)
	}
	source := decoded.perLane[lane]
	appendPageWindow(dst, TypedPage{
		Labels: source.Labels, Timestamps: source.Timestamps, StartTimestamps: source.StartTimestamps,
		Kinds: source.Kinds, Values: source.Values, Histograms: source.Histograms,
	}, mint, maxt)
	return nil
}

// appendPageWindow appends the samples of page inside [mint, maxt] to dst. It
// slices by index and copies once; an absent sample is never added.
func appendPageWindow(dst *Series, page TypedPage, mint, maxt int64) {
	from, to := 0, page.Len()
	for from < to && page.Timestamps[from] < mint {
		from++
	}
	for to > from && page.Timestamps[to-1] > maxt {
		to--
	}
	if from == to {
		return
	}
	window := TypedPage{
		Labels:     page.Labels,
		Timestamps: page.Timestamps[from:to],
		Kinds:      page.Kinds[from:to],
		Values:     page.Values[from:to],
	}
	if len(page.StartTimestamps) > 0 {
		window.StartTimestamps = page.StartTimestamps[from:to]
	}
	if len(page.Histograms) > 0 {
		window.Histograms = page.Histograms[from:to]
	}
	appendDecodedPage(dst, window)
}

// appendSeriesColumns appends src's sample columns to dst, keeping the optional
// start-timestamp and histogram columns aligned with the samples. A series can
// span several tiles, so one tile may contribute a column the next one lacks.
func appendSeriesColumns(dst *Series, src Series) {
	appendDecodedPage(dst, TypedPage{
		Labels: src.Labels, Timestamps: src.Timestamps, StartTimestamps: src.StartTimestamps,
		Kinds: src.Kinds, Values: src.Values, Histograms: src.Histograms,
	})
}
