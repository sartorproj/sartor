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

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

const delta = 1e-9

func quantity(s string) *resource.Quantity {
	if s == "" {
		return nil
	}
	q := resource.MustParse(s)
	return &q
}

// requests builds requests from CPU and memory strings; "" leaves a value
// unset, and two empty strings leave the requests unset.
func requests(cpu, memory string) autoscalingv1alpha1.ResourceRequirements {
	if cpu == "" && memory == "" {
		return autoscalingv1alpha1.ResourceRequirements{}
	}
	return autoscalingv1alpha1.ResourceRequirements{
		Requests: &autoscalingv1alpha1.ResourceValues{CPU: quantity(cpu), Memory: quantity(memory)},
	}
}

func ctr(name, curCPU, curMem, recCPU, recMem string) autoscalingv1alpha1.ContainerRecommendation {
	return autoscalingv1alpha1.ContainerRecommendation{
		Name:        name,
		Current:     requests(curCPU, curMem),
		Recommended: requests(recCPU, recMem),
	}
}

func replicas(n int32) *int32 { return &n }

// analyzed returns a Tailoring of Deployment name in namespace with a last
// analysis.
func analyzed(namespace, name, cluster string, reps *int32, containers ...autoscalingv1alpha1.ContainerRecommendation) autoscalingv1alpha1.Tailoring {
	return autoscalingv1alpha1.Tailoring{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: autoscalingv1alpha1.TailoringSpec{
			Target: autoscalingv1alpha1.TargetRef{Kind: "Deployment", Name: name, Cluster: cluster},
		},
		Status: autoscalingv1alpha1.TailoringStatus{
			LastAnalysis: &autoscalingv1alpha1.AnalysisResult{Replicas: reps, Containers: containers},
		},
	}
}

func notAnalyzed(namespace, name string) autoscalingv1alpha1.Tailoring {
	t := analyzed(namespace, name, "", nil)
	t.Status.LastAnalysis = nil
	return t
}

