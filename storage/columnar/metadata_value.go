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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	value := metadataTypedValue{state: "present", typ: "map", entries: slices.Clone(entries)}
	normalized, err := normalizeMetadataTypedValue(value)
	if err != nil {
		return metadataTypedValue{}, err
	}
	return normalized, nil
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
// source-attributed alternatives that disagree. Equal alternatives deduplicate;
// ambiguity requires at least two distinct alternatives to marshal.
func metadataAmbiguousValue(assertions []metadataTypedValue) metadataTypedValue {
	return metadataTypedValue{state: "ambiguous", assertions: dedupeMetadataTypedValues(assertions)}
}

// dedupeMetadataTypedValues sorts and removes semantically equal alternatives.
func dedupeMetadataTypedValues(values []metadataTypedValue) []metadataTypedValue {
	if len(values) == 0 {
		return nil
	}
	sorted := slices.Clone(values)
	slices.SortFunc(sorted, func(a, b metadataTypedValue) int {
		return strings.Compare(metadataTypedValueCanonical(a), metadataTypedValueCanonical(b))
	})
	return slices.CompactFunc(sorted, func(a, b metadataTypedValue) bool {
		return metadataTypedValueCanonical(a) == metadataTypedValueCanonical(b)
	})
}

// normalizeMetadataTypedValue validates one value and returns its canonical
// form: map entries sorted by key, ambiguous alternatives sorted and deduped, and
// arrays recursively normalized. It rejects invalid states, types, payloads and
// irrelevant fields instead of letting a later encoder or comparator panic.
func normalizeMetadataTypedValue(v metadataTypedValue) (metadataTypedValue, error) {
	switch v.state {
	case "present":
	case "null", "absent", "cleared":
		if v.typ != "" || v.str != nil || v.boolean != nil || v.decimal != nil || v.bits != nil ||
			len(v.items) != 0 || len(v.entries) != 0 || v.reason != nil || len(v.assertions) != 0 {
			return metadataTypedValue{}, fmt.Errorf("columnar: %s typed value carries a payload", v.state)
		}
		return metadataTypedValue{state: v.state}, nil
	case "unknown":
		if v.reason == nil || *v.reason == "" {
			return metadataTypedValue{}, errors.New("columnar: unknown typed value has no reason")
		}
		if v.typ != "" || v.str != nil || v.boolean != nil || v.decimal != nil || v.bits != nil ||
			len(v.items) != 0 || len(v.entries) != 0 || len(v.assertions) != 0 {
			return metadataTypedValue{}, errors.New("columnar: unknown typed value carries another payload")
		}
		return metadataTypedValue{state: v.state, reason: v.reason}, nil
	case "ambiguous":
		if v.typ != "" || v.str != nil || v.boolean != nil || v.decimal != nil || v.bits != nil ||
			len(v.items) != 0 || len(v.entries) != 0 || v.reason != nil {
			return metadataTypedValue{}, errors.New("columnar: ambiguous typed value carries another payload")
		}
		normalized := make([]metadataTypedValue, 0, len(v.assertions))
		for _, assertion := range v.assertions {
			item, err := normalizeMetadataTypedValue(assertion)
			if err != nil {
				return metadataTypedValue{}, err
			}
			normalized = append(normalized, item)
		}
		normalized = dedupeMetadataTypedValues(normalized)
		if len(normalized) < 2 {
			return metadataTypedValue{}, errors.New("columnar: ambiguous typed value needs at least two distinct alternatives")
		}
		return metadataTypedValue{state: v.state, assertions: normalized}, nil
	default:
		return metadataTypedValue{}, fmt.Errorf("columnar: unknown typed value state %q", v.state)
	}
	if v.typ == "" {
		return metadataTypedValue{}, errors.New("columnar: present typed value has no type")
	}
	switch v.typ {
	case "string":
		if v.str == nil {
			return metadataTypedValue{}, errors.New("columnar: present string has no value")
		}
		if v.boolean != nil || v.decimal != nil || v.bits != nil || len(v.items) != 0 ||
			len(v.entries) != 0 || v.reason != nil || len(v.assertions) != 0 {
			return metadataTypedValue{}, errors.New("columnar: present string carries another payload")
		}
		value := *v.str
		return metadataTypedValue{state: "present", typ: "string", str: &value}, nil
	case "boolean":
		if v.boolean == nil {
			return metadataTypedValue{}, errors.New("columnar: present boolean has no value")
		}
		if v.str != nil || v.decimal != nil || v.bits != nil || len(v.items) != 0 ||
			len(v.entries) != 0 || v.reason != nil || len(v.assertions) != 0 {
			return metadataTypedValue{}, errors.New("columnar: present boolean carries another payload")
		}
		value := *v.boolean
		return metadataTypedValue{state: "present", typ: "boolean", boolean: &value}, nil
	case "integer":
		if v.decimal == nil {
			return metadataTypedValue{}, errors.New("columnar: present integer has no decimal")
		}
		if err := validateMetadataInteger(*v.decimal); err != nil {
			return metadataTypedValue{}, err
		}
		if v.str != nil || v.boolean != nil || v.bits != nil || len(v.items) != 0 ||
			len(v.entries) != 0 || v.reason != nil || len(v.assertions) != 0 {
			return metadataTypedValue{}, errors.New("columnar: present integer carries another payload")
		}
		decimal := *v.decimal
		return metadataTypedValue{state: "present", typ: "integer", decimal: &decimal}, nil
	case "double":
		if v.bits == nil {
			return metadataTypedValue{}, errors.New("columnar: present double has no bits")
		}
		if err := validateMetadataDoubleBits(*v.bits); err != nil {
			return metadataTypedValue{}, err
		}
		if v.str != nil || v.boolean != nil || v.decimal != nil || len(v.items) != 0 ||
			len(v.entries) != 0 || v.reason != nil || len(v.assertions) != 0 {
			return metadataTypedValue{}, errors.New("columnar: present double carries another payload")
		}
		bits := *v.bits
		return metadataTypedValue{state: "present", typ: "double", bits: &bits}, nil
	case "array":
		if v.str != nil || v.boolean != nil || v.decimal != nil || v.bits != nil ||
			len(v.entries) != 0 || v.reason != nil || len(v.assertions) != 0 {
			return metadataTypedValue{}, errors.New("columnar: present array carries another payload")
		}
		items := make([]metadataTypedValue, 0, len(v.items))
		for _, item := range v.items {
			normalized, err := normalizeMetadataTypedValue(item)
			if err != nil {
				return metadataTypedValue{}, err
			}
			items = append(items, normalized)
		}
		return metadataTypedValue{state: "present", typ: "array", items: items}, nil
	case "map":
		if v.str != nil || v.boolean != nil || v.decimal != nil || v.bits != nil ||
			len(v.items) != 0 || v.reason != nil || len(v.assertions) != 0 {
			return metadataTypedValue{}, errors.New("columnar: present map carries another payload")
		}
		entries := make([]metadataTypedMapEntry, 0, len(v.entries))
		seen := make(map[string]struct{}, len(v.entries))
		for _, entry := range v.entries {
			if _, duplicate := seen[entry.Key]; duplicate {
				return metadataTypedValue{}, fmt.Errorf("columnar: duplicate typed map key %q", entry.Key)
			}
			seen[entry.Key] = struct{}{}
			normalized, err := normalizeMetadataTypedValue(entry.Value)
			if err != nil {
				return metadataTypedValue{}, err
			}
			entries = append(entries, metadataTypedMapEntry{Key: entry.Key, Value: normalized})
		}
		slices.SortFunc(entries, func(a, b metadataTypedMapEntry) int {
			return strings.Compare(a.Key, b.Key)
		})
		return metadataTypedValue{state: "present", typ: "map", entries: entries}, nil
	default:
		return metadataTypedValue{}, fmt.Errorf("columnar: unknown typed value type %q", v.typ)
	}
}

