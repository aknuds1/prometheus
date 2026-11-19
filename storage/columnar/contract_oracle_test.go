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

// This file is the architecture-neutral differential-oracle foundation. It is
// test-only: no production path is added, changed or optimised here.
//
// A fixture states ordered logical events and transaction boundaries and is
// independent of how any store implements them: exact labels, metadata content,
// effective and observation times, sample bits, kinds, start timestamps,
// complete histogram payloads, requested queries, lifecycle operations and
// transactional failures. Each observation event carries its own hand-authored
// expectation, and the final `state` block is a complete expectation for the
// retained series and metadata histories.
//
// Metadata content values are strings because that is the only value type the
// incumbent can represent. Typed content (booleans, integers, doubles, arrays,
// maps, null) is a recorded capability gap, not a decision that strings are the
// target representation.
//
// Decision 3 distinguishes present, typed null, absent, explicitly cleared,
// unknown, truncated and coalesced states. The incumbent can execute only the
// string-present, observation-bound and explicit-truncation parts; the typed,
// clear, termination, partial-source and state-specific read clauses are
// recorded as target cases owned by non-passing obligations.
//
// Decision 4 decides Kubernetes acquisition, identity and reconciliation:
// collector-supplied association is sufficient, plain scrape selects no
// built-in resolver, authoritative object identity comes from supplied UIDs,
// and partial descriptions produce a combined context view rather than a
// merged Resource. The incumbent executes only explicit association storage and
// lifecycle retention; supplied Entity grouping, provenance, authoritative UID
// identity, unresolved relations and the combined view are target cases owned
// by non-passing obligations.
//
// Decision 5 decides Source and provenance: producer, sender and final
// transport are distinct roles over a dedicated non-identifying, trusted
// channel; one typed provenance record per series and validity time carries a
// coverage scope, completeness and canonical components; partial observations
// cannot remove, complete scoped records replace explicitly, and conflicting
// producers stay ambiguous. The incumbent stores one opaque SourceOwner string
// on the native OTAP route and stamps it at sample timestamps, so the Source
// fixtures below execute only equal-observation idempotence, same-time conflict
// rejection, retention/replay and zero sample-payload reads. The typed roles,
// components, completeness, trust and receipt-time semantics are target cases
// owned by non-passing obligations, and the sample-time stamping is recorded as
// a defect.
//
// Decision 6 accepts native metadata references (~ and ~~), category-exact
// namespaces, typed literals, metadata_state, promotion, selector-local aliases,
// temporal band resolution, aggregation rejection, merge ambiguity and stable-band
// work accounting. The incumbent parser has none of that syntax, so
// promql_native_metadata_semantics executes only current plain-selector,
// ordinary-matcher, vector-matching and info() controls and records the accepted
// rules as target cases owned by non-passing obligations.
// TestPromQLNativeMetadataGrammarRejectedByIncumbent pins the parse rejections as
// absence evidence; no target expression is executed here.
//
// Expectations are authored from the fixture's stated arithmetic and semantics,
// never captured from a store and then declared independent. The companion
// manifest test (contract_manifest_test.go) refuses to let a contract obligation
// disappear: every fixed case is either backed by a named executable test, a
// recorded incumbent defect, an explicitly unsupported capability, or a named
// open decision.
//
// Two routes are driven by the same fixture:
//
//   - "columnar" is the current prototype: samples through the shared
//     storage.AppenderV2 surface, metadata through the exported owner APIs, the
//     experimental metadata HTTP handler, PromQL over the store's queryable, and
//     flush/reopen/retain/compact lifecycle operations.
//   - "tsdb" is stock TSDB through util/teststorage, which provides the legacy
//     sample/PromQL subset only. Metadata, HTTP and lifecycle events are
//     unsupported there and a fixture that claims one for this route fails
//     loudly instead of being skipped.
//   - "tsdb_st" is the same stock TSDB explicitly configured for start-timestamp
//     storage (EnableSTStorage, EncXOR2 float chunks, histogram ST encoding).
//     The default "tsdb" route is deliberately not ST-capable: its observed
//     ST=0 is a property of that configuration, not of stock TSDB. A fixture
//     that needs start timestamps must name "tsdb_st" so the comparison never
//     matches an ST-enabled candidate against an ST-disabled comparator.
//
// Route-independent logical semantics do not imply byte-identical
// representations. The store's own observables are rendered canonically:
// opaque owner references are replaced by the owner's identifying attributes,
// the compatibility identity label is replaced by "<resource>" and pagination
// tokens by "<token>", because those are implementation-derived identifiers
// rather than contract values. A separate check proves the replacement cannot
// hide a mismatched identity: every stored __otel_resource__ label must equal
// the reference of the owner its membership reports.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/teststorage"
)

// contractRouteGolden and contractRouteTSDB are the fixture route names.
const (
	contractRouteColumnar = "columnar"
	contractRouteTSDB     = "tsdb"
	// contractRouteTSDBST is stock TSDB configured for start-timestamp storage.
	contractRouteTSDBST = "tsdb_st"
)

// contractFixtureDir holds the machine-readable logical fixtures.
const contractFixtureDir = "testdata/contract"

// contractFixture is one logical scenario with hand-authored expectations.
type contractFixture struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Obligations are manifest obligation ids this fixture executes.
	Obligations []string `json:"obligations"`
	// Routes are the adapters the fixture is meaningful for.
	Routes []string `json:"routes"`
	// OpenDecisions names semantic decisions the fixture deliberately does not
	// answer. The behaviour it does assert must be independent of them.
	OpenDecisions []string `json:"open_decisions,omitempty"`
	// RetainedVersionLimit asks the store to retain at most this many versions
	// per owner. Zero leaves the route default, which the columnar contract route
	// deliberately makes unbounded. A route that cannot honour a bound must not be
	// listed for a fixture that states one.
	RetainedVersionLimit int `json:"retained_version_limit,omitempty"`
	// TargetCases are accepted target behaviours this fixture deliberately does
	// not execute because the incumbent cannot express them. Each names the
	// non-passing obligation that keeps the absence visible.
	TargetCases []contractTargetCase `json:"target_cases,omitempty"`
	// Scope records what the fixture does and does not claim, for review.
	Scope string `json:"scope"`
	// Events are applied in order.
	Events []contractEvent `json:"events"`
	// State is the complete retained state after every event. A nil slice means
	// "do not assert"; an empty slice means "assert there are none".
	State *contractState `json:"state,omitempty"`
}

// contractTargetCase is one accepted target behaviour the incumbent cannot
// execute. It is a machine-checked record of an absent test, not a skipped test:
// the runner never attempts it, and the manifest gate requires a non-passing
// obligation to own it.
type contractTargetCase struct {
	ID         string `json:"id"`
	Obligation string `json:"obligation"`
	Accepted   string `json:"accepted"`
	Reason     string `json:"reason"`
}

