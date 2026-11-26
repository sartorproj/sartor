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
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/argocd"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
)

const (
	// ConditionTypePrometheusConnected indicates Prometheus connectivity.
	ConditionTypePrometheusConnected = "PrometheusConnected"
	// ConditionTypeGitProviderConnected indicates Git provider connectivity.
	ConditionTypeGitProviderConnected = "GitProviderConnected"
	// ConditionTypeArgoCDConnected indicates ArgoCD connectivity.
	ConditionTypeArgoCDConnected = "ArgoCDConnected"
	// ConditionTypeSecretRefsValid indicates all secretRefs point to Secrets
	// the controller is allowed to read.
	ConditionTypeSecretRefsValid = "SecretRefsValid"

	// ConnectivityCheckInterval is the interval between connectivity checks.
	ConnectivityCheckInterval = 5 * time.Minute
)

// AtelierReconciler reconciles an Atelier object.
type AtelierReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ControllerNamespace is the only namespace credential Secrets are read from.
	ControllerNamespace string
	// EnableGitWriteBack enables the Git provider connectivity check. When false
	// the controller never contacts the Git provider.
	EnableGitWriteBack bool
	// NewGitProvider creates the Git provider; defaults to DefaultGitProviderFactory.
	NewGitProvider GitProviderFactory
}

// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=ateliers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=ateliers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=ateliers/finalizers,verbs=update
// +kubebuilder:rbac:groups=autoscaling.sartorproj.io,resources=tailorings,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
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

	if err := validateSecretRefs(atelier, r.ControllerNamespace); err != nil {
		logger.Error(err, "Atelier references a Secret outside the controller namespace")
		r.setCondition(atelier, ConditionTypeSecretRefsValid, metav1.ConditionFalse, ReasonSecretNamespaceNotAllowed, err.Error())
	} else {
		r.setCondition(atelier, ConditionTypeSecretRefsValid, metav1.ConditionTrue, "Valid", "All secretRefs are in the controller namespace")
	}

	// Check Prometheus connectivity
	promErr := r.checkPrometheusConnection(ctx, atelier)
	prometheusConnected := promErr == nil
	atelier.Status.PrometheusConnected = prometheusConnected
	atelier.Status.LastPrometheusCheck = &metav1.Time{Time: time.Now()}

	switch {
	case prometheusConnected:
		r.setCondition(atelier, ConditionTypePrometheusConnected, metav1.ConditionTrue, "Connected", "Prometheus connection successful")
	case IsSecretNamespaceError(promErr):
		r.setCondition(atelier, ConditionTypePrometheusConnected, metav1.ConditionFalse, ReasonSecretNamespaceNotAllowed, promErr.Error())
	default:
		r.setCondition(atelier, ConditionTypePrometheusConnected, metav1.ConditionFalse, "ConnectionFailed",
			fmt.Sprintf("Failed to connect to Prometheus: %v", promErr))
	}

	// Check Git provider connectivity
	if r.EnableGitWriteBack {
		gitConnected, gitErr := r.checkGitProviderConnection(ctx, atelier)
		atelier.Status.GitProviderConnected = gitConnected
		atelier.Status.LastGitProviderCheck = &metav1.Time{Time: time.Now()}

		switch {
		case gitConnected:
			r.setCondition(atelier, ConditionTypeGitProviderConnected, metav1.ConditionTrue, "Connected", "Git provider connection successful")
		case IsSecretNamespaceError(gitErr):
			r.setCondition(atelier, ConditionTypeGitProviderConnected, metav1.ConditionFalse, ReasonSecretNamespaceNotAllowed, gitErr.Error())
		default:
			r.setCondition(atelier, ConditionTypeGitProviderConnected, metav1.ConditionFalse, "ConnectionFailed", gitErr.Error())
		}
	} else {
		atelier.Status.GitProviderConnected = false
		r.setCondition(atelier, ConditionTypeGitProviderConnected, metav1.ConditionFalse, "WriteBackDisabled",
			"Git write-back is disabled (--enable-git-writeback=false); connectivity not checked")
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
		"gitConnected", atelier.Status.GitProviderConnected,
		"argoCDConnected", atelier.Status.ArgoCDConnected,
		"managedTailorings", atelier.Status.ManagedTailorings)

	return ctrl.Result{RequeueAfter: ConnectivityCheckInterval}, nil
}

// validateSecretRefs checks that every secretRef in the Atelier can be read
// under the controller-namespace restriction.
func validateSecretRefs(atelier *autoscalingv1alpha1.Atelier, controllerNamespace string) error {
	refs := []*corev1.SecretReference{atelier.Spec.Prometheus.SecretRef, &atelier.Spec.GitProvider.SecretRef}
	if atelier.Spec.ArgoCD != nil {
		refs = append(refs, atelier.Spec.ArgoCD.SecretRef)
	}
	var errs []error
	for _, ref := range refs {
		if ref == nil || ref.Name == "" {
			continue
		}
		if _, err := secretKeyFor(controllerNamespace, *ref); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkPrometheusConnection tests the Prometheus connection. It returns nil
// when the server answers queries.
func (r *AtelierReconciler) checkPrometheusConnection(ctx context.Context, atelier *autoscalingv1alpha1.Atelier) error {
	logger := log.FromContext(ctx)

	config := prometheus.Config{
		URL:                atelier.Spec.Prometheus.URL,
		InsecureSkipVerify: atelier.Spec.Prometheus.InsecureSkipVerify,
	}

	// Get credentials if configured
	if atelier.Spec.Prometheus.SecretRef != nil {
		secret, err := getCredentialSecret(ctx, r.Client, r.ControllerNamespace, *atelier.Spec.Prometheus.SecretRef)
		if err != nil {
			logger.Error(err, "Failed to get Prometheus secret")
			return err
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
		return err
	}

	if err := promClient.CheckConnection(ctx); err != nil {
		logger.Error(err, "Prometheus connection check failed")
		return err
	}

	return nil
}

// checkGitProviderConnection tests the Git provider connection.
func (r *AtelierReconciler) checkGitProviderConnection(ctx context.Context, atelier *autoscalingv1alpha1.Atelier) (bool, error) {
	logger := log.FromContext(ctx)

	cfg, err := loadGitHubConfig(ctx, r.Client, atelier, r.ControllerNamespace)
	if err != nil {
		logger.Error(err, "Failed to load Git provider credentials")
		return false, err
	}

	newProvider := r.NewGitProvider
	if newProvider == nil {
		newProvider = DefaultGitProviderFactory
	}
	gitProvider, err := newProvider(cfg)
	if err != nil {
		logger.Error(err, "Failed to create Git provider")
		return false, err
	}
	if err := gitProvider.CheckConnection(ctx); err != nil {
		logger.Error(err, "Git provider connection check failed")
		return false, err
	}
	return true, nil
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
			secret, err := getCredentialSecret(ctx, r.Client, r.ControllerNamespace, *atelier.Spec.ArgoCD.SecretRef)
			if err != nil {
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
	// Status writes (connectivity timestamps) must not retrigger the
	// reconcile; the periodic requeue drives the connectivity checks.
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.Atelier{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("atelier").
		Complete(r)
}
