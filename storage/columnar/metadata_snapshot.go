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

// Decision 7 pagination pins one local committed metadata snapshot per first
// page. The snapshot is an in-memory clone of the published catalog: creating it
// writes no samples, metadata versions, memberships or WAL records, and later
// commits, replay, flush and compaction cannot change it. No TTL, byte budget or
// admission policy is selected here; those belong to decision 9.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/prometheus/prometheus/model/labels"
)

// metadataCursorVersion is the accepted opaque cursor format version.
const metadataCursorVersion = 1

// metadataSnapshotRegistry owns the local snapshots and the per-process cursor
// integrity key and incarnation identity.
type metadataSnapshotRegistry struct {
	incarnation string
	key         []byte
	snapshots   map[string]*metadataSnapshot
}

// metadataSnapshot is one pinned local metadata read snapshot.
type metadataSnapshot struct {
	id      string
	catalog *metadataStore
	reads   *readStats
	// canceled, reclaimed and expired are explicit lifecycle states. No
	// production code sets expired until decision 9 selects a TTL; the class is
	// still defined so a future policy and its callers cannot misreport it.
	canceled        bool
	reclaimed       bool
	reclaimedReason string
	expired         bool
}

func newMetadataSnapshotRegistry() (*metadataSnapshotRegistry, error) {
	incarnation, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("columnar: metadata cursor key: %w", err)
	}
	return &metadataSnapshotRegistry{
		incarnation: incarnation, key: key, snapshots: map[string]*metadataSnapshot{},
	}, nil
}

// randomToken returns an opaque base64url token from n random bytes.
func randomToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("columnar: random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// pinMetadataSnapshot publishes one local snapshot of the committed catalog.
// The caller must not hold s.mtx.
func (s *Storage) pinMetadataSnapshot() (*metadataSnapshot, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.closed {
		return nil, metadataFailure(410, "metadata_snapshot_state_unavailable", "storage is closed")
	}
	if s.metadata == nil {
		return nil, metadataFailure(501, "metadata_disabled", "native metadata is disabled")
	}
	registry := s.metadataSnapshots
	if registry == nil {
		return nil, metadataFailure(500, "metadata_internal", "metadata snapshot registry is unavailable")
	}
	id, err := randomToken(16)
	if err != nil {
		return nil, metadataFailure(500, "metadata_internal", err.Error())
	}
	for {
		if _, exists := registry.snapshots[id]; !exists {
			break
		}
		if id, err = randomToken(16); err != nil {
			return nil, metadataFailure(500, "metadata_internal", err.Error())
		}
	}
	catalog := s.metadata.cloneForSnapshot()
	snapshot := &metadataSnapshot{id: id, catalog: catalog, reads: s.reads}
	if s.reads != nil {
		rows := len(catalog.bySeries)
		for _, history := range catalog.byOwner {
			rows += len(history.versions)
		}
		s.reads.metadataSnapshotRows.Add(uint64(rows))
	}
	registry.snapshots[id] = snapshot
	return snapshot, nil
}

// cloneForSnapshot deep-copies the mutable catalog structures a read needs. The
// clone shares no slices or maps with the published catalog; content maps are
// copied as well so even an accidental future in-place mutation cannot leak into
// a pinned page.
func (s *metadataStore) cloneForSnapshot() *metadataStore {
	out := &metadataStore{
		byOwner:  make(map[OwnerKey]*metadataOwnerHistory, len(s.byOwner)),
		bySeries: make(map[uint64]map[MetadataKind][]seriesMembershipRow, len(s.bySeries)),
		chains:   make(map[uint64]labels.Labels, len(s.chains)),
	}
	for owner, history := range s.byOwner {
		versions := make([]Version, len(history.versions))
		for i, version := range history.versions {
			versions[i] = Version{
				Content:       version.Content.Clone(),
				EffectiveFrom: version.EffectiveFrom,
				ObservedFrom:  version.ObservedFrom,
				ObservedUntil: version.ObservedUntil,
			}
		}
		out.byOwner[owner] = &metadataOwnerHistory{
			versions: versions, truncated: history.truncated, index: map[string]struct{}{},
		}
	}
	for hash, kinds := range s.bySeries {
		clonedKinds := make(map[MetadataKind][]seriesMembershipRow, len(kinds))
		for kind, rows := range kinds {
			clonedKinds[kind] = append([]seriesMembershipRow(nil), rows...)
		}
		out.bySeries[hash] = clonedKinds
	}
	maps.Copy(out.chains, s.chains)
	return out
}

// cancelMetadataSnapshot records explicit cancellation. Re-canceling a known
// canceled snapshot is idempotent; expired and reclaimed snapshots retain their
// respective classes.
func (s *Storage) cancelMetadataSnapshot(id string) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	registry := s.metadataSnapshots
	if registry == nil {
		return metadataFailure(500, "metadata_internal", "metadata snapshot registry is unavailable")
	}
	snapshot, ok := registry.snapshots[id]
	if !ok {
		return metadataFailure(404, "metadata_snapshot_unknown", fmt.Sprintf("metadata snapshot %q is unknown", id))
	}
	switch {
	case snapshot.expired:
		return metadataFailure(410, "metadata_snapshot_expired", fmt.Sprintf("metadata snapshot %q expired", id))
	case snapshot.reclaimed:
		return metadataFailure(410, "metadata_snapshot_reclaimed", fmt.Sprintf("metadata snapshot %q was reclaimed: %s", id, snapshot.reclaimedReason))
	case snapshot.canceled:
		return nil
	default:
		snapshot.canceled = true
		return nil
	}
}

