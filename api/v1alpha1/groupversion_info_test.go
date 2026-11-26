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

package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestAddToSchemeRegistersAllKinds(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	tests := []struct {
		obj  runtime.Object
		kind string
	}{
		{&Atelier{}, "Atelier"},
		{&AtelierList{}, "AtelierList"},
		{&FitProfile{}, "FitProfile"},
		{&FitProfileList{}, "FitProfileList"},
		{&Tailoring{}, "Tailoring"},
		{&TailoringList{}, "TailoringList"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			gvks, _, err := s.ObjectKinds(tt.obj)
			if err != nil {
				t.Fatalf("ObjectKinds: %v", err)
			}
			if len(gvks) != 1 || gvks[0] != GroupVersion.WithKind(tt.kind) {
				t.Fatalf("got %v, want %v", gvks, GroupVersion.WithKind(tt.kind))
			}
		})
	}

	if !s.IsVersionRegistered(GroupVersion) {
		t.Fatalf("%v not registered", GroupVersion)
	}
}
