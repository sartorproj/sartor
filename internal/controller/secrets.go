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
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// PodNamespaceEnv is the environment variable (set from the downward API)
	// holding the controller's namespace.
	PodNamespaceEnv = "POD_NAMESPACE"

	// serviceAccountNamespaceFile holds the pod's namespace when running in-cluster.
	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	// ReasonSecretNamespaceNotAllowed is the condition reason used when a
	// secretRef points outside the controller namespace.
	ReasonSecretNamespaceNotAllowed = "SecretNamespaceNotAllowed"
)

// SecretNamespaceError reports a secretRef the controller refuses to read.
type SecretNamespaceError struct {
	Ref                 corev1.SecretReference
	ControllerNamespace string
}

func (e *SecretNamespaceError) Error() string {
	if e.ControllerNamespace == "" {
		return fmt.Sprintf("refusing to read Secret %s/%s: controller namespace is unknown (set %s)",
			e.Ref.Namespace, e.Ref.Name, PodNamespaceEnv)
	}
	return fmt.Sprintf("refusing to read Secret %s/%s: secretRef.namespace must be empty or %q (the controller namespace)",
		e.Ref.Namespace, e.Ref.Name, e.ControllerNamespace)
}

// IsSecretNamespaceError reports whether err is a *SecretNamespaceError.
func IsSecretNamespaceError(err error) bool {
	var nsErr *SecretNamespaceError
	return errors.As(err, &nsErr)
}

// ResolveControllerNamespace returns the namespace the controller runs in:
// $POD_NAMESPACE, else the in-cluster service account namespace file, else "".
func ResolveControllerNamespace() string {
	return resolveControllerNamespace(os.Getenv, os.ReadFile)
}

func resolveControllerNamespace(getenv func(string) string, readFile func(string) ([]byte, error)) string {
	if ns := strings.TrimSpace(getenv(PodNamespaceEnv)); ns != "" {
		return ns
	}
	if data, err := readFile(serviceAccountNamespaceFile); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

// secretKeyFor returns the key of the Secret a secretRef may be read from.
// An empty secretRef.namespace means the controller namespace; any other
// namespace is rejected so credentials can only come from the namespace the
// controller is granted Secret access in.
func secretKeyFor(controllerNamespace string, ref corev1.SecretReference) (types.NamespacedName, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = controllerNamespace
	}
	if controllerNamespace == "" || ns != controllerNamespace {
		return types.NamespacedName{}, &SecretNamespaceError{Ref: ref, ControllerNamespace: controllerNamespace}
	}
	return types.NamespacedName{Namespace: ns, Name: ref.Name}, nil
}

// getCredentialSecret reads a credentials Secret referenced by the Atelier,
// enforcing the controller-namespace restriction. The reader should not be
// backed by a cluster-wide Secret informer.
func getCredentialSecret(ctx context.Context, c client.Reader, controllerNamespace string, ref corev1.SecretReference) (*corev1.Secret, error) {
	key, err := secretKeyFor(controllerNamespace, ref)
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	return secret, nil
}
