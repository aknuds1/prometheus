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
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

const (
	// ownerPaginationStart and ownerPaginationEnd bound every pagination query. Owner
	// version selection is inclusive at both ends.
	ownerPaginationStart int64 = 200
	ownerPaginationEnd   int64 = 500
	// ownerPaginationCandidates counts every Resource owner the fixture publishes,
	// including the two whose only version takes effect after the query end. The
	// candidate list is therefore longer than any returned page sequence, which is the
	// property a page cursor must not depend on.
	ownerPaginationCandidates = 8
	// ownerPaginationSelectedMetric and ownerPaginationOtherMetric split the fixture's
	// series so a matcher selects a subset of the owners.
	ownerPaginationSelectedMetric = "checkout_selected_total"
	ownerPaginationOtherMetric    = "checkout_other_total"
)

// ownerPaginationOwner is the Resource owner of one fixture instance.
func ownerPaginationOwner(instance string) OwnerKey {
	return ResourceOwner(ownerPaginationIdentity(instance))
}

// ownerPaginationIdentity is the identifying attribute set every view of that owner reports.
func ownerPaginationIdentity(instance string) map[string]string {
	return map[string]string{"service.name": "checkout", "service.namespace": "", "service.instance.id": instance}
}

// ownerPaginationContent is the descriptive content of one version. zone lets the
// projection case prove that projection filters content without touching owner identity.
func ownerPaginationContent(seq string) Content {
	return Content{Attributes: map[string]string{"seq": seq, "zone": "east"}}
}

// ownerPaginationVersion is one expected version view. The fixture observes each version
// from its own effective time, except where a re-observation extends that bound.
func ownerPaginationVersion(seq string, effectiveFrom int64, validUntil *int64, observedUntil int64) MetadataValueView {
	return MetadataValueView{
		Descriptive:   map[string]string{"seq": seq, "zone": "east"},
		EffectiveFrom: effectiveFrom, ValidUntil: validUntil,
		ObservedFrom: effectiveFrom, ObservedUntil: observedUntil,
	}
}

// ownerPaginationTime returns a pointer to v, which a version reports as its exclusive
// successor bound.
func ownerPaginationTime(v int64) *int64 { return &v }

// ownerPaginationView is the complete expected view of one fixture owner.
func ownerPaginationView(instance string, truncated bool, versions ...MetadataValueView) MetadataOwnerView {
	owner := ownerPaginationOwner(instance)
	return MetadataOwnerView{
		ID: owner.Ref(), Kind: MetadataResource.String(),
		Identifying:      ownerPaginationIdentity(instance),
		HistoryTruncated: truncated, Versions: versions,
	}
}

// ownerPaginationRowFor pairs one fixture owner with its complete expected view.
func ownerPaginationRowFor(instance string, truncated bool, versions ...MetadataValueView) ownerPaginationRow {
	return ownerPaginationRow{
		owner: ownerPaginationOwner(instance),
		view:  ownerPaginationView(instance, truncated, versions...),
	}
}

// ownerPaginationRow is one expected result row: the exact owner key the fixture gave the
// store and the complete view a query must report for it.
type ownerPaginationRow struct {
	owner OwnerKey
	view  MetadataOwnerView
}

// ownerPaginationPage is one expected page: its rows, the owner key of its last row, and
// the cursor it must report.
type ownerPaginationPage struct {
	owners []MetadataOwnerView
	last   OwnerKey
	token  string
}

// ownerPaginationRowsView returns the views of rows, in order.
func ownerPaginationRowsView(rows []ownerPaginationRow) []MetadataOwnerView {
	views := make([]MetadataOwnerView, len(rows))
	for i, row := range rows {
		views[i] = row.view
	}
	return views
}

// ownerPaginationRemaining returns the expected rows a query starting at an owner cursor
// selects: the existing cursor predicate excludes everything up to and including it.
func ownerPaginationRemaining(rows []ownerPaginationRow, after string) []ownerPaginationRow {
	var remaining []ownerPaginationRow
	for _, row := range rows {
		if row.owner.String() <= after {
			continue
		}
		remaining = append(remaining, row)
	}
	return remaining
}

