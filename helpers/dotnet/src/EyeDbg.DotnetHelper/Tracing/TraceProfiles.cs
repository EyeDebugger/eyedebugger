// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Diagnostics.Tracing;
using Microsoft.Diagnostics.NETCore.Client;

namespace EyeDbg.DotnetHelper.Tracing;

/// <summary>What a trace profile asks the runtime for.</summary>
/// <param name="Name">cpu or gc.</param>
/// <param name="Providers">The EventPipe providers, levels and keywords.</param>
/// <param name="RequestRundown">Whether stopping the session writes the method and module names (rundown).</param>
internal sealed record TraceProfile(string Name, IReadOnlyList<EventPipeProvider> Providers, bool RequestRundown);

/// <summary>
/// The trace profiles (docs/adr/0016, P2-M8). Keywords are minimal on purpose: no Exception
/// keyword (exception messages are program values) and no other payloads beyond names and GC
/// data, so a trace holds method, module and type names, GC events and the runtime's own
/// process information (its command line).
/// </summary>
internal static class TraceProfiles
{
    public const string Cpu = "cpu";
    public const string Gc = "gc";

    /// <summary>The runtime's EventPipe buffer in the target (dotnet-trace's default), allocated as needed.</summary>
    public const int BufferMB = 256;

    public const string RuntimeProvider = "Microsoft-Windows-DotNETRuntime";
    public const string SampleProfilerProvider = "Microsoft-DotNETCore-SampleProfiler";

    /// <summary>The runtime provider's GC keyword.</summary>
    public const long GcKeyword = 0x1;

    /// <summary>GC | Loader | Jit: GC start and end, and the module and method loads that name the samples' frames.</summary>
    public const long CpuRuntimeKeywords = 0x1 | 0x8 | 0x10;

    /// <summary>The profile with that name, or null.</summary>
    public static TraceProfile? Of(string name) => name switch
    {
        // The sample profiler pauses the program about a thousand times a second to walk every
        // managed thread's stack; the rundown at stop names the methods in those stacks.
        Cpu => new TraceProfile(
            Cpu,
            [
                new EventPipeProvider(SampleProfilerProvider, EventLevel.Informational, 0),
                new EventPipeProvider(RuntimeProvider, EventLevel.Informational, CpuRuntimeKeywords),
            ],
            RequestRundown: true),

        // Verbose adds GCAllocationTick: an event per about 100 KB allocated, with the type's name.
        Gc => new TraceProfile(
            Gc,
            [new EventPipeProvider(RuntimeProvider, EventLevel.Verbose, GcKeyword)],
            RequestRundown: false),
        _ => null,
    };
}
