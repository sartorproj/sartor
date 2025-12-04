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

package argocd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ArgoCDAppLabel is the label ArgoCD uses to identify managed resources.
	ArgoCDAppLabel = "argocd.argoproj.io/instance"

	// ArgoCDAppAnnotation is the annotation for tracking info.
	ArgoCDAppAnnotation = "argocd.argoproj.io/tracking-id"

	// ArgoCDSyncAnnotation triggers a sync when changed.
	ArgoCDSyncAnnotation = "argocd.argoproj.io/sync-wave"

	// ApplicationGVR is the GroupVersionResource for ArgoCD Applications.
	ApplicationGroup   = "argoproj.io"
	ApplicationVersion = "v1alpha1"
	ApplicationKind    = "Application"

	// refreshTypeHard is the hard refresh type for ArgoCD.
	refreshTypeHard = "hard"
)

// Config holds configuration for the ArgoCD client.
type Config struct {
	// ServerURL is the ArgoCD server URL (e.g., "https://argocd.example.com").
	ServerURL string
	// Token is the ArgoCD API token.
	Token string
	// InsecureSkipVerify skips TLS verification.
	InsecureSkipVerify bool
	// Timeout is the request timeout.
	Timeout time.Duration
}

// Client provides methods to interact with ArgoCD.
type Client struct {
	config     Config
	httpClient *http.Client
	k8sClient  client.Client
}

// NewClient creates a new ArgoCD client.
func NewClient(cfg Config, k8sClient client.Client) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		},
	}

	return &Client{
		config: cfg,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
		k8sClient: k8sClient,
	}
}

