// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Text.Json;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.Runtime;

namespace EyeDbg.DotnetHelper.Analysis;

/// <summary>
/// Why objects are alive (--gcroot): paths from GC roots to a target object or type, with a
/// static root named by its field.
/// </summary>
internal static class RootPaths
{
    /// <summary>A longer chain keeps its first <see cref="KeepHead"/> and last <see cref="KeepTail"/> links.</summary>
    public const int MaxChain = 32;

    public const int KeepHead = 8;
    public const int KeepTail = 23;

    /// <summary>At most this many type names in an ambiguous --gcroot error.</summary>
    public const int MaxListedMatches = 10;

    /// <summary>Finds up to paths root paths to the target; a timeout ends the search, not the result.</summary>
    public static GcrootResult Find(LoadedDump dump, HeapStats stats, GcrootParams query, int paths, ResultBudget budget, CancellationToken timeout, CancellationToken caller)
    {
        var heap = dump.Runtime.Heap;
        GCRoot gcroot;
        GcrootTarget target;
        if (query.Address is { } text)
        {
            var address = Names.ParseHex(text) ?? throw new HelperException(ErrorCodes.InvalidRequest, "--gcroot address must be hex like 0x1a2b");
            var obj = heap.GetObject(address);
            if (!obj.IsValid || obj.IsFree)
            {
                throw new HelperException(ErrorCodes.InvalidRequest, "no object at " + Names.Hex(address) + " in this dump", "take an address from a heap or threads output of the same dump");
            }

            target = new GcrootTarget(Names.Cut(obj.Type!.Name ?? "?"), Names.Hex(address), null);
            gcroot = new GCRoot(heap, [address]);
        }
        else
        {
            var (name, total) = ResolveType(stats.ByName, query.Type ?? "");
            var methodTables = total.MethodTables.ToHashSet();
            target = new GcrootTarget(name, null, total.Count);
            gcroot = new GCRoot(heap, o => o.Type is { } t && methodTables.Contains(t.MethodTable));
        }

        var statics = new Lazy<Dictionary<ulong, string>>(() => StaticFields(dump.Runtime));
        var found = new List<RootPath>();
        var seen = new HashSet<string>(StringComparer.Ordinal);
        int count = 0;
        bool complete = true;
        try
        {
            foreach (var (root, chain) in gcroot.EnumerateRootPaths(timeout))
            {
                if (count == paths)
                {
                    complete = false;
                    break;
                }

                // Two roots can read the same (a static's slot and the handle to its array).
                var path = Describe(heap, root, chain, statics);
                if (!seen.Add(JsonSerializer.Serialize(path, ProtocolJson.Default.RootPath)))
                {
                    continue;
                }

                count++;
                if (!budget.TryAdd(path, ProtocolJson.Default.RootPath))
                {
                    complete = false;
                    break;
                }

                found.Add(path);
            }
        }
        catch (OperationCanceledException) when (!caller.IsCancellationRequested)
        {
            complete = false;
        }

        return new GcrootResult(target, found, count, complete);
    }

    /// <summary>
    /// The type --gcroot names: an exact full name, else the one name containing the query;
    /// INVALID_REQUEST when none or several match.
    /// </summary>
    internal static (string Name, TypeTotal Total) ResolveType(IReadOnlyDictionary<string, TypeTotal> byName, string query)
    {
        if (query.Length == 0)
        {
            throw new HelperException(ErrorCodes.InvalidRequest, "--gcroot needs a type name or an address");
        }

        if (byName.TryGetValue(query, out var exact))
        {
            return (query, exact);
        }

        var matches = byName.Keys.Where(n => n.Contains(query, StringComparison.Ordinal)).Order(StringComparer.Ordinal).ToList();
        return matches.Count switch
        {
            1 => (matches[0], byName[matches[0]]),
            0 => throw new HelperException(
                ErrorCodes.InvalidRequest,
                "no objects of a type named or containing " + Quote(query) + " in this dump",
                "type names are case-sensitive and full (Namespace.Type); the heap's top types show what is there"),
            _ => throw new HelperException(
                ErrorCodes.InvalidRequest,
                Quote(query) + " matches " + matches.Count + " types: " + string.Join(", ", matches.Take(MaxListedMatches)) + (matches.Count > MaxListedMatches ? ", …" : ""),
                "give the full type name"),
        };
    }

    /// <summary>A chain of more than <see cref="MaxChain"/> links cut to its head and tail.</summary>
    internal static (List<T> Kept, int? Omitted) Cut<T>(IReadOnlyList<T> chain)
    {
        if (chain.Count <= MaxChain)
        {
            return ([.. chain], null);
        }

        var kept = new List<T>(KeepHead + KeepTail);
        kept.AddRange(chain.Take(KeepHead));
        kept.AddRange(chain.Skip(chain.Count - KeepTail));
        return (kept, chain.Count - KeepHead - KeepTail);
    }