// ownerPaginationExpectedPages slices the independent expected sequence into the pages the
// cursor rule requires: at most limit rows per page, all rows when the limit does not
// paginate, a cursor naming the last retained owner, and a cursor only while a further
// eligible owner remains.
func ownerPaginationExpectedPages(rows []ownerPaginationRow, limit int, after string) []ownerPaginationPage {
	remaining := ownerPaginationRemaining(rows, after)
	if limit <= 0 || limit >= len(remaining) {
		page := ownerPaginationPage{owners: ownerPaginationRowsView(remaining)}
		if len(remaining) > 0 {
			page.last = remaining[len(remaining)-1].owner
		}
		return []ownerPaginationPage{page}
	}
	pages := make([]ownerPaginationPage, 0, (len(remaining)+limit-1)/limit)
	for start := 0; start < len(remaining); start += limit {
		end := min(start+limit, len(remaining))
		page := ownerPaginationPage{
			owners: ownerPaginationRowsView(remaining[start:end]),
			last:   remaining[end-1].owner,
		}
		if end < len(remaining) {
			page.token = ownerCursor(page.last)
		}
		pages = append(pages, page)
	}
	return pages
}

// ownerPaginationProjectedRows projects rows to the seq attribute only, which must change
// no owner identity, no version bound and no cursor.
func ownerPaginationProjectedRows(rows []ownerPaginationRow) []ownerPaginationRow {
	projected := make([]ownerPaginationRow, len(rows))
	for i, row := range rows {
		view := row.view
		view.Versions = make([]MetadataValueView, len(row.view.Versions))
		for j, version := range row.view.Versions {
			version.Descriptive = map[string]string{"seq": version.Descriptive["seq"]}
			view.Versions[j] = version
		}
		projected[i] = ownerPaginationRow{owner: row.owner, view: view}
	}
	return projected
}

