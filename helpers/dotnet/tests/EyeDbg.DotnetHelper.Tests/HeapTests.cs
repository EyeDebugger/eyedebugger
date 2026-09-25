// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Analysis;
using EyeDbg.DotnetHelper.Methods;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.Runtime;

namespace EyeDbg.DotnetHelper.Tests;

public class HeapTests
{
    private sealed class FakeHeap(IEnumerable<HeapObject> objects, Dictionary<ulong, string?> names) : IHeapSource
    {
        public IEnumerable<HeapObject> Objects() => objects;

        public string? TypeName(ulong methodTable) => names.GetValueOrDefault(methodTable);
    }

    private static HeapStats Sample()
    {
        var objects = new List<HeapObject>();
        objects.AddRange(Enumerable.Repeat(new HeapObject(1, 48, Gen.Gen0), 5000)); // App.Retained
        objects.AddRange(Enumerable.Repeat(new HeapObject(2, 100, Gen.Gen2), 30)); // System.String
        objects.Add(new HeapObject(3, 90_000, Gen.Loh)); // System.Byte[]
        objects.Add(new HeapObject(4, 3000, Gen.Poh)); // System.Object[]
        objects.Add(new HeapObject(5, 3000, Gen.Frozen)); // System.String again (another method table)
        objects.Add(new HeapObject(6, 24, Gen.Gen1)); // unreadable name
        objects.Add(new HeapObject(7, 3000, Gen.Gen2)); // a name that ties System.Object[] by bytes
        var names = new Dictionary<ulong, string?>
        {
            [1] = "App.Retained",
            [2] = "System.String",
            [3] = "System.Byte[]",
            [4] = "System.Object[]",
            [5] = "System.String",
            [6] = null,
            [7] = "App.Cache",
        };
        return HeapStats.Walk(new FakeHeap(objects, names), TestContext.Current.CancellationToken);
    }

    [Fact]
    public void TotalsByGenerationAddUp()
    {
        var stats = Sample();

        Assert.Equal(5035, stats.Objects);
        Assert.Equal((5000 * 48) + (30 * 100) + 90_000 + 3000 + 3000 + 24 + 3000, stats.Bytes);
        Assert.Equal(["gen0", "gen1", "gen2", "loh", "poh", "frozen"], stats.Generations().Select(g => g.Name));
        Assert.Equal(stats.Bytes, stats.Generations().Sum(g => g.Bytes));
        Assert.Equal(stats.Objects, stats.Generations().Sum(g => g.Objects));
        Assert.Equal(new GenerationStat("gen2", 31, 6000), stats.Generations()[2]);
    }

    [Fact]
    public void UnknownGenerationIsListedWhenPresent()
    {
        var stats = HeapStats.Walk(new FakeHeap([new(1, 10, Gen.Unknown)], new() { [1] = "T" }), TestContext.Current.CancellationToken);

        Assert.Equal(new GenerationStat("unknown", 1, 10), stats.Generations()[^1]);
    }

    public static TheoryData<int, string?, int, string[]> TopCases => new()
    {
        { 20, null, 6, ["App.Retained", "System.Byte[]", "System.String", "App.Cache", "System.Object[]", "<unknown type 0x6>"] },
        { 2, null, 6, ["App.Retained", "System.Byte[]"] },
        { 20, "System.", 3, ["System.Byte[]", "System.String", "System.Object[]"] },
        { 20, "system.", 0, [] }, // case-sensitive
        { 20, "[]", 2, ["System.Byte[]", "System.Object[]"] },
    };

    [Theory]
    [MemberData(nameof(TopCases))]
    public void TopTypesBySizeThenName(int top, string? filter, int wantCount, string[] want)
    {
        var (count, types) = Sample().Top(top, filter, new ResultBudget());

        Assert.Equal(wantCount, count);
        Assert.Equal(want, types.Select(t => t.Name));
    }

    [Fact]
    public void SharedNamesAddUp()
    {
        var (_, types) = Sample().Top(20, "System.String", new ResultBudget());

        Assert.Equal(new TypeStat("System.String", 31, 6000), types.Single());
    }

