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

package opencost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveOpenCostAPI tests against a live OpenCost instance.
// Run with: go test -v -run TestLiveOpenCostAPI -tags=integration
// Requires OpenCost to be port-forwarded to localhost:9003 and localhost:8081
func TestLiveOpenCostAPI(t *testing.T) {
	// Skip if not running integration tests
	if os.Getenv("OPENCOST_INTEGRATION_TEST") != "true" {
		t.Skip("Skipping integration test. Set OPENCOST_INTEGRATION_TEST=true to run")
	}

	// Default to localhost if not specified
	opencostURL := os.Getenv("OPENCOST_URL")
	if opencostURL == "" {
		opencostURL = "http://localhost:9003"
	}

	client, err := NewClient(Config{
		URL:     opencostURL,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	ctx := context.Background()

	t.Run("HealthCheck", func(t *testing.T) {
		err := client.HealthCheck(ctx)
		if err != nil {
			t.Errorf("HealthCheck failed: %v", err)
		} else {
			t.Log("HealthCheck: OK")
		}
	})

	t.Run("GetMetrics", func(t *testing.T) {
		metrics, err := client.GetMetrics(ctx)
		if err != nil {
			t.Errorf("GetMetrics failed: %v", err)
			return
		}

		t.Logf("Container CPU Allocations: %d", len(metrics.ContainerCPUAllocations))
		t.Logf("Container Memory Allocations: %d", len(metrics.ContainerMemoryAllocations))
		t.Logf("Container GPU Allocations: %d", len(metrics.ContainerGPUAllocations))

		if metrics.NodeCapacity != nil {
			t.Logf("Node Capacity - CPU: %.2f cores, Memory: %.2f GB",
				metrics.NodeCapacity.CPUCores,
				metrics.NodeCapacity.MemoryBytes/(1024*1024*1024))
		}

		if metrics.NodeAllocatable != nil {
			t.Logf("Node Allocatable - CPU: %.2f cores, Memory: %.2f GB",
				metrics.NodeAllocatable.CPUCores,
				metrics.NodeAllocatable.MemoryBytes/(1024*1024*1024))
		}

		if metrics.ClusterInfo != nil {
			t.Logf("Cluster Info: ID=%s, Provider=%s, Version=%s",
				metrics.ClusterInfo.ID,
				metrics.ClusterInfo.Provider,
				metrics.ClusterInfo.Version)
		}
	})

	t.Run("GetResourceAnalytics", func(t *testing.T) {
		analytics, err := client.GetResourceAnalytics(ctx)
		if err != nil {
			t.Errorf("GetResourceAnalytics failed: %v", err)
			return
		}

		t.Logf("=== Resource Analytics ===")
		t.Logf("Total CPU Capacity: %.2f cores", analytics.TotalCPUCapacity)
		t.Logf("Total Memory Capacity: %.2f GB", analytics.TotalMemoryCapacity/(1024*1024*1024))
		t.Logf("Total CPU Allocated: %.2f cores", analytics.TotalCPUAllocated)
		t.Logf("Total Memory Allocated: %.2f GB", analytics.TotalMemoryAllocated/(1024*1024*1024))
		t.Logf("CPU Utilization: %.2f%%", analytics.CPUUtilizationPercent)
		t.Logf("Memory Utilization: %.2f%%", analytics.MemoryUtilizationPercent)

		t.Logf("\n=== CPU by Namespace ===")
		for ns, cpu := range analytics.CPUByNamespace {
			t.Logf("  %s: %.3f cores", ns, cpu)
		}

		t.Logf("\n=== Top CPU Consumers ===")
		for i, consumer := range analytics.TopCPUConsumers {
			if i >= 5 {
				break
			}
			t.Logf("  %d. %s: %.3f cores (%.1f%%)", i+1, consumer.Name, consumer.Value, consumer.Percent)
		}

		t.Logf("\n=== Top Memory Consumers ===")
		for i, consumer := range analytics.TopMemoryConsumers {
			if i >= 5 {
				break
			}
			t.Logf("  %d. %s: %.2f MB (%.1f%%)", i+1, consumer.Name, consumer.Value/(1024*1024), consumer.Percent)
		}
	})

	t.Run("GetClusterResourceSummary", func(t *testing.T) {
		summary, err := client.GetClusterResourceSummary(ctx)
		if err != nil {
			t.Errorf("GetClusterResourceSummary failed: %v", err)
			return
		}

		t.Logf("=== Cluster Resource Summary ===")
		t.Logf("Total Nodes: %d", summary.TotalNodes)
		t.Logf("Total CPU Cores: %.2f", summary.TotalCPUCores)
		t.Logf("Total Memory: %.2f GB", summary.TotalMemoryGB)
		t.Logf("Allocated CPU: %.2f cores", summary.AllocatedCPUCores)
		t.Logf("Allocated Memory: %.2f GB", summary.AllocatedMemoryGB)
		t.Logf("Available CPU: %.2f cores", summary.AvailableCPUCores)
		t.Logf("Available Memory: %.2f GB", summary.AvailableMemoryGB)
		t.Logf("CPU Utilization: %.2f%%", summary.CPUUtilization)
		t.Logf("Memory Utilization: %.2f%%", summary.MemoryUtilization)
		t.Logf("Total Namespaces: %d", summary.TotalNamespaces)
		t.Logf("Total Pods: %d", summary.TotalPods)
		t.Logf("Total Containers: %d", summary.TotalContainers)
	})

	t.Run("GetNamespaceBreakdown", func(t *testing.T) {
		// Test with a known namespace
		namespaces := []string{"default", "kube-system", "sartor-system"}

		for _, ns := range namespaces {
			breakdown, err := client.GetNamespaceBreakdown(ctx, ns)
			if err != nil {
				t.Errorf("GetNamespaceBreakdown(%s) failed: %v", ns, err)
				continue
			}

			if breakdown.TotalCPU > 0 || breakdown.TotalMemory > 0 {
				t.Logf("\n=== Namespace: %s ===", ns)
				t.Logf("Total CPU: %.3f cores (%.1f%% of cluster)", breakdown.TotalCPU, breakdown.CPUPercent)
				t.Logf("Total Memory: %.2f MB (%.1f%% of cluster)", breakdown.TotalMemory/(1024*1024), breakdown.MemoryPercent)
				t.Logf("Containers: %d", len(breakdown.Containers))
				for _, c := range breakdown.Containers {
					t.Logf("  - %s/%s: CPU=%.3f, Memory=%.2f MB",
						c.Pod, c.Container, c.CPU, c.Memory/(1024*1024))
				}
			}
		}
	})

	t.Run("GetCostAnalytics", func(t *testing.T) {
		analytics, err := client.GetCostAnalytics(ctx)
		if err != nil {
			t.Errorf("GetCostAnalytics failed: %v", err)
			return
		}

		t.Logf("=== Cost Analytics ===")
		t.Logf("Total Hourly Cost: $%.4f", analytics.TotalHourlyCost)
		t.Logf("CPU Hourly Cost: $%.4f", analytics.CPUHourlyCost)
		t.Logf("RAM Hourly Cost: $%.4f", analytics.RAMHourlyCost)
		t.Logf("GPU Hourly Cost: $%.4f", analytics.GPUHourlyCost)
		t.Logf("Storage Hourly Cost: $%.4f", analytics.StorageHourlyCost)
		t.Logf("Load Balancer Hourly Cost: $%.4f", analytics.LoadBalancerHourlyCost)
		t.Logf("Cluster Management Cost: $%.4f", analytics.ClusterManagementCost)
		t.Logf("Daily Cost (projected): $%.2f", analytics.DailyCost)
		t.Logf("Monthly Cost (projected): $%.2f", analytics.MonthlyCost)

		if len(analytics.CostByNamespace) > 0 {
			t.Logf("\n=== Estimated Cost by Namespace ===")
			for ns, cost := range analytics.CostByNamespace {
				t.Logf("  %s: $%.4f/hr", ns, cost)
			}
		}

		if len(analytics.NodeCosts) > 0 {
			t.Logf("\n=== Node Costs ===")
			for _, nc := range analytics.NodeCosts {
				t.Logf("  %s: CPU=$%.4f/hr, RAM=$%.4f/hr, Total=$%.4f/hr, GPUs=%d",
					nc.Node, nc.CPUHourlyCost, nc.RAMHourlyCost, nc.TotalHourlyCost, nc.GPUCount)
			}
		}
	})
}

// TestParsePrometheusMetrics tests the Prometheus metrics parser.
func TestParsePrometheusMetrics(t *testing.T) {
	// Sample metrics similar to what OpenCost produces
	sampleMetrics := `# HELP container_cpu_allocation container_cpu_allocation Percent of a single CPU used in a minute
# TYPE container_cpu_allocation gauge
container_cpu_allocation{container="app",instance="node1",namespace="default",node="node1",pod="sample-app-1-abc123"} 0.05
container_cpu_allocation{container="controller",instance="node1",namespace="sartor-system",node="node1",pod="sartor-controller-xyz789"} 0.1
# HELP container_memory_allocation_bytes container_memory_allocation_bytes Bytes of RAM used
# TYPE container_memory_allocation_bytes gauge
container_memory_allocation_bytes{container="app",instance="node1",namespace="default",node="node1",pod="sample-app-1-abc123"} 67108864
container_memory_allocation_bytes{container="controller",instance="node1",namespace="sartor-system",node="node1",pod="sartor-controller-xyz789"} 134217728
# HELP container_gpu_allocation container_gpu_allocation GPU used
# TYPE container_gpu_allocation gauge
container_gpu_allocation{container="app",instance="node1",namespace="default",node="node1",pod="sample-app-1-abc123"} 0
# HELP kube_node_status_capacity_cpu_cores kube_node_status_capacity_cpu_cores Node Capacity CPU Cores
# TYPE kube_node_status_capacity_cpu_cores gauge
kube_node_status_capacity_cpu_cores{node="node1",uid="abc123"} 10
# HELP kube_node_status_capacity_memory_bytes kube_node_status_capacity_memory_bytes Node Capacity Memory Bytes
# TYPE kube_node_status_capacity_memory_bytes gauge
kube_node_status_capacity_memory_bytes{node="node1",uid="abc123"} 8383180800
# HELP kubecost_cluster_info kubecost_cluster_info ClusterInfo
# TYPE kubecost_cluster_info gauge
kubecost_cluster_info{id="default-cluster",provider="custom",version="1.34",clusterprofile="development"} 1
# HELP node_cpu_hourly_cost node_cpu_hourly_cost Hourly cost per vCPU
# TYPE node_cpu_hourly_cost gauge
node_cpu_hourly_cost{node="node1",instance="node1",provider_id=""} 0.05
# HELP node_ram_hourly_cost node_ram_hourly_cost Hourly cost per GiB of memory
# TYPE node_ram_hourly_cost gauge
node_ram_hourly_cost{node="node1",instance="node1",provider_id=""} 0.01
# HELP node_total_hourly_cost node_total_hourly_cost Total node cost per hour
# TYPE node_total_hourly_cost gauge
node_total_hourly_cost{node="node1",instance="node1",provider_id=""} 0.15
`

	reader := strings.NewReader(sampleMetrics)
	metrics, err := parsePrometheusMetrics(reader)
	if err != nil {
		t.Fatalf("parsePrometheusMetrics failed: %v", err)
	}

	// Verify container CPU allocations
	if len(metrics.ContainerCPUAllocations) != 2 {
		t.Errorf("Expected 2 CPU allocations, got %d", len(metrics.ContainerCPUAllocations))
	}

	// Verify container memory allocations
	if len(metrics.ContainerMemoryAllocations) != 2 {
		t.Errorf("Expected 2 memory allocations, got %d", len(metrics.ContainerMemoryAllocations))
	}

	// Verify node capacity
	if metrics.NodeCapacity == nil {
		t.Error("Expected NodeCapacity to be set")
	} else {
		if metrics.NodeCapacity.CPUCores != 10 {
			t.Errorf("Expected CPU cores 10, got %.2f", metrics.NodeCapacity.CPUCores)
		}
	}

	// Verify cluster info
	if metrics.ClusterInfo == nil {
		t.Error("Expected ClusterInfo to be set")
	} else {
		if metrics.ClusterInfo.ID != "default-cluster" {
			t.Errorf("Expected cluster ID 'default-cluster', got '%s'", metrics.ClusterInfo.ID)
		}
		if metrics.ClusterInfo.Provider != "custom" {
			t.Errorf("Expected provider 'custom', got '%s'", metrics.ClusterInfo.Provider)
		}
	}

	// Verify node costs
	if len(metrics.NodeCPUHourlyCosts) != 1 {
		t.Errorf("Expected 1 node CPU cost, got %d", len(metrics.NodeCPUHourlyCosts))
	}
	if len(metrics.NodeTotalHourlyCosts) != 1 {
		t.Errorf("Expected 1 node total cost, got %d", len(metrics.NodeTotalHourlyCosts))
	}

	// Print parsed metrics as JSON for verification
	jsonData, _ := json.MarshalIndent(metrics, "", "  ")
	t.Logf("Parsed metrics:\n%s", string(jsonData))
}

// TestGetTopConsumers tests the top consumers helper function.
func TestGetTopConsumers(t *testing.T) {
	allocations := []ContainerAllocation{
		{Container: "c1", Namespace: "ns1", Pod: "p1", Value: 0.1},
		{Container: "c2", Namespace: "ns1", Pod: "p2", Value: 0.5},
		{Container: "c3", Namespace: "ns2", Pod: "p3", Value: 0.3},
		{Container: "c4", Namespace: "ns2", Pod: "p4", Value: 0.05},
		{Container: "c5", Namespace: "ns3", Pod: "p5", Value: 0.25},
	}

	total := 0.0
	for _, a := range allocations {
		total += a.Value
	}

	consumers := getTopConsumers(allocations, total, 3)

	if len(consumers) != 3 {
		t.Errorf("Expected 3 consumers, got %d", len(consumers))
	}

	// Verify sorted order (descending)
	if consumers[0].Value != 0.5 {
		t.Errorf("Expected top consumer value 0.5, got %.2f", consumers[0].Value)
	}
	if consumers[1].Value != 0.3 {
		t.Errorf("Expected second consumer value 0.3, got %.2f", consumers[1].Value)
	}
	if consumers[2].Value != 0.25 {
		t.Errorf("Expected third consumer value 0.25, got %.2f", consumers[2].Value)
	}

	// Verify percentages
	expectedPercent := (0.5 / total) * 100
	if fmt.Sprintf("%.2f", consumers[0].Percent) != fmt.Sprintf("%.2f", expectedPercent) {
		t.Errorf("Expected percent %.2f, got %.2f", expectedPercent, consumers[0].Percent)
	}
}
