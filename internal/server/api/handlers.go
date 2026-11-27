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
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/argocd"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
	"github.com/sartorproj/sartor/internal/opencost"
	"github.com/sartorproj/sartor/internal/recommender"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

// Handler provides HTTP handlers for the Sartor API.
type Handler struct {
	client     client.Client
	calculator *recommender.Calculator
	// forecastSlots bounds the number of concurrent forecast fits.
	forecastSlots   chan struct{}
	forecastTimeout time.Duration
}

// NewHandler creates a new API Handler.
func NewHandler(c client.Client) *Handler {
	return &Handler{
		client:          c,
		calculator:      recommender.NewCalculator(),
		forecastSlots:   make(chan struct{}, maxConcurrentForecasts),
		forecastTimeout: defaultForecastTimeout,
	}
}

// Response is a generic API response.
type Response struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Message string      `json:"message,omitempty"`
}

// TailoringSummary is a summary of a Tailoring resource.
type TailoringSummary struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace"`
	TargetKind     string `json:"targetKind"`
	TargetName     string `json:"targetName"`
	FitProfile     string `json:"fitProfile"`
	Paused         bool   `json:"paused"`
	TargetFound    bool   `json:"targetFound"`
	VPADetected    bool   `json:"vpaDetected"`
	LastAnalysis   string `json:"lastAnalysis,omitempty"`
	ContainerCount int    `json:"containerCount"`
	ReadyCondition string `json:"readyCondition"`
	PRState        string `json:"prState,omitempty"`
	PRNumber       *int   `json:"prNumber,omitempty"`
	PRUrl          string `json:"prUrl,omitempty"`
}

// AtelierSummary is a summary of an Atelier resource.
type AtelierSummary struct {
	Name                 string `json:"name"`
	PrometheusURL        string `json:"prometheusUrl"`
	PrometheusConnected  bool   `json:"prometheusConnected"`
	GitProviderType      string `json:"gitProviderType"`
	GitProviderConnected bool   `json:"gitProviderConnected"`
	ArgoCDEnabled        bool   `json:"argoCDEnabled"`
	ArgoCDConnected      bool   `json:"argoCDConnected"`
	OpenCostEnabled      bool   `json:"openCostEnabled"`
	OpenCostConnected    bool   `json:"openCostConnected"`
	OpenCostURL          string `json:"openCostUrl,omitempty"`
	ManagedTailorings    int    `json:"managedTailorings"`
	BatchMode            bool   `json:"batchMode"`
}

// ArgoCDAppSummary is a summary of an ArgoCD Application.
type ArgoCDAppSummary struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace"`
	Project        string `json:"project"`
	RepoURL        string `json:"repoUrl"`
	Path           string `json:"path"`
	TargetRevision string `json:"targetRevision"`
	SyncStatus     string `json:"syncStatus"`
	HealthStatus   string `json:"healthStatus"`
}

// ArgoCDSyncRequest is a request to sync an ArgoCD application.
type ArgoCDSyncRequest struct {
	Revision string `json:"revision,omitempty"`
	Prune    bool   `json:"prune,omitempty"`
	DryRun   bool   `json:"dryRun,omitempty"`
}

// DashboardStats contains dashboard statistics.
type DashboardStats struct {
	TotalTailorings  int `json:"totalTailorings"`
	ActiveTailorings int `json:"activeTailorings"`
	PausedTailorings int `json:"pausedTailorings"`
	TotalFitProfiles int `json:"totalFitProfiles"`
	OpenPRs          int `json:"openPRs"`
	MergedPRs        int `json:"mergedPRs"`
	ClosedPRs        int `json:"closedPRs"`
	IgnoredPRs       int `json:"ignoredPRs"`
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// writeError writes an error response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, Response{
		Success: false,
		Error:   message,
	})
}

// ListTailorings returns all Tailorings.
func (h *Handler) ListTailorings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Parse query params
	namespace := r.URL.Query().Get("namespace")

	tailoringList := &autoscalingv1alpha1.TailoringList{}
	listOpts := []client.ListOption{}
	if namespace != "" {
		listOpts = append(listOpts, client.InNamespace(namespace))
	}

	if err := h.client.List(ctx, tailoringList, listOpts...); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	summaries := make([]TailoringSummary, 0, len(tailoringList.Items))
	for _, t := range tailoringList.Items {
		summary := TailoringSummary{
			Name:        t.Name,
			Namespace:   t.Namespace,
			TargetKind:  t.Spec.Target.Kind,
			TargetName:  t.Spec.Target.Name,
			FitProfile:  t.Spec.FitProfileRef.Name, // Use FitProfile name instead
			Paused:      t.Spec.Paused,
			TargetFound: t.Status.TargetFound,
			VPADetected: t.Status.VPADetected,
		}

		if t.Status.LastAnalysis != nil {
			summary.LastAnalysis = t.Status.LastAnalysis.Timestamp.Format("2006-01-02T15:04:05Z")
			summary.ContainerCount = len(t.Status.LastAnalysis.Containers)
		}

		// Get ready condition
		for _, c := range t.Status.Conditions {
			if c.Type == "Ready" {
				summary.ReadyCondition = string(c.Status)
				break
			}
		}

		// Add PR status fields
		if t.Status.PRState != "" {
			summary.PRState = string(t.Status.PRState)
		}
		if t.Status.PRNumber != 0 {
			summary.PRNumber = &t.Status.PRNumber
		}
		if t.Status.PRUrl != "" {
			summary.PRUrl = t.Status.PRUrl
		}

		summaries = append(summaries, summary)
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    summaries,
	})
}

// GetTailoring returns a specific Tailoring.
func (h *Handler) GetTailoring(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Extract namespace and name from path
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/tailorings/"), "/")
	if len(parts) != 2 {
		writeError(w, http.StatusBadRequest, "Invalid path. Expected /api/v1/tailorings/{namespace}/{name}")
		return
	}
	namespace, name := parts[0], parts[1]

	tailoring := &autoscalingv1alpha1.Tailoring{}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, tailoring); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    tailoring,
	})
}

