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

package remote

import (
	"hash/maphash"
	"strings"
	"sync"
	"unsafe"

	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/record"
)

const (
	metadataInternerEntries = 1 << 15
	metadataInternerBytes   = 4 << 20
	// The ledger keeps values seen once until a second sighting admits them.
	metadataInternerLedgerSets  = 1 << 11
	metadataInternerLedgerBytes = 1 << 20
	// Sets of ledger slots keep values that alternate in one set from evicting
	// each other before either is admitted.
	metadataLedgerWays = 4
)

// walMetadataInterner shares metadata decoded from the WAL across series and
// queues in this process.
var walMetadataInterner = newMetadataInterner(metadataInternerEntries, metadataInternerBytes)

// metadataInterner maps metadata values to shared immutable pointers. It keeps
// two generations, each bounded by entries and string bytes, and admits a value
// to them on its second sighting. Until then, a set-associative ledger may keep
// the first sighting's value, so that the second returns the same pointer; a
// slot holds a value only if its string bytes fit the ledger's bound. Full sets
// replace their slots in turn. A value that leaves the generations and the
// ledger stays valid but is no longer shared with new callers.
type metadataInterner struct {
	mtx            sync.Mutex
	current, older map[metadata.Metadata]*metadata.Metadata
	bytes          int
	entries, limit int

	fingerprint func(metadata.Metadata) uint64
	// ledger holds sets of metadataLedgerWays slots; next is each full set's
	// next slot to replace.
	ledger                   []metadataLedgerSlot
	next                     []uint8
	ledgerBytes, ledgerLimit int
}

// metadataLedgerSlot records a value's first sighting. A zero fingerprint
// marks an empty slot; value is nil if the value's strings did not fit.
type metadataLedgerSlot struct {
	fingerprint uint64
	value       *metadata.Metadata
}

func newMetadataInterner(entries, bytes int) *metadataInterner {
	return newLedgerMetadataInterner(entries, bytes, metadataInternerLedgerSets, metadataInternerLedgerBytes)
}

// newLedgerMetadataInterner returns an interner whose ledger has sets sets, a
// power of two, and keeps at most ledgerBytes string bytes.
func newLedgerMetadataInterner(entries, bytes, sets, ledgerBytes int) *metadataInterner {
	if sets <= 0 || sets&(sets-1) != 0 {
		panic("metadata interner ledger sets must be a power of two")
	}
	seed := maphash.MakeSeed()
	return &metadataInterner{
		current:     map[metadata.Metadata]*metadata.Metadata{},
		entries:     entries,
		limit:       bytes,
		fingerprint: func(m metadata.Metadata) uint64 { return maphash.Comparable(seed, m) },
		ledger:      make([]metadataLedgerSlot, sets*metadataLedgerWays),
		next:        make([]uint8, sets),
		ledgerLimit: ledgerBytes,
	}
}

// intern returns an immutable value equal to m. Callers must not modify it,
// and the interner may retain m's strings. Values larger than a generation's
// byte bound are never shared.
func (i *metadataInterner) intern(m metadata.Metadata) *metadata.Metadata {
	return i.internValue(m, false)
}

// internBorrowed is intern for m whose strings may alias memory that the
// caller reuses: it never retains them, and copies them into any value it
// creates.
func (i *metadataInterner) internBorrowed(m metadata.Metadata) *metadata.Metadata {
	return i.internValue(m, true)
}

func (i *metadataInterner) internValue(m metadata.Metadata, borrowed bool) *metadata.Metadata {
	cost := metadataCost(m)
	if cost > i.limit {
		return newInternedValue(m, borrowed)
	}
	i.mtx.Lock()
	defer i.mtx.Unlock()
	if v, ok := i.current[m]; ok {
		return v
	}
	v, ok := i.older[m]
	if !ok {
		var admit bool
		if v, admit = i.sightLocked(m, cost, borrowed); !admit {
			return v
		}
	}
	if len(i.current) >= i.entries || i.bytes+cost > i.limit {
		i.older, i.current, i.bytes = i.current, map[metadata.Metadata]*metadata.Metadata{}, 0
	}
	// Promotion keeps a value hit in the older generation shared after rotation.
	// The key is the value itself, so it never refers to borrowed strings.
	i.current[*v] = v
	i.bytes += cost
	return v
}

