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
	"net/http"
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
	req.SetBasicAuth(b.username, b.password)
	return b.rt.RoundTrip(req)
}

// bearerAuthRoundTripper implements http.RoundTripper with bearer token.
type bearerAuthRoundTripper struct {
	token string
	rt    http.RoundTripper
}

func (b *bearerAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.rt.RoundTrip(req)
}

// NewClient creates a new Prometheus client.
func NewClient(cfg Config) (*Client, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		},
	}

	var rt http.RoundTripper = transport

	// Add authentication if configured
	if cfg.Username != "" && cfg.Password != "" {
		rt = &basicAuthRoundTripper{
			username: cfg.Username,
			password: cfg.Password,
			rt:       transport,
		}
	} else if cfg.Token != "" {
		rt = &bearerAuthRoundTripper{
			token: cfg.Token,
			rt:    transport,
		}
	}

	client, err := api.NewClient(api.Config{
		Address:      cfg.URL,
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

// CheckConnection verifies the Prometheus server is reachable.
func (c *Client) CheckConnection(ctx context.Context) error {
	_, err := c.api.Config(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to Prometheus: %w", err)
	}
	return nil
}

// GetContainerMetrics queries Prometheus for container resource metrics.
func (c *Client) GetContainerMetrics(ctx context.Context, namespace, podNamePrefix string, window time.Duration) ([]ContainerMetrics, error) {
	endTime := time.Now()
	startTime := endTime.Add(-window)
	timeRange := promv1.Range{
		Start: startTime,
		End:   endTime,
		Step:  time.Minute * 5, // 5-minute steps
	}

	metrics := make(map[string]*ContainerMetrics)

	// Query P95 CPU usage
	p95CPUQuery := fmt.Sprintf(
		`quantile_over_time(0.95, rate(container_cpu_usage_seconds_total{namespace="%s", pod=~"%s.*", container!="", container!="POD"}[5m])[%s:5m])`,
		namespace, podNamePrefix, formatDuration(window),
	)
	if err := c.queryAndExtract(ctx, p95CPUQuery, timeRange, metrics, func(m *ContainerMetrics, v float64) {
		q := resource.NewMilliQuantity(int64(v*1000), resource.DecimalSI)
		m.P95CPU = q
	}); err != nil {
		return nil, fmt.Errorf("failed to query P95 CPU: %w", err)
	}

	// Query P99 CPU usage
	p99CPUQuery := fmt.Sprintf(
		`quantile_over_time(0.99, rate(container_cpu_usage_seconds_total{namespace="%s", pod=~"%s.*", container!="", container!="POD"}[5m])[%s:5m])`,
		namespace, podNamePrefix, formatDuration(window),
	)
	if err := c.queryAndExtract(ctx, p99CPUQuery, timeRange, metrics, func(m *ContainerMetrics, v float64) {
		q := resource.NewMilliQuantity(int64(v*1000), resource.DecimalSI)
		m.P99CPU = q
	}); err != nil {
		return nil, fmt.Errorf("failed to query P99 CPU: %w", err)
	}

	// Query P95 Memory usage
	p95MemQuery := fmt.Sprintf(
		`quantile_over_time(0.95, container_memory_working_set_bytes{namespace="%s", pod=~"%s.*", container!="", container!="POD"}[%s:5m])`,
		namespace, podNamePrefix, formatDuration(window),
	)
	if err := c.queryAndExtract(ctx, p95MemQuery, timeRange, metrics, func(m *ContainerMetrics, v float64) {
		q := resource.NewQuantity(int64(v), resource.BinarySI)
		m.P95Memory = q
	}); err != nil {
		return nil, fmt.Errorf("failed to query P95 memory: %w", err)
	}

	// Query P99 Memory usage
	p99MemQuery := fmt.Sprintf(
		`quantile_over_time(0.99, container_memory_working_set_bytes{namespace="%s", pod=~"%s.*", container!="", container!="POD"}[%s:5m])`,
		namespace, podNamePrefix, formatDuration(window),
	)
	if err := c.queryAndExtract(ctx, p99MemQuery, timeRange, metrics, func(m *ContainerMetrics, v float64) {
		q := resource.NewQuantity(int64(v), resource.BinarySI)
		m.P99Memory = q
	}); err != nil {
		return nil, fmt.Errorf("failed to query P99 memory: %w", err)
	}

	// Convert map to slice
	result := make([]ContainerMetrics, 0, len(metrics))
	for _, m := range metrics {
		result = append(result, *m)
	}

	return result, nil
}

// queryAndExtract executes a Prometheus query and extracts values into ContainerMetrics.
func (c *Client) queryAndExtract(
	ctx context.Context,
	query string,
	timeRange promv1.Range,
	metrics map[string]*ContainerMetrics,
	setter func(*ContainerMetrics, float64),
) error {
	result, warnings, err := c.api.Query(ctx, query, timeRange.End)
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

		if _, exists := metrics[containerName]; !exists {
			metrics[containerName] = &ContainerMetrics{
				ContainerName: containerName,
			}
		}

		setter(metrics[containerName], float64(sample.Value))
	}

	return nil
}

// TimeSeriesPoint represents a single data point in a time series.
type TimeSeriesPoint struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

// TimeSeriesMetrics holds time series data for a container.
type TimeSeriesMetrics struct {
	ContainerName string            `json:"containerName"`
	ResourceType  string            `json:"resourceType"` // "cpu" or "memory"
	Data          []TimeSeriesPoint `json:"data"`
}

// GetTimeSeriesMetrics queries Prometheus for time series data.
func (c *Client) GetTimeSeriesMetrics(ctx context.Context, namespace, podNamePrefix, containerName, resourceType string, window time.Duration) ([]TimeSeriesPoint, error) {
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

	var query string
	switch resourceType {
	case "cpu":
		query = fmt.Sprintf(
			`rate(container_cpu_usage_seconds_total{namespace="%s", pod=~"%s.*", container="%s"}[1m])`,
			namespace, podNamePrefix, containerName,
		)
	case "memory":
		query = fmt.Sprintf(
			`container_memory_working_set_bytes{namespace="%s", pod=~"%s.*", container="%s"}`,
			namespace, podNamePrefix, containerName,
		)
	default:
		return nil, fmt.Errorf("unsupported resource type: %s", resourceType)
	}

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
			if resourceType == "cpu" {
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

// formatDuration formats a duration for Prometheus queries.
func formatDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := int(d.Minutes())
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours := int(d.Hours())
	if hours < 24 {
		return fmt.Sprintf("%dh", hours)
	}
	days := hours / 24
	return fmt.Sprintf("%dd", days)
}