// contractEvent is one ordered operation with its expectation.
type contractEvent struct {
	Op string `json:"op"`
	// Routes restricts the event to a subset of the fixture routes. An empty list
	// means every fixture route, and a route outside the list is skipped openly.
	Routes []string `json:"routes,omitempty"`
	// Args is operation-specific and decoded by contractRunEvent.
	Args json.RawMessage `json:"args,omitempty"`
	// Expect is the complete observable result of an observation operation.
	Expect json.RawMessage `json:"expect,omitempty"`
	// ExpectError names the error class a transactional operation must return.
	ExpectError string `json:"expect_error,omitempty"`
}

// contractState is a complete expectation for the retained series and metadata.
type contractState struct {
	Series []contractSeriesState `json:"series,omitempty"`
	Owners []contractOwnerState  `json:"owners,omitempty"`
}

// contractSeriesState is one series with every sample rendered exactly.
type contractSeriesState struct {
	Labels  string   `json:"labels"`
	Samples []string `json:"samples"`
}

// contractOwnerState is one owner history with its complete version list.
type contractOwnerState struct {
	Kind        string                 `json:"kind"`
	Identifying map[string]string      `json:"identifying"`
	Truncated   bool                   `json:"truncated"`
	Versions    []contractVersionState `json:"versions"`
}

// contractVersionState is one version: content, validity and observation bounds.
type contractVersionState struct {
	Content       map[string]string `json:"content"`
	EffectiveFrom int64             `json:"effective_from"`
	ValidUntil    *int64            `json:"valid_until,omitempty"`
	ObservedFrom  int64             `json:"observed_from"`
	ObservedUntil int64             `json:"observed_until"`
}

// contractSample is one logical input sample. Value is a decimal string for
// ordinary values, Bits is a 0x-prefixed IEEE-754 bit pattern for values whose
// bits are the contract (NaN payloads, signed zero, infinities), Stale marks an
// explicit staleness marker, and Histogram carries a complete native histogram.
type contractSample struct {
	T         int64              `json:"t"`
	Value     string             `json:"value,omitempty"`
	Bits      string             `json:"bits,omitempty"`
	Stale     bool               `json:"stale,omitempty"`
	Start     int64              `json:"start,omitempty"`
	Histogram *contractHistogram `json:"histogram,omitempty"`
}

// contractOwner names one metadata owner the way the exported constructors do.
type contractOwner struct {
	Kind        string            `json:"kind"`
	Identifying map[string]string `json:"identifying"`
}

// contractAppendArgs appends samples of one series as one transaction.
// ResourceOwner, when stated, is the Resource the series belongs to: the
// adapter derives the compatibility identity label from it, because that label
// is an opaque reference and not a hand-authorable value.
type contractAppendArgs struct {
	Series        map[string]string `json:"series"`
	ResourceOwner *contractOwner    `json:"resource_owner,omitempty"`
	Samples       []contractSample  `json:"samples"`
}

// contractObserveArgs states one metadata observation and series association.
type contractObserveArgs struct {
	Series        map[string]string `json:"series"`
	ResourceOwner *contractOwner    `json:"resource_owner,omitempty"`
	Owner         contractOwner     `json:"owner"`
	Content       map[string]string `json:"content"`
	EffectiveFrom int64             `json:"effective_from"`
}

// contractCommitPart is one series plus its observations in a single batch.
type contractCommitPart struct {
	Series        map[string]string     `json:"series"`
	ResourceOwner *contractOwner        `json:"resource_owner,omitempty"`
	Samples       []contractSample      `json:"samples,omitempty"`
	Observes      []contractObserveArgs `json:"observes,omitempty"`
}

// contractCommitArgs is one atomic sample-and-metadata transaction.
type contractCommitArgs struct {
	Parts       []contractCommitPart  `json:"parts"`
	OwnerUpdate []contractObserveArgs `json:"owner_updates,omitempty"`
}

// contractPromQLArgs is one instant query.
type contractPromQLArgs struct {
	Expr string `json:"expr"`
	At   int64  `json:"at"`
}

// contractHTTPArgs is one metadata HTTP request path with its query string.
type contractHTTPArgs struct {
	Path string `json:"path"`
}

// contractAttribute is one typed metadata attribute condition.
type contractAttribute struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// contractSeriesQueryArgs is one reverse metadata query. After "$next" continues
// from the previous observation's pagination token.
type contractSeriesQueryArgs struct {
	Kind       string              `json:"kind"`
	Start      int64               `json:"start"`
	End        int64               `json:"end"`
	Attributes []contractAttribute `json:"attributes,omitempty"`
	Projection []string            `json:"projection,omitempty"`
	Limit      int                 `json:"limit,omitempty"`
	After      string              `json:"after,omitempty"`
}

// contractLifecycleArgs is one retention cutoff.
type contractLifecycleArgs struct {
	Before int64 `json:"before"`
}

// contractCompactArgs is one head-compaction range for the stock routes.
type contractCompactArgs struct {
	MinT int64 `json:"mint"`
	MaxT int64 `json:"maxt"`
}

// contractStats is the subset of exposed physical-work counters a fixture may
// assert. Only counters that carry a contract obligation belong here.
type contractStats struct {
	SamplePayloadOpens   uint64 `json:"sample_payload_opens"`
	MetadataPayloadOpens uint64 `json:"metadata_payload_opens"`
	SegmentsRead         uint64 `json:"segments_read"`
	PayloadBytesRead     uint64 `json:"payload_bytes_read"`
	Blocks               int    `json:"blocks"`
	SealCount            uint64 `json:"seal_count"`
	// CommittedMetadataTransactions is the number of metadata transactions the
	// store actually published. It is read from the store's committed-work
	// accounting, not from attempts.
	CommittedMetadataTransactions uint64 `json:"committed_metadata_transactions"`
	// CommittedVersions and CommittedMemberships count published change points.
	CommittedVersions    uint64 `json:"committed_versions"`
	CommittedMemberships uint64 `json:"committed_memberships"`
	// WALSegments is the number of retained WAL segments in the store directory.
	WALSegments int `json:"wal_segments"`
}

// contractPromQLResult is a complete instant-query result.
type contractPromQLResult struct {
	Series []contractPromQLSeries `json:"series"`
}

// contractPromQLSeries is one result series with exact values.
type contractPromQLSeries struct {
	Labels  string   `json:"labels"`
	Samples []string `json:"samples"`
}

// TestContractFixtures runs every machine-readable fixture on every route it
// declares and compares every observation and the final state. A fixture's
// target_cases are accepted behaviours the incumbent cannot execute; they are
// never attempted here and the manifest gate requires each to name a non-passing
// obligation, so an absent test cannot look like a passing one.
func TestContractFixtures(t *testing.T) {
	fixtures := contractLoadFixtures(t)
	require.NotEmpty(t, fixtures, "the contract fixture directory must not be empty")
	for _, fixture := range fixtures {
		t.Run(fixture.ID, func(t *testing.T) {
			require.NotEmpty(t, fixture.Routes, "fixture %s declares no route", fixture.ID)
			for _, routeName := range fixture.Routes {
				t.Run(routeName, func(t *testing.T) {
					route := contractNewRoute(t, routeName, fixture.RetainedVersionLimit)
					for i, event := range fixture.Events {
						if !contractEventApplies(event, routeName) {
							continue
						}
						t.Run(fmt.Sprintf("%02d_%s", i, event.Op), func(t *testing.T) {
							contractRunEvent(t, route, event)
						})
					}
					contractRequireState(t, route, fixture.State)
				})
			}
		})
	}
}

