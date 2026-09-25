// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Text.Json;
using System.Text.Json.Serialization.Metadata;

namespace EyeDbg.DotnetHelper.Analysis;

/// <summary>
/// Keeps a result under the protocol's line limit: items are added while their serialized
/// size fits; the rest are counted, and the result says how many were left out.
/// </summary>
internal sealed class ResultBudget
{
    /// <summary>A result's list items together (the writer refuses a line of 1 MiB).</summary>
    public const long DefaultBytes = 900 * 1024;

    private long _left;

    public ResultBudget(long bytes = DefaultBytes)
    {
        _left = bytes;
    }

    /// <summary>Items that didn't fit.</summary>
    public int Omitted { get; private set; }

    /// <summary>Takes item's size (plus a separator) from the budget if it fits; false otherwise.</summary>
    public bool TryAdd<T>(T item, JsonTypeInfo<T> typeInfo)
    {
        long size = JsonSerializer.SerializeToUtf8Bytes(item, typeInfo).Length + 1;
        if (size > _left)
        {
            Omitted++;
            return false;
        }

        _left -= size;
        return true;
    }

    /// <summary>Counts an item left out without trying it (the list is cut by count).</summary>
    public void Omit(int n = 1) => Omitted += n;
}
