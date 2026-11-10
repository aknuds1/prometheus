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
	"errors"
	"fmt"
	"os"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/prometheus/prometheus/model/labels"
)

func readWALSegment(path string, sequence uint64) (Batch, error) {
	file, err := os.Open(path)
	if err != nil {
		return Batch{}, err
	}
	defer file.Close()
	reader, err := ipc.NewReader(file, ipc.WithAllocator(memory.NewGoAllocator()))
	if err != nil {
		return Batch{}, err
	}
	defer reader.Release()
	if !reader.Next() {
		if err := reader.Err(); err != nil {
			return Batch{}, err
		}
		return Batch{}, errors.New("empty Arrow IPC segment")
	}
	batch, err := decodeWALRecord(reader.RecordBatch(), sequence)
	if err != nil {
		return Batch{}, err
	}
	if reader.Next() {
		return Batch{}, errors.New("WAL segment contains more than one batch")
	}
	if err := reader.Err(); err != nil {
		return Batch{}, err
	}
	return batch, nil
}

func decodeWALRecord(record arrow.RecordBatch, sequence uint64) (Batch, error) {
	if !metadataSchemaMatches(record.Schema(), walSchema) {
		return Batch{}, fmt.Errorf("unexpected WAL schema %s", record.Schema())
	}
	sequences := record.Column(0).(*array.Uint64)
	entryKinds := record.Column(1).(*array.Uint8)
	labelsColumn := record.Column(2).(*array.List)
	timestampsColumn := record.Column(3).(*array.List)
	startTimestampsColumn := record.Column(4).(*array.List)
	kindsColumn := record.Column(5).(*array.List)
	valuesColumn := record.Column(6).(*array.List)
	histogramsColumn := record.Column(7).(*array.List)
	ownerKinds := record.Column(8).(*array.Uint8)
	ownerKeys := record.Column(9).(*array.String)
	attributesColumn := record.Column(10).(*array.List)
	effectiveFromColumn := record.Column(11).(*array.Int64)

	var batch Batch
	for row := range int(record.NumRows()) {
		if sequences.Value(row) != sequence {
			return Batch{}, fmt.Errorf("row sequence %d does not match segment sequence %d", sequences.Value(row), sequence)
		}
		ls, err := decodeWALLabels(labelsColumn, row)
		if err != nil {
			return Batch{}, err
		}
		switch entryKinds.Value(row) {
		case walSamples:
			series := BatchSeries{Labels: ls}
			if err := decodeWALSamples(row,
				timestampsColumn, startTimestampsColumn, kindsColumn, valuesColumn, histogramsColumn, &series); err != nil {
				return Batch{}, err
			}
			batch.Series = append(batch.Series, series)
		case walAssociation, walOwnerVersion:
			kind := MetadataKind(ownerKinds.Value(row))
			if kind > MetadataSource {
				return Batch{}, fmt.Errorf("unknown owner kind %d", kind)
			}
			attributes, err := decodeWALAttributes(attributesColumn, row)
			if err != nil {
				return Batch{}, err
			}
			update := Update{
				Owner: OwnerKey{Kind: kind, ID: ownerKeys.Value(row)}, Content: Content{Attributes: attributes},
				EffectiveFrom: effectiveFromColumn.Value(row),
			}
			if entryKinds.Value(row) == walOwnerVersion {
				batch.OwnerUpdates = append(batch.OwnerUpdates, update)
			} else {
				batch.Updates = append(batch.Updates, AssociationUpdate{Labels: ls, Update: update})
			}
		default:
			return Batch{}, fmt.Errorf("unknown WAL entry kind %d", entryKinds.Value(row))
		}
	}
	return batch, nil
}

func decodeWALLabels(column *array.List, row int) (labels.Labels, error) {
	attributes, err := decodeWALPairs(column, row)
	if err != nil {
		return labels.EmptyLabels(), err
	}
	return labels.FromMap(attributes), nil
}

func decodeWALAttributes(column *array.List, row int) (map[string]string, error) {
	return decodeWALPairs(column, row)
}

func decodeWALPairs(column *array.List, row int) (map[string]string, error) {
	pairs := column.ListValues().(*array.Struct)
	names := pairs.Field(0).(*array.String)
	values := pairs.Field(1).(*array.String)
	start, end := column.ValueOffsets(row)
	out := make(map[string]string, end-start)
	for i := start; i < end; i++ {
		name := names.Value(int(i))
		if _, exists := out[name]; exists {
			return nil, fmt.Errorf("duplicate label/attribute %q", name)
		}
		out[name] = values.Value(int(i))
	}
	return out, nil
}

func decodeWALSamples(
	row int,
	timestampsColumn, startTimestampsColumn, kindsColumn, valuesColumn, histogramsColumn *array.List,
	out *BatchSeries,
) error {
	timestamps := timestampsColumn.ListValues().(*array.Int64)
	startTimestamps := startTimestampsColumn.ListValues().(*array.Int64)
	kinds := kindsColumn.ListValues().(*array.Int8)
	values := valuesColumn.ListValues().(*array.Float64)
	histograms := histogramsColumn.ListValues().(*array.Binary)
	timestampStart, timestampEnd := timestampsColumn.ValueOffsets(row)
	startStart, startEnd := startTimestampsColumn.ValueOffsets(row)
	kindStart, _ := kindsColumn.ValueOffsets(row)
	valueStart, _ := valuesColumn.ValueOffsets(row)
	histogramStart, histogramEnd := histogramsColumn.ValueOffsets(row)
	for i := timestampStart; i < timestampEnd; i++ {
		index := int(i - timestampStart)
		out.Timestamps = append(out.Timestamps, timestamps.Value(int(i)))
		if startEnd > startStart {
			out.StartTimestamps = append(out.StartTimestamps, startTimestamps.Value(int(startStart)+index))
		}
		out.Kinds = append(out.Kinds, SampleKind(kinds.Value(int(kindStart)+index)))
		out.Values = append(out.Values, values.Value(int(valueStart)+index))
		if histogramEnd > histogramStart {
			var payload []byte
			if value := histograms.Value(int(histogramStart) + index); len(value) > 0 {
				payload = append([]byte(nil), value...)
			}
			out.Histograms = append(out.Histograms, payload)
		}
	}
	return nil
}
