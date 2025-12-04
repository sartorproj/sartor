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
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			name: "valid config",
			config: Config{
				URL: "http://opencost:9003",
			},
			wantErr: false,
		},
		{
			name: "config with insecure skip verify",
			config: Config{
				URL:                "https://opencost:9003",
				InsecureSkipVerify: true,
			},
			wantErr: false,
		},
		{
			name: "config with custom timeout",
			config: Config{
				URL:     "http://opencost:9003",
				Timeout: 60 * time.Second,
			},
			wantErr: false,
		},
		{
			name: "empty URL",
			config: Config{
				URL: "",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(tt.config)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, client)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, client)
			}
		})
	}
}

func TestClient_GetAllocations(t *testing.T) {
	// Create a mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/allocation" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Check query params
		window := r.URL.Query().Get("window")
		aggregate := r.URL.Query().Get("aggregate")

		if window == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		response := AllocationResponse{
			Code:   200,
			Status: "success",
			Data: []map[string]*AllocationEntry{
				{
					"default": {
						Name: "default",
						Properties: AllocationProperties{
							Namespace: "default",
							Cluster:   "test-cluster",
						},
						CPUCores:        0.5,
						CPUCost:         0.025,
						CPUEfficiency:   0.8,
						RAMBytes:        536870912,
						RAMCost:         0.01,
						RAMEfficiency:   0.7,
						TotalCost:       0.035,
						TotalEfficiency: 0.75,
					},
				},
			},
		}

		// Use aggregate to filter response
		_ = aggregate

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("basic allocation request", func(t *testing.T) {
		resp, err := client.GetAllocations(ctx, AllocationRequest{
			Window:    "7d",
			Aggregate: "namespace",
		})
		require.NoError(t, err)
		assert.Equal(t, 200, resp.Code)
		assert.Equal(t, "success", resp.Status)
		assert.Len(t, resp.Data, 1)
		assert.NotNil(t, resp.Data[0]["default"])
	})

	t.Run("default window and aggregate", func(t *testing.T) {
		resp, err := client.GetAllocations(ctx, AllocationRequest{})
		require.NoError(t, err)
		assert.Equal(t, 200, resp.Code)
	})

	t.Run("with step and resolution", func(t *testing.T) {
		resp, err := client.GetAllocations(ctx, AllocationRequest{
			Window:     "7d",
			Aggregate:  "namespace",
			Step:       "1d",
			Resolution: "1m",
		})
		require.NoError(t, err)
		assert.Equal(t, 200, resp.Code)
	})
}

func TestClient_GetAllocations_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	resp, err := client.GetAllocations(ctx, AllocationRequest{Window: "7d"})

	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "unexpected status code: 500")
}

func TestClient_HealthCheck(t *testing.T) {
	t.Run("healthy server", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code": 200, "status": "success", "data": []}`))
		}))
		defer server.Close()

		client, err := NewClient(Config{URL: server.URL})
		require.NoError(t, err)

		ctx := context.Background()
		err = client.HealthCheck(ctx)
		assert.NoError(t, err)
	})

	t.Run("unhealthy server", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		client, err := NewClient(Config{URL: server.URL})
		require.NoError(t, err)

		ctx := context.Background()
		err = client.HealthCheck(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "returned status 503")
	})
}

