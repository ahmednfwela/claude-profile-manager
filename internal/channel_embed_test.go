package internal

import (
	"bytes"
	"os"
	"testing"

	"github.com/jakubkontra/cpm/channel"
)

// The embedded bundle is what a RELEASED binary serves from (goreleaser ships no
// channel/ dir — its format:binary ignores archive `files:`, proven on v0.5.0).
// An empty or truncated embed would only surface at `cpm channel serve` time on
// a user's machine, so gate it here.
func TestEmbeddedBundlePresent(t *testing.T) {
	if len(channel.BundleMJS) == 0 {
		t.Fatal("embedded httpchan bundle is empty")
	}
	if len(channel.BundleMJS) < 100_000 {
		t.Fatalf("embedded bundle suspiciously small (%d bytes) — SDK inlining missing?", len(channel.BundleMJS))
	}
}

func TestExtractEmbeddedBundle(t *testing.T) {
	p, err := extractEmbeddedBundle()
	if err != nil {
		t.Fatalf("extractEmbeddedBundle: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read extracted bundle: %v", err)
	}
	if !bytes.Equal(got, channel.BundleMJS) {
		t.Fatalf("extracted bundle differs from embedded (len %d vs %d)", len(got), len(channel.BundleMJS))
	}
	// Second call must hit the cached copy and agree on the path.
	p2, err := extractEmbeddedBundle()
	if err != nil {
		t.Fatalf("second extract: %v", err)
	}
	if p2 != p {
		t.Fatalf("cache path unstable: %q vs %q", p, p2)
	}
}
