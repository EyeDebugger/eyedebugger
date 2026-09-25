// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Runtime.InteropServices;
using System.Text.RegularExpressions;
using EyeDbg.DotnetHelper.Rpc;
using Microsoft.Diagnostics.Runtime;

namespace EyeDbg.DotnetHelper.Analysis;

/// <summary>The runtime a dump was taken with, as results report it.</summary>
internal sealed record RuntimeInfo(string Version, string Gc, int Heaps, string Platform, string Arch);

/// <summary>An open dump and its .NET runtime; disposing closes both.</summary>
internal sealed class LoadedDump : IDisposable
{
    public LoadedDump(DataTarget target, ClrRuntime runtime, RuntimeInfo info)
    {
        Target = target;
        Runtime = runtime;
        Info = info;
    }

    public DataTarget Target { get; }

    public ClrRuntime Runtime { get; }

    public RuntimeInfo Info { get; }

    public void Dispose()
    {
        Runtime.Dispose();
        Target.Dispose();
    }
}

/// <summary>
/// Opens a dump with ClrMD without letting it fetch anything, and picks the DAC — native code
/// the helper loads — by a provenance rule (docs/adr/0016, P2-M7): the installed runtime of the
/// dotnet running the helper, or the dump's own recorded runtime directory only when eyedbg took
/// the dump itself (trustRecordedRuntime) and nobody else can write there.
/// </summary>
internal static partial class DumpLoader
{
    /// <summary>A runtime directory name: a version, nothing that could leave the directory.</summary>
    [GeneratedRegex(@"^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?\z", RegexOptions.CultureInvariant, matchTimeoutMilliseconds: 1000)]
    private static partial Regex VersionName();

    /// <summary>Whether a directory name is a runtime version (and so no path traversal).</summary>
    public static bool IsVersionName(string name) => name.Length <= 64 && !name.Contains("..", StringComparison.Ordinal) && VersionName().IsMatch(name);

    /// <summary>Opens path; errors are DUMP_UNSUPPORTED or DUMP_RUNTIME_MISSING.</summary>
    public static LoadedDump Open(string path, bool trustRecordedRuntime)
    {
        DataTarget target;
        try
        {
            // No symbol servers, no shared cache, no _NT_SYMBOL_PATH (NoFileLocator). The
            // Windows DAC signature check (VerifyDacOnWindows) and the parser limits stay at
            // their defaults.
            target = DataTarget.LoadDump(path, new DataTargetOptions { FileLocator = new NoFileLocator() });
        }
        catch (Exception e) when (IsUnreadable(e))
        {
            throw Unreadable(path, e);
        }

        try
        {
            return Load(target, path, trustRecordedRuntime);
        }
        catch
        {
            target.Dispose();
            throw;
        }
    }

    private static LoadedDump Load(DataTarget target, string path, bool trustRecordedRuntime)
    {
        OSPlatform platform;
        Architecture arch;
        ClrInfo? clr;
        try
        {
            platform = target.DataReader.TargetPlatform;
            arch = target.DataReader.Architecture;
            clr = target.ClrVersions.FirstOrDefault(c => c.Flavor == ClrFlavor.Core);
        }
        catch (Exception e) when (IsUnreadable(e))
        {
            throw Unreadable(path, e);
        }

        if (PlatformRefusal(platform, arch, CurrentPlatform(), RuntimeInformation.ProcessArchitecture) is { } refusal)
        {
            throw refusal;
        }

        if (clr is null)
        {
            throw new HelperException(
                ErrorCodes.DumpUnsupported,
                "no .NET (Core) runtime in this dump",
                "only dumps of .NET Core 3.0 and later processes can be analyzed");
        }

        var recordedDir = Path.GetDirectoryName(clr.ModuleInfo.FileName);
        var version = VersionOf(clr, recordedDir);
        var candidates = Candidates(InstalledRuntimesRoot(), recordedDir, trustRecordedRuntime, DacName());
        var tried = new List<string>();
        foreach (var candidate in candidates)
        {
            tried.Add(candidate.Path);
            if (!IsRegularFile(candidate.Path))
            {
                continue;
            }

            if (candidate.Recorded && (OthersCanWrite(candidate.Path) || OthersCanWrite(Path.GetDirectoryName(candidate.Path)!)))
            {
                continue;
            }

            if (TryCreate(clr, candidate.Path) is { } runtime)
            {
                var heap = runtime.Heap;
                var info = new RuntimeInfo(version, heap.IsServer ? "server" : "workstation", heap.SubHeaps.Length, PlatformName(platform), ArchName(arch));
                return new LoadedDump(target, runtime, info);
            }
        }

        throw new HelperException(
            ErrorCodes.DumpRuntimeMissing,
            "the dump's .NET runtime " + version + " (" + PlatformName(platform) + "-" + ArchName(arch) + ") isn't installed here",
            "install that runtime, or analyze the dump on the machine that took it; searched: " + (tried.Count == 0 ? "no runtime directory" : string.Join(", ", tried)));
    }

    /// <summary>Where a DAC may come from, in order.</summary>
    /// <param name="Path">The DAC's full path.</param>
    /// <param name="Recorded">From the dump's recorded runtime directory (candidate b).</param>
    internal readonly record struct Candidate(string Path, bool Recorded);

