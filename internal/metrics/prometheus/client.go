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
	"crypto/tls"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Config holds the configuration for the Prometheus client.
type Config struct {
	// URL is the Prometheus server URL.
	URL string
	// Username for basic auth (optional).
	Username string
	// Password for basic auth (optional).
	Password string
	// Token for bearer auth (optional).
	Token string
	// InsecureSkipVerify skips TLS verification.
	InsecureSkipVerify bool
}

// Client provides methods to query Prometheus for container metrics.
type Client struct {
	api    promv1.API
	config Config
}

// ContainerMetrics holds the metrics for a container.
type ContainerMetrics struct {
	// ContainerName is the name of the container.
	ContainerName string
	// P95CPU is the P95 CPU usage.
	P95CPU *resource.Quantity
	// P99CPU is the P99 CPU usage.
	P99CPU *resource.Quantity
	// P95Memory is the P95 memory usage.
	P95Memory *resource.Quantity
	// P99Memory is the P99 memory usage.
	P99Memory *resource.Quantity
}

// basicAuthRoundTripper implements http.RoundTripper with basic auth.
type basicAuthRoundTripper struct {
	username string
	password string
	rt       http.RoundTripper
}

func (b *basicAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.SetBasicAuth(b.username, b.password)
	return b.rt.RoundTrip(req)
}

// bearerAuthRoundTripper implements http.RoundTripper with bearer token.
type bearerAuthRoundTripper struct {
	token string
	rt    http.RoundTripper
}

func (b *bearerAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.rt.RoundTrip(req)
}

// queryParamsRoundTripper adds the query parameters of the configured URL
// (e.g. VictoriaMetrics' ?extra_label=cluster=staging) to every request.
// They are sent once in the URL, also for POST requests whose form body
// carries the PromQL parameters.
type queryParamsRoundTripper struct {
	params url.Values
	rt     http.RoundTripper
}

func (q *queryParamsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	values := req.URL.Query()
	for key, vals := range q.params {
		for _, v := range vals {
			if !slices.Contains(values[key], v) {
				values.Add(key, v)
			}
		}
	}
	req.URL.RawQuery = values.Encode()
	return q.rt.RoundTrip(req)
}

// NewClient creates a new Prometheus-compatible client.
//
// The URL may carry a path prefix (e.g. VictoriaMetrics vmselect
// "/select/0/prometheus") and a query string; both are applied to every API
// request. Basic auth is used when Username is set, otherwise a bearer token
// when Token is set.
func NewClient(cfg Config) (*Client, error) {
	endpoint, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid Prometheus URL: %w", err)
	}
	params := endpoint.Query()
	endpoint.RawQuery = ""
	endpoint.Fragment = ""

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		},
	}

	var rt http.RoundTripper = transport
	if cfg.Username != "" {
		rt = &basicAuthRoundTripper{
			username: cfg.Username,
			password: cfg.Password,
			rt:       rt,
		}
	} else if cfg.Token != "" {
		rt = &bearerAuthRoundTripper{
			token: cfg.Token,
			rt:    rt,
		}
	}
	if len(params) > 0 {
		rt = &queryParamsRoundTripper{params: params, rt: rt}
	}

	client, err := api.NewClient(api.Config{
		Address:      endpoint.String(),
		RoundTripper: rt,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Prometheus client: %w", err)
	}

	return &Client{
		api:    promv1.NewAPI(client),
		config: cfg,
	}, nil
}

// CheckConnection verifies the server answers PromQL queries by evaluating
// vector(1). Unlike /api/v1/status/config this works on VictoriaMetrics and
// other Prometheus-compatible backends, and it exercises the same path,
// query parameters and credentials as the real queries.
func (c *Client) CheckConnection(ctx context.Context) error {
	result, _, err := c.api.Query(ctx, "vector(1)", time.Now())
	if err != nil {
		return fmt.Errorf("failed to connect to Prometheus: %w", err)
	}
	vector, ok := result.(model.Vector)
	if !ok || len(vector) != 1 || float64(vector[0].Value) != 1 {
		return fmt.Errorf("failed to connect to Prometheus: unexpected response to vector(1): %v", result)
	}
	return nil
}