// validateMetadataInteger requires the one canonical signed 64-bit decimal
// form: no plus sign, no leading zeros, no negative zero, and an in-range value.
func validateMetadataInteger(value string) error {
	if value == "" || value == "-0" {
		return fmt.Errorf("columnar: invalid canonical integer %q", value)
	}
	digits := value
	if digits[0] == '-' {
		digits = digits[1:]
	}
	if digits == "" || (len(digits) > 1 && digits[0] == '0') {
		return fmt.Errorf("columnar: invalid canonical integer %q", value)
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return fmt.Errorf("columnar: invalid canonical integer %q", value)
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || strconv.FormatInt(parsed, 10) != value {
		return fmt.Errorf("columnar: invalid canonical integer %q", value)
	}
	return nil
}

// validateMetadataDoubleBits requires exactly one 0x-prefixed 16-hex-digit
// lowercase IEEE-754 binary64 pattern.
func validateMetadataDoubleBits(value string) error {
	if len(value) != 18 || !strings.HasPrefix(value, "0x") {
		return fmt.Errorf("columnar: invalid double bits %q", value)
	}
	for _, digit := range value[2:] {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
			return fmt.Errorf("columnar: invalid double bits %q", value)
		}
	}
	if _, err := strconv.ParseUint(value[2:], 16, 64); err != nil {
		return fmt.Errorf("columnar: invalid double bits %q", value)
	}
	return nil
}

