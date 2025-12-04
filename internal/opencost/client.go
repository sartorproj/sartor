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

// Package opencost provides a client for the OpenCost API.
package opencost

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config holds configuration for the OpenCost client.
type Config struct {
	// URL is the base URL of the OpenCost API (e.g., "http://opencost:9003")
	URL string
	// InsecureSkipVerify skips TLS certificate verification
	InsecureSkipVerify bool
	// Timeout is the HTTP client timeout
	Timeout time.Duration
}

// Client is an OpenCost API client.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a new OpenCost client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("opencost URL is required")
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		},
	}

	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   timeout,
		},
		baseURL: cfg.URL,
	}, nil
}

// AllocationResponse represents the response from the OpenCost allocation API.
type AllocationResponse struct {
	Code    int                           `json:"code"`
	Status  string                        `json:"status"`
	Data    []map[string]*AllocationEntry `json:"data"`
	Message string                        `json:"message,omitempty"`
}

// AllocationEntry represents a single allocation entry from OpenCost.
type AllocationEntry struct {
	Name       string               `json:"name"`
	Properties AllocationProperties `json:"properties"`
	Window     Window               `json:"window"`
	Start      string               `json:"start"`
	End        string               `json:"end"`
	Minutes    float64              `json:"minutes"`

	// CPU
	CPUCores              float64 `json:"cpuCores"`
	CPUCoreRequestAverage float64 `json:"cpuCoreRequestAverage"`
	CPUCoreUsageAverage   float64 `json:"cpuCoreUsageAverage"`
	CPUCoreHours          float64 `json:"cpuCoreHours"`
	CPUCost               float64 `json:"cpuCost"`
	CPUCostAdjustment     float64 `json:"cpuCostAdjustment"`
	CPUEfficiency         float64 `json:"cpuEfficiency"`

	// GPU
	GPUCount          float64 `json:"gpuCount"`
	GPUHours          float64 `json:"gpuHours"`
	GPUCost           float64 `json:"gpuCost"`
	GPUCostAdjustment float64 `json:"gpuCostAdjustment"`

	// Memory (RAM)
	RAMBytes              float64 `json:"ramBytes"`
	RAMByteRequestAverage float64 `json:"ramByteRequestAverage"`
	RAMByteUsageAverage   float64 `json:"ramByteUsageAverage"`
	RAMByteHours          float64 `json:"ramByteHours"`
	RAMCost               float64 `json:"ramCost"`
	RAMCostAdjustment     float64 `json:"ramCostAdjustment"`
	RAMEfficiency         float64 `json:"ramEfficiency"`

	// Network
	NetworkTransferBytes  float64 `json:"networkTransferBytes"`
	NetworkReceiveBytes   float64 `json:"networkReceiveBytes"`
	NetworkCost           float64 `json:"networkCost"`
	NetworkCostAdjustment float64 `json:"networkCostAdjustment"`

	// Persistent Volume
	PVBytes          float64     `json:"pvBytes"`
	PVByteHours      float64     `json:"pvByteHours"`
	PVCost           float64     `json:"pvCost"`
	PVCostAdjustment float64     `json:"pvCostAdjustment"`
	PVs              interface{} `json:"pvs"`

	// Load Balancer
	LoadBalancerCost           float64 `json:"loadBalancerCost"`
	LoadBalancerCostAdjustment float64 `json:"loadBalancerCostAdjustment"`

	// Totals
	SharedCost      float64     `json:"sharedCost"`
	ExternalCost    float64     `json:"externalCost"`
	TotalCost       float64     `json:"totalCost"`
	TotalEfficiency float64     `json:"totalEfficiency"`
	RawAllocation   interface{} `json:"rawAllocationOnly"`
}

