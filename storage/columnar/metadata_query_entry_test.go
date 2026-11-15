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
	"math"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

const (
	// metadataQueryEntryStart is the effective time of the fixture's first owner
	// version and of every series' only sample.
	metadataQueryEntryStart int64 = 100
	// metadataQueryEntrySeries is the number of series the fixture associates with
	// its one Resource owner, so every query path has a membership to report.
	metadataQueryEntrySeries = 2
	// metadataQueryEntryPublishes is the number of successful owner publications the
	// concurrent section commits while readers query the catalog.
	metadataQueryEntryPublishes = 200
	// metadataQueryEntryReaders is the number of concurrent readers per entry point.
	metadataQueryEntryReaders = 3
)

// metadataQueryEntryContent is the descriptive content version seq states.
func metadataQueryEntryContent(seq int) Content {
	return Content{Attributes: map[string]string{"region": "east", "seq": strconv.Itoa(seq)}}
}

// metadataQueryEntryEffectiveFrom is the effective time of version seq.
func metadataQueryEntryEffectiveFrom(seq int) int64 { return metadataQueryEntryStart + int64(seq) }

// metadataQueryEntrySeqValues returns the sorted seq values the first count versions
// publish.
func metadataQueryEntrySeqValues(count int) []string {
	values := make([]string, 0, count)
	for seq := range count {
		values = append(values, strconv.Itoa(seq))
	}
	slices.Sort(values)
	return values
}

// metadataQueryEntryFixture is the input-derived expected state of one storage: one
// Resource owner, metadataQueryEntrySeries associated series, one sample per series
// and one owner version per publication, in effective order.
type metadataQueryEntryFixture struct {
	store    *Storage
	owner    OwnerKey
	identity map[string]string
	series   []labels.Labels
	// versions is the complete history the fixture's inputs produce, including each
	// version's observation bounds.
	versions []Version
	// views renders the complete history the way the owner-view query entry points
	// report it.
	views []MetadataValueView
}