func TestClient_GetCostSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := AllocationResponse{
			Code:   200,
			Status: "success",
			Data: []map[string]*AllocationEntry{
				{
					"default": {
						Name: "default",
						Properties: AllocationProperties{
							Namespace: "default",
						},
						TotalCost:        50.0,
						CPUCost:          20.0,
						RAMCost:          15.0,
						GPUCost:          5.0,
						PVCost:           5.0,
						NetworkCost:      3.0,
						LoadBalancerCost: 1.0,
						SharedCost:       1.0,
						TotalEfficiency:  0.75,
						CPUEfficiency:    0.8,
						RAMEfficiency:    0.7,
					},
					"production": {
						Name: "production",
						Properties: AllocationProperties{
							Namespace: "production",
						},
						TotalCost:        50.0,
						CPUCost:          20.0,
						RAMCost:          15.0,
						GPUCost:          5.0,
						PVCost:           5.0,
						NetworkCost:      3.0,
						LoadBalancerCost: 1.0,
						SharedCost:       1.0,
						TotalEfficiency:  0.8,
						CPUEfficiency:    0.85,
						RAMEfficiency:    0.75,
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	summary, err := client.GetCostSummary(ctx, "7d")

	require.NoError(t, err)
	assert.Equal(t, 100.0, summary.TotalCost)
	assert.Equal(t, 40.0, summary.CPUCost)
	assert.Equal(t, 30.0, summary.RAMCost)
	assert.Len(t, summary.ByNamespace, 2)
	assert.Equal(t, 50.0, summary.ByNamespace["default"])
	assert.Equal(t, 50.0, summary.ByNamespace["production"])
}

func TestClient_GetMetrics(t *testing.T) {
	metricsData := `# HELP container_cpu_allocation container_cpu_allocation Percent of a single CPU used
# TYPE container_cpu_allocation gauge
container_cpu_allocation{container="app",namespace="default",pod="test-pod",node="node1"} 0.5
# HELP container_memory_allocation_bytes container_memory_allocation_bytes Bytes of RAM used
# TYPE container_memory_allocation_bytes gauge
container_memory_allocation_bytes{container="app",namespace="default",pod="test-pod",node="node1"} 536870912
# HELP kube_node_status_capacity_cpu_cores kube_node_status_capacity_cpu_cores Node Capacity CPU Cores
# TYPE kube_node_status_capacity_cpu_cores gauge
kube_node_status_capacity_cpu_cores{node="node1"} 8
# HELP kube_node_status_capacity_memory_bytes kube_node_status_capacity_memory_bytes Node Capacity Memory Bytes
# TYPE kube_node_status_capacity_memory_bytes gauge
kube_node_status_capacity_memory_bytes{node="node1"} 17179869184
# HELP node_cpu_hourly_cost node_cpu_hourly_cost Hourly cost per vCPU
# TYPE node_cpu_hourly_cost gauge
node_cpu_hourly_cost{node="node1",instance="node1"} 0.05
# HELP node_ram_hourly_cost node_ram_hourly_cost Hourly cost per GiB of memory
# TYPE node_ram_hourly_cost gauge
node_ram_hourly_cost{node="node1",instance="node1"} 0.01
# HELP node_total_hourly_cost node_total_hourly_cost Total node cost per hour
# TYPE node_total_hourly_cost gauge
node_total_hourly_cost{node="node1",instance="node1"} 0.15
`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(metricsData))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	metrics, err := client.GetMetrics(ctx)

	require.NoError(t, err)
	assert.Len(t, metrics.ContainerCPUAllocations, 1)
	assert.Len(t, metrics.ContainerMemoryAllocations, 1)
	assert.NotNil(t, metrics.NodeCapacity)
	assert.Equal(t, 8.0, metrics.NodeCapacity.CPUCores)
	assert.Len(t, metrics.NodeCPUHourlyCosts, 1)
}

func TestClient_GetResourceAnalytics(t *testing.T) {
	metricsData := `container_cpu_allocation{container="app1",namespace="default",pod="pod1",node="node1"} 0.5
container_cpu_allocation{container="app2",namespace="production",pod="pod2",node="node1"} 0.3
container_memory_allocation_bytes{container="app1",namespace="default",pod="pod1",node="node1"} 536870912
container_memory_allocation_bytes{container="app2",namespace="production",pod="pod2",node="node1"} 268435456
kube_node_status_capacity_cpu_cores{node="node1"} 8
kube_node_status_capacity_memory_bytes{node="node1"} 17179869184
kube_node_status_allocatable_cpu_cores{node="node1"} 7.5
kube_node_status_allocatable_memory_bytes{node="node1"} 16000000000
`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(metricsData))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	analytics, err := client.GetResourceAnalytics(ctx)

	require.NoError(t, err)
	assert.Equal(t, 8.0, analytics.TotalCPUCapacity)
	assert.Equal(t, 7.5, analytics.TotalCPUAllocatable)
	assert.Equal(t, 0.8, analytics.TotalCPUAllocated)
	assert.Greater(t, analytics.CPUUtilizationPercent, 0.0)

	// Check namespace breakdown
	assert.Contains(t, analytics.CPUByNamespace, "default")
	assert.Contains(t, analytics.CPUByNamespace, "production")
	assert.Equal(t, 0.5, analytics.CPUByNamespace["default"])
	assert.Equal(t, 0.3, analytics.CPUByNamespace["production"])

	// Check top consumers
	assert.GreaterOrEqual(t, len(analytics.TopCPUConsumers), 1)
}

func TestClient_GetClusterResourceSummary(t *testing.T) {
	metricsData := `container_cpu_allocation{container="app1",namespace="default",pod="pod1",node="node1"} 0.5
container_cpu_allocation{container="app2",namespace="default",pod="pod2",node="node1"} 0.3
container_cpu_allocation{container="app3",namespace="production",pod="pod3",node="node1"} 0.2
container_memory_allocation_bytes{container="app1",namespace="default",pod="pod1",node="node1"} 536870912
container_memory_allocation_bytes{container="app2",namespace="default",pod="pod2",node="node1"} 268435456
container_memory_allocation_bytes{container="app3",namespace="production",pod="pod3",node="node1"} 268435456
kube_node_status_capacity_cpu_cores{node="node1"} 8
kube_node_status_capacity_memory_bytes{node="node1"} 17179869184
`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(metricsData))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	summary, err := client.GetClusterResourceSummary(ctx)

	require.NoError(t, err)
	assert.Equal(t, 8.0, summary.TotalCPUCores)
	assert.Equal(t, 1.0, summary.AllocatedCPUCores)
	assert.Greater(t, summary.CPUUtilization, 0.0)
	assert.Equal(t, 2, summary.TotalNamespaces) // default and production
	assert.Equal(t, 3, summary.TotalPods)
	assert.Equal(t, 3, summary.TotalContainers)
}