// AllocationProperties contains metadata about the allocation.
type AllocationProperties struct {
	Cluster        string `json:"cluster,omitempty"`
	Node           string `json:"node,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
	Controller     string `json:"controller,omitempty"`
	ControllerKind string `json:"controllerKind,omitempty"`
	Service        string `json:"service,omitempty"`
	Pod            string `json:"pod,omitempty"`
	Container      string `json:"container,omitempty"`
	ProviderID     string `json:"providerID,omitempty"`
}

// Window represents a time window.
type Window struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// AllocationRequest represents parameters for querying allocations.
type AllocationRequest struct {
	// Window is the duration of time to query. Examples: "today", "yesterday", "week", "month", "lastweek", "lastmonth", "7d", "30m", "12h"
	Window string
	// Aggregate is the field to aggregate by. Examples: "cluster", "node", "namespace", "controller", "controllerKind", "service", "pod", "container"
	Aggregate string
	// Step is the duration of a single allocation set (optional)
	Step string
	// Resolution is the Prometheus query resolution (optional, default "1m")
	Resolution string
}

// GetAllocations queries the OpenCost allocation API.
func (c *Client) GetAllocations(ctx context.Context, req AllocationRequest) (*AllocationResponse, error) {
	if req.Window == "" {
		req.Window = "7d"
	}
	if req.Aggregate == "" {
		req.Aggregate = "namespace"
	}

	// Build URL
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	u.Path = "/allocation"

	// Add query parameters
	q := u.Query()
	q.Set("window", req.Window)
	q.Set("aggregate", req.Aggregate)
	if req.Step != "" {
		q.Set("step", req.Step)
	}
	if req.Resolution != "" {
		q.Set("resolution", req.Resolution)
	}
	u.RawQuery = q.Encode()

	// Create request
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")

	// Execute request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Parse response
	var allocResp AllocationResponse
	if err := json.NewDecoder(resp.Body).Decode(&allocResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &allocResp, nil
}

// HealthCheck checks if OpenCost is reachable.
func (c *Client) HealthCheck(ctx context.Context) error {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}
	u.Path = "/allocation"
	q := u.Query()
	q.Set("window", "1h")
	q.Set("aggregate", "cluster")
	u.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to connect to OpenCost: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OpenCost returned status %d", resp.StatusCode)
	}

	return nil
}

// CostSummary provides aggregated cost information.
type CostSummary struct {
	TotalCost        float64            `json:"totalCost"`
	CPUCost          float64            `json:"cpuCost"`
	RAMCost          float64            `json:"ramCost"`
	GPUCost          float64            `json:"gpuCost"`
	PVCost           float64            `json:"pvCost"`
	NetworkCost      float64            `json:"networkCost"`
	LoadBalancerCost float64            `json:"loadBalancerCost"`
	SharedCost       float64            `json:"sharedCost"`
	Efficiency       float64            `json:"efficiency"`
	CPUEfficiency    float64            `json:"cpuEfficiency"`
	RAMEfficiency    float64            `json:"ramEfficiency"`
	ByNamespace      map[string]float64 `json:"byNamespace,omitempty"`
}

// GetCostSummary returns a summary of costs for the given window.
func (c *Client) GetCostSummary(ctx context.Context, window string) (*CostSummary, error) {
	resp, err := c.GetAllocations(ctx, AllocationRequest{
		Window:    window,
		Aggregate: "namespace",
	})
	if err != nil {
		return nil, err
	}

	summary := &CostSummary{
		ByNamespace: make(map[string]float64),
	}

	// Aggregate data
	var totalEfficiency, totalCPUEff, totalRAMEff float64
	var count int

	for _, dataSet := range resp.Data {
		for name, entry := range dataSet {
			if entry == nil {
				continue
			}
			summary.TotalCost += entry.TotalCost
			summary.CPUCost += entry.CPUCost
			summary.RAMCost += entry.RAMCost
			summary.GPUCost += entry.GPUCost
			summary.PVCost += entry.PVCost
			summary.NetworkCost += entry.NetworkCost
			summary.LoadBalancerCost += entry.LoadBalancerCost
			summary.SharedCost += entry.SharedCost
			summary.ByNamespace[name] = entry.TotalCost

			totalEfficiency += entry.TotalEfficiency
			totalCPUEff += entry.CPUEfficiency
			totalRAMEff += entry.RAMEfficiency
			count++
		}
	}

	if count > 0 {
		summary.Efficiency = totalEfficiency / float64(count)
		summary.CPUEfficiency = totalCPUEff / float64(count)
		summary.RAMEfficiency = totalRAMEff / float64(count)
	}

	return summary, nil
}

// MetricsResponse represents parsed Prometheus metrics from OpenCost.
// Based on OpenCost metrics documentation: https://opencost.io/docs/integrations/metrics
type MetricsResponse struct {
	// Resource Allocation Metrics
	ContainerCPUAllocations    []ContainerAllocation `json:"containerCpuAllocations"`
	ContainerMemoryAllocations []ContainerAllocation `json:"containerMemoryAllocations"`
	ContainerGPUAllocations    []ContainerAllocation `json:"containerGpuAllocations"`
	PodPVCAllocations          []PVCAllocation       `json:"podPvcAllocations"`

	// Node Cost Metrics
	NodeCPUHourlyCosts   []NodeCost `json:"nodeCpuHourlyCosts"`
	NodeRAMHourlyCosts   []NodeCost `json:"nodeRamHourlyCosts"`
	NodeGPUHourlyCosts   []NodeCost `json:"nodeGpuHourlyCosts"`
	NodeTotalHourlyCosts []NodeCost `json:"nodeTotalHourlyCosts"`
	NodeGPUCounts        []NodeGPU  `json:"nodeGpuCounts"`

	// Storage Cost Metrics
	PVHourlyCosts     []PVCost `json:"pvHourlyCosts"`
	LoadBalancerCosts []LBCost `json:"loadBalancerCosts"`

	// Network Cost Metrics
	NetworkZoneEgressCosts     []NetworkCost `json:"networkZoneEgressCosts"`
	NetworkRegionEgressCosts   []NetworkCost `json:"networkRegionEgressCosts"`
	NetworkInternetEgressCosts []NetworkCost `json:"networkInternetEgressCosts"`

	// Cluster Metrics
	ClusterManagementCost float64      `json:"clusterManagementCost"`
	ClusterInfo           *ClusterInfo `json:"clusterInfo"`

	// Kubernetes State Metrics
	NodeCapacity        *NodeCapacity        `json:"nodeCapacity"`
	NodeAllocatable     *NodeAllocatable     `json:"nodeAllocatable"`
	PodResourceRequests []PodResourceRequest `json:"podResourceRequests"`
}

// ContainerAllocation represents container-level resource allocation.
type ContainerAllocation struct {
	Container string  `json:"container"`
	Namespace string  `json:"namespace"`
	Pod       string  `json:"pod"`
	Node      string  `json:"node"`
	Value     float64 `json:"value"`
}

// NodeCapacity represents node capacity information.
type NodeCapacity struct {
	Node           string  `json:"node"`
	CPUCores       float64 `json:"cpuCores"`
	MemoryBytes    float64 `json:"memoryBytes"`
	Pods           int     `json:"pods"`
	EphemeralBytes float64 `json:"ephemeralBytes"`
}

// NodeAllocatable represents node allocatable resources.
type NodeAllocatable struct {
	Node           string  `json:"node"`
	CPUCores       float64 `json:"cpuCores"`
	MemoryBytes    float64 `json:"memoryBytes"`
	Pods           int     `json:"pods"`
	EphemeralBytes float64 `json:"ephemeralBytes"`
}

// PodResourceRequest represents pod resource requests.
type PodResourceRequest struct {
	Container string  `json:"container"`
	Namespace string  `json:"namespace"`
	Pod       string  `json:"pod"`
	Node      string  `json:"node"`
	Resource  string  `json:"resource"`
	Value     float64 `json:"value"`
}

// ClusterInfo represents cluster information from OpenCost.
type ClusterInfo struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	Version        string `json:"version"`
	Region         string `json:"region"`
	ClusterProfile string `json:"clusterProfile"`
}

// NodeCost represents node-level cost metrics.
type NodeCost struct {
	Node       string  `json:"node"`
	Instance   string  `json:"instance"`
	ProviderID string  `json:"providerId"`
	Value      float64 `json:"value"`
}

// NodeGPU represents node GPU count.
type NodeGPU struct {
	Node       string `json:"node"`
	Instance   string `json:"instance"`
	ProviderID string `json:"providerId"`
	Count      int    `json:"count"`
}

// PVCAllocation represents PVC allocation for a pod.
type PVCAllocation struct {
	PersistentVolume string  `json:"persistentVolume"`
	Namespace        string  `json:"namespace"`
	Pod              string  `json:"pod"`
	Bytes            float64 `json:"bytes"`
}

// PVCost represents persistent volume cost.
type PVCost struct {
	PersistentVolume string  `json:"persistentVolume"`
	HourlyCost       float64 `json:"hourlyCost"`
}

// LBCost represents load balancer cost.
type LBCost struct {
	Namespace  string  `json:"namespace"`
	Service    string  `json:"service"`
	HourlyCost float64 `json:"hourlyCost"`
}

// NetworkCost represents network egress cost.
type NetworkCost struct {
	Namespace string  `json:"namespace"`
	Service   string  `json:"service"`
	CostPerGB float64 `json:"costPerGb"`
}

// ResourceAnalytics provides analytics over OpenCost metrics.
type ResourceAnalytics struct {
	// Cluster-level metrics
	TotalCPUCapacity       float64 `json:"totalCpuCapacity"`
	TotalMemoryCapacity    float64 `json:"totalMemoryCapacity"`
	TotalCPUAllocatable    float64 `json:"totalCpuAllocatable"`
	TotalMemoryAllocatable float64 `json:"totalMemoryAllocatable"`

	// Allocation metrics
	TotalCPUAllocated    float64 `json:"totalCpuAllocated"`
	TotalMemoryAllocated float64 `json:"totalMemoryAllocated"`
	TotalGPUAllocated    float64 `json:"totalGpuAllocated"`

	// Utilization percentages
	CPUUtilizationPercent    float64 `json:"cpuUtilizationPercent"`
	MemoryUtilizationPercent float64 `json:"memoryUtilizationPercent"`

	// Breakdown by namespace
	CPUByNamespace    map[string]float64 `json:"cpuByNamespace"`
	MemoryByNamespace map[string]float64 `json:"memoryByNamespace"`

	// Breakdown by pod
	CPUByPod    map[string]float64 `json:"cpuByPod"`
	MemoryByPod map[string]float64 `json:"memoryByPod"`

	// Top consumers
	TopCPUConsumers    []ResourceConsumer `json:"topCpuConsumers"`
	TopMemoryConsumers []ResourceConsumer `json:"topMemoryConsumers"`
}

// ResourceConsumer represents a resource consumer (pod/container).
type ResourceConsumer struct {
	Name      string  `json:"name"`
	Namespace string  `json:"namespace"`
	Pod       string  `json:"pod"`
	Container string  `json:"container"`
	Value     float64 `json:"value"`
	Percent   float64 `json:"percent"`
}

// GetMetrics fetches and parses OpenCost Prometheus metrics.
func (c *Client) GetMetrics(ctx context.Context) (*MetricsResponse, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	u.Path = "/metrics"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return parsePrometheusMetrics(resp.Body)
}

// GetResourceAnalytics provides comprehensive analytics over OpenCost metrics.
func (c *Client) GetResourceAnalytics(ctx context.Context) (*ResourceAnalytics, error) {
	metrics, err := c.GetMetrics(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get metrics: %w", err)
	}

	analytics := &ResourceAnalytics{
		CPUByNamespace:    make(map[string]float64),
		MemoryByNamespace: make(map[string]float64),
		CPUByPod:          make(map[string]float64),
		MemoryByPod:       make(map[string]float64),
	}

	// Aggregate node capacity and allocatable
	if metrics.NodeCapacity != nil {
		analytics.TotalCPUCapacity = metrics.NodeCapacity.CPUCores
		analytics.TotalMemoryCapacity = metrics.NodeCapacity.MemoryBytes
	}
	if metrics.NodeAllocatable != nil {
		analytics.TotalCPUAllocatable = metrics.NodeAllocatable.CPUCores
		analytics.TotalMemoryAllocatable = metrics.NodeAllocatable.MemoryBytes
	}

	// Aggregate CPU allocations
	for _, alloc := range metrics.ContainerCPUAllocations {
		analytics.TotalCPUAllocated += alloc.Value
		analytics.CPUByNamespace[alloc.Namespace] += alloc.Value
		podKey := fmt.Sprintf("%s/%s", alloc.Namespace, alloc.Pod)
		analytics.CPUByPod[podKey] += alloc.Value
	}

	// Aggregate memory allocations
	for _, alloc := range metrics.ContainerMemoryAllocations {
		analytics.TotalMemoryAllocated += alloc.Value
		analytics.MemoryByNamespace[alloc.Namespace] += alloc.Value
		podKey := fmt.Sprintf("%s/%s", alloc.Namespace, alloc.Pod)
		analytics.MemoryByPod[podKey] += alloc.Value
	}

	// Aggregate GPU allocations
	for _, alloc := range metrics.ContainerGPUAllocations {
		analytics.TotalGPUAllocated += alloc.Value
	}

	// Calculate utilization percentages
	if analytics.TotalCPUAllocatable > 0 {
		analytics.CPUUtilizationPercent = (analytics.TotalCPUAllocated / analytics.TotalCPUAllocatable) * 100
	}
	if analytics.TotalMemoryAllocatable > 0 {
		analytics.MemoryUtilizationPercent = (analytics.TotalMemoryAllocated / analytics.TotalMemoryAllocatable) * 100
	}

	// Find top CPU consumers
	analytics.TopCPUConsumers = getTopConsumers(metrics.ContainerCPUAllocations, analytics.TotalCPUAllocated, 10)

	// Find top memory consumers
	analytics.TopMemoryConsumers = getTopConsumers(metrics.ContainerMemoryAllocations, analytics.TotalMemoryAllocated, 10)

	return analytics, nil
}

// NamespaceResourceBreakdown provides detailed resource breakdown for a namespace.
type NamespaceResourceBreakdown struct {
	Namespace     string               `json:"namespace"`
	TotalCPU      float64              `json:"totalCpu"`
	TotalMemory   float64              `json:"totalMemory"`
	TotalGPU      float64              `json:"totalGpu"`
	CPUPercent    float64              `json:"cpuPercent"`
	MemoryPercent float64              `json:"memoryPercent"`
	Containers    []ContainerBreakdown `json:"containers"`
}

// ContainerBreakdown provides resource breakdown for a container.
type ContainerBreakdown struct {
	Container string  `json:"container"`
	Pod       string  `json:"pod"`
	CPU       float64 `json:"cpu"`
	Memory    float64 `json:"memory"`
	GPU       float64 `json:"gpu"`
}

// GetNamespaceBreakdown provides detailed resource breakdown for a specific namespace.
func (c *Client) GetNamespaceBreakdown(ctx context.Context, namespace string) (*NamespaceResourceBreakdown, error) {
	metrics, err := c.GetMetrics(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get metrics: %w", err)
	}

	breakdown := &NamespaceResourceBreakdown{
		Namespace:  namespace,
		Containers: []ContainerBreakdown{},
	}

	// Build container map
	containerMap := make(map[string]*ContainerBreakdown)

	// Aggregate CPU
	var totalClusterCPU float64
	for _, alloc := range metrics.ContainerCPUAllocations {
		totalClusterCPU += alloc.Value
		if alloc.Namespace != namespace {
			continue
		}
		key := fmt.Sprintf("%s/%s", alloc.Pod, alloc.Container)
		if _, exists := containerMap[key]; !exists {
			containerMap[key] = &ContainerBreakdown{
				Container: alloc.Container,
				Pod:       alloc.Pod,
			}
		}
		containerMap[key].CPU = alloc.Value
		breakdown.TotalCPU += alloc.Value
	}

	// Aggregate memory
	var totalClusterMemory float64
	for _, alloc := range metrics.ContainerMemoryAllocations {
		totalClusterMemory += alloc.Value
		if alloc.Namespace != namespace {
			continue
		}
		key := fmt.Sprintf("%s/%s", alloc.Pod, alloc.Container)
		if _, exists := containerMap[key]; !exists {
			containerMap[key] = &ContainerBreakdown{
				Container: alloc.Container,
				Pod:       alloc.Pod,
			}
		}
		containerMap[key].Memory = alloc.Value
		breakdown.TotalMemory += alloc.Value
	}

	// Aggregate GPU
	for _, alloc := range metrics.ContainerGPUAllocations {
		if alloc.Namespace != namespace {
			continue
		}
		key := fmt.Sprintf("%s/%s", alloc.Pod, alloc.Container)
		if _, exists := containerMap[key]; !exists {
			containerMap[key] = &ContainerBreakdown{
				Container: alloc.Container,
				Pod:       alloc.Pod,
			}
		}
		containerMap[key].GPU = alloc.Value
		breakdown.TotalGPU += alloc.Value
	}

	// Calculate percentages
	if totalClusterCPU > 0 {
		breakdown.CPUPercent = (breakdown.TotalCPU / totalClusterCPU) * 100
	}
	if totalClusterMemory > 0 {
		breakdown.MemoryPercent = (breakdown.TotalMemory / totalClusterMemory) * 100
	}

	// Convert map to slice
	for _, cb := range containerMap {
		breakdown.Containers = append(breakdown.Containers, *cb)
	}

	return breakdown, nil
}

// ClusterResourceSummary provides a high-level summary of cluster resources.
type ClusterResourceSummary struct {
	// Capacity
	TotalNodes    int     `json:"totalNodes"`
	TotalCPUCores float64 `json:"totalCpuCores"`
	TotalMemoryGB float64 `json:"totalMemoryGb"`

	// Allocation
	AllocatedCPUCores float64 `json:"allocatedCpuCores"`
	AllocatedMemoryGB float64 `json:"allocatedMemoryGb"`

	// Available
	AvailableCPUCores float64 `json:"availableCpuCores"`
	AvailableMemoryGB float64 `json:"availableMemoryGb"`

	// Utilization
	CPUUtilization    float64 `json:"cpuUtilization"`
	MemoryUtilization float64 `json:"memoryUtilization"`

	// Counts
	TotalPods       int `json:"totalPods"`
	TotalContainers int `json:"totalContainers"`
	TotalNamespaces int `json:"totalNamespaces"`
}

// GetClusterResourceSummary provides a high-level summary of cluster resources.
func (c *Client) GetClusterResourceSummary(ctx context.Context) (*ClusterResourceSummary, error) {
	metrics, err := c.GetMetrics(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get metrics: %w", err)
	}

	summary := &ClusterResourceSummary{
		TotalNodes: 1, // Single node from metrics
	}

	// Set capacity from node metrics
	if metrics.NodeCapacity != nil {
		summary.TotalCPUCores = metrics.NodeCapacity.CPUCores
		summary.TotalMemoryGB = metrics.NodeCapacity.MemoryBytes / (1024 * 1024 * 1024)
	}

	// Calculate allocated resources
	namespaces := make(map[string]bool)
	pods := make(map[string]bool)
	containers := make(map[string]bool)

	for _, alloc := range metrics.ContainerCPUAllocations {
		summary.AllocatedCPUCores += alloc.Value
		namespaces[alloc.Namespace] = true
		pods[fmt.Sprintf("%s/%s", alloc.Namespace, alloc.Pod)] = true
		containers[fmt.Sprintf("%s/%s/%s", alloc.Namespace, alloc.Pod, alloc.Container)] = true
	}

	for _, alloc := range metrics.ContainerMemoryAllocations {
		summary.AllocatedMemoryGB += alloc.Value / (1024 * 1024 * 1024)
	}

	// Calculate available resources
	summary.AvailableCPUCores = summary.TotalCPUCores - summary.AllocatedCPUCores
	summary.AvailableMemoryGB = summary.TotalMemoryGB - summary.AllocatedMemoryGB

	// Calculate utilization
	if summary.TotalCPUCores > 0 {
		summary.CPUUtilization = (summary.AllocatedCPUCores / summary.TotalCPUCores) * 100
	}
	if summary.TotalMemoryGB > 0 {
		summary.MemoryUtilization = (summary.AllocatedMemoryGB / summary.TotalMemoryGB) * 100
	}

	// Set counts
	summary.TotalNamespaces = len(namespaces)
	summary.TotalPods = len(pods)
	summary.TotalContainers = len(containers)

	return summary, nil
}

// CostAnalytics provides cost-focused analytics from OpenCost metrics.
// Based on metrics from https://opencost.io/docs/integrations/metrics
type CostAnalytics struct {
	// Hourly costs
	TotalHourlyCost        float64 `json:"totalHourlyCost"`
	CPUHourlyCost          float64 `json:"cpuHourlyCost"`
	RAMHourlyCost          float64 `json:"ramHourlyCost"`
	GPUHourlyCost          float64 `json:"gpuHourlyCost"`
	StorageHourlyCost      float64 `json:"storageHourlyCost"`
	NetworkHourlyCost      float64 `json:"networkHourlyCost"`
	LoadBalancerHourlyCost float64 `json:"loadBalancerHourlyCost"`
	ClusterManagementCost  float64 `json:"clusterManagementCost"`

	// Projected costs
	DailyCost   float64 `json:"dailyCost"`
	MonthlyCost float64 `json:"monthlyCost"`

	// Cost breakdown by namespace (estimated)
	CostByNamespace map[string]float64 `json:"costByNamespace"`

	// Node costs
	NodeCosts []NodeCostSummary `json:"nodeCosts"`
}

// NodeCostSummary summarizes costs for a single node.
type NodeCostSummary struct {
	Node            string  `json:"node"`
	CPUHourlyCost   float64 `json:"cpuHourlyCost"`
	RAMHourlyCost   float64 `json:"ramHourlyCost"`
	GPUHourlyCost   float64 `json:"gpuHourlyCost"`
	TotalHourlyCost float64 `json:"totalHourlyCost"`
	GPUCount        int     `json:"gpuCount"`
}

// GetCostAnalytics provides cost-focused analytics from OpenCost metrics.
func (c *Client) GetCostAnalytics(ctx context.Context) (*CostAnalytics, error) {
	metrics, err := c.GetMetrics(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get metrics: %w", err)
	}

	analytics := &CostAnalytics{
		CostByNamespace:       make(map[string]float64),
		ClusterManagementCost: metrics.ClusterManagementCost,
	}

	// Aggregate node costs
	nodeMap := make(map[string]*NodeCostSummary)

	for _, nc := range metrics.NodeCPUHourlyCosts {
		if _, exists := nodeMap[nc.Node]; !exists {
			nodeMap[nc.Node] = &NodeCostSummary{Node: nc.Node}
		}
		nodeMap[nc.Node].CPUHourlyCost = nc.Value
		analytics.CPUHourlyCost += nc.Value
	}

	for _, nc := range metrics.NodeRAMHourlyCosts {
		if _, exists := nodeMap[nc.Node]; !exists {
			nodeMap[nc.Node] = &NodeCostSummary{Node: nc.Node}
		}
		nodeMap[nc.Node].RAMHourlyCost = nc.Value
		analytics.RAMHourlyCost += nc.Value
	}

	for _, nc := range metrics.NodeGPUHourlyCosts {
		if _, exists := nodeMap[nc.Node]; !exists {
			nodeMap[nc.Node] = &NodeCostSummary{Node: nc.Node}
		}
		nodeMap[nc.Node].GPUHourlyCost = nc.Value
		analytics.GPUHourlyCost += nc.Value
	}

	for _, nc := range metrics.NodeTotalHourlyCosts {
		if _, exists := nodeMap[nc.Node]; !exists {
			nodeMap[nc.Node] = &NodeCostSummary{Node: nc.Node}
		}
		nodeMap[nc.Node].TotalHourlyCost = nc.Value
		analytics.TotalHourlyCost += nc.Value
	}

	for _, ng := range metrics.NodeGPUCounts {
		if _, exists := nodeMap[ng.Node]; !exists {
			nodeMap[ng.Node] = &NodeCostSummary{Node: ng.Node}
		}
		nodeMap[ng.Node].GPUCount = ng.Count
	}

	// Convert node map to slice
	for _, ns := range nodeMap {
		analytics.NodeCosts = append(analytics.NodeCosts, *ns)
	}

	// Aggregate storage costs
	for _, pv := range metrics.PVHourlyCosts {
		analytics.StorageHourlyCost += pv.HourlyCost
	}

	// Aggregate load balancer costs
	for _, lb := range metrics.LoadBalancerCosts {
		analytics.LoadBalancerHourlyCost += lb.HourlyCost
	}

	// Calculate projected costs
	analytics.DailyCost = analytics.TotalHourlyCost * 24
	analytics.MonthlyCost = analytics.TotalHourlyCost * 730 // ~730 hours per month

	// Estimate cost by namespace using resource allocation proportions
	if metrics.NodeCapacity != nil && analytics.TotalHourlyCost > 0 {
		totalCPU := metrics.NodeCapacity.CPUCores
		totalMem := metrics.NodeCapacity.MemoryBytes

		// Calculate namespace resource usage
		nsCPU := make(map[string]float64)
		nsMem := make(map[string]float64)
		var sumCPU, sumMem float64

		for _, alloc := range metrics.ContainerCPUAllocations {
			nsCPU[alloc.Namespace] += alloc.Value
			sumCPU += alloc.Value
		}
		for _, alloc := range metrics.ContainerMemoryAllocations {
			nsMem[alloc.Namespace] += alloc.Value
			sumMem += alloc.Value
		}

		// Estimate cost proportionally
		for ns := range nsCPU {
			cpuRatio := 0.0
			memRatio := 0.0
			if totalCPU > 0 {
				cpuRatio = nsCPU[ns] / totalCPU
			}
			if totalMem > 0 {
				memRatio = nsMem[ns] / totalMem
			}
			// Weight CPU and memory equally for cost estimation
			avgRatio := (cpuRatio + memRatio) / 2
			analytics.CostByNamespace[ns] = analytics.TotalHourlyCost * avgRatio
		}
	}

	return analytics, nil
}

// parsePrometheusMetrics parses Prometheus text format metrics from OpenCost.
func parsePrometheusMetrics(reader io.Reader) (*MetricsResponse, error) {
	metrics := newMetricsResponse()

	// Regex patterns for parsing Prometheus metrics
	metricLineRegex := regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{([^}]*)\})?\s+(.+)$`)
	labelRegex := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="([^"]*)"`)

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		matches := metricLineRegex.FindStringSubmatch(line)
		if matches == nil {
			continue
		}

		metricName := matches[1]
		labelsStr := matches[3]
		valueStr := matches[4]

		// Parse value
		value, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			continue
		}

		// Parse labels
		labels := parseLabels(labelRegex, labelsStr)

		// Process the metric
		processMetric(metrics, metricName, labels, value)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading metrics: %w", err)
	}

	return metrics, nil
}