// MarshalJSON encodes the selected state/type payload fields after validation.
// Irrelevant payload fields are rejected; an empty present array still encodes
// items: [].
func (v metadataTypedValue) MarshalJSON() ([]byte, error) {
	normalized, err := normalizeMetadataTypedValue(v)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"state": normalized.state}
	switch normalized.state {
	case "present":
		payload["type"] = normalized.typ
		switch normalized.typ {
		case "string":
			payload["value"] = *normalized.str
		case "boolean":
			payload["value"] = *normalized.boolean
		case "integer":
			payload["decimal"] = *normalized.decimal
		case "double":
			payload["bits"] = *normalized.bits
		case "array":
			items := normalized.items
			if items == nil {
				items = []metadataTypedValue{}
			}
			payload["items"] = items
		case "map":
			entries := normalized.entries
			if entries == nil {
				entries = []metadataTypedMapEntry{}
			}
			payload["entries"] = entries
		}
	case "null", "absent", "cleared":
	case "unknown":
		payload["reason"] = *normalized.reason
	case "ambiguous":
		payload["assertions"] = normalized.assertions
	}
	return json.Marshal(payload)
}

// UnmarshalJSON decodes one tagged value strictly. Unknown fields, invalid
// states/types, missing or irrelevant payloads, non-canonical integers/doubles,
// duplicate map keys and single/equal ambiguity alternatives are rejected.
func (v *metadataTypedValue) UnmarshalJSON(data []byte) error {
	var raw struct {
		State      *string                  `json:"state"`
		Type       *string                  `json:"type"`
		Value      json.RawMessage          `json:"value"`
		Decimal    *string                  `json:"decimal"`
		Bits       *string                  `json:"bits"`
		Items      *[]metadataTypedValue    `json:"items"`
		Entries    *[]metadataTypedMapEntry `json:"entries"`
		Reason     *string                  `json:"reason"`
		Assertions *[]metadataTypedValue    `json:"assertions"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("columnar: typed value has trailing data")
	}
	if raw.State == nil {
		return errors.New("columnar: typed value has no state")
	}
	value := metadataTypedValue{state: *raw.State}
	if raw.Type != nil {
		value.typ = *raw.Type
	}
	switch *raw.State {
	case "present":
		if raw.Type == nil {
			return errors.New("columnar: present typed value has no type")
		}
		switch *raw.Type {
		case "string":
			if len(raw.Value) == 0 || string(raw.Value) == "null" {
				return errors.New("columnar: present string has no value")
			}
			if raw.Decimal != nil || raw.Bits != nil || raw.Items != nil || raw.Entries != nil ||
				raw.Reason != nil || raw.Assertions != nil {
				return errors.New("columnar: present string carries an irrelevant field")
			}
			var text string
			if err := json.Unmarshal(raw.Value, &text); err != nil {
				return fmt.Errorf("columnar: present string value: %w", err)
			}
			value.str = &text
		case "boolean":
			if len(raw.Value) == 0 || (string(raw.Value) != "true" && string(raw.Value) != "false") {
				return errors.New("columnar: present boolean has no boolean value")
			}
			if raw.Decimal != nil || raw.Bits != nil || raw.Items != nil || raw.Entries != nil ||
				raw.Reason != nil || raw.Assertions != nil {
				return errors.New("columnar: present boolean carries an irrelevant field")
			}
			var boolean bool
			if err := json.Unmarshal(raw.Value, &boolean); err != nil {
				return fmt.Errorf("columnar: present boolean value: %w", err)
			}
			value.boolean = &boolean
		case "integer":
			if len(raw.Value) != 0 || raw.Bits != nil || raw.Items != nil || raw.Entries != nil ||
				raw.Reason != nil || raw.Assertions != nil {
				return errors.New("columnar: present integer carries an irrelevant field")
			}
			value.decimal = raw.Decimal
		case "double":
			if len(raw.Value) != 0 || raw.Decimal != nil || raw.Items != nil || raw.Entries != nil ||
				raw.Reason != nil || raw.Assertions != nil {
				return errors.New("columnar: present double carries an irrelevant field")
			}
			value.bits = raw.Bits
		case "array":
			if raw.Items == nil {
				return errors.New("columnar: present array has no items")
			}
			if len(raw.Value) != 0 || raw.Decimal != nil || raw.Bits != nil || raw.Entries != nil ||
				raw.Reason != nil || raw.Assertions != nil {
				return errors.New("columnar: present array carries an irrelevant field")
			}
			value.items = *raw.Items
		case "map":
			if raw.Entries == nil {
				return errors.New("columnar: present map has no entries")
			}
			if len(raw.Value) != 0 || raw.Decimal != nil || raw.Bits != nil || raw.Items != nil ||
				raw.Reason != nil || raw.Assertions != nil {
				return errors.New("columnar: present map carries an irrelevant field")
			}
			value.entries = *raw.Entries
		default:
			return fmt.Errorf("columnar: unknown typed value type %q", *raw.Type)
		}
	case "null", "absent", "cleared":
		if raw.Type != nil || len(raw.Value) != 0 || raw.Decimal != nil || raw.Bits != nil ||
			raw.Items != nil || raw.Entries != nil || raw.Reason != nil || raw.Assertions != nil {
			return fmt.Errorf("columnar: %s typed value carries a payload", *raw.State)
		}
	case "unknown":
		if raw.Type != nil || len(raw.Value) != 0 || raw.Decimal != nil || raw.Bits != nil ||
			raw.Items != nil || raw.Entries != nil || raw.Assertions != nil {
			return errors.New("columnar: unknown typed value carries an irrelevant field")
		}
		value.reason = raw.Reason
	case "ambiguous":
		if raw.Assertions == nil {
			return errors.New("columnar: ambiguous typed value has no assertions")
		}
		if raw.Type != nil || len(raw.Value) != 0 || raw.Decimal != nil || raw.Bits != nil ||
			raw.Items != nil || raw.Entries != nil || raw.Reason != nil {
			return errors.New("columnar: ambiguous typed value carries an irrelevant field")
		}
		value.assertions = *raw.Assertions
	default:
		return fmt.Errorf("columnar: unknown typed value state %q", *raw.State)
	}
	normalized, err := normalizeMetadataTypedValue(value)
	if err != nil {
		return err
	}
	*v = normalized
	return nil
}

// UnmarshalJSON decodes one map entry strictly.
func (e *metadataTypedMapEntry) UnmarshalJSON(data []byte) error {
	var raw struct {
		Key   *string             `json:"key"`
		Value *metadataTypedValue `json:"value"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("columnar: typed map entry has trailing data")
	}
	if raw.Key == nil || raw.Value == nil {
		return errors.New("columnar: typed map entry needs key and value")
	}
	e.Key, e.Value = *raw.Key, *raw.Value
	return nil
}