func TestClient_GetNamespaceBreakdown(t *testing.T) {
	metricsData := `container_cpu_allocation{container="app1",namespace="default",pod="pod1",node="node1"} 0.5
container_cpu_allocation{container="app2",namespace="default",pod="pod2",node="node1"} 0.3
container_cpu_allocation{container="app3",namespace="production",pod="pod3",node="node1"} 0.2
container_memory_allocation_bytes{container="app1",namespace="default",pod="pod1",node="node1"} 536870912
container_memory_allocation_bytes{container="app2",namespace="default",pod="pod2",node="node1"} 268435456
container_memory_allocation_bytes{container="app3",namespace="production",pod="pod3",node="node1"} 268435456
container_gpu_allocation{container="app1",namespace="default",pod="pod1",node="node1"} 1
`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(metricsData))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	breakdown, err := client.GetNamespaceBreakdown(ctx, "default")

	require.NoError(t, err)
	assert.Equal(t, "default", breakdown.Namespace)
	assert.Equal(t, 0.8, breakdown.TotalCPU)
	assert.Greater(t, breakdown.TotalMemory, 0.0)
	assert.Equal(t, 1.0, breakdown.TotalGPU)
	assert.Len(t, breakdown.Containers, 2)
}

func TestClient_GetCostAnalytics(t *testing.T) {
	metricsData := `node_cpu_hourly_cost{node="node1",instance="node1"} 0.05
node_ram_hourly_cost{node="node1",instance="node1"} 0.01
node_gpu_hourly_cost{node="node1",instance="node1"} 0.0
node_total_hourly_cost{node="node1",instance="node1"} 0.15
node_gpu_count{node="node1",instance="node1"} 0
pv_hourly_cost{persistentvolume="pv1"} 0.01
kubecost_load_balancer_cost{namespace="default",service="my-svc"} 0.02
kubecost_cluster_management_cost 0.05
container_cpu_allocation{container="app1",namespace="default",pod="pod1",node="node1"} 0.5
container_memory_allocation_bytes{container="app1",namespace="default",pod="pod1",node="node1"} 536870912
kube_node_status_capacity_cpu_cores{node="node1"} 8
kube_node_status_capacity_memory_bytes{node="node1"} 17179869184
`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(metricsData))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL})
	require.NoError(t, err)

	ctx := context.Background()
	analytics, err := client.GetCostAnalytics(ctx)

	require.NoError(t, err)
	assert.InDelta(t, 0.15, analytics.TotalHourlyCost, 0.001)
	assert.InDelta(t, 0.05, analytics.CPUHourlyCost, 0.001)
	assert.InDelta(t, 0.01, analytics.RAMHourlyCost, 0.001)
	assert.InDelta(t, 0.01, analytics.StorageHourlyCost, 0.001)
	assert.InDelta(t, 0.02, analytics.LoadBalancerHourlyCost, 0.001)
	assert.InDelta(t, 0.05, analytics.ClusterManagementCost, 0.001)
	assert.InDelta(t, 0.15*24, analytics.DailyCost, 0.01)
	assert.InDelta(t, 0.15*730, analytics.MonthlyCost, 0.1)
	assert.Len(t, analytics.NodeCosts, 1)
}

