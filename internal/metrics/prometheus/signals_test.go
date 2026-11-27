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
	"testing"
	"time"

	"github.com/VictoriaMetrics/metricsql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignalQueries_AreValidAndScoped(t *testing.T) {
	sel, err := newPodSelector("example-namespace", Workload{Kind: KindDeployment, Name: "web", Cluster: "example-cluster"}, "cluster")
	require.NoError(t, err)
	sel.ownerJoin = true
	cpu, mem, replicas, err := signalQueries(sel, 7*24*time.Hour)
	require.NoError(t, err)
	scope := `cluster="example-cluster", namespace="example-namespace"`
	for name, q := range map[string]string{"cpu": cpu, "memory": mem, "replicas": replicas} {
		_, err := metricsql.Parse(q)
		require.NoError(t, err, "%s query must be valid MetricsQL: %s", name, q)
		assert.Equal(t, strings.Count(q, "{"), strings.Count(q, "{"+scope), "%s query: every selector is scoped: %s", name, q)
		assert.Contains(t, q, "[7d:5m]", name)
	}
	for _, agg := range []string{aggP50, aggMax, aggTotalMedian} {
		assert.Contains(t, cpu, `"`+agg+`"`)
		assert.Contains(t, mem, `"`+agg+`"`)
	}
	assert.Contains(t, cpu, "sum by (container)")
	assert.Contains(t, replicas, "min_over_time")
	assert.Contains(t, replicas, `deployment="web"`)
}

func agg(a string, v string, kv ...string) sample {
	labels := map[string]string{aggLabel: a}
	for i := 0; i+1 < len(kv); i += 2 {
		labels[kv[i]] = kv[i+1]
	}
	return sample{labels: labels, value: v}
}

func TestGetScalingSignals(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.FormValue("query")
		queries = append(queries, q)
		switch {
		case strings.Contains(q, "working_set"):
			writeVector(w,
				agg(aggP50, "104857600", "container", "app"),
				agg(aggMax, "209715200", "container", "app"),
				agg(aggTotalMedian, "314572800", "container", "app"),
				agg(aggP50, "1", "container", ""), // no container: ignored
			)
		case strings.Contains(q, "cpu_usage"):
			writeVector(w,
				agg(aggP50, "0.1", "container", "app"),
				agg(aggP50, "0.2", "container", "app"), // duplicate series: the larger wins
				agg(aggMax, "NaN", "container", "app"),
				agg(aggTotalMedian, "-1", "container", "app"),
				agg(aggMax, "+Inf", "container", "sidecar"),
				agg("other", "5", "container", "sidecar"),
			)
		default:
			writeVector(w,
				agg(aggMin, "2"),
				agg(aggP50, "3"),
				agg(aggMax, "1e12"), // absurd: ignored
			)
		}
	}))
	defer server.Close()
	c, err := NewClient(Config{URL: server.URL, OwnerJoinOnly: true})
	require.NoError(t, err)

	got, err := c.GetScalingSignals(context.Background(), "example-namespace", Workload{Kind: KindDeployment, Name: "web"}, 24*time.Hour)
	require.NoError(t, err)
	require.Len(t, queries, 3)

	app := got.Containers["app"]
	require.NotNil(t, app.CPU.P50)
	assert.InDelta(t, 0.2, *app.CPU.P50, 1e-9)
	assert.Nil(t, app.CPU.Max, "NaN is dropped")
	assert.Nil(t, app.CPU.TotalMedian, "negative is dropped")
	assert.InDelta(t, 100*1024*1024, *app.Memory.P50, 1)
	assert.InDelta(t, 200*1024*1024, *app.Memory.Max, 1)
	assert.InDelta(t, 300*1024*1024, *app.Memory.TotalMedian, 1)
	assert.Equal(t, UsageSignals{}, got.Containers["sidecar"], "Inf and unknown aggregations are dropped")
	assert.NotContains(t, got.Containers, "")

	assert.InDelta(t, 2, *got.Replicas.Min, 0)
	assert.InDelta(t, 3, *got.Replicas.Median, 0)
	assert.Nil(t, got.Replicas.Max)
}

