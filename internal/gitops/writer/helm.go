/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package writer

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// HelmWriter updates Helm values.yaml files.
type HelmWriter struct{}

// NewHelmWriter creates a new HelmWriter.
func NewHelmWriter() *HelmWriter {
	return &HelmWriter{}
}

// UpdateValues updates the resource requests/limits in a Helm values.yaml file.
// valuePath specifies the path to the resources section (e.g., "resources" or "deployment.resources").
// A single resources block is shared by the workload, so only the first update is applied.
func (w *HelmWriter) UpdateValues(content []byte, updates []ResourceUpdate, valuePath string) ([]byte, error) {
	f, err := parseYAMLFile(content)
	if err != nil {
		return nil, err
	}
	doc, err := f.rootMapping(0)
	if err != nil {
		return nil, err
	}

	pathParts := strings.Split(valuePath, ".")

	parentNode := doc
	for i := 0; i < len(pathParts)-1; i++ {
		parentNode = findKey(parentNode, pathParts[i])
		if parentNode == nil || parentNode.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("path not found: %s", strings.Join(pathParts[:i+1], "."))
		}
	}

	resourcesKey := pathParts[len(pathParts)-1]
	if len(updates) == 0 {
		return f.render()
	}

	resourcesNode, err := f.ensureMapping(parentNode, resourcesKey)
	if err != nil {
		return nil, fmt.Errorf("path %s: %w", valuePath, err)
	}
	if err := updateResources(f, resourcesNode, updates[0]); err != nil {
		return nil, err
	}

	return f.render()
}

// UpdateValuesMultiContainer updates resources for multiple containers in a Helm values.yaml.
// containerPathMap maps container names to their value paths (e.g., {"app": "app.resources", "sidecar": "sidecar.resources"}).
func (w *HelmWriter) UpdateValuesMultiContainer(content []byte, updates []ResourceUpdate, containerPathMap map[string]string) ([]byte, error) {
	f, err := parseYAMLFile(content)
	if err != nil {
		return nil, err
	}
	doc, err := f.rootMapping(0)
	if err != nil {
		return nil, err
	}

	for _, update := range updates {
		valuePath, ok := containerPathMap[update.ContainerName]
		if !ok {
			// Skip containers without a path mapping
			continue
		}

		if err := w.updateAtPath(f, doc, valuePath, update); err != nil {
			return nil, fmt.Errorf("failed to update container %s: %w", update.ContainerName, err)
		}
	}

	return f.render()
}

// updateAtPath updates resources at a specific path.
func (w *HelmWriter) updateAtPath(f *yamlFile, doc *yaml.Node, valuePath string, update ResourceUpdate) error {
	pathParts := strings.Split(valuePath, ".")

	currentNode := doc
	for i, part := range pathParts {
		next, err := f.ensureMapping(currentNode, part)
		if err != nil {
			return fmt.Errorf("expected mapping node at path %s", strings.Join(pathParts[:i+1], "."))
		}
		currentNode = next
	}

	return updateResources(f, currentNode, update)
}

// DetectHelmStructure analyzes a values.yaml to detect the resources structure.
// Returns a map of detected container names to their value paths.
func (w *HelmWriter) DetectHelmStructure(content []byte) (map[string]string, error) {
	f, err := parseYAMLFile(content)
	if err != nil {
		return nil, err
	}
	doc, err := f.rootMapping(0)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)

	// Common patterns in Helm charts
	// 1. Top-level resources: "resources"
	// 2. Per-component: "app.resources", "worker.resources"
	// 3. Containers section: "containers.app.resources"

	w.findResourcePaths(doc, "", result)

	return result, nil
}

// findResourcePaths recursively finds all "resources" keys in the YAML structure.
func (w *HelmWriter) findResourcePaths(node *yaml.Node, currentPath string, result map[string]string) {
	if node.Kind != yaml.MappingNode {
		return
	}

	for i := 0; i < len(node.Content)-1; i += 2 {
		keyNode := node.Content[i]
		valueNode := node.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			continue
		}

		key := keyNode.Value
		var newPath string
		if currentPath == "" {
			newPath = key
		} else {
			newPath = currentPath + "." + key
		}

		// Check if this is a resources key
		if key == resourcesKey && valueNode.Kind == yaml.MappingNode {
			// Determine the container name from the path
			containerName := "main"
			if currentPath != "" {
				parts := strings.Split(currentPath, ".")
				containerName = parts[len(parts)-1]
			}
			result[containerName] = newPath
		}

		// Recurse into mapping nodes
		if valueNode.Kind == yaml.MappingNode {
			w.findResourcePaths(valueNode, newPath, result)
		}
	}
}