// ownerPaginationStorage opens the storage the pagination fixtures publish to.
func ownerPaginationStorage(t *testing.T) *Storage {
	t.Helper()
	store, err := NewStorage(t.TempDir(), StorageOptions{
		FlushSamples: 1 << 40, DisableWAL: true, MetadataVersionCap: 2,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

// ownerPaginationFixture is the input-derived expected state of one storage: the owners it
// published, the query range, and the exact owner views a query must report.
type ownerPaginationFixture struct {
	store      *Storage
	start, end int64
	// rows is the expected owner sequence for the query range: exactly the owners with a
	// version overlapping it, in the storage's owner order.
	rows []ownerPaginationRow
	// latestRows is rows reduced to each owner's latest applicable version.
	latestRows []ownerPaginationRow
	// matchedRows is rows restricted to the owners the fixture's matcher selects.
	matchedRows []ownerPaginationRow
}

func newOwnerPaginationFixture(t *testing.T) ownerPaginationFixture {
	t.Helper()
	store := ownerPaginationStorage(t)
	update := func(instance string, effectiveFrom int64, seq string) {
		t.Helper()
		require.NoError(t, store.MetadataUpdate(Update{
			Owner: ownerPaginationOwner(instance), EffectiveFrom: effectiveFrom,
			Content: ownerPaginationContent(seq),
		}))
	}

	// bucket-1 and bucket-4 take effect after the query end, so they are candidates with no
	// reportable version: one sorts before every returned owner, one sorts between them.
	update("bucket-1", 900, "b1")
	update("bucket-2", 200, "b2-a")
	update("bucket-2", 400, "b2-b")
	update("bucket-3", 250, "b3")
	update("bucket-4", 700, "b4")
	update("bucket-5", 300, "b5-a")
	update("bucket-5", 500, "b5-b")
	// Re-observing the content still active at 300 extends that version's own observation
	// bound to 450 without adding a change point.
	update("bucket-5", 450, "b5-a")
	update("bucket-6", 350, "b6")
	// The version cap is 2, so this owner keeps its two newest change points and reports a
	// truncated history.
	update("bucket-7", 450, "b7-a")
	update("bucket-7", 460, "b7-b")
	update("bucket-7", 470, "b7-c")
	update("bucket-8", 500, "b8")

	// One sample per series, so a matcher selects owners through real series. The metric
	// name splits the owners the fixture's matcher selects from the ones it must not.
	var batch Batch
	for _, instance := range []string{"bucket-2", "bucket-3", "bucket-4", "bucket-5", "bucket-6", "bucket-7", "bucket-8"} {
		name := ownerPaginationSelectedMetric
		if instance == "bucket-2" || instance == "bucket-5" || instance == "bucket-7" {
			name = ownerPaginationOtherMetric
		}
		batch.Series = append(batch.Series, BatchSeries{
			Labels:          labels.FromStrings(labels.MetricName, name, "instance", instance, OTELResourceLabel, ownerPaginationOwner(instance).Ref()),
			Timestamps:      []int64{300},
			StartTimestamps: []int64{0},
			Kinds:           []SampleKind{SampleFloat},
			Values:          []float64{1},
			Histograms:      [][]byte{nil},
		})
	}
	require.NoError(t, store.AppendBatch(context.Background(), batch))

	// Fixture sanity: the storage orders candidates by kind and canonical identity, which
	// for these owners is their instance name. If that order were different, every expected
	// sequence below would be meaningless.
	candidates := make([]OwnerKey, 0, ownerPaginationCandidates)
	for _, instance := range []string{"bucket-1", "bucket-2", "bucket-3", "bucket-4", "bucket-5", "bucket-6", "bucket-7", "bucket-8"} {
		candidates = append(candidates, ownerPaginationOwner(instance))
	}
	require.Len(t, candidates, ownerPaginationCandidates)
	sorted := slices.Clone(candidates)
	sortOwnerKeys(sorted)
	require.Equal(t, candidates, sorted, "the fixture's stated owner order")

	rows := []ownerPaginationRow{
		ownerPaginationRowFor("bucket-2", false,
			ownerPaginationVersion("b2-a", 200, ownerPaginationTime(400), 200),
			ownerPaginationVersion("b2-b", 400, nil, 400)),
		ownerPaginationRowFor("bucket-3", false, ownerPaginationVersion("b3", 250, nil, 250)),
		ownerPaginationRowFor("bucket-5", false,
			ownerPaginationVersion("b5-a", 300, ownerPaginationTime(500), 450),
			ownerPaginationVersion("b5-b", 500, nil, 500)),
		ownerPaginationRowFor("bucket-6", false, ownerPaginationVersion("b6", 350, nil, 350)),
		ownerPaginationRowFor("bucket-7", true,
			ownerPaginationVersion("b7-b", 460, ownerPaginationTime(470), 460),
			ownerPaginationVersion("b7-c", 470, nil, 470)),
		ownerPaginationRowFor("bucket-8", false, ownerPaginationVersion("b8", 500, nil, 500)),
	}
	return ownerPaginationFixture{
		store: store, start: ownerPaginationStart, end: ownerPaginationEnd,
		rows: rows,
		latestRows: []ownerPaginationRow{
			ownerPaginationRowFor("bucket-2", false, ownerPaginationVersion("b2-b", 400, nil, 400)),
			ownerPaginationRowFor("bucket-3", false, ownerPaginationVersion("b3", 250, nil, 250)),
			ownerPaginationRowFor("bucket-5", false, ownerPaginationVersion("b5-b", 500, nil, 500)),
			ownerPaginationRowFor("bucket-6", false, ownerPaginationVersion("b6", 350, nil, 350)),
			ownerPaginationRowFor("bucket-7", true, ownerPaginationVersion("b7-c", 470, nil, 470)),
			ownerPaginationRowFor("bucket-8", false, ownerPaginationVersion("b8", 500, nil, 500)),
		},
		matchedRows: []ownerPaginationRow{rows[1], rows[3], rows[5]},
	}
}

// query builds one pagination query against the fixture, which the caller adjusts.
func (f ownerPaginationFixture) query() MetadataQuery {
	return MetadataQuery{Kind: MetadataResource, Start: f.start, End: f.end}
}

// walkOwnerPages follows NextToken through After until it is empty, returning every page in
// order. It fails on a cursor that does not advance, on an empty page that carries a cursor,
// and on more pages than the expectation can justify, so a broken cursor cannot loop.
func walkOwnerPages(t *testing.T, store *Storage, query MetadataQuery, maxPages int) []MetadataOwnerPage {
	t.Helper()
	used := map[string]struct{}{}
	if query.After != "" {
		used[query.After] = struct{}{}
	}
	var pages []MetadataOwnerPage
	for {
		page, err := store.QueryMetadata(context.Background(), query)
		require.NoError(t, err)
		pages = append(pages, page)
		if len(pages) > maxPages {
			require.FailNowf(t, "owner pagination did not terminate", "after %d pages, cursor %q", len(pages), page.NextToken)
		}
		if page.NextToken == "" {
			return pages
		}
		require.NotEmpty(t, page.Owners, "page %d carries cursor %q but no owners", len(pages), page.NextToken)
		if _, repeated := used[page.NextToken]; repeated {
			require.FailNowf(t, "owner pagination did not advance",
				"page %d returned cursor %q, which the request or an earlier page already used", len(pages), page.NextToken)
		}
		used[page.NextToken] = struct{}{}
		query.After = page.NextToken
	}
}

// ownerPaginationCollected flattens pages into the delivered owner sequence.
func ownerPaginationCollected(pages []MetadataOwnerPage) []MetadataOwnerView {
	var owners []MetadataOwnerView
	for _, page := range pages {
		owners = append(owners, page.Owners...)
	}
	return owners
}

// ownerPaginationCursors returns the cursor of every page, including the empty final one.
func ownerPaginationCursors(pages []MetadataOwnerPage) []string {
	cursors := make([]string, len(pages))
	for i, page := range pages {
		cursors[i] = page.NextToken
	}
	return cursors
}

// ownerPaginationOwnerIDs returns the owner identities pages delivered, in order.
func ownerPaginationOwnerIDs(pages []MetadataOwnerPage) []string {
	owners := ownerPaginationCollected(pages)
	ids := make([]string, len(owners))
	for i, view := range owners {
		ids[i] = view.ID
	}
	return ids
}

// requireOwnerPaginationSequence proves the concatenated pages are exactly the expected
// sequence: the same count, no owner repeated, and every logical field unchanged.
func requireOwnerPaginationSequence(t *testing.T, want []ownerPaginationRow, got []MetadataOwnerView) {
	t.Helper()
	expected := ownerPaginationRowsView(want)
	require.Len(t, got, len(expected), "returned owner count")
	seen := make(map[string]int, len(got))
	for _, view := range got {
		seen[view.ID]++
	}
	for id, count := range seen {
		require.Equal(t, 1, count, "owner %s was returned %d times across pages", id, count)
	}
	require.Equal(t, expected, got, "every returned owner, in page order")
}

// verifyOwnerPagination runs one paginated traversal and proves it against the independent
// expectation, returning the pages.
func verifyOwnerPagination(t *testing.T, store *Storage, query MetadataQuery, rows []ownerPaginationRow) []MetadataOwnerPage {
	t.Helper()
	expected := ownerPaginationExpectedPages(rows, query.Limit, query.After)
	pages := walkOwnerPages(t, store, query, len(expected)+2)
	require.Len(t, pages, len(expected),
		"page count: a cursor must exist exactly while a further eligible owner remains")
	for i, page := range pages {
		want := expected[i]
		require.Equal(t, want.owners, page.Owners, "page %d rows", i+1)
		require.Equal(t, want.token, page.NextToken, "page %d cursor", i+1)
		if want.token != "" {
			// The only correct cursor position is the last row actually retained: never a
			// candidate-list position and never the look-ahead row.
			require.Equal(t, ownerCursor(want.last), page.NextToken, "page %d cursor names the last retained owner", i+1)
		}
		if query.Limit > 0 {
			require.LessOrEqual(t, len(page.Owners), query.Limit, "page %d rows exceed the limit", i+1)
		}
	}
	requireOwnerPaginationSequence(t, ownerPaginationRemaining(rows, query.After), ownerPaginationCollected(pages))
	return pages
}

// TestQueryMetadataOwnerPagination walks every required limit through the exported query and
// compares each page, each cursor and the whole concatenated delivery with an independent
// expectation, including owners filtered out of the page sequence but present in the
// candidate list.
func TestQueryMetadataOwnerPagination(t *testing.T) {
	f := newOwnerPaginationFixture(t)
	matcher := [][]*labels.Matcher{{labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, ownerPaginationSelectedMetric)}}
	projected := ownerPaginationProjectedRows(f.rows)

	cases := []struct {
		name  string
		rows  []ownerPaginationRow
		query MetadataQuery
	}{
		{"limit-1", f.rows, MetadataQuery{Limit: 1}},
		{"limit-2", f.rows, MetadataQuery{Limit: 2}},
		{"limit-3-two-full-pages", f.rows, MetadataQuery{Limit: 3}},
		{"limit-4-short-final-page", f.rows, MetadataQuery{Limit: 4}},
		{"limit-5-short-final-page", f.rows, MetadataQuery{Limit: 5}},
		{"limit-6-every-owner", f.rows, MetadataQuery{Limit: 6}},
		{"limit-9-more-than-owners", f.rows, MetadataQuery{Limit: 9}},
		{"latest-limit-1", f.latestRows, MetadataQuery{Limit: 1, Latest: true}},
		{"latest-limit-2", f.latestRows, MetadataQuery{Limit: 2, Latest: true}},
		{"projection-limit-2", projected, MetadataQuery{Limit: 2, Projection: []string{"seq"}}},
		{"matcher-limit-1", f.matchedRows, MetadataQuery{Limit: 1, Matchers: matcher}},
		{"matcher-limit-2", f.matchedRows, MetadataQuery{Limit: 2, Matchers: matcher}},
		{"matcher-limit-3-every-matched-owner", f.matchedRows, MetadataQuery{Limit: 3, Matchers: matcher}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := f.query()
			query.Limit, query.Latest, query.Projection, query.Matchers = tc.query.Limit, tc.query.Latest, tc.query.Projection, tc.query.Matchers
			pages := verifyOwnerPagination(t, f.store, query, tc.rows)

			// Cross-check, not the oracle: the concatenation of every page is the same
			// delivery the unlimited query returns for the same selection.
			unlimited := query
			unlimited.Limit, unlimited.After = 0, ""
			page, err := f.store.QueryMetadata(context.Background(), unlimited)
			require.NoError(t, err)
			require.Empty(t, page.NextToken, "an unlimited query returns no cursor")
			require.Equal(t, page.Owners, ownerPaginationCollected(pages), "paging delivers the unlimited result")
		})
	}
}

// TestQueryMetadataOwnerPaginationIntermediateCursor starts from an owner the fixture did
// not return, and from one it returned mid-sequence, and proves the first row, the advanced
// cursor and the absence of a repeated page.
func TestQueryMetadataOwnerPaginationIntermediateCursor(t *testing.T) {
	f := newOwnerPaginationFixture(t)
	cases := []struct {
		name  string
		after OwnerKey
		limit int
		// first is the expected first row of the traversal.
		first ownerPaginationRow
	}{
		// The filter candidate sorts before the first returned owner, so a cursor built from
		// the candidate list would restart the traversal at the first page again.
		{"after-first-returned-owner", f.rows[0].owner, 2, f.rows[1]},
		{"after-second-returned-owner", f.rows[1].owner, 1, f.rows[2]},
		// bucket-4 is a candidate with no reportable version: a cursor on it must resume at
		// the next returned owner.
		{"after-filtered-candidate", ownerPaginationOwner("bucket-4"), 1, f.rows[2]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := f.query()
			query.After, query.Limit = ownerCursor(tc.after), tc.limit
			pages := verifyOwnerPagination(t, f.store, query, f.rows)

			require.NotEmpty(t, pages[0].Owners, "the first page reports owners")
			require.Equal(t, tc.first.view, pages[0].Owners[0], "the traversal resumes at the next eligible owner")

			// The cursor names the last retained row and advances beyond the supplied
			// position, so following it cannot reproduce the first page.
			if pages[0].NextToken == "" {
				require.Len(t, pages, 1, "a traversal without a cursor is the last page")
				return
			}
			require.NotEqual(t, ownerCursor(tc.after), pages[0].NextToken, "the cursor advances past the supplied position")
			if len(pages) > 1 {
				require.NotEqual(t, pages[0].Owners, pages[1].Owners, "following the cursor does not reproduce the page")
			}
		})
	}
}