// contractLoadFixtures reads every fixture file in a stable order.
func contractLoadFixtures(t *testing.T) []contractFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(contractFixtureDir, "cases", "*.json"))
	require.NoError(t, err)
	slices.Sort(paths)
	fixtures := make([]contractFixture, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var fixture contractFixture
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		require.NoError(t, decoder.Decode(&fixture), "%s: the fixture must match the documented schema", path)
		require.NotEmpty(t, fixture.ID, "%s: a fixture needs an id", path)
		require.Equal(t, strings.TrimSuffix(filepath.Base(path), ".json"), fixture.ID,
			"%s: the file name must be the fixture id", path)
		fixtures = append(fixtures, fixture)
	}
	return fixtures
}

// contractEventApplies reports whether the event runs on a route. A restricted
// event is skipped openly; the fixture file itself is the record of that choice.
func contractEventApplies(event contractEvent, route string) bool {
	if len(event.Routes) == 0 {
		return true
	}
	return slices.Contains(event.Routes, route)
}

// contractRoute drives one implementation. It never assumes a physical layout:
// every read goes through storage.Queryable, storage.AppenderV2 or the exported
// metadata API.
type contractRoute struct {
	name      string
	columnar  *Storage
	tsdb      storage.Storage
	dir       string
	opts      StorageOptions
	engine    promql.QueryEngine
	lastToken string
	lastStats map[string]float64
}

// contractNewRoute builds one route adapter for a fixture. Routes own their
// temporary directory and are closed when the test ends. A fixture that bounds
// retained history must not be run on a route that cannot honour the bound.
func contractNewRoute(t *testing.T, name string, retainedVersionLimit int) *contractRoute {
	t.Helper()
	route := &contractRoute{name: name, engine: contractEngine(t)}
	switch name {
	case contractRouteColumnar:
		route.dir = t.TempDir()
		limit := -1
		if retainedVersionLimit > 0 {
			limit = retainedVersionLimit
		}
		route.opts = StorageOptions{FlushSamples: 1_000, MetadataVersionCap: limit}
		store, err := NewStorage(route.dir, route.opts)
		require.NoError(t, err)
		route.columnar = store
		t.Cleanup(func() { require.NoError(t, store.Close()) })
	case contractRouteTSDB:
		require.Zero(t, retainedVersionLimit,
			"stock TSDB route cannot honour a retained version limit; restrict the fixture to the columnar route")
		route.tsdb = teststorage.New(t)
	case contractRouteTSDBST:
		require.Zero(t, retainedVersionLimit,
			"stock TSDB route cannot honour a retained version limit; restrict the fixture to the columnar route")
		// The supported stock start-timestamp configuration: ST storage requires
		// an ST-capable float encoding, and histograms need their own ST encoding.
		route.tsdb = teststorage.New(t, func(opts *tsdb.Options) {
			opts.EnableSTStorage = true
			opts.FloatChunkEncoding = chunkenc.EncXOR2
			opts.EnableHistogramSTEncoding = true
		})
	default:
		t.Fatalf("unknown contract route %q", name)
	}
	return route
}

// contractEngine is the PromQL engine every route is evaluated with, so a
// result difference cannot come from two engine configurations.
func contractEngine(t *testing.T) promql.QueryEngine {
	t.Helper()
	return promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples:    promqltest.DefaultMaxSamplesPerQuery,
		Timeout:       time.Minute,
		LookbackDelta: 5 * time.Minute,
		LabelNamerConfig: &promql.LabelNamerConfig{
			UnderscoreLabelSanitization: true, PreserveMultipleUnderscores: true,
		},
		Parser: parser.NewParser(promqltest.TestParserOpts),
	})
}

// appendable returns the route's append surface. Both stores provide the typed
// V2 appender, which is what carries start timestamps and native histograms.
func (r *contractRoute) appendable() storage.AppenderV2 {
	if r.columnar != nil {
		return r.columnar.AppenderV2(context.Background())
	}
	appender, ok := r.tsdb.(interface {
		AppenderV2(context.Context) storage.AppenderV2
	})
	if !ok {
		r.unsupported("typed append")
	}
	return appender.AppenderV2(context.Background())
}

// queryable returns the route's query surface.
func (r *contractRoute) queryable() storage.Queryable {
	if r.columnar != nil {
		return r.columnar
	}
	return r.tsdb
}

// requireColumnar fails loudly for an operation the legacy route does not have.
func (r *contractRoute) requireColumnar(op string) *Storage {
	if r.columnar == nil {
		r.unsupported(op)
	}
	return r.columnar
}

// unsupported states that a fixture claimed behaviour a route does not provide.
// It is deliberately fatal: silently skipping the assertion would make an
// unsupported capability look covered.
func (r *contractRoute) unsupported(op string) {
	panic(fmt.Sprintf("contract fixture claims %q on route %q, which does not support it; "+
		"restrict the event with its routes field or record the gap in the manifest", op, r.name))
}

// contractRunEvent dispatches one event and asserts its expectation.
func contractRunEvent(t *testing.T, route *contractRoute, event contractEvent) {
	t.Helper()
	var err error
	switch event.Op {
	case "append":
		var args contractAppendArgs
		contractDecodeArgs(t, event, &args)
		err = route.appendSamples(args)
	case "observe":
		var args contractObserveArgs
		contractDecodeArgs(t, event, &args)
		err = route.observe(args)
	case "commit":
		var args contractCommitArgs
		contractDecodeArgs(t, event, &args)
		err = route.commit(args)
	case "flush":
		route.requireColumnar(event.Op)
		err = route.columnar.Flush()
	case "reopen":
		route.requireColumnar(event.Op)
		route.reopen(t)
	case "retain_before":
		var args contractLifecycleArgs
		contractDecodeArgs(t, event, &args)
		route.requireColumnar(event.Op)
		_, err = route.columnar.RetainBefore(t.Context(), args.Before)
	case "compact":
		var args contractCompactArgs
		contractDecodeArgs(t, event, &args)
		if route.columnar != nil {
			_, err = route.columnar.Compact(t.Context())
		} else {
			err = route.compactHead(args)
		}
	case "promql":
		var args contractPromQLArgs
		contractDecodeArgs(t, event, &args)
		contractRequireJSON(t, contractRenderPromQL(t, route, args), event.Expect)
		return
	case "http":
		var args contractHTTPArgs
		contractDecodeArgs(t, event, &args)
		contractRequireJSON(t, route.httpGet(t, args), event.Expect)
		return
	case "metadata_series":
		var args contractSeriesQueryArgs
		contractDecodeArgs(t, event, &args)
		contractRequireJSON(t, route.metadataSeries(t, args), event.Expect)
		return
	case "state":
		// A mid-fixture complete state assertion, so a fixture can prove the same
		// exact payload survives both before and after a persistence operation.
		var want contractState
		require.NotEmpty(t, event.Expect, "a state event needs a complete expectation")
		decoder := json.NewDecoder(strings.NewReader(string(event.Expect)))
		decoder.DisallowUnknownFields()
		require.NoError(t, decoder.Decode(&want), "decode the state expectation")
		contractRequireState(t, route, &want)
		return
	case "stats_reset":
		route.requireColumnar(event.Op)
		route.columnar.reads.reset()
		return
	case "stats":
		route.requireColumnar(event.Op)
		contractRequireStats(t, route, route.stats(t), event.Expect)
		return
	default:
		t.Fatalf("unknown fixture operation %q", event.Op)
	}
	contractRequireError(t, err, event.ExpectError)
}

