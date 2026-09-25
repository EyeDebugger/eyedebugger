// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Text.Json;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.NETCore.Client;

namespace EyeDbg.DotnetHelper.Methods;

/// <summary>Lists the pids of processes that publish a .NET diagnostics endpoint.</summary>
internal interface IProcessSource
{
    IEnumerable<int> PublishedProcesses();
}

/// <summary>The endpoints NETCore.Client finds (sockets in the temp dir; Windows pipes).</summary>
internal sealed class DiagnosticsProcessSource : IProcessSource
{
    public IEnumerable<int> PublishedProcesses() => DiagnosticsClient.GetPublishedProcesses();
}

/// <summary>The processes method: pids only, without the helper's own; eyedbg adds names and owners.</summary>
internal static class ProcessesMethod
{
    public const string Method = "processes";

    public static MethodHandler Handler(IProcessSource source, int selfPid) =>
        (parameters, _, _) =>
        {
            _ = parameters;
            return Task.FromResult(new Reply(List(source, selfPid), ProtocolJson.Default.ProcessesResult));
        };

    internal static ProcessesResult List(IProcessSource source, int selfPid) =>
        new(source.PublishedProcesses()
            .Where(pid => pid > 0 && pid != selfPid)
            .Distinct()
            .Order()
            .Select(pid => new ProcessEntry(pid))
            .ToList());
}