// TestQueryMetadataOwnerPaginationLastRetainedOwner pins the cursor rule itself: the cursor
// names the last retained owner, which is neither the candidate at the limit's position nor
// the look-ahead owner that proves another page exists.
func TestQueryMetadataOwnerPaginationLastRetainedOwner(t *testing.T) {
	f := newOwnerPaginationFixture(t)
	query := f.query()
	query.Limit = 2
	page, err := f.store.QueryMetadata(context.Background(), query)
	require.NoError(t, err)
	require.Len(t, page.Owners, query.Limit, "the first page is full")
	require.Equal(t, page.Owners, ownerPaginationRowsView(f.rows[:query.Limit]))

	// The candidate list is bucket-1 .. bucket-8 and bucket-1 and bucket-4 have no version in
	// range, so for a limit of 2 the three neighbouring positions are all different owners.
	lastRetained := f.rows[query.Limit-1].owner
	candidatePosition := ownerPaginationOwner("bucket-2")
	lookAhead := ownerPaginationOwner("bucket-5")
	require.Equal(t, ownerCursor(lastRetained), page.NextToken, "the cursor names the last retained owner")
	require.NotEqual(t, ownerCursor(candidatePosition), page.NextToken,
		"the cursor must not be the candidate at the limit's position")
	require.NotEqual(t, ownerCursor(lookAhead), page.NextToken,
		"the cursor must not be the look-ahead owner")

	// Following the cursor keeps the look-ahead owner and does not repeat the last retained
	// owner, which is what each neighbouring mistake would break.
	query.After = page.NextToken
	next, err := f.store.QueryMetadata(context.Background(), query)
	require.NoError(t, err)
	require.Equal(t, []MetadataOwnerView{f.rows[2].view, f.rows[3].view}, next.Owners,
		"the next page continues after the last retained owner")
}