    [Fact]
    public void BudgetCutsTheList()
    {
        var budget = new ResultBudget(120);
        var (count, types) = Sample().Top(20, null, budget);

        Assert.Equal(6, count);
        Assert.Equal(2, types.Count); // {"name":"App.Retained","count":5000,"bytes":240000} is 53 bytes + 1
        Assert.Equal(1, budget.Omitted);
    }

    [Fact]
    public void WalkHonorsCancellation()
    {
        using var canceled = new CancellationTokenSource();
        canceled.Cancel();

        var many = Enumerable.Repeat(new HeapObject(1, 8, Gen.Gen0), HeapStats.CheckEvery);
        Assert.Throws<OperationCanceledException>(() => HeapStats.Walk(new FakeHeap(many, []), canceled.Token));
    }

    [Fact]
    public void LongNamesAreCut()
    {
        var stats = HeapStats.Walk(new FakeHeap([new(1, 8, Gen.Gen0)], new() { [1] = new string('x', 1000) }), TestContext.Current.CancellationToken);
        var name = stats.ByName.Keys.Single();

        Assert.Equal(Names.MaxLength, name.Length);
        Assert.EndsWith("…", name, StringComparison.Ordinal);
    }

    public static TheoryData<string, string?, string?> ResolveCases => new()
    {
        { "App.Retained", "App.Retained", null },
        { "Retained", "App.Retained", null },
        { "System.String", "System.String", null },
        { "System.", null, "'System.' matches 3 types: System.Byte[], System.Object[], System.String" },
        { "Nope", null, "no objects of a type named or containing 'Nope' in this dump" },
        { "", null, "--gcroot needs a type name or an address" },
    };

    [Theory]
    [MemberData(nameof(ResolveCases))]
    public void ResolvesTheGcrootType(string query, string? want, string? wantError)
    {
        var stats = Sample();
        if (wantError is not null)
        {
            var e = Assert.Throws<HelperException>(() => RootPaths.ResolveType(stats.ByName, query));
            Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
            Assert.Equal(wantError, e.Message);
            return;
        }

        var (name, total) = RootPaths.ResolveType(stats.ByName, query);
        Assert.Equal(want, name);
        Assert.Equal(stats.ByName[want!].Count, total.Count);
    }

    [Fact]
    public void AmbiguousListsAtMostTen()
    {
        var names = Enumerable.Range(0, 15).ToDictionary(i => (ulong)i, i => (string?)("T" + i.ToString("00", System.Globalization.CultureInfo.InvariantCulture)));
        var stats = HeapStats.Walk(new FakeHeap(names.Keys.Select(k => new HeapObject(k, 8, Gen.Gen0)), names), TestContext.Current.CancellationToken);

        var e = Assert.Throws<HelperException>(() => RootPaths.ResolveType(stats.ByName, "T"));

        Assert.Equal("'T' matches 15 types: T00, T01, T02, T03, T04, T05, T06, T07, T08, T09, …", e.Message);
    }

    [Theory]
    [InlineData(1, 1, null)]
    [InlineData(32, 32, null)]
    [InlineData(33, 31, 2)]
    [InlineData(100, 31, 69)]
    public void LongChainsKeepHeadAndTail(int length, int kept, int? omitted)
    {
        var chain = Enumerable.Range(0, length).ToList();

        var (got, cut) = RootPaths.Cut(chain);

        Assert.Equal(kept, got.Count);
        Assert.Equal(omitted, cut);
        Assert.Equal(0, got[0]);
        Assert.Equal(length - 1, got[^1]);
        if (omitted is not null)
        {
            Assert.Equal(RootPaths.KeepHead - 1, got[RootPaths.KeepHead - 1]);
            Assert.Equal(length - RootPaths.KeepTail, got[RootPaths.KeepHead]);
        }
    }

