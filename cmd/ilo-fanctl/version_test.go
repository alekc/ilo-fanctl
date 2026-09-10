package main

import (
	"runtime/debug"
	"testing"
)

func buildInfo(v string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: v}}, true
	}
}

func TestResolveVersion(t *testing.T) {
	tests := []struct {
		name    string
		current string
		read    func() (*debug.BuildInfo, bool)
		want    string
	}{
		{
			// The release path. ldflags won, and nothing may overwrite it:
			// the tag is more specific than anything build info carries.
			name:    "an ldflags version is kept",
			current: "v1.2.3",
			read:    buildInfo("v9.9.9"),
			want:    "v1.2.3",
		},
		{
			// The whole point: go install <module>@<tag>.
			name:    "go install picks up the module version",
			current: "dev",
			read:    buildInfo("v1.2.3"),
			want:    "v1.2.3",
		},
		{
			// go build inside a checkout. "(devel)" is not more useful than
			// "dev", and swapping one for the other would only make the
			// metric harder to read.
			name:    "a devel build stays dev",
			current: "dev",
			read:    buildInfo("(devel)"),
			want:    "dev",
		},
		{
			name:    "an empty module version stays dev",
			current: "dev",
			read:    buildInfo(""),
			want:    "dev",
		},
		{
			// ReadBuildInfo returns false for a binary built without module
			// support. Rare, but it must not panic on the nil.
			name:    "no build info at all stays dev",
			current: "dev",
			read:    func() (*debug.BuildInfo, bool) { return nil, false },
			want:    "dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveVersion(tt.current, tt.read); got != tt.want {
				t.Errorf("resolveVersion(%q) = %q, want %q", tt.current, got, tt.want)
			}
		})
	}
}
