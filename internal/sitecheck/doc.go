// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package sitecheck is a test-only guard over what site/ publishes at
// https://eyedbg.izzat.dev: internal/sitecheck/site_test.go walks every text
// file under site/, except site/README.md (the one file
// .github/workflows/pages.yml doesn't publish), and fails, word-bounded and
// case-insensitive, on the name of a competing debugger or IDE eyedbg
// doesn't ship, or on "Visual Studio" not followed by "Code" (AGENTS.md rule
// 1). The same walk also fails on an em dash (U+2014) or a banned AI-tell
// word (seamless, powerful, robust, leverage, unlock; not "delve", the Go
// adapter's name), per the site's tone rule (p2-discoverability plan, step
// 7). It also keeps the agent install prompt in sync: site/install-prompt.txt
// is canonical, and the copies fenced between
// "<!-- install-prompt:start -->"/"<!-- install-prompt:end -->" in README.md
// and site/llms.txt, and the escaped copy in site/index.html's
// <code id="install-prompt">, must all match it byte for byte; site/llms.txt
// must name the real VS Code extension id, publisher and debug type read
// from extensions/vscode/package.json (D6/D10 in the p2-discoverability
// plan; index.html joined the sync check in step 6). Nothing here is
// imported outside its own tests.
package sitecheck