    public static TheoryData<ClrRootKind, string?, string?, bool, string, string?, bool> Labels => new()
    {
        { ClrRootKind.StaticVar, "Holder.Keep", null, false, "static", "Holder.Keep", false },
        { ClrRootKind.StaticVar, null, "Holder.Keep", true, "static", "Holder.Keep", true },
        { ClrRootKind.StaticVar, null, null, false, "static", null, false },
        { ClrRootKind.StrongHandle, null, "Holder.Keep", true, "static", "Holder.Keep", true }, // the statics array
        { ClrRootKind.PinnedHandle, null, "Holder.Keep", true, "static", "Holder.Keep", true },
        { ClrRootKind.StrongHandle, null, "Holder.Keep", false, "strong", null, false }, // not an array: a real handle
        { ClrRootKind.StrongHandle, "Holder.Keep", null, false, "strong", null, false },
        { ClrRootKind.PinnedHandle, null, null, true, "pinned", null, false },
        { ClrRootKind.AsyncPinnedHandle, null, null, false, "async-pinned", null, false },
        { ClrRootKind.RefCountedHandle, null, null, false, "ref-counted", null, false },
        { ClrRootKind.SizedRefHandle, null, null, false, "sized-ref", null, false },
        { ClrRootKind.FinalizerQueue, null, null, false, "finalizer", null, false },
        { ClrRootKind.ThreadStaticVar, "X.y", null, false, "thread-static", null, false },
        { ClrRootKind.None, null, null, false, "other", null, false },
    };

    [Theory]
    [MemberData(nameof(Labels))]
    public void LabelsRoots(ClrRootKind kind, string? static0, string? static1, bool firstIsArray, string wantKind, string? wantName, bool wantDrop)
    {
        var (label, drop) = RootPaths.Label(kind, static0, static1, firstIsArray);

        Assert.Equal(wantKind, label.Kind);
        Assert.Equal(wantName, label.Name);
        Assert.Equal(wantDrop, drop);
    }

    [Theory]
    [InlineData(0, 1, null, null, "path must be absolute")]
    [InlineData(1, 0, null, null, "top must be 1 to 1000")]
    [InlineData(1, 1001, null, null, "top must be 1 to 1000")]
    [InlineData(1, 20, 0, null, "paths must be 1 to 20")]
    [InlineData(1, 20, 21, null, "paths must be 1 to 20")]
    [InlineData(1, 20, 3, "both", "gcroot needs exactly one of type and address")]
    [InlineData(1, 20, 3, "neither", "gcroot needs exactly one of type and address")]
    [InlineData(1, 20, 3, "type", null)]
    [InlineData(1, 20, 1, null, null)]
    [InlineData(2, 20, 1, null, "no dump file at ")]
    public void ValidatesParams(int pathKind, int top, int? paths, string? gcroot, string? wantError)
    {
        var file = Path.GetTempFileName();
        try
        {
            var p = new HeapParams
            {
                Path = pathKind switch { 0 => "x.dmp", 1 => file, _ => file + ".missing" },
                Top = top,
                Paths = paths ?? 1,
                TimeoutMs = 60_000,
                Gcroot = gcroot switch
                {
                    "both" => new GcrootParams { Type = "T", Address = "0x1" },
                    "neither" => new GcrootParams(),
                    "type" => new GcrootParams { Type = "T" },
                    _ => null,
                },
            };

            if (wantError is null)
            {
                Assert.Equal(p, HeapMethod.Validate(p));
                return;
            }

            var e = Assert.Throws<HelperException>(() => HeapMethod.Validate(p));
            Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
            Assert.StartsWith(wantError, e.Message, StringComparison.Ordinal);
        }
        finally
        {
            File.Delete(file);
        }
    }

    [Theory]
    [InlineData("0x1a2b", 0x1a2bUL)]
    [InlineData("0X1A2B", 0x1a2bUL)]
    [InlineData("0xffffffffffffffff", ulong.MaxValue)]
    [InlineData("0x", null)]
    [InlineData("1a2b", null)]
    [InlineData("0x1ffffffffffffffff", null)]
    [InlineData("0x-1", null)]
    [InlineData("0xg", null)]
    public void ParsesHexAddresses(string text, ulong? want) => Assert.Equal(want, Names.ParseHex(text));

    [Fact]
    public void FormatsHexAddresses() => Assert.Equal("0x11ee15750", Names.Hex(0x11ee15750));
}
