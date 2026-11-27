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
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

const testMiB = 1024 * 1024

// usageFunc returns the CPU usage in cores at sample i of a range query, and
// false for a missing sample. Memory is served as the same shape times 512 MiB.
type usageFunc func(i int) (float64, bool)

var testNoise = func() []float64 {
	r := rand.New(rand.NewSource(1))
	noise := make([]float64, 20000)
	for i := range noise {
		noise[i] = r.NormFloat64()
	}
	return noise
}()

// dailyUsage is a daily cycle at 5m samples with noise.
func dailyUsage(i int) (float64, bool) {
	return 1 + 0.5*math.Sin(2*math.Pi*float64(i)/288) + 0.05*testNoise[i%len(testNoise)], true
}

// fakeForecastProm serves usage for container "app" on range queries and
// records every request. delay holds each request until it passes or the
// request is cancelled.
type fakeForecastProm struct {
	*httptest.Server
	mu       sync.Mutex
	requests []url.Values
	usage    usageFunc
	delay    time.Duration
}

func newFakeForecastProm(t *testing.T, usage usageFunc) *fakeForecastProm {
	t.Helper()
	p := &fakeForecastProm{usage: usage}
	p.Server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.Close)
	return p
}

func (p *fakeForecastProm) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	p.requests = append(p.requests, r.Form)
	p.mu.Unlock()
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if !strings.HasSuffix(r.URL.Path, "query_range") {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		return
	}
	start, _ := strconv.ParseFloat(r.FormValue("start"), 64)
	end, _ := strconv.ParseFloat(r.FormValue("end"), 64)
	step, _ := strconv.ParseFloat(r.FormValue("step"), 64)
	scale := 1.0
	if strings.Contains(r.FormValue("query"), "container_memory_working_set_bytes") {
		scale = 512 * testMiB
	}
	values := [][]interface{}{}
	n := -1
	if step > 0 {
		n = int(math.Round((end - start) / step))
	}
	for i := 0; i <= n; i++ {
		v, ok := p.usage(i)
		if !ok {
			continue
		}
		values = append(values, []interface{}{start + float64(i)*step, strconv.FormatFloat(v*scale, 'f', -1, 64)})
	}
	result := []interface{}{}
	if len(values) > 0 {
		result = append(result, map[string]interface{}{"metric": map[string]string{"container": "app"}, "values": values})
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   map[string]interface{}{"resultType": "matrix", "result": result},
	})
}

func (p *fakeForecastProm) rangeRequests() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []url.Values
	for _, r := range p.requests {
		if r.Get("step") != "" {
			out = append(out, r)
		}
	}
	return out
}

func (p *fakeForecastProm) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func newForecastHandler(promURL, clusterLabel string) *Handler {
	atelier := &autoscalingv1alpha1.Atelier{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: autoscalingv1alpha1.AtelierSpec{Prometheus: autoscalingv1alpha1.PrometheusConfig{
			URL: promURL, ClusterLabel: clusterLabel,
		}},
	}
	return NewHandler(newFakeClient(atelier))
}

type forecastBody struct {
	Success bool             `json:"success"`
	Error   string           `json:"error"`
	Data    ForecastResponse `json:"data"`
}

func forecastParams(overrides ...string) url.Values {
	v := url.Values{"namespace": {"example"}, "name": {"api"}, "containerName": {"app"}, "resourceType": {"cpu"}}
	for i := 0; i+1 < len(overrides); i += 2 {
		if overrides[i+1] == "" {
			v.Del(overrides[i])
		} else {
			v.Set(overrides[i], overrides[i+1])
		}
	}
	return v
}

func getForecast(t *testing.T, h *Handler, ctx context.Context, params url.Values) (int, forecastBody) {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prometheus/forecast?"+params.Encode(), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var body forecastBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return rec.Code, body
}