// newMetricsResponse creates a new initialized MetricsResponse.
func newMetricsResponse() *MetricsResponse {
	return &MetricsResponse{
		ContainerCPUAllocations:    []ContainerAllocation{},
		ContainerMemoryAllocations: []ContainerAllocation{},
		ContainerGPUAllocations:    []ContainerAllocation{},
		PodPVCAllocations:          []PVCAllocation{},
		NodeCPUHourlyCosts:         []NodeCost{},
		NodeRAMHourlyCosts:         []NodeCost{},
		NodeGPUHourlyCosts:         []NodeCost{},
		NodeTotalHourlyCosts:       []NodeCost{},
		NodeGPUCounts:              []NodeGPU{},
		PVHourlyCosts:              []PVCost{},
		LoadBalancerCosts:          []LBCost{},
		NetworkZoneEgressCosts:     []NetworkCost{},
		NetworkRegionEgressCosts:   []NetworkCost{},
		NetworkInternetEgressCosts: []NetworkCost{},
		PodResourceRequests:        []PodResourceRequest{},
	}
}

// parseLabels parses Prometheus labels from a label string.
func parseLabels(labelRegex *regexp.Regexp, labelsStr string) map[string]string {
	labels := make(map[string]string)
	labelMatches := labelRegex.FindAllStringSubmatch(labelsStr, -1)
	for _, lm := range labelMatches {
		labels[lm[1]] = lm[2]
	}
	return labels
}

