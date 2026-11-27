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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/VictoriaMetrics/metricsql"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// snap builds a snapshot sample: source, aggregation, extra label pairs.
func snap(source, agg string, value float64, kv ...string) *model.Sample {
	m := model.Metric{sourceLabel: model.LabelValue(source)}
	if agg != "" {
		m[aggLabel] = model.LabelValue(agg)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[model.LabelName(kv[i])] = model.LabelValue(kv[i+1])
	}
	return &model.Sample{Metric: m, Value: model.SampleValue(value)}
}

// agreeingPods returns the samples of n pods that all run container app with
// 250m/512Mi requests and a 1Gi memory limit.
func agreeingPods(n float64) model.Vector {
	c := func(source, res string, v float64) []*model.Sample {
		kv := []string{"container", "app"}
		if res != "" {
			kv = append(kv, "resource", res)
		}
		return []*model.Sample{snap(source, "max", v, kv...), snap(source, "min", v, kv...), snap(source, "count", n, kv...)}
	}
	v := make(model.Vector, 0, 14)
	v = append(v, snap(srcExists, "", 1), snap(srcPods, "count", n))
	v = append(v, c(srcInfo, "", 1)...)
	v = append(v, c(srcRequests, "cpu", 0.25)...)
	v = append(v, c(srcRequests, "memory", 512*1024*1024)...)
	v = append(v, c(srcLimits, "memory", 1024*1024*1024)...)
	return v
}

