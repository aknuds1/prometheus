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

// metadataPipelineResetInterner gives the next repetition a new, empty
// metadata interner, as a fresh process has.
func metadataPipelineResetInterner() {
	walMetadataInterner = newMetadataInterner(metadataInternerEntries, metadataInternerBytes)
}

// metadataPipelineInterner returns the current metadata interner.
func metadataPipelineInterner() any {
	return walMetadataInterner
}

// metadataPipelineInternerSize returns how many values the current interner
// holds, in its generations and its ledger.
func metadataPipelineInternerSize() int {
	i := walMetadataInterner
	i.mtx.Lock()
	defer i.mtx.Unlock()
	n := len(i.current) + len(i.older)
	for _, slot := range i.ledger {
		if slot.fingerprint != 0 {
			n++
		}
	}
	return n
}
