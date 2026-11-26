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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

func TestGetPrometheusMetrics_Validation(t *testing.T) {
	var queries atomic.Int32
	var lastQuery atomic.Value
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries.Add(1)
		lastQuery.Store(r.FormValue("query"))
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "query_range") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer prom.Close()

	atelier := &autoscalingv1alpha1.Atelier{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       autoscalingv1alpha1.AtelierSpec{Prometheus: autoscalingv1alpha1.PrometheusConfig{URL: prom.URL}},
	}
	mux := http.NewServeMux()
	NewHandler(newFakeClient(atelier)).RegisterRoutes(mux)

	valid := url.Values{
		"namespace": {"example"}, "name": {"api"}, "containerName": {"app"}, "resourceType": {"cpu"},
	}
	with := func(key, value string) url.Values {
		v := url.Values{}
		for k, vals := range valid {
			v[k] = vals
		}
		if value == "" {
			v.Del(key)
		} else {
			v.Set(key, value)
		}
		return v
	}

	tests := []struct {
		name       string
		params     url.Values
		wantStatus int
	}{
		{"valid", valid, http.StatusOK},
		{"legacy podNamePrefix", func() url.Values { v := with("name", ""); v.Set("podNamePrefix", "api"); return v }(), http.StatusOK},
		{"statefulset kind", with("kind", "StatefulSet"), http.StatusOK},
		{"injection in namespace", with("namespace", `example"} or vector(1) or up{a="`), http.StatusBadRequest},
		{"injection in name", with("name", `api"}) or vector(1) #`), http.StatusBadRequest},
		{"regex in name", with("name", "back.*"), http.StatusBadRequest},
		{"injection in container", with("containerName", `app",container!="`), http.StatusBadRequest},
		{"unsupported kind", with("kind", "CronJob"), http.StatusBadRequest},
		{"unsupported resource", with("resourceType", "disk"), http.StatusBadRequest},
		{"negative window", with("window", "-1h"), http.StatusBadRequest},
		{"huge window", with("window", "100000h"), http.StatusBadRequest},
		{"missing name", with("name", ""), http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := queries.Load()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/prometheus/metrics?"+tt.params.Encode(), nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			assert.Equal(t, tt.wantStatus, rec.Code, rec.Body.String())
			if tt.wantStatus != http.StatusOK {
				assert.Equal(t, before, queries.Load(), "rejected input must not reach Prometheus")
				return
			}
			q, _ := lastQuery.Load().(string)
			assert.Contains(t, q, `container="app"`)
			assert.NotContains(t, q, `api.*`)
		})
	}
}