// Application represents an ArgoCD Application.
type Application struct {
	Name           string            `json:"name"`
	Namespace      string            `json:"namespace"`
	Project        string            `json:"project"`
	RepoURL        string            `json:"repoURL"`
	Path           string            `json:"path"`
	TargetRevision string            `json:"targetRevision"`
	SyncStatus     string            `json:"syncStatus"`
	HealthStatus   string            `json:"healthStatus"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// SyncRequest represents a sync request.
type SyncRequest struct {
	// Revision is the Git revision to sync to.
	Revision string `json:"revision,omitempty"`
	// Prune enables pruning of resources.
	Prune bool `json:"prune,omitempty"`
	// DryRun performs a dry run.
	DryRun bool `json:"dryRun,omitempty"`
}

// SyncResult represents the result of a sync operation.
type SyncResult struct {
	// Status is the sync status (Synced, OutOfSync, etc.).
	Status string `json:"status"`
	// Message contains any status message.
	Message string `json:"message,omitempty"`
	// Revision is the synced revision.
	Revision string `json:"revision,omitempty"`
}

// RefreshResult represents the result of a refresh operation.
type RefreshResult struct {
	// Success indicates if the refresh was successful.
	Success bool `json:"success"`
	// Message contains any status message.
	Message string `json:"message,omitempty"`
}

// DetectApplication detects the ArgoCD Application managing a workload.
func (c *Client) DetectApplication(ctx context.Context, namespace, workloadName string) (*Application, error) {
	// First, try to find the application by checking the workload's labels
	if c.k8sClient != nil {
		app, err := c.detectFromWorkloadLabels(ctx, namespace, workloadName)
		if err == nil && app != nil {
			return app, nil
		}
	}

	// If we have API access, try to find the application that manages this namespace
	if c.config.ServerURL != "" && c.config.Token != "" {
		return c.findApplicationByResource(ctx, namespace, workloadName)
	}

	return nil, fmt.Errorf("could not detect ArgoCD application for %s/%s", namespace, workloadName)
}

// detectFromWorkloadLabels detects the ArgoCD app from workload labels.
func (c *Client) detectFromWorkloadLabels(ctx context.Context, namespace, workloadName string) (*Application, error) {
	// Try Deployment first
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "Deployment",
	})

	if err := c.k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: workloadName}, deployment); err == nil {
		if appName, ok := deployment.GetLabels()[ArgoCDAppLabel]; ok {
			return &Application{
				Name:      appName,
				Namespace: "argocd", // Default namespace, could be different
			}, nil
		}
	}

	// Try StatefulSet
	statefulSet := &unstructured.Unstructured{}
	statefulSet.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "StatefulSet",
	})

	if err := c.k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: workloadName}, statefulSet); err == nil {
		if appName, ok := statefulSet.GetLabels()[ArgoCDAppLabel]; ok {
			return &Application{
				Name:      appName,
				Namespace: "argocd",
			}, nil
		}
	}

	return nil, fmt.Errorf("workload does not have ArgoCD label")
}

// findApplicationByResource finds an ArgoCD application by its managed resources.
func (c *Client) findApplicationByResource(ctx context.Context, namespace, resourceName string) (*Application, error) {
	// List all applications and check their resources
	apps, err := c.ListApplications(ctx)
	if err != nil {
		return nil, err
	}

	for _, app := range apps {
		// Check if this app manages the namespace
		// This is a simplified check - real implementation would check managed resources
		if app.Namespace == namespace || strings.Contains(app.Path, namespace) {
			return &app, nil
		}
	}

	return nil, fmt.Errorf("no ArgoCD application found managing %s/%s", namespace, resourceName)
}

// ListApplications lists all ArgoCD applications.
func (c *Client) ListApplications(ctx context.Context) ([]Application, error) {
	if c.config.ServerURL == "" {
		return c.listApplicationsFromCluster(ctx)
	}

	url := fmt.Sprintf("%s/api/v1/applications", strings.TrimSuffix(c.config.ServerURL, "/"))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.config.Token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to list applications: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ArgoCD API returned %d: %s", resp.StatusCode, string(body))
	}

	var response struct {
		Items []struct {
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Project string `json:"project"`
				Source  struct {
					RepoURL        string `json:"repoURL"`
					Path           string `json:"path"`
					TargetRevision string `json:"targetRevision"`
				} `json:"source"`
			} `json:"spec"`
			Status struct {
				Sync struct {
					Status string `json:"status"`
				} `json:"sync"`
				Health struct {
					Status string `json:"status"`
				} `json:"health"`
			} `json:"status"`
		} `json:"items"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	apps := make([]Application, 0, len(response.Items))
	for _, item := range response.Items {
		apps = append(apps, Application{
			Name:           item.Metadata.Name,
			Namespace:      item.Metadata.Namespace,
			Project:        item.Spec.Project,
			RepoURL:        item.Spec.Source.RepoURL,
			Path:           item.Spec.Source.Path,
			TargetRevision: item.Spec.Source.TargetRevision,
			SyncStatus:     item.Status.Sync.Status,
			HealthStatus:   item.Status.Health.Status,
			Labels:         item.Metadata.Labels,
		})
	}

	return apps, nil
}

// listApplicationsFromCluster lists applications directly from the cluster.
func (c *Client) listApplicationsFromCluster(ctx context.Context) ([]Application, error) {
	if c.k8sClient == nil {
		return nil, fmt.Errorf("no Kubernetes client available")
	}

	appList := &unstructured.UnstructuredList{}
	appList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   ApplicationGroup,
		Version: ApplicationVersion,
		Kind:    "ApplicationList",
	})

	if err := c.k8sClient.List(ctx, appList); err != nil {
		return nil, fmt.Errorf("failed to list ArgoCD applications: %w", err)
	}

	apps := make([]Application, 0, len(appList.Items))
	for _, item := range appList.Items {
		app := Application{
			Name:      item.GetName(),
			Namespace: item.GetNamespace(),
			Labels:    item.GetLabels(),
		}

		// Extract spec fields
		if spec, ok := item.Object["spec"].(map[string]interface{}); ok {
			if project, ok := spec["project"].(string); ok {
				app.Project = project
			}
			if source, ok := spec["source"].(map[string]interface{}); ok {
				if repoURL, ok := source["repoURL"].(string); ok {
					app.RepoURL = repoURL
				}
				if path, ok := source["path"].(string); ok {
					app.Path = path
				}
				if targetRevision, ok := source["targetRevision"].(string); ok {
					app.TargetRevision = targetRevision
				}
			}
		}

		// Extract status fields
		if status, ok := item.Object["status"].(map[string]interface{}); ok {
			if sync, ok := status["sync"].(map[string]interface{}); ok {
				if syncStatus, ok := sync["status"].(string); ok {
					app.SyncStatus = syncStatus
				}
			}
			if health, ok := status["health"].(map[string]interface{}); ok {
				if healthStatus, ok := health["status"].(string); ok {
					app.HealthStatus = healthStatus
				}
			}
		}

		apps = append(apps, app)
	}

	return apps, nil
}

