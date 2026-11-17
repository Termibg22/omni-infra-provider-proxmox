// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// BuildUISchema uses the same streamlined VM form in both modes. Omni generates its own UI
// and does not support provider-supplied tabs or local draft persistence.
// Flat property names keep existing pre-v4 Machine Classes editable without a
// server-side migration or changes to the provisioned VMs.
func BuildUISchema(base, mode string) (string, error) {
	if mode != "basic" && mode != "advanced" {
		return "", fmt.Errorf("schema-mode must be basic or advanced")
	}

	var root map[string]any
	if err := json.Unmarshal([]byte(base), &root); err != nil {
		return "", err
	}

	if _, ok := root["properties"].(map[string]any); !ok {
		return "", fmt.Errorf("schema must contain an object of properties")
	}

	return marshalSchemaInOrder(root, []byte(base))
}

// Omni generates its form in JSON property insertion order. encoding/json sorts
// map keys; reapply the source order at every object, including array items.
func marshalSchemaInOrder(value any, source json.RawMessage) (string, error) {
	raw, err := orderedSchemaValue(value, source)
	if err != nil {
		return "", err
	}

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return "", err
	}

	return pretty.String(), nil
}

func orderedSchemaValue(value any, source json.RawMessage) ([]byte, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		if arr, ok := value.([]any); ok {
			return orderedSchemaArray(arr, source)
		}

		return json.Marshal(value)
	}

	originals, order, err := schemaObjectOrder(obj, source)
	if err != nil {
		return nil, err
	}

	for _, key := range slices.Sorted(maps.Keys(obj)) {
		if _, exists := originals[key]; !exists {
			order = append(order, key)
		}
	}

	out := []byte{'{'}

	for i, key := range order {
		if i > 0 {
			out = append(out, ',')
		}

		k, marshalErr := json.Marshal(key)
		if marshalErr != nil {
			return nil, marshalErr
		}

		out = append(out, k...)
		out = append(out, ':')

		b, err := orderedSchemaValue(obj[key], originals[key])
		if err != nil {
			return nil, err
		}

		out = append(out, b...)
	}

	return append(out, '}'), nil
}

func orderedSchemaArray(arr []any, source json.RawMessage) ([]byte, error) {
	var original []json.RawMessage
	if len(source) != 0 {
		if err := json.Unmarshal(source, &original); err != nil {
			return nil, err
		}
	}

	out := []byte{'['}

	for i, item := range arr {
		if i > 0 {
			out = append(out, ',')
		}

		var ref json.RawMessage
		if i < len(original) {
			ref = original[i]
		}

		b, err := orderedSchemaValue(item, ref)
		if err != nil {
			return nil, err
		}

		out = append(out, b...)
	}

	return append(out, ']'), nil
}

func schemaObjectOrder(obj map[string]any, source json.RawMessage) (map[string]json.RawMessage, []string, error) {
	originals := map[string]json.RawMessage{}

	var order []string
	if len(source) == 0 {
		return originals, order, nil
	}

	dec := json.NewDecoder(bytes.NewReader(source))

	token, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}

	if token != json.Delim('{') {
		return originals, order, nil
	}

	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}

		key, ok := token.(string)
		if !ok {
			return nil, nil, fmt.Errorf("schema object key must be a string")
		}

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, err
		}

		originals[key] = raw
		if _, exists := obj[key]; exists {
			order = append(order, key)
		}
	}

	return originals, order, nil
}
