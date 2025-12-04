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

func TestKustomizeWriter_GenerateJSONPatch_AllResources(t *testing.T) {
	updates := []ResourceUpdate{
		{
			ContainerName: "app",
			Requests: &ResourceValues{
				CPU:    resourcePtr("100m"),
				Memory: resourcePtr("128Mi"),
			},
			Limits: &ResourceValues{
				CPU:    resourcePtr("200m"),
				Memory: resourcePtr("256Mi"),
			},
		},
	}

	writer := NewKustomizeWriter()
	patch, err := writer.GenerateJSONPatch(updates)

	assert.NoError(t, err)
	patchStr := string(patch)
	assert.Contains(t, patchStr, "resources/requests/cpu")
	assert.Contains(t, patchStr, "resources/requests/memory")
	assert.Contains(t, patchStr, "resources/limits/cpu")
	assert.Contains(t, patchStr, "resources/limits/memory")
}

func TestKustomizeWriter_GeneratePatch_OnlyRequests(t *testing.T) {
	target := PatchTarget{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-app",
		Namespace:  "default",
	}

	updates := []ResourceUpdate{
		{
			ContainerName: "app",
			Requests: &ResourceValues{
				CPU:    resourcePtr("100m"),
				Memory: resourcePtr("128Mi"),
			},
		},
	}

	writer := NewKustomizeWriter()
	patch, err := writer.GeneratePatch(target, updates)

	assert.NoError(t, err)
	patchStr := string(patch)
	assert.Contains(t, patchStr, "requests")
	assert.Contains(t, patchStr, "100m")
	assert.Contains(t, patchStr, "128Mi")
}

func TestKustomizeWriter_GeneratePatch_OnlyLimits(t *testing.T) {
	target := PatchTarget{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-app",
		Namespace:  "default",
	}

	updates := []ResourceUpdate{
		{
			ContainerName: "app",
			Limits: &ResourceValues{
				CPU:    resourcePtr("500m"),
				Memory: resourcePtr("512Mi"),
			},
		},
	}

	writer := NewKustomizeWriter()
	patch, err := writer.GeneratePatch(target, updates)

	assert.NoError(t, err)
	patchStr := string(patch)
	assert.Contains(t, patchStr, "limits")
	assert.Contains(t, patchStr, "500m")
	assert.Contains(t, patchStr, "512Mi")
}