// contractDecodeArgs decodes one event's arguments strictly.
func contractDecodeArgs(t *testing.T, event contractEvent, args any) {
	t.Helper()
	require.NotEmpty(t, event.Args, "%s needs arguments", event.Op)
	decoder := json.NewDecoder(strings.NewReader(string(event.Args)))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(args), "decode %s arguments", event.Op)
}

// contractRequireJSON compares a rendered observable with its expectation.
func contractRequireJSON(t *testing.T, got any, want json.RawMessage) {
	t.Helper()
	require.NotEmpty(t, want, "an observation event needs an expectation")
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(encoded))
}

// contractRequireStats compares the named physical-work counters. Other counters
// are not asserted by the fixture, so an unrelated future counter cannot make a
// semantic case fail.
func contractRequireStats(t *testing.T, route *contractRoute, got contractStats, want json.RawMessage) {
	t.Helper()
	require.NotEmpty(t, want, "a stats event needs an expectation")
	var wanted map[string]any
	require.NoError(t, json.Unmarshal(want, &wanted))
	require.NotEmpty(t, wanted)
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	var observed map[string]float64
	require.NoError(t, json.Unmarshal(encoded, &observed))
	// "$previous" asserts that the counter did not change since the previous
	// stats event, which is how a fixture states "no committed work" without
	// depending on an implementation-specific absolute value.
	require.NotNil(t, route)
	for name, value := range wanted {
		require.Contains(t, observed, name, "unknown counter %q", name)
		if placeholder, ok := value.(string); ok {
			require.Equal(t, "$previous", placeholder, "counter %q has an unknown placeholder", name)
			previous, seen := route.lastStats[name]
			require.True(t, seen, "counter %q has no previous value to compare with", name)
			require.Equal(t, previous, observed[name], "counter %q", name)
			continue
		}
		number, ok := value.(float64)
		require.True(t, ok, "counter %q must be a number or \"$previous\"", name)
		require.Equal(t, number, observed[name], "counter %q", name)
	}
	route.lastStats = observed
}

// contractRequireError asserts the named error class, or that none occurred.
func contractRequireError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		require.NoError(t, err)
		return
	}
	require.Error(t, err)
	if contains, ok := strings.CutPrefix(want, "contains:"); ok {
		require.Contains(t, err.Error(), contains)
		return
	}
	classes := map[string]error{
		"conflicting_metadata":   ErrConflictingMetadata,
		"conflicting_membership": ErrConflictingMembership,
		"series_hash_collision":  ErrSeriesHashCollision,
		"metadata_disabled":      ErrMetadataDisabled,
		"duplicate_sample":       storage.ErrDuplicateSampleForTimestamp,
		"out_of_order":           storage.ErrOutOfOrderSample,
		"out_of_bounds":          storage.ErrOutOfBounds,
	}
	target, ok := classes[want]
	require.True(t, ok, "unknown expected error class %q", want)
	require.ErrorIs(t, err, target, "expected error class %q, got %v", want, err)
}

// appendSamples appends one series' samples as one transaction.
func (r *contractRoute) appendSamples(args contractAppendArgs) error {
	ls := contractSeriesLabels(args.Series, args.ResourceOwner)
	app := r.appendable()
	for _, sample := range args.Samples {
		h, fh, err := sample.histogramPayload()
		if err != nil {
			return err
		}
		v, err := sample.valueBits()
		if err != nil {
			return err
		}
		if _, err := app.Append(0, ls, sample.Start, sample.T, v, h, fh, storage.AOptions{}); err != nil {
			_ = app.Rollback()
			return err
		}
	}
	return app.Commit()
}

// observe records one metadata observation and association.
func (r *contractRoute) observe(args contractObserveArgs) error {
	store := r.requireColumnar("observe")
	_, err := store.MetadataAssociate(contractSeriesLabels(args.Series, args.ResourceOwner), args.update())
	return err
}

// commit applies one atomic sample-and-metadata batch.
func (r *contractRoute) commit(args contractCommitArgs) error {
	store := r.requireColumnar("commit")
	batch := Batch{}
	for _, part := range args.Parts {
		ls := contractSeriesLabels(part.Series, part.ResourceOwner)
		series := BatchSeries{Labels: ls}
		for _, sample := range part.Samples {
			h, fh, err := sample.histogramPayload()
			if err != nil {
				return err
			}
			v, err := sample.valueBits()
			if err != nil {
				return err
			}
			var encoded []byte
			if h != nil || fh != nil {
				encoded, err = encodeHistogram(h, fh)
				if err != nil {
					return err
				}
			}
			series.Timestamps = append(series.Timestamps, sample.T)
			series.StartTimestamps = append(series.StartTimestamps, sample.Start)
			series.Values = append(series.Values, v)
			series.Histograms = append(series.Histograms, encoded)
			switch {
			case h != nil:
				series.Kinds = append(series.Kinds, SampleHistogram)
			case fh != nil:
				series.Kinds = append(series.Kinds, SampleFloatHistogram)
			case value.IsStaleNaN(v):
				series.Kinds = append(series.Kinds, SampleStale)
			default:
				series.Kinds = append(series.Kinds, SampleFloat)
			}
		}
		if len(part.Samples) > 0 {
			batch.Series = append(batch.Series, series)
		}
		for _, observe := range part.Observes {
			batch.Updates = append(batch.Updates, AssociationUpdate{
				Labels: contractSeriesLabels(observe.Series, observe.ResourceOwner), Update: observe.update(),
			})
		}
	}
	for _, update := range args.OwnerUpdate {
		batch.OwnerUpdates = append(batch.OwnerUpdates, update.update())
	}
	return store.AppendBatch(context.Background(), batch)
}

// compactHead compacts a stock head range. It is the applicable persistence
// operation for the stock routes: the block chunks are what a stock TSDB
// restart would read, and start timestamps must survive that write.
func (r *contractRoute) compactHead(args contractCompactArgs) error {
	store, ok := r.tsdb.(*teststorage.TestStorage)
	if !ok {
		panic(fmt.Sprintf("contract route %q cannot compact a %T", r.name, r.tsdb))
	}
	return store.CompactHead(tsdb.NewRangeHead(store.Head(), args.MinT, args.MaxT))
}

