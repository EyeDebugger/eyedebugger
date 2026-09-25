// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Collections.Immutable;
using EyeDbg.DotnetHelper.Analysis;
using EyeDbg.DotnetHelper.Methods;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.Runtime;

namespace EyeDbg.DotnetHelper.Tests;

public class ThreadsTests
{
    private static readonly RuntimeInfo _runtime = new("10.0.12", "workstation", 1, "linux", "x64");

    private static readonly FrameInfo _managed = new("managed", "App.Work()", "app.dll", 3, "/src/App.cs", 12);
    private static readonly FrameInfo _native = new("runtime", "InlinedCallFrame", null, null, null, null);

    private static (ThreadInfo, ulong) Thread(int id, ulong address, params FrameInfo[] frames) =>
        (new ThreadInfo(id, (uint)(1000 + id), true, false, false, false, false, null, frames, 0), address);

    [Fact]
    public void OrdersLockOwnersThenManagedThenTheRest()
    {
        var threads = new[]
        {
            Thread(1, 0x100, _native, _managed),
            Thread(2, 0x200, _native),
            Thread(3, 0x300, _managed),
            Thread(7, 0x700, _managed),
            Thread(9, 0x900),
        };
        var locks = new[] { new LockData(0x5000, "System.Object", 0x700, true, 1) };

        var r = ThreadStacks.Build(_runtime, threads, locks, true, new ResultBudget());

        Assert.Equal([7, 1, 3, 2, 9], r.Threads.Select(t => t.ManagedId));
        Assert.Equal(0, r.ThreadsOmitted);
        Assert.Equal([new LockInfo("0x5000", "System.Object", 7, 1)], r.Locks);
        Assert.True(r.LocksAvailable);
    }

    [Fact]
    public void LockWithoutAKnownOwner()
    {
        var locks = new[]
        {
            new LockData(0x6000, "App.Gate", 0xdead, true, 2), // the holder isn't a managed thread we know
            new LockData(0x5000, "System.Object", 0, false, 1), // waiters, nobody holds it
        };

        var r = ThreadStacks.Build(_runtime, [Thread(1, 0x100, _managed)], locks, true, new ResultBudget());

        Assert.Equal([new LockInfo("0x5000", "System.Object", null, 1), new LockInfo("0x6000", "App.Gate", null, 2)], r.Locks);
    }

    [Fact]
    public void NoLocksWithoutAHeap()
    {
        var r = ThreadStacks.Build(_runtime, [Thread(1, 0x100, _managed)], [], false, new ResultBudget());

        Assert.False(r.LocksAvailable);
        Assert.Empty(r.Locks);
        Assert.Single(r.Threads);
    }

    [Fact]
    public void BudgetKeepsTheFirstThreads()
    {
        var threads = Enumerable.Range(1, 50).Select(i => Thread(i, (ulong)i * 0x100, _managed)).ToArray();

        var r = ThreadStacks.Build(_runtime, threads, [], true, new ResultBudget(2000));

        Assert.InRange(r.Threads.Count, 1, 49);
        Assert.Equal(50, r.Threads.Count + r.ThreadsOmitted);
        Assert.Equal(Enumerable.Range(1, r.Threads.Count), r.Threads.Select(t => t.ManagedId));
    }

    [Theory]
    [InlineData(10, 3, 3, 0)]
    [InlineData(2, 3, 2, 1)]
    [InlineData(1, 0, 0, 0)]
    public void TakesTheTopFrames(int n, int available, int kept, int omitted)
    {
        var (got, cut) = ThreadStacks.Take(Enumerable.Range(0, available), n, (x, i) => (x, i));

        Assert.Equal(kept, got.Count);
        Assert.Equal(omitted, cut);
        Assert.All(got, g => Assert.Equal(g.x, g.i)); // the index says which is the leaf
    }

    [Theory]
    [InlineData(ClrThreadState.TS_Background, true, false)]
    [InlineData(ClrThreadState.TS_Background | ClrThreadState.TS_TPWorkerThread, true, true)]
    [InlineData(ClrThreadState.TS_CompletionPortThread, false, true)]
    [InlineData(ClrThreadState.TS_InMTA, false, false)]
    public void MapsThreadFlags(ClrThreadState state, bool background, bool threadpool) =>
        Assert.Equal((background, threadpool), ThreadStacks.Flags(state));

    public static TheoryData<ulong, int?> IpCases => new()
    {
        { 0x1000, 0 },
        { 0x100f, 0 },
        { 0x1010, 6 },
        { 0x1020, null }, // an epilog entry (-3)
        { 0x2000, null }, // outside the map: no guess
    };

    [Theory]
    [MemberData(nameof(IpCases))]
    public void MapsInstructionPointersToILOffsets(ulong ip, int? want)
    {
        ImmutableArray<ILToNativeMap> map =
        [
            new() { ILOffset = 0, StartAddress = 0x1000, EndAddress = 0x100f },
            new() { ILOffset = 6, StartAddress = 0x1010, EndAddress = 0x101f },
            new() { ILOffset = -3, StartAddress = 0x1020, EndAddress = 0x102f },
        ];

        Assert.Equal(want, ThreadStacks.ILOffset(map, ip));
    }

    [Fact]
    public void NoMapNoILOffset()
    {
        Assert.Null(ThreadStacks.ILOffset(default, 0x1000));
        Assert.Null(ThreadStacks.ILOffset([], 0x1000));
    }

    [Theory]
    [InlineData(0, "frames must be 1 to 64")]
    [InlineData(65, "frames must be 1 to 64")]
    [InlineData(64, null)]
    [InlineData(1, null)]
    public void ValidatesParams(int frames, string? wantError)
    {
        var file = Path.GetTempFileName();
        try
        {
            var p = new ThreadsParams { Path = file, Frames = frames, TimeoutMs = 1000 };
            if (wantError is null)
            {
                Assert.Equal(p, ThreadsMethod.Validate(p));
                return;
            }

            var e = Assert.Throws<HelperException>(() => ThreadsMethod.Validate(p));
            Assert.Equal(wantError, e.Message);
        }
        finally
        {
            File.Delete(file);
        }
    }
}
