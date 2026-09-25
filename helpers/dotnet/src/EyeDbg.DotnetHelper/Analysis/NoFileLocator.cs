// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Collections.Immutable;
using System.Runtime.InteropServices;
using Microsoft.Diagnostics.Runtime;

namespace EyeDbg.DotnetHelper.Analysis;

/// <summary>
/// A file locator that finds nothing. Without one, ClrMD builds its own from symbol servers
/// (msdl.microsoft.com, _NT_SYMBOL_PATH) with a cache in the shared temp directory, and would
/// load a DAC downloaded or planted there. The helper names the DAC itself (<see cref="DumpLoader"/>).
/// </summary>
internal sealed class NoFileLocator : IFileLocator
{
    public string? FindPEImage(string fileName, int buildTimeStamp, int imageSize, bool checkProperties) => null;

    public string? FindPEImage(string fileName, SymbolProperties archivedUnder, ImmutableArray<byte> buildIdOrUUID, OSPlatform originalPlatform, bool checkProperties) => null;
}