// metadataTypedValueCanonical is the semantic identity of one typed value. It
// uses type tags, length-delimited components, array order, sorted map entries
// and exact double bits, so equality never depends on JSON map iteration order,
// construction order or decoding order. Invalid values are represented
// defensively and never panic.
func metadataTypedValueCanonical(v metadataTypedValue) string {
	var builder []byte
	appendPart := func(part string) {
		builder = append(builder, strconv.Itoa(len(part))...)
		builder = append(builder, ':')
		builder = append(builder, part...)
	}
	appendPart(v.state)
	switch v.state {
	case "present":
		appendPart(v.typ)
		switch v.typ {
		case "string":
			appendPart(metadataStringPointer(v.str))
		case "boolean":
			if v.boolean == nil {
				appendPart("<nil>")
			} else {
				appendPart(strconv.FormatBool(*v.boolean))
			}
		case "integer":
			appendPart(metadataStringPointer(v.decimal))
		case "double":
			appendPart(metadataStringPointer(v.bits))
		case "array":
			appendPart(strconv.Itoa(len(v.items)))
			for _, item := range v.items {
				appendPart(metadataTypedValueCanonical(item))
			}
		case "map":
			entries := slices.Clone(v.entries)
			slices.SortFunc(entries, func(a, b metadataTypedMapEntry) int {
				return strings.Compare(a.Key, b.Key)
			})
			appendPart(strconv.Itoa(len(entries)))
			for _, entry := range entries {
				appendPart(entry.Key)
				appendPart(metadataTypedValueCanonical(entry.Value))
			}
		}
	case "unknown":
		appendPart(metadataStringPointer(v.reason))
	case "ambiguous":
		values := make([]string, 0, len(v.assertions))
		for _, assertion := range v.assertions {
			values = append(values, metadataTypedValueCanonical(assertion))
		}
		slices.Sort(values)
		values = slices.Compact(values)
		appendPart(strconv.Itoa(len(values)))
		for _, value := range values {
			appendPart(value)
		}
	}
	return string(builder)
}

// metadataStringPointer renders a required pointer defensively.
func metadataStringPointer(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
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
		return "present\x00string\x00" + metadataStringPointer(v.str)
	case "boolean":
		if v.boolean == nil {
			return "present\x00boolean\x00<nil>"
		}
		return "present\x00boolean\x00" + strconv.FormatBool(*v.boolean)
	case "integer":
		return "present\x00integer\x00" + metadataStringPointer(v.decimal)
	case "double":
		return "present\x00double\x00" + metadataStringPointer(v.bits)
	default:
		return "present\x00" + v.typ + "\x00" + metadataTypedValueCanonical(v)
	}
}