func TestGetScalingSignals_Errors(t *testing.T) {
	c, err := NewClient(Config{URL: "http://127.0.0.1:1", OwnerJoinOnly: true})
	require.NoError(t, err)
	_, err = c.GetScalingSignals(context.Background(), "example-namespace", Workload{Kind: KindDeployment, Name: "web"}, 0)
	assert.ErrorContains(t, err, "window must be positive")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()
	c, err = NewClient(Config{URL: server.URL, OwnerJoinOnly: true})
	require.NoError(t, err)
	_, err = c.GetScalingSignals(context.Background(), "example-namespace", Workload{Kind: KindDeployment, Name: "web"}, time.Hour)
	assert.ErrorContains(t, err, "failed to query CPU signals")
}

func TestParseSnapshot_AutoscalerDetails(t *testing.T) {
	v := agreeingPods(2)
	v = append(v,
		snap(srcHPA, "", 1, "horizontalpodautoscaler", "web"),
		snap(srcHPATarget, "", 70, "horizontalpodautoscaler", "web", "metric_name", "cpu"),
		snap(srcHPATarget, "", 80, "horizontalpodautoscaler", "web", "metric_name", "memory"),
		snap(srcHPAMetric, "", 1, "horizontalpodautoscaler", "web", "metric_name", "cpu", "metric_target_type", "utilization"),
		snap(srcHPAMetric, "", 1, "horizontalpodautoscaler", "web", "metric_name", "queue_depth", "metric_target_type", "average"),
		snap(srcHPAMetric, "", 1, "horizontalpodautoscaler", "web", "metric_name", "memory", "metric_target_type", "average"),
		snap(srcHPAMin, "", 2, "horizontalpodautoscaler", "web"),
		snap(srcHPAMax, "", 10, "horizontalpodautoscaler", "web"),
		snap(srcVPA, "", 1, "verticalpodautoscaler", "web"),
		snap(srcVPABound, "", 0.1, "verticalpodautoscaler", "web", "container", "app", boundLabel, "minallowed_cpu"),
		snap(srcVPABound, "", 2, "verticalpodautoscaler", "web", "container", "app", boundLabel, "maxallowed_cpu"),
		snap(srcVPABound, "", 1<<30, "verticalpodautoscaler", "web", "container", "app", boundLabel, "maxallowed_memory"),
		snap(srcVPABound, "", -1, "verticalpodautoscaler", "web", "container", "app", boundLabel, "minallowed_memory"),
		snap(srcVPABound, "", 1e300, "verticalpodautoscaler", "web", "container", "*", boundLabel, "maxallowed_cpu"),
		snap(srcVPABound, "", 1, "verticalpodautoscaler", "web", "container", "app", boundLabel, "bogus"),
		snap(srcVPABound, "", 1, "verticalpodautoscaler", "web", "container", "app", boundLabel, "target_cpu"),
	)
	st := parseSnapshot(v)
	require.Len(t, st.HPAs, 1)
	h := st.HPAs[0]
	assert.Equal(t, map[string]int32{"cpu": 70, "memory": 80}, h.UtilizationTargets)
	assert.Equal(t, []string{"memory average", "queue_depth average"}, h.OtherMetrics)
	assert.Equal(t, int32(2), *h.MinReplicas)
	assert.Equal(t, int32(10), *h.MaxReplicas)

	require.Len(t, st.VPAs, 1)
	vpa := st.VPAs[0]
	assert.Equal(t, "100m", vpa.MinAllowed["app"].CPU.String())
	assert.Nil(t, vpa.MinAllowed["app"].Memory, "negative bound dropped")
	assert.Equal(t, "2", vpa.MaxAllowed["app"].CPU.String())
	assert.Equal(t, "1Gi", vpa.MaxAllowed["app"].Memory.String())
	assert.NotContains(t, vpa.MaxAllowed, "*", "absurd bound dropped")
}
