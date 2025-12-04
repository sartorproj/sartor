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

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// notAvailable is the placeholder for unavailable values.
	notAvailable = "N/A"
)

// ResourceUpdate contains the updated resource values for a container.
type ResourceUpdate struct {
	ContainerName string
	Requests      *ResourceValues
	Limits        *ResourceValues
}

// ResourceValues holds CPU and Memory values.
type ResourceValues struct {
	CPU    *resource.Quantity
	Memory *resource.Quantity
}

// RawWriter updates raw Kubernetes manifest YAML files.
type RawWriter struct{}

// NewRawWriter creates a new RawWriter.
func NewRawWriter() *RawWriter {
	return &RawWriter{}
}

// UpdateManifest updates the resource requests/limits in a Kubernetes manifest.
func (w *RawWriter) UpdateManifest(content []byte, updates []ResourceUpdate) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("invalid YAML document")
	}

	// Navigate to spec.template.spec.containers
	containers, err := findContainers(&root)
	if err != nil {
		return nil, err
	}

	// Apply updates to matching containers
	for _, update := range updates {
		if err := updateContainer(containers, update); err != nil {
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

// findContainers navigates the YAML tree to find the containers array.
func findContainers(root *yaml.Node) (*yaml.Node, error) {
	// Root should be a document node containing a mapping
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("expected document node")
	}

	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping node at document root")
	}

	// Navigate: spec -> template -> spec -> containers
	spec := findKey(doc, "spec")
	if spec == nil {
		return nil, fmt.Errorf("spec not found")
	}

	template := findKey(spec, "template")
	if template == nil {
		return nil, fmt.Errorf("spec.template not found")
	}

	templateSpec := findKey(template, "spec")
	if templateSpec == nil {
		return nil, fmt.Errorf("spec.template.spec not found")
	}

	containers := findKey(templateSpec, "containers")
	if containers == nil {
		return nil, fmt.Errorf("spec.template.spec.containers not found")
	}

	if containers.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("containers is not a sequence")
	}

	return containers, nil
}

// findKey finds a key in a mapping node and returns its value node.
func findKey(mapping *yaml.Node, key string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}

	return nil
}

// findKeyIndex finds a key in a mapping node and returns its index.
func findKeyIndex(mapping *yaml.Node, key string) int {
	if mapping.Kind != yaml.MappingNode {
		return -1
	}

	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			return i
		}
	}

	return -1
}

// updateContainer updates a single container's resources.
func updateContainer(containers *yaml.Node, update ResourceUpdate) error {
	for _, container := range containers.Content {
		if container.Kind != yaml.MappingNode {
			continue
		}

		// Find container name
		nameNode := findKey(container, "name")
		if nameNode == nil || nameNode.Value != update.ContainerName {
			continue
		}

		// Find or create resources node
		resourcesIdx := findKeyIndex(container, "resources")
		var resources *yaml.Node

		if resourcesIdx == -1 {
			// Create resources node
			keyNode := &yaml.Node{
				Kind:  yaml.ScalarNode,
				Value: "resources",
			}
			resources = &yaml.Node{
				Kind:    yaml.MappingNode,
				Content: []*yaml.Node{},
			}
			container.Content = append(container.Content, keyNode, resources)
		} else {
			resources = container.Content[resourcesIdx+1]
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

	return fmt.Errorf("container %s not found", update.ContainerName)
}

// updateResourceSection updates the requests or limits section.
func updateResourceSection(resources *yaml.Node, sectionName string, values *ResourceValues) error {
	sectionIdx := findKeyIndex(resources, sectionName)
	var section *yaml.Node

	if sectionIdx == -1 {
		// Create section node
		keyNode := &yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: sectionName,
		}
		section = &yaml.Node{
			Kind:    yaml.MappingNode,
			Content: []*yaml.Node{},
		}
		resources.Content = append(resources.Content, keyNode, section)
	} else {
		section = resources.Content[sectionIdx+1]
	}

	// Update CPU
	if values.CPU != nil {
		updateScalarValue(section, "cpu", values.CPU.String())
	}

	// Update Memory
	if values.Memory != nil {
		updateScalarValue(section, "memory", values.Memory.String())
	}

	return nil
}

// updateScalarValue updates or creates a scalar value in a mapping.
func updateScalarValue(mapping *yaml.Node, key, value string) {
	idx := findKeyIndex(mapping, key)
	if idx == -1 {
		// Create new key-value pair
		keyNode := &yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: key,
		}
		valueNode := &yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: value,
		}
		mapping.Content = append(mapping.Content, keyNode, valueNode)
	} else {
		// Update existing value
		mapping.Content[idx+1].Value = value
	}
}