// update resolves one fixture observation into the exported Update type.
func (a contractObserveArgs) update() Update {
	return Update{
		Owner:         contractOwnerKey(a.Owner),
		Content:       Content{Attributes: contractCloneMap(a.Content)},
		EffectiveFrom: a.EffectiveFrom,
	}
}

// reopen closes and reopens the columnar store, which replays the WAL and
// reopens published blocks.
func (r *contractRoute) reopen(t *testing.T) {
	t.Helper()
	require.NoError(t, r.columnar.Close())
	store, err := NewStorage(r.dir, r.opts)
	require.NoError(t, err)
	r.columnar = store
}

// httpGet drives the real metadata HTTP handler.
func (r *contractRoute) httpGet(t *testing.T, args contractHTTPArgs) any {
	t.Helper()
	store := r.requireColumnar("http")
	handler := NewMetadataHandler(store, slog.New(slog.DiscardHandler))
	request := httptest.NewRequest(http.MethodGet, args.Path, http.NoBody)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	var body any
	if recorder.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), "the handler must answer with JSON")
	}
	if failure, ok := body.(map[string]any); ok {
		if message, present := failure["error"]; present {
			body = map[string]any{"status": failure["status"], "error": message}
		}
	}
	return map[string]any{
		"status": recorder.Code,
		"data":   contractNormalizeOpaque(body),
	}
}

// metadataSeries runs one reverse metadata query through the exported API.
func (r *contractRoute) metadataSeries(t *testing.T, args contractSeriesQueryArgs) any {
	t.Helper()
	store := r.requireColumnar("metadata_series")
	kind, err := parseMetadataKind(args.Kind)
	require.NoError(t, err)
	after := args.After
	if after == "$next" {
		after = r.lastToken
	}
	conditions := make([]MetadataAttributeCondition, 0, len(args.Attributes))
	for _, attribute := range args.Attributes {
		conditions = append(conditions, MetadataAttributeCondition(attribute))
	}
	page, err := store.QueryMetadataSeries(context.Background(), MetadataSeriesQuery{
		Kind: kind, Start: args.Start, End: args.End, Attributes: conditions,
		Projection: args.Projection, After: after, Limit: args.Limit,
	})
	require.NoError(t, err)
	r.lastToken = page.NextToken
	rendered := contractSeriesRows{Series: []contractSeriesRow{}}
	for _, row := range page.Series {
		rendered.Series = append(rendered.Series, contractSeriesRow{
			Labels:               contractNormalizeLabels(row.Labels),
			Owner:                contractCanonicalIdentity(contractAnyMap(row.OwnerIdentifying)),
			Kind:                 row.Kind,
			OwnerIdentifying:     row.OwnerIdentifying,
			MembershipStart:      row.MembershipStart,
			MembershipUntil:      row.MembershipUntil,
			MembershipObservedTo: row.MembershipObservedTo,
			Versions:             contractVersionViews(row.Versions),
		})
	}
	if page.NextToken != "" {
		rendered.NextToken = "<token>"
	}
	// The store's page order is defined over opaque owner references, which are
	// implementation-derived. Sort the rendered rows by their canonical labels
	// and identity so a multi-owner expectation states semantic content rather
	// than an accidental digest order. Page membership itself is unchanged.
	slices.SortFunc(rendered.Series, func(a, b contractSeriesRow) int {
		if byLabels := strings.Compare(a.Labels, b.Labels); byLabels != 0 {
			return byLabels
		}
		if byOwner := strings.Compare(a.Owner, b.Owner); byOwner != 0 {
			return byOwner
		}
		if a.MembershipStart < b.MembershipStart {
			return -1
		}
		if a.MembershipStart > b.MembershipStart {
			return 1
		}
		return 0
	})
	return rendered
}

// contractSeriesRows is the canonical rendering of one reverse-query page.
type contractSeriesRows struct {
	Series    []contractSeriesRow `json:"series"`
	NextToken string              `json:"next_token,omitempty"`
}

// contractSeriesRow is one series membership with its applicable versions.
type contractSeriesRow struct {
	Labels               string                 `json:"labels"`
	Owner                string                 `json:"owner"`
	Kind                 string                 `json:"kind"`
	OwnerIdentifying     map[string]string      `json:"owner_identifying"`
	MembershipStart      int64                  `json:"membership_start"`
	MembershipUntil      *int64                 `json:"membership_until,omitempty"`
	MembershipObservedTo int64                  `json:"membership_observed_until"`
	Versions             []contractVersionState `json:"versions"`
}

// contractVersionViews renders metadata value views as version states.
func contractVersionViews(views []MetadataValueView) []contractVersionState {
	out := make([]contractVersionState, 0, len(views))
	for _, view := range views {
		out = append(out, contractVersionState{
			Content: view.Descriptive, EffectiveFrom: view.EffectiveFrom, ValidUntil: view.ValidUntil,
			ObservedFrom: view.ObservedFrom, ObservedUntil: view.ObservedUntil,
		})
	}
	return out
}

// contractAnyMap widens a string map for the canonical identity renderer.
func contractAnyMap(attributes map[string]string) map[string]any {
	out := make(map[string]any, len(attributes))
	for name, value := range attributes {
		out[name] = value
	}
	return out
}

// stats renders the exposed physical-work counters a fixture asserts.
func (r *contractRoute) stats(t *testing.T) contractStats {
	t.Helper()
	store := r.requireColumnar("stats")
	read := store.ReadStats()
	work := store.reads.metadataWorkSnapshot()
	return contractStats{
		SamplePayloadOpens:            read.SamplePayloadOpens,
		MetadataPayloadOpens:          read.MetadataPayloadOpens,
		SegmentsRead:                  read.SegmentsRead,
		PayloadBytesRead:              read.PayloadBytesRead,
		Blocks:                        read.Blocks,
		SealCount:                     read.SealCount,
		CommittedMetadataTransactions: uint64(work.metadataTransactions),
		CommittedVersions:             uint64(work.versionsInserted),
		CommittedMemberships:          uint64(work.membershipsInserted),
		WALSegments:                   contractWALSegments(t, store.dir),
	}
}

// contractWALSegments counts retained WAL segment files. It is an
// incumbent-specific observation used only by the rollback obligation; a future
// implementation records its own equivalent in the manifest.
func contractWALSegments(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, walDirName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		require.NoError(t, err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".arrow") {
			count++
		}
	}
	return count
}

// contractRequireState asserts the complete retained state. A nil field is not
// asserted; a present field is asserted exactly, including emptiness.
func contractRequireState(t *testing.T, route *contractRoute, want *contractState) {
	t.Helper()
	if want == nil {
		return
	}
	if want.Series != nil {
		require.JSONEq(t, contractMustJSON(t, want.Series), contractMustJSON(t, route.captureSeries(t)))
	}
	if want.Owners != nil {
		require.Equal(t, contractRouteColumnar, route.name,
			"only the columnar route carries native metadata; a fixture asserting owners must declare that route alone")
		require.JSONEq(t, contractMustJSON(t, want.Owners), contractMustJSON(t, route.captureOwners(t)))
	}
}

