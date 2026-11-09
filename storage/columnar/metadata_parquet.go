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
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/prometheus/prometheus/model/labels"
)

type metadataTableSpec struct {
	name   string
	schema *arrow.Schema
	build  func(*array.RecordBuilder)
}

// writeMetadataRelations writes all four normalized relations before a block
// rename. A nil snapshot writes nothing and leaves the catalog entry absent.
func writeMetadataRelations(dir string, relations *metadataRelations, opts WriteOptions) error {
	if relations == nil {
		return nil
	}
	tables := []metadataTableSpec{
		{name: metadataOwnersFile, schema: metadataOwnersSchema(), build: func(builder *array.RecordBuilder) {
			ids := builder.Field(0).(*array.Uint64Builder)
			kinds := builder.Field(1).(*array.Uint8Builder)
			keys := builder.Field(2).(*array.StringBuilder)
			truncated := builder.Field(3).(*array.BooleanBuilder)
			replace := builder.Field(4).(*array.BooleanBuilder)
			for _, row := range relations.owners {
				ids.Append(row.id)
				kinds.Append(uint8(row.key.Kind))
				keys.Append(row.key.ID)
				truncated.Append(row.truncated)
				replace.Append(row.replace)
			}
		}},
		{name: metadataContentsFile, schema: metadataContentsSchema(), build: func(builder *array.RecordBuilder) {
			ids := builder.Field(0).(*array.Uint64Builder)
			digests := builder.Field(1).(*array.BinaryBuilder)
			attributes := builder.Field(2).(*array.ListBuilder)
			for _, row := range relations.contents {
				ids.Append(row.id)
				digests.Append([]byte(row.digest))
				appendMetadataAttributes(attributes, row.content.Attributes)
			}
		}},
		{name: metadataVersionsFile, schema: metadataVersionsSchema(), build: func(builder *array.RecordBuilder) {
			owners := builder.Field(0).(*array.Uint64Builder)
			contents := builder.Field(1).(*array.Uint64Builder)
			effective := builder.Field(2).(*array.Int64Builder)
			observedFrom := builder.Field(3).(*array.Int64Builder)
			observedUntil := builder.Field(4).(*array.Int64Builder)
			for _, row := range relations.versions {
				owners.Append(row.ownerID)
				contents.Append(row.contentID)
				effective.Append(row.effectiveFrom)
				observedFrom.Append(row.observedFrom)
				observedUntil.Append(row.observedUntil)
			}
		}},
		{name: metadataMembershipsFile, schema: metadataMembershipsSchema(), build: func(builder *array.RecordBuilder) {
			series := builder.Field(0).(*array.Uint64Builder)
			owners := builder.Field(1).(*array.Uint64Builder)
			effective := builder.Field(2).(*array.Int64Builder)
			observedUntil := builder.Field(3).(*array.Int64Builder)
			labelSets := builder.Field(4).(*array.ListBuilder)
			replace := builder.Field(5).(*array.BooleanBuilder)
			for _, row := range relations.memberships {
				series.Append(row.seriesHash)
				owners.Append(row.ownerID)
				effective.Append(row.effectiveFrom)
				observedUntil.Append(row.observedUntil)
				appendMetadataLabels(labelSets, row.labels)
				replace.Append(row.replace)
			}
		}},
	}
	for _, table := range tables {
		if err := writeMetadataTable(filepath.Join(dir, table.name), table.schema, table.build, opts); err != nil {
			return fmt.Errorf("writing %s: %w", table.name, err)
		}
	}
	return nil
}

func writeMetadataTable(path string, schema *arrow.Schema, build func(*array.RecordBuilder), opts WriteOptions) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	writer, err := pqarrow.NewFileWriter(
		schema,
		f,
		parquet.NewWriterProperties(parquet.WithCompression(opts.Compression)),
		pqarrow.NewArrowWriterProperties(pqarrow.WithStoreSchema()),
	)
	if err != nil {
		f.Close()
		return err
	}
	defer func() {
		if closeErr := writer.Close(); err == nil {
			err = closeErr
		}
	}()

	mem := memory.NewGoAllocator()
	builder := array.NewRecordBuilder(mem, schema)
	defer builder.Release()
	build(builder)
	record := builder.NewRecordBatch()
	defer record.Release()
	return writer.Write(record)
}

func appendMetadataAttributes(builder *array.ListBuilder, attributes map[string]string) {
	builder.Append(true)
	pairs := builder.ValueBuilder().(*array.StructBuilder)
	names := pairs.FieldBuilder(0).(*array.StringBuilder)
	values := pairs.FieldBuilder(1).(*array.StringBuilder)
	for _, name := range slices.Sorted(maps.Keys(attributes)) {
		pairs.Append(true)
		names.Append(name)
		values.Append(attributes[name])
	}
}

