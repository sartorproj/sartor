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

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/argocd"
	"github.com/sartorproj/sartor/internal/gitops/provider/github"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
)

const (
	// ConditionTypePrometheusConnected indicates Prometheus connectivity.
	ConditionTypePrometheusConnected = "PrometheusConnected"
	// ConditionTypeGitProviderConnected indicates Git provider connectivity.
	ConditionTypeGitProviderConnected = "GitProviderConnected"
	// ConditionTypeArgoCDConnected indicates ArgoCD connectivity.
	ConditionTypeArgoCDConnected = "ArgoCDConnected"

	// ConnectivityCheckInterval is the interval between connectivity checks.
	ConnectivityCheckInterval = 5 * time.Minute
)

// AtelierReconciler reconciles an Atelier object.
type AtelierReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=ateliers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=ateliers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=ateliers/finalizers,verbs=update
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=tailorings,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch;update;patch

// Reconcile implements the reconciliation loop for Atelier resources.
func (r *AtelierReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Atelier instance
	atelier := &autoscalingv1alpha1.Atelier{}
	if err := r.Get(ctx, req.NamespacedName, atelier); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling Atelier", "name", atelier.Name)

	// Check Prometheus connectivity
	prometheusConnected := r.checkPrometheusConnection(ctx, atelier)
	atelier.Status.PrometheusConnected = prometheusConnected
	atelier.Status.LastPrometheusCheck = &metav1.Time{Time: time.Now()}

	if prometheusConnected {
		r.setCondition(atelier, ConditionTypePrometheusConnected, metav1.ConditionTrue, "Connected", "Prometheus connection successful")
	} else {
		r.setCondition(atelier, ConditionTypePrometheusConnected, metav1.ConditionFalse, "ConnectionFailed", "Failed to connect to Prometheus")
	}

	// Check Git provider connectivity
	gitConnected := r.checkGitProviderConnection(ctx, atelier)
	atelier.Status.GitProviderConnected = gitConnected
	atelier.Status.LastGitProviderCheck = &metav1.Time{Time: time.Now()}

	if gitConnected {
		r.setCondition(atelier, ConditionTypeGitProviderConnected, metav1.ConditionTrue, "Connected", "Git provider connection successful")
	} else {
		r.setCondition(atelier, ConditionTypeGitProviderConnected, metav1.ConditionFalse, "ConnectionFailed", "Failed to connect to Git provider")
	}

	// Check ArgoCD connectivity if enabled
	if atelier.Spec.ArgoCD != nil && atelier.Spec.ArgoCD.Enabled {
		argoCDConnected := r.checkArgoCDConnection(ctx, atelier)
		atelier.Status.ArgoCDConnected = argoCDConnected
		atelier.Status.LastArgoCDCheck = &metav1.Time{Time: time.Now()}

		if argoCDConnected {
			r.setCondition(atelier, ConditionTypeArgoCDConnected, metav1.ConditionTrue, "Connected", "ArgoCD connection successful")
		} else {
			r.setCondition(atelier, ConditionTypeArgoCDConnected, metav1.ConditionFalse, "ConnectionFailed", "Failed to connect to ArgoCD")
		}
	}

	// Count managed Tailorings
	tailoringList := &autoscalingv1alpha1.TailoringList{}
	if err := r.List(ctx, tailoringList); err != nil {
		logger.Error(err, "Failed to list Tailorings")
	} else {
		atelier.Status.ManagedTailorings = int32(len(tailoringList.Items))
	}

	// Update status
	if err := r.Status().Update(ctx, atelier); err != nil {
		logger.Error(err, "Failed to update Atelier status")
		return ctrl.Result{}, err
	}

	logger.Info("Atelier reconciled",
		"prometheusConnected", prometheusConnected,
		"gitConnected", gitConnected,
		"argoCDConnected", atelier.Status.ArgoCDConnected,
		"managedTailorings", atelier.Status.ManagedTailorings)

	return ctrl.Result{RequeueAfter: ConnectivityCheckInterval}, nil
}