// TestQueryMetadataOwnerPaginationEmptyAndFinal covers the settled, empty and final-page
// results, which must carry no cursor and a non-nil owner list.
func TestQueryMetadataOwnerPaginationEmptyAndFinal(t *testing.T) {
	ctx := context.Background()
	requireEmptyOwnerPage := func(t *testing.T, store *Storage, query MetadataQuery, msg string) {
		t.Helper()
		page, err := store.QueryMetadata(ctx, query)
		require.NoError(t, err)
		require.NotNil(t, page.Owners, "%s: owners must not be nil", msg)
		require.Empty(t, page.Owners, msg)
		require.Empty(t, page.NextToken, "%s: no cursor", msg)
	}

	t.Run("no owners", func(t *testing.T) {
		store := ownerPaginationStorage(t)
		for _, limit := range []int{0, 1, 5} {
			requireEmptyOwnerPage(t, store, MetadataQuery{Kind: MetadataResource, Start: 0, End: ownerPaginationEnd, Limit: limit},
				"a storage without metadata owners")
		}
	})

	t.Run("owners without a reportable version", func(t *testing.T) {
		store := ownerPaginationStorage(t)
		require.NoError(t, store.MetadataUpdate(Update{
			Owner: ownerPaginationOwner("bucket-9"), EffectiveFrom: 900, Content: ownerPaginationContent("b9"),
		}))
		for _, limit := range []int{0, 1, 2} {
			requireEmptyOwnerPage(t, store, MetadataQuery{Kind: MetadataResource, Start: 100, End: 200, Limit: limit},
				"owners whose only version takes effect after the query end")
		}
	})

	t.Run("cursor at the last eligible owner", func(t *testing.T) {
		f := newOwnerPaginationFixture(t)
		for _, limit := range []int{1, 2, 6} {
			query := f.query()
			query.After, query.Limit = ownerCursor(f.rows[len(f.rows)-1].owner), limit
			requireEmptyOwnerPage(t, f.store, query, "a cursor at the last eligible owner")
		}
	})

	t.Run("cursor past every candidate", func(t *testing.T) {
		f := newOwnerPaginationFixture(t)
		query := f.query()
		query.After, query.Limit = ownerCursor(ownerPaginationOwner("bucket-9")), 2
		requireEmptyOwnerPage(t, f.store, query, "a cursor past every candidate")
	})

	t.Run("final page", func(t *testing.T) {
		f := newOwnerPaginationFixture(t)
		query := f.query()
		query.After, query.Limit = ownerCursor(f.rows[len(f.rows)-2].owner), 6
		page, err := f.store.QueryMetadata(ctx, query)
		require.NoError(t, err)
		require.Equal(t, []MetadataOwnerView{f.rows[len(f.rows)-1].view}, page.Owners,
			"only the last eligible owner remains")
		require.Empty(t, page.NextToken, "the final page carries no cursor")
	})
}

