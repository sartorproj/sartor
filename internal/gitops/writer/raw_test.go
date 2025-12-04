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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawWriter_UpdateManifest(t *testing.T) {
	tests := []struct {
		name      string
		inputYAML string
		updates   []ResourceUpdate
		wantErr   bool
		checkFunc func(t *testing.T, output string)
	}{
		{
			name: "update single container deployment",
			inputYAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
  namespace: default
spec:
  template:
    spec:
      containers:
      - name: app
        image: nginx:latest
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
          limits:
            cpu: 200m
            memory: 256Mi
`,
			updates: []ResourceUpdate{
				{
					ContainerName: "app",
					Requests: &ResourceValues{
						CPU:    resourcePtr("150m"),
						Memory: resourcePtr("192Mi"),
					},
					Limits: &ResourceValues{
						CPU:    resourcePtr("300m"),
						Memory: resourcePtr("384Mi"),
					},
				},
			},
			wantErr: false,
			checkFunc: func(t *testing.T, output string) {
				assert.Contains(t, output, "150m")
				assert.Contains(t, output, "192Mi")
				assert.Contains(t, output, "300m")
				assert.Contains(t, output, "384Mi")
			},
		},
		{
			name: "update multi-container deployment",
			inputYAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
spec:
  template:
    spec:
      containers:
      - name: app
        image: nginx:latest
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
      - name: sidecar
        image: envoy:latest
        resources:
          requests:
            cpu: 50m
            memory: 64Mi
`,
			updates: []ResourceUpdate{
				{
					ContainerName: "app",
					Requests: &ResourceValues{
						CPU:    resourcePtr("200m"),
						Memory: resourcePtr("256Mi"),
					},
				},
			},
			wantErr: false,
			checkFunc: func(t *testing.T, output string) {
				assert.Contains(t, output, "200m")
				assert.Contains(t, output, "256Mi")
				// Sidecar should remain unchanged
				assert.Contains(t, output, "50m")
			},
		},
		{
			name: "container not found",
			inputYAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
spec:
  template:
    spec:
      containers:
      - name: other
        image: nginx:latest
`,
			updates: []ResourceUpdate{
				{
					ContainerName: "nonexistent",
					Requests: &ResourceValues{
						CPU: resourcePtr("100m"),
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := NewRawWriter()
			updated, err := writer.UpdateManifest([]byte(tt.inputYAML), tt.updates)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, updated)

			if tt.checkFunc != nil {
				tt.checkFunc(t, string(updated))
			}
		})
	}
}

func TestGeneratePRDescription(t *testing.T) {
	updates := []ResourceUpdate{
		{
			ContainerName: "app",
			Requests: &ResourceValues{
				CPU:    resourcePtr("150m"),
				Memory: resourcePtr("192Mi"),
			},
			Limits: &ResourceValues{
				CPU:    resourcePtr("300m"),
				Memory: resourcePtr("384Mi"),
			},
		},
	}

	description := GeneratePRDescription(updates, "7d")
	assert.Contains(t, description, "Sartor Resource Recommendations")
	assert.Contains(t, description, "app")
	assert.Contains(t, description, "150m")
	assert.Contains(t, description, "192Mi")
}

func TestGeneratePRDescriptionDetailed(t *testing.T) {
	tests := []struct {
		name      string
		updates   []ResourceUpdate
		opts      PRDescriptionOptions
		checkFunc func(t *testing.T, output string)
	}{
		{
			name: "with target info",
			updates: []ResourceUpdate{
				{
					ContainerName: "app",
					Requests: &ResourceValues{
						CPU:    resourcePtr("100m"),
						Memory: resourcePtr("128Mi"),
					},
				},
			},
			opts: PRDescriptionOptions{
				AnalysisWindow: "7d",
				Intent:         "Eco",
				TargetName:     "my-deployment",
				TargetKind:     "Deployment",
				Namespace:      "production",
			},
			checkFunc: func(t *testing.T, output string) {
				assert.Contains(t, output, "my-deployment")
				assert.Contains(t, output, "Deployment")
				assert.Contains(t, output, "production")
				assert.Contains(t, output, "Eco")
				assert.Contains(t, output, "P95 + 10%")
			},
		},
		{
			name: "with metrics info",
			updates: []ResourceUpdate{
				{
					ContainerName: "app",
					Requests: &ResourceValues{
						CPU: resourcePtr("200m"),
					},
				},
			},
			opts: PRDescriptionOptions{
				AnalysisWindow: "24h",
				Metrics: map[string]*MetricsInfo{
					"app": {
						P95CPU:    "150m",
						P99CPU:    "180m",
						P95Memory: "100Mi",
						P99Memory: "120Mi",
					},
				},
			},
			checkFunc: func(t *testing.T, output string) {
				assert.Contains(t, output, "Metrics Analysis")
				assert.Contains(t, output, "P95 CPU")
				assert.Contains(t, output, "150m")
			},
		},
		{
			name: "with current resources",
			updates: []ResourceUpdate{
				{
					ContainerName: "app",
					Requests: &ResourceValues{
						CPU:    resourcePtr("200m"),
						Memory: resourcePtr("256Mi"),
					},
				},
			},
			opts: PRDescriptionOptions{
				AnalysisWindow: "7d",
				CurrentResources: map[string]*ResourceValues{
					"app": {
						CPU:    resourcePtr("100m"),
						Memory: resourcePtr("128Mi"),
					},
				},
			},
			checkFunc: func(t *testing.T, output string) {
				assert.Contains(t, output, "Current")
				assert.Contains(t, output, "Recommended")
				assert.Contains(t, output, "Change")
			},
		},
		{
			name: "Balanced intent",
			updates: []ResourceUpdate{
				{
					ContainerName: "app",
					Requests:      &ResourceValues{CPU: resourcePtr("100m")},
				},
			},
			opts: PRDescriptionOptions{
				AnalysisWindow: "7d",
				Intent:         "Balanced",
			},
			checkFunc: func(t *testing.T, output string) {
				assert.Contains(t, output, "Balanced")
				assert.Contains(t, output, "P95 + 20%")
			},
		},
		{
			name: "Critical intent",
			updates: []ResourceUpdate{
				{
					ContainerName: "app",
					Requests:      &ResourceValues{CPU: resourcePtr("100m")},
				},
			},
			opts: PRDescriptionOptions{
				AnalysisWindow: "7d",
				Intent:         "Critical",
			},
			checkFunc: func(t *testing.T, output string) {
				assert.Contains(t, output, "Critical")
				assert.Contains(t, output, "P95 + 40%")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := GeneratePRDescriptionDetailed(tt.updates, tt.opts)
			assert.Contains(t, output, "Sartor Resource Recommendations")
			if tt.checkFunc != nil {
				tt.checkFunc(t, output)
			}
		})
	}
}

func TestRawWriter_UpdateManifest_InvalidYAML(t *testing.T) {
	writer := NewRawWriter()

	_, err := writer.UpdateManifest([]byte("invalid: yaml: : :"), []ResourceUpdate{})
	assert.Error(t, err)
}

func TestRawWriter_UpdateManifest_MissingSpec(t *testing.T) {
	writer := NewRawWriter()

	yamlContent := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
`

	_, err := writer.UpdateManifest([]byte(yamlContent), []ResourceUpdate{
		{ContainerName: "app", Requests: &ResourceValues{CPU: resourcePtr("100m")}},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "spec not found")
}

func TestRawWriter_UpdateManifest_MissingTemplate(t *testing.T) {
	writer := NewRawWriter()

	yamlContent := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
spec:
  replicas: 1
`

	_, err := writer.UpdateManifest([]byte(yamlContent), []ResourceUpdate{
		{ContainerName: "app", Requests: &ResourceValues{CPU: resourcePtr("100m")}},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "spec.template not found")
}

func TestRawWriter_UpdateManifest_CreateResourcesSection(t *testing.T) {
	writer := NewRawWriter()

	// Container without resources section
	yamlContent := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
spec:
  template:
    spec:
      containers:
      - name: app
        image: nginx:latest
`

	updates := []ResourceUpdate{
		{
			ContainerName: "app",
			Requests: &ResourceValues{
				CPU:    resourcePtr("100m"),
				Memory: resourcePtr("128Mi"),
			},
		},
	}

	output, err := writer.UpdateManifest([]byte(yamlContent), updates)
	require.NoError(t, err)
	assert.Contains(t, string(output), "resources")
	assert.Contains(t, string(output), "100m")
	assert.Contains(t, string(output), "128Mi")
}

func TestRawWriter_UpdateManifest_OnlyLimits(t *testing.T) {
	writer := NewRawWriter()

	yamlContent := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
spec:
  template:
    spec:
      containers:
      - name: app
        image: nginx:latest
`

	updates := []ResourceUpdate{
		{
			ContainerName: "app",
			Limits: &ResourceValues{
				CPU:    resourcePtr("500m"),
				Memory: resourcePtr("512Mi"),
			},
		},
	}

	output, err := writer.UpdateManifest([]byte(yamlContent), updates)
	require.NoError(t, err)
	assert.Contains(t, string(output), "limits")
	assert.Contains(t, string(output), "500m")
	assert.Contains(t, string(output), "512Mi")
}

func TestRawWriter_UpdateManifest_OnlyCPU(t *testing.T) {
	writer := NewRawWriter()

	yamlContent := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
spec:
  template:
    spec:
      containers:
      - name: app
        image: nginx:latest
`

	updates := []ResourceUpdate{
		{
			ContainerName: "app",
			Requests: &ResourceValues{
				CPU: resourcePtr("100m"),
			},
		},
	}

	output, err := writer.UpdateManifest([]byte(yamlContent), updates)
	require.NoError(t, err)
	assert.Contains(t, string(output), "100m")
}

func TestRawWriter_UpdateManifest_OnlyMemory(t *testing.T) {
	writer := NewRawWriter()

	yamlContent := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
spec:
  template:
    spec:
      containers:
      - name: app
        image: nginx:latest
`

	updates := []ResourceUpdate{
		{
			ContainerName: "app",
			Requests: &ResourceValues{
				Memory: resourcePtr("256Mi"),
			},
		},
	}

	output, err := writer.UpdateManifest([]byte(yamlContent), updates)
	require.NoError(t, err)
	assert.Contains(t, string(output), "256Mi")
}

func TestCalculateChangePercent(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		recommended string
		want        string
	}{
		{
			name:        "increase",
			current:     "100m",
			recommended: "150m",
			want:        "+50.0%",
		},
		{
			name:        "decrease",
			current:     "200m",
			recommended: "100m",
			want:        "-50.0%",
		},
		{
			name:        "no change",
			current:     "100m",
			recommended: "100m",
			want:        "0.0%",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := resourcePtr(tt.current)
			recommended := resourcePtr(tt.recommended)
			result := calculateChangePercent(current, recommended)
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestCalculateChangePercent_NilValues(t *testing.T) {
	result := calculateChangePercent(nil, resourcePtr("100m"))
	assert.Equal(t, "", result)

	result = calculateChangePercent(resourcePtr("100m"), nil)
	assert.Equal(t, "", result)
}

func TestCalculateChangePercent_ZeroCurrent(t *testing.T) {
	result := calculateChangePercent(resourcePtr("0"), resourcePtr("100m"))
	assert.Equal(t, "N/A", result)
}

func TestResourceUpdate_Fields(t *testing.T) {
	update := ResourceUpdate{
		ContainerName: "my-container",
		Requests: &ResourceValues{
			CPU:    resourcePtr("100m"),
			Memory: resourcePtr("128Mi"),
		},
		Limits: &ResourceValues{
			CPU:    resourcePtr("200m"),
			Memory: resourcePtr("256Mi"),
		},
	}

	assert.Equal(t, "my-container", update.ContainerName)
	assert.NotNil(t, update.Requests)
	assert.NotNil(t, update.Limits)
}

func TestPRDescriptionOptions_Fields(t *testing.T) {
	opts := PRDescriptionOptions{
		AnalysisWindow: "7d",
		Intent:         "Eco",
		TargetName:     "my-app",
		TargetKind:     "Deployment",
		Namespace:      "default",
		CurrentResources: map[string]*ResourceValues{
			"app": {CPU: resourcePtr("100m")},
		},
		Metrics: map[string]*MetricsInfo{
			"app": {P95CPU: "80m"},
		},
	}

	assert.Equal(t, "7d", opts.AnalysisWindow)
	assert.Equal(t, "Eco", opts.Intent)
	assert.Equal(t, "my-app", opts.TargetName)
}

func TestMetricsInfo_Fields(t *testing.T) {
	metrics := MetricsInfo{
		P95CPU:    "100m",
		P99CPU:    "150m",
		P95Memory: "128Mi",
		P99Memory: "192Mi",
	}

	assert.Equal(t, "100m", metrics.P95CPU)
	assert.Equal(t, "150m", metrics.P99CPU)
	assert.Equal(t, "128Mi", metrics.P95Memory)
	assert.Equal(t, "192Mi", metrics.P99Memory)
}