// sightLocked returns a value equal to m, which is in neither generation, and
// whether this sighting admits it. A second sighting returns the ledger's value
// if it kept one. Fingerprint collisions can only delay admission.
func (i *metadataInterner) sightLocked(m metadata.Metadata, cost int, borrowed bool) (*metadata.Metadata, bool) {
	fingerprint := i.fingerprint(m)
	if fingerprint == 0 {
		fingerprint = 1
	}
	set := int(fingerprint & uint64(len(i.next)-1))
	ways := i.ledger[set*metadataLedgerWays : (set+1)*metadataLedgerWays]
	free := -1
	for w := range ways {
		slot := &ways[w]
		if slot.fingerprint == fingerprint && (slot.value == nil || *slot.value == m) {
			v := slot.value
			i.releaseLocked(slot)
			if v == nil {
				v = newInternedValue(m, borrowed)
			}
			return v, true
		}
		if slot.fingerprint == 0 && free < 0 {
			free = w
		}
	}
	if free < 0 {
		free = int(i.next[set])
		i.next[set] = uint8((free + 1) % metadataLedgerWays)
	}
	slot := &ways[free]
	v := newInternedValue(m, borrowed)
	i.releaseLocked(slot)
	slot.fingerprint = fingerprint
	if cost <= i.ledgerLimit-i.ledgerBytes {
		slot.value = v
		i.ledgerBytes += cost
	}
	return v, false
}

func (i *metadataInterner) releaseLocked(slot *metadataLedgerSlot) {
	if slot.value != nil {
		i.ledgerBytes -= metadataCost(*slot.value)
	}
	*slot = metadataLedgerSlot{}
}

func metadataCost(m metadata.Metadata) int {
	return len(m.Type) + len(m.Unit) + len(m.Help)
}

// newInternedValue returns a new value equal to m, owning copies of m's
// strings if they are borrowed.
func newInternedValue(m metadata.Metadata, borrowed bool) *metadata.Metadata {
	if borrowed {
		return ownMetadata(m)
	}
	// Copy m only in this branch: taking the parameter's address would move it
	// to the heap on entry, allocating on the borrowed path too.
	owned := m
	return &owned
}

// ownMetadata returns a copy of m that does not alias m's memory. Known metric
// types are shared constants. The other non-empty strings share one exact-size
// payload, so retaining any of them retains all of them.
func ownMetadata(m metadata.Metadata) *metadata.Metadata {
	owned := &metadata.Metadata{Unit: m.Unit, Help: m.Help}
	typ := string(m.Type)
	if known := record.ToMetricType(record.GetMetricType(m.Type)); known == m.Type {
		owned.Type, typ = known, ""
	}
	// Concatenation cannot be used: it returns an operand when the others are
	// empty.
	fields := [...]*string{&typ, &owned.Unit, &owned.Help}
	size, count := 0, 0
	for _, f := range fields {
		if *f == "" {
			// An empty substring still refers to its backing.
			*f = ""
			continue
		}
		size += len(*f)
		count++
	}
	switch count {
	case 0:
	case 1:
		for _, f := range fields {
			*f = strings.Clone(*f)
		}
	default:
		payload := make([]byte, size)
		offset := 0
		for _, f := range fields {
			if *f == "" {
				continue
			}
			n := copy(payload[offset:], *f)
			*f = unsafe.String(&payload[offset], n)
			offset += n
		}
	}
	if typ != "" {
		owned.Type = model.MetricType(typ)
	}
	return owned
}