// Sync triggers a sync for an ArgoCD application.
func (c *Client) Sync(ctx context.Context, appName, appNamespace string, req SyncRequest) (*SyncResult, error) {
	if c.config.ServerURL != "" && c.config.Token != "" {
		return c.syncViaAPI(ctx, appName, req)
	}

	return c.syncViaCluster(ctx, appName, appNamespace)
}

// syncViaAPI syncs an application via the ArgoCD API.
func (c *Client) syncViaAPI(ctx context.Context, appName string, req SyncRequest) (*SyncResult, error) {
	url := fmt.Sprintf("%s/api/v1/applications/%s/sync", strings.TrimSuffix(c.config.ServerURL, "/"), appName)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal sync request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+c.config.Token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to sync application: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ArgoCD sync returned %d: %s", resp.StatusCode, string(respBody))
	}

	return &SyncResult{
		Status:  "Syncing",
		Message: "Sync triggered successfully",
	}, nil
}

// syncViaCluster syncs an application by updating the Application resource.
func (c *Client) syncViaCluster(ctx context.Context, appName, appNamespace string) (*SyncResult, error) {
	if c.k8sClient == nil {
		return nil, fmt.Errorf("no Kubernetes client available")
	}

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   ApplicationGroup,
		Version: ApplicationVersion,
		Kind:    ApplicationKind,
	})

	if err := c.k8sClient.Get(ctx, client.ObjectKey{Namespace: appNamespace, Name: appName}, app); err != nil {
		return nil, fmt.Errorf("failed to get application: %w", err)
	}

	// Add refresh annotation to trigger sync
	annotations := app.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations["argocd.argoproj.io/refresh"] = refreshTypeHard
	app.SetAnnotations(annotations)

	if err := c.k8sClient.Update(ctx, app); err != nil {
		return nil, fmt.Errorf("failed to update application: %w", err)
	}

	return &SyncResult{
		Status:  "Refreshing",
		Message: "Hard refresh triggered",
	}, nil
}

// Refresh triggers a refresh for an ArgoCD application.
func (c *Client) Refresh(ctx context.Context, appName, appNamespace string, hard bool) (*RefreshResult, error) {
	if c.config.ServerURL != "" && c.config.Token != "" {
		return c.refreshViaAPI(ctx, appName, hard)
	}

	return c.refreshViaCluster(ctx, appName, appNamespace, hard)
}

// refreshViaAPI refreshes an application via the ArgoCD API.
func (c *Client) refreshViaAPI(ctx context.Context, appName string, hard bool) (*RefreshResult, error) {
	refreshType := "normal"
	if hard {
		refreshType = refreshTypeHard
	}

	url := fmt.Sprintf("%s/api/v1/applications/%s?refresh=%s", strings.TrimSuffix(c.config.ServerURL, "/"), appName, refreshType)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.config.Token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh application: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ArgoCD refresh returned %d: %s", resp.StatusCode, string(body))
	}

	return &RefreshResult{
		Success: true,
		Message: fmt.Sprintf("%s refresh triggered", refreshType),
	}, nil
}

// refreshViaCluster refreshes an application by updating the Application resource.
func (c *Client) refreshViaCluster(ctx context.Context, appName, appNamespace string, hard bool) (*RefreshResult, error) {
	if c.k8sClient == nil {
		return nil, fmt.Errorf("no Kubernetes client available")
	}

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   ApplicationGroup,
		Version: ApplicationVersion,
		Kind:    ApplicationKind,
	})

	if err := c.k8sClient.Get(ctx, client.ObjectKey{Namespace: appNamespace, Name: appName}, app); err != nil {
		return nil, fmt.Errorf("failed to get application: %w", err)
	}

	// Add refresh annotation
	annotations := app.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	refreshType := "normal"
	if hard {
		refreshType = refreshTypeHard
	}
	annotations["argocd.argoproj.io/refresh"] = refreshType
	app.SetAnnotations(annotations)

	if err := c.k8sClient.Update(ctx, app); err != nil {
		return nil, fmt.Errorf("failed to update application: %w", err)
	}

	return &RefreshResult{
		Success: true,
		Message: fmt.Sprintf("%s refresh triggered", refreshType),
	}, nil
}