func TestGetPrometheusForecast_Validation(t *testing.T) {
	prom := newFakeForecastProm(t, dailyUsage)
	h := newForecastHandler(prom.URL, "")

	tests := []struct {
		name   string
		params url.Values
		err    string
	}{
		{"missing namespace", forecastParams("namespace", ""), "missing required parameters"},
		{"missing name", forecastParams("name", ""), "missing required parameters"},
		{"missing container", forecastParams("containerName", ""), "missing required parameters"},
		{"missing resource type", forecastParams("resourceType", ""), "missing required parameters"},
		{"unsupported resource", forecastParams("resourceType", "disk"), "resourceType must be cpu or memory"},
		{"unsupported kind", forecastParams("kind", "CronJob"), "unsupported workload kind"},
		{"injection in namespace", forecastParams("namespace", `example"} or vector(1) or up{a="`), "invalid namespace"},
		{"regex in name", forecastParams("name", "back.*"), "invalid workload name"},
		{"injection in container", forecastParams("containerName", `app",container!="`), "invalid container name"},
		{"unparsable window", forecastParams("window", "7d"), "invalid window duration"},
		{"zero window", forecastParams("window", "0s"), "window must be between"},
		{"negative window", forecastParams("window", "-168h"), "window must be between"},
		{"window too large", forecastParams("window", "745h"), "window must be between"},
		{"overflowing window", forecastParams("window", "9999999999h"), "invalid window duration"},
		{"unparsable horizon", forecastParams("horizon", "1d"), "invalid horizon duration"},
		{"zero horizon", forecastParams("horizon", "0"), "horizon must be positive"},
		{"negative horizon", forecastParams("horizon", "-1h"), "horizon must be positive"},
		{"horizon too large", forecastParams("horizon", "169h", "window", "744h"), "at most 168h0m0s"},
		{"window shorter than horizon", forecastParams("window", "12h"), "must not be shorter than horizon"},
		{"zero confidence", forecastParams("confidence", "0"), "confidence must be"},
		{"confidence of one", forecastParams("confidence", "1"), "confidence must be"},
		{"confidence above one", forecastParams("confidence", "95"), "confidence must be"},
		{"negative confidence", forecastParams("confidence", "-0.5"), "confidence must be"},
		{"NaN confidence", forecastParams("confidence", "NaN"), "confidence must be"},
		{"infinite confidence", forecastParams("confidence", "Inf"), "confidence must be"},
		{"unparsable confidence", forecastParams("confidence", "high"), "invalid confidence"},
		{"invalid mode", forecastParams("mode", "best"), "mode must be"},
		{"empty mode", url.Values{"namespace": {"example"}, "name": {"api"}, "containerName": {"app"}, "resourceType": {"cpu"}, "mode": {""}}, "mode must be"},
		{"fixed without order", forecastParams("mode", "fixed"), "requires order"},
		{"order without fixed mode", forecastParams("order", "1,1,1"), "order requires mode 'fixed'"},
		{"short order", forecastParams("mode", "fixed", "order", "1,1"), "order must have 3 elements"},
		{"non-numeric order", forecastParams("mode", "fixed", "order", "a,b,c"), "order[0]"},
		{"fractional order", forecastParams("mode", "fixed", "order", "1.5,0,0"), "order[0]"},
		{"order out of range", forecastParams("mode", "fixed", "order", "1,9,1"), "order[1]"},
		{"seasonal order without period", forecastParams("mode", "fixed", "order", "1,0,0", "seasonalOrder", "1,0,0"), "requires seasonalPeriod"},
		{"invalid seasonal flag", forecastParams("seasonal", "maybe"), "seasonal must be a boolean"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := prom.requestCount()
			status, body := getForecast(t, h, context.Background(), tt.params)
			assert.Equal(t, http.StatusBadRequest, status)
			assert.False(t, body.Success)
			assert.Contains(t, body.Error, tt.err)
			assert.Equal(t, before, prom.requestCount(), "rejected input must not reach Prometheus")
		})
	}

	t.Run("method not allowed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.GetPrometheusForecast(rec, httptest.NewRequest(http.MethodPost, "/api/v1/prometheus/forecast?"+forecastParams().Encode(), nil))
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})
}

func assertForecastPoints(t *testing.T, body forecastBody, n int, interval time.Duration) {
	t.Helper()
	require.True(t, body.Success, body.Error)
	require.Empty(t, body.Data.FallbackReason)
	require.Len(t, body.Data.Forecast, n)
	assert.NotEmpty(t, body.Data.Model)
	assert.Equal(t, interval.String(), body.Data.SampleInterval)
	last := body.Data.History[len(body.Data.History)-1].Timestamp
	for i, p := range body.Data.Forecast {
		assert.LessOrEqual(t, p.Lower, p.Value, "point %d", i)
		assert.LessOrEqual(t, p.Value, p.Upper, "point %d", i)
		assert.GreaterOrEqual(t, p.Lower, 0.0, "point %d", i)
		assert.Greater(t, p.Timestamp, last, "forecast point %d must follow the history", i)
		if i > 0 {
			assert.Equal(t, int64(interval/time.Second), p.Timestamp-body.Data.Forecast[i-1].Timestamp)
		}
	}
}

