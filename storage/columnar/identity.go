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
	"encoding/binary"

	"github.com/prometheus/prometheus/model/labels"
)

// stableSeriesHash is deterministic across processes and restarts.
//
// It is not metadata-identity deduplication by an unchecked Bloom filter or
// process-local ref. Complete labels are checked before any series/metadata
// association is recorded. Encoded label strings preserve order and the
// identity bearing reserved label, including __otel_resource__.
func stableSeriesHash(ls labels.Labels) uint64 {
	var raw []byte
	lenBuf := make([]byte, 4)
	ls.Range(func(l labels.Label) {
		for _, value := range []string{l.Name, l.Value} {
			binary.BigEndian.PutUint32(lenBuf, uint32(len(value)))
			raw = append(raw, lenBuf...)
			raw = append(raw, value...)
		}
	})
	digest := sha256.Sum256(raw)
	return binary.BigEndian.Uint64(digest[:8])
}

// resourceLabel returns the exact label used in OTLP-derived metric series.
func resourceLabel(owner OwnerKey) labels.Label {
	return labels.Label{Name: OTELResourceLabel, Value: owner.Ref()}
}

// metadataSeriesKey is the series identity shared with storage.ResourceQuerier.
func metadataSeriesKey(ls labels.Labels) uint64 { return labels.StableHash(ls) }