// GetApplication retrieves an ArgoCD application by name.
func (c *Client) GetApplication(ctx context.Context, appName, appNamespace string) (*Application, error) {
	if c.config.ServerURL != "" && c.config.Token != "" {
		return c.getApplicationViaAPI(ctx, appName)
	}

	return c.getApplicationFromCluster(ctx, appName, appNamespace)
}

// getApplicationViaAPI retrieves an application via the ArgoCD API.
func (c *Client) getApplicationViaAPI(ctx context.Context, appName string) (*Application, error) {
	url := fmt.Sprintf("%s/api/v1/applications/%s", strings.TrimSuffix(c.config.ServerURL, "/"), appName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.config.Token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get application: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ArgoCD API returned %d: %s", resp.StatusCode, string(body))
	}

	var response struct {
		Metadata struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Project string `json:"project"`
			Source  struct {
				RepoURL        string `json:"repoURL"`
				Path           string `json:"path"`
				TargetRevision string `json:"targetRevision"`
			} `json:"source"`
		} `json:"spec"`
		Status struct {
			Sync struct {
				Status string `json:"status"`
			} `json:"sync"`
			Health struct {
				Status string `json:"status"`
			} `json:"health"`
		} `json:"status"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &Application{
		Name:           response.Metadata.Name,
		Namespace:      response.Metadata.Namespace,
		Project:        response.Spec.Project,
		RepoURL:        response.Spec.Source.RepoURL,
		Path:           response.Spec.Source.Path,
		TargetRevision: response.Spec.Source.TargetRevision,
		SyncStatus:     response.Status.Sync.Status,
		HealthStatus:   response.Status.Health.Status,
		Labels:         response.Metadata.Labels,
	}, nil
}

// getApplicationFromCluster retrieves an application from the cluster.
func (c *Client) getApplicationFromCluster(ctx context.Context, appName, appNamespace string) (*Application, error) {
	if c.k8sClient == nil {
		return nil, fmt.Errorf("no Kubernetes client available")
	}

	item := &unstructured.Unstructured{}
	item.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   ApplicationGroup,
		Version: ApplicationVersion,
		Kind:    ApplicationKind,
	})

	if err := c.k8sClient.Get(ctx, client.ObjectKey{Namespace: appNamespace, Name: appName}, item); err != nil {
		return nil, fmt.Errorf("failed to get application: %w", err)
	}

	app := &Application{
		Name:      item.GetName(),
		Namespace: item.GetNamespace(),
		Labels:    item.GetLabels(),
	}

	// Extract spec and status fields
	if spec, ok := item.Object["spec"].(map[string]interface{}); ok {
		if project, ok := spec["project"].(string); ok {
			app.Project = project
		}
		if source, ok := spec["source"].(map[string]interface{}); ok {
			if repoURL, ok := source["repoURL"].(string); ok {
				app.RepoURL = repoURL
			}
			if path, ok := source["path"].(string); ok {
				app.Path = path
			}
			if targetRevision, ok := source["targetRevision"].(string); ok {
				app.TargetRevision = targetRevision
			}
		}
	}

	if status, ok := item.Object["status"].(map[string]interface{}); ok {
		if sync, ok := status["sync"].(map[string]interface{}); ok {
			if syncStatus, ok := sync["status"].(string); ok {
				app.SyncStatus = syncStatus
			}
		}
		if health, ok := status["health"].(map[string]interface{}); ok {
			if healthStatus, ok := health["status"].(string); ok {
				app.HealthStatus = healthStatus
			}
		}
	}

	return app, nil
}

// IsArgoCDManaged checks if a workload is managed by ArgoCD.
func (c *Client) IsArgoCDManaged(ctx context.Context, namespace, workloadName string) bool {
	app, err := c.DetectApplication(ctx, namespace, workloadName)
	return err == nil && app != nil
}
