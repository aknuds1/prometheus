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
// one of six statuses:
//
//   - passing:    an executable test or fixture asserts the complete obligation.
//   - partial:    some clauses have executable passing evidence, while other
//                 clauses are defective, unsupported or blocked by a named
//                 decision. A partial obligation must name the demonstrated
//                 subset in its note and link the non-passing remainder through
//                 `gaps` and/or `decisions`; it may never be read as covered.
//   - defect:     an executable test reproduces incumbent behaviour that the
//                 contract does not accept; it is recorded, not repaired here.
//   - unsupported: the incumbent cannot express the behaviour at all.
//   - blocked:    the behaviour needs an open semantic decision, named here.
//   - recorded:   the obligation is a reporting or architecture-scope rule with
//                 no executable check.
//
// The test also proves that every evidence entry names a real Go test function
// in a real file, that every listed fixture exists and lists the obligation
// back, that every gap names a non-passing obligation that really exists, and
// that every open decision is attached to at least one blocked obligation.
//
// Decisions distinguish `open` from `decided`. A decided decision records a
// resolution, an authority and a date and may stay attached as provenance; it
// never makes a partial obligation look covered and it never blocks a case. A
// target case that the incumbent cannot execute is recorded in its fixture as a
// gap-controlled absence and must name a non-passing obligation, so accepted but
// unimplemented semantics stay visible instead of becoming passing cases.

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
	// Gaps names the non-passing obligations that carry a partial obligation's
	// undemonstrated clauses. It keeps a compound obligation from looking wholly
	// covered when a clause is a recorded defect, an unsupported capability or an
	// independently blocked gap.
	Gaps []string `json:"gaps"`
	Note string   `json:"note"`
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

// contractDecision is one semantic decision. An open decision names who still
// holds it; a decided decision records the resolution, its authority and date.
type contractDecision struct {
	ID         string `json:"id"`
	Question   string `json:"question"`
	Status     string `json:"status"`
	Authority  string `json:"authority"`
	Date       string `json:"date"`
	Resolution string `json:"resolution,omitempty"`
	Limits     string `json:"limits,omitempty"`
	HeldBy     string `json:"held_by,omitempty"`
}

// contractStatuses are the permitted coverage statuses.
var contractStatuses = []string{"passing", "partial", "defect", "unsupported", "blocked", "recorded"}

// contractDecisionStatuses are the permitted decision statuses.
var contractDecisionStatuses = []string{"open", "decided"}

// contractDecidedDecisions must be decided by this package. Decisions 1, 2, 3
// and 4 were accepted by the user/product owner on 2026-11-15.
var contractDecidedDecisions = []string{"1", "2", "3", "4"}

// contractOpenDecisions must remain open here: this package may not guess a
// resolution from incumbent behaviour.
var contractOpenDecisions = []string{"5", "6", "7", "8", "9", "10"}

// contractRequiredTargetCases pins accepted target behaviour that the incumbent
// cannot execute. Removing or renaming one of these absences is a coverage
// regression, not a cleanup.
var contractRequiredTargetCases = []string{
	"identity-absent-vs-empty-fallback",
	"identity-candidate-collision",
	"identity-entity-adoption",
	"identity-entity-description-only",
	"identity-entity-malformed-duplicate-rejection",
	"identity-entity-schema-url",
	"identity-entity-typed-content",
	"identity-entity-typed-ids",
	"k8s-authoritative-uid-identity",
	"k8s-combined-view-separate-assertions",
	"k8s-discovery-no-payload-reads",
	"k8s-entityref-atomic-rejection",
	"k8s-missing-identity-unresolved",
	"k8s-partial-does-not-clear",
	"k8s-runtime-execution-distinct",
	"k8s-supplied-grouping-and-provenance",
	"state-association-termination-reassociation",
	"state-clear-diff-state-encoding",
	"state-complete-omission-vs-partial-unknown",
	"state-explicit-key-clear-reappearance",
	"state-idempotent-clear-termination",
	"state-missing-value-filtering",
	"state-owner-termination-conflict-rollback",
	"state-tombstone-lifecycle-durability",
	"state-truncated-read-reporting",
	"state-typed-present-null-absent",
}