// captureSeries renders every retained series and sample exactly.
func (r *contractRoute) captureSeries(t *testing.T) []contractSeriesState {
	t.Helper()
	querier, err := r.queryable().Querier(math.MinInt64, math.MaxInt64)
	require.NoError(t, err)
	defer func() { require.NoError(t, querier.Close()) }()
	// Both stores require at least one matcher; "every metric name" is the
	// route-neutral way to ask for every retained series.
	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+"))
	out := []contractSeriesState{}
	for set.Next() {
		series := set.At()
		entry := contractSeriesState{Labels: contractNormalizeLabels(series.Labels())}
		iterator := series.Iterator(nil)
		for typ := iterator.Next(); typ != chunkenc.ValNone; typ = iterator.Next() {
			entry.Samples = append(entry.Samples, contractRenderSample(iterator, typ))
		}
		require.NoError(t, iterator.Err())
		out = append(out, entry)
	}
	require.NoError(t, set.Err())
	slices.SortFunc(out, func(a, b contractSeriesState) int {
		if byLabels := strings.Compare(a.Labels, b.Labels); byLabels != 0 {
			return byLabels
		}
		// The canonical rendering replaces the Resource reference with a
		// placeholder, so two Resources that differ only in identity render the
		// same labels. Sort their exact samples as a tie-breaker so the complete
		// state expectation has a deterministic order and still shows both series.
		return strings.Compare(strings.Join(a.Samples, ","), strings.Join(b.Samples, ","))
	})
	return out
}

// captureOwners renders every owner history the store retains, and checks that
// a stored Resource identity label matches the reference of the owner its
// membership reports.
func (r *contractRoute) captureOwners(t *testing.T) []contractOwnerState {
	t.Helper()
	store := r.requireColumnar("owners")
	out := []contractOwnerState{}
	for kind := MetadataResource; kind <= MetadataSource; kind++ {
		page, err := store.QueryMetadata(context.Background(), MetadataQuery{
			Kind: kind, Start: math.MinInt64, End: math.MaxInt64,
		})
		require.NoError(t, err)
		require.Empty(t, page.NextToken, "the complete owner expectation needs one page")
		for _, view := range page.Owners {
			owner := contractOwnerKey(contractOwner{Kind: view.Kind, Identifying: view.Identifying})
			history, ok := store.MetadataHistory(owner)
			require.True(t, ok, "owner %s has a page entry but no history", owner)
			entry := contractOwnerState{
				Kind: view.Kind, Identifying: view.Identifying, Truncated: history.Truncated,
			}
			for i, version := range history.Versions {
				view := metadataValueView(history.Versions, i, nil)
				entry.Versions = append(entry.Versions, contractVersionState{
					Content: version.Content.Clone().Attributes, EffectiveFrom: view.EffectiveFrom,
					ValidUntil: view.ValidUntil, ObservedFrom: view.ObservedFrom,
					ObservedUntil: view.ObservedUntil,
				})
			}
			out = append(out, entry)
		}
	}
	slices.SortFunc(out, func(a, b contractOwnerState) int {
		if byKind := strings.Compare(a.Kind, b.Kind); byKind != 0 {
			return byKind
		}
		return strings.Compare(contractCanonicalMap(a.Identifying), contractCanonicalMap(b.Identifying))
	})
	contractCheckResourceIdentityLabels(t, store)
	return out
}

// contractCheckResourceIdentityLabels proves the normalised owner references in
// rendered series are not hiding a mismatch: every series that carries the
// Resource identity label is associated with exactly that owner at the time the
// membership starts.
func contractCheckResourceIdentityLabels(t *testing.T, store *Storage) {
	t.Helper()
	page, err := store.QueryMetadataSeries(context.Background(), MetadataSeriesQuery{
		Kind: MetadataResource, Start: math.MinInt64, End: math.MaxInt64,
	})
	require.NoError(t, err)
	for _, row := range page.Series {
		reference := row.Labels.Get(OTELResourceLabel)
		require.NotEmpty(t, reference, "resource-associated series %s must carry the identity label", row.Labels)
		owner := contractOwnerKey(contractOwner{Kind: row.Kind, Identifying: row.OwnerIdentifying})
		require.Equal(t, owner.Ref(), reference, "identity label of %s must match its associated owner", row.Labels)
	}
}

// contractRenderSample renders one iterator position completely.
func contractRenderSample(iterator chunkenc.Iterator, typ chunkenc.ValueType) string {
	timestamp, start := iterator.AtT(), iterator.AtST()
	switch typ {
	case chunkenc.ValFloat:
		_, v := iterator.At()
		kind := "float"
		if value.IsStaleNaN(v) {
			kind = "stale"
		}
		return fmt.Sprintf("%d|%s|%s|%d", timestamp, contractFloatBits(v), kind, start)
	case chunkenc.ValHistogram:
		_, h := iterator.AtHistogram(nil)
		return fmt.Sprintf("%d|%s|histogram|%d", timestamp, contractHistogramString(h, nil), start)
	case chunkenc.ValFloatHistogram:
		_, fh := iterator.AtFloatHistogram(nil)
		return fmt.Sprintf("%d|%s|float_histogram|%d", timestamp, contractHistogramString(nil, fh), start)
	default:
		panic(fmt.Sprintf("unexpected value type %v", typ))
	}
}

// contractRenderPromQL evaluates one instant query and renders every result.
func contractRenderPromQL(t *testing.T, route *contractRoute, args contractPromQLArgs) contractPromQLResult {
	t.Helper()
	query, err := route.engine.NewInstantQuery(t.Context(), route.queryable(), nil, args.Expr, time.UnixMilli(args.At))
	require.NoError(t, err)
	defer query.Close()
	result := query.Exec(t.Context())
	require.NoError(t, result.Err)
	vector, ok := result.Value.(promql.Vector)
	require.True(t, ok, "an instant selector must return a vector, got %T", result.Value)
	out := contractPromQLResult{Series: []contractPromQLSeries{}}
	for _, sample := range vector {
		value := contractFloatBits(sample.F)
		if sample.H != nil {
			value = contractHistogramString(nil, sample.H)
		}
		out.Series = append(out.Series, contractPromQLSeries{
			Labels: contractNormalizeLabels(sample.Metric), Samples: []string{value},
		})
	}
	slices.SortFunc(out.Series, func(a, b contractPromQLSeries) int {
		if byLabels := strings.Compare(a.Labels, b.Labels); byLabels != 0 {
			return byLabels
		}
		return strings.Compare(strings.Join(a.Samples, ","), strings.Join(b.Samples, ","))
	})
	return out
}

// contractSeriesLabels builds a label set from a fixture map, adding the
// compatibility Resource identity label when the fixture states the owner the
// series belongs to. The label value is the exported owner reference, so a
// fixture never has to restate an opaque digest.
func contractSeriesLabels(attributes map[string]string, owner *contractOwner) labels.Labels {
	if owner == nil {
		return labels.FromMap(attributes)
	}
	builder := labels.NewBuilder(labels.FromMap(attributes))
	builder.Set(OTELResourceLabel, contractOwnerKey(*owner).Ref())
	return builder.Labels()
}

