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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalVersionCacheUsesOwnerAndEviction(t *testing.T) {
	store := newMetadataStore(1)
	first := ResourceOwner(map[string]string{"service.name": "first"})
	second := ResourceOwner(map[string]string{"service.name": "second"})
	require.NoError(t, store.update(Update{Owner: first, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"host": "a"}}}))
	_ = store.snapshot()
	require.Len(t, store.canonicalVersions, 1)
	require.NoError(t, store.update(Update{Owner: first, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"host": "b"}}}))
	require.NoError(t, store.update(Update{Owner: second, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"host": "b"}}}))
	_ = store.snapshot()
	require.Len(t, store.canonicalVersions, 2)
	_, expired := store.canonicalVersions[metadataVersionKey{owner: first, effectiveFrom: 100}]
	require.False(t, expired)
	history, ok := store.byOwner[first]
	require.True(t, ok)
	require.True(t, history.truncated)
	store.deleteOwner(first)
	require.Len(t, store.canonicalVersions, 1)
	_, ok = store.byOwner[first]
	require.False(t, ok)
}

func TestMetadataCOWOwnerHistoryIsolation(t *testing.T) {
	base := newMetadataStore(5)
	owner := ResourceOwner(map[string]string{"service.name": "cow"})
	require.NoError(t, base.update(Update{Owner: owner, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"host": "a"}}}))
	candidate, err := base.clone()
	require.NoError(t, err)
	history := candidate.ownerHistory(owner)
	require.NotSame(t, base.byOwner[owner], history)
	require.NoError(t, candidate.update(Update{Owner: owner, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"host": "b"}}}))
	require.Len(t, base.byOwner[owner].versions, 1)
	require.Equal(t, "a", base.byOwner[owner].versions[0].Content.Attributes["host"])
}
