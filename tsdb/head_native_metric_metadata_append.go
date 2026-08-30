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

package tsdb

import (
	"unique"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

const (
	maxNativeMetricMetadataHandles = 128 // Limits transient memory for high-cardinality metadata.
)

// nativeMetricMetadataAppender buffers metadata observations for one transaction
// until commit. It has a single owner and is not safe for concurrent use.
// Returning it through putAppender resets it for reuse. References into its
// mutable scratch storage must not survive its return to the pool.
type nativeMetricMetadataAppender struct {
	pending       map[chunks.HeadSeriesRef]nativeMetricMetadataPending
	handles       map[metadata.Metadata]unique.Handle[metadata.Metadata]
	seenHandles   map[unique.Handle[metadata.Metadata]]metadata.Metadata
	cacheActive   bool
	cacheDisabled bool
}

func newNativeMetricMetadataAppender() *nativeMetricMetadataAppender {
	return &nativeMetricMetadataAppender{
		pending:     make(map[chunks.HeadSeriesRef]nativeMetricMetadataPending),
		seenHandles: make(map[unique.Handle[metadata.Metadata]]metadata.Metadata),
	}
}

func (a *nativeMetricMetadataAppender) handle(m metadata.Metadata) unique.Handle[metadata.Metadata] {
	if a.cacheActive {
		if handle, ok := a.handles[m]; ok {
			return handle
		}
	}

	// Build the value-keyed cache only after a repeated handle proves that the
	// transaction contains reusable metadata.
	handle := unique.Make(m)
	if a.cacheDisabled {
		return handle
	}
	if a.cacheActive {
		if len(a.handles) == maxNativeMetricMetadataHandles {
			clear(a.handles)
			a.cacheActive = false
			a.cacheDisabled = true
		} else {
			a.handles[m] = handle
		}
		return handle
	}

	if _, ok := a.seenHandles[handle]; ok {
		if a.handles == nil {
			a.handles = make(map[metadata.Metadata]unique.Handle[metadata.Metadata])
		}
		for seenHandle, seenMetadata := range a.seenHandles {
			a.handles[seenMetadata] = seenHandle
		}
		clear(a.seenHandles)
		a.cacheActive = true
		return handle
	}
	if len(a.seenHandles) == maxNativeMetricMetadataHandles {
		clear(a.seenHandles)
		a.cacheDisabled = true
		return handle
	}
	a.seenHandles[handle] = m
	return handle
}

func (s *nativeMetricMetadataStore) getAppender() *nativeMetricMetadataAppender {
	if appender := s.appenderPool.Get(); appender != nil {
		return appender.(*nativeMetricMetadataAppender)
	}
	return newNativeMetricMetadataAppender()
}

func (s *nativeMetricMetadataStore) putAppender(appender *nativeMetricMetadataAppender) {
	clear(appender.pending)
	clear(appender.handles)
	clear(appender.seenHandles)
	appender.cacheActive = false
	appender.cacheDisabled = false
	s.appenderPool.Put(appender)
}
