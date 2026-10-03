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
	"sync"

	"github.com/prometheus/prometheus/model/metadata"
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
	cost := len(m.Type) + len(m.Unit) + len(m.Help)
	if cost > i.limit {
		return &m
	}
	i.mtx.Lock()
	defer i.mtx.Unlock()
	if v, ok := i.current[m]; ok {
		return v
	}
	v, ok := i.older[m]
	if !ok {
		v = &m
	}
	if len(i.current) >= i.entries || i.bytes+cost > i.limit {
		i.older, i.current, i.bytes = i.current, map[metadata.Metadata]*metadata.Metadata{}, 0
	}
	// Promotion keeps a value hit in the older generation shared after rotation.
	i.current[*v] = v
	i.bytes += cost
	return v
}