// contractGroups are the permitted obligation groups.
var contractGroups = []string{"fixed-case", "invariant", "route", "route-difference", "capability-gap", "concurrency"}

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
		require.Contains(t, contractDecisionStatuses, decision.Status,
			"decision %q has unknown status %q", decision.ID, decision.Status)
		require.NotEmpty(t, decision.Question, "decision %q needs its question", decision.ID)
		switch decision.Status {
		case "open":
			require.Empty(t, decision.Authority,
				"open decision %q must not carry an authority", decision.ID)
			require.Empty(t, decision.Date,
				"open decision %q must not carry a date", decision.ID)
			require.Empty(t, decision.Resolution,
				"open decision %q must not carry a resolution", decision.ID)
			require.NotEmpty(t, decision.HeldBy,
				"open decision %q must state who still holds it", decision.ID)
		case "decided":
			require.NotEmpty(t, decision.Authority,
				"decided decision %q must name its authority", decision.ID)
			require.Regexp(t, `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`, decision.Date,
				"decided decision %q must state its decision date", decision.ID)
			require.NotEmpty(t, decision.Resolution,
				"decided decision %q must record its resolution", decision.ID)
			require.NotEmpty(t, decision.Limits,
				"decided decision %q must record its scope limits or exclusions", decision.ID)
			require.Empty(t, decision.HeldBy,
				"decided decision %q cannot still be held by someone", decision.ID)
		}
		decisionByID[decision.ID] = decision
	}
	// The register is an exact set: deleting or renaming a decision fails here,
	// including a decision that has since been answered.
	require.Len(t, decisionByID, 10, "the decision register must contain exactly decisions 1-10")
	for _, id := range contractDecidedDecisions {
		decision, ok := decisionByID[id]
		require.True(t, ok, "semantic decision %s is missing from the register", id)
		require.Equal(t, "decided", decision.Status,
			"decision %s is accepted and must be recorded as decided", id)
	}
	for _, id := range contractOpenDecisions {
		decision, ok := decisionByID[id]
		require.True(t, ok, "semantic decision %s is missing from the register", id)
		require.Equal(t, "open", decision.Status,
			"decision %s is not answered by this package and must stay open", id)
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
			openDecision := false
			for _, id := range obligation.Decisions {
				decision, ok := decisionByID[id]
				require.True(t, ok, "%s names unknown decision %q", obligation.ID, id)
				if decision.Status == "open" {
					openDecision = true
					if obligation.Status == "blocked" {
						blockedDecisions[id] = true
					}
				}
				if obligation.Status == "blocked" {
					require.Equal(t, "open", decision.Status,
						"%s is blocked by decision %q, which is already decided", obligation.ID, id)
				}
			}
			if obligation.Status == "blocked" {
				require.True(t, openDecision,
					"%s is blocked but names no open decision", obligation.ID)
			}
			switch obligation.Status {
			case "passing", "defect":
				require.NotEmpty(t, obligation.Evidence,
					"%s is %s but names no executable test", obligation.ID, obligation.Status)
			case "blocked":
				require.NotEmpty(t, obligation.Decisions,
					"%s is blocked but names no semantic decision", obligation.ID)
			case "partial":
				// A partial obligation demonstrates some clauses and must say which
				// remainder it is not claiming, so it can never read as covered. A
				// decided decision is provenance, not a non-passing remainder: only an
				// open decision or a named non-passing gap keeps the remainder visible.
				require.NotEmpty(t, obligation.Evidence,
					"%s is partial but names no executable evidence for its demonstrated subset", obligation.ID)
				require.True(t, openDecision || len(obligation.Gaps) > 0,
					"%s is partial but links neither an open decision nor a non-passing gap", obligation.ID)
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

			for _, id := range obligation.Gaps {
				gap, ok := byID[id]
				require.True(t, ok, "%s names unknown gap %q", obligation.ID, id)
				require.NotEqual(t, obligation.ID, id, "%s cannot be its own gap", obligation.ID)
				require.NotEqual(t, "passing", gap.Status,
					"%s names %q as a gap, but that obligation is passing; a gap must be non-passing", obligation.ID, id)
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
			require.Contains(t, []string{contractRouteColumnar, contractRouteTSDB, contractRouteTSDBST}, route,
				"fixture %q names unknown route %q", fixture.ID, route)
		}
	}

	// Every open decision must be attached to at least one blocked obligation, so
	// the register cannot grow without a corresponding blocked case. A decided
	// decision may stay attached as provenance and needs no blocked obligation.
	for id, decision := range decisionByID {
		if decision.Status != "open" {
			continue
		}
		require.True(t, blockedDecisions[id],
			"decision %q is open but no blocked obligation names it; the decision register and the coverage report disagree", id)
	}

	// A target case is accepted semantics the incumbent cannot execute. It must
	// name a non-passing obligation and the fixture must declare that obligation,
	// so it cannot quietly become a passing case or disappear.
	targetCaseObligation := map[string]string{}
	targetCaseFixture := map[string]string{}
	for _, fixture := range fixtures {
		for _, target := range fixture.TargetCases {
			require.NotEmpty(t, target.ID, "fixture %q has a target case without an id", fixture.ID)
			require.NotContains(t, targetCaseObligation, target.ID, "duplicate target case %q", target.ID)
			require.NotEmpty(t, target.Accepted, "target case %q must state the accepted behaviour", target.ID)
			require.NotEmpty(t, target.Reason, "target case %q must state why it cannot execute", target.ID)
			require.Contains(t, fixture.Obligations, target.Obligation,
				"fixture %q must declare obligation %q for target case %q", fixture.ID, target.Obligation, target.ID)
			targetCaseObligation[target.ID] = target.Obligation
			targetCaseFixture[target.ID] = fixture.ID
		}
	}
	for id, obligationID := range targetCaseObligation {
		obligation, ok := byID[obligationID]
		require.True(t, ok, "target case %q names unknown obligation %q", id, obligationID)
		require.NotEqual(t, "passing", obligation.Status,
			"target case %q is attached to passing obligation %q; an unexecuted case cannot be coverage", id, obligationID)
		require.Contains(t, obligation.Fixtures, targetCaseFixture[id],
			"obligation %q must list fixture %q for target case %q", obligationID, targetCaseFixture[id], id)
	}
	for _, id := range contractRequiredTargetCases {
		require.Contains(t, targetCaseObligation, id,
			"required target case %q is missing; an accepted but unimplemented case may not disappear", id)
	}

	// The populated statuses keep the report honest: a suite that silently marks
	// everything passing is not a coverage report.
	statuses := map[string]int{}
	for _, obligation := range manifest.Obligations {
		statuses[obligation.Status]++
	}
	for _, status := range []string{"passing", "partial", "unsupported", "blocked", "defect", "recorded"} {
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
