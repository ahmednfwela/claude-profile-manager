// Package channel embeds the self-contained channel server bundle so a released
// cpm binary can run `cpm channel serve` without a repo checkout on disk.
// goreleaser ships bare binaries (format: binary silently ignores archive
// `files:` — proven empirically on the v0.5.0 release assets), so the bundle
// must travel inside the binary itself.
package channel

import _ "embed"

//go:embed httpchan.bundle.mjs
var BundleMJS []byte