func TestGetPrometheusForecast_CPU(t *testing.T) {
	prom := newFakeForecastProm(t, dailyUsage)
	status, body := getForecast(t, newForecastHandler(prom.URL, ""), context.Background(), forecastParams())
	require.Equal(t, http.StatusOK, status, body.Error)

	// 7 days at the strategy's 5m resolution, fitted at 15m, forecast for 24h.
	assertForecastPoints(t, body, 96, 15*time.Minute)
	assert.Equal(t, 0.95, body.Data.Level)
	assert.Len(t, body.Data.History, 7*288+1)
	assert.InDelta(t, 1.0, body.Data.History[0].Value, 0.2, "CPU is returned in cores")
	for _, p := range body.Data.Forecast {
		assert.Less(t, p.Upper, 3.0)
		assert.Greater(t, p.Upper, 0.5)
	}

	ranges := prom.rangeRequests()
	require.Len(t, ranges, 2, "one CPU and one memory history query")
	for _, r := range ranges {
		assert.Equal(t, "300", r.Get("step"))
		assert.Contains(t, r.Get("query"), `namespace="example"`)
	}
}

func TestGetPrometheusForecast_MemoryInMiB(t *testing.T) {
	prom := newFakeForecastProm(t, dailyUsage)
	h := newForecastHandler(prom.URL, "")
	_, cpu := getForecast(t, h, context.Background(), forecastParams())
	status, mem := getForecast(t, h, context.Background(), forecastParams("resourceType", "memory"))
	require.Equal(t, http.StatusOK, status, mem.Error)
	assertForecastPoints(t, mem, 96, 15*time.Minute)

	// Memory is served as the CPU shape times 512 MiB, so in MiB it is the
	// CPU series times 512.
	require.Len(t, mem.Data.History, len(cpu.Data.History))
	for i := range mem.Data.History {
		assert.InDelta(t, cpu.Data.History[i].Value*512, mem.Data.History[i].Value, 1e-6)
	}
	assert.Equal(t, cpu.Data.Model, mem.Data.Model)
	for i := range mem.Data.Forecast {
		assert.InEpsilon(t, cpu.Data.Forecast[i].Upper*512, mem.Data.Forecast[i].Upper, 1e-3)
		assert.InEpsilon(t, cpu.Data.Forecast[i].Value*512, mem.Data.Forecast[i].Value, 1e-3)
	}
}

