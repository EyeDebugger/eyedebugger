// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Text.Json;
using System.Text.Json.Serialization;
using System.Text.Json.Serialization.Metadata;
using EyeDbg.DotnetHelper.Analysis;

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

/// <summary>Parameters of dump.</summary>
internal sealed record DumpParams
{
    [JsonRequired]
    public int Pid { get; init; }

    /// <summary>heap, mini, triage or full.</summary>
    [JsonRequired]
    public string Type { get; init; } = "";

    /// <summary>Where the runtime writes it: absolute, and nothing there yet.</summary>
    [JsonRequired]
    public string Path { get; init; } = "";

    [JsonRequired]
    public long TimeoutMs { get; init; }
}

/// <summary>Result of dump.</summary>
internal sealed record DumpResult(long Bytes, long ElapsedMs);

/// <summary>Parameters of heap.</summary>
internal sealed record HeapParams
{
    [JsonRequired]
    public string Path { get; init; } = "";

    /// <summary>The dump is eyedbg's own: its recorded runtime directory may supply the DAC.</summary>
    public bool TrustRecordedRuntime { get; init; }

    [JsonRequired]
    public int Top { get; init; }

    /// <summary>Only types whose full name contains this (case-sensitive).</summary>
    public string? TypeFilter { get; init; }

    public GcrootParams? Gcroot { get; init; }

    /// <summary>With gcroot: how many root paths to find.</summary>
    public int Paths { get; init; } = 1;

    [JsonRequired]
    public long TimeoutMs { get; init; }
}

/// <summary>What --gcroot asks about: a type (exact full name, else a unique substring) or an object's address.</summary>
internal sealed record GcrootParams
{
    public string? Type { get; init; }

    public string? Address { get; init; }
}

/// <summary>Result of heap.</summary>
internal sealed record HeapResult(
    RuntimeInfo Runtime,
    long Objects,
    long Bytes,
    IReadOnlyList<GenerationStat> Generations,
    int TypeCount,
    IReadOnlyList<TypeStat> Types,
    int TypesOmitted,
    GcrootResult? Gcroot);

/// <summary>Objects and bytes in one generation (gen0, gen1, gen2, loh, poh, frozen; unknown when any).</summary>
internal sealed record GenerationStat(string Name, long Objects, long Bytes);

/// <summary>Objects and bytes of one type.</summary>
internal sealed record TypeStat(string Name, long Count, long Bytes);

/// <summary>Why the target is alive: root paths.</summary>
internal sealed record GcrootResult(GcrootTarget Target, IReadOnlyList<RootPath> Paths, int PathsFound, bool Complete);

/// <summary>The resolved target: its type, the object's address (for an address), instances (for a type).</summary>
internal sealed record GcrootTarget(string? Type, string? Address, long? Instances);

/// <summary>A path from a root to the target; chainOmitted links left out of the middle.</summary>
internal sealed record RootPath(RootLabel Root, IReadOnlyList<ChainLinkInfo> Chain, int? ChainOmitted);

/// <summary>
/// A root: kind (static, stack, strong, pinned, async-pinned, ref-counted, sized-ref, finalizer,
/// thread-static), the static field's name (Type.field), the stack root's thread and frame.
/// </summary>
internal sealed record RootLabel(string Kind, string? Name, int? Thread, string? Frame);

/// <summary>One object of a chain.</summary>
internal sealed record ChainLinkInfo(string Address, string Type);

/// <summary>Parameters of threads.</summary>
internal sealed record ThreadsParams
{
    [JsonRequired]
    public string Path { get; init; } = "";

    /// <summary>The dump is eyedbg's own: its recorded runtime directory may supply the DAC.</summary>
    public bool TrustRecordedRuntime { get; init; }

    /// <summary>Frames per thread, from the top.</summary>
    [JsonRequired]
    public int Frames { get; init; }

    [JsonRequired]
    public long TimeoutMs { get; init; }
}

/// <summary>Result of threads.</summary>
internal sealed record ThreadsResult(
    RuntimeInfo Runtime,
    IReadOnlyList<ThreadInfo> Threads,
    int ThreadsOmitted,
    IReadOnlyList<LockInfo> Locks,
    bool LocksAvailable);

/// <summary>A managed thread: ids, flags, the type of its current exception, its top frames.</summary>
internal sealed record ThreadInfo(
    int ManagedId,
    uint OsId,
    bool Alive,
    bool Background,
    bool Threadpool,
    bool Finalizer,
    bool Gc,
    string? Exception,
    IReadOnlyList<FrameInfo> Frames,
    int FramesOmitted);

/// <summary>A frame: managed (method, module, IL offset, source) or runtime (its name as method).</summary>
internal sealed record FrameInfo(string Kind, string Method, string? Module, int? IlOffset, string? File, int? Line);

/// <summary>A lock (a monitor with a sync block) held or waited on: object, its type, owner's managed id, waiters.</summary>
internal sealed record LockInfo(string Object, string Type, int? Owner, int Waiting);

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
[JsonSerializable(typeof(DumpParams))]
[JsonSerializable(typeof(DumpResult))]
[JsonSerializable(typeof(HeapParams))]
[JsonSerializable(typeof(HeapResult))]
[JsonSerializable(typeof(TypeStat))]
[JsonSerializable(typeof(RootPath))]
[JsonSerializable(typeof(ThreadsParams))]
[JsonSerializable(typeof(ThreadsResult))]
[JsonSerializable(typeof(ThreadInfo))]
[JsonSerializable(typeof(LockInfo))]
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