func TestKustomizeWriter_UpdatePatch(t *testing.T) {
	existingPatch := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: default
spec:
  template:
    spec:
      containers:
      - name: app
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
`

	updates := []ResourceUpdate{
		{
			ContainerName: "app",
			Requests: &ResourceValues{
				CPU:    resourcePtr("200m"),
				Memory: resourcePtr("256Mi"),
			},
		},
	}

	writer := NewKustomizeWriter()
	updated, err := writer.UpdatePatch([]byte(existingPatch), updates)

	assert.NoError(t, err)
	updatedStr := string(updated)
	assert.Contains(t, updatedStr, "200m")
	assert.Contains(t, updatedStr, "256Mi")
}

func TestKustomizeWriter_UpdatePatch_InvalidYAML(t *testing.T) {
	writer := NewKustomizeWriter()
	_, err := writer.UpdatePatch([]byte("invalid: yaml: : :"), []ResourceUpdate{})
	assert.Error(t, err)
}

func TestKustomizeWriter_UpdatePatch_MissingSpec(t *testing.T) {
	invalidPatch := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
`

	writer := NewKustomizeWriter()
	_, err := writer.UpdatePatch([]byte(invalidPatch), []ResourceUpdate{
		{ContainerName: "app", Requests: &ResourceValues{CPU: resourcePtr("100m")}},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "spec not found")
}

func TestKustomizeWriter_UpdatePatch_MissingTemplate(t *testing.T) {
	invalidPatch := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  replicas: 1
`

	writer := NewKustomizeWriter()
	_, err := writer.UpdatePatch([]byte(invalidPatch), []ResourceUpdate{
		{ContainerName: "app", Requests: &ResourceValues{CPU: resourcePtr("100m")}},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "spec.template not found")
}

func TestKustomizeWriter_UpdatePatch_AddNewContainer(t *testing.T) {
	existingPatch := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  template:
    spec:
      containers:
      - name: app
        resources:
          requests:
            cpu: 100m
`

	updates := []ResourceUpdate{
		{
			ContainerName: "sidecar",
			Requests: &ResourceValues{
				CPU: resourcePtr("50m"),
			},
		},
	}

	writer := NewKustomizeWriter()
	updated, err := writer.UpdatePatch([]byte(existingPatch), updates)

	assert.NoError(t, err)
	updatedStr := string(updated)
	assert.Contains(t, updatedStr, "sidecar")
	assert.Contains(t, updatedStr, "50m")
}

func TestKustomizeWriter_UpdateKustomization(t *testing.T) {
	kustomization := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
`

	entry := KustomizationEntry{
		PatchPath: "sartor-deployment-my-app-patch.yaml",
		Target: PatchTarget{
			Kind: "Deployment",
			Name: "my-app",
		},
	}

	writer := NewKustomizeWriter()
	updated, err := writer.UpdateKustomization([]byte(kustomization), entry)

	assert.NoError(t, err)
	updatedStr := string(updated)
	assert.Contains(t, updatedStr, "patches")
	assert.Contains(t, updatedStr, "sartor-deployment-my-app-patch.yaml")
}

func TestKustomizeWriter_UpdateKustomization_InvalidYAML(t *testing.T) {
	writer := NewKustomizeWriter()
	_, err := writer.UpdateKustomization([]byte("invalid: yaml: : :"), KustomizationEntry{})
	assert.Error(t, err)
}

func TestKustomizeWriter_UpdateKustomization_EmptyDocument(t *testing.T) {
	writer := NewKustomizeWriter()
	_, err := writer.UpdateKustomization([]byte(""), KustomizationEntry{})
	assert.Error(t, err)
}

func TestKustomizeWriter_UpdateKustomization_DuplicatePatch(t *testing.T) {
	kustomization := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
patches:
  - path: sartor-deployment-my-app-patch.yaml
    target:
      kind: Deployment
      name: my-app
`

	entry := KustomizationEntry{
		PatchPath: "sartor-deployment-my-app-patch.yaml",
		Target: PatchTarget{
			Kind: "Deployment",
			Name: "my-app",
		},
	}

	writer := NewKustomizeWriter()
	updated, err := writer.UpdateKustomization([]byte(kustomization), entry)

	assert.NoError(t, err)
	// Should return original content since patch already exists
	assert.Equal(t, kustomization, string(updated))
}

func TestKustomizeWriter_UpdateKustomization_SimplePatchPath(t *testing.T) {
	kustomization := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
patches:
  - existing-patch.yaml
`

	entry := KustomizationEntry{
		PatchPath: "existing-patch.yaml",
		Target: PatchTarget{
			Kind: "Deployment",
			Name: "my-app",
		},
	}

	writer := NewKustomizeWriter()
	updated, err := writer.UpdateKustomization([]byte(kustomization), entry)

	assert.NoError(t, err)
	// Should return original content since patch already exists
	assert.Equal(t, kustomization, string(updated))
}

func TestKustomizeWriter_UpdateKustomization_PatchesStrategicMerge(t *testing.T) {
	kustomization := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
patchesStrategicMerge:
  - existing-patch.yaml
`

	entry := KustomizationEntry{
		PatchPath: "new-patch.yaml",
		Target: PatchTarget{
			Kind: "Deployment",
			Name: "my-app",
		},
	}

	writer := NewKustomizeWriter()
	updated, err := writer.UpdateKustomization([]byte(kustomization), entry)

	assert.NoError(t, err)
	updatedStr := string(updated)
	assert.Contains(t, updatedStr, "new-patch.yaml")
}

func TestGeneratePatchFilename(t *testing.T) {
	tests := []struct {
		target   PatchTarget
		expected string
	}{
		{
			target:   PatchTarget{Kind: "Deployment", Name: "my-app"},
			expected: "sartor-deployment-my-app-patch.yaml",
		},
		{
			target:   PatchTarget{Kind: "StatefulSet", Name: "my-db"},
			expected: "sartor-statefulset-my-db-patch.yaml",
		},
		{
			target:   PatchTarget{Kind: "DaemonSet", Name: "node-agent"},
			expected: "sartor-daemonset-node-agent-patch.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := GeneratePatchFilename(tt.target)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestPatchTarget_Fields(t *testing.T) {
	target := PatchTarget{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-app",
		Namespace:  "production",
	}

	assert.Equal(t, "apps/v1", target.APIVersion)
	assert.Equal(t, "Deployment", target.Kind)
	assert.Equal(t, "my-app", target.Name)
	assert.Equal(t, "production", target.Namespace)
}

func TestKustomizationEntry_Fields(t *testing.T) {
	entry := KustomizationEntry{
		PatchPath: "patches/my-patch.yaml",
		Target: PatchTarget{
			Kind: "Deployment",
			Name: "my-app",
		},
	}

	assert.Equal(t, "patches/my-patch.yaml", entry.PatchPath)
	assert.Equal(t, "Deployment", entry.Target.Kind)
}

func TestJSONPatch_Fields(t *testing.T) {
	patch := JSONPatch{
		Op:    "replace",
		Path:  "/spec/template/spec/containers/0/resources/requests/cpu",
		Value: "200m",
	}

	assert.Equal(t, "replace", patch.Op)
	assert.Equal(t, "/spec/template/spec/containers/0/resources/requests/cpu", patch.Path)
	assert.Equal(t, "200m", patch.Value)
}

func TestStrategicMergePatch_Structure(t *testing.T) {
	patch := StrategicMergePatch{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Metadata: StrategicMergePatchMeta{
			Name:      "my-app",
			Namespace: "default",
		},
		Spec: &StrategicMergePatchSpec{
			Template: &PodTemplateSpec{
				Spec: &PodSpec{
					Containers: []ContainerPatch{
						{
							Name: "app",
							Resources: &ResourcesPatch{
								Requests: map[string]string{"cpu": "100m"},
								Limits:   map[string]string{"cpu": "200m"},
							},
						},
					},
				},
			},
		},
	}

	assert.Equal(t, "apps/v1", patch.APIVersion)
	assert.Equal(t, "Deployment", patch.Kind)
	assert.Equal(t, "my-app", patch.Metadata.Name)
	assert.Len(t, patch.Spec.Template.Spec.Containers, 1)
}
