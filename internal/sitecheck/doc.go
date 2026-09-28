// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package sitecheck is a test-only guard over what site/ publishes at
// https://eyedbg.izzat.dev: internal/sitecheck/site_test.go walks every text
// file under site/, except site/README.md (the one file
// .github/workflows/pages.yml doesn't publish), and fails, word-bounded and
// case-insensitive, on the name of a competing debugger or IDE eyedbg
// doesn't ship, or on "Visual Studio" not followed by "Code" (AGENTS.md rule
// 1, docs/DESIGN.md's naming rule). Nothing here is imported outside its own
// tests.
package sitecheck