// GetAtelier returns the Atelier configuration.
func (h *Handler) GetAtelier(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	atelierList := &autoscalingv1alpha1.AtelierList{}
	if err := h.client.List(ctx, atelierList); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if len(atelierList.Items) == 0 {
		writeError(w, http.StatusNotFound, "No Atelier found")
		return
	}

	atelier := atelierList.Items[0]
	summary := AtelierSummary{
		Name:                 atelier.Name,
		PrometheusURL:        atelier.Spec.Prometheus.URL,
		PrometheusConnected:  atelier.Status.PrometheusConnected,
		GitProviderType:      string(atelier.Spec.GitProvider.Type),
		GitProviderConnected: atelier.Status.GitProviderConnected,
		ArgoCDEnabled:        atelier.Spec.ArgoCD != nil && atelier.Spec.ArgoCD.Enabled,
		ArgoCDConnected:      atelier.Status.ArgoCDConnected,
		OpenCostEnabled:      atelier.Spec.OpenCost != nil && atelier.Spec.OpenCost.Enabled,
		OpenCostConnected:    atelier.Status.OpenCostConnected,
		ManagedTailorings:    int(atelier.Status.ManagedTailorings),
		BatchMode:            atelier.Spec.BatchMode,
	}
	if atelier.Spec.OpenCost != nil {
		summary.OpenCostURL = atelier.Spec.OpenCost.URL
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    summary,
	})
}

// GetDashboardStats returns dashboard statistics.
func (h *Handler) GetDashboardStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	stats := DashboardStats{}

	// Count Tailorings
	tailoringList := &autoscalingv1alpha1.TailoringList{}
	if err := h.client.List(ctx, tailoringList); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	stats.TotalTailorings = len(tailoringList.Items)
	for _, t := range tailoringList.Items {
		if t.Spec.Paused {
			stats.PausedTailorings++
		} else {
			stats.ActiveTailorings++
		}
	}

	// Count PRs from Tailorings
	for _, t := range tailoringList.Items {
		switch t.Status.PRState {
		case autoscalingv1alpha1.PRStateOpen:
			stats.OpenPRs++
		case autoscalingv1alpha1.PRStateMerged:
			stats.MergedPRs++
		case autoscalingv1alpha1.PRStateClosed:
			if t.Status.Ignored {
				stats.IgnoredPRs++
			} else {
				stats.ClosedPRs++
			}
		}
	}

	// Count FitProfiles
	fitProfileList := &autoscalingv1alpha1.FitProfileList{}
	if err := h.client.List(ctx, fitProfileList); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	stats.TotalFitProfiles = len(fitProfileList.Items)

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    stats,
	})
}

// GetRecommendations returns current recommendations for a Tailoring.
func (h *Handler) GetRecommendations(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/recommendations/"), "/")
	if len(parts) != 2 {
		writeError(w, http.StatusBadRequest, "Invalid path. Expected /api/v1/recommendations/{namespace}/{tailoring}")
		return
	}
	namespace, name := parts[0], parts[1]

	tailoring := &autoscalingv1alpha1.Tailoring{}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, tailoring); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if tailoring.Status.LastAnalysis == nil {
		writeJSON(w, http.StatusOK, Response{
			Success: true,
			Message: "No analysis available yet",
			Data:    nil,
		})
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    tailoring.Status.LastAnalysis,
	})
}

