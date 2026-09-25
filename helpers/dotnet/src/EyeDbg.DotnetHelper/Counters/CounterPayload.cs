// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Globalization;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Counters;

/// <summary>Maps EventCounters payloads to the wire's counter values.</summary>
internal static class CounterPayload
{
    /// <summary>The provider every M6 counter comes from.</summary>
    public const string RuntimeProvider = "System.Runtime";

    /// <summary>The event EventSource counters arrive as.</summary>
    public const string EventName = "EventCounters";

    public const string Gauge = "gauge";
    public const string Sum = "sum";

    /// <summary>
    /// The counter in an EventCounters "Payload" dictionary: CounterType Mean is a gauge (value
    /// Mean), Sum a sum (value Increment). Null when it has no name, an unknown type, or a
    /// value that isn't a finite number (JSON can't carry NaN).
    /// </summary>
    public static CounterValue? From(string provider, IDictionary<string, object?>? payload)
    {
        if (payload is null || Text(payload, "Name") is not { Length: > 0 } name)
        {
            return null;
        }

        var type = Text(payload, "CounterType") ?? (payload.ContainsKey("Increment") ? "Sum" : "Mean");
        var (kind, field) = type switch
        {
            "Mean" => (Gauge, "Mean"),
            "Sum" => (Sum, "Increment"),
            _ => (null, null),
        };
        if (kind is null || Number(payload, field!) is not { } value)
        {
            return null;
        }

        var displayName = Text(payload, "DisplayName") is { Length: > 0 } d ? d : name;
        return new CounterValue(provider, name, displayName, Text(payload, "DisplayUnits") ?? "", kind, value);
    }

    /// <summary>
    /// The counter's fields from an EventCounters event's first payload value: TraceEvent
    /// decodes it as a struct with one field, "Payload", holding the counter's fields.
    /// </summary>
    public static IDictionary<string, object?>? Fields(object? payloadValue) =>
        payloadValue is IDictionary<string, object?> outer && outer.TryGetValue("Payload", out var inner)
            ? inner as IDictionary<string, object?>
            : null;

    private static string? Text(IDictionary<string, object?> payload, string key) =>
        payload.TryGetValue(key, out var v) ? v as string : null;

    private static double? Number(IDictionary<string, object?> payload, string key)
    {
        if (!payload.TryGetValue(key, out var v))
        {
            return null;
        }

        double? value = v switch
        {
            double d => d,
            float f => f,
            int or long or short or byte or uint or ulong or ushort or sbyte or decimal => Convert.ToDouble(v, CultureInfo.InvariantCulture),
            _ => null,
        };
        return value is { } x && double.IsFinite(x) ? x : null;
    }
}