func TestGetPrometheusForecast_Options(t *testing.T) {
	prom := newFakeForecastProm(t, dailyUsage)
	h := newForecastHandler(prom.URL, "")

	t.Run("longest window and horizon", func(t *testing.T) {
		status, body := getForecast(t, h, context.Background(), forecastParams("window", "744h", "horizon", "168h"))
		require.Equal(t, http.StatusOK, status, body.Error)
		// 31 days at 5m (8928 samples) are fitted at 45m.
		assertForecastPoints(t, body, 224, 45*time.Minute)
		assert.LessOrEqual(t, len(body.Data.History), 10000)
	})
	t.Run("horizon equal to window", func(t *testing.T) {
		status, body := getForecast(t, h, context.Background(), forecastParams("window", "24h", "horizon", "24h"))
		require.Equal(t, http.StatusOK, status, body.Error)
		assertForecastPoints(t, body, 288, 5*time.Minute)
	})
	t.Run("odd durations", func(t *testing.T) {
		status, body := getForecast(t, h, context.Background(), forecastParams("window", "100h30m", "horizon", "1h1m1s"))
		require.Equal(t, http.StatusOK, status, body.Error)
		require.Empty(t, body.Data.FallbackReason)
		assert.NotEmpty(t, body.Data.Forecast)
	})
	t.Run("tiny horizon", func(t *testing.T) {
		status, body := getForecast(t, h, context.Background(), forecastParams("horizon", "1ns"))
		require.Equal(t, http.StatusOK, status, body.Error)
		assertForecastPoints(t, body, 1, 15*time.Minute)
	})
	t.Run("custom confidence", func(t *testing.T) {
		_, narrow := getForecast(t, h, context.Background(), forecastParams("confidence", "0.5"))
		_, wide := getForecast(t, h, context.Background(), forecastParams("confidence", "0.999"))
		assert.Equal(t, 0.5, narrow.Data.Level)
		assert.Equal(t, 0.999, wide.Data.Level)
		for i := range narrow.Data.Forecast {
			assert.Less(t, narrow.Data.Forecast[i].Upper, wide.Data.Forecast[i].Upper)
		}
	})
	t.Run("fixed order", func(t *testing.T) {
		status, body := getForecast(t, h, context.Background(), forecastParams("mode", "fixed", "order", "2, 0, 1"))
		require.Equal(t, http.StatusOK, status, body.Error)
		assert.Equal(t, "ARIMA(2,0,1)", body.Data.Model)
	})
	t.Run("fixed seasonal order", func(t *testing.T) {
		status, body := getForecast(t, h, context.Background(),
			forecastParams("mode", "fixed", "order", "1,0,0", "seasonalOrder", "0,1,1", "seasonalPeriod", "24h"))
		require.Equal(t, http.StatusOK, status, body.Error)
		assert.Equal(t, "SARIMA(1,0,0)(0,1,1)[96]", body.Data.Model)
	})
	t.Run("non-seasonal auto", func(t *testing.T) {
		status, body := getForecast(t, h, context.Background(), forecastParams("seasonal", "false"))
		require.Equal(t, http.StatusOK, status, body.Error)
		assert.True(t, strings.HasPrefix(body.Data.Model, "ARIMA("), body.Data.Model)
	})
	t.Run("statefulset with legacy parameter", func(t *testing.T) {
		status, body := getForecast(t, h, context.Background(), forecastParams("name", "", "podNamePrefix", "api", "kind", "StatefulSet"))
		require.Equal(t, http.StatusOK, status, body.Error)
		assert.NotEmpty(t, body.Data.Forecast)
	})
}

func TestGetPrometheusForecast_Fallback(t *testing.T) {
	tests := []struct {
		name    string
		usage   usageFunc
		params  url.Values
		reason  string
		history int
	}{
		{
			name:   "no history",
			usage:  func(int) (float64, bool) { return 0, false },
			reason: "no usage history", history: 0,
		},
		{
			name:   "constant",
			usage:  func(int) (float64, bool) { return 0.25, true },
			reason: "usage history is constant", history: 2017,
		},
		{
			name:   "all zero",
			usage:  func(int) (float64, bool) { return 0, true },
			reason: "usage history is constant", history: 2017,
		},
		{
			name:   "only NaN",
			usage:  func(int) (float64, bool) { return math.NaN(), true },
			reason: "0 valid samples", history: 0,
		},
		{
			name:   "only negative",
			usage:  func(int) (float64, bool) { return -1, true },
			reason: "0 valid samples", history: 0,
		},
		{
			name:   "infinite",
			usage:  func(int) (float64, bool) { return math.Inf(1), true },
			reason: "0 valid samples", history: 0,
		},
		{
			name: "gappy",
			usage: func(i int) (float64, bool) {
				if i > 300 && i < 1500 {
					return 0, false
				}
				return dailyUsage(i)
			},
			reason: "too many gaps", history: 2017 - 1199,
		},
		{
			name:   "tiny window",
			usage:  dailyUsage,
			params: forecastParams("window", "1h", "horizon", "1h"),
			reason: "history too short: 13 valid samples", history: 13,
		},
		{
			name:   "one second window",
			usage:  dailyUsage,
			params: forecastParams("window", "1s", "horizon", "1s"),
			reason: "history too short", history: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prom := newFakeForecastProm(t, tt.usage)
			params := tt.params
			if params == nil {
				params = forecastParams()
			}
			for _, resource := range []string{"cpu", "memory"} {
				params.Set("resourceType", resource)
				status, body := getForecast(t, newForecastHandler(prom.URL, ""), context.Background(), params)
				require.Equal(t, http.StatusOK, status, body.Error)
				assert.True(t, body.Success)
				assert.Contains(t, body.Data.FallbackReason, tt.reason)
				assert.NotNil(t, body.Data.Forecast)
				assert.Empty(t, body.Data.Forecast)
				assert.Empty(t, body.Data.Model)
				assert.Equal(t, "5m0s", body.Data.SampleInterval)
				assert.Len(t, body.Data.History, tt.history)
			}
		})
	}
}

