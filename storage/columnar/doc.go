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
// Nothing here is durable, compacted, or safe to run in production. A block is
// written once and never modified; there is no WAL, no crash recovery, and no
// deletion. Format versioning exists so that prototypes are discarded rather
// than migrated.
//
// The data model is series-major: one row per series, with its labels and its
// samples as nested lists. That matches PromQL's unit of computation and keeps
// series identity in one place instead of repeating labels on every sample.
package columnar

// FormatVersion is the version of the block format written by this package.
//
// It is bumped whenever the on-disk schema changes; blocks are not migrated.
const FormatVersion = 2