func TestSnapshotQuery_IsValidAndScoped(t *testing.T) {
	for _, tt := range []struct {
		name, clusterLabel, cluster string
		wantScope                   string
	}{
		{name: "namespace only", wantScope: `namespace="blue"`},
		{name: "cluster and namespace", clusterLabel: "cluster", cluster: "east", wantScope: `cluster="east", namespace="blue"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sel, err := newPodSelector("blue", Workload{Kind: KindDeployment, Name: "web", Cluster: tt.cluster}, tt.clusterLabel)
			require.NoError(t, err)
			sel.ownerJoin = true
			q, err := snapshotQuery(sel)
			require.NoError(t, err)
			_, err = metricsql.Parse(q)
			require.NoError(t, err, "query must be valid MetricsQL: %s", q)

			// Every selector of the snapshot is scoped to the target.
			selectors := strings.Count(q, "{")
			assert.Equal(t, selectors, strings.Count(q, "{"+tt.wantScope), q)
			assert.Contains(t, q, `deployment="web"`)
			assert.Contains(t, q, `scaletargetref_name="web"`)
			assert.Contains(t, q, `target_name="web"`)
			assert.Contains(t, q, `reason="OOMKilled"`)
			assert.Contains(t, q, `phase=~"Pending|Running"`)
			if tt.clusterLabel == "" {
				assert.NotContains(t, q, "cluster=")
			}
		})
	}
}

func TestNewPodSelector_Cluster(t *testing.T) {
	_, err := newPodSelector("blue", Workload{Kind: KindDeployment, Name: "web"}, "cluster")
	assert.ErrorContains(t, err, "a target cluster is required")
	_, err = newPodSelector("blue", Workload{Kind: KindDeployment, Name: "web", Cluster: "east"}, "")
	assert.ErrorContains(t, err, "sets no prometheus.clusterLabel", "a cluster without a label would mix clusters")
	_, err = newPodSelector("blue", Workload{Kind: KindDeployment, Name: "web", Cluster: "east"}, "bad-label")
	assert.ErrorContains(t, err, "invalid cluster label")
	sel, err := newPodSelector("blue", Workload{Kind: KindDeployment, Name: "web", Cluster: `east"} or vector(1) #`}, "cluster")
	require.NoError(t, err)
	assert.Equal(t, `cluster="east\"} or vector(1) #", namespace="blue"`, sel.scopeMatchers(), "the cluster value is escaped")
}

func TestParseSnapshot_Agreeing(t *testing.T) {
	v := agreeingPods(3)
	v = append(v,
		snap(srcOOM, "", 1, "container", "app"),
		snap(srcQuota, "", 4, "resourcequota", "b-quota", "resource", "limits.cpu"),
		snap(srcQuota, "", 8, "resourcequota", "a-quota", "resource", "limits.cpu"),
		snap(srcQuota, "", 16*1024*1024*1024, "resourcequota", "a-quota", "resource", "limits.memory"),
		snap(srcHPA, "", 1, "horizontalpodautoscaler", "keda-hpa-web"),
		snap(srcHPATarget, "", 80, "horizontalpodautoscaler", "keda-hpa-web", "metric_name", "cpu"),
		snap(srcKEDA, "", 1, "horizontalpodautoscaler", "keda-hpa-web", "label_scaledobject_keda_sh_name", "web"),
		snap(srcHPA, "", 1, "horizontalpodautoscaler", "plain"),
		snap(srcVPA, "", 1, "verticalpodautoscaler", "web"),
		snap(srcVPAMode, "", 1, "verticalpodautoscaler", "web", "update_mode", "Initial"),
	)
	st := parseSnapshot(v)

	assert.True(t, st.Exists)
	assert.Equal(t, 3, st.Pods)
	assert.Empty(t, st.Disagreeing)
	require.Contains(t, st.Containers, "app")
	app := st.Containers["app"]
	assert.Equal(t, "250m", app.Requests.CPU.String())
	assert.Equal(t, "512Mi", app.Requests.Memory.String())
	assert.Equal(t, "1Gi", app.Limits.Memory.String())
	assert.Nil(t, app.Limits.CPU)
	assert.Equal(t, []string{"app"}, st.OOMKilled)
	require.NotNil(t, st.Quota)
	assert.Equal(t, "a-quota", st.Quota.Name, "first quota by name, as the Kubernetes path")
	assert.Equal(t, "8", st.Quota.CPULimit.String())
	assert.Equal(t, "16Gi", st.Quota.MemoryLimit.String())
	assert.Equal(t, []HPAInfo{
		{Name: "keda-hpa-web", UtilizationResources: []string{"cpu"}, UtilizationTargets: map[string]int32{"cpu": 80}, ScaledObject: "web"},
		{Name: "plain"},
	}, st.HPAs)
	assert.Equal(t, []VPAInfo{{Name: "web", UpdateMode: "Initial"}}, st.VPAs)
}

func TestParseSnapshot_DisagreeingPods(t *testing.T) {
	set := func(v model.Vector, source, agg, container, res string, value float64) model.Vector {
		for _, s := range v {
			if string(s.Metric[sourceLabel]) == source && string(s.Metric[aggLabel]) == agg &&
				string(s.Metric["container"]) == container && string(s.Metric["resource"]) == res {
				s.Value = model.SampleValue(value)
			}
		}
		return v
	}
	tests := []struct {
		name   string
		vector func() model.Vector
	}{
		{name: "different requests (rollout)", vector: func() model.Vector {
			return set(agreeingPods(3), srcRequests, "min", "app", "cpu", 0.1)
		}},
		{name: "different limits", vector: func() model.Vector {
			return set(agreeingPods(3), srcLimits, "min", "app", "memory", 512*1024*1024)
		}},
		{name: "request set on some pods only", vector: func() model.Vector {
			return set(agreeingPods(3), srcRequests, "count", "app", "memory", 2)
		}},
		{name: "container missing on some pods", vector: func() model.Vector {
			v := agreeingPods(3)
			for _, src := range []string{srcInfo} {
				v = set(v, src, "count", "app", "", 2)
			}
			for _, res := range []string{"cpu", "memory"} {
				v = set(v, srcRequests, "count", "app", res, 2)
			}
			return set(v, srcLimits, "count", "app", "memory", 2)
		}},
		{name: "extra container on some pods (injected sidecar)", vector: func() model.Vector {
			return append(agreeingPods(3), snap(srcInfo, "max", 1, "container", "proxy"), snap(srcInfo, "min", 1, "container", "proxy"),
				snap(srcInfo, "count", 1, "container", "proxy"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := parseSnapshot(tt.vector())
			assert.NotEmpty(t, st.Disagreeing)
		})
	}
	// A sidecar on every pod is not a disagreement.
	all := append(agreeingPods(2), snap(srcInfo, "max", 1, "container", "proxy"), snap(srcInfo, "min", 1, "container", "proxy"),
		snap(srcInfo, "count", 2, "container", "proxy"))
	st := parseSnapshot(all)
	assert.Empty(t, st.Disagreeing)
	assert.Contains(t, st.Containers, "proxy")
}

// snapshotServer answers the snapshot query with the vector for the queried
// namespace and records the queries.
type snapshotServer struct {
	mu      sync.Mutex
	queries []string
	byNS    map[string]model.Vector
}

func (s *snapshotServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.FormValue("query")
	s.mu.Lock()
	s.queries = append(s.queries, q)
	s.mu.Unlock()
	var out []sample
	for ns, v := range s.byNS {
		if !strings.Contains(q, `namespace="`+ns+`"`) {
			continue
		}
		for _, smp := range v {
			labels := map[string]string{}
			for k, val := range smp.Metric {
				labels[string(k)] = string(val)
			}
			out = append(out, sample{labels: labels, value: smp.Value.String()})
		}
	}
	writeVector(w, out...)
}

func TestGetWorkloadState_Namespaces(t *testing.T) {
	blue := agreeingPods(2)
	green := agreeingPods(1)
	for _, s := range green {
		if s.Metric[sourceLabel] == srcRequests && s.Metric["resource"] == "cpu" && s.Metric[aggLabel] != "count" {
			s.Value = 2
		}
	}
	srv := &snapshotServer{byNS: map[string]model.Vector{"blue": blue, "green": green}}
	server := httptest.NewServer(srv)
	defer server.Close()
	c, err := NewClient(Config{URL: server.URL, OwnerJoinOnly: true})
	require.NoError(t, err)
	ctx := context.Background()
	web := Workload{Kind: KindDeployment, Name: "web"}

	// Same workload name in two namespaces: each read sees only its own pods.
	b, err := c.GetWorkloadState(ctx, "blue", web)
	require.NoError(t, err)
	g, err := c.GetWorkloadState(ctx, "green", web)
	require.NoError(t, err)
	assert.Equal(t, 2, b.Pods)
	assert.Equal(t, "250m", b.Containers["app"].Requests.CPU.String())
	assert.Equal(t, 1, g.Pods)
	assert.Equal(t, "2", g.Containers["app"].Requests.CPU.String())

	// A namespace without series: the workload does not exist.
	none, err := c.GetWorkloadState(ctx, "empty", web)
	require.NoError(t, err)
	assert.False(t, none.Exists)
	assert.Zero(t, none.Pods)

	require.Len(t, srv.queries, 3, "one query per read; OwnerJoinOnly skips the owner probe")
}
