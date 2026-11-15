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

// The manifest is the machine-readable coverage record for the contract-oracle
// foundation. It exists so an unsupported capability, a reproduced defect or a
// blocked semantic decision cannot quietly become a passing case by being
// deleted, renamed or skipped.
//
// Every fixed case and invariant in the assignment must appear exactly once with
// one of five statuses:
//
//   - passing:    an executable test or fixture asserts the obligation.
//   - defect:     an executable test reproduces incumbent behaviour that the
//                 contract does not accept; it is recorded, not repaired here.
//   - unsupported: the incumbent cannot express the behaviour at all.
//   - blocked:    the behaviour needs an open semantic decision, named here.
//   - recorded:   the obligation is a reporting or architecture-scope rule with
//                 no executable check.
//
// The test also proves that every evidence entry names a real Go test function
// in a real file, that every listed fixture exists and lists the obligation
// back, and that every open decision is attached to at least one blocked
// obligation.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// contractManifest is the coverage manifest.
type contractManifest struct {
	Obligations []contractObligation `json:"obligations"`
}

// contractObligation is one contract obligation and its evidence.
type contractObligation struct {
	ID        string             `json:"id"`
	Group     string             `json:"group"`
	Title     string             `json:"title"`
	Status    string             `json:"status"`
	Evidence  []contractEvidence `json:"evidence"`
	Fixtures  []string           `json:"fixtures"`
	Decisions []string           `json:"decisions"`
	Note      string             `json:"note"`
}

// contractEvidence names one executable test.
type contractEvidence struct {
	Path string `json:"path"`
	Test string `json:"test"`
}

// contractDecisionFile is the semantic decision register.
type contractDecisionFile struct {
	Decisions []contractDecision `json:"decisions"`
}

// contractDecision is one unresolved semantic decision.
type contractDecision struct {
	ID        string `json:"id"`
	Question  string `json:"question"`
	Status    string `json:"status"`
	Authority string `json:"authority"`
	Date      string `json:"date"`
	HeldBy    string `json:"held_by"`
}

// contractStatuses are the permitted coverage statuses.
var contractStatuses = []string{"passing", "defect", "unsupported", "blocked", "recorded"}

// contractGroups are the permitted obligation groups.
var contractGroups = []string{"fixed-case", "invariant", "route", "route-difference", "capability-gap"}

