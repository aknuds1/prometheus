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
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
)

const (
	// OTELResourceLabel is the identity-bearing label for Resource-associated
	// metric series ingested over OTLP.
	// Metadata descriptive context doesn't alter this label across Resource versions.
	OTELResourceLabel = storage.ResourceIdentityLabel
)

// MetadataAssociate registers a synchronous metadata observation and series
// association. The main purpose is native metadata that is received with samples,
// before any label translation discards the identity it needs.
//
// The returned string is suitable for __otel_resource__. Other metadata kinds
// must remain their own category, not be promoted into labels as a side effect.
func (s *Storage) MetadataAssociate(ls labels.Labels, update Update) (string, error) {
	err := s.AppendBatch(context.Background(), Batch{
		Updates:              []AssociationUpdate{{Labels: ls, Update: update}},
		ordinaryAssociations: true,
	})
	if err != nil {
		return "", err
	}
	return update.Owner.Ref(), nil
}

// MetadataUpdate records an owner-scoped version without associating it with a
// series and without requiring a sample. This is useful for explicit owner
// metadata inspection; it does not claim asynchronous transport delivery.
func (s *Storage) MetadataUpdate(update Update) error {
	return s.AppendBatch(context.Background(), Batch{OwnerUpdates: []Update{update}, ordinaryAssociations: true})
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

// ingestCounts is per-transaction mechanism accounting for the ingestion path.
// It is accumulated with plain arithmetic while one transaction is prepared and
// published with one atomic add per field when a commit succeeds, so proving
// that a hash or a lookup stopped happening costs no per-sample atomics.
type ingestCounts struct {
	// seriesForHash counts label hashes taken to find or create the appender's
	// transaction series.
	seriesForHash int
	// refForHash counts label hashes taken to build the reference returned to the
	// caller.
	refForHash int
	// prepareHash counts label hashes taken to order a batch for preparation and
	// to build the prepared series, which now share one hash per series.
	prepareHash int
	// refTrusted and refFallback count appends that did and did not reuse a
	// verified reference instead of hashing.
	refTrusted  int
	refFallback int
	// native is the native OTAP builder's accounting for this transaction, carried
	// from the built batch and published with the commit.
	native NativeDerivationStats
	// ordinary is the ordinary scalar appender's metadata-derivation accounting for
	// this transaction. It is never mixed with native accounting.
	ordinary ordinaryDerivationStats
}

// add accumulates other into c.
func (c *ingestCounts) add(other ingestCounts) {
	c.seriesForHash += other.seriesForHash
	c.refForHash += other.refForHash
	c.prepareHash += other.prepareHash
	c.refTrusted += other.refTrusted
	c.refFallback += other.refFallback
	c.ordinary.add(other.ordinary)
}

// OrdinaryDerivationNote explains what the ordinary counters are for.
//
// The ordinary OTLP consumer reaches storage through AppenderV2.Append and one
// Commit, so its metadata cost is the per-sample derivation in the appender plus
// the shared change detection and history recording that consumes the resulting
// association. These counters exist to prove which of those derivations happen
// per sample and which happen per distinct series and distinct metadata context,
// so a timing change can be attributed to a mechanism rather than to a profile
// share.
//
// They are deliberately not published on ReadStats: the ordinary path's exported
// accounting surface stays as it was, and the counters are read by in-package
// tests through the storage's own accumulator.
type ordinaryDerivationStats struct {
	// Appends counts appends that carried non-empty AOptions.Metadata.
	Appends uint64
	// SeriesWithMetadata counts the transaction's series that stated metadata at
	// least once, so the distinct-series dimension of the derivation is visible.
	SeriesWithMetadata uint64
	// ContextRuns counts maximal runs of appends that stated equal metadata, so the
	// locality a per-context reuse could exploit is visible without a per-sample set.
	ContextRuns uint64
	// OwnerDerivations counts MetricOwner constructions, each of which formats the
	// complete label set.
	OwnerDerivations uint64
	// ContentBuilds counts type/unit/help attribute maps constructed.
	ContentBuilds uint64
	// Observations counts the AssociationUpdates an ordinary batch staged, which the
	// store must still apply one by one. It is counted once per commit from the
	// batch rather than per append call.
	Observations uint64
	// ChangeDetectionContentCanonicalizations counts Content.Canonical calls made to
	// decide whether the batch changes the published catalog.
	ChangeDetectionContentCanonicalizations uint64
	// ApplyContentCanonicalizations counts Content.Canonical calls made while applying
	// an association or owner update to the transaction's clone.
	ApplyContentCanonicalizations uint64
	// ChangeDetectionSeriesHashes and ApplySeriesHashes count complete-label hashes
	// taken by each phase, so a reused association identity is visible as a fall.
	ChangeDetectionSeriesHashes uint64
	ApplySeriesHashes           uint64
	// OwnerHistoryLookups counts owner histories this transaction had to resolve.
	OwnerHistoryLookups uint64
	// CompleteLabelChecks counts labels.Equal comparisons that keep a hash from
	// aliasing a series.
	CompleteLabelChecks uint64
	// MembershipSearches counts ordered searches of owner versions and series
	// memberships.
	MembershipSearches uint64
}

// add accumulates other into o.
func (o *ordinaryDerivationStats) add(other ordinaryDerivationStats) {
	o.Appends += other.Appends
	o.SeriesWithMetadata += other.SeriesWithMetadata
	o.ContextRuns += other.ContextRuns
	o.OwnerDerivations += other.OwnerDerivations
	o.ContentBuilds += other.ContentBuilds
	o.Observations += other.Observations
	o.ChangeDetectionContentCanonicalizations += other.ChangeDetectionContentCanonicalizations
	o.ApplyContentCanonicalizations += other.ApplyContentCanonicalizations
	o.ChangeDetectionSeriesHashes += other.ChangeDetectionSeriesHashes
	o.ApplySeriesHashes += other.ApplySeriesHashes
	o.OwnerHistoryLookups += other.OwnerHistoryLookups
	o.CompleteLabelChecks += other.CompleteLabelChecks
	o.MembershipSearches += other.MembershipSearches
}

// NativeDerivationAssociationSites are the call sites that derive one owner
// association per sample in the native OTAP builder, in the order they run.
const (
	// NativeDerivationResource associates the Resource owner.
	NativeDerivationResource = iota
	// NativeDerivationResourceSchema associates the Resource schema owner.
	NativeDerivationResourceSchema
	// NativeDerivationScope associates the Scope owner of a metric row that has one.
	NativeDerivationScope
	// NativeDerivationSource associates the ingestion source owner.
	NativeDerivationSource
	// NativeDerivationMetric associates the exact per-series Metric owner.
	NativeDerivationMetric
	// NativeDerivationAssociationSites is the number of association call sites.
	NativeDerivationAssociationSites
)

// NativeDerivationStats is the per-batch mechanism accounting of the native OTAP
// association builder. It exists to prove which derivations happen per sample and
// which happen per distinct context, so a timing change can be attributed to a
// mechanism rather than to a profile share.
//
// Every count is plain batch-local arithmetic while one batch is built and is
// published with one atomic add per field when that batch commits, so the builder
// adds no per-sample atomic. Counters describe committed work only.
type NativeDerivationStats struct {
	// Batches is the number of committed native batches. It is the divisor for the
	// shape counts below.
	Batches   uint64
	Samples   uint64
	Series    uint64
	Resources uint64
	// OwnersByKind is the number of distinct owners asserted in the batch, indexed
	// by MetadataKind.
	OwnersByKind [6]uint64
	// OwnerRevisionChanges counts the assertions that closed an owner/series
	// observation epoch: a changed owner content or a moved series membership.
	OwnerRevisionChanges uint64

	// PointContextCanonicalizations counts the canonical identity of one point
	// attribute set. It runs once per sample row that states point attributes,
	// because the encoder assigns a fresh attribute row to every data point even
	// when the content repeats, so the content is the only reusable identity.
	PointContextCanonicalizations uint64
	// TemplateHits and TemplateMisses count series-context derivations served from
	// the batch's reuse cache and derived for the first time. A context is either an
	// exact hit on the reference tuple or derived; there is no conservative or
	// approximate fallback, because the key names the metric row and point attribute
	// set exactly rather than a projection of the final labels.
	TemplateHits   uint64
	TemplateMisses uint64
	// SeriesLookupHashes counts the complete-label hashes taken to find or create a
	// series. It runs once per sample before the derivation reuse and once per
	// distinct context after it.
	SeriesLookupHashes uint64
	// SeriesLookupHits and SeriesLookupNew split the series lookups that reused an
	// existing series from those that created one.
	SeriesLookupHits uint64
	SeriesLookupNew  uint64
	// AssocKeyHashes counts complete-label hashes taken to form an association key,
	// indexed by NativeDerivation* site. It is the hash that a reusable per-context
	// association removes without removing the site itself: an association keyed
	// from a reused context takes no label hash of its own.
	AssocKeyHashes [NativeDerivationAssociationSites]uint64
	// ContentCanonicalizations counts Content.Canonical calls made to form an
	// association key, one per association call.
	ContentCanonicalizations uint64
	// ContentDerivations counts Content values constructed for an association, one
	// per association call.
	ContentDerivations uint64
	// SchemaContentDerivations counts the subset that constructs a schema Content
	// map for a schema URL.
	SchemaContentDerivations uint64
	// OwnerConstructors counts owner key constructions, indexed by MetadataKind.
	OwnerConstructors [6]uint64
	// OwnerReferences counts OwnerKey.Ref calls, which digest an owner identity for
	// the series' identity label.
	OwnerReferences uint64
	// LabelBuilds counts complete label sets built from a metric row and a point
	// attribute set.
	LabelBuilds uint64
	// LabelNameHits and LabelNameMisses split label-name translation cache probes.
	LabelNameHits   uint64
	LabelNameMisses uint64
	// TranslationHits and TranslationMisses split metric translation cache probes.
	TranslationHits   uint64
	TranslationMisses uint64
	// EpochOwnerChecks and EpochMembershipChecks count the ordered-fold domain
	// probes that keep a re-observation from moving an already emitted boundary.
	EpochOwnerChecks      uint64
	EpochMembershipChecks uint64
	// EmittedChangePoints counts associations appended as an epoch's change point.
	EmittedChangePoints uint64
	// EmittedObservationBounds counts the further entries a context needs to state
	// its latest observation within the same epoch.
	EmittedObservationBounds uint64
	// FoldedObservations counts observations absorbed by an open epoch.
	FoldedObservations uint64
	// EpochRestatements counts observations emitted as a new change point because a
	// content or membership assertion intervened in their domain.
	EpochRestatements uint64
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
	// counts is internal mechanism accounting carried by an in-package caller.
	counts ingestCounts
	// nativeDerivation is the native builder's mechanism accounting for this batch.
	// An in-package builder fills it; other callers leave it zero.
	nativeDerivation NativeDerivationStats
	// ordinaryAssociations marks Updates as the ordinary scalar path's metadata
	// observations, so the shared store attributes its work to the ordinary
	// counters and native batches stay untouched.
	ordinaryAssociations bool
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

	// hash carries a series hash that an in-package caller already computed or
	// verified, so preparation does not hash the same labels again. External
	// callers leave both fields zero-valued and preparation hashes the labels.
	hash      uint64
	hashValid bool
}

// AssociationUpdate relates a metadata observation to a concrete series.
// The series' label set is used to validate its StableHash association.
// These coincide with samples in the same batch for the prototype's synchronous
// metadata use cases, not an implicit "any future time" association.
type AssociationUpdate struct {
	Labels labels.Labels
	Update Update
}

// batchMetadataTransaction resolves each owner history once per batch/context,
// then reuses it for repeated Resource/schema/scope/source associations. It
// operates on the transactional clone, not the published metadata catalog.
type batchMetadataTransaction struct {
	store  *metadataStore
	owners map[OwnerKey]*metadataOwnerHistory
	// counts, when set, receives the ordinary path's call-site accounting for the
	// associations and owner updates this transaction applies.
	counts *ordinaryDerivationStats
}

func (t *batchMetadataTransaction) ownerHistory(owner OwnerKey) *metadataOwnerHistory {
	if history, ok := t.owners[owner]; ok {
		return history
	}
	if t.counts != nil {
		t.counts.OwnerHistoryLookups++
	}
	history := t.store.ownerHistory(owner)
	t.owners[owner] = history
	return history
}

func (t *batchMetadataTransaction) associate(update AssociationUpdate) error {
	history := t.ownerHistory(update.Update.Owner)
	if t.counts != nil {
		t.counts.ApplySeriesHashes++
	}
	return t.store.associateWithHistory(metadataSeriesKey(update.Labels), update.Labels, update.Update, history, t.counts)
}

func (t *batchMetadataTransaction) update(update Update) error {
	if update.Owner.Kind > MetadataSource {
		return fmt.Errorf("columnar: invalid metadata kind %d", update.Owner.Kind)
	}
	return t.store.updateHistory(t.ownerHistory(update.Owner), update, t.counts)
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

	// Prepare sorted typed arrays and validate every sample range before taking
	// the store transaction lock or publishing a WAL segment.
	prepared, counts, err := prepareBatchSeries(batch)
	if err != nil {
		return err
	}
	for i, up := range batch.Updates {
		if up.Labels.IsEmpty() {
			return fmt.Errorf("columnar: association %d has no labels", i)
		}
		if up.Update.EffectiveFrom < 0 {
			return fmt.Errorf("columnar: association %d has invalid effective timestamp %d", i, up.Update.EffectiveFrom)
		}
	}

	acquireStart := time.Now()
	s.mtx.Lock()
	if s.reads != nil {
		acquired := time.Now()
		s.reads.observeCommit(acquired.Sub(acquireStart))
		defer func() { s.reads.observeCommitHold(time.Since(acquired)) }()
	}
	defer s.mtx.Unlock()

	if s.closed {
		return errors.New("columnar: storage is closed")
	}

	// Check the complete transaction against both block tails and the head before
	// any metadata clone, WAL file, or in-memory sample becomes visible.
	for _, series := range prepared {
		if err := s.batchValidateAgainstHead(series); err != nil {
			return err
		}
	}

	// ordinaryAccounting is set only for the ordinary scalar path, so the shared
	// store never attributes a native batch's work to the ordinary counters.
	var ordinaryAccounting *ordinaryDerivationStats
	if batch.ordinaryAssociations {
		counts.ordinary.Observations += uint64(len(batch.Updates))
		ordinaryAccounting = &counts.ordinary
	}

	metadataChanged := len(batch.Updates)+len(batch.OwnerUpdates) > 0
	nextMetadata := s.metadata
	if metadataChanged {
		if s.metadata == nil {
			return ErrMetadataDisabled
		}
		// Repeated observations of unchanged content must not clone the whole
		// catalog build a transaction. This is the common steady-state case.
		if !s.metadata.batchChanges(batch, ordinaryAccounting) {
			metadataChanged = false
			nextMetadata = s.metadata
		}
	}
	if metadataChanged {
		if s.metadata == nil {
			return ErrMetadataDisabled
		}
		var err error
		nextMetadata, err = s.metadata.clone()
		if err != nil {
			return fmt.Errorf("preparing metadata transaction: %w", err)
		}
		transaction := batchMetadataTransaction{
			store: nextMetadata, owners: make(map[OwnerKey]*metadataOwnerHistory, len(batch.Updates)+len(batch.OwnerUpdates)),
			counts: ordinaryAccounting,
		}
		for i, update := range batch.Updates {
			if err := transaction.associate(update); err != nil {
				return fmt.Errorf("metadata update %d: %w", i, err)
			}
		}
		for i, update := range batch.OwnerUpdates {
			if err := transaction.update(update); err != nil {
				return fmt.Errorf("owner metadata update %d: %w", i, err)
			}
		}
	}
	if err := s.appendWALBatch(batch); err != nil {
		return err
	}
	s.metadata = nextMetadata
	s.metadataDirty = s.metadataDirty || metadataChanged
	if err := s.commitHead(prepared); err != nil {
		return err
	}
	// Publish mechanism accounting once the transaction is visible, so the
	// counters describe committed work rather than attempts.
	counts.add(batch.counts)
	counts.native = batch.nativeDerivation
	if s.reads != nil {
		s.reads.observeIngestCounts(counts)
	}
	return nil
}

func appendTypedSeries(dst *headSeries, src BatchSeries) {
	previousCount := len(dst.ts)
	// Fill absent older columns only when this batch first supplies them.
	if len(src.StartTimestamps) > 0 && len(dst.st) == 0 && previousCount > 0 {
		dst.st = make([]int64, previousCount)
	}
	if len(src.Histograms) > 0 && len(dst.hist) == 0 && previousCount > 0 {
		dst.hist = make([][]byte, previousCount)
	}
	dst.ts = append(dst.ts, src.Timestamps...)
	if len(src.StartTimestamps) > 0 {
		dst.st = append(dst.st, src.StartTimestamps...)
	} else if len(dst.st) > 0 {
		dst.st = append(dst.st, make([]int64, len(src.Timestamps))...)
	}
	dst.v = append(dst.v, src.Values...)
	dst.kind = append(dst.kind, src.Kinds...)
	if len(src.Histograms) == 0 && len(dst.hist) > 0 {
		dst.hist = append(dst.hist, make([][]byte, len(src.Timestamps))...)
	} else {
		dst.hist = append(dst.hist, src.Histograms...)
	}
}

func validateSeriesColumns(series BatchSeries) error {
	if series.Labels.IsEmpty() {
		return errors.New("columnar: series without labels")
	}
	n := len(series.Timestamps)
	if len(series.Values) != n || len(series.Kinds) != n {
		return fmt.Errorf("columnar: %d samples need values/kinds columns, got %d/%d", n, len(series.Values), len(series.Kinds))
	}
	if len(series.Histograms) != 0 && len(series.Histograms) != n {
		return fmt.Errorf("columnar: %d samples have %d histogram entries", n, len(series.Histograms))
	}
	if len(series.StartTimestamps) != 0 && len(series.StartTimestamps) != n {
		return fmt.Errorf("columnar: %d samples but %d start timestamps", n, len(series.StartTimestamps))
	}
	for i := range n {
		kind := series.Kinds[i]
		if kind == SampleStale && !value.IsStaleNaN(series.Values[i]) {
			return fmt.Errorf("columnar: stale sample at index %d does not carry the stale NaN marker", i)
		}
		switch kind {
		case SampleFloat, SampleStale:
			if len(series.Histograms) > 0 && len(series.Histograms[i]) > 0 {
				return fmt.Errorf("columnar: non-histogram kind %d at index %d carries a histogram blob", kind, i)
			}
		case SampleHistogram, SampleFloatHistogram:
			if len(series.Histograms) == 0 || len(series.Histograms[i]) == 0 {
				return fmt.Errorf("columnar: histogram sample at index %d has no payload", i)
			}
			if _, _, err := decodeSampleHistogram(series.Histograms[i]); err != nil {
				return fmt.Errorf("columnar: invalid histogram at index %d: %w", i, err)
			}
		default:
			return fmt.Errorf("columnar: invalid kind %d at index %d", kind, i)
		}
		if i > 0 && series.Timestamps[i] <= series.Timestamps[i-1] {
			return fmt.Errorf("columnar: sample index %d: %w", i, batchTimestampError(series.Timestamps[i-1], series.Timestamps[i]))
		}
	}
	return nil
}