// newMetadataQueryEntryFixture seeds one batch of samples and associations, which is
// also version 0, and derives the expected final state without querying the store.
func newMetadataQueryEntryFixture(t *testing.T) metadataQueryEntryFixture {
	t.Helper()
	store, err := NewStorage(t.TempDir(), StorageOptions{
		FlushSamples:       1 << 40,
		DisableWAL:         true,
		MetadataVersionCap: -1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	// The identifying attributes are also the identity every owner view reports,
	// including the namespace the fixture states as empty rather than omits.
	identity := map[string]string{"service.name": "checkout", "service.namespace": "", "service.instance.id": "instance-0"}
	owner := ResourceOwner(identity)
	series := make([]labels.Labels, 0, metadataQueryEntrySeries)
	batch := Batch{}
	for i := range metadataQueryEntrySeries {
		ls := labels.FromStrings(
			labels.MetricName, "checkout_requests_total",
			"instance", "instance-"+strconv.Itoa(i),
			OTELResourceLabel, owner.Ref(),
		)
		series = append(series, ls)
		batch.Series = append(batch.Series, BatchSeries{
			Labels:          ls,
			Timestamps:      []int64{metadataQueryEntryStart},
			StartTimestamps: []int64{0},
			Kinds:           []SampleKind{SampleFloat},
			Values:          []float64{float64(i + 1)},
			Histograms:      [][]byte{nil},
		})
		batch.Updates = append(batch.Updates, AssociationUpdate{Labels: ls, Update: Update{
			Owner: owner, EffectiveFrom: metadataQueryEntryStart, Content: metadataQueryEntryContent(0),
		}})
	}
	require.NoError(t, store.AppendBatch(context.Background(), batch))

	f := metadataQueryEntryFixture{store: store, owner: owner, identity: identity, series: series}
	for seq := 0; seq <= metadataQueryEntryPublishes; seq++ {
		effectiveFrom := metadataQueryEntryEffectiveFrom(seq)
		f.versions = append(f.versions, Version{
			Content:       metadataQueryEntryContent(seq),
			EffectiveFrom: effectiveFrom, ObservedFrom: effectiveFrom, ObservedUntil: effectiveFrom,
		})
	}
	f.views = f.ownerViews()

	// The seeded state must already match the input-derived expectation for version 0,
	// so a final mismatch can only come from the concurrent section.
	history, ok := store.MetadataHistory(owner)
	require.True(t, ok)
	require.False(t, history.Truncated)
	require.Equal(t, f.versions[:1], history.Versions)
	return f
}

// lastEffectiveFrom is the query end that includes every publication.
func (metadataQueryEntryFixture) lastEffectiveFrom() int64 {
	return metadataQueryEntryEffectiveFrom(metadataQueryEntryPublishes)
}

// ownerView is the owner identity a diff query reports for the fixture's owner.
func (f metadataQueryEntryFixture) ownerView() MetadataOwnerView {
	return MetadataOwnerView{ID: f.owner.Ref(), Kind: MetadataResource.String(), Identifying: f.identity}
}

// ownerViews renders the complete history, giving each version its exclusive
// successor. The last published version has none.
func (f metadataQueryEntryFixture) ownerViews() []MetadataValueView {
	views := make([]MetadataValueView, len(f.versions))
	for i, version := range f.versions {
		views[i] = MetadataValueView{
			Descriptive:   version.Content.Clone().Attributes,
			EffectiveFrom: version.EffectiveFrom,
			ObservedFrom:  version.ObservedFrom,
			ObservedUntil: version.ObservedUntil,
		}
		if i+1 < len(f.versions) {
			next := f.versions[i+1].EffectiveFrom
			views[i].ValidUntil = &next
		}
	}
	return views
}

// checkSnapshotVersions accepts the versions one query observed: a contiguous prefix
// of the published history whose last entry is the newest version of the catalog that
// query observed, and therefore has no successor in its result.
func (f metadataQueryEntryFixture) checkSnapshotVersions(got []MetadataValueView) error {
	if len(got) == 0 {
		return errors.New("query observed no owner versions")
	}
	if len(got) > len(f.views) {
		return fmt.Errorf("query observed %d owner versions, the fixture publishes %d", len(got), len(f.views))
	}
	want := slices.Clone(f.views[:len(got)])
	want[len(want)-1].ValidUntil = nil
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("query observed owner versions %+v, want the snapshot %+v", got, want)
	}
	return nil
}

// publish commits one owner version through MetadataUpdate, which replaces the
// published catalog exactly as an ingestion commit does.
func (f metadataQueryEntryFixture) publish(seq int) error {
	if err := f.store.MetadataUpdate(Update{
		Owner: f.owner, EffectiveFrom: metadataQueryEntryEffectiveFrom(seq), Content: metadataQueryEntryContent(seq),
	}); err != nil {
		return fmt.Errorf("publishing owner version %d: %w", seq, err)
	}
	return nil
}

// metadataQueryEntryCase is one exported metadata query entry point. query performs a
// single call and reports an error unless the result is a consistent snapshot of the
// published catalog, so it can run while a publication replaces that catalog.
type metadataQueryEntryCase struct {
	name  string
	query func(context.Context, metadataQueryEntryFixture) error
}

func metadataQueryEntryCases() []metadataQueryEntryCase {
	return []metadataQueryEntryCase{
		{name: "query_metadata", query: func(ctx context.Context, f metadataQueryEntryFixture) error {
			page, err := f.store.QueryMetadata(ctx, MetadataQuery{
				Kind: MetadataResource, Start: 0, End: f.lastEffectiveFrom(),
			})
			if err != nil {
				return fmt.Errorf("QueryMetadata: %w", err)
			}
			if page.NextToken != "" {
				return fmt.Errorf("QueryMetadata returned cursor %q for an unlimited page", page.NextToken)
			}
			if len(page.Owners) != 1 {
				return fmt.Errorf("QueryMetadata returned %d owners, want 1", len(page.Owners))
			}
			got, want := page.Owners[0], f.ownerView()
			if got.ID != want.ID || got.Kind != want.Kind || got.HistoryTruncated ||
				!reflect.DeepEqual(got.Identifying, want.Identifying) {
				return fmt.Errorf("QueryMetadata owner %+v, want %+v", got, want)
			}
			return f.checkSnapshotVersions(got.Versions)
		}},
		{name: "query_metadata_series", query: func(ctx context.Context, f metadataQueryEntryFixture) error {
			page, err := f.store.QueryMetadataSeries(ctx, MetadataSeriesQuery{
				Kind: MetadataResource, Start: 0, End: f.lastEffectiveFrom(),
				Attributes: []MetadataAttributeCondition{{Name: "service.name", Value: "checkout"}},
			})
			if err != nil {
				return fmt.Errorf("QueryMetadataSeries: %w", err)
			}
			if page.NextToken != "" || len(page.Series) != len(f.series) {
				return fmt.Errorf("QueryMetadataSeries returned %d series with cursor %q, want %d series without cursor",
					len(page.Series), page.NextToken, len(f.series))
			}
			for i, got := range page.Series {
				if !labels.Equal(got.Labels, f.series[i]) {
					return fmt.Errorf("QueryMetadataSeries result %d is %s, want %s", i, got.Labels, f.series[i])
				}
				if got.Owner != f.owner.Ref() || got.Kind != MetadataResource.String() ||
					!reflect.DeepEqual(got.OwnerIdentifying, f.identity) {
					return fmt.Errorf("QueryMetadataSeries result %d reports owner %+v, want %s",
						i, got.OwnerIdentifying, f.owner.ID)
				}
				// No owner publication changes the membership the fixture's first
				// batch asserted, so its bounds are the same in every snapshot.
				if got.MembershipStart != metadataQueryEntryStart || got.MembershipUntil != nil ||
					got.MembershipObservedTo != metadataQueryEntryStart {
					return fmt.Errorf("QueryMetadataSeries result %d membership [%d,%v) observed to %d, want [%d,open) observed to %d",
						i, got.MembershipStart, got.MembershipUntil, got.MembershipObservedTo,
						metadataQueryEntryStart, metadataQueryEntryStart)
				}
				if err := f.checkSnapshotVersions(got.Versions); err != nil {
					return fmt.Errorf("QueryMetadataSeries result %d: %w", i, err)
				}
			}
			return nil
		}},
		{name: "diff_metadata", query: func(ctx context.Context, f metadataQueryEntryFixture) error {
			page, err := f.store.DiffMetadata(ctx, MetadataDiffQuery{
				Kind: MetadataResource, Time: f.lastEffectiveFrom(),
			})
			if err != nil {
				return fmt.Errorf("DiffMetadata: %w", err)
			}
			if page.NextToken != "" || len(page.Changes) != 1 {
				return fmt.Errorf("DiffMetadata returned %d changes with cursor %q, want 1 change without cursor",
					len(page.Changes), page.NextToken)
			}
			change := page.Changes[0]
			if want := f.ownerView(); change.Owner.ID != want.ID || change.Owner.Kind != want.Kind ||
				change.Owner.HistoryTruncated || !reflect.DeepEqual(change.Owner.Identifying, want.Identifying) {
				return fmt.Errorf("DiffMetadata owner %+v, want %+v", change.Owner, want)
			}
			// Every version the observed catalog holds is effective at the query time,
			// so the changed version names the snapshot the query observed.
			seq := int(change.After.EffectiveFrom - metadataQueryEntryStart)
			if seq < 0 || seq > metadataQueryEntryPublishes {
				return fmt.Errorf("DiffMetadata changed version effective from %d, want one of [%d,%d]",
					change.After.EffectiveFrom, metadataQueryEntryStart, f.lastEffectiveFrom())
			}
			wantAfter := f.views[seq]
			wantAfter.ValidUntil = nil
			if !reflect.DeepEqual(change.After, wantAfter) {
				return fmt.Errorf("DiffMetadata after %+v, want %+v", change.After, wantAfter)
			}
			wantChanged := metadataQueryEntryContent(seq).Attributes
			var wantBefore *MetadataValueView
			if seq > 0 {
				before := f.views[seq-1]
				wantBefore = &before
				wantChanged = map[string]string{"seq": strconv.Itoa(seq-1) + "->" + strconv.Itoa(seq)}
			}
			switch {
			case change.Before == nil && wantBefore != nil:
				return fmt.Errorf("DiffMetadata before is absent, want %+v", *wantBefore)
			case change.Before != nil && wantBefore == nil:
				return fmt.Errorf("DiffMetadata before %+v, want none for the first version", *change.Before)
			case change.Before != nil && !reflect.DeepEqual(*change.Before, *wantBefore):
				return fmt.Errorf("DiffMetadata before %+v, want %+v", *change.Before, *wantBefore)
			}
			if !reflect.DeepEqual(change.Changed, wantChanged) {
				return fmt.Errorf("DiffMetadata changed %v, want %v", change.Changed, wantChanged)
			}
			return nil
		}},
		{name: "discover_metadata", query: func(ctx context.Context, f metadataQueryEntryFixture) error {
			page, err := f.store.DiscoverMetadata(ctx, MetadataDiscoveryQuery{
				Kind: MetadataResource, Start: 0, End: f.lastEffectiveFrom(),
			})
			if err != nil {
				return fmt.Errorf("DiscoverMetadata: %w", err)
			}
			if page.NextToken != "" {
				return fmt.Errorf("DiscoverMetadata returned cursor %q for an unlimited page", page.NextToken)
			}
			// Each version states both names, and no publication removes a value, so
			// the reported seq values name the snapshot the query observed.
			if len(page.Attributes) != 2 || page.Attributes[0].Name != "region" || page.Attributes[1].Name != "seq" {
				return fmt.Errorf("DiscoverMetadata reported %+v, want the region and seq attributes", page.Attributes)
			}
			if !reflect.DeepEqual(page.Attributes[0].Values, []string{"east"}) {
				return fmt.Errorf("DiscoverMetadata region values %v, want [east]", page.Attributes[0].Values)
			}
			count := len(page.Attributes[1].Values)
			if count == 0 || count > metadataQueryEntryPublishes+1 {
				return fmt.Errorf("DiscoverMetadata reported %d seq values, want 1..%d",
					count, metadataQueryEntryPublishes+1)
			}
			if want := metadataQueryEntrySeqValues(count); !reflect.DeepEqual(page.Attributes[1].Values, want) {
				return fmt.Errorf("DiscoverMetadata seq values %v, want %v", page.Attributes[1].Values, want)
			}
			return nil
		}},
	}
}

// TestMetadataQueryEntryConcurrentPublication queries through every exported metadata
// query entry point while real metadata publications replace the catalog those entry
// points read. Run under the race detector it proves each entry point's first read of
// Storage.metadata is synchronized with the publication that replaces it, and it proves
// every result is one complete committed catalog rather than a partly prepared one.
func TestMetadataQueryEntryConcurrentPublication(t *testing.T) {
	for _, entry := range metadataQueryEntryCases() {
		t.Run(entry.name, func(t *testing.T) {
			f := newMetadataQueryEntryFixture(t)
			ctx := context.Background()

			// Readers and the publisher wait for the same barrier, so the concurrent
			// section cannot start before both sides are running.
			start := make(chan struct{})
			published := make(chan struct{})
			errs := make(chan error, 1+metadataQueryEntryReaders)
			report := func(err error) {
				select {
				case errs <- err:
				default:
				}
			}
			// Each reader owns one counter, and readers.Wait orders the final read.
			counts := make([]int, metadataQueryEntryReaders)
			var readers, publisher sync.WaitGroup
			for i := range metadataQueryEntryReaders {
				readers.Go(func() {
					<-start
					for {
						select {
						case <-published:
							return
						default:
						}
						if err := entry.query(ctx, f); err != nil {
							report(err)
							return
						}
						counts[i]++
					}
				})
			}
			publisher.Go(func() {
				<-start
				defer close(published)
				for seq := 1; seq <= metadataQueryEntryPublishes; seq++ {
					if err := f.publish(seq); err != nil {
						report(err)
						return
					}
				}
			})
			close(start)
			publisher.Wait()
			readers.Wait()
			close(errs)
			for err := range errs {
				require.NoError(t, err, "%s while metadata was published concurrently", entry.name)
			}
			queries := 0
			for _, count := range counts {
				queries += count
			}
			require.Positive(t, queries, "the %s entry point never ran", entry.name)
			t.Logf("%s: %d concurrent queries against %d publications", entry.name, queries, metadataQueryEntryPublishes)

			verifyMetadataQueryEntryState(t, f)
		})
	}
}

// verifyMetadataQueryEntryState compares the settled catalog with the input-derived
// expectation on samples, owner histories, memberships and observation bounds.
func verifyMetadataQueryEntryState(t *testing.T, f metadataQueryEntryFixture) {
	t.Helper()
	ctx := context.Background()

	series, err := f.store.SelectSeries(ctx, 0, math.MaxInt64, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, len(f.series), "sample series")
	for i, got := range series {
		require.Equal(t, f.series[i], got.Labels, "sample series %d labels", i)
		require.Equal(t, []int64{metadataQueryEntryStart}, got.Timestamps, "sample series %d timestamps", i)
		require.Equal(t, []float64{float64(i + 1)}, got.Values, "sample series %d values", i)
		require.Equal(t, []SampleKind{SampleFloat}, got.Kinds, "sample series %d kinds", i)
	}

	history, ok := f.store.MetadataHistory(f.owner)
	require.True(t, ok, "owner history")
	require.Equal(t, OwnerHistory{Owner: f.owner, Versions: f.versions}, history, "owner history and observation bounds")

	for _, ls := range f.series {
		owner, ok := f.store.SeriesMetadataAssociation(metadataSeriesKey(ls), MetadataResource, metadataQueryEntryStart)
		require.True(t, ok, "membership of %s", ls)
		require.Equal(t, f.owner, owner, "membership of %s", ls)
	}

	owners, err := f.store.QueryMetadata(ctx, MetadataQuery{Kind: MetadataResource, Start: 0, End: f.lastEffectiveFrom()})
	require.NoError(t, err)
	require.Equal(t, []MetadataOwnerView{{
		ID: f.owner.Ref(), Kind: MetadataResource.String(), Identifying: f.identity, Versions: f.views,
	}}, owners.Owners, "settled owner page")

	memberships, err := f.store.QueryMetadataSeries(ctx, MetadataSeriesQuery{
		Kind: MetadataResource, Start: 0, End: f.lastEffectiveFrom(),
		Attributes: []MetadataAttributeCondition{{Name: "service.name", Value: "checkout"}},
	})
	require.NoError(t, err)
	require.Len(t, memberships.Series, len(f.series), "settled membership page")
	for i, got := range memberships.Series {
		require.Equal(t, MetadataSeriesResult{
			Labels: f.series[i], Owner: f.owner.Ref(), Kind: MetadataResource.String(),
			OwnerIdentifying: f.identity,
			MembershipStart:  metadataQueryEntryStart, MembershipObservedTo: metadataQueryEntryStart,
			Versions: f.views,
		}, got, "settled membership result %d", i)
	}

	diff, err := f.store.DiffMetadata(ctx, MetadataDiffQuery{Kind: MetadataResource, Time: f.lastEffectiveFrom()})
	require.NoError(t, err)
	require.Len(t, diff.Changes, 1, "settled diff page")
	change := diff.Changes[0]
	last := len(f.views) - 1
	require.Equal(t, f.ownerView(), change.Owner, "settled diff owner")
	require.Equal(t, f.views[last], change.After, "settled diff after")
	require.NotNil(t, change.Before, "settled diff before")
	require.Equal(t, f.views[last-1], *change.Before, "settled diff before")
	require.Equal(t, map[string]string{"seq": strconv.Itoa(metadataQueryEntryPublishes-1) + "->" + strconv.Itoa(metadataQueryEntryPublishes)},
		change.Changed, "settled diff changed")

	discovery, err := f.store.DiscoverMetadata(ctx, MetadataDiscoveryQuery{Kind: MetadataResource, Start: 0, End: f.lastEffectiveFrom()})
	require.NoError(t, err)
	require.Equal(t, []MetadataNameValues{
		{Name: "region", Values: []string{"east"}},
		{Name: "seq", Values: metadataQueryEntrySeqValues(metadataQueryEntryPublishes + 1)},
	}, discovery.Attributes, "settled discovery page")
}

// TestMetadataQueryEntryErrorPrecedence pins the errors every metadata query entry
// point reports before it reads the catalog: a disabled store is reported before any
// input or context error, and an invalid range is reported before a cancelled context.
func TestMetadataQueryEntryErrorPrecedence(t *testing.T) {
	type entryCase struct {
		name string
		// valid performs one well-formed query.
		valid func(context.Context, *Storage) error
		// invalid performs one query whose start is after its end, which DiffMetadata
		// has no equivalent of because it states a single time.
		invalid func(context.Context, *Storage) error
	}
	entries := []entryCase{
		{
			name: "query_metadata",
			valid: func(ctx context.Context, st *Storage) error {
				_, err := st.QueryMetadata(ctx, MetadataQuery{Kind: MetadataResource, Start: 0, End: math.MaxInt64})
				return err
			},
			invalid: func(ctx context.Context, st *Storage) error {
				page, err := st.QueryMetadata(ctx, MetadataQuery{Kind: MetadataResource, Start: 10, End: 0})
				require.Empty(t, page.Owners, "an invalid range returns no owners")
				return err
			},
		},
		{
			name: "query_metadata_series",
			valid: func(ctx context.Context, st *Storage) error {
				_, err := st.QueryMetadataSeries(ctx, MetadataSeriesQuery{
					Kind: MetadataResource, Start: 0, End: math.MaxInt64,
					Attributes: []MetadataAttributeCondition{{Name: "service.name", Value: "checkout"}},
				})
				return err
			},
			invalid: func(ctx context.Context, st *Storage) error {
				page, err := st.QueryMetadataSeries(ctx, MetadataSeriesQuery{
					Kind: MetadataResource, Start: 10, End: 0,
					Attributes: []MetadataAttributeCondition{{Name: "service.name", Value: "checkout"}},
				})
				require.Empty(t, page.Series, "an invalid range returns no series")
				return err
			},
		},
		{
			name: "diff_metadata",
			valid: func(ctx context.Context, st *Storage) error {
				_, err := st.DiffMetadata(ctx, MetadataDiffQuery{Kind: MetadataResource, Time: metadataQueryEntryStart})
				return err
			},
		},
		{
			name: "discover_metadata",
			valid: func(ctx context.Context, st *Storage) error {
				_, err := st.DiscoverMetadata(ctx, MetadataDiscoveryQuery{Kind: MetadataResource, Start: 0, End: math.MaxInt64})
				return err
			},
			invalid: func(ctx context.Context, st *Storage) error {
				page, err := st.DiscoverMetadata(ctx, MetadataDiscoveryQuery{Kind: MetadataResource, Start: 10, End: 0})
				require.Empty(t, page.Attributes, "an invalid range returns no attributes")
				return err
			},
		},
	}

	disabled, err := NewStorage(t.TempDir(), StorageOptions{DisableNativeMetadata: true, DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, disabled.Close()) })
	enabled := newMetadataQueryEntryFixture(t).store
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			require.ErrorIs(t, entry.valid(context.Background(), disabled), ErrMetadataDisabled,
				"a disabled store rejects a well-formed query")
			require.ErrorIs(t, entry.valid(cancelled, disabled), ErrMetadataDisabled,
				"a disabled store is reported before a cancelled context")
			if entry.invalid != nil {
				require.ErrorIs(t, entry.invalid(context.Background(), disabled), ErrMetadataDisabled,
					"a disabled store is reported before an invalid range")
			}

			require.ErrorIs(t, entry.valid(cancelled, enabled), context.Canceled,
				"an enabled store reports a cancelled context")
			if entry.invalid != nil {
				err := entry.invalid(cancelled, enabled)
				require.NotErrorIs(t, err, context.Canceled, "an invalid range is reported before a cancelled context")
				require.NotErrorIs(t, err, ErrMetadataDisabled)
				require.ErrorContains(t, err, "is after end", "an invalid range is reported as such")
			}

			require.NoError(t, entry.valid(context.Background(), enabled), "a well-formed query on an enabled store succeeds")
		})
	}
}
