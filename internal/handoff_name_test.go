package internal

import (
	"os"
	"path/filepath"
	"testing"
)

// State shapes mirror real daemon job files: "name" is a top-level field and
// may be a string, empty, null, or absent entirely (verified against live
// <profile>/jobs/<short>/state.json files).
func TestResolveHandoffName(t *testing.T) {
	const short = "87a43e44"

	cases := []struct {
		name      string
		override  string
		stateJSON string // "" means: do not create jobs/<short>/state.json at all
		want      string
	}{
		{
			name:      "origin name carries over verbatim",
			stateJSON: `{"state":"working","name":"asgf-work-r2","nameSource":"user"}`,
			want:      "asgf-work-r2",
		},
		{
			name:      "empty origin name falls back to slug",
			stateJSON: `{"state":"working","name":""}`,
			want:      "handoff-" + short,
		},
		{
			name:      "null origin name falls back to slug",
			stateJSON: `{"state":"working","name":null}`,
			want:      "handoff-" + short,
		},
		{
			name:      "absent name field falls back to slug",
			stateJSON: `{"state":"working"}`,
			want:      "handoff-" + short,
		},
		{
			name: "missing state.json falls back to slug",
			want: "handoff-" + short,
		},
		{
			name:      "malformed state.json falls back to slug",
			stateJSON: `{not json`,
			want:      "handoff-" + short,
		},
		{
			name:      "--name override wins over origin name",
			override:  "my-explicit-name",
			stateJSON: `{"state":"working","name":"asgf-work-r2"}`,
			want:      "my-explicit-name",
		},
		{
			name:     "--name override wins with no state.json",
			override: "my-explicit-name",
			want:     "my-explicit-name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fromDir := t.TempDir()
			if tc.stateJSON != "" {
				jobDir := filepath.Join(fromDir, "jobs", short)
				if err := os.MkdirAll(jobDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(jobDir, "state.json"), []byte(tc.stateJSON), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := ResolveHandoffName(tc.override, fromDir, short); got != tc.want {
				t.Fatalf("ResolveHandoffName(%q, ., %q) = %q, want %q", tc.override, short, got, tc.want)
			}
		})
	}
}
