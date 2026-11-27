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
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// Aggregations of the scaling signal queries, in the sartor_agg label.
const (
	aggP50         = "p50"
	aggMax         = "max"
	aggMin         = "min"
	aggTotalMedian = "total_median"
)

// UsageStats are usage statistics of one container and resource over the
// analysis window, in cores or bytes. Nil means not reported.
type UsageStats struct {
	// P50 and Max are the median and the maximum over time of the usage of
	// the busiest pod (the same per-step reduction as the P95/P99).
	P50, Max *float64
	// TotalMedian is the median over time of the usage summed over all pods.
	TotalMedian *float64
}

// UsageSignals are the usage statistics of one container.
type UsageSignals struct {
	CPU, Memory UsageStats
}

// ReplicaHistory is the desired replica count over the analysis window. Nil
// fields mean no history (for example a new workload or missing series).
type ReplicaHistory struct {
	Min, Median, Max *float64
}

// ScalingSignals are the usage and replica statistics autoscaler-aware
// recommendations work from.
type ScalingSignals struct {
	Containers map[string]UsageSignals
	Replicas   ReplicaHistory
}

// signalQueries returns the usage queries (CPU, memory) and the replica
// history query of sel over window.
func signalQueries(sel podSelector, window time.Duration) (cpu, mem, replicas string, err error) {
	w := formatDuration(window)
	usage := func(pod string) string {
		busiest := "max by (container) (" + pod + ")"
		total := "sum by (container) (" + pod + ")"
		return strings.Join([]string{
			tag(fmt.Sprintf(`quantile_over_time(0.5, (%s)[%s:5m])`, busiest, w), aggLabel, aggP50),
			tag(fmt.Sprintf(`max_over_time((%s)[%s:5m])`, busiest, w), aggLabel, aggMax),
			tag(fmt.Sprintf(`quantile_over_time(0.5, (%s)[%s:5m])`, total, w), aggLabel, aggTotalMedian),
		}, " or ")
	}
	podCPU, podMem := podUsageExprs(sel)
	r, err := replicasExpr(sel)
	if err != nil {
		return "", "", "", err
	}
	replicas = strings.Join([]string{
		tag(fmt.Sprintf(`min_over_time((%s)[%s:5m])`, r, w), aggLabel, aggMin),
		tag(fmt.Sprintf(`quantile_over_time(0.5, (%s)[%s:5m])`, r, w), aggLabel, aggP50),
		tag(fmt.Sprintf(`max_over_time((%s)[%s:5m])`, r, w), aggLabel, aggMax),
	}, " or ")
	return usage(podCPU), usage(podMem), replicas, nil
}

// GetScalingSignals reads the usage baseline (P50), peak (max) and summed
// usage of every container, and the replica history, over window.
func (c *Client) GetScalingSignals(ctx context.Context, namespace string, workload Workload, window time.Duration) (*ScalingSignals, error) {
	if window <= 0 {
		return nil, fmt.Errorf("window must be positive")
	}
	sel, err := c.newSelector(ctx, namespace, workload)
	if err != nil {
		return nil, err
	}
	cpuQ, memQ, replicasQ, err := signalQueries(sel, window)
	if err != nil {
		return nil, err
	}
	at := time.Now()
	out := &ScalingSignals{Containers: map[string]UsageSignals{}}
	for _, q := range []struct {
		name, query string
		stats       func(*UsageSignals) *UsageStats
	}{
		{"CPU signals", cpuQ, func(u *UsageSignals) *UsageStats { return &u.CPU }},
		{"memory signals", memQ, func(u *UsageSignals) *UsageStats { return &u.Memory }},
	} {
		vector, err := c.instantVector(ctx, q.query, at)
		if err != nil {
			return nil, fmt.Errorf("failed to query %s: %w", q.name, err)
		}
		for _, s := range vector {
			name := string(s.Metric["container"])
			v, ok := signalValue(float64(s.Value))
			if name == "" || !ok {
				continue
			}
			u := out.Containers[name]
			st := q.stats(&u)
			switch string(s.Metric[aggLabel]) {
			case aggP50:
				st.P50 = maxPtr(st.P50, v)
			case aggMax:
				st.Max = maxPtr(st.Max, v)
			case aggTotalMedian:
				st.TotalMedian = maxPtr(st.TotalMedian, v)
			}
			out.Containers[name] = u
		}
	}

	vector, err := c.instantVector(ctx, replicasQ, at)
	if err != nil {
		return nil, fmt.Errorf("failed to query replica history: %w", err)
	}
	for _, s := range vector {
		v, ok := signalValue(float64(s.Value))
		if !ok || v > math.MaxInt32 {
			continue
		}
		switch string(s.Metric[aggLabel]) {
		case aggMin:
			out.Replicas.Min = &v
		case aggP50:
			out.Replicas.Median = &v
		case aggMax:
			out.Replicas.Max = &v
		}
	}
	return out, nil
}

func (c *Client) instantVector(ctx context.Context, query string, at time.Time) (model.Vector, error) {
	result, _, _, err := c.api.Query(ctx, query, at)
	if err != nil {
		return nil, err
	}
	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}
	return vector, nil
}

// signalValue rejects NaN, infinite and negative samples.
func signalValue(v float64) (float64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return v, true
}

// maxPtr keeps the larger value when a backend returns duplicate series.
func maxPtr(cur *float64, v float64) *float64 {
	if cur != nil && *cur >= v {
		return cur
	}
	return &v
}
