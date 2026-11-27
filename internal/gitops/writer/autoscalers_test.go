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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

const webDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: app
          resources:
            requests:
              cpu: 500m
              memory: 256Mi
            limits:
              memory: 512Mi
`

func qty(s string) *resource.Quantity {
	v := resource.MustParse(s)
	return &v
}

func appUpdate(cpu, memory string) []ResourceUpdate {
	return []ResourceUpdate{{
		ContainerName: "app",
		Requests:      &ResourceValues{CPU: qty(cpu), Memory: qty(memory)},
	}}
}

// lookup returns the scalar at path in document doc of a multi-document YAML.
func lookup(t *testing.T, content string, doc int, path ...string) string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(content))
	var node yaml.Node
	for i := 0; i <= doc; i++ {
		require.NoError(t, dec.Decode(&node))
	}
	n := node.Content[0]
	for _, p := range path {
		switch n.Kind {
		case yaml.SequenceNode:
			idx := int(p[0] - '0')
			require.Less(t, idx, len(n.Content), "path %v", path)
			n = n.Content[idx]
		default:
			n = findKey(n, p)
			require.NotNil(t, n, "path %v", path)
		}
	}
	return n.Value
}

func updateWeb(t *testing.T, content string, updates []ResourceUpdate) *RawResult {
	t.Helper()
	res, err := NewRawWriter().Update([]byte(content), ManifestTarget{Kind: "Deployment", Name: "web"}, updates, RawOptions{})
	require.NoError(t, err)
	return res
}

func vpaDoc(name, mode, target string) string {
	modeLine := ""
	if mode != "" {
		modeLine = "  updatePolicy:\n    updateMode: " + mode + "\n"
	}
	return `---
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata:
  name: ` + name + `
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: ` + target + `
` + modeLine + `  resourcePolicy:
    containerPolicies:
      - containerName: app
        minAllowed:
          cpu: 400m
          memory: 200Mi
        maxAllowed:
          cpu: "1"
          memory: 1Gi
`
}

func TestUpdate_VPAUpdateModes(t *testing.T) {
	tests := []struct {
		mode         string
		wantAdjusted bool
	}{
		{mode: "Off"},
		{mode: "Initial", wantAdjusted: true},
		{mode: "Auto", wantAdjusted: true},
		{mode: "", wantAdjusted: true}, // VPA default is Auto
	}
	for _, tt := range tests {
		t.Run("updateMode "+tt.mode, func(t *testing.T) {
			res := updateWeb(t, webDeployment+vpaDoc("web", tt.mode, "web"), appUpdate("2", "128Mi"))
			out := string(res.Content)

			assert.Equal(t, "2", lookup(t, out, 0, "spec", "template", "spec", "containers", "0", "resources", "requests", "cpu"))
			require.Len(t, res.Autoscalers, 1)
			note := res.Autoscalers[0]
			assert.Equal(t, KindVerticalPodAutoscaler, note.Kind)
			assert.Equal(t, "web", note.Name)
			assert.Equal(t, tt.wantAdjusted, note.Adjusted, note.Details)

			policy := []string{"spec", "resourcePolicy", "containerPolicies", "0"}
			maxCPU := lookup(t, out, 1, append(policy, "maxAllowed", "cpu")...)
			minMem := lookup(t, out, 1, append(policy, "minAllowed", "memory")...)
			if tt.wantAdjusted {
				assert.Equal(t, "2", maxCPU, "maxAllowed must contain the new request")
				assert.Equal(t, "128Mi", minMem, "minAllowed must contain the new request")
			} else {
				assert.Equal(t, "1", maxCPU)
				assert.Equal(t, "200Mi", minMem)
				assert.Contains(t, note.Details[0], "Off")
			}
			// Bounds that already contain the new requests are not touched.
			assert.Equal(t, "400m", lookup(t, out, 1, append(policy, "minAllowed", "cpu")...))
			assert.Equal(t, "1Gi", lookup(t, out, 1, append(policy, "maxAllowed", "memory")...))
		})
	}
}

func TestUpdate_VPAPolicies(t *testing.T) {
	doc := `---
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata:
  name: web