// newSelector builds the pod selector for a workload, preferring the
// kube-state-metrics owner join when ownership series are available.
func (c *Client) newSelector(ctx context.Context, namespace string, w Workload) (podSelector, error) {
	sel, err := newPodSelector(namespace, w)
	if err != nil {
		return podSelector{}, err
	}
	sel.ownerJoin = c.ownerSeriesAvailable(ctx, sel)
	return sel, nil
}

// ownerSeriesAvailable reports whether kube-state-metrics ownership series
// exist for the workload. Any failure falls back to pod-name matching.
func (c *Client) ownerSeriesAvailable(ctx context.Context, sel podSelector) bool {
	result, _, err := c.api.Query(ctx, sel.ownerProbeQuery(), time.Now())
	if err != nil {
		return false
	}
	vector, ok := result.(model.Vector)
	return ok && len(vector) > 0 && vector[0].Value > 0
}

// GetContainerMetrics queries per-container P95/P99 CPU and memory usage for
// all pods of the workload. At each step the usage of the workload's pods is
// reduced with max by container before taking the quantile, so the result is
// one value per container regardless of the number of pods or duplicate
// cAdvisor series.
func (c *Client) GetContainerMetrics(ctx context.Context, namespace string, workload Workload, window time.Duration) ([]ContainerMetrics, error) {
	sel, err := c.newSelector(ctx, namespace, workload)
	if err != nil {
		return nil, err
	}

	at := time.Now()
	metrics := make(map[string]*ContainerMetrics)
	for _, q := range containerMetricQueries(sel, window) {
		if err := c.queryAndExtract(ctx, q.query, at, metrics, q.set); err != nil {
			return nil, fmt.Errorf("failed to query %s: %w", q.name, err)
		}
	}

	result := make([]ContainerMetrics, 0, len(metrics))
	for _, m := range metrics {
		result = append(result, *m)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ContainerName < result[j].ContainerName })

	return result, nil
}

type containerMetricQuery struct {
	name  string
	query string
	set   func(*ContainerMetrics, float64)
}

// containerMetricQueries returns the P95/P99 CPU and memory queries for sel.
func containerMetricQueries(sel podSelector, window time.Duration) []containerMetricQuery {
	w := formatDuration(window)
	cpu := sel.scope(fmt.Sprintf(`rate(container_cpu_usage_seconds_total{%s}[5m])`,
		sel.matchers(`container!=""`, `container!="POD"`)))
	mem := sel.scope(fmt.Sprintf(`container_memory_working_set_bytes{%s}`,
		sel.matchers(`container!=""`, `container!="POD"`)))
	quantile := func(q, expr string) string {
		return fmt.Sprintf(`quantile_over_time(%s, (max by (container) (%s))[%s:5m])`, q, expr, w)
	}
	setCPU := func(set func(*ContainerMetrics, *resource.Quantity)) func(*ContainerMetrics, float64) {
		return func(m *ContainerMetrics, v float64) {
			set(m, resource.NewMilliQuantity(int64(v*1000), resource.DecimalSI))
		}
	}
	setMem := func(set func(*ContainerMetrics, *resource.Quantity)) func(*ContainerMetrics, float64) {
		return func(m *ContainerMetrics, v float64) { set(m, resource.NewQuantity(int64(v), resource.BinarySI)) }
	}
	return []containerMetricQuery{
		{"P95 CPU", quantile("0.95", cpu), setCPU(func(m *ContainerMetrics, q *resource.Quantity) { m.P95CPU = maxQuantity(m.P95CPU, q) })},
		{"P99 CPU", quantile("0.99", cpu), setCPU(func(m *ContainerMetrics, q *resource.Quantity) { m.P99CPU = maxQuantity(m.P99CPU, q) })},
		{"P95 memory", quantile("0.95", mem), setMem(func(m *ContainerMetrics, q *resource.Quantity) { m.P95Memory = maxQuantity(m.P95Memory, q) })},
		{"P99 memory", quantile("0.99", mem), setMem(func(m *ContainerMetrics, q *resource.Quantity) { m.P99Memory = maxQuantity(m.P99Memory, q) })},
	}
}

// maxQuantity returns the larger of a and b, treating nil as absent.
func maxQuantity(a, b *resource.Quantity) *resource.Quantity {
	if a == nil || (b != nil && b.Cmp(*a) > 0) {
		return b
	}
	return a
}

