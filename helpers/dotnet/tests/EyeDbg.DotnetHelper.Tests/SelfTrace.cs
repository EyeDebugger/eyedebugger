// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Runtime.CompilerServices;
using EyeDbg.DotnetHelper.Rpc;
using EyeDbg.DotnetHelper.Tracing;

namespace EyeDbg.DotnetHelper.Tests;

/// <summary>
/// Tests that trace this test process itself run one at a time: parallel tests would compete
/// with the load a trace measures.
/// </summary>
[CollectionDefinition(nameof(SelfTracing), DisableParallelization = true)]
public sealed class SelfTracing
{
}

/// <summary>A thread keeping the process busy while it traces itself: spinning, or allocating.</summary>
internal sealed class Load : IDisposable
{
    private readonly Thread _thread;
    private volatile bool _stop;

    private Load(bool allocate)
    {
        using var started = new ManualResetEventSlim();
        _thread = new Thread(() =>
        {
            started.Set();
            if (allocate)
            {
                Allocate();
            }
            else
            {
                Spin();
            }
        })
        {
            IsBackground = true,
            Name = "eyedbg-test-load",
        };
        _thread.Start();
        started.Wait();
    }

    /// <summary>The method a CPU trace of <see cref="Spinning"/> finds hottest.</summary>
    public const string HotMethod = "EyeDbg.DotnetHelper.Tests.Load.Burn(int32)";

    public static Load Spinning() => new(allocate: false);

    public static Load Allocating() => new(allocate: true);

    public void Dispose()
    {
        _stop = true;
        _thread.Join();
    }

    /// <summary>Traces this process for a second into a fresh file in dir.</summary>
    public static async Task<(string Path, TraceResult Result)> TraceSelfAsync(string dir, string profile, CancellationToken cancellationToken)
    {
        var path = System.IO.Path.Combine(dir, profile + "-" + Guid.NewGuid().ToString("N") + ".nettrace");
        var p = new TraceParams { Pid = Environment.ProcessId, Profile = profile, DurationMs = 1000, Path = path };
        return (path, await TraceCollector.RunAsync(p, cancellationToken));
    }

    [MethodImpl(MethodImplOptions.NoInlining)]
    internal static long Burn(int n)
    {
        long s = 0;
        for (var i = 0; i < n; i++)
        {
            s = unchecked((s * 31) + (i ^ (s >> 7)));
        }

        return s;
    }

    private void Spin()
    {
        long total = 0;
        while (!_stop)
        {
            total += Burn(100_000);
        }

        GC.KeepAlive(total);
    }

    private void Allocate()
    {
        var ring = new byte[256][];
        var i = 0;
        while (!_stop)
        {
            ring[i++ & 255] = new byte[512];
        }

        GC.KeepAlive(ring);
    }
}
