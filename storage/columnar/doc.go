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

// Package columnar is an experimental, write-once columnar block store for
// Prometheus samples.
//
// It exists to answer two questions with measurements rather than assertions:
// whether an Arrow-native ingestion path is cheaper than the existing
// row-oriented one, and whether a columnar store can serve PromQL
// competitively.
//
// Blocks are written once and never modified. Sample blocks and native metadata
// are restored from blocks; unflushed batches replay through Arrow IPC WAL
// segments. CompactTo writes a separate measurement block. Compact publishes a
// synchronous parent-replacing snapshot and RetainBefore retires whole expired
// blocks in the lifecycle experiment. Nothing here is safe to run in production.
// Format versions are discarded rather than migrated.
//
// Each block has a hash-sorted series dictionary, sorted label/value postings
// and per-series segment offsets. Sample Parquet row groups are cohort slices
// with one row per time window: equal timestamp vectors are stored once and
// each row holds independent typed payload columns for its member series.
// Native metadata memberships and sample series dictionary entries both use
// labels.StableHash as their identity key.
package columnar

// FormatVersion is the version of the block format written by this package.
//
// It is bumped whenever the on-disk schema changes; blocks are not migrated.
const FormatVersion = 9