// FitProfileSummary is a summary of a FitProfile resource.
type FitProfileSummary struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Strategy    string `json:"strategy"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	UsageCount  int    `json:"usageCount"`
	Valid       bool   `json:"valid"`
}

// ListFitProfiles returns all FitProfiles.
func (h *Handler) ListFitProfiles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	namespace := r.URL.Query().Get("namespace")

	fitProfileList := &autoscalingv1alpha1.FitProfileList{}
	listOpts := []client.ListOption{}
	if namespace != "" {
		listOpts = append(listOpts, client.InNamespace(namespace))
	}

	if err := h.client.List(ctx, fitProfileList, listOpts...); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Return full FitProfile objects for UI compatibility
	profiles := make([]*autoscalingv1alpha1.FitProfile, 0, len(fitProfileList.Items))
	for i := range fitProfileList.Items {
		profiles = append(profiles, &fitProfileList.Items[i])
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    profiles,
	})
}

// GetFitProfile returns a specific FitProfile.
func (h *Handler) GetFitProfile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/fitprofiles/"), "/")
	if len(parts) != 2 {
		writeError(w, http.StatusBadRequest, "Invalid path. Expected /api/v1/fitprofiles/{namespace}/{name}")
		return
	}
	namespace, name := parts[0], parts[1]

	fitProfile := &autoscalingv1alpha1.FitProfile{}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, fitProfile); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    fitProfile,
	})
}

// ListArgoCDApps returns all ArgoCD applications.
func (h *Handler) ListArgoCDApps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get Atelier for ArgoCD configuration
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if atelier.Spec.ArgoCD == nil || !atelier.Spec.ArgoCD.Enabled {
		writeError(w, http.StatusBadRequest, "ArgoCD integration is not enabled")
		return
	}

	argoCDClient, err := h.createArgoCDClient(ctx, atelier)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	apps, err := argoCDClient.ListApplications(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	summaries := make([]ArgoCDAppSummary, 0, len(apps))
	for _, app := range apps {
		summaries = append(summaries, ArgoCDAppSummary{
			Name:           app.Name,
			Namespace:      app.Namespace,
			Project:        app.Project,
			RepoURL:        app.RepoURL,
			Path:           app.Path,
			TargetRevision: app.TargetRevision,
			SyncStatus:     app.SyncStatus,
			HealthStatus:   app.HealthStatus,
		})
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    summaries,
	})
}

// GetArgoCDApp returns a specific ArgoCD application.
func (h *Handler) GetArgoCDApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/argocd/apps/"), "/")
	if len(parts) != 2 {
		writeError(w, http.StatusBadRequest, "Invalid path. Expected /api/v1/argocd/apps/{namespace}/{name}")
		return
	}
	namespace, name := parts[0], parts[1]

	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if atelier.Spec.ArgoCD == nil || !atelier.Spec.ArgoCD.Enabled {
		writeError(w, http.StatusBadRequest, "ArgoCD integration is not enabled")
		return
	}

	argoCDClient, err := h.createArgoCDClient(ctx, atelier)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	app, err := argoCDClient.GetApplication(ctx, name, namespace)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data: ArgoCDAppSummary{
			Name:           app.Name,
			Namespace:      app.Namespace,
			Project:        app.Project,
			RepoURL:        app.RepoURL,
			Path:           app.Path,
			TargetRevision: app.TargetRevision,
			SyncStatus:     app.SyncStatus,
			HealthStatus:   app.HealthStatus,
		},
	})
}

// SyncArgoCDApp triggers a sync for an ArgoCD application.
func (h *Handler) SyncArgoCDApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/argocd/apps/"), "/")
	if len(parts) < 2 {
		writeError(w, http.StatusBadRequest, "Invalid path. Expected /api/v1/argocd/apps/{namespace}/{name}/sync")
		return
	}
	namespace, name := parts[0], parts[1]

	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if atelier.Spec.ArgoCD == nil || !atelier.Spec.ArgoCD.Enabled {
		writeError(w, http.StatusBadRequest, "ArgoCD integration is not enabled")
		return
	}

	// Parse request body
	var syncReq ArgoCDSyncRequest
	if r.Body != nil {
		// Ignore decode errors, use defaults
		_ = json.NewDecoder(r.Body).Decode(&syncReq)
	}

	argoCDClient, err := h.createArgoCDClient(ctx, atelier)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	result, err := argoCDClient.Sync(ctx, name, namespace, argocd.SyncRequest{
		Revision: syncReq.Revision,
		Prune:    syncReq.Prune,
		DryRun:   syncReq.DryRun,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Message: result.Message,
		Data: map[string]string{
			"status": result.Status,
		},
	})
}

// RefreshArgoCDApp triggers a refresh for an ArgoCD application.
func (h *Handler) RefreshArgoCDApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/argocd/apps/"), "/")
	if len(parts) < 2 {
		writeError(w, http.StatusBadRequest, "Invalid path. Expected /api/v1/argocd/apps/{namespace}/{name}/refresh")
		return
	}
	namespace, name := parts[0], parts[1]

	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if atelier.Spec.ArgoCD == nil || !atelier.Spec.ArgoCD.Enabled {
		writeError(w, http.StatusBadRequest, "ArgoCD integration is not enabled")
		return
	}

	hard := r.URL.Query().Get("hard") == "true"

	argoCDClient, err := h.createArgoCDClient(ctx, atelier)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	result, err := argoCDClient.Refresh(ctx, name, namespace, hard)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: result.Success,
		Message: result.Message,
	})
}

// DetectArgoCDApp detects which ArgoCD application manages a workload.
// With the optional cluster parameter the workload runs in another cluster, so
// its labels are not looked up in this cluster's API.
func (h *Handler) DetectArgoCDApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	namespace := r.URL.Query().Get("namespace")
	workload := r.URL.Query().Get("workload")
	cluster := r.URL.Query().Get("cluster")

	if namespace == "" || workload == "" {
		writeError(w, http.StatusBadRequest, "namespace and workload query parameters are required")
		return
	}

	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if atelier.Spec.ArgoCD == nil || !atelier.Spec.ArgoCD.Enabled {
		writeError(w, http.StatusBadRequest, "ArgoCD integration is not enabled")
		return
	}

	cfg, err := h.argoCDConfig(ctx, atelier)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	workloads := h.client
	if cluster != "" {
		workloads = nil
	}
	argoCDClient := argocd.NewClient(cfg, workloads)

	app, err := argoCDClient.DetectApplication(ctx, namespace, workload)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{
			Success: true,
			Message: "No ArgoCD application found",
			Data:    nil,
		})
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data: ArgoCDAppSummary{
			Name:           app.Name,
			Namespace:      app.Namespace,
			Project:        app.Project,
			RepoURL:        app.RepoURL,
			Path:           app.Path,
			TargetRevision: app.TargetRevision,
			SyncStatus:     app.SyncStatus,
			HealthStatus:   app.HealthStatus,
		},
	})
}

// getAtelier retrieves the Atelier resource.
func (h *Handler) getAtelier(ctx context.Context) (*autoscalingv1alpha1.Atelier, error) {
	atelierList := &autoscalingv1alpha1.AtelierList{}
	if err := h.client.List(ctx, atelierList); err != nil {
		return nil, err
	}

	if len(atelierList.Items) == 0 {
		return nil, &notFoundError{message: "No Atelier found"}
	}

	return &atelierList.Items[0], nil
}

// createArgoCDClient creates an ArgoCD client from Atelier configuration.
func (h *Handler) createArgoCDClient(ctx context.Context, atelier *autoscalingv1alpha1.Atelier) (*argocd.Client, error) {
	cfg, err := h.argoCDConfig(ctx, atelier)
	if err != nil {
		return nil, err
	}
	return argocd.NewClient(cfg, h.client), nil
}

// argoCDConfig reads the ArgoCD client configuration from the Atelier.
func (h *Handler) argoCDConfig(ctx context.Context, atelier *autoscalingv1alpha1.Atelier) (argocd.Config, error) {
	cfg := argocd.Config{
		InsecureSkipVerify: atelier.Spec.ArgoCD.InsecureSkipVerify,
	}

	if atelier.Spec.ArgoCD.ServerURL != "" {
		cfg.ServerURL = atelier.Spec.ArgoCD.ServerURL

		if atelier.Spec.ArgoCD.SecretRef != nil {
			secret := &corev1.Secret{}
			if err := h.client.Get(ctx, types.NamespacedName{
				Name:      atelier.Spec.ArgoCD.SecretRef.Name,
				Namespace: atelier.Spec.ArgoCD.SecretRef.Namespace,
			}, secret); err != nil {
				return cfg, err
			}

			if token, ok := secret.Data["token"]; ok {
				cfg.Token = string(token)
			}
		}
	}

	return cfg, nil
}

// createPrometheusClient creates a Prometheus client from Atelier configuration.
func (h *Handler) createPrometheusClient(ctx context.Context, atelier *autoscalingv1alpha1.Atelier) (*prometheus.Client, error) {
	config := prometheus.Config{
		URL:                atelier.Spec.Prometheus.URL,
		InsecureSkipVerify: atelier.Spec.Prometheus.InsecureSkipVerify,
		ClusterLabel:       atelier.Spec.Prometheus.ClusterLabel,
	}

	if atelier.Spec.Prometheus.SecretRef != nil {
		secret := &corev1.Secret{}
		if err := h.client.Get(ctx, types.NamespacedName{
			Name:      atelier.Spec.Prometheus.SecretRef.Name,
			Namespace: atelier.Spec.Prometheus.SecretRef.Namespace,
		}, secret); err != nil {
			return nil, err
		}

		if username, ok := secret.Data["username"]; ok {
			config.Username = string(username)
		}
		if password, ok := secret.Data["password"]; ok {
			config.Password = string(password)
		}
		if token, ok := secret.Data["token"]; ok {
			config.Token = string(token)
		}
	}

	return prometheus.NewClient(config)
}

// GetMetrics returns Prometheus metrics for a specific tailoring.
func (h *Handler) GetMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/metrics/"), "/")
	if len(parts) != 2 {
		writeError(w, http.StatusBadRequest, "Invalid path. Expected /api/v1/metrics/{namespace}/{name}")
		return
	}
	namespace, name := parts[0], parts[1]

	// Get Tailoring
	tailoring := &autoscalingv1alpha1.Tailoring{}
	if err := h.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, tailoring); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create Prometheus client
	promClient, err := h.createPrometheusClient(ctx, atelier)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to create Prometheus client: %v", err))
		return
	}

	// Determine analysis window
	analysisWindow := 7 * 24 * time.Hour
	if tailoring.Spec.AnalysisWindow != nil {
		analysisWindow = tailoring.Spec.AnalysisWindow.Duration
	} else if atelier.Spec.DefaultAnalysisWindow != nil {
		analysisWindow = atelier.Spec.DefaultAnalysisWindow.Duration
	}

	// Query Prometheus for metrics
	metrics, err := promClient.GetContainerMetrics(ctx, tailoring.TargetNamespace(), prometheus.Workload{
		Kind:    tailoring.Spec.Target.Kind,
		Name:    tailoring.Spec.Target.Name,
		Cluster: tailoring.Spec.Target.Cluster,
	}, analysisWindow)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get metrics: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    metrics,
	})
}

// maxTimeSeriesWindow bounds the range of time series queries.
const maxTimeSeriesWindow = 31 * 24 * time.Hour

// bytesPerMiB converts memory usage to the MiB returned by the API.
const bytesPerMiB = 1024 * 1024

// containerQuery identifies the container usage queried by the Prometheus
// time series endpoints.
type containerQuery struct {
	namespace     string
	workload      prometheus.Workload
	containerName string
	resourceType  string
	window        time.Duration
}

// parseContainerQuery reads and validates the query parameters shared by the
// Prometheus time series endpoints. window defaults to defaultWindow. Every
// error is a client error.
func parseContainerQuery(r *http.Request, defaultWindow time.Duration) (containerQuery, error) {
	query := r.URL.Query()
	// name is the workload name; podNamePrefix is the legacy parameter name.
	workloadName := query.Get("name")
	if workloadName == "" {
		workloadName = query.Get("podNamePrefix")
	}
	kind := query.Get("kind")
	if kind == "" {
		kind = prometheus.KindDeployment
	}
	q := containerQuery{
		namespace:     query.Get("namespace"),
		workload:      prometheus.Workload{Kind: kind, Name: workloadName, Cluster: query.Get("cluster")},
		containerName: query.Get("containerName"),
		resourceType:  query.Get("resourceType"),
		window:        defaultWindow,
	}

	if q.namespace == "" || workloadName == "" || q.containerName == "" || q.resourceType == "" {
		return q, fmt.Errorf("missing required parameters: namespace, name (or podNamePrefix), containerName, resourceType")
	}
	if _, err := prometheus.PodNameRegex(q.workload); err != nil {
		return q, err
	}
	if err := prometheus.ValidateQueryTarget(q.namespace, q.workload, q.containerName); err != nil {
		return q, err
	}
	if q.resourceType != prometheus.ResourceTypeCPU && q.resourceType != prometheus.ResourceTypeMemory {
		return q, fmt.Errorf("resourceType must be cpu or memory")
	}

	if windowStr := query.Get("window"); windowStr != "" {
		var err error
		q.window, err = time.ParseDuration(windowStr)
		if err != nil {
			return q, fmt.Errorf("invalid window duration: %v", err)
		}
		if q.window <= 0 || q.window > maxTimeSeriesWindow {
			return q, fmt.Errorf("window must be between 0 and %s", maxTimeSeriesWindow)
		}
	}
	return q, nil
}

// GetPrometheusMetrics returns Prometheus time series metrics for a specific container.
// Query parameters: namespace, name (or legacy podNamePrefix), kind
// (Deployment, StatefulSet or DaemonSet; default Deployment), containerName,
// resourceType (cpu or memory), window and cluster (required when the Atelier
// sets prometheus.clusterLabel).
func (h *Handler) GetPrometheusMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	q, err := parseContainerQuery(r, time.Hour)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := prometheus.ValidateCluster(atelier.Spec.Prometheus.ClusterLabel, q.workload.Cluster); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Create Prometheus client
	promClient, err := h.createPrometheusClient(ctx, atelier)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to create Prometheus client: %v", err))
		return
	}

	// Query Prometheus for time series data
	points, err := promClient.GetTimeSeriesMetrics(ctx, q.namespace, q.workload, q.containerName, q.resourceType, q.window)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get time series metrics: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    points,
	})
}

const (
	defaultForecastWindow     = 7 * 24 * time.Hour
	defaultForecastHorizon    = 24 * time.Hour
	defaultForecastConfidence = 0.95
	// defaultForecastTimeout stays below the server's 15s WriteTimeout, so a
	// timeout is still answered with a JSON error.
	defaultForecastTimeout = 12 * time.Second
	maxConcurrentForecasts = 4
)

// ForecastResponse is the data returned by GetPrometheusForecast. Values are
// in cores for CPU and in MiB for memory, as for GetPrometheusMetrics.
type ForecastResponse struct {
	// History is the usage history the forecast is based on, at the
	// strategy's query resolution.
	History []prometheus.TimeSeriesPoint `json:"history"`
	// Forecast is empty when the history cannot be forecast.
	Forecast []strategy.ForecastPoint `json:"forecast"`
	// Level is the confidence level of the prediction interval.
	Level float64 `json:"level"`
	// Model is the fitted model, empty without a forecast.
	Model string `json:"model"`
	// SampleInterval is the interval of the forecast points, or of the
	// history query without a forecast.
	SampleInterval string `json:"sampleInterval"`
	// FallbackReason explains why there is no forecast.
	FallbackReason string `json:"fallbackReason,omitempty"`
}

// GetPrometheusForecast forecasts a container's CPU or memory usage with the
// ARIMA strategy. It accepts the query parameters of GetPrometheusMetrics
// (window defaults to 168h), plus horizon (default 24h, at most 168h, not
// longer than window), confidence (default 0.95, in (0, 1)) and the model
// parameters mode, seasonal, order, seasonalOrder and seasonalPeriod of the
// ARIMA strategy. A history that cannot be forecast is reported in
// fallbackReason, not as an error.
func (h *Handler) GetPrometheusForecast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q, err := parseContainerQuery(r, defaultForecastWindow)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, horizon, confidence, err := parseForecastConfig(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if q.window < horizon {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("window (%s) must not be shorter than horizon (%s)", q.window, horizon))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.forecastTimeout)
	defer cancel()
	fail := func(status int, message string) {
		if err := ctx.Err(); err != nil {
			status, message = http.StatusGatewayTimeout, fmt.Sprintf("Forecast cancelled: %v", err)
		}
		writeError(w, status, message)
	}

	atelier, err := h.getAtelier(ctx)
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	if err := prometheus.ValidateCluster(atelier.Spec.Prometheus.ClusterLabel, q.workload.Cluster); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	promClient, err := h.createPrometheusClient(ctx, atelier)
	if err != nil {
		fail(http.StatusInternalServerError, fmt.Sprintf("Failed to create Prometheus client: %v", err))
		return
	}

	arima := strategy.NewARIMAStrategy(strategy.DefaultARIMASampleInterval)
	step := arima.HistoryStep(q.window)
	histories, err := promClient.GetContainerHistory(ctx, q.namespace, q.workload, q.window, step)
	if err != nil {
		fail(http.StatusInternalServerError, fmt.Sprintf("Failed to get usage history: %v", err))
		return
	}
	history, scale := histories[q.containerName].CPU, 1.0
	if q.resourceType == prometheus.ResourceTypeMemory {
		history, scale = histories[q.containerName].Memory, 1.0/bytesPerMiB
	}

	data := ForecastResponse{
		History:        usagePoints(history, scale),
		Forecast:       []strategy.ForecastPoint{},
		Level:          confidence,
		SampleInterval: step.String(),
	}
	fc, err := h.runForecast(ctx, arima, history, cfg)
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		fail(http.StatusGatewayTimeout, fmt.Sprintf("Forecast cancelled: %v", err))
		return
	case err != nil:
		data.FallbackReason = err.Error()
	default:
		data.Model = fc.Model
		data.SampleInterval = fc.SampleInterval.String()
		for _, p := range fc.Points {
			data.Forecast = append(data.Forecast, strategy.ForecastPoint{
				Timestamp: p.Timestamp, Value: p.Value * scale, Lower: p.Lower * scale, Upper: p.Upper * scale,
			})
		}
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: data})
}

// parseForecastConfig reads and validates the forecast query parameters.
func parseForecastConfig(query url.Values) (strategy.ForecastConfig, time.Duration, float64, error) {
	horizon, confidence := defaultForecastHorizon, defaultForecastConfidence
	if v := query.Get("horizon"); v != "" {
		var err error
		if horizon, err = time.ParseDuration(v); err != nil {
			return strategy.ForecastConfig{}, 0, 0, fmt.Errorf("invalid horizon duration: %v", err)
		}
	}
	if v := query.Get("confidence"); v != "" {
		var err error
		if confidence, err = strconv.ParseFloat(v, 64); err != nil {
			return strategy.ForecastConfig{}, 0, 0, fmt.Errorf("invalid confidence: %q is not a number", v)
		}
	}
	cfg, err := strategy.NewForecastConfig(horizon, confidence, forecastModelParameters(query))
	return cfg, horizon, confidence, err
}

// forecastModelParameters converts the model query parameters to ARIMA
// FitProfile parameters, which the strategy validates: order and
// seasonalOrder are comma-separated integers, seasonal is a boolean.
// Values that do not convert are passed on as strings, so they are rejected.
func forecastModelParameters(query url.Values) map[string]interface{} {
	params := make(map[string]interface{})
	for _, key := range strategy.ARIMAForecastModelKeys {
		if !query.Has(key) {
			continue
		}
		value := query.Get(key)
		params[key] = value
		switch key {
		case "order", "seasonalOrder":
			var order []interface{}
			for _, item := range strings.Split(value, ",") {
				if n, err := strconv.Atoi(strings.TrimSpace(item)); err == nil {
					order = append(order, n)
				} else {
					order = append(order, item)
				}
			}
			params[key] = order
		case "seasonal":
			if b, err := strconv.ParseBool(value); err == nil {
				params[key] = b
			}
		}
	}
	return params
}

// runForecast fits the model in a goroutine, so the request returns when ctx
// is done even if a fit does not. A fit holds one of the forecast slots until
// it returns, which bounds the CPU used by fits whose request has gone.
func (h *Handler) runForecast(
	ctx context.Context,
	s *strategy.ARIMAStrategy,
	history []prometheus.TimeSeriesPoint,
	cfg strategy.ForecastConfig,
) (strategy.Forecast, error) {
	select {
	case h.forecastSlots <- struct{}{}:
	case <-ctx.Done():
		return strategy.Forecast{}, fmt.Errorf("waiting for a forecast slot: %w", ctx.Err())
	}
	type result struct {
		fc  strategy.Forecast
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-h.forecastSlots }()
		var res result
		// A panic in this goroutine would take down the server.
		defer func() {
			if p := recover(); p != nil {
				res.err = fmt.Errorf("forecast failed: %v", p)
			}
			done <- res
		}()
		res.fc, res.err = s.Forecast(ctx, history, cfg)
	}()
	select {
	case res := <-done:
		return res.fc, res.err
	case <-ctx.Done():
		return strategy.Forecast{}, ctx.Err()
	}
}

// usagePoints returns the finite, non-negative samples of history multiplied
// by scale. JSON cannot encode NaN or infinite values.
func usagePoints(history []prometheus.TimeSeriesPoint, scale float64) []prometheus.TimeSeriesPoint {
	points := make([]prometheus.TimeSeriesPoint, 0, len(history))
	for _, p := range history {
		if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) || p.Value < 0 {
			continue
		}
		points = append(points, prometheus.TimeSeriesPoint{Timestamp: p.Timestamp, Value: p.Value * scale})
	}
	return points
}

// notFoundError represents a not found error.
type notFoundError struct {
	message string
}

func (e *notFoundError) Error() string {
	return e.message
}

// createOpenCostClient creates an OpenCost client from Atelier configuration.
func (h *Handler) createOpenCostClient(atelier *autoscalingv1alpha1.Atelier) (*opencost.Client, error) {
	if atelier.Spec.OpenCost == nil || !atelier.Spec.OpenCost.Enabled {
		return nil, fmt.Errorf("OpenCost integration is not enabled")
	}

	if atelier.Spec.OpenCost.URL == "" {
		return nil, fmt.Errorf("OpenCost URL is not configured")
	}

	return opencost.NewClient(opencost.Config{
		URL:                atelier.Spec.OpenCost.URL,
		InsecureSkipVerify: atelier.Spec.OpenCost.InsecureSkipVerify,
	})
}

// OpenCostAllocationRequest represents parameters for OpenCost allocation queries.
type OpenCostAllocationRequest struct {
	Window     string `json:"window"`
	Aggregate  string `json:"aggregate"`
	Step       string `json:"step,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

