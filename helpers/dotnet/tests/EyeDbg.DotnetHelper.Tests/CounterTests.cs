// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Globalization;
using EyeDbg.DotnetHelper.Counters;
using EyeDbg.DotnetHelper.Methods;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Tests;

public class CounterTests
{
    private const string P = CounterPayload.RuntimeProvider;

    public static TheoryData<Dictionary<string, object?>, string?> Payloads => new()
    {
        {
            new() { ["Name"] = "cpu-usage", ["DisplayName"] = "CPU Usage", ["CounterType"] = "Mean", ["Mean"] = 1.5, ["DisplayUnits"] = "%" },
            "cpu-usage|CPU Usage|%|gauge|1.5"
        },
        {
            new() { ["Name"] = "exception-count", ["DisplayName"] = "Exception Count", ["CounterType"] = "Sum", ["Increment"] = 3.0, ["DisplayUnits"] = "", ["Mean"] = 99.0 },
            "exception-count|Exception Count||sum|3"
        },
        {
            // No type: Increment means a sum; no DisplayName: the name; no units: "".
            new() { ["Name"] = "alloc-rate", ["Increment"] = 7 },
            "alloc-rate|alloc-rate||sum|7"
        },
        {
            new() { ["Name"] = "working-set", ["Mean"] = 2.5f },
            "working-set|working-set||gauge|2.5"
        },
        { new() { ["Name"] = "x", ["CounterType"] = "Mean", ["Mean"] = double.NaN }, null },
        { new() { ["Name"] = "x", ["CounterType"] = "Mean", ["Mean"] = double.PositiveInfinity }, null },
        { new() { ["Name"] = "x", ["CounterType"] = "Mean", ["Mean"] = "1.0" }, null },
        { new() { ["Name"] = "x", ["CounterType"] = "Mean" }, null },
        { new() { ["Name"] = "x", ["CounterType"] = "Histogram", ["Mean"] = 1.0 }, null },
        { new() { ["Name"] = "", ["CounterType"] = "Mean", ["Mean"] = 1.0 }, null },
        { new() { ["Name"] = 5, ["CounterType"] = "Mean", ["Mean"] = 1.0 }, null },
        { new() { ["CounterType"] = "Mean", ["Mean"] = 1.0 }, null },
    };

    [Theory]
    [MemberData(nameof(Payloads))]
    public void MapsPayloads(Dictionary<string, object?> payload, string? want)
    {
        var v = CounterPayload.From(P, payload);

        Assert.Equal(want, v is null ? null : string.Join('|', v.Name, v.DisplayName, v.Unit, v.Kind, v.Value.ToString(CultureInfo.InvariantCulture)));
        Assert.Equal(P, v?.Provider ?? P);
    }

    [Fact]
    public void MapsNullPayload() => Assert.Null(CounterPayload.From(P, null));

    [Fact]
    public void UnwrapsTheOuterPayloadStruct()
    {
        var inner = new Dictionary<string, object?> { ["Name"] = "n" };

        Assert.Same(inner, CounterPayload.Fields(new Dictionary<string, object?> { ["Payload"] = inner }));
        Assert.Null(CounterPayload.Fields(new Dictionary<string, object?> { ["Other"] = inner }));
        Assert.Null(CounterPayload.Fields(new Dictionary<string, object?> { ["Payload"] = "text" }));
        Assert.Null(CounterPayload.Fields("text"));
        Assert.Null(CounterPayload.Fields(null));
    }

