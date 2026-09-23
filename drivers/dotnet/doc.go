// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package dotnet is the .NET language driver: project detection, build, and
// launch configuration for netcoredbg (docs/DESIGN.md §8; SharpDbg comes
// later). It must never download, detect or drive vsdbg
// (docs/adr/0005-never-use-vsdbg.md).
package dotnet