// OpenCostAllocationSummary is a simplified view of allocation data for the UI.
type OpenCostAllocationSummary struct {
	Name             string  `json:"name"`
	Cluster          string  `json:"cluster,omitempty"`
	Namespace        string  `json:"namespace,omitempty"`
	Controller       string  `json:"controller,omitempty"`
	ControllerKind   string  `json:"controllerKind,omitempty"`
	Pod              string  `json:"pod,omitempty"`
	Container        string  `json:"container,omitempty"`
	CPUCores         float64 `json:"cpuCores"`
	CPUCost          float64 `json:"cpuCost"`
	CPUEfficiency    float64 `json:"cpuEfficiency"`
	RAMBytes         float64 `json:"ramBytes"`
	RAMCost          float64 `json:"ramCost"`
	RAMEfficiency    float64 `json:"ramEfficiency"`
	GPUCost          float64 `json:"gpuCost"`
	PVCost           float64 `json:"pvCost"`
	NetworkCost      float64 `json:"networkCost"`
	LoadBalancerCost float64 `json:"loadBalancerCost"`
	SharedCost       float64 `json:"sharedCost"`
	TotalCost        float64 `json:"totalCost"`
	TotalEfficiency  float64 `json:"totalEfficiency"`
}

// OpenCostSummary is the overall cost summary.
type OpenCostSummary struct {
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

// GetOpenCostAllocations returns cost allocations from OpenCost.
func (h *Handler) GetOpenCostAllocations(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create OpenCost client
	ocClient, err := h.createOpenCostClient(atelier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Parse query parameters
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "7d"
	}
	aggregate := r.URL.Query().Get("aggregate")
	if aggregate == "" {
		aggregate = "namespace"
	}
	step := r.URL.Query().Get("step")
	resolution := r.URL.Query().Get("resolution")

	// Query OpenCost
	resp, err := ocClient.GetAllocations(ctx, opencost.AllocationRequest{
		Window:     window,
		Aggregate:  aggregate,
		Step:       step,
		Resolution: resolution,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get allocations: %v", err))
		return
	}

	// Convert to UI-friendly format
	allocations := make([]OpenCostAllocationSummary, 0)
	for _, dataSet := range resp.Data {
		for name, entry := range dataSet {
			if entry == nil {
				continue
			}
			alloc := OpenCostAllocationSummary{
				Name:             name,
				Cluster:          entry.Properties.Cluster,
				Namespace:        entry.Properties.Namespace,
				Controller:       entry.Properties.Controller,
				ControllerKind:   entry.Properties.ControllerKind,
				Pod:              entry.Properties.Pod,
				Container:        entry.Properties.Container,
				CPUCores:         entry.CPUCores,
				CPUCost:          entry.CPUCost,
				CPUEfficiency:    entry.CPUEfficiency,
				RAMBytes:         entry.RAMBytes,
				RAMCost:          entry.RAMCost,
				RAMEfficiency:    entry.RAMEfficiency,
				GPUCost:          entry.GPUCost,
				PVCost:           entry.PVCost,
				NetworkCost:      entry.NetworkCost,
				LoadBalancerCost: entry.LoadBalancerCost,
				SharedCost:       entry.SharedCost,
				TotalCost:        entry.TotalCost,
				TotalEfficiency:  entry.TotalEfficiency,
			}
			allocations = append(allocations, alloc)
		}
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    allocations,
	})
}

// GetOpenCostSummary returns a summary of costs from OpenCost.
func (h *Handler) GetOpenCostSummary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create OpenCost client
	ocClient, err := h.createOpenCostClient(atelier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Parse window parameter
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "7d"
	}

	// Get cost summary
	summary, err := ocClient.GetCostSummary(ctx, window)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get cost summary: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data: OpenCostSummary{
			TotalCost:        summary.TotalCost,
			CPUCost:          summary.CPUCost,
			RAMCost:          summary.RAMCost,
			GPUCost:          summary.GPUCost,
			PVCost:           summary.PVCost,
			NetworkCost:      summary.NetworkCost,
			LoadBalancerCost: summary.LoadBalancerCost,
			SharedCost:       summary.SharedCost,
			Efficiency:       summary.Efficiency,
			CPUEfficiency:    summary.CPUEfficiency,
			RAMEfficiency:    summary.RAMEfficiency,
			ByNamespace:      summary.ByNamespace,
		},
	})
}

