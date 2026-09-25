// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Text.Json;
using System.Text.Json.Serialization;
using System.Text.Json.Serialization.Metadata;

namespace EyeDbg.DotnetHelper.Rpc;

/// <summary>Parameters of hello, the first request.</summary>
internal sealed record HelloParams
{
    [JsonRequired]
    public int Protocol { get; init; }
}

/// <summary>Result of hello.</summary>
internal sealed record HelloResult(int Protocol, string Version, string Runtime);

/// <summary>Result of processes.</summary>
internal sealed record ProcessesResult(IReadOnlyList<ProcessEntry> Processes);

/// <summary>A process with a .NET diagnostics endpoint.</summary>
internal sealed record ProcessEntry(int Pid);

/// <summary>Parameters of counters.</summary>
internal sealed record CountersParams
{
    [JsonRequired]
    public int Pid { get; init; }

    [JsonRequired]
    public int IntervalSec { get; init; }

    /// <summary>How long to collect; 0 until stdin closes.</summary>
    public long DurationMs { get; init; }
}

/// <summary>One counter's value in a sample.</summary>
internal sealed record CounterValue(string Provider, string Name, string DisplayName, string Unit, string Kind, double Value);

/// <summary>The counters.sample notification: the counters of one interval.</summary>
internal sealed record CounterSample(long ElapsedMs, IReadOnlyList<CounterValue> Counters);

/// <summary>Result of counters.</summary>
internal sealed record CountersResult(int Samples, string EndReason);

/// <summary>The data of an error response, as eyedbg's api.Error.</summary>
internal sealed record ErrorData(string Code, string Message, string? Hint);

/// <summary>A method's result and how to encode it.</summary>
internal readonly record struct Reply(object Value, JsonTypeInfo TypeInfo);

[JsonSourceGenerationOptions(
    PropertyNamingPolicy = JsonKnownNamingPolicy.CamelCase,
    DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull,
    NumberHandling = JsonNumberHandling.Strict)]
[JsonSerializable(typeof(HelloParams))]
[JsonSerializable(typeof(HelloResult))]
[JsonSerializable(typeof(ProcessesResult))]
[JsonSerializable(typeof(CountersParams))]
[JsonSerializable(typeof(CounterSample))]
[JsonSerializable(typeof(CountersResult))]
[JsonSerializable(typeof(ErrorData))]
[JsonSerializable(typeof(string))]
internal sealed partial class ProtocolJson : JsonSerializerContext
{
    /// <summary>
    /// Decodes a request's params (absent or null reads as {}); a bad shape is INVALID_REQUEST.
    /// </summary>
    public static T ParseParams<T>(JsonElement parameters, JsonTypeInfo<T> typeInfo)
    {
        try
        {
            var value = parameters.ValueKind is JsonValueKind.Undefined or JsonValueKind.Null
                ? JsonSerializer.Deserialize("{}", typeInfo)
                : JsonSerializer.Deserialize(parameters, typeInfo);
            return value ?? throw new HelperException(ErrorCodes.InvalidRequest, "invalid params: null");
        }
        catch (JsonException e)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "invalid params: " + e.Message);
        }
    }
}
