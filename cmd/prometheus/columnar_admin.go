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

package main

import (
	"context"
	"errors"
	"math"

	"github.com/oklog/ulid/v2"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/tsdb"
)

var errColumnarAdminUnsupported = errors.New("columnar prototype does not offer this TSDB administration operation")

// columnarLocalStorage adapts only the TSDB-flavored administrative surface to
// the prototype's real catalog. It must not simulate TSDB replay/cardinality or
// perform arbitrary sample deletion.
type columnarLocalStorage struct{ *columnar.Storage }

func (*columnarLocalStorage) CleanTombstones() error { return nil }

func (*columnarLocalStorage) Delete(context.Context, int64, int64, ...*labels.Matcher) error {
	return errColumnarAdminUnsupported
}

func (*columnarLocalStorage) Snapshot(string, bool) error {
	return errColumnarAdminUnsupported
}

func (c *columnarLocalStorage) Stats(_ string, _ int) (*tsdb.Stats, error) {
	var count uint64
	var minTime, maxTime int64 = math.MinInt64, math.MaxInt64
	blocks := c.Storage.BlockMetas()
	for _, meta := range blocks {
		if meta.NumSeries == 0 {
			continue
		}
		if count == 0 {
			minTime, maxTime = meta.MinTime, meta.MaxTime
		} else {
			if meta.MinTime < minTime {
				minTime = meta.MinTime
			}
			if meta.MaxTime > maxTime {
				maxTime = meta.MaxTime
			}
		}
		count += uint64(meta.NumSeries)
	}
	return &tsdb.Stats{NumSeries: count, MinTime: minTime, MaxTime: maxTime}, nil
}

func (c *columnarLocalStorage) WALReplayStatus() (tsdb.WALReplayStatus, error) {
	return tsdb.WALReplayStatus{}, errColumnarAdminUnsupported
}

func (c *columnarLocalStorage) BlockMetas() ([]tsdb.BlockMeta, error) {
	metas := c.Storage.BlockMetas()
	var out []tsdb.BlockMeta
	for _, meta := range metas {
		id, err := ulid.Parse(meta.ULID)
		if err != nil {
			return nil, err
		}
		metaStat := tsdb.BlockStats{
			NumSamples: uint64(meta.NumSamples), NumSeries: uint64(meta.NumSeries),
		}
		out = append(out, tsdb.BlockMeta{
			ULID: id, MinTime: meta.MinTime, MaxTime: meta.MaxTime, Stats: metaStat,
		})
	}
	return out, nil
}