// GetOpenCostNamespaceCosts returns costs grouped by namespace.
func (h *Handler) GetOpenCostNamespaceCosts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create OpenCost client
	ocClient, err := h.createOpenCostClient(atelier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	window := r.URL.Query().Get("window")
	if window == "" {
		window = "7d"
	}

	resp, err := ocClient.GetAllocations(ctx, opencost.AllocationRequest{
		Window:    window,
		Aggregate: "namespace",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get namespace costs: %v", err))
		return
	}

	// Convert to UI-friendly format
	allocations := make([]OpenCostAllocationSummary, 0)
	for _, dataSet := range resp.Data {
		for name, entry := range dataSet {
			if entry == nil {
				continue
			}
			alloc := OpenCostAllocationSummary{
				Name:            name,
				Namespace:       entry.Properties.Namespace,
				CPUCores:        entry.CPUCores,
				CPUCost:         entry.CPUCost,
				CPUEfficiency:   entry.CPUEfficiency,
				RAMBytes:        entry.RAMBytes,
				RAMCost:         entry.RAMCost,
				RAMEfficiency:   entry.RAMEfficiency,
				GPUCost:         entry.GPUCost,
				PVCost:          entry.PVCost,
				NetworkCost:     entry.NetworkCost,
				SharedCost:      entry.SharedCost,
				TotalCost:       entry.TotalCost,
				TotalEfficiency: entry.TotalEfficiency,
			}
			allocations = append(allocations, alloc)
		}
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    allocations,
	})
}

