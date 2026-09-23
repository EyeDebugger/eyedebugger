// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package adapters describes debug adapters with declarative manifests
// (docs/adapter-manifests.md, ADR 0011): the schema and its validation
// (manifest.go, validate.go), the launch/attach templates (template.go),
// the registry of bundled and user manifests with its permission check
// (registry.go, trust_*.go), finding and installing adapters (find.go,
// install.go: downloads are pinned to one release and verified by size
// and SHA-256 before anything is extracted), and the Python runtime that
// Python-hosted adapters run on (python.go). Bundled manifests live in
// manifests/. No adapter or language is named in Go code here.
package adapters
