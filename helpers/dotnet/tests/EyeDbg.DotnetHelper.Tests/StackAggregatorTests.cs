// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using EyeDbg.DotnetHelper.Analysis;
using EyeDbg.DotnetHelper.Rpc;
using EyeDbg.DotnetHelper.Tracing;

namespace EyeDbg.DotnetHelper.Tests;

public class StackAggregatorTests
{
    [Theory]
    [InlineData("Program.<Main>$(class System.String[])", "Program.<Main>$(System.String[])")]
    [InlineData("A.B(int32,value class S,class C)", "A.B(int32,S,C)")]
    [InlineData("A.B(class System.Collections.Generic.List`1<class Foo>)", "A.B(System.Collections.Generic.List`1<Foo>)")]
    [InlineData("A.B(value class System.Nullable`1<value class System.IO.UnixFileMode>)", "A.B(System.Nullable`1<System.IO.UnixFileMode>)")]
    [InlineData("A.Subclass.M(class Myclass)", "A.Subclass.M(Myclass)")]
    [InlineData("A.M(unsigned int8*,int32)", "A.M(unsigned int8*,int32)")]
    [InlineData("class", "class")]
    [InlineData("", "")]
    public void StripsIlKeywords(string name, string want) => Assert.Equal(want, MethodNames.Display(name));

    [Fact]
    public void CutsLongNames()
    {
        var name = "A." + new string('m', 1000) + "(class X)";

        var got = MethodNames.Display(name);

        Assert.Equal(Names.MaxLength, got.Length);
        Assert.EndsWith("…", got, StringComparison.Ordinal);
    }

    [Fact]
    public void CountsTopAndEveryMethodOnce()
    {
        var s = new StackAggregator();

        s.Add(1, managed: true, [F("Leaf"), F("Mid"), F("Main")]);
        s.Add(1, managed: true, [F("Leaf"), F("Rec"), F("Rec"), F("Rec"), F("Main")]); // recursion counts once
        s.Add(2, managed: true, [F("Mid"), F("Main")]);
        s.Add(3, managed: false, [F("Sleep"), F("Main")]); // not managed: waiting only
        s.Add(3, managed: false, []); // no stack: counted, in no list

        var r = s.Result(10, new ResultBudget());

        Assert.Equal(5, r.Samples);
        Assert.Equal(3, r.Managed);
        Assert.Equal(2, r.Other);
        Assert.Equal(3, r.Threads);
        Assert.Equal(0, r.UnresolvedFrames);
        Assert.Equal(["Leaf 2", "Mid 1"], Rows(r.Exclusive));
        Assert.Equal(["Main 3", "Leaf 2", "Mid 2", "Rec 1"], Rows(r.Inclusive));
        Assert.Equal(["Sleep 1"], Rows(r.Waiting));
        Assert.Equal(0, r.ExclusiveOmitted + r.InclusiveOmitted + r.WaitingOmitted);
    }

    [Fact]
    public void TiesSortByMethodThenModule()
    {
        var s = new StackAggregator();
        s.Add(1, true, [new Frame("B", "m2")]);
        s.Add(1, true, [new Frame("B", "m1")]);
        s.Add(1, true, [new Frame("A", null)]);
        s.Add(1, true, [new Frame("C", "m")]);
        s.Add(1, true, [new Frame("C", "m")]);

        var r = s.Result(10, new ResultBudget());

        Assert.Equal(["C [m] 2", "A 1", "B [m1] 1", "B [m2] 1"], Rows(r.Exclusive));
        Assert.Null(r.Exclusive[1].Module);
    }

    [Fact]
    public void UnresolvedFramesKeepTheirModule()
    {
        var s = new StackAggregator();
        s.Add(1, true, [new Frame(null, "libfoo"), new Frame("Main", "app"), new Frame(null, null)]);
        s.Add(1, true, [new Frame(null, "libfoo")]);

        var r = s.Result(10, new ResultBudget());

        Assert.Equal(3, r.UnresolvedFrames);
        Assert.Equal(["(unresolved) [libfoo] 2"], Rows(r.Exclusive));
        Assert.Equal(["(unresolved) [libfoo] 2", "(unresolved) 1", "Main [app] 1"], Rows(r.Inclusive));
    }

    [Theory]
    [InlineData(1, 1, 2)]
    [InlineData(2, 2, 1)]
    [InlineData(3, 3, 0)]
    [InlineData(1000, 3, 0)]
    public void TopBoundsEachList(int top, int wantRows, int wantOmitted)
    {
        var s = new StackAggregator();
        s.Add(1, true, [F("A"), F("B"), F("C")]);
        s.Add(1, true, [F("B"), F("C")]);
        s.Add(1, true, [F("C")]);
        s.Add(1, false, [F("X")]);
        s.Add(1, false, [F("Y")]);
        s.Add(1, false, [F("Z")]);

        var r = s.Result(top, new ResultBudget());

        Assert.Equal(wantRows, r.Exclusive.Count);
        Assert.Equal(wantOmitted, r.ExclusiveOmitted);
        Assert.Equal(wantRows, r.Inclusive.Count);
        Assert.Equal(wantOmitted, r.InclusiveOmitted);
        Assert.Equal(wantRows, r.Waiting.Count);
        Assert.Equal(wantOmitted, r.WaitingOmitted);
    }

    [Fact]
    public void BudgetCutsRowsAndCountsThem()
    {
        var s = new StackAggregator();
        for (var i = 0; i < 20; i++)
        {
            s.Add(1, true, [F("M" + i.ToString("00", System.Globalization.CultureInfo.InvariantCulture))]);
        }

        // Room for about five rows: exclusive fills it; inclusive gets none.
        var r = s.Result(1000, new ResultBudget(5 * 34));

        Assert.InRange(r.Exclusive.Count, 1, 6);
        Assert.Equal(20, r.Exclusive.Count + r.ExclusiveOmitted);
        Assert.Empty(r.Inclusive);
        Assert.Equal(20, r.InclusiveOmitted);
    }

    private static Frame F(string method) => new(method, null);

    private static List<string> Rows(IReadOnlyList<MethodSamples> rows) =>
        [.. rows.Select(r => r.Method + (r.Module is null ? "" : " [" + r.Module + "]") + " " + r.Samples)];
}
