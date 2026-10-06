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

//go:build linux || darwin

package remote

import (
	"cmp"
	"slices"
	"testing"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
)

// The W1 sender adapter for unmodified ACTE in WAL-only mode: legacy Metadata
// records of each group's newest value, stored as the watcher passes them to
// queues. It has no WAL metadata interner, so it resolves nothing.

func w1SenderUnavailable(workload string) string {
	if workload == "mp" {
		return "legacy records hold one value per series"
	}
	return ""
}

// w1RecordFormat names this build's metadata record format for the byte
// evidence.
func w1RecordFormat() string { return "legacy" }

// w1EncodeRecord encodes one transaction's groups as WAL-only appends log
// them: in append order, which is ref order here, not Commit's stripe order.
func w1EncodeRecord(txn []w1Group) []byte {
	entries := make([]record.RefMetadata, 0, len(txn))
	for _, g := range txn {
		m := g.points[len(g.points)-1].m
		entries = append(entries, record.RefMetadata{Ref: g.ref, Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: m.Help})
	}
	slices.SortFunc(entries, func(a, b record.RefMetadata) int { return cmp.Compare(a.Ref, b.Ref) })
	var enc record.Encoder
	return enc.Metadata(entries, nil)
}

func w1NewQueueManager(tb testing.TB) *QueueManager {
	return newTestQueueManager(tb, testDefaultQueueConfig(), config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType)
}

// w1Decoder decodes records with reused buffers, and stores them as the
// watcher passes them to queues.
type w1Decoder struct {
	dec  record.Decoder
	meta []record.RefMetadata
}

func newW1Decoder() *w1Decoder { return &w1Decoder{} }

func (d *w1Decoder) decode(tb testing.TB, rec []byte) {
	var err error
	d.meta, err = d.dec.Metadata(rec, d.meta[:0])
	if err != nil {
		require.NoError(tb, err)
	}
}

func (d *w1Decoder) store(qm *QueueManager) {
	qm.StoreMetadata(d.meta)
}

// w1View returns legacy metadata as a history of one version.
func w1View(_ testing.TB, qm *QueueManager, ref chunks.HeadSeriesRef) (int, *metadata.Metadata) {
	return 1, qm.seriesMetadata[ref]
}

// w1UseInterner has no interner to replace or count.
func w1UseInterner(func(func(metadata.Metadata) *metadata.Metadata) func(metadata.Metadata) *metadata.Metadata) func() {
	return func() {}
}