func TestGetPrometheusForecast_InvalidSamplesAreDropped(t *testing.T) {
	prom := newFakeForecastProm(t, func(i int) (float64, bool) {
		switch {
		case i%10 == 5:
			return math.NaN(), true
		case i%97 == 0:
			return math.Inf(1), true
		case i%101 == 0:
			return -5, true
		}
		return dailyUsage(i)
	})
	status, body := getForecast(t, newForecastHandler(prom.URL, ""), context.Background(), forecastParams())
	require.Equal(t, http.StatusOK, status, body.Error)
	assertForecastPoints(t, body, 96, 15*time.Minute)
	for _, p := range body.Data.History {
		assert.False(t, math.IsNaN(p.Value) || math.IsInf(p.Value, 0) || p.Value < 0, "%+v", p)
	}
	assert.Less(t, len(body.Data.History), 7*288)
}

func TestGetPrometheusForecast_Cluster(t *testing.T) {
	prom := newFakeForecastProm(t, dailyUsage)
	h := newForecastHandler(prom.URL, "cluster")

	status, body := getForecast(t, h, context.Background(), forecastParams())
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, body.Error, "a target cluster is required")
	assert.Zero(t, prom.requestCount(), "no unscoped query")

	status, body = getForecast(t, h, context.Background(), forecastParams("cluster", "cluster-a"))
	require.Equal(t, http.StatusOK, status, body.Error)
	assert.NotEmpty(t, body.Data.Forecast)
	for _, r := range prom.rangeRequests() {
		q := r.Get("query")
		assert.Equal(t, strings.Count(q, `namespace="`), strings.Count(q, `cluster="cluster-a", namespace="example"`), q)
	}

	unscoped := newForecastHandler(prom.URL, "")
	before := prom.requestCount()
	status, body = getForecast(t, unscoped, context.Background(), forecastParams("cluster", "cluster-a"))
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, body.Error, "sets no prometheus.clusterLabel")
	assert.Equal(t, before, prom.requestCount())
}

func TestGetPrometheusForecast_Cancellation(t *testing.T) {
	t.Run("cancelled request", func(t *testing.T) {
		prom := newFakeForecastProm(t, dailyUsage)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		status, body := getForecast(t, newForecastHandler(prom.URL, ""), ctx, forecastParams())
		assert.Equal(t, http.StatusGatewayTimeout, status)
		assert.Contains(t, body.Error, "context canceled")
	})
	t.Run("slow backend times out", func(t *testing.T) {
		prom := newFakeForecastProm(t, dailyUsage)
		prom.delay = 10 * time.Second
		h := newForecastHandler(prom.URL, "")
		h.forecastTimeout = 100 * time.Millisecond
		start := time.Now()
		status, body := getForecast(t, h, context.Background(), forecastParams())
		assert.Equal(t, http.StatusGatewayTimeout, status)
		assert.Contains(t, body.Error, "deadline exceeded")
		assert.Less(t, time.Since(start), 5*time.Second)
	})
	t.Run("busy forecast slots time out", func(t *testing.T) {
		prom := newFakeForecastProm(t, dailyUsage)
		h := newForecastHandler(prom.URL, "")
		h.forecastTimeout = 500 * time.Millisecond
		for range maxConcurrentForecasts {
			h.forecastSlots <- struct{}{}
		}
		status, body := getForecast(t, h, context.Background(), forecastParams())
		assert.Equal(t, http.StatusGatewayTimeout, status)
		assert.Contains(t, body.Error, "deadline exceeded")

		for range maxConcurrentForecasts {
			<-h.forecastSlots
		}
		status, body = getForecast(t, h, context.Background(), forecastParams())
		require.Equal(t, http.StatusOK, status, body.Error)
	})
	t.Run("concurrent requests share the slots", func(t *testing.T) {
		prom := newFakeForecastProm(t, dailyUsage)
		h := newForecastHandler(prom.URL, "")
		var wg sync.WaitGroup
		statuses := make([]int, 3*maxConcurrentForecasts)
		for i := range statuses {
			wg.Add(1)
			go func() {
				defer wg.Done()
				statuses[i], _ = getForecast(t, h, context.Background(), forecastParams("horizon", fmt.Sprintf("%dh", i+1)))
			}()
		}
		wg.Wait()
		for i, status := range statuses {
			assert.Equal(t, http.StatusOK, status, "request %d", i)
		}
		assert.Empty(t, h.forecastSlots, "every slot is released")
	})
}