// GetOpenCostControllerCosts returns costs grouped by controller.
func (h *Handler) GetOpenCostControllerCosts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create OpenCost client
	ocClient, err := h.createOpenCostClient(atelier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	window := r.URL.Query().Get("window")
	if window == "" {
		window = "7d"
	}

	namespace := r.URL.Query().Get("namespace")

	resp, err := ocClient.GetAllocations(ctx, opencost.AllocationRequest{
		Window:    window,
		Aggregate: "controller",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get controller costs: %v", err))
		return
	}

	// Convert to UI-friendly format
	allocations := make([]OpenCostAllocationSummary, 0)
	for _, dataSet := range resp.Data {
		for name, entry := range dataSet {
			if entry == nil {
				continue
			}
			// Filter by namespace if provided
			if namespace != "" && entry.Properties.Namespace != namespace {
				continue
			}
			alloc := OpenCostAllocationSummary{
				Name:            name,
				Namespace:       entry.Properties.Namespace,
				Controller:      entry.Properties.Controller,
				ControllerKind:  entry.Properties.ControllerKind,
				CPUCores:        entry.CPUCores,
				CPUCost:         entry.CPUCost,
				CPUEfficiency:   entry.CPUEfficiency,
				RAMBytes:        entry.RAMBytes,
				RAMCost:         entry.RAMCost,
				RAMEfficiency:   entry.RAMEfficiency,
				GPUCost:         entry.GPUCost,
				PVCost:          entry.PVCost,
				NetworkCost:     entry.NetworkCost,
				SharedCost:      entry.SharedCost,
				TotalCost:       entry.TotalCost,
				TotalEfficiency: entry.TotalEfficiency,
			}
			allocations = append(allocations, alloc)
		}
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    allocations,
	})
}