spec:
  targetRef:
    kind: Deployment
    name: web
  resourcePolicy:
    containerPolicies:
      - containerName: "*"
        maxAllowed:
          cpu: 100m
      - containerName: app
        controlledResources: ["memory"]
        maxAllowed:
          cpu: 200m
          memory: 300Mi
`
	res := updateWeb(t, webDeployment+doc, appUpdate("2", "1Gi"))
	out := string(res.Content)
	// The named policy wins over "*", and cpu is not controlled by it.
	assert.Equal(t, "100m", lookup(t, out, 1, "spec", "resourcePolicy", "containerPolicies", "0", "maxAllowed", "cpu"))
	assert.Equal(t, "200m", lookup(t, out, 1, "spec", "resourcePolicy", "containerPolicies", "1", "maxAllowed", "cpu"))
	assert.Equal(t, "1Gi", lookup(t, out, 1, "spec", "resourcePolicy", "containerPolicies", "1", "maxAllowed", "memory"))
}

func TestUpdate_VPAOfAnotherWorkloadUntouched(t *testing.T) {
	in := webDeployment + vpaDoc("other", "Auto", "other")
	res := updateWeb(t, in, appUpdate("2", "128Mi"))
	assert.Empty(t, res.Autoscalers)
	assert.Equal(t, "1", lookup(t, string(res.Content), 1, "spec", "resourcePolicy", "containerPolicies", "0", "maxAllowed", "cpu"))
}

const scaledObject = `---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: web
spec:
  scaleTargetRef:
    name: web
  triggers:
    - type: cpu
      metricType: Utilization
      metadata:
        value: "60"
    - type: memory
      metricType: AverageValue
      metadata:
        value: "300Mi"
    - type: prometheus
      metadata:
        serverAddress: http://prometheus:9090
        query: sum(rate(http_requests_total[1m]))
        threshold: "100"
    - type: memory
      metadata:
        type: Utilization
        value: "80"
`

func TestUpdate_ScaledObjectTriggers(t *testing.T) {
	res := updateWeb(t, webDeployment+scaledObject, appUpdate("1", "512Mi"))
	out := string(res.Content)
	trigger := func(i string, path ...string) string {
		return lookup(t, out, 1, append([]string{"spec", "triggers", i}, path...)...)
	}

	// cpu Utilization 60% of 500m = 300m; with a 1 CPU request that is 30%.
	assert.Equal(t, "30", trigger("0", "metadata", "value"))
	// AverageValue is absolute and does not depend on requests.
	assert.Equal(t, "300Mi", trigger("1", "metadata", "value"))
	// Non-cpu/memory triggers are left untouched.
	assert.Equal(t, "100", trigger("2", "metadata", "threshold"))
	assert.Equal(t, "sum(rate(http_requests_total[1m]))", trigger("2", "metadata", "query"))
	// Legacy metadata.type: memory 80% of 256Mi = 204.8Mi; of 512Mi that is 40%.
	assert.Equal(t, "40", trigger("3", "metadata", "value"))

	require.Len(t, res.Autoscalers, 1)
	note := res.Autoscalers[0]
	assert.Equal(t, KindScaledObject, note.Kind)
	assert.True(t, note.Adjusted)
	assert.True(t, note.KeepsScalingPoint)
	assert.Contains(t, strings.Join(note.Details, "\n"), "AverageValue is an absolute target")
	assert.Contains(t, out, `value: "30"`, "quoting must be preserved")

	// Writing the same values again (e.g. on the PR branch) must not rescale twice.
	again := updateWeb(t, out, appUpdate("1", "512Mi"))
	assert.Equal(t, out, string(again.Content))
	assert.False(t, again.Autoscalers[0].Adjusted)
	assert.True(t, again.Autoscalers[0].KeepsScalingPoint, "already consistent triggers still keep the scaling point")
}

func TestUpdate_ScaledObjectContainerName(t *testing.T) {
	deployment := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: app
          resources:
            requests:
              cpu: 500m
        - name: proxy
          resources:
            requests:
              cpu: 500m
`
	so := `---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: web
spec:
  scaleTargetRef:
    kind: Deployment
    name: web
  triggers:
    - type: cpu
      metricType: Utilization
      metadata:
        containerName: proxy
        value: "50"
    - type: cpu
      metricType: Utilization
      metadata:
        value: "50"
    - type: cpu
      metadata:
        value: "50"
`
	res := updateWeb(t, deployment+so, []ResourceUpdate{{ContainerName: "app", Requests: &ResourceValues{CPU: qty("1500m")}}})
	out := string(res.Content)
	// Only app changed: a proxy-only trigger keeps its value.
	assert.Equal(t, "50", lookup(t, out, 1, "spec", "triggers", "0", "metadata", "value"))
	// Pod-level: 50% of 1000m = 500m; of 2000m that is 25%.
	assert.Equal(t, "25", lookup(t, out, 1, "spec", "triggers", "1", "metadata", "value"))
	// No metric type: not guessed.
	assert.Equal(t, "50", lookup(t, out, 1, "spec", "triggers", "2", "metadata", "value"))
	assert.Contains(t, strings.Join(res.Autoscalers[0].Details, "\n"), `metric type ""`)
	assert.False(t, res.Autoscalers[0].KeepsScalingPoint, "a trigger without metric type is not kept")
}

