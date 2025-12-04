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

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/server/api"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(autoscalingv1alpha1.AddToScheme(scheme))
}

func main() {
	var addr string
	var apiKeySecretName string
	var apiKeySecretNamespace string
	var enableCORS bool

	flag.StringVar(&addr, "addr", ":8080", "The address the server binds to.")
	flag.StringVar(&apiKeySecretName, "api-key-secret-name", "sartor-api-keys", "Name of the Secret containing API keys.")
	flag.StringVar(&apiKeySecretNamespace, "api-key-secret-namespace", "sartor-system", "Namespace of the API key Secret.")
	flag.BoolVar(&enableCORS, "enable-cors", true, "Enable CORS headers for browser access.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Create Kubernetes client
	cfg, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to get kubeconfig")
		os.Exit(1)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}

	// Load API keys from Secret
	apiKeys := loadAPIKeys(k8sClient, apiKeySecretNamespace, apiKeySecretName)
	if len(apiKeys) == 0 {
		setupLog.Info("Warning: No API keys loaded. API will be unauthenticated!")
	} else {
		setupLog.Info("Loaded API keys", "count", len(apiKeys))
	}

	// Create API handler
	apiHandler := api.NewHandler(k8sClient)

	// Create HTTP server with routes
	mux := http.NewServeMux()

	// Health endpoints
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	// Register API routes
	apiHandler.RegisterRoutes(mux)

	// MCP endpoint placeholder
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		fmt.Fprint(w, `{"error": "not implemented", "message": "MCP server will be available in Phase 4"}`)
	})

	// Apply middleware
	var handler http.Handler = mux

	// Add CORS if enabled
	if enableCORS {
		handler = api.CORSMiddleware(handler)
	}

	// Add authentication if API keys are configured
	if len(apiKeys) > 0 {
		handler = api.AuthMiddleware(apiKeys, handler)
	}

	// Create server
	server := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		setupLog.Info("starting server", "addr", addr, "cors", enableCORS, "auth", len(apiKeys) > 0)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "server failed")
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	setupLog.Info("shutting down server")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		setupLog.Error(err, "server forced to shutdown")
		os.Exit(1)
	}

	setupLog.Info("server stopped")
}

// loadAPIKeys loads API keys from a Kubernetes Secret.
func loadAPIKeys(c client.Client, namespace, name string) map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		setupLog.Info("Could not load API key secret", "error", err.Error())
		return nil
	}

	keys := make(map[string]bool)
	for keyName, value := range secret.Data {
		if strings.HasPrefix(keyName, "key-") || keyName == "api-key" {
			keys[string(value)] = true
		}
	}

	return keys
}
