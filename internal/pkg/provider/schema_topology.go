// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

func topologyMappings(topology NodeTopology, field string) []string {
	if field == "pci_devices" {
		return topology.PCIMappings
	}

	return topology.USBMappings
}

func setHardwareMappingOptions(props map[string]any, field string, values []string, opts ClusterOptions) {
	// Adding a device must not silently select the first GPU or USB device.
	setArrayObjectPropertyEnum(props, field, "mapping", values, "")

	array, ok := props[field].(map[string]any)
	if !ok {
		return
	}

	items, ok := array["items"].(map[string]any)
	if !ok {
		return
	}

	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		return
	}

	mapping, ok := itemProps["mapping"].(map[string]any)
	if !ok {
		return
	}

	if len(values) == 0 {
		mapping["description"] = "No mappings on current nodes were discovered. Configure mappings in Proxmox Datacenter > Resource Mappings, " +
			"ensure the provider can read them, then restart the provider. Remove this device if unused."

		return
	}

	details := []string{"Select a Proxmox resource mapping. All selected devices must be available on the same node within Target Node / Node Group. " +
		"Clear to choose another mapping, or remove an unused device."}

	for _, value := range values {
		var nodes []string

		for _, node := range opts.Nodes {
			if slices.Contains(topologyMappings(opts.NodeTopologies[node], field), value) {
				nodes = append(nodes, node)
			}
		}

		if len(nodes) > 0 {
			details = append(details, fmt.Sprintf("%s: %s.", value, strings.Join(nodes, ", ")))
		}
	}

	mapping["description"] = strings.Join(details, " ")
}

// Hosted Omni resolves dropdown options from properties without consulting
// form data. Conditional schemas can validate placement, but cannot filter
// the visible list. Keep the list stable and validate the entire device set.
func addHardwareTopologyConstraints(root, props map[string]any, opts ClusterOptions) {
	if len(opts.NodeTopologies) == 0 || props["node"] == nil {
		return
	}

	var fields []string

	for _, field := range []string{"pci_devices", "usb_devices"} {
		if props[field] != nil {
			fields = append(fields, field)
		}
	}

	if len(fields) == 0 {
		return
	}

	var constraints []any
	if existing, ok := root["allOf"].([]any); ok {
		constraints = slices.Clone(existing)
	}

	if props["node_group"] == nil {
		opts.NodeGroups = nil
	}

	nodeRules := make(map[string]any, len(opts.Nodes))
	allNodes := make([]any, 0, len(opts.Nodes))

	for _, node := range opts.Nodes {
		topology, known := opts.NodeTopologies[node]
		if !known {
			// Do not interpret incomplete discovery as unavailable hardware.
			return
		}

		rule := hardwareMappingConstraint(topology, fields)
		nodeRules[node] = rule
		allNodes = append(allNodes, rule)
		constraints = append(constraints, conditionalSchema("node", node, rule))
	}

	if len(allNodes) == 0 {
		return
	}

	// In automatic placement, one node must have ALL requested PCI and USB
	// mappings. A union of mappings would incorrectly allow disjoint devices.
	constraints = append(constraints, map[string]any{"anyOf": allNodes})

	for _, group := range slices.Sorted(maps.Keys(opts.NodeGroups)) {
		var groupRules []any

		nodes := []string{"", "auto", "none"}

		for _, node := range opts.NodeGroups[group] {
			if rule, ok := nodeRules[node]; ok {
				groupRules = append(groupRules, rule)
				nodes = append(nodes, node)
			}
		}

		if len(groupRules) == 0 {
			continue
		}

		constraints = append(constraints, conditionalSchema("node_group", group, map[string]any{
			"properties": map[string]any{"node": map[string]any{"enum": nodes}},
			"anyOf":      groupRules,
		}))
	}

	root["allOf"] = constraints
}

func hardwareMappingConstraint(topology NodeTopology, fields []string) map[string]any {
	props := map[string]any{}

	for _, field := range fields {
		values := append([]string{""}, topologyMappings(topology, field)...)
		props[field] = map[string]any{
			"items": map[string]any{
				"properties": map[string]any{"mapping": map[string]any{"enum": values}},
			},
		}
	}

	return map[string]any{"properties": props}
}

func conditionalSchema(field, value string, constraint any) map[string]any {
	return map[string]any{
		"if": map[string]any{
			"properties": map[string]any{field: map[string]any{"const": value}},
			"required":   []string{field},
		},
		"then": constraint,
	}
}