// contractOwnerKey resolves a fixture owner through the exported constructors.
func contractOwnerKey(owner contractOwner) OwnerKey {
	kind, err := parseMetadataKind(owner.Kind)
	if err != nil {
		panic(err)
	}
	switch kind {
	case MetadataResource:
		return ResourceOwner(owner.Identifying)
	case MetadataResourceSchema:
		return OwnerKey{Kind: MetadataResourceSchema, ID: owner.Identifying["resource"]}
	case MetadataScope:
		return ScopeOwner(owner.Identifying["name"], owner.Identifying["version"])
	case MetadataMetric:
		return OwnerKey{Kind: MetadataMetric, ID: owner.Identifying["series"]}
	case MetadataScrape:
		return ScrapeOwner(owner.Identifying["scrape"])
	case MetadataSource:
		return SourceOwner(owner.Identifying["source"])
	default:
		panic(fmt.Sprintf("unsupported fixture owner kind %q", owner.Kind))
	}
}

// contractCloneMap copies a fixture map so the store never aliases it.
func contractCloneMap(attributes map[string]string) map[string]string {
	if attributes == nil {
		return map[string]string{}
	}
	return maps.Clone(attributes)
}

// contractCanonicalMap renders a string map deterministically.
func contractCanonicalMap(attributes map[string]string) string {
	return Content{Attributes: attributes}.Canonical()
}

// contractFloatBits renders an exact IEEE-754 bit pattern.
func contractFloatBits(v float64) string { return fmt.Sprintf("%#016x", math.Float64bits(v)) }

// contractMustJSON renders a value for comparison and fails on an encoding error.
func contractMustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

// valueBits resolves a fixture sample's float value.
func (s contractSample) valueBits() (float64, error) {
	switch {
	case s.Stale:
		return math.Float64frombits(value.StaleNaN), nil
	case s.Bits != "":
		raw, err := strconv.ParseUint(strings.TrimPrefix(s.Bits, "0x"), 16, 64)
		if err != nil {
			return 0, fmt.Errorf("sample bits %q: %w", s.Bits, err)
		}
		return math.Float64frombits(raw), nil
	case s.Value != "":
		parsed, err := strconv.ParseFloat(s.Value, 64)
		if err != nil {
			return 0, fmt.Errorf("sample value %q: %w", s.Value, err)
		}
		return parsed, nil
	case s.Histogram != nil:
		return 0, nil
	default:
		return 0, nil
	}
}

// histogramPayload builds the typed histogram a sample states.
func (s contractSample) histogramPayload() (*histogram.Histogram, *histogram.FloatHistogram, error) {
	if s.Histogram == nil {
		return nil, nil, nil
	}
	return s.Histogram.payload()
}

// contractHistogram is a complete native histogram payload. Float fields are
// bit patterns so the fixture cannot lose precision through decimal text.
type contractHistogram struct {
	Type              string                  `json:"type"`
	Count             string                  `json:"count,omitempty"`
	SumBits           string                  `json:"sum_bits,omitempty"`
	Schema            int32                   `json:"schema"`
	ZeroThresholdBits string                  `json:"zero_threshold_bits,omitempty"`
	ZeroCount         string                  `json:"zero_count,omitempty"`
	ZeroCountBits     string                  `json:"zero_count_bits,omitempty"`
	PositiveSpans     []contractHistogramSpan `json:"positive_spans,omitempty"`
	PositiveBuckets   []string                `json:"positive_buckets,omitempty"`
	NegativeSpans     []contractHistogramSpan `json:"negative_spans,omitempty"`
	NegativeBuckets   []string                `json:"negative_buckets,omitempty"`
	CounterResetHint  uint8                   `json:"counter_reset_hint,omitempty"`
}

// contractHistogramSpan is one span of relative bucket offsets.
type contractHistogramSpan struct {
	Offset int32  `json:"offset"`
	Length uint32 `json:"length"`
}

// payload resolves the fixture payload into the typed histogram.
func (c contractHistogram) payload() (*histogram.Histogram, *histogram.FloatHistogram, error) {
	spans := func(in []contractHistogramSpan) []histogram.Span {
		out := make([]histogram.Span, 0, len(in))
		for _, span := range in {
			out = append(out, histogram.Span{Offset: span.Offset, Length: span.Length})
		}
		return out
	}
	sum, err := contractParseBits(c.SumBits)
	if err != nil {
		return nil, nil, err
	}
	threshold, err := contractParseBits(c.ZeroThresholdBits)
	if err != nil {
		return nil, nil, err
	}
	if c.Type == "float" {
		buckets := make([]float64, 0, len(c.PositiveBuckets))
		for _, bucket := range c.PositiveBuckets {
			value, err := contractParseBits(bucket)
			if err != nil {
				return nil, nil, err
			}
			buckets = append(buckets, value)
		}
		negative := make([]float64, 0, len(c.NegativeBuckets))
		for _, bucket := range c.NegativeBuckets {
			value, err := contractParseBits(bucket)
			if err != nil {
				return nil, nil, err
			}
			negative = append(negative, value)
		}
		zeroCount, err := contractParseBits(c.ZeroCountBits)
		if err != nil {
			return nil, nil, err
		}
		return nil, &histogram.FloatHistogram{
			CounterResetHint: histogram.CounterResetHint(c.CounterResetHint),
			Schema:           c.Schema,
			ZeroThreshold:    threshold,
			ZeroCount:        zeroCount,
			Count:            contractParseFloat(c.Count),
			Sum:              sum,
			PositiveSpans:    spans(c.PositiveSpans),
			PositiveBuckets:  buckets,
			NegativeSpans:    spans(c.NegativeSpans),
			NegativeBuckets:  negative,
		}, nil
	}
	count, err := strconv.ParseUint(c.Count, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("histogram count %q: %w", c.Count, err)
	}
	zeroCount, err := strconv.ParseUint(c.ZeroCount, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("histogram zero count %q: %w", c.ZeroCount, err)
	}
	buckets := make([]int64, 0, len(c.PositiveBuckets))
	for _, bucket := range c.PositiveBuckets {
		value, err := strconv.ParseInt(bucket, 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("histogram bucket %q: %w", bucket, err)
		}
		buckets = append(buckets, value)
	}
	negative := make([]int64, 0, len(c.NegativeBuckets))
	for _, bucket := range c.NegativeBuckets {
		value, err := strconv.ParseInt(bucket, 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("histogram negative bucket %q: %w", bucket, err)
		}
		negative = append(negative, value)
	}
	return &histogram.Histogram{
		CounterResetHint: histogram.CounterResetHint(c.CounterResetHint),
		Schema:           c.Schema,
		ZeroThreshold:    threshold,
		ZeroCount:        zeroCount,
		Count:            count,
		Sum:              sum,
		PositiveSpans:    spans(c.PositiveSpans),
		PositiveBuckets:  buckets,
		NegativeSpans:    spans(c.NegativeSpans),
		NegativeBuckets:  negative,
	}, nil, nil
}

