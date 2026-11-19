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

// Decision 7's tagged recursive value representation. Stored content is still
// string-only, so the HTTP surface emits present strings and explicit absent
// entries. The remaining constructors define the accepted representation for
// typed content and are exercised by representation tests; they do not claim
// that the incumbent can ingest or persist those types.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// metadataTypedValue is one state-tagged JSON value. It is built as an ordered
// list of members rather than a map so every accepted form has one deterministic
// encoding.
type metadataTypedValue struct {
	state      string
	typ        string
	str        *string
	boolean    *bool
	decimal    *string
	bits       *string
	items      []metadataTypedValue
	entries    []metadataTypedMapEntry
	reason     *string
	assertions []metadataTypedValue
}

// metadataTypedMapEntry is one map entry. Keys are unique within a map.
type metadataTypedMapEntry struct {
	Key   string             `json:"key"`
	Value metadataTypedValue `json:"value"`
}

// metadataPresentString returns a present string value, including the empty
// string, which remains distinct from absent.
func metadataPresentString(value string) metadataTypedValue {
	return metadataTypedValue{state: "present", typ: "string", str: &value}
}

// metadataPresentBoolean returns a present boolean value.
func metadataPresentBoolean(value bool) metadataTypedValue {
	return metadataTypedValue{state: "present", typ: "boolean", boolean: &value}
}

// metadataPresentInteger returns a present signed 64-bit integer value.
func metadataPresentInteger(value int64) metadataTypedValue {
	decimal := strconv.FormatInt(value, 10)
	return metadataTypedValue{state: "present", typ: "integer", decimal: &decimal}
}

// metadataPresentDouble returns a present double from its exact IEEE-754 bits,
// preserving NaN payloads, signed zero, infinities and denormals.
func metadataPresentDouble(bits uint64) metadataTypedValue {
	pattern := fmt.Sprintf("0x%016x", bits)
	return metadataTypedValue{state: "present", typ: "double", bits: &pattern}
}

// metadataPresentArray returns a present array that preserves item order.
func metadataPresentArray(items []metadataTypedValue) metadataTypedValue {
	return metadataTypedValue{state: "present", typ: "array", items: slices.Clone(items)}
}

// metadataPresentMap returns a present map. Duplicate keys are rejected instead
// of selecting one by iteration order.
func metadataPresentMap(entries []metadataTypedMapEntry) (metadataTypedValue, error) {
	seen := make(map[string]struct{}, len(entries))
	cloned := make([]metadataTypedMapEntry, len(entries))
	for i, entry := range entries {
		if _, duplicate := seen[entry.Key]; duplicate {
			return metadataTypedValue{}, fmt.Errorf("columnar: duplicate typed map key %q", entry.Key)
		}
		seen[entry.Key] = struct{}{}
		cloned[i] = entry
	}
	slices.SortFunc(cloned, func(a, b metadataTypedMapEntry) int {
		switch {
		case a.Key < b.Key:
			return -1
		case a.Key > b.Key:
			return 1
		default:
			return 0
		}
	})
	return metadataTypedValue{state: "present", typ: "map", entries: cloned}, nil
}

// metadataNullValue returns an explicit typed null.
func metadataNullValue() metadataTypedValue { return metadataTypedValue{state: "null"} }

// metadataAbsentValue returns an authoritative absent resolution.
func metadataAbsentValue() metadataTypedValue { return metadataTypedValue{state: "absent"} }

// metadataClearedValue returns an explicit clear resolution.
func metadataClearedValue() metadataTypedValue { return metadataTypedValue{state: "cleared"} }

// metadataUnknownValue returns an unknown resolution with its evidence reason.
func metadataUnknownValue(reason string) metadataTypedValue {
	return metadataTypedValue{state: "unknown", reason: &reason}
}

// metadataAmbiguousValue returns an ambiguous resolution carrying the
// source-attributed assertions that disagree. No chosen top-level value is
// included.
func metadataAmbiguousValue(assertions []metadataTypedValue) metadataTypedValue {
	return metadataTypedValue{state: "ambiguous", assertions: slices.Clone(assertions)}
}