func TestUpdate_ScaledObjectMinimumOnePercent(t *testing.T) {
	so := strings.Replace(scaledObject, `value: "60"`, `value: "1"`, 1)
	res := updateWeb(t, webDeployment+so, appUpdate("64", "256Mi"))
	assert.Equal(t, "1", lookup(t, string(res.Content), 1, "spec", "triggers", "0", "metadata", "value"))
}

// TestUpdate_HPAAndVPAOnSameWorkload: a file carrying a VPA and a
// KEDA-managed HPA's ScaledObject next to the workload gets both adjusted.
func TestUpdate_HPAAndVPAOnSameWorkload(t *testing.T) {
	res := updateWeb(t, webDeployment+vpaDoc("web", "Initial", "web")+scaledObject, appUpdate("2", "128Mi"))
	require.Len(t, res.Autoscalers, 2)
	assert.Equal(t, KindVerticalPodAutoscaler, res.Autoscalers[0].Kind)
	assert.Equal(t, KindScaledObject, res.Autoscalers[1].Kind)
	assert.True(t, res.Autoscalers[0].Adjusted)
	assert.True(t, res.Autoscalers[1].Adjusted)
	out := string(res.Content)
	assert.Equal(t, "2", lookup(t, out, 1, "spec", "resourcePolicy", "containerPolicies", "0", "maxAllowed", "cpu"))
	assert.Equal(t, "15", lookup(t, out, 2, "spec", "triggers", "0", "metadata", "value"))
}

func TestUpdate_SkipMissingContainers(t *testing.T) {
	updates := append(appUpdate("1", "256Mi"), ResourceUpdate{ContainerName: "injected-proxy", Requests: &ResourceValues{CPU: qty("100m")}})

	_, err := NewRawWriter().Update([]byte(webDeployment), ManifestTarget{Kind: "Deployment", Name: "web"}, updates, RawOptions{})
	require.Error(t, err, "without the option a missing container is an error")

	res, err := NewRawWriter().Update([]byte(webDeployment), ManifestTarget{Kind: "Deployment", Name: "web"}, updates,
		RawOptions{SkipMissingContainers: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"injected-proxy"}, res.Skipped)
	assert.NotContains(t, string(res.Content), "injected-proxy")
	assert.Equal(t, "1", lookup(t, string(res.Content), 0, "spec", "template", "spec", "containers", "0", "resources", "requests", "cpu"))
}

