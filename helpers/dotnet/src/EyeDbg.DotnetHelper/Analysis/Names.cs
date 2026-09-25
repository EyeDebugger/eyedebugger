// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Globalization;

namespace EyeDbg.DotnetHelper.Analysis;

/// <summary>How names and addresses read from a dump go into results.</summary>
internal static class Names
{
    /// <summary>The longest name, method or type string in a result.</summary>
    public const int MaxLength = 400;

    /// <summary>s cut to <see cref="MaxLength"/> characters, the cut marked with "…".</summary>
    public static string Cut(string s) => s.Length <= MaxLength ? s : string.Concat(s.AsSpan(0, MaxLength - 1), "…");

    /// <summary>An address as a hex string ("0x1a2b"): a uint64 exceeds JSON numbers' precision in JavaScript.</summary>
    public static string Hex(ulong address) => "0x" + address.ToString("x", CultureInfo.InvariantCulture);

    /// <summary>Parses <see cref="Hex"/>'s form (0x and 1 to 16 hex digits); null otherwise.</summary>
    public static ulong? ParseHex(string s) =>
        s.Length is > 2 and <= 18 && s.StartsWith("0x", StringComparison.OrdinalIgnoreCase)
            && ulong.TryParse(s.AsSpan(2), NumberStyles.AllowHexSpecifier, CultureInfo.InvariantCulture, out var v)
            ? v
            : null;
}