// CheckOpenCostHealth checks if OpenCost is reachable.
func (h *Handler) CheckOpenCostHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create OpenCost client
	ocClient, err := h.createOpenCostClient(atelier)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: true, Data: openCostStatus(false, false, err)})
		return
	}

	// Check health
	err = ocClient.HealthCheck(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: true, Data: openCostStatus(false, true, err)})
		return
	}

	writeJSON(w, http.StatusOK, Response{Success: true, Data: openCostStatus(true, true, nil)})
}

// openCostStatus builds the OpenCost status payload returned by the API.
func openCostStatus(connected, enabled bool, err error) map[string]interface{} {
	status := map[string]interface{}{
		"connected": connected,
		"enabled":   enabled,
	}
	if err != nil {
		status["error"] = err.Error()
	}
	return status
}

// GetOpenCostResourceAnalytics returns resource analytics from OpenCost metrics.
func (h *Handler) GetOpenCostResourceAnalytics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create OpenCost client
	ocClient, err := h.createOpenCostClient(atelier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get resource analytics
	analytics, err := ocClient.GetResourceAnalytics(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get resource analytics: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    analytics,
	})
}

// GetOpenCostCostAnalytics returns cost analytics from OpenCost metrics.
func (h *Handler) GetOpenCostCostAnalytics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create OpenCost client
	ocClient, err := h.createOpenCostClient(atelier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get cost analytics
	analytics, err := ocClient.GetCostAnalytics(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get cost analytics: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    analytics,
	})
}

// GetOpenCostClusterSummary returns cluster resource summary from OpenCost.
func (h *Handler) GetOpenCostClusterSummary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create OpenCost client
	ocClient, err := h.createOpenCostClient(atelier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get cluster summary
	summary, err := ocClient.GetClusterResourceSummary(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get cluster summary: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    summary,
	})
}

// GetOpenCostNamespaceBreakdown returns namespace resource breakdown from OpenCost.
func (h *Handler) GetOpenCostNamespaceBreakdown(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Parse namespace from path
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/opencost/namespace/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "Namespace is required")
		return
	}
	namespace := parts[0]

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create OpenCost client
	ocClient, err := h.createOpenCostClient(atelier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get namespace breakdown
	breakdown, err := ocClient.GetNamespaceBreakdown(ctx, namespace)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get namespace breakdown: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    breakdown,
	})
}

// GetOpenCostMetrics returns raw metrics from OpenCost Prometheus exporter.
func (h *Handler) GetOpenCostMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Get Atelier
	atelier, err := h.getAtelier(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create OpenCost client
	ocClient, err := h.createOpenCostClient(atelier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get metrics
	metrics, err := ocClient.GetMetrics(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get metrics: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    metrics,
	})
}