func TestBuildSavingsReport_Totals(t *testing.T) {
	type want struct {
		cpuReclaim, cpuIncrease, memReclaim, memIncrease float64
		counts                                           SavingsCounts
	}
	tests := []struct {
		name  string
		items []autoscalingv1alpha1.Tailoring
		want  want
	}{
		{
			name: "zero tailorings",
			want: want{},
		},
		{
			name:  "no last analysis",
			items: []autoscalingv1alpha1.Tailoring{notAnalyzed("example-namespace", "my-app")},
			want:  want{counts: SavingsCounts{Total: 1, SkippedNoAnalysis: 1}},
		},
		{
			name: "replicas unset is skipped",
			items: []autoscalingv1alpha1.Tailoring{
				analyzed("example-namespace", "my-app", "", nil, ctr("app", "1", "1Gi", "500m", "512Mi")),
			},
			want: want{counts: SavingsCounts{Total: 1, SkippedNoReplicas: 1}},
		},
		{
			name: "replicas 0 is included with no savings",
			items: []autoscalingv1alpha1.Tailoring{
				analyzed("example-namespace", "my-app", "", replicas(0), ctr("app", "1", "1Gi", "500m", "512Mi")),
			},
			want: want{counts: SavingsCounts{Total: 1, Included: 1}},
		},
		{
			name: "negative replicas are treated as unset",
			items: []autoscalingv1alpha1.Tailoring{
				analyzed("example-namespace", "my-app", "", replicas(-1), ctr("app", "1", "1Gi", "500m", "512Mi")),
			},
			want: want{counts: SavingsCounts{Total: 1, SkippedNoReplicas: 1}},
		},
		{
			name: "missing current CPU request only drops CPU",
			items: []autoscalingv1alpha1.Tailoring{
				analyzed("example-namespace", "my-app", "", replicas(2), ctr("app", "", "1Gi", "500m", "512Mi")),
			},
			want: want{memReclaim: 1, counts: SavingsCounts{Total: 1, Included: 1}},
		},
		{
			name: "no current requests at all",
			items: []autoscalingv1alpha1.Tailoring{
				analyzed("example-namespace", "my-app", "", replicas(2), ctr("app", "", "", "500m", "512Mi")),
			},
			want: want{counts: SavingsCounts{Total: 1, SkippedNoCurrentRequest: 1}},
		},
		{
			name: "missing recommended request is not a saving",
			items: []autoscalingv1alpha1.Tailoring{
				analyzed("example-namespace", "my-app", "", replicas(2), ctr("app", "1", "1Gi", "", "")),
			},
			want: want{counts: SavingsCounts{Total: 1, SkippedNoCurrentRequest: 1}},
		},
		{
			name: "no containers",
			items: []autoscalingv1alpha1.Tailoring{
				analyzed("example-namespace", "my-app", "", replicas(2)),
			},
			want: want{counts: SavingsCounts{Total: 1, SkippedNoCurrentRequest: 1}},
		},
		{
			name: "recommended above current is an increase, not reclaimable",
			items: []autoscalingv1alpha1.Tailoring{
				analyzed("example-namespace", "my-app", "", replicas(3), ctr("app", "100m", "256Mi", "300m", "1Gi")),
			},
			want: want{cpuIncrease: 0.6, memIncrease: 2.25, counts: SavingsCounts{Total: 1, Included: 1}},
		},
		{
			name: "mixed units",
			items: []autoscalingv1alpha1.Tailoring{
				analyzed("example-namespace", "my-app", "", replicas(1),
					ctr("cores-vs-milli", "1.5", "2Gi", "500m", "1048576Ki"),
					ctr("milli-vs-cores", "2000m", "1536Mi", "1.5", "0.5Gi"),
					ctr("plain-bytes", "250m", "1073741824", "250m", "512Mi"),
				),
			},
			want: want{cpuReclaim: 1.5, memReclaim: 2.5, counts: SavingsCounts{Total: 1, Included: 1}},
		},
		{
			name: "multiple containers with opposite changes are not netted",
			items: []autoscalingv1alpha1.Tailoring{
				analyzed("example-namespace", "my-app", "", replicas(2),
					ctr("app", "1", "1Gi", "250m", "768Mi"),
					ctr("sidecar", "100m", "64Mi", "200m", "128Mi"),
				),
			},
			want: want{cpuReclaim: 1.5, cpuIncrease: 0.2, memReclaim: 0.5, memIncrease: 0.125, counts: SavingsCounts{Total: 1, Included: 1}},
		},
		{
			name: "invalid quantities are ignored",
			items: []autoscalingv1alpha1.Tailoring{
				analyzed("example-namespace", "my-app", "", replicas(1),
					ctr("negative", "-1", "-1Gi", "100m", "128Mi"),
					ctr("huge", "1e30", "1e30", "100m", "128Mi"),
					ctr("valid", "200m", "256Mi", "100m", "128Mi"),
				),
			},
			want: want{cpuReclaim: 0.1, memReclaim: 0.125, counts: SavingsCounts{Total: 1, Included: 1}},
		},
		{
			name: "mixed tailorings",
			items: []autoscalingv1alpha1.Tailoring{
				notAnalyzed("example-namespace", "pending"),
				analyzed("example-namespace", "no-replicas", "", nil, ctr("app", "1", "1Gi", "500m", "512Mi")),
				analyzed("example-namespace", "no-requests", "", replicas(1), ctr("app", "", "", "500m", "512Mi")),
				analyzed("example-namespace", "my-app", "", replicas(4), ctr("app", "1", "1Gi", "500m", "512Mi")),
			},
			want: want{cpuReclaim: 2, memReclaim: 2, counts: SavingsCounts{
				Total: 4, Included: 1, SkippedNoAnalysis: 1, SkippedNoReplicas: 1, SkippedNoCurrentRequest: 1,
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := buildSavingsReport(tt.items, SavingsPricing{})
			assert.Equal(t, tt.want.counts, r.Tailorings)
			assert.InDelta(t, tt.want.cpuReclaim, r.Totals.CPU.ReclaimableCores, delta)
			assert.InDelta(t, tt.want.cpuIncrease, r.Totals.CPU.IncreaseCores, delta)
			assert.InDelta(t, tt.want.cpuReclaim-tt.want.cpuIncrease, r.Totals.CPU.NetCores, delta)
			assert.InDelta(t, tt.want.memReclaim, r.Totals.Memory.ReclaimableGiB, delta)
			assert.InDelta(t, tt.want.memIncrease, r.Totals.Memory.IncreaseGiB, delta)
			assert.InDelta(t, tt.want.memReclaim-tt.want.memIncrease, r.Totals.Memory.NetGiB, delta)
			assert.Nil(t, r.Totals.Cost, "no cost without a price")
			assert.Nil(t, r.Pricing)
			assert.NotNil(t, r.ByCluster)
			assert.NotNil(t, r.ByNamespace)
			assert.Len(t, r.ByCluster, min(r.Tailorings.Included, 1))
		})
	}
}

func TestBuildSavingsReport_Groups(t *testing.T) {
	overridden := analyzed("example-namespace", "other-app", "example-cluster", replicas(1), ctr("app", "1", "1Gi", "500m", "512Mi"))
	overridden.Spec.Target.Namespace = "workload-namespace"
	items := []autoscalingv1alpha1.Tailoring{
		analyzed("example-namespace", "no-cluster", "", replicas(1), ctr("app", "1", "1Gi", "500m", "512Mi")),
		analyzed("example-namespace", "my-app", "example-cluster", replicas(2), ctr("app", "1", "1Gi", "500m", "512Mi")),
		analyzed("another-namespace", "my-app", "example-cluster", replicas(1), ctr("app", "100m", "128Mi", "200m", "256Mi")),
		overridden,
		notAnalyzed("example-namespace", "pending"),
	}

	r := buildSavingsReport(items, SavingsPricing{})

	type group struct {
		cluster, namespace string
		tailorings         int
		cpuReclaim, cpuInc float64
	}
	got := func(gs []SavingsGroup) []group {
		out := make([]group, 0, len(gs))
		for _, g := range gs {
			out = append(out, group{g.Cluster, g.Namespace, g.Tailorings, g.CPU.ReclaimableCores, g.CPU.IncreaseCores})
		}
		return out
	}
	assert.Equal(t, []group{
		{cluster: "", tailorings: 1, cpuReclaim: 0.5},
		{cluster: "example-cluster", tailorings: 3, cpuReclaim: 1.5, cpuInc: 0.1},
	}, got(r.ByCluster))
	assert.Equal(t, []group{
		{cluster: "", namespace: "example-namespace", tailorings: 1, cpuReclaim: 0.5},
		{cluster: "example-cluster", namespace: "another-namespace", tailorings: 1, cpuInc: 0.1},
		{cluster: "example-cluster", namespace: "example-namespace", tailorings: 1, cpuReclaim: 1},
		{cluster: "example-cluster", namespace: "workload-namespace", tailorings: 1, cpuReclaim: 0.5},
	}, got(r.ByNamespace), "grouped by the workload namespace")
	assert.Equal(t, SavingsCounts{Total: 5, Included: 4, SkippedNoAnalysis: 1}, r.Tailorings)
}

func TestBuildSavingsReport_Pricing(t *testing.T) {
	cpuPrice, memPrice := 0.04, 0.005
	items := []autoscalingv1alpha1.Tailoring{
		analyzed("example-namespace", "my-app", "example-cluster", replicas(2),
			ctr("app", "1500m", "3Gi", "500m", "1Gi"),
			ctr("sidecar", "100m", "128Mi", "600m", "128Mi"),
		),
	}
	tests := []struct {
		name                         string
		pricing                      SavingsPricing
		reclaim, increase, netPerHrs float64
	}{
		// 2 cores and 4 GiB reclaimable, 1 core increase.
		{name: "both prices", pricing: SavingsPricing{CPUPricePerCoreHour: &cpuPrice, MemPricePerGiBHour: &memPrice},
			reclaim: 0.1, increase: 0.04, netPerHrs: 0.06},
		{name: "cpu price only", pricing: SavingsPricing{CPUPricePerCoreHour: &cpuPrice}, reclaim: 0.08, increase: 0.04, netPerHrs: 0.04},
		{name: "memory price only", pricing: SavingsPricing{MemPricePerGiBHour: &memPrice}, reclaim: 0.02, netPerHrs: 0.02},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := buildSavingsReport(items, tt.pricing)
			require.NotNil(t, r.Pricing)
			assert.Equal(t, tt.pricing, *r.Pricing)
			for _, a := range []SavingsAmounts{r.Totals, r.ByCluster[0].SavingsAmounts, r.ByNamespace[0].SavingsAmounts} {
				require.NotNil(t, a.Cost)
				assert.InDelta(t, tt.reclaim, a.Cost.ReclaimablePerHour, delta)
				assert.InDelta(t, tt.increase, a.Cost.IncreasePerHour, delta)
				assert.InDelta(t, tt.netPerHrs, a.Cost.NetPerHour, delta)
			}
		})
	}
}