    [Fact]
    public void BatchesOneSamplePerInterval()
    {
        long now = 0;
        var samples = new List<CounterSample>();
        var batcher = new SampleBatcher(() => now, samples.Add);

        now = 1000;
        batcher.Add(Value("a", 1));
        now = 1001;
        batcher.Add(Value("b", 2));
        Assert.Empty(samples);

        now = 2000;
        batcher.Add(Value("a", 3)); // a repeats: the first interval is complete
        batcher.Add(Value("b", 4));
        now = 3000;
        batcher.Close();
        batcher.Add(Value("a", 5)); // dropped after Close
        batcher.Close();

        Assert.Equal(2, batcher.Samples);
        Assert.Equal(2, samples.Count);
        Assert.Equal(1000, samples[0].ElapsedMs);
        Assert.Equal(["a", "b"], samples[0].Counters.Select(c => c.Name));
        Assert.Equal(2000, samples[1].ElapsedMs);
        Assert.Equal([3.0, 4.0], samples[1].Counters.Select(c => c.Value));
    }

    [Fact]
    public void CloseWithoutFlushDropsThePending()
    {
        var samples = new List<CounterSample>();
        var batcher = new SampleBatcher(() => 0, samples.Add);
        batcher.Add(Value("a", 1));

        batcher.Close(flush: false);

        Assert.Empty(samples);
        Assert.Equal(0, batcher.Samples);
    }

    [Fact]
    public void CloseWithNothingPendingSendsNothing()
    {
        var samples = new List<CounterSample>();
        new SampleBatcher(() => 0, samples.Add).Close();

        Assert.Empty(samples);
    }

    [Fact]
    public void EmitFailureClosesTheBatcher()
    {
        var calls = 0;
        var batcher = new SampleBatcher(() => 0, _ =>
        {
            calls++;
            throw new IOException("stdout gone");
        });
        batcher.Add(Value("a", 1));

        batcher.Add(Value("a", 2));
        batcher.Add(Value("a", 3));
        batcher.Close();

        Assert.Equal(1, calls);
        Assert.Equal(0, batcher.Samples);
        Assert.IsType<IOException>(batcher.EmitError);
        Assert.True(batcher.Failed.IsCompleted);
    }

    [Theory]
    [InlineData(1, 1, 0, null)]
    [InlineData(1, 60, CountersMethod.MaxDurationMs, null)]
    [InlineData(0, 1, 0, "pid must be positive")]
    [InlineData(-4, 1, 0, "pid must be positive")]
    [InlineData(1, 0, 0, "intervalSec must be 1 to 60")]
    [InlineData(1, 61, 0, "intervalSec must be 1 to 60")]
    [InlineData(1, 1, -1, "durationMs must be 0 to a day")]
    [InlineData(1, 1, CountersMethod.MaxDurationMs + 1, "durationMs must be 0 to a day")]
    public void ValidatesParams(int pid, int intervalSec, long durationMs, string? wantError)
    {
        var p = new CountersParams { Pid = pid, IntervalSec = intervalSec, DurationMs = durationMs };

        if (wantError is null)
        {
            Assert.Equal(p, CountersMethod.Validate(p));
            return;
        }

        var e = Assert.Throws<HelperException>(() => CountersMethod.Validate(p));
        Assert.Equal(ErrorCodes.InvalidRequest, e.Code);
        Assert.Equal(wantError, e.Message);
    }

    [Fact]
    public void EndTrackingStreamSeesTheEnd()
    {
        var stream = new EndTrackingStream(new MemoryStream([1, 2, 3]));
        var buffer = new byte[2];

        Assert.Equal(2, stream.Read(buffer, 0, 2));
        Assert.False(stream.Ended);
        Assert.Equal(1, stream.Read(buffer));
        Assert.False(stream.Ended);
        Assert.Equal(0, stream.Read(buffer, 0, 0)); // an empty read isn't the end
        Assert.False(stream.Ended);
        Assert.Equal(0, stream.Read(buffer));
        Assert.True(stream.Ended);
    }

    [Fact]
    public void EndTrackingStreamSeesFailures()
    {
        var inner = new MemoryStream([1]);
        var stream = new EndTrackingStream(inner);
        inner.Dispose();

        Assert.Throws<ObjectDisposedException>(() => stream.Read(new byte[1]));
        Assert.True(stream.Ended);
    }

    private static CounterValue Value(string name, double value) => new(P, name, name, "", CounterPayload.Gauge, value);
}
