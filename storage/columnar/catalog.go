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

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/prometheus/prometheus/model/labels"
)

const (
	catalogFileName = "index"
	catalogVersion  = 1
)

// blockSegmentRef locates one series segment in a cohort/time-window page.
// Offset is both the series' fixed position in its cohort slice and its offset
// in the page's per-series payload list.
type blockSegmentRef struct {
	RowGroup int
	Row      int
	Offset   int
	MinTime  int64
	MaxTime  int64
}

// blockSeriesEntry is the block series dictionary. ID is labels.StableHash of
// Labels, exactly the key used by native metadata memberships. In particular,
// __otel_resource__ participates in identity wherever OTLP ingestion adds it.
type blockSeriesEntry struct {
	ID       uint64
	Labels   labels.Labels
	Segments []blockSegmentRef
}

type blockPosting struct {
	Name  string
	Value string
	IDs   []uint64
}

type blockPostingKey struct {
	name  string
	value string
}

type blockCohortEntry struct {
	ID          uint64
	RowGroup    int
	NumSegments int
	NumSeries   int
	SeriesIDs   []uint64
	MinTime     int64
	MaxTime     int64
}

// blockCatalog is loaded when a block opens. It provides the series dictionary,
// sorted postings and per-series segment offsets without opening the sample
// Parquet payload. Public block queries must still validate full label sets.
type blockCatalog struct {
	Version int
	Series  []blockSeriesEntry
	// SeriesIDs mirrors Series IDs in order, so matcher resolution never has to
	// rebuild the full ID list per query or per negated matcher.
	SeriesIDs []uint64
	Postings  []blockPosting
	Cohorts   []blockCohortEntry
}

// buildBlockCatalog builds the series dictionary, postings and cohort map.
// intern optionally supplies a shared immutable label allocation; it is called
// once per series and may be nil for standalone writes.
func buildBlockCatalog(series []Series, opts WriteOptions, intern func(labels.Labels) (labels.Labels, error)) (blockCatalog, error) {
	catalog := blockCatalog{Version: catalogVersion}
	byID := make(map[uint64]int, len(series))
	postings := map[blockPostingKey]*blockPosting{}
	for i := range series {
		id := metadataSeriesKey(series[i].Labels)
		if previous, exists := byID[id]; exists {
			if !labels.Equal(catalog.Series[previous].Labels, series[i].Labels) {
				return blockCatalog{}, fmt.Errorf("%w: %q and %q", ErrSeriesHashCollision, catalog.Series[previous].Labels, series[i].Labels)
			}
			return blockCatalog{}, fmt.Errorf("duplicate series %q", series[i].Labels)
		}
		byID[id] = len(catalog.Series)
		entryLabels := series[i].Labels.Copy()
		if intern != nil {
			shared, err := intern(series[i].Labels)
			if err != nil {
				return blockCatalog{}, err
			}
			entryLabels = shared
		}
		catalog.Series = append(catalog.Series, blockSeriesEntry{ID: id, Labels: entryLabels})
		series[i].Labels.Range(func(l labels.Label) {
			key := blockPostingKey{name: l.Name, value: l.Value}
			posting := postings[key]
			if posting == nil {
				posting = &blockPosting{Name: l.Name, Value: l.Value}
				postings[key] = posting
			}
			posting.IDs = append(posting.IDs, id)
		})
	}
	// A hash-sorted dictionary makes stable-ID probes binary searches. Selection
	// results are sorted by complete labels before they cross the storage API.
	slices.SortFunc(catalog.Series, func(a, b blockSeriesEntry) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return labels.Compare(a.Labels, b.Labels)
		}
	})
	catalog.SeriesIDs = make([]uint64, len(catalog.Series))
	for i, entry := range catalog.Series {
		catalog.SeriesIDs[i] = entry.ID
	}
	for _, posting := range postings {
		slices.Sort(posting.IDs)
		catalog.Postings = append(catalog.Postings, *posting)
	}
	slices.SortFunc(catalog.Postings, func(a, b blockPosting) int {
		if a.Name != b.Name {
			switch {
			case a.Name < b.Name:
				return -1
			default:
				return 1
			}
		}
		switch {
		case a.Value < b.Value:
			return -1
		case a.Value > b.Value:
			return 1
		default:
			return 0
		}
	})

	// Only identical timestamp vectors share a cohort. Large cohorts are split
	// into independent slices so a cohort remains the Parquet row-group unit
	// without forcing every member into one read.
	type timestampGroup struct {
		series []int
	}
	groups := map[string]*timestampGroup{}
	var groupOrder []string
	seriesByID := make(map[uint64]int, len(series))
	for i := range series {
		seriesByID[metadataSeriesKey(series[i].Labels)] = i
	}
	for _, entry := range catalog.Series {
		i := seriesByID[entry.ID]
		if series[i].Len() == 0 {
			continue
		}
		key := timestampVectorKey(series[i].Timestamps)
		group := groups[key]
		if group == nil {
			group = &timestampGroup{}
			groups[key] = group
			groupOrder = append(groupOrder, key)
		}
		group.series = append(group.series, i)
	}

	var nextCohortID uint64 = 1
	for _, key := range groupOrder {
		group := groups[key]
		for start := 0; start < len(group.series); start += int(opts.MaxSeriesPerRowGroup) {
			end := min(start+int(opts.MaxSeriesPerRowGroup), len(group.series))
			members := group.series[start:end]
			cohortID := nextCohortID
			nextCohortID++
			rowGroup := len(catalog.Cohorts)
			numSegments := (series[members[0]].Len() + DefaultSamplesPerSegment - 1) / DefaultSamplesPerSegment
			cohort := blockCohortEntry{
				ID: cohortID, RowGroup: rowGroup, NumSegments: numSegments, NumSeries: len(members),
				MinTime: series[members[0]].Timestamps[0],
				MaxTime: series[members[0]].Timestamps[series[members[0]].Len()-1],
			}
			catalog.Cohorts = append(catalog.Cohorts, cohort)
			for offset, seriesIndex := range members {
				id := metadataSeriesKey(series[seriesIndex].Labels)
				catalog.Cohorts[rowGroup].SeriesIDs = append(catalog.Cohorts[rowGroup].SeriesIDs, id)
				catalogIndex := sortSearchSeries(catalog.Series, id)
				if catalogIndex < 0 || !labels.Equal(catalog.Series[catalogIndex].Labels, series[seriesIndex].Labels) {
					return blockCatalog{}, errors.New("columnar: series dictionary lost a cohort member")
				}
				for segment, sample := 0, 0; sample < series[seriesIndex].Len(); segment, sample = segment+1, sample+DefaultSamplesPerSegment {
					endSample := min(sample+DefaultSamplesPerSegment, series[seriesIndex].Len())
					catalog.Series[catalogIndex].Segments = append(catalog.Series[catalogIndex].Segments, blockSegmentRef{
						RowGroup: rowGroup, Row: segment, Offset: offset,
						MinTime: series[seriesIndex].Timestamps[sample], MaxTime: series[seriesIndex].Timestamps[endSample-1],
					})
				}
			}
		}
	}
	return catalog, nil
}