// contractParseBits parses a 0x-prefixed IEEE-754 bit pattern.
func contractParseBits(raw string) (float64, error) {
	if raw == "" {
		return 0, nil
	}
	bits, err := strconv.ParseUint(strings.TrimPrefix(raw, "0x"), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("bit pattern %q: %w", raw, err)
	}
	return math.Float64frombits(bits), nil
}

// contractParseFloat parses a decimal float, which histograms use for counts
// where the fixture does not state bits.
func contractParseFloat(raw string) float64 {
	if raw == "" {
		return 0
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		panic(fmt.Sprintf("histogram count %q: %v", raw, err))
	}
	return parsed
}

// contractHistogramString renders a complete histogram payload canonically.
func contractHistogramString(h *histogram.Histogram, fh *histogram.FloatHistogram) string {
	renderSpans := func(spans []histogram.Span) string {
		parts := make([]string, 0, len(spans))
		for _, span := range spans {
			parts = append(parts, fmt.Sprintf("%d/%d", span.Offset, span.Length))
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	if fh != nil {
		buckets := make([]string, 0, len(fh.PositiveBuckets))
		for _, bucket := range fh.PositiveBuckets {
			buckets = append(buckets, contractFloatBits(bucket))
		}
		negative := make([]string, 0, len(fh.NegativeBuckets))
		for _, bucket := range fh.NegativeBuckets {
			negative = append(negative, contractFloatBits(bucket))
		}
		return fmt.Sprintf("fh:count=%s;sum=%s;schema=%d;zero_threshold=%s;zero_count=%s;pos=%s%s;neg=%s%s;hint=%d",
			contractFloatBits(fh.Count), contractFloatBits(fh.Sum), fh.Schema,
			contractFloatBits(fh.ZeroThreshold), contractFloatBits(fh.ZeroCount),
			renderSpans(fh.PositiveSpans), strings.Join(buckets, ","),
			renderSpans(fh.NegativeSpans), strings.Join(negative, ","), fh.CounterResetHint)
	}
	buckets := make([]string, 0, len(h.PositiveBuckets))
	for _, bucket := range h.PositiveBuckets {
		buckets = append(buckets, strconv.FormatInt(bucket, 10))
	}
	negative := make([]string, 0, len(h.NegativeBuckets))
	for _, bucket := range h.NegativeBuckets {
		negative = append(negative, strconv.FormatInt(bucket, 10))
	}
	return fmt.Sprintf("h:count=%d;sum=%s;schema=%d;zero_threshold=%s;zero_count=%d;pos=%s%s;neg=%s%s;hint=%d",
		h.Count, contractFloatBits(h.Sum), h.Schema, contractFloatBits(h.ZeroThreshold), h.ZeroCount,
		renderSpans(h.PositiveSpans), strings.Join(buckets, ","),
		renderSpans(h.NegativeSpans), strings.Join(negative, ","), h.CounterResetHint)
}

// contractNormalizeLabels renders labels with the Resource reference replaced by
// a placeholder. The reference is derived from the owner identity, which the
// owner histories assert; contractCheckResourceIdentityLabels proves the
// replacement does not hide a mismatched identity.
func contractNormalizeLabels(ls labels.Labels) string {
	normalized := make([]labels.Label, 0, ls.Len())
	ls.Range(func(label labels.Label) {
		if label.Name == OTELResourceLabel {
			label.Value = "<resource>"
		}
		normalized = append(normalized, label)
	})
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Name < normalized[j].Name })
	return labels.New(normalized...).String()
}

// contractNormalizeOpaque replaces implementation-derived identifiers inside a
// decoded JSON document: owner references and pagination tokens.
func contractNormalizeOpaque(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		if _, isLabels := typed["__name__"]; isLabels {
			// A decoded label set is rendered as the same canonical label string a
			// stored series reports, so an HTTP golden stays readable.
			attributes := make(map[string]string, len(typed))
			for name, item := range typed {
				text, ok := item.(string)
				if !ok {
					continue
				}
				attributes[name] = text
			}
			return contractNormalizeLabels(labels.FromMap(attributes))
		}
		identity, _ := typed["identifying"].(map[string]any)
		ownerIdentity, _ := typed["owner_identifying"].(map[string]any)
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			switch {
			case key == "next_token":
				if item == nil {
					out[key] = nil
				} else {
					out[key] = "<token>"
				}
			case key == "snapshot_id":
				out[key] = "<snapshot>"
			case key == "ref":
				out[key] = "<ref>"
			case key == "id" && identity != nil:
				out[key] = contractCanonicalIdentity(identity)
			case key == "owner" && ownerIdentity != nil:
				out[key] = contractCanonicalIdentity(ownerIdentity)
			default:
				out[key] = contractNormalizeOpaque(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = contractNormalizeOpaque(item)
		}
		return out
	default:
		return value
	}
}

// contractCanonicalIdentity renders a decoded identifying attribute object as a
// deterministic JSON object, which is what a fixture states readably.
func contractCanonicalIdentity(value map[string]any) string {
	attributes := make(map[string]string, len(value))
	for name, item := range value {
		text, ok := item.(string)
		if !ok {
			encoded, err := json.Marshal(item)
			if err != nil {
				panic(err)
			}
			text = string(encoded)
		}
		attributes[name] = text
	}
	encoded, err := json.Marshal(attributes)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// TestHistogramValidationMatchesStock pins the validation both appenders now
// share. The N0 oracle originally recorded a divergence: the columnar appender
// accepted an internally inconsistent native histogram that stock TSDB rejected.
// The defect has since been closed by validating with the model in the columnar
// appender. This test keeps both routes on the same semantics and fails if
// either moves again.
func TestHistogramValidationMatchesStock(t *testing.T) {
	// Count 4, but the two positive deltas state 1 and 4 observations and the zero
	// count is 0, so the buckets state 5 observations.
	inconsistent := &histogram.Histogram{
		Count: 4, Sum: 10.5, Schema: 0,
		PositiveSpans: []histogram.Span{{Offset: 1, Length: 2}}, PositiveBuckets: []int64{1, 3},
	}

	ls := labels.FromStrings("__name__", "inconsistent_histogram")
	columnarRoute := contractNewRoute(t, contractRouteColumnar, 0)
	_, columnarErr := columnarRoute.appendable().Append(0, ls, 0, 1000, 0, inconsistent, nil, storage.AOptions{})
	tsdbRoute := contractNewRoute(t, contractRouteTSDB, 0)
	_, tsdbErr := tsdbRoute.appendable().Append(0, ls, 0, 1000, 0, inconsistent, nil, storage.AOptions{})

	require.ErrorIs(t, columnarErr, histogram.ErrHistogramCountMismatch,
		"the columnar appender must reject a histogram whose buckets state more observations than Count")
	require.ErrorIs(t, tsdbErr, histogram.ErrHistogramCountMismatch,
		"stock TSDB rejects a histogram whose buckets state more observations than Count")
	require.ErrorContains(t, columnarErr, "observations found in buckets")
	require.ErrorContains(t, tsdbErr, "observations found in buckets")
}
