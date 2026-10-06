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

// metadataPipelineResetInterner does nothing: this base has no process-wide
// metadata interner.
func metadataPipelineResetInterner() {}

// metadataPipelineInterner returns nil: this base has no metadata interner.
func metadataPipelineInterner() any {
	return nil
}

// metadataPipelineInternerSize returns 0: this base has no metadata interner.
func metadataPipelineInternerSize() int {
	return 0
}
