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
	"errors"
	"fmt"

	"github.com/prometheus/prometheus/model/labels"
)

const (
	// OTELResourceLabel is the identity-bearing label for Resource-associated
	// metric series ingested over OTLP.
	// Metadata descriptive context doesn't alter this label across Resource versions.
	OTELResourceLabel = "__otel_resource__"
)

// MetadataAssociate registers a synchronous metadata observation and series
// association. The main purpose is native metadata that is received with samples,
// before any label translation discards the identity it needs.
//
// The returned string is suitable for __otel_resource__. Other metadata kinds
// must remain their own category, not be promoted into labels as a side effect.
func (s *Storage) MetadataAssociate(ls labels.Labels, update Update) (string, error) {
	err := s.AppendBatch(context.Background(), Batch{Updates: []AssociationUpdate{{Labels: ls, Update: update}}})
	if err != nil {
		return "", err
	}
	return update.Owner.Ref(), nil
}

// MetadataUpdate records an owner-scoped version without associating it with a
// series and without requiring a sample. This is useful for explicit owner
// metadata inspection; it does not claim asynchronous transport delivery.
func (s *Storage) MetadataUpdate(update Update) error {
	return s.AppendBatch(context.Background(), Batch{OwnerUpdates: []Update{update}})
}

// MetadataAtOwner returns the owner version effective at t without traversing a
// series membership. The booleans report value presence and history truncation.
func (s *Storage) MetadataAtOwner(owner OwnerKey, t int64) (Content, bool, bool) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.metadata == nil {
		return Content{}, false, false
	}
	return s.metadata.at(owner, t)
}

// MetadataAt resolves series→owner→version at timestamp t. seriesHash must be
// the series' labels.StableHash, matching storage.ResourceQuerier.
//
// The boolean flags mean "has value" and "history was intentionally truncated",
// respectively. The returned content is copied and can be kept by the caller.
func (s *Storage) MetadataAt(seriesHash uint64, kind MetadataKind, t int64) (Content, bool, bool) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.metadata == nil {
		return Content{}, false, false
	}
	return s.metadata.getAt(seriesHash, kind, t)
}

// MetadataAtLabels is the label-keyed equivalent of MetadataAt.
func (s *Storage) MetadataAtLabels(ls labels.Labels, kind MetadataKind, t int64) (Content, bool, bool) {
	return s.MetadataAt(metadataSeriesKey(ls), kind, t)
}

// MetadataHistory returns the owner's variable history with its Truncated bit.
func (s *Storage) MetadataHistory(owner OwnerKey) (OwnerHistory, bool) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.metadata == nil {
		return OwnerHistory{}, false
	}
	return s.metadata.history(owner)
}

// SeriesMetadataAssociation reports the owner for a series, kind and timestamp.
// seriesHash is the labels.StableHash of the series' labels. It does not
// enumerate samples; it is a membership query.
func (s *Storage) SeriesMetadataAssociation(seriesHash uint64, kind MetadataKind, at int64) (OwnerKey, bool) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.metadata == nil {
		return OwnerKey{}, false
	}
	return s.metadata.ownersAt(seriesHash, kind, at)
}

// Batch is an ingestion batch of typed per-series columns and metadata updates.
//
// Resource/context association isn't reconstructed from promoted labels later;
// each update carries the original owner. Callers that supply both samples and
// metadata in one batch get a single transaction/seal path, instead of converting
// metadata to extra samples (or to copies owned by each series).
type Batch struct {
	Series  []BatchSeries
	Updates []AssociationUpdate
	// OwnerUpdates carry owner versions without series memberships. They are
	// logged and replayed independently of samples and associations.
	OwnerUpdates []Update
}

// BatchSeries is one series and its sample columns.
// Labels must include OTELResourceLabel for Resource-associated series; samples
// are strictly timed within their own series. An entire batch must also be
// applied in time order per series.
type BatchSeries struct {
	Labels labels.Labels
	// Timestamps are sample times in milliseconds.
	Timestamps      []int64
	StartTimestamps []int64 // 0 = unknown; nil means the entire column is unknown
	Kinds           []SampleKind
	Values          []float64
	Histograms      [][]byte
}

// AssociationUpdate relates a metadata observation to a concrete series.
// The series' label set is used to validate its StableHash association.
// These coincide with samples in the same batch for the prototype's synchronous
// metadata use cases, not an implicit "any future time" association.
type AssociationUpdate struct {
	Labels labels.Labels
	Update Update
}