func appendMetadataLabels(builder *array.ListBuilder, ls labels.Labels) {
	builder.Append(true)
	pairs := builder.ValueBuilder().(*array.StructBuilder)
	names := pairs.FieldBuilder(0).(*array.StringBuilder)
	values := pairs.FieldBuilder(1).(*array.StringBuilder)
	ls.Range(func(label labels.Label) {
		pairs.Append(true)
		names.Append(label.Name)
		values.Append(label.Value)
	})
}

// readMetadataRelations reads only metadata Parquet files, not sample payloads.
// The decoded tables are validated against all owner/content references and the
// catalog before they are returned.
func (b *Block) readMetadataRelations(ctx context.Context) (metadataRelations, error) {
	var out metadataRelations
	if b.meta.Metadata == nil {
		return out, nil
	}
	tables := []struct {
		name   string
		schema *arrow.Schema
		append func(arrow.RecordBatch) error
	}{
		{name: metadataOwnersFile, schema: metadataOwnersSchema(), append: func(record arrow.RecordBatch) error {
			return appendMetadataOwnerRows(&out.owners, record)
		}},
		{name: metadataContentsFile, schema: metadataContentsSchema(), append: func(record arrow.RecordBatch) error {
			return appendMetadataContentRows(&out.contents, record)
		}},
		{name: metadataVersionsFile, schema: metadataVersionsSchema(), append: func(record arrow.RecordBatch) error {
			appendMetadataVersionRows(&out.versions, record)
			return nil
		}},
		{name: metadataMembershipsFile, schema: metadataMembershipsSchema(), append: func(record arrow.RecordBatch) error {
			return appendMetadataMembershipRows(&out.memberships, record)
		}},
	}
	for _, table := range tables {
		if err := readMetadataTable(ctx, filepath.Join(b.dir, table.name), table.schema, table.append); err != nil {
			return metadataRelations{}, fmt.Errorf("reading %s: %w", table.name, err)
		}
	}
	if err := validateMetadataRelations(out, b.meta.Metadata); err != nil {
		return metadataRelations{}, err
	}
	return out, nil
}

func readMetadataTable(ctx context.Context, path string, schema *arrow.Schema, appendRecords func(arrow.RecordBatch) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	parquetFile, err := file.NewParquetReader(f)
	if err != nil {
		return err
	}
	defer parquetFile.Close()
	reader, err := pqarrow.NewFileReader(parquetFile, pqarrow.ArrowReadProperties{}, memory.NewGoAllocator())
	if err != nil {
		return err
	}
	records, err := reader.GetRecordReader(ctx, nil, nil)
	if err != nil {
		return err
	}
	defer records.Release()
	for records.Next() {
		record := records.RecordBatch()
		if !metadataSchemaMatches(record.Schema(), schema) {
			return fmt.Errorf("unexpected Arrow schema %s", record.Schema())
		}
		if err := appendRecords(record); err != nil {
			return err
		}
	}
	return records.Err()
}

// metadataSchemaMatches ignores Parquet implementation metadata such as field
// IDs while still checking every semantic field contract.
func metadataSchemaMatches(actual, expected *arrow.Schema) bool {
	if actual.NumFields() != expected.NumFields() {
		return false
	}
	for i := range actual.NumFields() {
		a, e := actual.Field(i), expected.Field(i)
		if a.Name != e.Name || a.Nullable != e.Nullable || a.Type.Fingerprint() != e.Type.Fingerprint() {
			return false
		}
	}
	return true
}

func appendMetadataOwnerRows(out *[]metadataOwnerRow, record arrow.RecordBatch) error {
	ids := record.Column(0).(*array.Uint64)
	kinds := record.Column(1).(*array.Uint8)
	keys := record.Column(2).(*array.String)
	truncated := record.Column(3).(*array.Boolean)
	replace := record.Column(4).(*array.Boolean)
	for row := range int(record.NumRows()) {
		kind := MetadataKind(kinds.Value(row))
		if kind > MetadataSource {
			return fmt.Errorf("unknown metadata kind %d", kind)
		}
		*out = append(*out, metadataOwnerRow{
			id: ids.Value(row), key: OwnerKey{Kind: kind, ID: keys.Value(row)},
			truncated: truncated.Value(row), replace: replace.Value(row),
		})
	}
	return nil
}

