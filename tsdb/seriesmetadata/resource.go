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

// Package seriesmetadata contains storage-neutral native metadata read models.
package seriesmetadata

// ResourceVersion is a snapshot of Resource-owned metadata selected at a query
// timestamp. Descriptive attributes come from the selected owner version.
// Identifying attributes are kept separate from descriptive metadata so query
// enrichment cannot accidentally redefine Prometheus series identity.
//
// Versions are selected by inclusive effective timestamps and the exclusive
// effective timestamp of their successor. Unlike the earlier per-series model,
// observation bounds are not validity bounds and are consequently not exposed
// as MinTime and MaxTime here.
type ResourceVersion struct {
	Identifying map[string]string
	Descriptive map[string]string
}