// AppendBatch validates and appends an entire batch in one atomic store
// transaction. A label/metadata error doesn't produce a half-written batch.
//
// It is the columnar-path append: samples come in typed per-series columns, and
// the series dictionary is built before block sealing. AppenderV2 remains as a
// compatibility adapter, including for conformance and scrape workloads.
func (s *Storage) AppendBatch(ctx context.Context, batch Batch) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Prepare/validate before the transaction, not while mutating.
	for _, series := range batch.Series {
		if err := validateSeriesColumns(series); err != nil {
			return err
		}
	}
	for i, up := range batch.Updates {
		if up.Labels.IsEmpty() {
			return fmt.Errorf("columnar: association %d has no labels", i)
		}
		if up.Update.EffectiveFrom < 0 {
			return fmt.Errorf("columnar: association %d has invalid effective timestamp %d", i, up.Update.EffectiveFrom)
		}
	}

	s.mtx.Lock()
	defer s.mtx.Unlock()

	if s.closed {
		return errors.New("columnar: storage is closed")
	}

	// Build the complete transaction first; a failure disk flush or member update
	// doesn't commit the prefix that happened to pass validation.
	for _, series := range batch.Series {
		if err := s.batchValidateAgainstHead(series); err != nil {
			return err
		}
	}

	txs := map[uint64]*headSeries{}
	order := make([]uint64, 0, len(batch.Series))
	for _, series := range batch.Series {
		if len(series.Timestamps) == 0 {
			continue
		}
		hash := stableSeriesHash(series.Labels)
		hs, ok := txs[hash]
		if !ok {
			hs = newHeadSeries(series.Labels)
			txs[hash] = hs
			order = append(order, hash)
		} else if !labels.Equal(hs.labels, series.Labels) {
			return ErrSeriesHashCollision
		}
		// Nothing new series is merged by StableHash without checking the actual
		// labels, preventing association collapse even in compressed ID maps.
		appendTypedSeries(hs, series)
	}

	metadataChanged := len(batch.Updates)+len(batch.OwnerUpdates) > 0
	nextMetadata := s.metadata
	if metadataChanged {
		if s.metadata == nil {
			return ErrMetadataDisabled
		}
		var err error
		nextMetadata, err = s.metadata.clone()
		if err != nil {
			return fmt.Errorf("preparing metadata transaction: %w", err)
		}
		for i, update := range batch.Updates {
			if err := nextMetadata.associate(metadataSeriesKey(update.Labels), update.Labels, update.Update); err != nil {
				return fmt.Errorf("metadata update %d: %w", i, err)
			}
		}
		for i, update := range batch.OwnerUpdates {
			if err := nextMetadata.update(update); err != nil {
				return fmt.Errorf("owner metadata update %d: %w", i, err)
			}
		}
	}
	if err := s.appendWALBatch(batch); err != nil {
		return err
	}
	s.metadata = nextMetadata
	s.metadataDirty = s.metadataDirty || metadataChanged
	return s.commitHead(txs, order)
}

// headSeries is a typed columnar head series. native histograms store the encoded
// payload per sample; no per-row interface walks the samples when writing.
type BSERIES_DO_NOT_USE struct {
	labels labels.Labels
	ts     []int64
	st     []int64
	v      []float64
	kind   []SampleKind
	hist   [][]byte
}

func appendTypedSeries(dst *headSeries, src BatchSeries) {
	dst.ts = append(dst.ts, src.Timestamps...)
	if len(src.StartTimestamps) > 0 {
		dst.st = append(dst.st, src.StartTimestamps...)
	} else {
		dst.st = append(dst.st, make([]int64, len(src.Timestamps))...)
	}
	dst.v = append(dst.v, src.Values...)
	dst.kind = append(dst.kind, src.Kinds...)
	dst.hist = append(dst.hist, src.Histograms...)
}

func validateSeriesColumns(series BatchSeries) error {
	if series.Labels.IsEmpty() {
		return errors.New("columnar: series without labels")
	}
	n := len(series.Timestamps)
	if len(series.Values) != n || len(series.Kinds) != n || len(series.Histograms) != n {
		return fmt.Errorf("columnar: %d samples need values/kinds/histograms columns, got %d/%d/%d", n, len(series.Values), len(series.Kinds), len(series.Histograms))
	}
	if len(series.StartTimestamps) != 0 && len(series.StartTimestamps) != n {
		return fmt.Errorf("columnar: %d samples but %d start timestamps", n, len(series.StartTimestamps))
	}
	for i := range n {
		kind := series.Kinds[i]
		switch kind {
		case SampleFloat, SampleStale:
			if len(series.Histograms[i]) != 0 {
				return fmt.Errorf("columnar: non-histogram kind %d at index %d carries a histogram payload", kind, i)
			}
		case SampleHistogram, SampleFloatHistogram:
			if len(series.Histograms[i]) == 0 {
				return fmt.Errorf("columnar: histogram sample at index %d has no payload", i)
			}
			if _, _, err := decodeSampleHistogram(series.Histograms[i]); err != nil {
				return fmt.Errorf("columnar: invalid histogram at index %d: %w", i, err)
			}
		default:
			return fmt.Errorf("columnar: invalid kind %d at index %d", kind, i)
		}
		if i > 0 && series.Timestamps[i] <= series.Timestamps[i-1] {
			return fmt.Errorf("columnar: non-increasing timestamp at index %d", i)
		}
	}
	return nil
}
