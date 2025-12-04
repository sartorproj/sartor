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
	"bytes"
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
func (w *HelmWriter) UpdateValues(content []byte, updates []ResourceUpdate, valuePath string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("invalid YAML document")
	}

	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping node at document root")
	}

	// Parse the value path
	pathParts := strings.Split(valuePath, ".")

	// Navigate to the parent of the resources section
	parentNode := doc
	for i := 0; i < len(pathParts)-1; i++ {
		parentNode = findKey(parentNode, pathParts[i])
		if parentNode == nil {
			return nil, fmt.Errorf("path not found: %s", strings.Join(pathParts[:i+1], "."))
		}
	}

	// Get or create the resources key
	resourcesKey := pathParts[len(pathParts)-1]

	// For Helm values, we update the global resources section
	// The structure depends on the chart, but typically:
	// resources:
	//   requests:
	//     cpu: "100m"
	//     memory: "128Mi"
	//   limits:
	//     cpu: "200m"
	//     memory: "256Mi"

	resourcesIdx := findKeyIndex(parentNode, resourcesKey)
	var resourcesNode *yaml.Node

	if resourcesIdx == -1 {
		// Create resources node
		keyNode := &yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: resourcesKey,
		}
		resourcesNode = &yaml.Node{
			Kind:    yaml.MappingNode,
			Content: []*yaml.Node{},
		}
		parentNode.Content = append(parentNode.Content, keyNode, resourcesNode)
	} else {
		resourcesNode = parentNode.Content[resourcesIdx+1]
	}

	// For Helm, we typically have a single resources block (not per-container)
	// If there are multiple containers, the chart usually uses different keys
	// For now, we'll update the main resources block with the first container's values
	if len(updates) > 0 {
		update := updates[0]

		// Update requests
		if update.Requests != nil {
			if err := updateResourceSection(resourcesNode, "requests", update.Requests); err != nil {
				return nil, err
			}
		}

		// Update limits
		if update.Limits != nil {
			if err := updateResourceSection(resourcesNode, "limits", update.Limits); err != nil {
				return nil, err
			}
		}
	}

	// Marshal back to YAML preserving formatting
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return nil, fmt.Errorf("failed to encode YAML: %w", err)
	}

	return buf.Bytes(), nil
}

// UpdateValuesMultiContainer updates resources for multiple containers in a Helm values.yaml.
// containerPathMap maps container names to their value paths (e.g., {"app": "app.resources", "sidecar": "sidecar.resources"}).
func (w *HelmWriter) UpdateValuesMultiContainer(content []byte, updates []ResourceUpdate, containerPathMap map[string]string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("invalid YAML document")
	}

	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping node at document root")
	}

	for _, update := range updates {
		valuePath, ok := containerPathMap[update.ContainerName]
		if !ok {
			// Skip containers without a path mapping
			continue
		}

		if err := w.updateAtPath(doc, valuePath, update); err != nil {
			return nil, fmt.Errorf("failed to update container %s: %w", update.ContainerName, err)
		}
	}

	// Marshal back to YAML preserving formatting
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return nil, fmt.Errorf("failed to encode YAML: %w", err)
	}

	return buf.Bytes(), nil
}

// updateAtPath updates resources at a specific path.
func (w *HelmWriter) updateAtPath(doc *yaml.Node, valuePath string, update ResourceUpdate) error {
	pathParts := strings.Split(valuePath, ".")

	// Navigate to the target node, creating intermediate nodes as needed
	currentNode := doc
	for i, part := range pathParts {
		idx := findKeyIndex(currentNode, part)
		if idx == -1 {
			// Create the node
			keyNode := &yaml.Node{
				Kind:  yaml.ScalarNode,
				Value: part,
			}
			valueNode := &yaml.Node{
				Kind:    yaml.MappingNode,
				Content: []*yaml.Node{},
			}
			currentNode.Content = append(currentNode.Content, keyNode, valueNode)
			currentNode = valueNode
		} else {
			currentNode = currentNode.Content[idx+1]
			if currentNode.Kind != yaml.MappingNode {
				return fmt.Errorf("expected mapping node at path %s", strings.Join(pathParts[:i+1], "."))
			}
		}
	}

	// Update requests
	if update.Requests != nil {
		if err := updateResourceSection(currentNode, "requests", update.Requests); err != nil {
			return err
		}
	}

	// Update limits
	if update.Limits != nil {
		if err := updateResourceSection(currentNode, "limits", update.Limits); err != nil {
			return err
		}
	}

	return nil
}

// DetectHelmStructure analyzes a values.yaml to detect the resources structure.
// Returns a map of detected container names to their value paths.
func (w *HelmWriter) DetectHelmStructure(content []byte) (map[string]string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("invalid YAML document")
	}

	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping node at document root")
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
		if key == "resources" && valueNode.Kind == yaml.MappingNode {
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