// RegisterRoutes registers API routes on an http.ServeMux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Tailorings
	mux.HandleFunc("/api/v1/tailorings", h.ListTailorings)
	mux.HandleFunc("/api/v1/tailorings/", h.GetTailoring)

	// Cuts

	// Atelier
	mux.HandleFunc("/api/v1/atelier", h.GetAtelier)

	// Dashboard
	mux.HandleFunc("/api/v1/dashboard/stats", h.GetDashboardStats)

	// Recommendations
	mux.HandleFunc("/api/v1/recommendations/", h.GetRecommendations)

	// FitProfiles
	mux.HandleFunc("/api/v1/fitprofiles", h.ListFitProfiles)
	mux.HandleFunc("/api/v1/fitprofiles/", h.GetFitProfile)

	// ArgoCD
	mux.HandleFunc("/api/v1/argocd/apps", h.ListArgoCDApps)
	mux.HandleFunc("/api/v1/argocd/apps/", func(w http.ResponseWriter, r *http.Request) {
		// Route to specific handlers based on path suffix
		path := r.URL.Path
		if strings.HasSuffix(path, "/sync") {
			h.SyncArgoCDApp(w, r)
		} else if strings.HasSuffix(path, "/refresh") {
			h.RefreshArgoCDApp(w, r)
		} else {
			h.GetArgoCDApp(w, r)
		}
	})
	mux.HandleFunc("/api/v1/argocd/detect", h.DetectArgoCDApp)

	// Strategies
	mux.HandleFunc("/api/v1/strategies", h.ListStrategies)
	mux.HandleFunc("/api/v1/strategies/", h.GetStrategy)

	// Metrics
	mux.HandleFunc("/api/v1/metrics/", h.GetMetrics)
	mux.HandleFunc("/api/v1/prometheus/metrics", h.GetPrometheusMetrics)
	mux.HandleFunc("/api/v1/prometheus/forecast", h.GetPrometheusForecast)

	// OpenCost
	mux.HandleFunc("/api/v1/opencost/allocations", h.GetOpenCostAllocations)
	mux.HandleFunc("/api/v1/opencost/summary", h.GetOpenCostSummary)
	mux.HandleFunc("/api/v1/opencost/namespaces", h.GetOpenCostNamespaceCosts)
	mux.HandleFunc("/api/v1/opencost/controllers", h.GetOpenCostControllerCosts)
	mux.HandleFunc("/api/v1/opencost/health", h.CheckOpenCostHealth)

	// OpenCost Analytics (based on Prometheus metrics exporter)
	mux.HandleFunc("/api/v1/opencost/analytics/resources", h.GetOpenCostResourceAnalytics)
	mux.HandleFunc("/api/v1/opencost/analytics/costs", h.GetOpenCostCostAnalytics)
	mux.HandleFunc("/api/v1/opencost/analytics/cluster", h.GetOpenCostClusterSummary)
	mux.HandleFunc("/api/v1/opencost/namespace/", h.GetOpenCostNamespaceBreakdown)
	mux.HandleFunc("/api/v1/opencost/metrics", h.GetOpenCostMetrics)
}

// AuthMiddleware provides API key authentication.
func AuthMiddleware(apiKeys map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health endpoints
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}

		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			apiKey = r.URL.Query().Get("api_key")
		}

		if apiKey == "" {
			writeError(w, http.StatusUnauthorized, "API key required")
			return
		}

		if !apiKeys[apiKey] {
			writeError(w, http.StatusForbidden, "Invalid API key")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// CORSMiddleware adds CORS headers for the allowed origins. An empty list
// disables CORS (same-origin only); "*" must be listed explicitly to allow
// any origin. Preflight requests are answered here, so this middleware must
// wrap AuthMiddleware: browsers never send X-API-Key on a preflight.
func CORSMiddleware(allowedOrigins []string, next http.Handler) http.Handler {
	allowAny := slices.Contains(allowedOrigins, "*")
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[strings.TrimSuffix(o, "/")] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""

		if origin != "" && len(allowedOrigins) > 0 {
			w.Header().Add("Vary", "Origin")
			if allowAny || allowed[origin] {
				if allowAny {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
		}

		if preflight && origin != "" {
			// Disallowed origins get no CORS headers, so the browser blocks them.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ParseOrigins splits a comma-separated origin list, dropping empty entries.
func ParseOrigins(value string) []string {
	var origins []string
	for _, o := range strings.Split(value, ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	return origins
}

// WrapHandler applies the server middleware in the required order: CORS
// outermost (so preflights are answered without credentials), then API key
// authentication when keys are configured.
func WrapHandler(next http.Handler, apiKeys map[string]bool, corsOrigins []string) http.Handler {
	if len(apiKeys) > 0 {
		next = AuthMiddleware(apiKeys, next)
	}
	if len(corsOrigins) > 0 {
		next = CORSMiddleware(corsOrigins, next)
	}
	return next
}

// ListStrategies lists all available recommendation strategies.
func (h *Handler) ListStrategies(w http.ResponseWriter, r *http.Request) {
	_, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Get strategy registry from calculator
	registry := h.calculator.GetRegistry()
	strategyNames := registry.List()

	strategies := make([]map[string]interface{}, 0, len(strategyNames))
	for _, name := range strategyNames {
		s, exists := registry.Get(name)
		if !exists {
			continue
		}
		spec := s.GetDefaultFitProfileSpec()
		strategies = append(strategies, map[string]interface{}{
			"name":        name,
			"displayName": spec.DisplayName,
			"description": spec.Description,
		})
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    strategies,
	})
}

// GetStrategy returns details for a specific strategy including its default spec.
func (h *Handler) GetStrategy(w http.ResponseWriter, r *http.Request) {
	_, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Extract strategy name from path: /api/v1/strategies/{name}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/strategies/")
	if path == "" {
		writeError(w, http.StatusBadRequest, "strategy name required")
		return
	}

	registry := h.calculator.GetRegistry()
	s, exists := registry.Get(path)
	if !exists {
		writeError(w, http.StatusNotFound, fmt.Sprintf("strategy %s not found", path))
		return
	}

	spec := s.GetDefaultFitProfileSpec()
	result := map[string]interface{}{
		"name":              spec.Strategy,
		"displayName":       spec.DisplayName,
		"description":       spec.Description,
		"parametersSchema":  spec.ParametersSchema,
		"exampleParameters": spec.ExampleParameters,
	}

	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    result,
	})
}

// Ensure context is used
var _ = context.Background
var _ = metav1.Now