// checkPrometheusConnection tests the Prometheus connection.
func (r *AtelierReconciler) checkPrometheusConnection(ctx context.Context, atelier *autoscalingv1alpha1.Atelier) bool {
	logger := log.FromContext(ctx)

	config := prometheus.Config{
		URL:                atelier.Spec.Prometheus.URL,
		InsecureSkipVerify: atelier.Spec.Prometheus.InsecureSkipVerify,
	}

	// Get credentials if configured
	if atelier.Spec.Prometheus.SecretRef != nil {
		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      atelier.Spec.Prometheus.SecretRef.Name,
			Namespace: atelier.Spec.Prometheus.SecretRef.Namespace,
		}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			logger.Error(err, "Failed to get Prometheus secret")
			return false
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

	promClient, err := prometheus.NewClient(config)
	if err != nil {
		logger.Error(err, "Failed to create Prometheus client")
		return false
	}

	if err := promClient.CheckConnection(ctx); err != nil {
		logger.Error(err, "Prometheus connection check failed")
		return false
	}

	return true
}

// checkGitProviderConnection tests the Git provider connection.
func (r *AtelierReconciler) checkGitProviderConnection(ctx context.Context, atelier *autoscalingv1alpha1.Atelier) bool {
	logger := log.FromContext(ctx)

	// Get credentials
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      atelier.Spec.GitProvider.SecretRef.Name,
		Namespace: atelier.Spec.GitProvider.SecretRef.Namespace,
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		logger.Error(err, "Failed to get Git provider secret")
		return false
	}

	token, ok := secret.Data["token"]
	if !ok {
		logger.Error(fmt.Errorf("missing token"), "Git provider secret missing 'token' key")
		return false
	}

	switch atelier.Spec.GitProvider.Type {
	case autoscalingv1alpha1.GitProviderGitHub:
		provider, err := github.New(github.Config{
			Token:   string(token),
			BaseURL: atelier.Spec.GitProvider.BaseURL,
		})
		if err != nil {
			logger.Error(err, "Failed to create GitHub provider")
			return false
		}

		if err := provider.CheckConnection(ctx); err != nil {
			logger.Error(err, "GitHub connection check failed")
			return false
		}

		return true

	default:
		logger.Error(fmt.Errorf("unsupported provider"), "Unsupported Git provider", "type", atelier.Spec.GitProvider.Type)
		return false
	}
}

// checkArgoCDConnection tests the ArgoCD connection.
func (r *AtelierReconciler) checkArgoCDConnection(ctx context.Context, atelier *autoscalingv1alpha1.Atelier) bool {
	logger := log.FromContext(ctx)

	cfg := argocd.Config{
		InsecureSkipVerify: atelier.Spec.ArgoCD.InsecureSkipVerify,
	}

	// If server URL is configured, get the token
	if atelier.Spec.ArgoCD.ServerURL != "" {
		cfg.ServerURL = atelier.Spec.ArgoCD.ServerURL

		if atelier.Spec.ArgoCD.SecretRef != nil {
			secret := &corev1.Secret{}
			secretKey := types.NamespacedName{
				Name:      atelier.Spec.ArgoCD.SecretRef.Name,
				Namespace: atelier.Spec.ArgoCD.SecretRef.Namespace,
			}
			if err := r.Get(ctx, secretKey, secret); err != nil {
				logger.Error(err, "Failed to get ArgoCD secret")
				return false
			}

			if token, ok := secret.Data["token"]; ok {
				cfg.Token = string(token)
			} else {
				logger.Error(fmt.Errorf("missing token"), "ArgoCD secret missing 'token' key")
				return false
			}
		}
	}

	argoClient := argocd.NewClient(cfg, r.Client)

	// Try to list applications to verify connectivity
	apps, err := argoClient.ListApplications(ctx)
	if err != nil {
		logger.Error(err, "ArgoCD connection check failed")
		return false
	}

	logger.Info("ArgoCD connection successful", "applicationCount", len(apps))
	return true
}

// setCondition sets a condition on the Atelier status.
func (r *AtelierReconciler) setCondition(atelier *autoscalingv1alpha1.Atelier, conditionType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
	meta.SetStatusCondition(&atelier.Status.Conditions, condition)
}

// SetupWithManager sets up the controller with the Manager.
func (r *AtelierReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.Atelier{}).
		Named("atelier").
		Complete(r)
}
