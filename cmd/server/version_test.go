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
	"flag"
	"testing"
)

func TestVersionString(t *testing.T) {
	origVersion, origCommit, origDate := version, commit, date
	t.Cleanup(func() { version, commit, date = origVersion, origCommit, origDate })

	tests := []struct {
		name                  string
		version, commit, date string
		want                  string
	}{
		{
			name:    "defaults when ldflags are not set",
			version: "dev", commit: "unknown", date: "unknown",
			want: "sartor-server dev (commit unknown, built unknown)",
		},
		{
			name:    "release build",
			version: "0.1.0", commit: "60374c4", date: "2026-11-26T12:00:00Z",
			want: "sartor-server 0.1.0 (commit 60374c4, built 2026-11-26T12:00:00Z)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version, commit, date = tt.version, tt.commit, tt.date
			if got := versionString(); got != tt.want {
				t.Errorf("versionString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVersionFlagRegistered(t *testing.T) {
	f := flag.CommandLine.Lookup("version")
	if f == nil {
		t.Fatal("--version flag is not registered")
	}
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !bf.IsBoolFlag() {
		t.Error("--version must be a boolean flag so it works without a value")
	}
}
