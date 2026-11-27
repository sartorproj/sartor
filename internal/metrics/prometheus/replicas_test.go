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

package prometheus

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VictoriaMetrics/metricsql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplicasExpr(t *testing.T) {
	tests := []struct {
		name, kind, clusterLabel, cluster string
		want                              string
		wantErr                           string
	}{
		{
			name: "deployment", kind: KindDeployment,
			want: `max(kube_deployment_spec_replicas{namespace="example-namespace", deployment="my-app"})`,
		},
		{
			name: "statefulset", kind: KindStatefulSet,
			want: `max(kube_statefulset_replicas{namespace="example-namespace", statefulset="my-app"})`,
		},
		{
			name: "daemonset", kind: KindDaemonSet,
			want: `max(kube_daemonset_status_desired_number_scheduled{namespace="example-namespace", daemonset="my-app"})`,
		},
		{
			name: "cluster label", kind: KindDeployment, clusterLabel: "cluster", cluster: "example-cluster",
			want: `max(kube_deployment_spec_replicas{cluster="example-cluster", namespace="example-namespace", deployment="my-app"})`,
		},
		{name: "unsupported kind", kind: "CronJob", wantErr: "unsupported workload kind"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel := podSelector{
				namespace:    "example-namespace",
				workload:     Workload{Kind: tt.kind, Name: "my-app", Cluster: tt.cluster},
				clusterLabel: tt.clusterLabel,
			}
			got, err := replicasExpr(sel)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			_, err = metricsql.Parse(got)
			require.NoError(t, err)
		})
	}
}

func TestReplicaCount(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want *int32
	}{
		{name: "zero", in: 0, want: ptrInt32(0)},
		{name: "integer", in: 3, want: ptrInt32(3)},
		{name: "rounded", in: 2.9999, want: ptrInt32(3)},
		{name: "negative", in: -1},
		{name: "NaN", in: math.NaN()},
		{name: "infinite", in: math.Inf(1)},
		{name: "beyond int32", in: math.MaxInt32 + 1.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, replicaCount(tt.in))
		})
	}
}

func TestParseSnapshot_Replicas(t *testing.T) {
	st := parseSnapshot(agreeingPods(2))
	assert.Nil(t, st.Replicas, "no replica series means unset, not 0")

	v := append(agreeingPods(2), snap(srcReplicas, "", 0))
	st = parseSnapshot(v)
	require.NotNil(t, st.Replicas)
	assert.Equal(t, int32(0), *st.Replicas, "a workload scaled to zero reports 0")

	v = append(agreeingPods(2), snap(srcReplicas, "", 4))
	st = parseSnapshot(v)
	require.NotNil(t, st.Replicas)
	assert.Equal(t, int32(4), *st.Replicas)
}

func TestSnapshotQuery_IncludesReplicas(t *testing.T) {
	sel, err := newPodSelector("example-namespace", Workload{Kind: KindStatefulSet, Name: "my-app", Cluster: "example-cluster"}, "cluster")
	require.NoError(t, err)
	q, err := snapshotQuery(sel)
	require.NoError(t, err)
	assert.Contains(t, q, `kube_statefulset_replicas{cluster="example-cluster", namespace="example-namespace", statefulset="my-app"}`)
	assert.Contains(t, q, `"sartor_source", "replicas"`)
}

func TestClient_GetReplicas(t *testing.T) {
	tests := []struct {
		name         string
		clusterLabel string
		workload     Workload
		body         string
		status       int
		want         *int32
		wantErr      string
		wantQuery    string
	}{
		{
			name:      "reported",
			workload:  Workload{Kind: KindDeployment, Name: "my-app"},
			body:      `[{"metric":{},"value":[1700000000,"3"]}]`,
			want:      ptrInt32(3),
			wantQuery: `max(kube_deployment_spec_replicas{namespace="example-namespace", deployment="my-app"})`,
		},
		{
			name:     "scaled to zero",
			workload: Workload{Kind: KindDeployment, Name: "my-app"},
			body:     `[{"metric":{},"value":[1700000000,"0"]}]`,
			want:     ptrInt32(0),
		},
		{
			name:     "no series",
			workload: Workload{Kind: KindDeployment, Name: "my-app"},
			body:     `[]`,
		},
		{
			name:     "NaN",
			workload: Workload{Kind: KindDeployment, Name: "my-app"},
			body:     `[{"metric":{},"value":[1700000000,"NaN"]}]`,
		},
		{
			name:         "cluster label",
			clusterLabel: "cluster",
			workload:     Workload{Kind: KindDaemonSet, Name: "my-app", Cluster: "example-cluster"},
			body:         `[{"metric":{},"value":[1700000000,"5"]}]`,
			want:         ptrInt32(5),
			wantQuery:    `max(kube_daemonset_status_desired_number_scheduled{cluster="example-cluster", namespace="example-namespace", daemonset="my-app"})`,
		},
		{
			name:         "missing cluster is rejected before querying",
			clusterLabel: "cluster",
			workload:     Workload{Kind: KindDeployment, Name: "my-app"},
			wantErr:      "a target cluster is required",
		},
		{
			name:     "backend error",
			workload: Workload{Kind: KindDeployment, Name: "my-app"},
			status:   http.StatusServiceUnavailable,
			wantErr:  "failed to query replicas",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var query string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query = r.FormValue("query")
				if tt.status != 0 {
					http.Error(w, "unavailable", tt.status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":%s}}`, tt.body)
			}))
			defer server.Close()
			c, err := NewClient(Config{URL: server.URL, ClusterLabel: tt.clusterLabel})
			require.NoError(t, err)

			got, err := c.GetReplicas(context.Background(), "example-namespace", tt.workload)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			if tt.wantQuery != "" {
				assert.Equal(t, tt.wantQuery, query)
			}
		})
	}
}

func ptrInt32(v int32) *int32 { return &v }
