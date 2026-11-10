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
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
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

// GobEncode uses an explicit slice because the default labels.Labels
// representation deliberately hides its fields.
func (e blockSeriesEntry) GobEncode() ([]byte, error) {
	var pairs []labels.Label
	e.Labels.Range(func(l labels.Label) { pairs = append(pairs, l) })
	wire := struct {
		ID       uint64
		Labels   []labels.Label
		Segments []blockSegmentRef
	}{e.ID, pairs, e.Segments}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(wire); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (e *blockSeriesEntry) GobDecode(data []byte) error {
	var wire struct {
		ID       uint64
		Labels   []labels.Label
		Segments []blockSegmentRef
	}
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&wire); err != nil {
		return err
	}
	e.ID, e.Labels, e.Segments = wire.ID, labels.New(wire.Labels...), wire.Segments
	return nil
}

type blockPosting struct {
	Name  string
	Value string
	IDs   []uint64
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
	Version  int
	Series   []blockSeriesEntry
	Postings []blockPosting
	Cohorts  []blockCohortEntry
}

func buildBlockCatalog(series []Series, opts WriteOptions) (blockCatalog, error) {
	catalog := blockCatalog{Version: catalogVersion}
	byID := make(map[uint64]int, len(series))
	postings := map[string]*blockPosting{}
	postingKey := func(name, value string) string {
		return fmt.Sprintf("%d:%s%s", len(name), name, value)
	}
	for i := range series {
		id := metadataSeriesKey(series[i].Labels)
		if previous, exists := byID[id]; exists {
			if !labels.Equal(catalog.Series[previous].Labels, series[i].Labels) {
				return blockCatalog{}, fmt.Errorf("%w: %q and %q", ErrSeriesHashCollision, catalog.Series[previous].Labels, series[i].Labels)
			}
			return blockCatalog{}, fmt.Errorf("duplicate series %q", series[i].Labels)
		}
		byID[id] = len(catalog.Series)
		catalog.Series = append(catalog.Series, blockSeriesEntry{ID: id, Labels: series[i].Labels.Copy()})
		series[i].Labels.Range(func(l labels.Label) {
			key := postingKey(l.Name, l.Value)
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

func writeBlockCatalog(dir string, catalog blockCatalog) error {
	f, err := os.Create(filepath.Join(dir, catalogFileName))
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(catalog); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readBlockCatalog(dir string, meta Meta) (blockCatalog, error) {
	f, err := os.Open(filepath.Join(dir, catalogFileName))
	if err != nil {
		return blockCatalog{}, err
	}
	defer f.Close()
	var catalog blockCatalog
	if err := gob.NewDecoder(f).Decode(&catalog); err != nil {
		return blockCatalog{}, fmt.Errorf("decoding block catalog: %w", err)
	}
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