func appendMetadataContentRows(out *[]metadataContentRow, record arrow.RecordBatch) error {
	ids := record.Column(0).(*array.Uint64)
	digests := record.Column(1).(*array.Binary)
	attributes := record.Column(2).(*array.List)
	pairs := attributes.ListValues().(*array.Struct)
	names := pairs.Field(0).(*array.String)
	values := pairs.Field(1).(*array.String)
	for row := range int(record.NumRows()) {
		start, end := attributes.ValueOffsets(row)
		content := Content{Attributes: make(map[string]string, end-start)}
		for i := start; i < end; i++ {
			name := names.Value(int(i))
			if _, exists := content.Attributes[name]; exists {
				return fmt.Errorf("duplicate content attribute %q", name)
			}
			content.Attributes[name] = values.Value(int(i))
		}
		*out = append(*out, metadataContentRow{id: ids.Value(row), digest: string(digests.Value(row)), content: content})
	}
	return nil
}

func appendMetadataVersionRows(out *[]metadataVersionRow, record arrow.RecordBatch) {
	owners := record.Column(0).(*array.Uint64)
	contents := record.Column(1).(*array.Uint64)
	effective := record.Column(2).(*array.Int64)
	observedFrom := record.Column(3).(*array.Int64)
	observedUntil := record.Column(4).(*array.Int64)
	for row := range int(record.NumRows()) {
		*out = append(*out, metadataVersionRow{
			ownerID:       owners.Value(row),
			contentID:     contents.Value(row),
			effectiveFrom: effective.Value(row),
			observedFrom:  observedFrom.Value(row),
			observedUntil: observedUntil.Value(row),
		})
	}
}

func appendMetadataMembershipRows(out *[]metadataMembershipRow, record arrow.RecordBatch) error {
	series := record.Column(0).(*array.Uint64)
	owners := record.Column(1).(*array.Uint64)
	effective := record.Column(2).(*array.Int64)
	observedUntil := record.Column(3).(*array.Int64)
	labelSets := record.Column(4).(*array.List)
	pairs := labelSets.ListValues().(*array.Struct)
	names := pairs.Field(0).(*array.String)
	values := pairs.Field(1).(*array.String)
	replace := record.Column(5).(*array.Boolean)
	for row := range int(record.NumRows()) {
		start, end := labelSets.ValueOffsets(row)
		labelSlice := make([]labels.Label, 0, end-start)
		seen := map[string]struct{}{}
		for i := start; i < end; i++ {
			name := names.Value(int(i))
			if _, exists := seen[name]; exists {
				return fmt.Errorf("duplicate membership label %q", name)
			}
			seen[name] = struct{}{}
			labelSlice = append(labelSlice, labels.Label{Name: name, Value: values.Value(int(i))})
		}
		*out = append(*out, metadataMembershipRow{
			seriesHash: series.Value(row), ownerID: owners.Value(row),
			effectiveFrom: effective.Value(row), observedUntil: observedUntil.Value(row),
			labels: labels.New(labelSlice...), replace: replace.Value(row),
		})
	}
	return nil
}

func validateMetadataRelations(relations metadataRelations, summary *MetadataSummary) error {
	owners := make(map[uint64]struct{}, len(relations.owners))
	for _, owner := range relations.owners {
		if _, exists := owners[owner.id]; exists {
			return fmt.Errorf("duplicate metadata owner ID %d", owner.id)
		}
		owners[owner.id] = struct{}{}
	}
	contents := make(map[uint64]struct{}, len(relations.contents))
	for _, content := range relations.contents {
		if _, exists := contents[content.id]; exists {
			return fmt.Errorf("duplicate metadata content ID %d", content.id)
		}
		if content.digest != metadataContentHash(content.content) {
			return fmt.Errorf("metadata content ID %d has an invalid hash", content.id)
		}
		contents[content.id] = struct{}{}
	}
	for _, version := range relations.versions {
		if _, ok := owners[version.ownerID]; !ok {
			return fmt.Errorf("metadata version references unknown owner ID %d", version.ownerID)
		}
		if _, ok := contents[version.contentID]; !ok {
			return fmt.Errorf("metadata version references unknown content ID %d", version.contentID)
		}
	}
	for _, membership := range relations.memberships {
		if _, ok := owners[membership.ownerID]; !ok {
			return fmt.Errorf("metadata membership references unknown owner ID %d", membership.ownerID)
		}
		if hash := metadataSeriesKey(membership.labels); hash != membership.seriesHash {
			return fmt.Errorf("metadata membership hash %d does not match labels.StableHash %d", membership.seriesHash, hash)
		}
	}
	if actual := relations.summary(); !reflect.DeepEqual(summary, actual) {
		return fmt.Errorf("metadata catalog mismatch: got %+v, decoded %+v", summary, actual)
	}
	return nil
}
