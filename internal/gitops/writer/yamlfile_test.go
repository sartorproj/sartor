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
)

const rawManifest = `# Payment service — owned by team-payments
---
apiVersion: v1
kind: Service
metadata:
  name: payment
spec:
  ports:
  - port: 80

---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: payment # keep in sync with the Service
spec:
  template:
    spec:
      containers:
      # main container
      - name: app
        image: "payment:1.2.3"
        resources:
          requests:
            cpu: "250m"   # tuned in 2024
            memory: 256Mi

          limits:
            cpu: 1
            memory: '512Mi'
      - name: sidecar
        image: envoy
`

func TestUpdatesPreserveFormatting(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		apply  func([]byte) ([]byte, error)
		expect string
	}{
		{
			name:  "raw multi-document: only edited scalars change, quotes and comments kept",
			input: rawManifest,
			apply: func(in []byte) ([]byte, error) {
				return NewRawWriter().UpdateManifestFor(in, ManifestTarget{Kind: "Deployment", Name: "payment"}, []ResourceUpdate{{
					ContainerName: "app",
					Requests:      &ResourceValues{CPU: resourcePtr("300m"), Memory: resourcePtr("320Mi")},
					Limits:        &ResourceValues{CPU: resourcePtr("1500m"), Memory: resourcePtr("640Mi")},
				}})
			},
			expect: strings.NewReplacer(
				`cpu: "250m"   # tuned in 2024`, `cpu: "300m"   # tuned in 2024`,
				"memory: 256Mi", "memory: 320Mi",
				"cpu: 1\n", "cpu: 1500m\n",
				"memory: '512Mi'", "memory: '640Mi'",
			).Replace(rawManifest),
		},
		{
			name: "helm values: blank lines and comments untouched",
			input: `# Default values for api.
replicaCount: 2

api:
  # Resources for the API pods
  resources:
    requests:
      cpu: 100m # p95 + 20%
      memory: 128Mi

worker:
  resources: {}
`,
			apply: func(in []byte) ([]byte, error) {
				w := NewHelmWriter()
				return w.UpdateValues(in, []ResourceUpdate{{
					ContainerName: "api",
					Requests:      &ResourceValues{CPU: resourcePtr("150m"), Memory: resourcePtr("192Mi")},
				}}, "api.resources")
			},
			expect: `# Default values for api.
replicaCount: 2

api:
  # Resources for the API pods
  resources:
    requests:
      cpu: 150m # p95 + 20%
      memory: 192Mi

worker:
  resources: {}
`,
		},
		{
			name: "kustomize patch with compact sequences",
			input: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
      - name: web
        resources:
          requests:
            cpu: 100m
`,
			apply: func(in []byte) ([]byte, error) {
				return NewKustomizeWriter().UpdatePatchFor(in, ManifestTarget{Kind: "Deployment", Name: "web"}, []ResourceUpdate{{
					ContainerName: "web",
					Requests:      &ResourceValues{CPU: resourcePtr("200m")},
				}})
			},
			expect: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
      - name: web
        resources:
          requests:
            cpu: 200m
`,
		},
		{
			name:  "equivalent quantity is not rewritten",
			input: "resources:\n  requests:\n    cpu: 1 # one core\n",
			apply: func(in []byte) ([]byte, error) {
				return NewHelmWriter().UpdateValues(in, []ResourceUpdate{{
					ContainerName: "main", Requests: &ResourceValues{CPU: resourcePtr("1000m")},
				}}, "resources")
			},
			expect: "resources:\n  requests:\n    cpu: 1 # one core\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := tt.apply([]byte(tt.input))
			require.NoError(t, err)
			assert.Equal(t, tt.expect, string(out))

			again, err := tt.apply(out)
			require.NoError(t, err)
			assert.Equal(t, string(out), string(again), "applying the same update twice must be a no-op")
		})
	}
}

func TestRawWriterTargetSelection(t *testing.T) {
	update := []ResourceUpdate{{ContainerName: "app", Requests: &ResourceValues{CPU: resourcePtr("1")}}}
	tests := []struct {
		name    string
		target  ManifestTarget
		wantErr string
	}{
		{name: "matching kind and name", target: ManifestTarget{Kind: "Deployment", Name: "payment"}},
		{name: "wrong name", target: ManifestTarget{Kind: "Deployment", Name: "pay"}, wantErr: `Deployment "pay" not found`},
		{name: "same name, other kind", target: ManifestTarget{Kind: "DaemonSet", Name: "payment"}, wantErr: "not found in manifest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRawWriter().UpdateManifestFor([]byte(rawManifest), tt.target, update)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