// reclaimMetadataSnapshot explicitly invalidates a snapshot, for a future
// retention policy that chooses reclamation over keeping pinned facts readable.
// It is not called automatically: retention of the live store cannot affect a
// clone, and no retention budget is selected by decision 7.
func (s *Storage) reclaimMetadataSnapshot(id, reason string) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if snapshot, ok := s.metadataSnapshots.snapshots[id]; ok {
		snapshot.reclaimed = true
		snapshot.reclaimedReason = reason
	}
}

// expireMetadataSnapshotForTesting marks a snapshot expired. No production
// TTL exists yet, so this hook is test-only and unexported.
func (s *Storage) expireMetadataSnapshotForTesting(id string) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if snapshot, ok := s.metadataSnapshots.snapshots[id]; ok {
		snapshot.expired = true
	}
}

// metadataCursorSigned is the integrity-protected cursor payload. Position names
// the last returned logical object by complete canonical identity, never by a
// digest-only reference.
type metadataCursorSigned struct {
	Version     int             `json:"version"`
	Incarnation string          `json:"incarnation"`
	Snapshot    string          `json:"snapshot"`
	Binding     string          `json:"binding"`
	Position    json.RawMessage `json:"position"`
}

// metadataCursorEnvelope adds the signature over metadataCursorSigned.
type metadataCursorEnvelope struct {
	metadataCursorSigned
	Signature string `json:"signature"`
}

