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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/provider"
	"github.com/sartorproj/sartor/internal/gitops/provider/github"
)

// Keys read from the Git provider Secret referenced by the Atelier.
const (
	gitSecretKeyToken          = "token"
	gitSecretKeyAppID          = "appId"
	gitSecretKeyInstallationID = "installationId"
	gitSecretKeyPrivateKey     = "privateKey"
)

// GitProviderFactory creates a Git provider from GitHub credentials.
type GitProviderFactory func(cfg github.Config) (provider.Provider, error)

// DefaultGitProviderFactory creates a real GitHub provider.
func DefaultGitProviderFactory(cfg github.Config) (provider.Provider, error) {
	return github.New(cfg)
}

// loadGitHubConfig reads the Git credentials Secret referenced by the Atelier.
//
// The Secret is fetched with a single GET through reader, which must not be
// backed by an informer (use the manager's APIReader), and only from
// controllerNamespace: the controller is granted "get" on Secrets in its own
// namespace and nothing else, so a secretRef elsewhere is rejected before any
// API call.
func loadGitHubConfig(
	ctx context.Context,
	reader client.Reader,
	atelier *autoscalingv1alpha1.Atelier,
	controllerNamespace string,
) (github.Config, error) {
	gp := atelier.Spec.GitProvider
	if gp.Type != autoscalingv1alpha1.GitProviderGitHub {
		return github.Config{}, fmt.Errorf("git provider %q is not supported yet (only github)", gp.Type)
	}

	ns, err := gitSecretNamespace(gp.SecretRef, controllerNamespace)
	if err != nil {
		return github.Config{}, err
	}

	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Name: gp.SecretRef.Name, Namespace: ns}, secret); err != nil {
		return github.Config{}, fmt.Errorf("failed to get Git provider secret %s/%s: %w", ns, gp.SecretRef.Name, err)
	}

	cfg := github.Config{BaseURL: gp.BaseURL}
	if token := strings.TrimSpace(string(secret.Data[gitSecretKeyToken])); token != "" {
		cfg.Token = token
	}
	if v, ok := secret.Data[gitSecretKeyAppID]; ok {
		id, err := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
		if err != nil {
			return github.Config{}, fmt.Errorf("git provider secret: invalid %s: %w", gitSecretKeyAppID, err)
		}
		cfg.AppID = id
	}
	if v, ok := secret.Data[gitSecretKeyInstallationID]; ok {
		id, err := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
		if err != nil {
			return github.Config{}, fmt.Errorf("git provider secret: invalid %s: %w", gitSecretKeyInstallationID, err)
		}
		cfg.InstallationID = id
	}
	cfg.PrivateKey = secret.Data[gitSecretKeyPrivateKey]

	if err := cfg.Validate(); err != nil {
		return github.Config{}, fmt.Errorf("git provider secret %s/%s: %w", ns, gp.SecretRef.Name, err)
	}
	return cfg, nil
}

// gitSecretNamespace resolves the namespace of the Git credentials Secret and
// rejects any namespace other than the controller's own.
func gitSecretNamespace(ref corev1.SecretReference, controllerNamespace string) (string, error) {
	if ref.Name == "" {
		return "", fmt.Errorf("gitProvider.secretRef.name is required")
	}
	if controllerNamespace == "" {
		return "", fmt.Errorf("controller namespace is unknown (set POD_NAMESPACE); refusing to read Git credentials")
	}
	if ref.Namespace != "" && ref.Namespace != controllerNamespace {
		return "", fmt.Errorf("gitProvider.secretRef.namespace %q is not allowed: the Secret must be in the controller namespace %q",
			ref.Namespace, controllerNamespace)
	}
	return controllerNamespace, nil
}
