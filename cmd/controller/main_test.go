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
	"testing"

	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// recordingManager records the runnables (controllers) added to it.
type recordingManager struct {
	manager.Manager
	added []manager.Runnable
}

func (m *recordingManager) Add(r manager.Runnable) error {
	m.added = append(m.added, r)
	return nil
}

func TestSetupOOMPatching(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		wantAdded  int
		nilManager bool
	}{
		{
			name:       "disabled registers nothing and never touches the manager",
			enabled:    false,
			nilManager: true,
		},
		{
			name:      "enabled registers the OOM controller",
			enabled:   true,
			wantAdded: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.nilManager {
				// Any call on a nil embedded Manager panics, proving no
				// controller, watch or client is set up.
				if err := setupOOMPatching(&recordingManager{}, tt.enabled); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}

			mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
				Scheme:  scheme,
				Metrics: metricsserver.Options{BindAddress: "0"},
			})
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			rec := &recordingManager{Manager: mgr}
			if err := setupOOMPatching(rec, tt.enabled); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(rec.added) != tt.wantAdded {
				t.Fatalf("added %d runnables, want %d", len(rec.added), tt.wantAdded)
			}
		})
	}
}