// processMetric processes a single Prometheus metric and adds it to the response.
func processMetric(metrics *MetricsResponse, metricName string, labels map[string]string, value float64) {
	switch metricName {
	case "container_cpu_allocation", "container_memory_allocation_bytes", "container_gpu_allocation":
		processContainerAllocation(metrics, metricName, labels, value)
	case "pod_pvc_allocation":
		metrics.PodPVCAllocations = append(metrics.PodPVCAllocations, PVCAllocation{
			PersistentVolume: labels["persistentvolume"],
			Namespace:        labels["namespace"],
			Pod:              labels["pod"],
			Bytes:            value,
		})
	case "node_cpu_hourly_cost", "node_ram_hourly_cost", "node_gpu_hourly_cost", "node_total_hourly_cost":
		processNodeCost(metrics, metricName, labels, value)
	case "node_gpu_count":
		metrics.NodeGPUCounts = append(metrics.NodeGPUCounts, NodeGPU{
			Node:       labels["node"],
			Instance:   labels["instance"],
			ProviderID: labels["provider_id"],
			Count:      int(value),
		})
	case "pv_hourly_cost":
		metrics.PVHourlyCosts = append(metrics.PVHourlyCosts, PVCost{
			PersistentVolume: labels["persistentvolume"],
			HourlyCost:       value,
		})
	case "kubecost_load_balancer_cost":
		metrics.LoadBalancerCosts = append(metrics.LoadBalancerCosts, LBCost{
			Namespace:  labels["namespace"],
			Service:    labels["service"],
			HourlyCost: value,
		})
	case "kubecost_network_zone_egress_cost", "kubecost_network_region_egress_cost", "kubecost_network_internet_egress_cost":
		processNetworkCost(metrics, metricName, labels, value)
	case "kubecost_cluster_management_cost":
		metrics.ClusterManagementCost = value
	case "kubecost_cluster_info":
		metrics.ClusterInfo = &ClusterInfo{
			ID:             labels["id"],
			Provider:       labels["provider"],
			Version:        labels["version"],
			Region:         labels["region"],
			ClusterProfile: labels["clusterprofile"],
		}
	case "kube_node_status_capacity_cpu_cores", "kube_node_status_capacity_memory_bytes":
		processNodeCapacity(metrics, metricName, labels, value)
	case "kube_node_status_allocatable_cpu_cores", "kube_node_status_allocatable_memory_bytes":
		processNodeAllocatable(metrics, metricName, labels, value)
	case "kube_pod_container_resource_requests":
		metrics.PodResourceRequests = append(metrics.PodResourceRequests, PodResourceRequest{
			Container: labels["container"],
			Namespace: labels["namespace"],
			Pod:       labels["pod"],
			Node:      labels["node"],
			Resource:  labels["resource"],
			Value:     value,
		})
	}
}

