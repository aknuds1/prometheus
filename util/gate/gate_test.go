// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGate(t *testing.T) {
	t.Run("start succeeds", func(t *testing.T) {
		g := New(1)

		require.NoError(t, g.Start(context.Background()))
		g.Done()
	})

	t.Run("start blocks when full", func(t *testing.T) {
		g := New(1)

		require.NoError(t, g.Start(context.Background()))

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		err := g.Start(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)

		g.Done()
	})

	t.Run("done panics when none started", func(t *testing.T) {
		g := New(1)
		require.Panics(t, func() { g.Done() })
	})
}
