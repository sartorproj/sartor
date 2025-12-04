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

// KustomizeWriter generates and updates Kustomize patches.
type KustomizeWriter struct{}

// NewKustomizeWriter creates a new KustomizeWriter.
func NewKustomizeWriter() *KustomizeWriter {
	return &KustomizeWriter{}
}

// PatchTarget identifies the resource to patch.
type PatchTarget struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

// StrategicMergePatch represents a Kustomize strategic merge patch.
type StrategicMergePatch struct {
	APIVersion string                   `yaml:"apiVersion"`
	Kind       string                   `yaml:"kind"`
	Metadata   StrategicMergePatchMeta  `yaml:"metadata"`
	Spec       *StrategicMergePatchSpec `yaml:"spec,omitempty"`
}

// StrategicMergePatchMeta contains metadata for the patch.
type StrategicMergePatchMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
}

// StrategicMergePatchSpec contains the spec to patch.
type StrategicMergePatchSpec struct {
	Template *PodTemplateSpec `yaml:"template,omitempty"`
}

// PodTemplateSpec contains the pod template spec for patching.
type PodTemplateSpec struct {
	Spec *PodSpec `yaml:"spec,omitempty"`
}

// PodSpec contains the pod spec for patching.
type PodSpec struct {
	Containers []ContainerPatch `yaml:"containers,omitempty"`
}

// ContainerPatch contains the container patch data.
type ContainerPatch struct {
	Name      string          `yaml:"name"`
	Resources *ResourcesPatch `yaml:"resources,omitempty"`
}

// ResourcesPatch contains resource requirements for the patch.
type ResourcesPatch struct {
	Requests map[string]string `yaml:"requests,omitempty"`
	Limits   map[string]string `yaml:"limits,omitempty"`
}

// GeneratePatch creates a Kustomize strategic merge patch for the given updates.
func (w *KustomizeWriter) GeneratePatch(target PatchTarget, updates []ResourceUpdate) ([]byte, error) {
	patch := StrategicMergePatch{
		APIVersion: target.APIVersion,
		Kind:       target.Kind,
		Metadata: StrategicMergePatchMeta{
			Name:      target.Name,
			Namespace: target.Namespace,
		},
	}

	containers := make([]ContainerPatch, 0, len(updates))
	for _, update := range updates {
		containerPatch := ContainerPatch{
			Name:      update.ContainerName,
			Resources: &ResourcesPatch{},
		}

		if update.Requests != nil {
			containerPatch.Resources.Requests = make(map[string]string)
			if update.Requests.CPU != nil {
				containerPatch.Resources.Requests["cpu"] = update.Requests.CPU.String()
			}
			if update.Requests.Memory != nil {
				containerPatch.Resources.Requests["memory"] = update.Requests.Memory.String()
			}
		}

		if update.Limits != nil {
			containerPatch.Resources.Limits = make(map[string]string)
			if update.Limits.CPU != nil {
				containerPatch.Resources.Limits["cpu"] = update.Limits.CPU.String()
			}
			if update.Limits.Memory != nil {
				containerPatch.Resources.Limits["memory"] = update.Limits.Memory.String()
			}
		}

		containers = append(containers, containerPatch)
	}

	patch.Spec = &StrategicMergePatchSpec{
		Template: &PodTemplateSpec{
			Spec: &PodSpec{
				Containers: containers,
			},
		},
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(patch); err != nil {
		return nil, fmt.Errorf("failed to encode patch: %w", err)
	}

	return buf.Bytes(), nil
}

// UpdatePatch updates an existing Kustomize patch with new resource values.
func (w *KustomizeWriter) UpdatePatch(content []byte, updates []ResourceUpdate) ([]byte, error) {
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

	// Navigate to spec.template.spec.containers
	spec := findKey(doc, "spec")
	if spec == nil {
		return nil, fmt.Errorf("spec not found in patch")
	}

	template := findKey(spec, "template")
	if template == nil {
		return nil, fmt.Errorf("spec.template not found in patch")
	}

	templateSpec := findKey(template, "spec")
	if templateSpec == nil {
		return nil, fmt.Errorf("spec.template.spec not found in patch")
	}

	containers := findKey(templateSpec, "containers")
	if containers == nil {
		return nil, fmt.Errorf("containers not found in patch")
	}

	// Update each container
	for _, update := range updates {
		if err := updateContainerInPatch(containers, update); err != nil {
			return nil, fmt.Errorf("failed to update container %s: %w", update.ContainerName, err)
		}
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return nil, fmt.Errorf("failed to encode patch: %w", err)
	}

	return buf.Bytes(), nil
}

// updateContainerInPatch updates a container's resources in the patch.
func updateContainerInPatch(containers *yaml.Node, update ResourceUpdate) error {
	if containers.Kind != yaml.SequenceNode {
		return fmt.Errorf("containers is not a sequence")
	}

	// Find the container by name
	var containerNode *yaml.Node
	for _, container := range containers.Content {
		if container.Kind != yaml.MappingNode {
			continue
		}

		nameNode := findKey(container, "name")
		if nameNode != nil && nameNode.Value == update.ContainerName {
			containerNode = container
			break
		}
	}

	// If container not found, add it
	if containerNode == nil {
		containerNode = &yaml.Node{
			Kind: yaml.MappingNode,
			Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "name"},
				{Kind: yaml.ScalarNode, Value: update.ContainerName},
			},
		}
		containers.Content = append(containers.Content, containerNode)
	}

	// Find or create resources node
	resourcesIdx := findKeyIndex(containerNode, "resources")
	var resources *yaml.Node

	if resourcesIdx == -1 {
		keyNode := &yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: "resources",
		}
		resources = &yaml.Node{
			Kind:    yaml.MappingNode,
			Content: []*yaml.Node{},
		}
		containerNode.Content = append(containerNode.Content, keyNode, resources)
	} else {
		resources = containerNode.Content[resourcesIdx+1]
	}

	// Update requests
	if update.Requests != nil {
		if err := updateResourceSection(resources, "requests", update.Requests); err != nil {
			return err
		}
	}

	// Update limits
	if update.Limits != nil {
		if err := updateResourceSection(resources, "limits", update.Limits); err != nil {
			return err
		}
	}

	return nil
}