// processContainerAllocation handles container allocation metrics.
func processContainerAllocation(metrics *MetricsResponse, metricName string, labels map[string]string, value float64) {
	alloc := ContainerAllocation{
		Container: labels["container"],
		Namespace: labels["namespace"],
		Pod:       labels["pod"],
		Node:      labels["node"],
		Value:     value,
	}
	switch metricName {
	case "container_cpu_allocation":
		metrics.ContainerCPUAllocations = append(metrics.ContainerCPUAllocations, alloc)
	case "container_memory_allocation_bytes":
		metrics.ContainerMemoryAllocations = append(metrics.ContainerMemoryAllocations, alloc)
	case "container_gpu_allocation":
		metrics.ContainerGPUAllocations = append(metrics.ContainerGPUAllocations, alloc)
	}
}

// processNodeCost handles node cost metrics.
func processNodeCost(metrics *MetricsResponse, metricName string, labels map[string]string, value float64) {
	cost := NodeCost{
		Node:       labels["node"],
		Instance:   labels["instance"],
		ProviderID: labels["provider_id"],
		Value:      value,
	}
	switch metricName {
	case "node_cpu_hourly_cost":
		metrics.NodeCPUHourlyCosts = append(metrics.NodeCPUHourlyCosts, cost)
	case "node_ram_hourly_cost":
		metrics.NodeRAMHourlyCosts = append(metrics.NodeRAMHourlyCosts, cost)
	case "node_gpu_hourly_cost":
		metrics.NodeGPUHourlyCosts = append(metrics.NodeGPUHourlyCosts, cost)
	case "node_total_hourly_cost":
		metrics.NodeTotalHourlyCosts = append(metrics.NodeTotalHourlyCosts, cost)
	}
}

