// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Text;
using EyeDbg.DotnetHelper.Analysis;

namespace EyeDbg.DotnetHelper.Tracing;

/// <summary>How TraceEvent's method names go into a summary.</summary>
internal static class MethodNames
{
    private const string ValueClass = "value class ";
    private const string Class = "class ";

    /// <summary>
    /// TraceEvent's full method name without the IL keywords "class " and "value class " before
    /// parameter types ("Program.Main(class System.String[])" → "Program.Main(System.String[])"),
    /// cut to <see cref="Names.MaxLength"/>. Parameter types otherwise stay as TraceEvent spells
    /// them (int32, unsigned int8*).
    /// </summary>
    public static string Display(string fullMethodName)
    {
        if (!fullMethodName.Contains(Class, StringComparison.Ordinal))
        {
            return Names.Cut(fullMethodName);
        }

        var sb = new StringBuilder(fullMethodName.Length);
        var i = 0;
        while (i < fullMethodName.Length)
        {
            if (AtTokenStart(fullMethodName, i))
            {
                if (string.CompareOrdinal(fullMethodName, i, ValueClass, 0, ValueClass.Length) == 0)
                {
                    i += ValueClass.Length;
                    continue;
                }

                if (string.CompareOrdinal(fullMethodName, i, Class, 0, Class.Length) == 0)
                {
                    i += Class.Length;
                    continue;
                }
            }

            sb.Append(fullMethodName[i]);
            i++;
        }

        return Names.Cut(sb.ToString());
    }

    /// <summary>Whether a type may start at i: the start, or after a separator of a signature.</summary>
    private static bool AtTokenStart(string s, int i) => i == 0 || s[i - 1] is '(' or ',' or '<' or '[' or ' ';
}
