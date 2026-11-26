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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

var yamlBlock = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// TestSamplesAndDocsDecodeStrictly keeps the samples and the documentation
// examples in sync with the API: unknown fields and invalid durations fail.
func TestSamplesAndDocsDecodeStrictly(t *testing.T) {
	var docs []string
	samples, err := filepath.Glob("../../config/samples/*.yaml")
	if err != nil || len(samples) == 0 {
		t.Fatalf("no samples found: %v", err)
	}
	for _, f := range samples {
		if filepath.Base(f) == "kustomization.yaml" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, d := range strings.Split(string(b), "\n---\n") {
			docs = append(docs, fmt.Sprintf("%s document %d\x00%s", f, i, d))
		}
	}
	markdown, _ := filepath.Glob("../../docs/*.md")
	for _, f := range append(markdown, "../../README.md") {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, m := range yamlBlock.FindAllStringSubmatch(string(b), -1) {
			for _, d := range strings.Split(m[1], "\n---\n") {
				docs = append(docs, fmt.Sprintf("%s yaml block %d\x00%s", f, i, d))
			}
		}
	}

	checked := 0
	for _, entry := range docs {
		name, doc, _ := strings.Cut(entry, "\x00")
		if !strings.Contains(doc, "apiVersion: autoscaling.sartorproj.io/") {
			continue
		}
		var obj interface{}
		switch {
		case strings.Contains(doc, "kind: Atelier"):
			obj = &Atelier{}
		case strings.Contains(doc, "kind: Tailoring"):
			obj = &Tailoring{}
		case strings.Contains(doc, "kind: FitProfile"):
			obj = &FitProfile{}
		default:
			t.Errorf("%s: unknown kind", name)
			continue
		}
		checked++
		if err := yaml.UnmarshalStrict([]byte(doc), obj); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if checked < 10 {
		t.Fatalf("only %d examples checked; the extraction is broken", checked)
	}
}