// TestContractManifest is the coverage gate.
func TestContractManifest(t *testing.T) {
	manifest := contractLoadManifest(t)
	decisions := contractLoadDecisions(t)
	fixtures := contractLoadFixtures(t)

	byID := make(map[string]contractObligation, len(manifest.Obligations))
	for _, obligation := range manifest.Obligations {
		require.NotEmpty(t, obligation.ID, "an obligation needs an id")
		require.NotContains(t, byID, obligation.ID, "duplicate obligation %q", obligation.ID)
		byID[obligation.ID] = obligation
	}

	// The assignment's fixed cases and invariants must all be present, once each.
	for i := 1; i <= 13; i++ {
		require.Contains(t, byID, "fixed-case-"+itoa(i),
			"fixed case %d has no manifest entry; an obligation may not disappear", i)
	}
	for i := 1; i <= 17; i++ {
		require.Contains(t, byID, "invariant-"+itoa(i),
			"invariant %d has no manifest entry; an obligation may not disappear", i)
	}

	decisionByID := make(map[string]contractDecision, len(decisions.Decisions))
	for _, decision := range decisions.Decisions {
		require.NotEmpty(t, decision.ID)
		require.NotContains(t, decisionByID, decision.ID, "duplicate decision %q", decision.ID)
		require.Equal(t, "open", decision.Status,
			"decision %q must be open: N0 may not resolve a semantic decision", decision.ID)
		require.NotEmpty(t, decision.Question, "decision %q needs its question", decision.ID)
		decisionByID[decision.ID] = decision
	}
	for i := 1; i <= 10; i++ {
		require.Contains(t, decisionByID, itoa(i),
			"semantic decision %d is missing from the register", i)
	}
	blockedDecisions := map[string]bool{}

	fixtureByID := make(map[string]contractFixture, len(fixtures))
	for _, fixture := range fixtures {
		fixtureByID[fixture.ID] = fixture
	}
	referencedFixtures := map[string]bool{}

	for _, obligation := range manifest.Obligations {
		t.Run(obligation.ID, func(t *testing.T) {
			require.NotEmpty(t, obligation.Title, "an obligation needs a title")
			require.Contains(t, contractGroups, obligation.Group, "unknown obligation group %q", obligation.Group)
			require.Contains(t, contractStatuses, obligation.Status, "unknown status %q", obligation.Status)
			switch obligation.Status {
			case "passing", "defect":
				require.NotEmpty(t, obligation.Evidence,
					"%s is %s but names no executable test", obligation.ID, obligation.Status)
			case "blocked":
				require.NotEmpty(t, obligation.Decisions,
					"%s is blocked but names no semantic decision", obligation.ID)
			}
			if obligation.Status != "passing" {
				require.NotEmpty(t, obligation.Note,
					"%s is %s and must state why in its note", obligation.ID, obligation.Status)
			}

			for _, evidence := range obligation.Evidence {
				require.NotEmpty(t, evidence.Path, "%s has an evidence entry without a path", obligation.ID)
				require.NotEmpty(t, evidence.Test, "%s has an evidence entry without a test", obligation.ID)
				contractRequireGoTest(t, evidence)
			}

			for _, id := range obligation.Decisions {
				decision, ok := decisionByID[id]
				require.True(t, ok, "%s names unknown decision %q", obligation.ID, id)
				require.Equal(t, "open", decision.Status)
				if obligation.Status == "blocked" {
					blockedDecisions[id] = true
				}
			}

			for _, id := range obligation.Fixtures {
				fixture, ok := fixtureByID[id]
				require.True(t, ok, "%s names unknown fixture %q", obligation.ID, id)
				require.Contains(t, fixture.Obligations, obligation.ID,
					"fixture %q does not declare obligation %q", id, obligation.ID)
				referencedFixtures[id] = true
			}
		})
	}

	// Every fixture must be reachable from the manifest, declare known
	// obligations and reference only open decisions.
	for _, fixture := range fixtures {
		require.NotEmpty(t, fixture.Obligations, "fixture %q declares no obligation", fixture.ID)
		require.True(t, referencedFixtures[fixture.ID],
			"fixture %q is not referenced by any obligation; an unregistered fixture is not coverage", fixture.ID)
		require.NotEmpty(t, fixture.Scope, "fixture %q must state what it does and does not claim", fixture.ID)
		for _, obligation := range fixture.Obligations {
			require.Contains(t, byID, obligation, "fixture %q declares unknown obligation %q", fixture.ID, obligation)
		}
		for _, id := range fixture.OpenDecisions {
			decision, ok := decisionByID[id]
			require.True(t, ok, "fixture %q names unknown decision %q", fixture.ID, id)
			require.Equal(t, "open", decision.Status)
		}
		for _, route := range fixture.Routes {
			require.Contains(t, []string{contractRouteColumnar, contractRouteTSDB}, route,
				"fixture %q names unknown route %q", fixture.ID, route)
		}
	}

	// Every open decision must be attached to at least one blocked obligation, so
	// the register cannot grow without a corresponding blocked case.
	for id := range decisionByID {
		require.True(t, blockedDecisions[id],
			"decision %q is open but no blocked obligation names it; the decision register and the coverage report disagree", id)
	}

	// The populated statuses keep the report honest: a suite that silently marks
	// everything passing is not a coverage report.
	statuses := map[string]int{}
	for _, obligation := range manifest.Obligations {
		statuses[obligation.Status]++
	}
	for _, status := range []string{"passing", "unsupported", "blocked", "defect", "recorded"} {
		require.Positive(t, statuses[status], "the manifest must record at least one %q obligation", status)
	}
}

// contractLoadManifest reads the coverage manifest strictly.
func contractLoadManifest(t *testing.T) contractManifest {
	t.Helper()
	var manifest contractManifest
	contractDecodeStrict(t, filepath.Join(contractFixtureDir, "manifest.json"), &manifest)
	require.NotEmpty(t, manifest.Obligations)
	return manifest
}

// contractLoadDecisions reads the semantic decision register strictly.
func contractLoadDecisions(t *testing.T) contractDecisionFile {
	t.Helper()
	var decisions contractDecisionFile
	contractDecodeStrict(t, filepath.Join(contractFixtureDir, "decisions.json"), &decisions)
	require.NotEmpty(t, decisions.Decisions)
	return decisions
}

// contractDecodeStrict decodes one JSON document and rejects unknown fields.
func contractDecodeStrict(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(into), "%s must match the documented schema", path)
}

// contractRequireGoTest proves an evidence entry names a real test function in a
// real file. Paths are stated relative to the repository root.
func contractRequireGoTest(t *testing.T, evidence contractEvidence) {
	t.Helper()
	path := evidence.Path
	if local, ok := strings.CutPrefix(path, "storage/columnar/"); ok {
		path = local
	} else {
		path = filepath.Join("..", "..", path)
	}
	_, err := os.Stat(path)
	require.NoError(t, err, "evidence file %q does not exist", evidence.Path)
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	require.NoError(t, err, "evidence file %q does not parse", evidence.Path)
	names := make([]string, 0, len(file.Decls))
	found := false
	for _, decl := range file.Decls {
		if function, ok := decl.(*ast.FuncDecl); ok {
			names = append(names, function.Name.Name)
			if function.Name.Name == evidence.Test {
				found = true
			}
		}
	}
	require.True(t, found, "evidence file %q has no test %q (it defines %v)",
		evidence.Path, evidence.Test, names)
}

// itoa formats a small decision or case number.
func itoa(value int) string { return strconv.Itoa(value) }
