// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import "go.yaml.in/yaml/v4"

// UnmarshalYAML remembers presence separately from zero values so workload
// defaults never overwrite an explicit false from YAML or the Omni form.
func (d *Data) UnmarshalYAML(node *yaml.Node) error {
	type plain Data

	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}

	*d = Data(decoded)
	d.explicitFields = suppliedFields(node, "")

	return nil
}

// UnmarshalYAML tracks per-disk overrides for the storage-node profile.
func (d *Disk) UnmarshalYAML(node *yaml.Node) error {
	type plain Disk

	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}

	*d = Disk(decoded)
	d.explicitFields = suppliedFields(node, "")

	return nil
}

func suppliedFields(node *yaml.Node, prefix string) map[string]bool {
	fields := map[string]bool{}
	if node.Kind != yaml.MappingNode {
		return fields
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		name := prefix + node.Content[i].Value

		value := node.Content[i+1]
		if value.Tag == "!!null" {
			continue
		}

		fields[name] = true
		if value.Kind == yaml.MappingNode {
			for key := range suppliedFields(value, name+".") {
				fields[key] = true
			}
		}
	}

	return fields
}

func (d *Data) isExplicit(name string) bool {
	if d.explicitFields[name] {
		return true
	}

	for _, section := range []string{"general.", "compute.", "system."} {
		if d.explicitFields[section+name] {
			return true
		}
	}

	return false
}