// JSONPatch represents a JSON Patch operation (RFC 6902).
type JSONPatch struct {
	Op    string      `json:"op" yaml:"op"`
	Path  string      `json:"path" yaml:"path"`
	Value interface{} `json:"value,omitempty" yaml:"value,omitempty"`
}

// GenerateJSONPatch creates a JSON Patch for the given updates.
func (w *KustomizeWriter) GenerateJSONPatch(updates []ResourceUpdate) ([]byte, error) {
	patches := make([]JSONPatch, 0)

	for _, update := range updates {
		containerIndex := 0 // Assume first container; real impl would need to find by name

		if update.Requests != nil {
			if update.Requests.CPU != nil {
				patches = append(patches, JSONPatch{
					Op:    "replace",
					Path:  fmt.Sprintf("/spec/template/spec/containers/%d/resources/requests/cpu", containerIndex),
					Value: update.Requests.CPU.String(),
				})
			}
			if update.Requests.Memory != nil {
				patches = append(patches, JSONPatch{
					Op:    "replace",
					Path:  fmt.Sprintf("/spec/template/spec/containers/%d/resources/requests/memory", containerIndex),
					Value: update.Requests.Memory.String(),
				})
			}
		}

		if update.Limits != nil {
			if update.Limits.CPU != nil {
				patches = append(patches, JSONPatch{
					Op:    "replace",
					Path:  fmt.Sprintf("/spec/template/spec/containers/%d/resources/limits/cpu", containerIndex),
					Value: update.Limits.CPU.String(),
				})
			}
			if update.Limits.Memory != nil {
				patches = append(patches, JSONPatch{
					Op:    "replace",
					Path:  fmt.Sprintf("/spec/template/spec/containers/%d/resources/limits/memory", containerIndex),
					Value: update.Limits.Memory.String(),
				})
			}
		}
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(patches); err != nil {
		return nil, fmt.Errorf("failed to encode JSON patch: %w", err)
	}

	return buf.Bytes(), nil
}

// KustomizationEntry represents an entry to add to kustomization.yaml.
type KustomizationEntry struct {
	// PatchPath is the path to the patch file.
	PatchPath string
	// Target specifies the resource to patch.
	Target PatchTarget
}

// UpdateKustomization adds a patch reference to a kustomization.yaml file.
func (w *KustomizeWriter) UpdateKustomization(content []byte, entry KustomizationEntry) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("failed to parse kustomization.yaml: %w", err)
	}

	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("invalid kustomization.yaml")
	}

	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping node at document root")
	}

	// Find or create patches section
	patchesIdx := findKeyIndex(doc, "patches")
	var patches *yaml.Node

	if patchesIdx == -1 {
		// Try patchesStrategicMerge for older kustomization format
		patchesIdx = findKeyIndex(doc, "patchesStrategicMerge")
	}

	if patchesIdx == -1 {
		// Create patches section
		keyNode := &yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: "patches",
		}
		patches = &yaml.Node{
			Kind:    yaml.SequenceNode,
			Content: []*yaml.Node{},
		}
		doc.Content = append(doc.Content, keyNode, patches)
	} else {
		patches = doc.Content[patchesIdx+1]
	}

	// Check if patch already exists
	for _, patch := range patches.Content {
		if patch.Kind == yaml.ScalarNode && patch.Value == entry.PatchPath {
			// Patch already exists
			return content, nil
		}
		if patch.Kind == yaml.MappingNode {
			pathNode := findKey(patch, "path")
			if pathNode != nil && pathNode.Value == entry.PatchPath {
				// Patch already exists
				return content, nil
			}
		}
	}

	// Add new patch entry
	patchEntry := &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "path"},
			{Kind: yaml.ScalarNode, Value: entry.PatchPath},
			{Kind: yaml.ScalarNode, Value: "target"},
			{
				Kind: yaml.MappingNode,
				Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Value: "kind"},
					{Kind: yaml.ScalarNode, Value: entry.Target.Kind},
					{Kind: yaml.ScalarNode, Value: "name"},
					{Kind: yaml.ScalarNode, Value: entry.Target.Name},
				},
			},
		},
	}
	patches.Content = append(patches.Content, patchEntry)

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return nil, fmt.Errorf("failed to encode kustomization.yaml: %w", err)
	}

	return buf.Bytes(), nil
}

// GeneratePatchFilename generates a filename for a Sartor patch.
func GeneratePatchFilename(target PatchTarget) string {
	return fmt.Sprintf("sartor-%s-%s-patch.yaml", strings.ToLower(target.Kind), target.Name)
}
