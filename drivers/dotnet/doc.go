// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package dotnet is the .NET language driver: project detection, build, and
// launch and attach configuration for netcoredbg (docs/DESIGN.md §8; SharpDbg
// comes later), which it finds as the adapter manifest serving dotnet
// describes (internal/adapters/manifests/netcoredbg.json), 'dotnet test' runs with VSTest's host debugging (testrun.go)
// and the check that keeps eval from visibly changing the program
// (SideEffects). It also finds and specifies the .NET side helper
// (helper.go, helpers/dotnet): its lookup, protocol and wire types, which
// 'eyedbg dotnet …' runs through internal/helper. It must never download,
// detect or drive vsdbg
// (docs/adr/0005-never-use-vsdbg.md).
package dotnet