func TestBuildTopGainsReport(t *testing.T) {
	items := []autoscalingv1alpha1.Tailoring{
		analyzed("example-namespace", "b-app", "example-cluster", replicas(2),
			ctr("app", "1", "1Gi", "500m", "512Mi"),          // +1 core, +1 GiB
			ctr("sidecar", "100m", "", "200m", "64Mi"),       // CPU increase, no memory current
			ctr("unchanged", "100m", "64Mi", "100m", "64Mi"), // nothing to reclaim
		),
		analyzed("example-namespace", "a-app", "example-cluster", replicas(1),
			ctr("app", "1500m", "4Gi", "500m", "1Gi"), // +1 core (tie with b-app/app), +3 GiB
		),
		analyzed("other-namespace", "c-app", "", replicas(5),
			ctr("app", "400m", "256Mi", "100m", "512Mi"), // +1.5 cores, memory increase
		),
		analyzed("other-namespace", "no-replicas", "", nil, ctr("app", "8", "8Gi", "1", "1Gi")),
		notAnalyzed("other-namespace", "pending"),
	}

	t.Run("ranking", func(t *testing.T) {
		r := buildTopGainsReport(items, 20)
		assert.Equal(t, 20, r.Limit)
		assert.Equal(t, SavingsCounts{Total: 5, Included: 3, SkippedNoAnalysis: 1, SkippedNoReplicas: 1}, r.Tailorings)

		names := func(gs []ContainerGain) []string {
			out := make([]string, 0, len(gs))
			for _, g := range gs {
				out = append(out, g.Namespace+"/"+g.Tailoring+"/"+g.Container)
			}
			return out
		}
		assert.Equal(t, []string{
			"other-namespace/c-app/app",
			"example-namespace/a-app/app",
			"example-namespace/b-app/app",
		}, names(r.ByCPU), "largest first, ties by namespace, Tailoring and container; increases left out")
		assert.Equal(t, []string{
			"example-namespace/a-app/app",
			"example-namespace/b-app/app",
		}, names(r.ByMemory))

		top := r.ByCPU[0]
		assert.Equal(t, "", top.Cluster)
		assert.Equal(t, "Deployment", top.WorkloadKind)
		assert.Equal(t, "c-app", top.WorkloadName)
		assert.Equal(t, "other-namespace", top.WorkloadNamespace)
		assert.Equal(t, int32(5), top.Replicas)
		assert.Equal(t, "400m", top.CurrentRequests.CPU.String())
		assert.Equal(t, "100m", top.RecommendedRequests.CPU.String())
		require.NotNil(t, top.CPUSavingCores)
		assert.InDelta(t, 1.5, *top.CPUSavingCores, delta)
		require.NotNil(t, top.MemorySavingGiB)
		assert.InDelta(t, -1.25, *top.MemorySavingGiB, delta, "the other resource keeps its sign")

		mem := r.ByMemory[0]
		assert.Equal(t, "example-cluster", mem.Cluster)
		require.NotNil(t, mem.MemorySavingGiB)
		assert.InDelta(t, 3, *mem.MemorySavingGiB, delta)
	})

	t.Run("limit", func(t *testing.T) {
		r := buildTopGainsReport(items, 1)
		assert.Len(t, r.ByCPU, 1)
		assert.Len(t, r.ByMemory, 1)
		assert.Equal(t, "c-app", r.ByCPU[0].Tailoring)
	})

	t.Run("missing memory request leaves the memory saving unset", func(t *testing.T) {
		only := []autoscalingv1alpha1.Tailoring{analyzed("example-namespace", "my-app", "", replicas(1), ctr("app", "1", "", "500m", "64Mi"))}
		r := buildTopGainsReport(only, 20)
		require.Len(t, r.ByCPU, 1)
		assert.Nil(t, r.ByCPU[0].MemorySavingGiB)
		assert.Empty(t, r.ByMemory)
	})

	t.Run("zero tailorings", func(t *testing.T) {
		r := buildTopGainsReport(nil, 20)
		assert.NotNil(t, r.ByCPU)
		assert.NotNil(t, r.ByMemory)
		assert.Empty(t, r.ByCPU)
		assert.Empty(t, r.ByMemory)
	})
}