// processNetworkCost handles network egress cost metrics.
func processNetworkCost(metrics *MetricsResponse, metricName string, labels map[string]string, value float64) {
	cost := NetworkCost{
		Namespace: labels["namespace"],
		Service:   labels["service"],
		CostPerGB: value,
	}
	switch metricName {
	case "kubecost_network_zone_egress_cost":
		metrics.NetworkZoneEgressCosts = append(metrics.NetworkZoneEgressCosts, cost)
	case "kubecost_network_region_egress_cost":
		metrics.NetworkRegionEgressCosts = append(metrics.NetworkRegionEgressCosts, cost)
	case "kubecost_network_internet_egress_cost":
		metrics.NetworkInternetEgressCosts = append(metrics.NetworkInternetEgressCosts, cost)
	}
}

// processNodeCapacity handles node capacity metrics.
func processNodeCapacity(metrics *MetricsResponse, metricName string, labels map[string]string, value float64) {
	if metrics.NodeCapacity == nil {
		metrics.NodeCapacity = &NodeCapacity{Node: labels["node"]}
	}
	switch metricName {
	case "kube_node_status_capacity_cpu_cores":
		metrics.NodeCapacity.CPUCores = value
	case "kube_node_status_capacity_memory_bytes":
		metrics.NodeCapacity.MemoryBytes = value
	}
}

