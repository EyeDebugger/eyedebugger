// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package dotnet is the .NET language driver: project detection, build, and
// launch configuration for netcoredbg (default) and SharpDbg
// (docs/DESIGN.md §8). It must never download, detect or drive vsdbg
// (docs/adr/0005-never-use-vsdbg.md).
//
// Not implemented yet (docs/DESIGN.md §13, milestone 2).
package dotnet