// queryAndExtract executes an instant query and passes each per-container
// sample to setter. Setters must aggregate, because a backend may still
// return several series for one container.
func (c *Client) queryAndExtract(
	ctx context.Context,
	query string,
	at time.Time,
	metrics map[string]*ContainerMetrics,
	setter func(*ContainerMetrics, float64),
) error {
	result, warnings, err := c.api.Query(ctx, query, at)
	if err != nil {
		return err
	}

	if len(warnings) > 0 {
		// Log warnings but continue
		fmt.Printf("Prometheus query warnings: %v\n", warnings)
	}

	vector, ok := result.(model.Vector)
	if !ok {
		return fmt.Errorf("unexpected result type: %T", result)
	}

	for _, sample := range vector {
		containerName := string(sample.Metric["container"])
		if containerName == "" {
			continue
		}
		v := float64(sample.Value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}

		if _, exists := metrics[containerName]; !exists {
			metrics[containerName] = &ContainerMetrics{
				ContainerName: containerName,
			}
		}

		setter(metrics[containerName], v)
	}

	return nil
}

// TimeSeriesPoint represents a single data point in a time series.
type TimeSeriesPoint struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

// Resource types accepted by GetTimeSeriesMetrics.
const (
	ResourceTypeCPU    = "cpu"
	ResourceTypeMemory = "memory"
)

// TimeSeriesMetrics holds time series data for a container.
type TimeSeriesMetrics struct {
	ContainerName string            `json:"containerName"`
	ResourceType  string            `json:"resourceType"` // "cpu" or "memory"
	Data          []TimeSeriesPoint `json:"data"`
}

// GetTimeSeriesMetrics queries a container's usage over time across all pods
// of the workload, reduced with max per step.
func (c *Client) GetTimeSeriesMetrics(ctx context.Context, namespace string, workload Workload, containerName, resourceType string, window time.Duration) ([]TimeSeriesPoint, error) {
	if resourceType != ResourceTypeCPU && resourceType != ResourceTypeMemory {
		return nil, fmt.Errorf("unsupported resource type: %s", resourceType)
	}
	if err := ValidateQueryTarget(namespace, workload, containerName); err != nil {
		return nil, err
	}
	sel, err := c.newSelector(ctx, namespace, workload)
	if err != nil {
		return nil, err
	}

	endTime := time.Now()
	startTime := endTime.Add(-window)

	// Use smaller step for better granularity
	step := window / 200 // ~200 data points
	if step < 15*time.Second {
		step = 15 * time.Second
	}

	timeRange := promv1.Range{
		Start: startTime,
		End:   endTime,
		Step:  step,
	}

	containerMatch := "container=" + promString(containerName)
	var expr string
	if resourceType == ResourceTypeCPU {
		expr = fmt.Sprintf(`rate(container_cpu_usage_seconds_total{%s}[1m])`, sel.matchers(containerMatch))
	} else {
		expr = fmt.Sprintf(`container_memory_working_set_bytes{%s}`, sel.matchers(containerMatch))
	}
	query := fmt.Sprintf(`max(%s)`, sel.scope(expr))

	result, warnings, err := c.api.QueryRange(ctx, query, timeRange)
	if err != nil {
		return nil, fmt.Errorf("failed to query Prometheus: %w", err)
	}

	if len(warnings) > 0 {
		fmt.Printf("Prometheus query warnings: %v\n", warnings)
	}

	matrix, ok := result.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	var points []TimeSeriesPoint
	for _, series := range matrix {
		for _, sample := range series.Values {
			var value float64
			if resourceType == ResourceTypeCPU {
				// Convert CPU seconds to cores (already in seconds)
				value = float64(sample.Value)
			} else {
				// Memory in bytes, convert to MiB
				value = float64(sample.Value) / (1024 * 1024)
			}
			points = append(points, TimeSeriesPoint{
				Timestamp: sample.Timestamp.Unix(),
				Value:     value,
			})
		}
	}

	return points, nil
}

// formatDuration formats a duration for PromQL using the largest unit that
// represents it exactly (36h stays "36h" rather than being truncated to "1d").
func formatDuration(d time.Duration) string {
	seconds := int64(d / time.Second)
	switch {
	case seconds >= 86400 && seconds%86400 == 0:
		return fmt.Sprintf("%dd", seconds/86400)
	case seconds >= 3600 && seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	case seconds >= 60 && seconds%60 == 0:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}