func TestParseTopGainsLimit(t *testing.T) {
	tests := []struct {
		query   string
		want    int
		wantErr bool
	}{
		{query: "", want: 20},
		{query: "limit=1", want: 1},
		{query: "limit=200", want: 200},
		{query: "limit=0", wantErr: true},
		{query: "limit=201", wantErr: true},
		{query: "limit=-5", wantErr: true},
		{query: "limit=abc", wantErr: true},
		{query: "limit=1.5", wantErr: true},
		{query: "limit=1e2", wantErr: true},
		{query: "limit=", wantErr: true},
		{query: "limit=%205", wantErr: true},
		{query: "limit=99999999999999999999", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			require.NoError(t, err)
			got, err := parseTopGainsLimit(q)
			if tt.wantErr {
				assert.ErrorContains(t, err, "must be an integer from 1 to 200")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParsePrice(t *testing.T) {
	tests := []struct {
		query   string
		want    *float64
		wantErr bool
	}{
		{query: ""},
		{query: "p=0", want: ptrFloat(0)},
		{query: "p=0.031", want: ptrFloat(0.031)},
		{query: "p=1e-3", want: ptrFloat(0.001)},
		{query: "p=abc", wantErr: true},
		{query: "p=-0.01", wantErr: true},
		{query: "p=NaN", wantErr: true},
		{query: "p=Inf", wantErr: true},
		{query: "p=-Inf", wantErr: true},
		{query: "p=1e400", wantErr: true},
		{query: "p=", wantErr: true},
		{query: "p=1,5", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			require.NoError(t, err)
			got, err := parsePrice(q, "p")
			if tt.wantErr {
				assert.ErrorContains(t, err, "must be a non-negative number")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func ptrFloat(v float64) *float64 { return &v }

func failingListClient() client.Client {
	scheme := runtime.NewScheme()
	_ = autoscalingv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("list failed")
		},
	}).Build()
}

func TestAnalyticsEndpoints_StatusCodes(t *testing.T) {
	tl := analyzed("example-namespace", "my-app", "example-cluster", replicas(2), ctr("app", "1", "1Gi", "500m", "512Mi"))
	tests := []struct {
		name   string
		method string
		target string
		client client.Client
		want   int
	}{
		{name: "savings", target: "/api/v1/analytics/savings", want: http.StatusOK},
		{name: "savings with prices", target: "/api/v1/analytics/savings?cpuPricePerCoreHour=0.03&memPricePerGiBHour=0.004", want: http.StatusOK},
		{name: "savings with zero price", target: "/api/v1/analytics/savings?cpuPricePerCoreHour=0", want: http.StatusOK},
		{name: "savings ignores unknown parameters", target: "/api/v1/analytics/savings?foo=bar", want: http.StatusOK},
		{name: "savings non-numeric cpu price", target: "/api/v1/analytics/savings?cpuPricePerCoreHour=cheap", want: http.StatusBadRequest},
		{name: "savings negative memory price", target: "/api/v1/analytics/savings?memPricePerGiBHour=-1", want: http.StatusBadRequest},
		{name: "savings NaN price", target: "/api/v1/analytics/savings?memPricePerGiBHour=NaN", want: http.StatusBadRequest},
		{name: "savings empty price", target: "/api/v1/analytics/savings?cpuPricePerCoreHour=", want: http.StatusBadRequest},
		{name: "savings POST", method: http.MethodPost, target: "/api/v1/analytics/savings", want: http.StatusMethodNotAllowed},
		{name: "savings list error", target: "/api/v1/analytics/savings", client: failingListClient(), want: http.StatusInternalServerError},
		{name: "top gains", target: "/api/v1/analytics/top-gains", want: http.StatusOK},
		{name: "top gains limit", target: "/api/v1/analytics/top-gains?limit=200", want: http.StatusOK},
		{name: "top gains limit too large", target: "/api/v1/analytics/top-gains?limit=201", want: http.StatusBadRequest},
		{name: "top gains limit zero", target: "/api/v1/analytics/top-gains?limit=0", want: http.StatusBadRequest},
		{name: "top gains limit non-numeric", target: "/api/v1/analytics/top-gains?limit=ten", want: http.StatusBadRequest},
		{name: "top gains DELETE", method: http.MethodDelete, target: "/api/v1/analytics/top-gains", want: http.StatusMethodNotAllowed},
		{name: "top gains list error", target: "/api/v1/analytics/top-gains", client: failingListClient(), want: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.client
			if c == nil {
				c = newFakeClient(&tl)
			}
			mux := http.NewServeMux()
			NewHandler(c).RegisterRoutes(mux)
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(method, tt.target, nil))

			assert.Equal(t, tt.want, rec.Code, rec.Body.String())
			var resp Response
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp), "always a JSON envelope")
			assert.Equal(t, tt.want == http.StatusOK, resp.Success)
			if tt.want != http.StatusOK {
				assert.NotEmpty(t, resp.Error)
			}
		})
	}
}

func TestGetSavings_Response(t *testing.T) {
	t.Run("zero tailorings", func(t *testing.T) {
		rec := httptest.NewRecorder()
		NewHandler(newFakeClient()).GetSavings(rec, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/savings", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		assert.Contains(t, body, `"byCluster":[]`)
		assert.Contains(t, body, `"byNamespace":[]`)
		assert.Contains(t, body, `"tailorings":{"total":0,"included":0,"skippedNoAnalysis":0,"skippedNoReplicas":0,"skippedNoCurrentRequest":0}`)
		assert.NotContains(t, body, `"pricing"`)
		assert.NotContains(t, body, `"cost"`)
	})

	t.Run("with prices", func(t *testing.T) {
		tl := analyzed("example-namespace", "my-app", "example-cluster", replicas(2), ctr("app", "1", "1Gi", "500m", "512Mi"))
		pending := notAnalyzed("example-namespace", "pending")
		rec := httptest.NewRecorder()
		NewHandler(newFakeClient(&tl, &pending)).GetSavings(rec, httptest.NewRequest(http.MethodGet,
			"/api/v1/analytics/savings?cpuPricePerCoreHour=0.03&memPricePerGiBHour=0.004", nil))
		require.Equal(t, http.StatusOK, rec.Code)

		var resp struct {
			Success bool          `json:"success"`
			Data    SavingsReport `json:"data"`
		}
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		r := resp.Data
		assert.InDelta(t, 1, r.Totals.CPU.ReclaimableCores, delta)
		assert.InDelta(t, 1, r.Totals.Memory.ReclaimableGiB, delta)
		require.NotNil(t, r.Totals.Cost)
		assert.InDelta(t, 0.034, r.Totals.Cost.ReclaimablePerHour, delta)
		require.Len(t, r.ByCluster, 1)
		assert.Equal(t, "example-cluster", r.ByCluster[0].Cluster)
		require.Len(t, r.ByNamespace, 1)
		assert.Equal(t, "example-namespace", r.ByNamespace[0].Namespace)
		assert.Equal(t, SavingsCounts{Total: 2, Included: 1, SkippedNoAnalysis: 1}, r.Tailorings)
		require.NotNil(t, r.Pricing)
		assert.InDelta(t, 0.03, *r.Pricing.CPUPricePerCoreHour, delta)
	})
}

func TestGetTopGains_Response(t *testing.T) {
	tl := analyzed("example-namespace", "my-app", "example-cluster", replicas(3), ctr("app", "1", "1Gi", "500m", "512Mi"))
	rec := httptest.NewRecorder()
	NewHandler(newFakeClient(&tl)).GetTopGains(rec, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/top-gains", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data struct {
			Limit int                      `json:"limit"`
			ByCPU []map[string]interface{} `json:"byCPU"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 20, resp.Data.Limit, "default limit")
	require.Len(t, resp.Data.ByCPU, 1)
	assert.Equal(t, map[string]interface{}{
		"cluster":             "example-cluster",
		"namespace":           "example-namespace",
		"tailoring":           "my-app",
		"workloadKind":        "Deployment",
		"workloadName":        "my-app",
		"workloadNamespace":   "example-namespace",
		"container":           "app",
		"replicas":            3.0,
		"currentRequests":     map[string]interface{}{"cpu": "1", "memory": "1Gi"},
		"recommendedRequests": map[string]interface{}{"cpu": "500m", "memory": "512Mi"},
		"cpuSavingCores":      1.5,
		"memorySavingGiB":     1.5,
	}, resp.Data.ByCPU[0])
}
