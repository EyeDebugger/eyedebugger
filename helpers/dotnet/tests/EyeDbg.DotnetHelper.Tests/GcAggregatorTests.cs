// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Analysis;
using EyeDbg.DotnetHelper.Tracing;

namespace EyeDbg.DotnetHelper.Tests;

public class GcAggregatorTests
{
    [Fact]
    public void NothingIsNull()
    {
        var g = new GcAggregator();

        Assert.Null(g.Gc());
        Assert.Null(g.Allocations(10, new ResultBudget()));
    }

    [Fact]
    public void CountsCollections()
    {
        var g = new GcAggregator();
        g.AddGc(0, false, "AllocSmall", 1.5);
        g.AddGc(0, false, "AllocSmall", 0.5);
        g.AddGc(1, false, "Induced", 2);
        g.AddGc(2, true, "LowMemory", 4);
        g.AddGc(2, false, "InducedCompacting", 3);
        g.AddGc(2, false, "InducedNotForced", double.NaN); // no pause JSON can't carry
        g.AddGc(7, false, "InducedLowMemory", -1); // out of range: counted in total only
        g.AddGc(0, false, "Internal", double.PositiveInfinity);

        var r = g.Gc()!;

        Assert.Equal(8, r.Collections);
        Assert.Equal(3, r.Gen0);
        Assert.Equal(1, r.Gen1);
        Assert.Equal(3, r.Gen2);
        Assert.Equal(1, r.Background);
        Assert.Equal(4, r.Induced);
        Assert.Equal(11, r.PauseTotalMs);
        Assert.Equal(4, r.PauseMaxMs);
    }

    [Fact]
    public void CountsAllocationsByType()
    {
        var g = new GcAggregator();
        g.AddTick("System.Byte[]", 100_000);
        g.AddTick("System.Byte[]", 100_000);
        g.AddTick("System.String", 150_000);
        g.AddTick("B", 50_000);
        g.AddTick("A", 50_000);
        g.AddTick(null, 10);
        g.AddTick("", -5);

        var r = g.Allocations(10, new ResultBudget())!;

        Assert.Equal(7, r.Ticks);
        Assert.Equal(450_010, r.Bytes);
        Assert.Equal(5, r.TypeCount);
        Assert.Equal(
            ["System.Byte[] 2 200000", "System.String 1 150000", "A 1 50000", "B 1 50000", "(unknown type) 2 10"],
            r.Types.Select(t => t.Name + " " + t.Ticks + " " + t.Bytes));
        Assert.Equal(0, r.TypesOmitted);
    }

    [Fact]
    public void TopAndBudgetBoundTypes()
    {
        var g = new GcAggregator();
        for (var i = 0; i < 5; i++)
        {
            g.AddTick("T" + i, 1000 - i);
        }

        var top = g.Allocations(2, new ResultBudget())!;
        Assert.Equal(["T0", "T1"], top.Types.Select(t => t.Name));
        Assert.Equal(3, top.TypesOmitted);

        var none = g.Allocations(10, new ResultBudget(10))!;
        Assert.Empty(none.Types);
        Assert.Equal(5, none.TypesOmitted);
        Assert.Equal(5, none.TypeCount);
    }

    [Fact]
    public void LongTypeNamesAreCut()
    {
        var g = new GcAggregator();
        g.AddTick(new string('T', 1000), 1);

        Assert.Equal(Names.MaxLength, g.Allocations(1, new ResultBudget())!.Types[0].Name.Length);
    }

    [Fact]
    public void BytesSaturate()
    {
        var g = new GcAggregator();
        g.AddTick("T", long.MaxValue);
        g.AddTick("T", long.MaxValue);

        var r = g.Allocations(1, new ResultBudget())!;
        Assert.Equal(long.MaxValue, r.Bytes);
        Assert.Equal(long.MaxValue, r.Types[0].Bytes);
    }
}
