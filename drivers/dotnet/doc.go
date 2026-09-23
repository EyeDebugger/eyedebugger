// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package dotnet is the .NET language driver: project detection, build, and
// launch and attach configuration for netcoredbg (docs/DESIGN.md §8; SharpDbg
// comes later), 'dotnet test' runs with VSTest's host debugging (testrun.go)
// and the check that keeps eval from visibly changing the program
// (SideEffects). It must never download, detect or drive vsdbg
// (docs/adr/0005-never-use-vsdbg.md).
package dotnet