func TestKustomize_SkipMissingContainers(t *testing.T) {
	patch := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: app
          resources:
            requests:
              cpu: 500m
`
	updates := append(appUpdate("1", "256Mi"), ResourceUpdate{ContainerName: "injected-proxy", Requests: &ResourceValues{CPU: qty("100m")}})
	target := ManifestTarget{Kind: "Deployment", Name: "web"}

	out, skipped, err := NewKustomizeWriter().UpdateExistingPatchFor([]byte(patch), target, updates, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"injected-proxy"}, skipped)
	assert.NotContains(t, string(out), "injected-proxy")

	out, skipped, err = NewKustomizeWriter().UpdateExistingPatchFor([]byte(patch), target, updates, false)
	require.NoError(t, err)
	assert.Empty(t, skipped)
	assert.Contains(t, string(out), "injected-proxy", "without the option missing containers are appended")
}

func updateWebWith(t *testing.T, content string, updates []ResourceUpdate, opts RawOptions) *RawResult {
	t.Helper()
	res, err := NewRawWriter().Update([]byte(content), ManifestTarget{Kind: "Deployment", Name: "web"}, updates, opts)
	require.NoError(t, err)
	return res
}

func appBounds(minCPU, maxCPU string) map[string]map[string]VPABounds {
	return map[string]map[string]VPABounds{"web": {"app": {
		MinAllowed: &ResourceValues{CPU: qty(minCPU)},
		MaxAllowed: &ResourceValues{CPU: qty(maxCPU)},
	}}}
}

// The existing policy has cpu 400m-1: minAllowed is set to the recommended
// 120m, a narrower maxAllowed is raised to the recommended 1800m.
func TestUpdate_VPABoundsNarrowerExisting(t *testing.T) {
	res := updateWebWith(t, webDeployment+vpaDoc("web", "Auto", "web"), appUpdate("120m", "256Mi"),
		RawOptions{VPABounds: appBounds("120m", "1800m")})
	out := string(res.Content)
	policy := []string{"spec", "resourcePolicy", "containerPolicies", "0"}
	assert.Equal(t, "120m", lookup(t, out, 1, append(policy, "minAllowed", "cpu")...))
	assert.Equal(t, "1800m", lookup(t, out, 1, append(policy, "maxAllowed", "cpu")...))
	assert.Equal(t, "200Mi", lookup(t, out, 1, append(policy, "minAllowed", "memory")...), "no memory recommendation: unchanged")
	require.Len(t, res.Autoscalers, 1)
	assert.True(t, res.Autoscalers[0].Adjusted)
	details := strings.Join(res.Autoscalers[0].Details, "\n")
	assert.Contains(t, details, "minAllowed.cpu 400m -> 120m (container app)")
	assert.Contains(t, details, "maxAllowed.cpu 1 -> 1800m (container app)")

	again := updateWebWith(t, out, appUpdate("120m", "256Mi"), RawOptions{VPABounds: appBounds("120m", "1800m")})
	assert.Equal(t, out, string(again.Content), "idempotent")
	assert.False(t, again.Autoscalers[0].Adjusted)
}

func TestUpdate_VPABoundsWiderMaxKept(t *testing.T) {
	res := updateWebWith(t, webDeployment+vpaDoc("web", "Auto", "web"), appUpdate("450m", "256Mi"),
		RawOptions{VPABounds: appBounds("450m", "800m")})
	out := string(res.Content)
	policy := []string{"spec", "resourcePolicy", "containerPolicies", "0"}
	assert.Equal(t, "450m", lookup(t, out, 1, append(policy, "minAllowed", "cpu")...))
	assert.Equal(t, "1", lookup(t, out, 1, append(policy, "maxAllowed", "cpu")...), "a wider maxAllowed is never narrowed")
	assert.Contains(t, strings.Join(res.Autoscalers[0].Details, "\n"), "maxAllowed.cpu 1 kept (wider than the recommended 800m)")
}

func TestUpdate_VPABoundsUpdateModeOff(t *testing.T) {
	in := webDeployment + vpaDoc("web", "Off", "web")
	res := updateWebWith(t, in, appUpdate("500m", "256Mi"), RawOptions{VPABounds: appBounds("120m", "1800m")})
	assert.Equal(t, in, string(res.Content))
	assert.False(t, res.Autoscalers[0].Adjusted)
}

func TestUpdate_VPABoundsWithoutPolicies(t *testing.T) {
	doc := `---
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata:
  name: web
spec:
  targetRef:
    kind: Deployment
    name: web
  updatePolicy:
    updateMode: Recreate
`
	res := updateWebWith(t, webDeployment+doc, appUpdate("120m", "256Mi"), RawOptions{VPABounds: appBounds("120m", "1800m")})
	out := string(res.Content)
	policy := []string{"spec", "resourcePolicy", "containerPolicies", "0"}
	assert.Equal(t, "app", lookup(t, out, 1, append(policy, "containerName")...))
	assert.Equal(t, "120m", lookup(t, out, 1, append(policy, "minAllowed", "cpu")...))
	assert.Equal(t, "1800m", lookup(t, out, 1, append(policy, "maxAllowed", "cpu")...))
	assert.True(t, res.Autoscalers[0].Adjusted)
	assert.Contains(t, res.Autoscalers[0].Details, "added a containerPolicy for container app")
	assert.Equal(t, "Recreate", lookup(t, out, 1, "spec", "updatePolicy", "updateMode"))
}

func TestUpdate_VPABoundsCopiesWildcardPolicy(t *testing.T) {
	doc := `---
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata:
  name: web
spec:
  targetRef:
    kind: Deployment
    name: web
  resourcePolicy:
    containerPolicies:
      - containerName: "*"
        controlledResources: ["memory"]
        maxAllowed:
          memory: 2Gi
`
	bounds := map[string]map[string]VPABounds{"web": {"app": {
		MinAllowed: &ResourceValues{CPU: qty("120m"), Memory: qty("100Mi")},
		MaxAllowed: &ResourceValues{CPU: qty("1800m"), Memory: qty("1Gi")},
	}}}
	res := updateWebWith(t, webDeployment+doc, appUpdate("500m", "100Mi"), RawOptions{VPABounds: bounds})
	out := string(res.Content)
	assert.Equal(t, "*", lookup(t, out, 1, "spec", "resourcePolicy", "containerPolicies", "0", "containerName"))
	assert.Equal(t, "2Gi", lookup(t, out, 1, "spec", "resourcePolicy", "containerPolicies", "0", "maxAllowed", "memory"), "the * policy is not changed")
	added := []string{"spec", "resourcePolicy", "containerPolicies", "1"}
	assert.Equal(t, "app", lookup(t, out, 1, append(added, "containerName")...))
	assert.Equal(t, "memory", lookup(t, out, 1, append(added, "controlledResources", "0")...), "copied from *")
	assert.Equal(t, "100Mi", lookup(t, out, 1, append(added, "minAllowed", "memory")...))
	assert.Equal(t, "2Gi", lookup(t, out, 1, append(added, "maxAllowed", "memory")...), "the wider copied maxAllowed is kept")
	assert.NotContains(t, out, "1800m", "cpu is not controlled")
}

func TestUpdate_VPABoundsPolicyModeOff(t *testing.T) {
	for name, policy := range map[string]string{
		"own policy": `      - containerName: app
        mode: "Off"
`,
		"wildcard policy": `      - containerName: "*"
        mode: "Off"
`,
	} {
		t.Run(name, func(t *testing.T) {
			doc := `---
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata:
  name: web
spec:
  targetRef:
    kind: Deployment
    name: web
  resourcePolicy:
    containerPolicies:
` + policy
			in := webDeployment + doc
			res := updateWebWith(t, in, appUpdate("500m", "256Mi"), RawOptions{VPABounds: appBounds("120m", "1800m")})
			assert.NotContains(t, string(res.Content), "1800m")
			assert.False(t, res.Autoscalers[0].Adjusted)
			assert.Contains(t, strings.Join(res.Autoscalers[0].Details, "\n"), "policy mode is Off")
		})
	}
}

// Requests sized for the cpu Utilization target keep the cpu trigger as it
// is; the memory Utilization triggers are still rescaled.
func TestUpdate_ScaledObjectKeepUtilizationTargets(t *testing.T) {
	res := updateWebWith(t, webDeployment+scaledObject, appUpdate("1", "512Mi"), RawOptions{KeepUtilizationTargets: []string{"cpu"}})
	out := string(res.Content)
	assert.Equal(t, "60", lookup(t, out, 1, "spec", "triggers", "0", "metadata", "value"))
	assert.Equal(t, "40", lookup(t, out, 1, "spec", "triggers", "3", "metadata", "value"))
	note := res.Autoscalers[0]
	assert.True(t, note.Adjusted)
	assert.True(t, note.KeepsScalingPoint)
	assert.Contains(t, strings.Join(note.Details, "\n"), "sized for")
}
