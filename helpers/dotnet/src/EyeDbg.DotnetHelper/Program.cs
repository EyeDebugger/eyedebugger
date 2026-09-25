// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Methods;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper;

internal static class Program
{
    private static async Task<int> Main()
    {
        // stdout carries the protocol only: whatever a library writes to Console lands on stderr.
        Console.SetOut(Console.Error);
        using var output = Console.OpenStandardOutput();
        using var input = Console.OpenStandardInput();

        var methods = new Dictionary<string, MethodHandler>(StringComparer.Ordinal)
        {
            [ProcessesMethod.Method] = ProcessesMethod.Handler(new DiagnosticsProcessSource(), Environment.ProcessId),
            [CountersMethod.Method] = CountersMethod.Handler(),
            [DumpMethod.Method] = DumpMethod.Handler(),
            [HeapMethod.Method] = HeapMethod.Handler(),
            [ThreadsMethod.Method] = ThreadsMethod.Handler(),
        };
        var server = new Server(new LineReader(input), new MessageWriter(output), methods);
        return await server.RunAsync().ConfigureAwait(false);
    }
}
