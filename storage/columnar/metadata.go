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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/prometheus/prometheus/model/labels"
)

// Synchronisation of samples and their metadata is intentional here: this store
// is the authority for the owner identities before label promotion happens.

// MetadataKind is the ownership category of a metadata value.
//
// Kinds are not interchangeable. Resource attributes are stated by the owner
// Resource; scrape cadence belongs to its scrape source; scope schema URLs
// belong to their Scope/source. info() enriches Resource values, while values
// from other kinds stay in their own category and aren't automatically labels.
type MetadataKind uint8

const (
	MetadataResource MetadataKind = iota
	MetadataResourceSchema
	MetadataMetric
	MetadataScope
	MetadataScrape
	MetadataSource
)

// KindNames is the stable list of names used at API/index boundaries.
var KindNames = [...]string{"resource", "resource_schema", "metric", "scope", "scrape", "source"}

// String returns the kind's category name.
func (k MetadataKind) String() string {
	if k > MetadataSource {
		return fmt.Sprintf("metadata-kind(%d)", k)
	}
	return KindNames[k]
}

// OwnerKey identifies a metadata owner across blocks and series.
//
// ID is a canonical representation of the identity attributes, not a hash of
// descriptive Resource content. A descriptive content update must therefore keep
// using the same key. That's central to this model: metadata ownership survives
// independently of changes that aren't meant to affect series identity.
type OwnerKey struct {
	Kind MetadataKind
	ID   string
}

// Ref returns a stable, opaque identifier suitable for __otel_resource__.
// Identity derivation is done once at ingestion. The digest avoids exposing a
// long, composite identity as an ordinary user-facing string.
func (k OwnerKey) Ref() string {
	digest := sha256.Sum256([]byte(k.String()))
	return hex.EncodeToString(digest[:])
}

// String returns the fully scoped canonical identity.
func (k OwnerKey) String() string { return k.Kind.String() + "\x00" + k.ID }

// ResourceOwner returns the owner key of the known service identifying Resource
// attributes: service.name, service.namespace and service.instance.id.
//
// Inline entity identity is deliberately not invented here: before the prototype
// supports it, its identifying attributes must come from an agreed OTel contract.
// Descriptive properties never enter this key.
func ResourceOwner(attributes map[string]string) OwnerKey {
	return OwnerKey{Kind: MetadataResource, ID: canonicalAttributes(attributes, "service.name", "service.namespace", "service.instance.id")}
}

// ResourceSchemaOwner returns the owner of schema metadata for resource.
// Schema changes are versions of this owner; they do not become Resource
// attributes or alter resource's series memberships.
func ResourceSchemaOwner(resource OwnerKey) OwnerKey {
	return OwnerKey{Kind: MetadataResourceSchema, ID: resource.String()}
}

// ScopeOwner returns a scoped/schema owner key. A full instrumentation-scope
// attributes model is deferred; schema URL semantics are captured here.
func ScopeOwner(name, version string) OwnerKey {
	scopeID, err := jsonCanonical(map[string]string{"name": name, "version": version})
	if err != nil {
		// This json encoding only contains Go strings and cannot fail, so callers
		// need no error for the input.
		panic(err)
	}
	return OwnerKey{Kind: MetadataScope, ID: string(scopeID)}
}

// MetricOwner returns the metadata owner for one exact Prometheus series.
// Family name alone is insufficient: related series may share content, but their
// version lifecycles remain independent. Resource metadata stays a separate kind.
func MetricOwner(series labels.Labels) OwnerKey {
	return OwnerKey{Kind: MetadataMetric, ID: series.String()}
}

// SourceOwner identifies the ingestion source. Unlike metric identity, this
// ownership is not inferred from the current metric layout.
func SourceOwner(source string) OwnerKey {
	return OwnerKey{Kind: MetadataSource, ID: source}
}

// ScrapeOwner identifies the configuration owning scrape cadence.
func ScrapeOwner(job string) OwnerKey {
	return OwnerKey{Kind: MetadataScrape, ID: job}
}

// Content is metadata content, as string keys/values. Complex type choices and
// meanings belong to a kind's descriptor; they're not indiscriminately labels.
type Content struct {
	Attributes map[string]string
}

// Clone returns a deep copy. Owners/modules may reuse their input maps after
// Append/update calls.
func (c Content) Clone() Content {
	return Content{Attributes: maps.Clone(c.Attributes)}
}

// Canonical returns deterministic content bytes used for identity.
// Attribute ordering can't create duplicate metadata copies.
// Hash collisions can't be used to merge contexts: this digest is a lookup ID,
// but all semantic updates still compare the actual Content.
func (c Content) Canonical() string {
	return canonicalAttributes(c.Attributes)
}

// Update is a metadata observation. EffectiveFrom is authoritative as of the
// time supplied by the caller. It doesn't infer a timestamp from batch arrival,
// sample spacing or process wall-clock.
type Update struct {
	Owner         OwnerKey
	Content       Content
	EffectiveFrom int64
}

// Version is a metadata version with an inclusive begin time. The next version
// ends it exclusively when read; without a successor, validity is open-ended.
// ObservedFrom and ObservedUntil record actual observations, not validity bounds.
// Versions are not expanded into per-sample rows.
type Version struct {
	Content       Content
	EffectiveFrom int64
	ObservedFrom  int64
	ObservedUntil int64
}

// Association is the time-valid membership of one series in one owner.
// EffectiveFrom is inclusive. The next association of the same series and kind
// ends this membership exclusively; without one, membership is open-ended.
// ObservedUntil is an observation bound, not an expiry time.
type Association struct {
	Owner         OwnerKey
	EffectiveFrom int64
	ObservedUntil int64
}

// SourceSeriesKeys is all the stable identities a query needs, accumulated
// when an association first takes effect. Mapping these later from output labels
// isn't required, and would be unreliable after aggregation discarded them.
type SourceSeriesKeys struct {
	SeriesHash uint64
	Owner      OwnerKey
}

// OwnerHistory is a Resource/metric/source version history.
type OwnerHistory struct {
	Owner    OwnerKey
	Versions []Version
	// Truncated is set when history intentionally discarded older versions.
	Truncated bool
}

// SeriesMembership points a series at the metadata owners active for it.
type SeriesMembership struct {
	Owners []Association
}

// canonicalAttributes produces deterministic owner/content identity.
// A nil keys list includes all of the attributes; otherwise only the named
// identity attributes participate, while preserving attribute names.
func canonicalAttributes(attributes map[string]string, keys ...string) string {
	if keys == nil {
		keys = slices.Sorted(maps.Keys(attributes))
	}
	slices.Sort(keys)

	parts := make([]string, len(keys)*2)
	for i, key := range keys {
		parts[i*2] = key
		parts[i*2+1] = attributes[key]
	}

	encoded, err := json.Marshal(parts)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// jsonCanonical is the shared JSON canonicalization helper.
func jsonCanonical(value any) ([]byte, error) {
	return json.Marshal(value)
}