// encodeMetadataCursor builds one opaque, versioned, integrity-checked,
// query-bound and snapshot-bound cursor.
func (s *Storage) encodeMetadataCursor(snapshot *metadataSnapshot, binding string, position any) (string, error) {
	registry := s.metadataSnapshots
	if registry == nil {
		return "", metadataFailure(500, "metadata_internal", "metadata snapshot registry is unavailable")
	}
	rawPosition, err := json.Marshal(position)
	if err != nil {
		return "", metadataFailure(500, "metadata_internal", err.Error())
	}
	signed := metadataCursorSigned{
		Version: metadataCursorVersion, Incarnation: registry.incarnation,
		Snapshot: snapshot.id, Binding: binding, Position: rawPosition,
	}
	rawSigned, err := json.Marshal(signed)
	if err != nil {
		return "", metadataFailure(500, "metadata_internal", err.Error())
	}
	mac := hmac.New(sha256.New, registry.key)
	_, _ = mac.Write(rawSigned)
	envelope := metadataCursorEnvelope{metadataCursorSigned: signed, Signature: hex.EncodeToString(mac.Sum(nil))}
	rawEnvelope, err := json.Marshal(envelope)
	if err != nil {
		return "", metadataFailure(500, "metadata_internal", err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(rawEnvelope), nil
}

// decodeMetadataCursor validates the cursor and returns its snapshot and raw
// after-position. The binding must equal the current request's canonical query;
// only the page limit may change between requests.
func (s *Storage) decodeMetadataCursor(token, binding string) (*metadataSnapshot, json.RawMessage, error) {
	rawEnvelope, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, nil, metadataFailure(400, "metadata_cursor_malformed", "next_token is not base64url text")
	}
	var envelope metadataCursorEnvelope
	if err := json.Unmarshal(rawEnvelope, &envelope); err != nil {
		return nil, nil, metadataFailure(400, "metadata_cursor_malformed", "next_token is not a metadata cursor")
	}
	if envelope.Version == 0 {
		return nil, nil, metadataFailure(400, "metadata_cursor_malformed", "next_token is not a metadata cursor")
	}
	if envelope.Version != metadataCursorVersion {
		return nil, nil, metadataFailure(400, "metadata_cursor_version_unsupported",
			fmt.Sprintf("next_token uses unsupported cursor version %d", envelope.Version))
	}
	registry := s.metadataSnapshots
	if registry == nil {
		return nil, nil, metadataFailure(500, "metadata_internal", "metadata snapshot registry is unavailable")
	}
	// The incarnation is not secret and is checked before the signature so a token
	// from a previous store incarnation reports store_restarted even though that
	// process's integrity key is gone.
	if envelope.Incarnation != registry.incarnation {
		return nil, nil, metadataFailure(410, "metadata_snapshot_reclaimed",
			"metadata snapshot was reclaimed: store_restarted", "reason", "store_restarted")
	}
	rawSigned, err := json.Marshal(envelope.metadataCursorSigned)
	if err != nil {
		return nil, nil, metadataFailure(400, "metadata_cursor_corrupt", "next_token payload cannot be verified")
	}
	mac := hmac.New(sha256.New, registry.key)
	_, _ = mac.Write(rawSigned)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(envelope.Signature)) {
		return nil, nil, metadataFailure(400, "metadata_cursor_corrupt", "next_token integrity check failed")
	}
	if envelope.Binding != binding {
		return nil, nil, metadataFailure(400, "metadata_cursor_query_mismatch",
			"next_token was issued for a different metadata query")
	}
	if len(envelope.Position) == 0 || string(envelope.Position) == "null" {
		return nil, nil, metadataFailure(400, "metadata_cursor_corrupt", "next_token has no after-position")
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	snapshot, ok := registry.snapshots[envelope.Snapshot]
	if !ok {
		return nil, nil, metadataFailure(404, "metadata_snapshot_unknown",
			fmt.Sprintf("metadata snapshot %q is unknown", envelope.Snapshot))
	}
	switch {
	case snapshot.expired:
		return nil, nil, metadataFailure(410, "metadata_snapshot_expired",
			fmt.Sprintf("metadata snapshot %q expired", snapshot.id))
	case snapshot.reclaimed:
		return nil, nil, metadataFailure(410, "metadata_snapshot_reclaimed",
			fmt.Sprintf("metadata snapshot %q was reclaimed: %s", snapshot.id, snapshot.reclaimedReason),
			"reason", snapshot.reclaimedReason)
	case snapshot.canceled:
		return nil, nil, metadataFailure(410, "metadata_snapshot_canceled",
			fmt.Sprintf("metadata snapshot %q was canceled", snapshot.id))
	case snapshot.catalog == nil:
		return nil, nil, metadataFailure(410, "metadata_snapshot_state_unavailable",
			fmt.Sprintf("metadata snapshot %q has no readable state", snapshot.id))
	}
	return snapshot, envelope.Position, nil
}

// valid reports whether an owner position names a complete owner identity.
func (p metadataOwnerPosition) valid() bool { return p.Kind != "" && p.ID != "" }

// valid reports whether a discovery position names an exact key.
func (p metadataDiscoveryPosition) valid() bool { return p.Key != "" }

// valid reports whether a series position names a complete series/owner state.
func (p metadataSeriesPosition) valid() bool {
	if p.Labels == "" {
		return false
	}
	switch p.State {
	case "applicable":
		return p.OwnerKind != "" && p.OwnerID != ""
	case "unassociated":
		return true
	default:
		return false
	}
}

// valid reports whether a diff position names a complete owner identity.
func (p metadataDiffPosition) valid() bool { return p.Kind != "" && p.ID != "" }

// valid reports whether a group position names a complete series group.
func (p metadataGroupPosition) valid() bool { return p.Labels != "" }

// metadataOwnerPosition is a complete canonical owner identity.
type metadataOwnerPosition struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// metadataDiscoveryPosition is one exact attribute key.
type metadataDiscoveryPosition struct {
	Key string `json:"key"`
}

// metadataSeriesPosition is one series/owner membership position. State keeps
// applicable and unassociated rows collision-safe.
type metadataSeriesPosition struct {
	Labels    string `json:"labels"`
	State     string `json:"state"`
	OwnerKind string `json:"owner_kind,omitempty"`
	OwnerID   string `json:"owner_id,omitempty"`
	FromMS    int64  `json:"from_ms"`
}

// metadataDiffPosition is one owner identity at a numeric effective position.
type metadataDiffPosition struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	From int64  `json:"from_ms"`
}

