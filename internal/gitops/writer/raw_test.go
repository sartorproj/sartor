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