// TestQueryMetadataOwnerPaginationUnlimitedLimits pins the current unlimited path: only a
// positive limit paginates, so zero and negative limits return every eligible owner once.
func TestQueryMetadataOwnerPaginationUnlimitedLimits(t *testing.T) {
	f := newOwnerPaginationFixture(t)
	expected := ownerPaginationRowsView(f.rows)
	for _, limit := range []int{0, -1, -100} {
		query := f.query()
		query.Limit = limit
		page, err := f.store.QueryMetadata(context.Background(), query)
		require.NoError(t, err, "limit %d", limit)
		require.Equal(t, expected, page.Owners, "limit %d returns every eligible owner", limit)
		require.Empty(t, page.NextToken, "limit %d returns no cursor", limit)
	}

	store := ownerPaginationStorage(t)
	page, err := store.QueryMetadata(context.Background(), MetadataQuery{Kind: MetadataResource, Start: 0, End: ownerPaginationEnd, Limit: 0})
	require.NoError(t, err)
	require.NotNil(t, page.Owners, "an unlimited query over no owners still reports a list")
	require.Empty(t, page.Owners)
	require.Empty(t, page.NextToken)
}

// TestQueryMetadataOwnerPaginationLatestAndProjectionKeepSequencing proves that Latest and
// Projection change only the delivered content: the owner sequence and every cursor are the
// same as the plain traversal.
func TestQueryMetadataOwnerPaginationLatestAndProjectionKeepSequencing(t *testing.T) {
	f := newOwnerPaginationFixture(t)
	const limit = 2
	plain := f.query()
	plain.Limit = limit
	plainPages := walkOwnerPages(t, f.store, plain, len(f.rows)+2)

	latest := plain
	latest.Latest = true
	latestPages := verifyOwnerPagination(t, f.store, latest, f.latestRows)
	require.Equal(t, ownerPaginationCursors(plainPages), ownerPaginationCursors(latestPages), "Latest keeps the page cursors")
	require.Equal(t, ownerPaginationOwnerIDs(plainPages), ownerPaginationOwnerIDs(latestPages), "Latest keeps the owner sequence")
	for _, view := range ownerPaginationCollected(latestPages) {
		require.Len(t, view.Versions, 1, "Latest delivers one version per owner")
	}

	projected := plain
	projected.Projection = []string{"seq"}
	projectedPages := verifyOwnerPagination(t, f.store, projected, ownerPaginationProjectedRows(f.rows))
	require.Equal(t, ownerPaginationCursors(plainPages), ownerPaginationCursors(projectedPages), "Projection keeps the page cursors")
	require.Equal(t, ownerPaginationOwnerIDs(plainPages), ownerPaginationOwnerIDs(projectedPages), "Projection keeps the owner sequence")
	// verifyOwnerPagination already compared identity, bounds and the projected content with
	// the fixture's expectation; this states the projection's own effect explicitly.
	for _, view := range ownerPaginationCollected(projectedPages) {
		for _, version := range view.Versions {
			require.Equal(t, map[string]string{"seq": version.Descriptive["seq"]}, version.Descriptive,
				"Projection keeps only the selected attribute")
		}
	}
}