// PRDescriptionOptions holds options for generating PR descriptions.
type PRDescriptionOptions struct {
	// AnalysisWindow is the time window used for analysis.
	AnalysisWindow string
	// Intent is the optimization intent (Eco, Balanced, Critical).
	Intent string
	// TargetName is the name of the target workload.
	TargetName string
	// TargetKind is the kind of the target workload.
	TargetKind string
	// Namespace is the workload namespace.
	Namespace string
	// CurrentResources maps container names to their current resources.
	CurrentResources map[string]*ResourceValues
	// Metrics contains the observed metrics.
	Metrics map[string]*MetricsInfo
}

// MetricsInfo holds metrics information for a container.
type MetricsInfo struct {
	P95CPU    string
	P99CPU    string
	P95Memory string
	P99Memory string
}

// GeneratePRDescription generates a PR description for the changes.
func GeneratePRDescription(updates []ResourceUpdate, analysisWindow string) string {
	return GeneratePRDescriptionDetailed(updates, PRDescriptionOptions{
		AnalysisWindow: analysisWindow,
	})
}

// GeneratePRDescriptionDetailed generates a detailed PR description with metrics reasoning.
func GeneratePRDescriptionDetailed(updates []ResourceUpdate, opts PRDescriptionOptions) string {
	var buf bytes.Buffer

	buf.WriteString("## 🎯 Sartor Resource Recommendations\n\n")

	// Summary section
	buf.WriteString("### Summary\n\n")
	if opts.TargetName != "" {
		buf.WriteString(fmt.Sprintf("- **Target:** `%s/%s` in namespace `%s`\n", opts.TargetKind, opts.TargetName, opts.Namespace))
	}
	buf.WriteString(fmt.Sprintf("- **Analysis window:** %s\n", opts.AnalysisWindow))
	if opts.Intent != "" {
		buf.WriteString(fmt.Sprintf("- **Optimization intent:** %s\n", opts.Intent))
	}
	buf.WriteString(fmt.Sprintf("- **Containers modified:** %d\n\n", len(updates)))

	// Changes section
	buf.WriteString("### Changes\n\n")

	for _, update := range updates {
		buf.WriteString(fmt.Sprintf("#### Container: `%s`\n\n", update.ContainerName))

		// Get current resources if available
		var currentRes *ResourceValues
		if opts.CurrentResources != nil {
			if current, ok := opts.CurrentResources[update.ContainerName]; ok {
				currentRes = current
			}
		}

		// Get metrics if available
		var metrics *MetricsInfo
		if opts.Metrics != nil {
			metrics = opts.Metrics[update.ContainerName]
		}

		// Write resource tables
		writeResourceTable(&buf, "Requests", update.Requests, currentRes)
		writeResourceTable(&buf, "Limits", update.Limits, currentRes)

		// Metrics explanation
		writeMetricsAnalysis(&buf, metrics)
	}

	// Intent explanation
	writeIntentProfile(&buf, opts.Intent)

	// How to reject
	buf.WriteString("### Actions\n\n")
	buf.WriteString("- **Approve & Merge:** The changes will be applied on next GitOps sync\n")
	buf.WriteString("- **Close with `sartor-ignore` label:** Sartor will pause this Tailoring and stop creating PRs\n")
	buf.WriteString("- **Close without label:** Sartor will create a new PR on next analysis\n\n")

	buf.WriteString("---\n")
	buf.WriteString("*Generated by [Sartor](https://sartorproj.io) - The Kubernetes Resource Tailor*\n")

	return buf.String()
}

