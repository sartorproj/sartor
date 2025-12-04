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

func TestKustomizeWriter_GeneratePatch(t *testing.T) {
	tests := []struct {
		name       string
		target     PatchTarget
		updates    []ResourceUpdate
		wantErr    bool
		checkPatch func(t *testing.T, patch []byte)
	}{
		{
			name: "deployment patch",
			target: PatchTarget{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "my-app",
				Namespace:  "default",
			},
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
			checkPatch: func(t *testing.T, patch []byte) {
				patchStr := string(patch)
				assert.Contains(t, patchStr, "kind: Deployment")
				assert.Contains(t, patchStr, "name: my-app")
				assert.Contains(t, patchStr, "name: app")
				assert.Contains(t, patchStr, "150m")
				assert.Contains(t, patchStr, "192Mi")
			},
		},
		{
			name: "statefulset patch",
			target: PatchTarget{
				APIVersion: "apps/v1",
				Kind:       "StatefulSet",
				Name:       "my-db",
				Namespace:  "default",
			},
			updates: []ResourceUpdate{
				{
					ContainerName: "postgres",
					Requests: &ResourceValues{
						CPU:    resourcePtr("500m"),
						Memory: resourcePtr("1Gi"),
					},
				},
			},
			wantErr: false,
			checkPatch: func(t *testing.T, patch []byte) {
				patchStr := string(patch)
				assert.Contains(t, patchStr, "kind: StatefulSet")
				assert.Contains(t, patchStr, "name: my-db")
				assert.Contains(t, patchStr, "name: postgres")
			},
		},
		{
			name: "multi-container patch",
			target: PatchTarget{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "multi-app",
				Namespace:  "default",
			},
			updates: []ResourceUpdate{
				{
					ContainerName: "app",
					Requests: &ResourceValues{
						CPU: resourcePtr("100m"),
					},
				},
				{
					ContainerName: "sidecar",
					Requests: &ResourceValues{
						CPU: resourcePtr("50m"),
					},
				},
			},
			wantErr: false,
			checkPatch: func(t *testing.T, patch []byte) {
				patchStr := string(patch)
				assert.Contains(t, patchStr, "name: app")
				assert.Contains(t, patchStr, "name: sidecar")
				assert.Contains(t, patchStr, "100m")
				assert.Contains(t, patchStr, "50m")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := NewKustomizeWriter()
			patch, err := writer.GeneratePatch(tt.target, tt.updates)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.NotEmpty(t, patch)

			if tt.checkPatch != nil {
				tt.checkPatch(t, patch)
			}
		})
	}
}

func TestKustomizeWriter_GenerateJSONPatch(t *testing.T) {
	updates := []ResourceUpdate{
		{
			ContainerName: "app",
			Requests: &ResourceValues{
				CPU: resourcePtr("200m"),
			},
		},
	}

	writer := NewKustomizeWriter()
	patch, err := writer.GenerateJSONPatch(updates)

	assert.NoError(t, err)
	assert.NotEmpty(t, patch)
	patchStr := string(patch)
	assert.Contains(t, patchStr, "op")
	assert.Contains(t, patchStr, "path")
}