// processNodeAllocatable handles node allocatable metrics.
func processNodeAllocatable(metrics *MetricsResponse, metricName string, labels map[string]string, value float64) {
	if metrics.NodeAllocatable == nil {
		metrics.NodeAllocatable = &NodeAllocatable{Node: labels["node"]}
	}
	switch metricName {
	case "kube_node_status_allocatable_cpu_cores":
		metrics.NodeAllocatable.CPUCores = value
	case "kube_node_status_allocatable_memory_bytes":
		metrics.NodeAllocatable.MemoryBytes = value
	}
}

// getTopConsumers returns the top N resource consumers sorted by value.
func getTopConsumers(allocations []ContainerAllocation, total float64, n int) []ResourceConsumer {
	consumers := make([]ResourceConsumer, 0, len(allocations))

	for _, alloc := range allocations {
		percent := 0.0
		if total > 0 {
			percent = (alloc.Value / total) * 100
		}
		consumers = append(consumers, ResourceConsumer{
			Name:      fmt.Sprintf("%s/%s/%s", alloc.Namespace, alloc.Pod, alloc.Container),
			Namespace: alloc.Namespace,
			Pod:       alloc.Pod,
			Container: alloc.Container,
			Value:     alloc.Value,
			Percent:   percent,
		})
	}

	// Sort by value descending
	sort.Slice(consumers, func(i, j int) bool {
		return consumers[i].Value > consumers[j].Value
	})

	// Return top N
	if len(consumers) > n {
		return consumers[:n]
	}
	return consumers
}