func timestampVectorKey(timestamps []int64) string {
	// A fixed-width binary key is collision-free, unlike a digest-only cohort
	// key. Cohort grouping must never align unequal sample timestamps.
	key := make([]byte, len(timestamps)*8)
	for i, timestamp := range timestamps {
		u := uint64(timestamp)
		for j := range 8 {
			key[i*8+j] = byte(u >> (56 - 8*j))
		}
	}
	return string(key)
}

func sortSearchSeries(entries []blockSeriesEntry, id uint64) int {
	i, found := slices.BinarySearchFunc(entries, id, func(entry blockSeriesEntry, id uint64) int {
		switch {
		case entry.ID < id:
			return -1
		case entry.ID > id:
			return 1
		default:
			return 0
		}
	})
	if !found {
		return -1
	}
	return i
}

// Catalog binary format (all integers little-endian):
//
// magic: uint32 = catalogBinaryMagic
// version: uint32
// numSeries: uint32
// per series: id uint64, numLabels uint32, per label: nameLen uint32, name,
//
//	valueLen uint32, value, numSegments uint32,
//	per segment: rowGroup int32, row int32, offset int32, minTime int64, maxTime int64
//
// numPostings: uint32
// per posting: nameLen uint32, name, valueLen uint32, value, numIDs uint32, ids...
// numCohorts: uint32
// per cohort: id uint64, rowGroup int32, numSegments int32, numSeries int32,
//
//	numSeriesIDs uint32, seriesIDs..., minTime int64, maxTime int64
const catalogBinaryMagic = uint32(0x43424331) // "CBC1"

