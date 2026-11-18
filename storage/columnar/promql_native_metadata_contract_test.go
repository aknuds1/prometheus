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

// Decision 6 accepts a native metadata reference grammar that the incumbent
// cannot parse. This file records that absence explicitly as incumbent
// evidence: it asserts the current parse failures and the legacy info()
// controls, and it does not implement, emulate or execute the accepted target
// expressions. The accepted cases remain target cases in
// testdata/contract/cases/promql_native_metadata_semantics.json.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
)

// TestPromQLNativeMetadataGrammarRejectedByIncumbent pins the incumbent parser
// rejection that decision-6 target cases depend on. The accepted grammar has no
// implementation, so every proposed form must fail to parse rather than being
// silently reinterpreted as an ordinary label or function.
func TestPromQLNativeMetadataGrammarRejectedByIncumbent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expr     string
		fragment string
	}{
		{name: "filter without value", expr: `native_probe_total ~~resource.zone`, fragment: "unexpected character: '~'"},
		{name: "filter with value", expr: `native_probe_total ~~resource.zone = "east"`, fragment: "unexpected character: '~'"},
		{name: "projection alias", expr: `native_probe_total ~resource.zone as resource_zone`, fragment: "unexpected character: '~'"},
		{name: "quoted dotted key", expr: `native_probe_total ~~resource."host.name"`, fragment: "unexpected character: '~'"},
		{name: "unicode key", expr: `native_probe_total ~~resource."主机名"`, fragment: "unexpected character: '~'"},
		{name: "escaped key", expr: `native_probe_total ~~resource."host\\name"`, fragment: "unexpected character: '~'"},
		{name: "source namespace", expr: `native_probe_total ~~source.transport = "otap"`, fragment: "unexpected character: '~'"},
		{name: "matrix step", expr: `native_probe_total[2m] ~~resource.zone = "east"`, fragment: "unexpected character: '~'"},
		{name: "selector matcher", expr: `native_probe_total{~~resource.zone="east"}`, fragment: "unexpected character inside braces: '~'"},
		{name: "by modifier", expr: `sum by (~~resource.zone) (native_probe_total)`, fragment: "unexpected character: '~'"},
		{name: "without modifier", expr: `sum without (~~resource.zone) (native_probe_total)`, fragment: "unexpected character: '~'"},
		{name: "state predicate", expr: `metadata_state(~~resource.zone, value)`, fragment: "unexpected character: '~'"},
		{name: "state predicate without reference", expr: `metadata_state(native_probe_total, value)`, fragment: `unknown function with name "metadata_state"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parser.NewParser(promqltest.TestParserOpts).ParseExpr(tc.expr)
			require.Error(t, err, "the incumbent must not appear to support %s", tc.expr)
			require.Contains(t, err.Error(), tc.fragment,
				"the incumbent rejection of %s must be a parse error, not a silent reinterpretation", tc.expr)
		})
	}

	// The legacy controls decision 6 preserves must still parse. Executing them
	// is covered by the executable events in the contract fixture.
	for _, expr := range []string{
		`native_probe_total`,
		`native_probe_total{zone="east"}`,
		`native_probe_total{zone!="base-zone"}`,
		`info(native_probe_total)`,
		`info(native_probe_total, {zone="east"})`,
		`sum(info(native_probe_total))`,
		`info(sum(native_probe_total))`,
	} {
		_, err := parser.NewParser(promqltest.TestParserOpts).ParseExpr(expr)
		require.NoError(t, err, "legacy form %s must keep parsing", expr)
	}
}
