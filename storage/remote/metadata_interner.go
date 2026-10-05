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
)

// walMetadataInterner shares metadata decoded from the WAL across series and
// queues in this process.
var walMetadataInterner = newMetadataInterner(metadataInternerEntries, metadataInternerBytes)

// metadataInterner maps metadata values to shared immutable pointers. It keeps
// two generations, each bounded by entries and string bytes. A value that
// leaves both generations stays valid but is no longer shared with new callers.
type metadataInterner struct {
	mtx            sync.Mutex
	current, older map[metadata.Metadata]*metadata.Metadata
	bytes          int
	entries, limit int
}

func newMetadataInterner(entries, bytes int) *metadataInterner {
	return &metadataInterner{current: map[metadata.Metadata]*metadata.Metadata{}, entries: entries, limit: bytes}
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
	cost := len(m.Type) + len(m.Unit) + len(m.Help)
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
		v = newInternedValue(m, borrowed)
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