// metadataGroupPosition is one combined logical group. Relation is empty until
// authoritative Entity evidence is representable.
type metadataGroupPosition struct {
	Labels    string `json:"labels"`
	Relation  string `json:"relation"`
	OwnerKind string `json:"owner_kind,omitempty"`
	OwnerID   string `json:"owner_id,omitempty"`
	FromMS    int64  `json:"from_ms"`
}

func compareMetadataOwnerPosition(a, b metadataOwnerPosition) int {
	if a.Kind < b.Kind {
		return -1
	}
	if a.Kind > b.Kind {
		return 1
	}
	switch {
	case a.ID < b.ID:
		return -1
	case a.ID > b.ID:
		return 1
	default:
		return 0
	}
}

func compareMetadataSeriesPosition(a, b metadataSeriesPosition) int {
	switch {
	case a.Labels < b.Labels:
		return -1
	case a.Labels > b.Labels:
		return 1
	}
	switch {
	case a.State < b.State:
		return -1
	case a.State > b.State:
		return 1
	}
	if a.OwnerKind < b.OwnerKind {
		return -1
	}
	if a.OwnerKind > b.OwnerKind {
		return 1
	}
	switch {
	case a.OwnerID < b.OwnerID:
		return -1
	case a.OwnerID > b.OwnerID:
		return 1
	}
	switch {
	case a.FromMS < b.FromMS:
		return -1
	case a.FromMS > b.FromMS:
		return 1
	default:
		return 0
	}
}

func compareMetadataDiffPosition(a, b metadataDiffPosition) int {
	if comparison := compareMetadataOwnerPosition(
		metadataOwnerPosition{Kind: a.Kind, ID: a.ID},
		metadataOwnerPosition{Kind: b.Kind, ID: b.ID},
	); comparison != 0 {
		return comparison
	}
	switch {
	case a.From < b.From:
		return -1
	case a.From > b.From:
		return 1
	default:
		return 0
	}
}

func compareMetadataGroupPosition(a, b metadataGroupPosition) int {
	seriesComparison := compareMetadataSeriesPosition(
		metadataSeriesPosition{Labels: a.Labels, State: a.Relation, OwnerKind: a.OwnerKind, OwnerID: a.OwnerID, FromMS: a.FromMS},
		metadataSeriesPosition{Labels: b.Labels, State: b.Relation, OwnerKind: b.OwnerKind, OwnerID: b.OwnerID, FromMS: b.FromMS},
	)
	return seriesComparison
}