func TestParsePrometheusMetrics(t *testing.T) {
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
# HELP kube_node_status_allocatable_cpu_cores kube_node_status_allocatable_cpu_cores Node Allocatable CPU Cores
# TYPE kube_node_status_allocatable_cpu_cores gauge
kube_node_status_allocatable_cpu_cores{node="node1"} 9.5
# HELP kube_node_status_allocatable_memory_bytes kube_node_status_allocatable_memory_bytes Node Allocatable Memory Bytes
# TYPE kube_node_status_allocatable_memory_bytes gauge
kube_node_status_allocatable_memory_bytes{node="node1"} 8000000000
# HELP pod_pvc_allocation pod_pvc_allocation PVC allocation
# TYPE pod_pvc_allocation gauge
pod_pvc_allocation{persistentvolume="pv1",namespace="default",pod="pod1"} 10737418240
# HELP pv_hourly_cost pv_hourly_cost PV hourly cost
# TYPE pv_hourly_cost gauge
pv_hourly_cost{persistentvolume="pv1"} 0.005
# HELP kubecost_load_balancer_cost kubecost_load_balancer_cost LB cost
# TYPE kubecost_load_balancer_cost gauge
kubecost_load_balancer_cost{namespace="default",service="my-svc"} 0.01
# HELP kubecost_network_zone_egress_cost kubecost_network_zone_egress_cost Network zone egress cost
# TYPE kubecost_network_zone_egress_cost gauge
kubecost_network_zone_egress_cost{namespace="default",service="my-svc"} 0.001
# HELP kubecost_network_region_egress_cost kubecost_network_region_egress_cost Network region egress cost
# TYPE kubecost_network_region_egress_cost gauge
kubecost_network_region_egress_cost{namespace="default",service="my-svc"} 0.005
# HELP kubecost_network_internet_egress_cost kubecost_network_internet_egress_cost Network internet egress cost
# TYPE kubecost_network_internet_egress_cost gauge
kubecost_network_internet_egress_cost{namespace="default",service="my-svc"} 0.01
# HELP kubecost_cluster_management_cost kubecost_cluster_management_cost Cluster management cost
# TYPE kubecost_cluster_management_cost gauge
kubecost_cluster_management_cost 0.05
# HELP node_gpu_count node_gpu_count Node GPU count
# TYPE node_gpu_count gauge
node_gpu_count{node="node1",instance="node1",provider_id=""} 2
# HELP kube_pod_container_resource_requests kube_pod_container_resource_requests Pod resource requests
# TYPE kube_pod_container_resource_requests gauge
kube_pod_container_resource_requests{container="app",namespace="default",pod="pod1",node="node1",resource="cpu"} 0.1
`

	reader := strings.NewReader(sampleMetrics)
	metrics, err := parsePrometheusMetrics(reader)
	require.NoError(t, err)

	// Verify container allocations
	assert.Len(t, metrics.ContainerCPUAllocations, 2)
	assert.Len(t, metrics.ContainerMemoryAllocations, 2)
	assert.Len(t, metrics.ContainerGPUAllocations, 1)

	// Verify node capacity
	require.NotNil(t, metrics.NodeCapacity)
	assert.Equal(t, 10.0, metrics.NodeCapacity.CPUCores)
	assert.Equal(t, 8383180800.0, metrics.NodeCapacity.MemoryBytes)

	// Verify node allocatable
	require.NotNil(t, metrics.NodeAllocatable)
	assert.Equal(t, 9.5, metrics.NodeAllocatable.CPUCores)

	// Verify cluster info
	require.NotNil(t, metrics.ClusterInfo)
	assert.Equal(t, "default-cluster", metrics.ClusterInfo.ID)
	assert.Equal(t, "custom", metrics.ClusterInfo.Provider)
	assert.Equal(t, "1.34", metrics.ClusterInfo.Version)
	assert.Equal(t, "development", metrics.ClusterInfo.ClusterProfile)

	// Verify node costs
	assert.Len(t, metrics.NodeCPUHourlyCosts, 1)
	assert.Len(t, metrics.NodeRAMHourlyCosts, 1)
	assert.Len(t, metrics.NodeTotalHourlyCosts, 1)
	assert.Len(t, metrics.NodeGPUCounts, 1)
	assert.Equal(t, 2, metrics.NodeGPUCounts[0].Count)

	// Verify PVC and PV
	assert.Len(t, metrics.PodPVCAllocations, 1)
	assert.Len(t, metrics.PVHourlyCosts, 1)

	// Verify network costs
	assert.Len(t, metrics.NetworkZoneEgressCosts, 1)
	assert.Len(t, metrics.NetworkRegionEgressCosts, 1)
	assert.Len(t, metrics.NetworkInternetEgressCosts, 1)

	// Verify load balancer costs
	assert.Len(t, metrics.LoadBalancerCosts, 1)

	// Verify cluster management cost
	assert.Equal(t, 0.05, metrics.ClusterManagementCost)

	// Verify pod resource requests
	assert.Len(t, metrics.PodResourceRequests, 1)
}

func TestParsePrometheusMetrics_EmptyAndComments(t *testing.T) {
	metricsData := `# This is a comment
# Another comment

# Empty line above


`
	reader := strings.NewReader(metricsData)
	metrics, err := parsePrometheusMetrics(reader)

	require.NoError(t, err)
	assert.Empty(t, metrics.ContainerCPUAllocations)
}

func TestParsePrometheusMetrics_InvalidValues(t *testing.T) {
	metricsData := `container_cpu_allocation{container="app",namespace="default"} invalid
container_cpu_allocation{container="app2",namespace="default"} 0.5
`
	reader := strings.NewReader(metricsData)
	metrics, err := parsePrometheusMetrics(reader)

	require.NoError(t, err)
	// Should skip the invalid value and parse the valid one
	assert.Len(t, metrics.ContainerCPUAllocations, 1)
}

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

	t.Run("top 3 consumers", func(t *testing.T) {
		consumers := getTopConsumers(allocations, total, 3)
		assert.Len(t, consumers, 3)
		assert.Equal(t, 0.5, consumers[0].Value)
		assert.Equal(t, 0.3, consumers[1].Value)
		assert.Equal(t, 0.25, consumers[2].Value)
	})

	t.Run("request more than available", func(t *testing.T) {
		consumers := getTopConsumers(allocations, total, 10)
		assert.Len(t, consumers, 5)
	})

	t.Run("verify percentages", func(t *testing.T) {
		consumers := getTopConsumers(allocations, total, 1)
		expectedPercent := (0.5 / total) * 100
		assert.InDelta(t, expectedPercent, consumers[0].Percent, 0.01)
	})

	t.Run("zero total", func(t *testing.T) {
		consumers := getTopConsumers(allocations, 0, 3)
		assert.Len(t, consumers, 3)
		assert.Equal(t, 0.0, consumers[0].Percent)
	})
}

func TestAllocationResponse_Structures(t *testing.T) {
	t.Run("AllocationEntry", func(t *testing.T) {
		entry := AllocationEntry{
			Name: "test",
			Properties: AllocationProperties{
				Namespace:      "default",
				Controller:     "my-deployment",
				ControllerKind: "Deployment",
			},
			CPUCores:         0.5,
			CPUCost:          0.025,
			CPUEfficiency:    0.8,
			RAMBytes:         536870912,
			RAMCost:          0.01,
			RAMEfficiency:    0.7,
			GPUCount:         1,
			GPUCost:          0.5,
			NetworkCost:      0.001,
			PVCost:           0.005,
			LoadBalancerCost: 0.01,
			SharedCost:       0.002,
			TotalCost:        0.603,
			TotalEfficiency:  0.75,
		}

		data, err := json.Marshal(entry)
		require.NoError(t, err)

		var decoded AllocationEntry
		err = json.Unmarshal(data, &decoded)
		require.NoError(t, err)

		assert.Equal(t, entry.Name, decoded.Name)
		assert.Equal(t, entry.CPUCores, decoded.CPUCores)
		assert.Equal(t, entry.TotalCost, decoded.TotalCost)
	})

	t.Run("CostSummary", func(t *testing.T) {
		summary := CostSummary{
			TotalCost:        100.0,
			CPUCost:          40.0,
			RAMCost:          30.0,
			GPUCost:          10.0,
			PVCost:           10.0,
			NetworkCost:      5.0,
			LoadBalancerCost: 3.0,
			SharedCost:       2.0,
			Efficiency:       0.75,
			CPUEfficiency:    0.8,
			RAMEfficiency:    0.7,
			ByNamespace: map[string]float64{
				"default": 50.0,
			},
		}

		data, err := json.Marshal(summary)
		require.NoError(t, err)

		var decoded CostSummary
		err = json.Unmarshal(data, &decoded)
		require.NoError(t, err)

		assert.Equal(t, summary.TotalCost, decoded.TotalCost)
		assert.Equal(t, summary.ByNamespace["default"], decoded.ByNamespace["default"])
	})
}

func TestResourceAnalytics_Structures(t *testing.T) {
	analytics := ResourceAnalytics{
		TotalCPUCapacity:         8.0,
		TotalMemoryCapacity:      17179869184,
		TotalCPUAllocatable:      7.5,
		TotalMemoryAllocatable:   16000000000,
		TotalCPUAllocated:        2.0,
		TotalMemoryAllocated:     8000000000,
		CPUUtilizationPercent:    26.67,
		MemoryUtilizationPercent: 50.0,
		CPUByNamespace: map[string]float64{
			"default": 1.0,
		},
		MemoryByNamespace: map[string]float64{
			"default": 4000000000,
		},
		TopCPUConsumers: []ResourceConsumer{
			{Name: "test", Value: 0.5, Percent: 25.0},
		},
		TopMemoryConsumers: []ResourceConsumer{
			{Name: "test", Value: 2000000000, Percent: 25.0},
		},
	}

	data, err := json.Marshal(analytics)
	require.NoError(t, err)

	var decoded ResourceAnalytics
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, analytics.TotalCPUCapacity, decoded.TotalCPUCapacity)
	assert.Equal(t, analytics.CPUUtilizationPercent, decoded.CPUUtilizationPercent)
}

func TestCostAnalytics_Structures(t *testing.T) {
	analytics := CostAnalytics{
		TotalHourlyCost:        0.15,
		CPUHourlyCost:          0.05,
		RAMHourlyCost:          0.03,
		GPUHourlyCost:          0.0,
		StorageHourlyCost:      0.02,
		LoadBalancerHourlyCost: 0.01,
		ClusterManagementCost:  0.04,
		DailyCost:              3.6,
		MonthlyCost:            109.5,
		CostByNamespace: map[string]float64{
			"default": 0.10,
		},
		NodeCosts: []NodeCostSummary{
			{
				Node:            "node1",
				CPUHourlyCost:   0.05,
				RAMHourlyCost:   0.03,
				TotalHourlyCost: 0.15,
			},
		},
	}

	data, err := json.Marshal(analytics)
	require.NoError(t, err)

	var decoded CostAnalytics
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, analytics.TotalHourlyCost, decoded.TotalHourlyCost)
	assert.Equal(t, analytics.MonthlyCost, decoded.MonthlyCost)
	assert.Len(t, decoded.NodeCosts, 1)
}

func TestNamespaceResourceBreakdown_Structures(t *testing.T) {
	breakdown := NamespaceResourceBreakdown{
		Namespace:     "default",
		TotalCPU:      1.0,
		TotalMemory:   4000000000,
		TotalGPU:      0.0,
		CPUPercent:    12.5,
		MemoryPercent: 25.0,
		Containers: []ContainerBreakdown{
			{
				Container: "app",
				Pod:       "pod1",
				CPU:       0.5,
				Memory:    2000000000,
			},
		},
	}

	data, err := json.Marshal(breakdown)
	require.NoError(t, err)

	var decoded NamespaceResourceBreakdown
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, breakdown.Namespace, decoded.Namespace)
	assert.Len(t, decoded.Containers, 1)
}

func TestClusterResourceSummary_Structures(t *testing.T) {
	summary := ClusterResourceSummary{
		TotalNodes:        3,
		TotalCPUCores:     24.0,
		TotalMemoryGB:     48.0,
		AllocatedCPUCores: 12.0,
		AllocatedMemoryGB: 24.0,
		AvailableCPUCores: 12.0,
		AvailableMemoryGB: 24.0,
		CPUUtilization:    50.0,
		MemoryUtilization: 50.0,
		TotalPods:         50,
		TotalContainers:   75,
		TotalNamespaces:   10,
	}

	data, err := json.Marshal(summary)
	require.NoError(t, err)

	var decoded ClusterResourceSummary
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, summary.TotalNodes, decoded.TotalNodes)
	assert.Equal(t, summary.CPUUtilization, decoded.CPUUtilization)
}

// TestLiveOpenCostAPI tests against a live OpenCost instance.
// Run with: go test -v -run TestLiveOpenCostAPI -tags=integration
// Requires OpenCost to be port-forwarded to localhost:9003
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
	})

	t.Run("GetResourceAnalytics", func(t *testing.T) {
		analytics, err := client.GetResourceAnalytics(ctx)
		if err != nil {
			t.Errorf("GetResourceAnalytics failed: %v", err)
			return
		}

		t.Logf("Total CPU Capacity: %.2f cores", analytics.TotalCPUCapacity)
		t.Logf("CPU Utilization: %.2f%%", analytics.CPUUtilizationPercent)
	})

	t.Run("GetClusterResourceSummary", func(t *testing.T) {
		summary, err := client.GetClusterResourceSummary(ctx)
		if err != nil {
			t.Errorf("GetClusterResourceSummary failed: %v", err)
			return
		}

		t.Logf("Total Namespaces: %d", summary.TotalNamespaces)
		t.Logf("Total Pods: %d", summary.TotalPods)
	})

	t.Run("GetCostAnalytics", func(t *testing.T) {
		analytics, err := client.GetCostAnalytics(ctx)
		if err != nil {
			t.Errorf("GetCostAnalytics failed: %v", err)
			return
		}

		t.Logf("Total Hourly Cost: $%.4f", analytics.TotalHourlyCost)
		t.Logf("Monthly Cost (projected): $%.2f", analytics.MonthlyCost)
	})
}

// Helper test for newMetricsResponse
func TestNewMetricsResponse(t *testing.T) {
	metrics := newMetricsResponse()

	assert.NotNil(t, metrics)
	assert.NotNil(t, metrics.ContainerCPUAllocations)
	assert.NotNil(t, metrics.ContainerMemoryAllocations)
	assert.NotNil(t, metrics.ContainerGPUAllocations)
	assert.NotNil(t, metrics.PodPVCAllocations)
	assert.NotNil(t, metrics.NodeCPUHourlyCosts)
	assert.NotNil(t, metrics.NodeRAMHourlyCosts)
	assert.NotNil(t, metrics.NodeGPUHourlyCosts)
	assert.NotNil(t, metrics.NodeTotalHourlyCosts)
	assert.NotNil(t, metrics.NodeGPUCounts)
	assert.NotNil(t, metrics.PVHourlyCosts)
	assert.NotNil(t, metrics.LoadBalancerCosts)
	assert.NotNil(t, metrics.NetworkZoneEgressCosts)
	assert.NotNil(t, metrics.NetworkRegionEgressCosts)
	assert.NotNil(t, metrics.NetworkInternetEgressCosts)
	assert.NotNil(t, metrics.PodResourceRequests)
}

// Test context cancellation
func TestClient_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(Config{URL: server.URL, Timeout: 5 * time.Second})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = client.GetAllocations(ctx, AllocationRequest{Window: "7d"})
	assert.Error(t, err)
}

// Test URL parsing error
func TestClient_InvalidBaseURL(t *testing.T) {
	client, err := NewClient(Config{URL: "://invalid-url"})
	require.NoError(t, err)

	ctx := context.Background()
	_, err = client.GetAllocations(ctx, AllocationRequest{Window: "7d"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid base URL")
}

// Helper for coverage: Ensure getTopConsumers handles edge cases
func TestGetTopConsumers_EdgeCases(t *testing.T) {
	t.Run("empty allocations", func(t *testing.T) {
		consumers := getTopConsumers([]ContainerAllocation{}, 0, 5)
		assert.Empty(t, consumers)
	})

	t.Run("single allocation", func(t *testing.T) {
		allocations := []ContainerAllocation{
			{Container: "c1", Value: 1.0},
		}
		consumers := getTopConsumers(allocations, 1.0, 5)
		assert.Len(t, consumers, 1)
		assert.Equal(t, 100.0, consumers[0].Percent)
	})
}

// Ensure _ = fmt is used
var _ = fmt.Sprint