    /// <summary>What a root is, from ClrMD's root kind.</summary>
    internal static string KindName(ClrRootKind kind) => kind switch
    {
        ClrRootKind.StaticVar => "static",
        ClrRootKind.ThreadStaticVar => "thread-static",
        ClrRootKind.Stack => "stack",
        ClrRootKind.StrongHandle => "strong",
        ClrRootKind.PinnedHandle => "pinned",
        ClrRootKind.AsyncPinnedHandle => "async-pinned",
        ClrRootKind.RefCountedHandle => "ref-counted",
        ClrRootKind.SizedRefHandle => "sized-ref",
        ClrRootKind.FinalizerQueue => "finalizer",
        _ => "other",
    };

    /// <summary>
    /// The label of a non-stack root. A static variable root is named by the static field that
    /// holds its object, or the object after it (the runtime's statics array); a strong or pinned
    /// handle to that array (how .NET keeps statics) is a static root too, with the array
    /// dropped from the chain. staticOf0/1: the field holding the chain's first/second object.
    /// </summary>
    internal static (RootLabel Label, bool DropFirst) Label(ClrRootKind kind, string? staticOf0, string? staticOf1, bool firstIsObjectArray)
    {
        if (kind == ClrRootKind.StaticVar)
        {
            return staticOf0 is not null
                ? (new RootLabel("static", staticOf0, null, null), false)
                : (new RootLabel("static", staticOf1, null, null), staticOf1 is not null);
        }

        if (kind is ClrRootKind.StrongHandle or ClrRootKind.PinnedHandle && firstIsObjectArray && staticOf1 is not null)
        {
            return (new RootLabel("static", staticOf1, null, null), true);
        }

        return (new RootLabel(KindName(kind), null, null, null), false);
    }

    private static RootPath Describe(ClrHeap heap, ClrRoot root, GCRoot.ChainLink chain, Lazy<Dictionary<ulong, string>> statics)
    {
        var addresses = new List<ulong>();
        for (var link = chain; link is not null; link = link.Next)
        {
            addresses.Add(link.Object);
        }

        RootLabel label;
        bool dropFirst = false;
        if (root is ClrStackRoot stack)
        {
            var frame = stack.StackFrame;
            label = new RootLabel(
                "stack",
                null,
                frame?.Thread?.ManagedThreadId,
                frame?.Method?.Signature is { } signature ? Names.Cut(signature) : frame?.FrameName is { } name ? Names.Cut("[" + name + "]") : null);
        }
        else if (root.RootKind is ClrRootKind.StaticVar or ClrRootKind.StrongHandle or ClrRootKind.PinnedHandle)
        {
            string? Static(int i) => i < addresses.Count && statics.Value.TryGetValue(addresses[i], out var n) ? n : null;
            var firstIsArray = heap.GetObjectType(addresses[0])?.Name == "System.Object[]";
            (label, dropFirst) = Label(root.RootKind, Static(0), Static(1), firstIsArray);
        }
        else
        {
            label = new RootLabel(KindName(root.RootKind), null, null, null);
        }

        var links = addresses
            .Skip(dropFirst ? 1 : 0)
            .Select(a => new ChainLinkInfo(Names.Hex(a), Names.Cut(heap.GetObjectType(a)?.Name ?? "?")))
            .ToList();
        var (kept, omitted) = Cut(links);
        return new RootPath(label, kept, omitted);
    }

    /// <summary>The objects static reference fields hold, and "Type.field" for each (first one wins).</summary>
    private static Dictionary<ulong, string> StaticFields(ClrRuntime runtime)
    {
        var map = new Dictionary<ulong, string>();
        var domains = runtime.AppDomains.ToList();
        if (runtime.SystemDomain is { } system)
        {
            domains.Add(system);
        }

        if (runtime.SharedDomain is { } shared)
        {
            domains.Add(shared);
        }

        foreach (var domain in domains)
        {
            foreach (var module in domain.Modules)
            {
                try
                {
                    foreach (var type in module.EnumerateTypesWithStaticFields())
                    {
                        foreach (var field in type.StaticFields)
                        {
                            if (!field.IsObjectReference)
                            {
                                continue;
                            }

                            var obj = field.ReadObject(domain);
                            if (!obj.IsNull)
                            {
                                map.TryAdd(obj.Address, Names.Cut(type.Name + "." + field.Name));
                            }
                        }
                    }
                }
                catch (Exception e) when (e is InvalidOperationException or IOException or ClrDiagnosticsException)
                {
                    // A module ClrMD can't read: its statics stay unnamed.
                }
            }
        }

        return map;
    }

    private static string Quote(string s) => "'" + (s.Length <= 100 ? s : s[..100]) + "'";
}