func writeBlockCatalog(dir string, catalog blockCatalog) (err error) {
	f, err := os.Create(filepath.Join(dir, catalogFileName))
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 64*1024)
	w := &catalogWriter{w: bw}
	defer func() {
		if flushErr := bw.Flush(); err == nil {
			err = flushErr
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	w.u32(catalogBinaryMagic)
	w.u32(uint32(catalog.Version))
	w.u32(uint32(len(catalog.Series)))
	for _, entry := range catalog.Series {
		w.u64(entry.ID)
		// Labels are stored as a raw count of label pairs followed by
		// (name, value) string pairs; order is already sorted.
		var pairs []labels.Label
		entry.Labels.Range(func(l labels.Label) { pairs = append(pairs, l) })
		w.u32(uint32(len(pairs)))
		for _, l := range pairs {
			w.str(l.Name)
			w.str(l.Value)
		}
		w.u32(uint32(len(entry.Segments)))
		for _, seg := range entry.Segments {
			w.i32(seg.RowGroup)
			w.i32(seg.Row)
			w.i32(seg.Offset)
			w.i64(seg.MinTime)
			w.i64(seg.MaxTime)
		}
	}
	w.u32(uint32(len(catalog.Postings)))
	for _, posting := range catalog.Postings {
		w.str(posting.Name)
		w.str(posting.Value)
		w.u32(uint32(len(posting.IDs)))
		for _, id := range posting.IDs {
			w.u64(id)
		}
	}
	w.u32(uint32(len(catalog.Cohorts)))
	for _, cohort := range catalog.Cohorts {
		w.u64(cohort.ID)
		w.i32(cohort.RowGroup)
		w.i32(cohort.NumSegments)
		w.i32(cohort.NumSeries)
		w.u32(uint32(len(cohort.SeriesIDs)))
		for _, id := range cohort.SeriesIDs {
			w.u64(id)
		}
		w.i64(cohort.MinTime)
		w.i64(cohort.MaxTime)
	}
	return w.err
}

type catalogWriter struct {
	w   io.Writer
	err error
	buf [8]byte
}

func (w *catalogWriter) u32(v uint32) {
	if w.err != nil {
		return
	}
	binary.LittleEndian.PutUint32(w.buf[:4], v)
	_, w.err = w.w.Write(w.buf[:4])
}

func (w *catalogWriter) u64(v uint64) {
	if w.err != nil {
		return
	}
	binary.LittleEndian.PutUint64(w.buf[:8], v)
	_, w.err = w.w.Write(w.buf[:8])
}

func (w *catalogWriter) i32(v int) {
	if w.err != nil {
		return
	}
	binary.LittleEndian.PutUint32(w.buf[:4], uint32(int32(v)))
	_, w.err = w.w.Write(w.buf[:4])
}

func (w *catalogWriter) i64(v int64) {
	if w.err != nil {
		return
	}
	binary.LittleEndian.PutUint64(w.buf[:8], uint64(v))
	_, w.err = w.w.Write(w.buf[:8])
}

func (w *catalogWriter) str(s string) {
	w.u32(uint32(len(s)))
	if w.err != nil || s == "" {
		return
	}
	_, w.err = io.WriteString(w.w, s)
}

func readBlockCatalog(dir string, meta Meta) (blockCatalog, error) {
	f, err := os.Open(filepath.Join(dir, catalogFileName))
	if err != nil {
		return blockCatalog{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return blockCatalog{}, err
	}
	if len(data) < 12 {
		return blockCatalog{}, errors.New("short block catalog")
	}
	if binary.LittleEndian.Uint32(data[0:4]) != catalogBinaryMagic {
		return blockCatalog{}, errors.New("invalid block catalog magic")
	}
	r := &catalogReader{data: data, pos: 4}
	var catalog blockCatalog
	catalog.Version = int(r.u32())
	numSeries := r.u32()
	catalog.Series = make([]blockSeriesEntry, 0, numSeries)
	for range numSeries {
		var entry blockSeriesEntry
		entry.ID = r.u64()
		numLabels := r.u32()
		pairs := make([]labels.Label, 0, numLabels)
		for range numLabels {
			name := r.str()
			value := r.str()
			pairs = append(pairs, labels.Label{Name: name, Value: value})
		}
		entry.Labels = labels.New(pairs...)
		numSegments := r.u32()
		entry.Segments = make([]blockSegmentRef, 0, numSegments)
		for range numSegments {
			var seg blockSegmentRef
			seg.RowGroup = r.i32()
			seg.Row = r.i32()
			seg.Offset = r.i32()
			seg.MinTime = r.i64()
			seg.MaxTime = r.i64()
			entry.Segments = append(entry.Segments, seg)
		}
		catalog.Series = append(catalog.Series, entry)
	}
	numPostings := r.u32()
	catalog.Postings = make([]blockPosting, 0, numPostings)
	for range numPostings {
		var posting blockPosting
		posting.Name = r.str()
		posting.Value = r.str()
		numIDs := r.u32()
		posting.IDs = make([]uint64, 0, numIDs)
		for range numIDs {
			posting.IDs = append(posting.IDs, r.u64())
		}
		catalog.Postings = append(catalog.Postings, posting)
	}
	numCohorts := r.u32()
	catalog.Cohorts = make([]blockCohortEntry, 0, numCohorts)
	for range numCohorts {
		var cohort blockCohortEntry
		cohort.ID = r.u64()
		cohort.RowGroup = r.i32()
		cohort.NumSegments = r.i32()
		cohort.NumSeries = r.i32()
		numSeriesIDs := r.u32()
		cohort.SeriesIDs = make([]uint64, 0, numSeriesIDs)
		for range numSeriesIDs {
			cohort.SeriesIDs = append(cohort.SeriesIDs, r.u64())
		}
		cohort.MinTime = r.i64()
		cohort.MaxTime = r.i64()
		catalog.Cohorts = append(catalog.Cohorts, cohort)
	}
	if r.err != nil {
		return blockCatalog{}, r.err
	}
	if r.pos != len(data) {
		return blockCatalog{}, errors.New("trailing bytes in block catalog")
	}
	catalog.SeriesIDs = make([]uint64, len(catalog.Series))
	for i, entry := range catalog.Series {
		catalog.SeriesIDs[i] = entry.ID
	}
	return validateBlockCatalog(catalog, meta)
}

type catalogReader struct {
	data []byte
	pos  int
	err  error
}

func (r *catalogReader) u32() uint32 {
	if r.err != nil || r.pos+4 > len(r.data) {
		if r.err == nil {
			r.err = errors.New("short block catalog read")
		}
		return 0
	}
	v := binary.LittleEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return v
}

func (r *catalogReader) u64() uint64 {
	if r.err != nil || r.pos+8 > len(r.data) {
		if r.err == nil {
			r.err = errors.New("short block catalog read")
		}
		return 0
	}
	v := binary.LittleEndian.Uint64(r.data[r.pos:])
	r.pos += 8
	return v
}

func (r *catalogReader) i32() int {
	return int(int32(r.u32()))
}

func (r *catalogReader) i64() int64 {
	return int64(r.u64())
}

func (r *catalogReader) str() string {
	length := int(r.u32())
	if r.err != nil {
		return ""
	}
	if r.pos+length > len(r.data) {
		r.err = errors.New("short block catalog string")
		return ""
	}
	s := string(r.data[r.pos : r.pos+length])
	r.pos += length
	return s
}

func validateBlockCatalog(catalog blockCatalog, meta Meta) (blockCatalog, error) {
	if catalog.Version != catalogVersion {
		return blockCatalog{}, fmt.Errorf("block catalog has version %d, this build reads version %d", catalog.Version, catalogVersion)
	}
	if len(catalog.Series) != meta.NumSeries || len(catalog.Cohorts) != meta.NumRowGroups {
		return blockCatalog{}, errors.New("block catalog counts disagree with meta.json")
	}
	for i, entry := range catalog.Series {
		if id := metadataSeriesKey(entry.Labels); id != entry.ID {
			return blockCatalog{}, fmt.Errorf("series dictionary ID %d does not match labels.StableHash %d", entry.ID, id)
		}
		if i > 0 && catalog.Series[i-1].ID >= entry.ID {
			return blockCatalog{}, errors.New("series dictionary is not strictly sorted by stable ID")
		}
		for _, segment := range entry.Segments {
			if segment.RowGroup < 0 || segment.RowGroup >= len(catalog.Cohorts) {
				return blockCatalog{}, errors.New("series segment points outside the cohort catalog")
			}
			cohort := catalog.Cohorts[segment.RowGroup]
			if segment.Row < 0 || segment.Row >= cohort.NumSegments || segment.Offset < 0 || segment.Offset >= cohort.NumSeries {
				return blockCatalog{}, errors.New("series segment has invalid cohort coordinates")
			}
			if cohort.SeriesIDs[segment.Offset] != entry.ID {
				return blockCatalog{}, errors.New("cohort member offset references a different stable series ID")
			}
		}
	}
	for _, cohort := range catalog.Cohorts {
		if len(cohort.SeriesIDs) != cohort.NumSeries {
			return blockCatalog{}, errors.New("cohort member count disagrees with the series dictionary")
		}
	}
	for _, posting := range catalog.Postings {
		if !slices.IsSorted(posting.IDs) {
			return blockCatalog{}, errors.New("postings are not sorted by stable series ID")
		}
	}
	return catalog, nil
}