// MarshalJSON encodes the selected state/type payload fields. Irrelevant payload
// fields are omitted; an empty present array still encodes items: [].
func (v metadataTypedValue) MarshalJSON() ([]byte, error) {
	payload := map[string]any{"state": v.state}
	switch v.state {
	case "present":
		if v.typ == "" {
			return nil, errors.New("columnar: present typed value has no type")
		}
		payload["type"] = v.typ
		switch v.typ {
		case "string":
			if v.str == nil {
				return nil, errors.New("columnar: present string has no value")
			}
			payload["value"] = *v.str
		case "boolean":
			if v.boolean == nil {
				return nil, errors.New("columnar: present boolean has no value")
			}
			payload["value"] = *v.boolean
		case "integer":
			if v.decimal == nil {
				return nil, errors.New("columnar: present integer has no decimal")
			}
			payload["decimal"] = *v.decimal
		case "double":
			if v.bits == nil {
				return nil, errors.New("columnar: present double has no bits")
			}
			payload["bits"] = *v.bits
		case "array":
			items := v.items
			if items == nil {
				items = []metadataTypedValue{}
			}
			payload["items"] = items
		case "map":
			entries := v.entries
			if entries == nil {
				entries = []metadataTypedMapEntry{}
			}
			payload["entries"] = entries
		default:
			return nil, fmt.Errorf("columnar: unknown typed value type %q", v.typ)
		}
	case "null", "absent", "cleared":
	case "unknown":
		if v.reason == nil {
			return nil, errors.New("columnar: unknown typed value has no reason")
		}
		payload["reason"] = *v.reason
	case "ambiguous":
		assertions := v.assertions
		if assertions == nil {
			assertions = []metadataTypedValue{}
		}
		payload["assertions"] = assertions
	default:
		return nil, fmt.Errorf("columnar: unknown typed value state %q", v.state)
	}
	return json.Marshal(payload)
}

// UnmarshalJSON decodes one tagged value for tests and cursor round-trips.
func (v *metadataTypedValue) UnmarshalJSON(data []byte) error {
	var raw struct {
		State      string                  `json:"state"`
		Type       string                  `json:"type"`
		Value      json.RawMessage         `json:"value"`
		Decimal    *string                 `json:"decimal"`
		Bits       *string                 `json:"bits"`
		Items      []metadataTypedValue    `json:"items"`
		Entries    []metadataTypedMapEntry `json:"entries"`
		Reason     *string                 `json:"reason"`
		Assertions []metadataTypedValue    `json:"assertions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*v = metadataTypedValue{
		state: raw.State, typ: raw.Type, decimal: raw.Decimal, bits: raw.Bits,
		items: raw.Items, entries: raw.Entries, reason: raw.Reason, assertions: raw.Assertions,
	}
	if raw.State != "present" {
		return nil
	}
	switch raw.Type {
	case "string":
		var value string
		if err := json.Unmarshal(raw.Value, &value); err != nil {
			return fmt.Errorf("columnar: present string value: %w", err)
		}
		v.str = &value
	case "boolean":
		var value bool
		if err := json.Unmarshal(raw.Value, &value); err != nil {
			return fmt.Errorf("columnar: present boolean value: %w", err)
		}
		v.boolean = &value
	}
	return nil
}

// UnmarshalJSON decodes one map entry.
func (e *metadataTypedMapEntry) UnmarshalJSON(data []byte) error {
	var raw struct {
		Key   string             `json:"key"`
		Value metadataTypedValue `json:"value"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	e.Key, e.Value = raw.Key, raw.Value
	return nil
}

// metadataTypedValueCanonical is the semantic identity of one typed value. It
// uses type tags, length-delimited components, array order, sorted map entries
// and exact double bits, so equality never depends on JSON map iteration order.
func metadataTypedValueCanonical(v metadataTypedValue) string {
	var builder []byte
	appendPart := func(part string) {
		builder = append(builder, strconv.Itoa(len(part))...)
		builder = append(builder, ':')
		builder = append(builder, part...)
	}
	appendPart(v.state)
	if v.state != "present" {
		if v.reason != nil {
			appendPart(*v.reason)
		}
		if v.assertions != nil {
			appendPart(strconv.Itoa(len(v.assertions)))
			for _, assertion := range v.assertions {
				appendPart(metadataTypedValueCanonical(assertion))
			}
		}
		return string(builder)
	}
	appendPart(v.typ)
	switch v.typ {
	case "string":
		appendPart(*v.str)
	case "boolean":
		appendPart(strconv.FormatBool(*v.boolean))
	case "integer":
		appendPart(*v.decimal)
	case "double":
		appendPart(*v.bits)
	case "array":
		appendPart(strconv.Itoa(len(v.items)))
		for _, item := range v.items {
			appendPart(metadataTypedValueCanonical(item))
		}
	case "map":
		appendPart(strconv.Itoa(len(v.entries)))
		for _, entry := range v.entries {
			appendPart(entry.Key)
			appendPart(metadataTypedValueCanonical(entry.Value))
		}
	}
	return string(builder)
}

// metadataTypedValueDisplayKey is the deterministic display order for typed
// values: state, type, then the type's natural value order. Semantic equality
// still uses metadataTypedValueCanonical.
func metadataTypedValueDisplayKey(v metadataTypedValue) string {
	if v.state != "present" {
		return v.state + "\x00" + metadataTypedValueCanonical(v)
	}
	switch v.typ {
	case "string":
		return "present\x00string\x00" + *v.str
	case "boolean":
		return "present\x00boolean\x00" + strconv.FormatBool(*v.boolean)
	case "integer":
		return "present\x00integer\x00" + *v.decimal
	case "double":
		return "present\x00double\x00" + *v.bits
	default:
		return "present\x00" + v.typ + "\x00" + metadataTypedValueCanonical(v)
	}
}