    /// <summary>
    /// The DAC candidates: (a) the installed runtime of this dotnet whose version directory has the
    /// name of the dump's runtime directory; (b) the dump's recorded runtime directory, only when
    /// trusted. installedRoot is shared/Microsoft.NETCore.App of this dotnet.
    /// </summary>
    internal static List<Candidate> Candidates(string? installedRoot, string? recordedDir, bool trustRecordedRuntime, string dacName)
    {
        var list = new List<Candidate>(2);
        var name = recordedDir is null ? null : Path.GetFileName(recordedDir);
        if (installedRoot is not null && name is not null && IsVersionName(name))
        {
            list.Add(new Candidate(Path.Combine(installedRoot, name, dacName), false));
        }

        if (trustRecordedRuntime && recordedDir is not null && Path.IsPathFullyQualified(recordedDir))
        {
            var recorded = Path.Combine(recordedDir, dacName);
            if (!list.Any(c => string.Equals(c.Path, recorded, StringComparison.Ordinal)))
            {
                list.Add(new Candidate(recorded, true));
            }
        }

        return list;
    }

    /// <summary>The error for a dump of another OS or architecture than this machine, or null.</summary>
    internal static HelperException? PlatformRefusal(OSPlatform dump, Architecture dumpArch, OSPlatform here, Architecture hereArch)
    {
        if (dump == here && dumpArch == hereArch)
        {
            return null;
        }

        var theirs = PlatformName(dump) + "-" + ArchName(dumpArch);
        return new HelperException(
            ErrorCodes.DumpUnsupported,
            "a " + theirs + " dump; analyze it on " + theirs,
            "this machine is " + PlatformName(here) + "-" + ArchName(hereArch) + ", and a dump is analyzed by its own OS's runtime libraries");
    }

    /// <summary>
    /// Whether a Unix file or directory is writable by anyone but its owner: group or other write
    /// (on macOS every local user's primary group is staff, so group write admits them all).
    /// </summary>
    internal static bool OthersCanWrite(string path)
    {
        if (OperatingSystem.IsWindows())
        {
            return false; // the profile's ACL is the boundary there (docs/adr/0016)
        }

        try
        {
            return (File.GetUnixFileMode(path) & (UnixFileMode.GroupWrite | UnixFileMode.OtherWrite)) != 0;
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            return true;
        }
    }

    private static bool IsRegularFile(string path)
    {
        var info = new FileInfo(path);
        return info.Exists && info.LinkTarget is null;
    }

    /// <summary>A runtime from the DAC at path, or null when it isn't the dump's runtime.</summary>
    private static ClrRuntime? TryCreate(ClrInfo clr, string dac)
    {
        try
        {
            if (OperatingSystem.IsLinux())
            {
                // ClrMD can't read the runtime's file version on Linux (it reports 0.0): the
                // directory's libcoreclr.so must have the dump's runtime build id instead.
                var id = ElfBuildId.Read(Path.Combine(Path.GetDirectoryName(dac)!, "libcoreclr.so"));
                if (id is null || clr.BuildId.IsDefaultOrEmpty || !clr.BuildId.AsSpan().SequenceEqual(id))
                {
                    return null;
                }

                return clr.CreateRuntime(dac, ignoreMismatch: true);
            }

            // Windows and macOS: ClrMD compares the DAC's file version with the dump's runtime
            // before loading it (and checks its signature on Windows).
            return clr.CreateRuntime(dac, ignoreMismatch: false);
        }
        catch (Exception e) when (e is ClrDiagnosticsException or IOException or InvalidOperationException or BadImageFormatException or DllNotFoundException)
        {
            return null;
        }
    }

    /// <summary>The runtime version: its directory's name, else ClrMD's (0.0 on Linux).</summary>
    private static string VersionOf(ClrInfo clr, string? recordedDir)
    {
        var name = recordedDir is null ? null : Path.GetFileName(recordedDir);
        if (name is not null && IsVersionName(name))
        {
            return name;
        }

        return clr.Version.Major > 0 ? clr.Version.ToString() : "unknown";
    }

    /// <summary>shared/Microsoft.NETCore.App of the dotnet running the helper.</summary>
    private static string? InstalledRuntimesRoot()
    {
        var dir = RuntimeEnvironment.GetRuntimeDirectory().TrimEnd(Path.DirectorySeparatorChar, Path.AltDirectorySeparatorChar);
        return Path.GetDirectoryName(dir);
    }

    private static string DacName() =>
        OperatingSystem.IsWindows() ? "mscordaccore.dll" : OperatingSystem.IsMacOS() ? "libmscordaccore.dylib" : "libmscordaccore.so";

    private static OSPlatform CurrentPlatform() =>
        OperatingSystem.IsWindows() ? OSPlatform.Windows
        : OperatingSystem.IsMacOS() ? OSPlatform.OSX
        : OperatingSystem.IsFreeBSD() ? OSPlatform.FreeBSD
        : OSPlatform.Linux;

    internal static string PlatformName(OSPlatform p) =>
        p == OSPlatform.Windows ? "windows"
        : p == OSPlatform.OSX ? "osx"
        : p == OSPlatform.Linux ? "linux"
        : p == OSPlatform.FreeBSD ? "freebsd"
        : p.ToString().ToLowerInvariant();

    internal static string ArchName(Architecture a) => a.ToString().ToLowerInvariant();

    private static bool IsUnreadable(Exception e) =>
        e is InvalidDataException or IOException or NotSupportedException or UnauthorizedAccessException
            or ClrDiagnosticsException or ArgumentException or OverflowException or IndexOutOfRangeException;

    private static HelperException Unreadable(string path, Exception e)
    {
        var why = e.Message.Length <= 200 ? e.Message : e.Message[..200];
        return new HelperException(
            ErrorCodes.DumpUnsupported,
            "can't read " + path + " as a dump: " + e.GetType().Name + ": " + why,
            "give it a dump taken by 'eyedbg dotnet dump' (or dotnet-dump, createdump)");
    }
}