// calculateChangePercent calculates the percentage change between two quantities.
func calculateChangePercent(current, recommended *resource.Quantity) string {
	if current == nil || recommended == nil {
		return ""
	}

	currentVal := current.AsApproximateFloat64()
	recommendedVal := recommended.AsApproximateFloat64()

	if currentVal == 0 {
		return notAvailable
	}

	change := ((recommendedVal - currentVal) / currentVal) * 100

	if change > 0 {
		return fmt.Sprintf("+%.1f%%", change)
	}
	return fmt.Sprintf("%.1f%%", change)
}

// writeResourceTable writes a resource table (Requests or Limits) to the buffer.
func writeResourceTable(buf *bytes.Buffer, label string, update *ResourceValues, current *ResourceValues) {
	if update == nil || (update.CPU == nil && update.Memory == nil) {
		return
	}

	fmt.Fprintf(buf, "**%s:**\n\n", label)
	buf.WriteString("| Resource | Current | Recommended | Change |\n")
	buf.WriteString("|----------|---------|-------------|--------|\n")

	if update.CPU != nil {
		currentCPU := notAvailable
		change := ""
		if current != nil && current.CPU != nil {
			currentCPU = current.CPU.String()
			change = calculateChangePercent(current.CPU, update.CPU)
		}
		fmt.Fprintf(buf, "| CPU | %s | %s | %s |\n", currentCPU, update.CPU.String(), change)
	}

	if update.Memory != nil {
		currentMem := notAvailable
		change := ""
		if current != nil && current.Memory != nil {
			currentMem = current.Memory.String()
			change = calculateChangePercent(current.Memory, update.Memory)
		}
		fmt.Fprintf(buf, "| Memory | %s | %s | %s |\n", currentMem, update.Memory.String(), change)
	}
	buf.WriteString("\n")
}

// writeMetricsAnalysis writes the metrics analysis section to the buffer.
func writeMetricsAnalysis(buf *bytes.Buffer, metrics *MetricsInfo) {
	if metrics == nil {
		return
	}

	buf.WriteString("<details>\n<summary>📊 Metrics Analysis</summary>\n\n")
	buf.WriteString("Observed usage during analysis window:\n\n")
	buf.WriteString("| Metric | Value |\n")
	buf.WriteString("|--------|-------|\n")
	if metrics.P95CPU != "" {
		fmt.Fprintf(buf, "| P95 CPU | %s |\n", metrics.P95CPU)
	}
	if metrics.P99CPU != "" {
		fmt.Fprintf(buf, "| P99 CPU | %s |\n", metrics.P99CPU)
	}
	if metrics.P95Memory != "" {
		fmt.Fprintf(buf, "| P95 Memory | %s |\n", metrics.P95Memory)
	}
	if metrics.P99Memory != "" {
		fmt.Fprintf(buf, "| P99 Memory | %s |\n", metrics.P99Memory)
	}
	buf.WriteString("\n")

	buf.WriteString("**Calculation method:**\n")
	buf.WriteString("- Requests are based on P95 usage + buffer (varies by intent)\n")
	buf.WriteString("- Limits are based on P99 usage + buffer (varies by intent)\n\n")
	buf.WriteString("</details>\n\n")
}

// writeIntentProfile writes the intent profile explanation to the buffer.
func writeIntentProfile(buf *bytes.Buffer, intent string) {
	if intent == "" {
		return
	}

	buf.WriteString("### Intent Profile\n\n")
	switch intent {
	case "Eco":
		buf.WriteString("**Eco** - Aggressive optimization for cost savings\n")
		buf.WriteString("- Requests: P95 + 10% buffer\n")
		buf.WriteString("- Limits: P99 + 20% buffer\n")
		buf.WriteString("- Max change per PR: Unlimited\n\n")
	case "Balanced":
		buf.WriteString("**Balanced** - Balance between savings and headroom\n")
		buf.WriteString("- Requests: P95 + 20% buffer\n")
		buf.WriteString("- Limits: P99 + 30% buffer\n")
		buf.WriteString("- Max change per PR: 50%\n\n")
	case "Critical":
		buf.WriteString("**Critical** - Conservative with extra headroom\n")
		buf.WriteString("- Requests: P95 + 40% buffer\n")
		buf.WriteString("- Limits: P99 + 50% buffer\n")
		buf.WriteString("- Max change per PR: 30%\n\n")
	}
}
