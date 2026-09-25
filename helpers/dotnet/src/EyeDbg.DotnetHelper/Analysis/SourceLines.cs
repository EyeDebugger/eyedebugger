// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Reflection.Metadata;
using System.Reflection.Metadata.Ecma335;
using System.Reflection.PortableExecutable;

namespace EyeDbg.DotnetHelper.Analysis;

/// <summary>A source location: the document path the PDB records and a 1-based line.</summary>
internal readonly record struct SourceLine(string File, int Line);

/// <summary>
/// Source lines of managed frames, best effort: the portable PDB embedded in the module the dump
/// names, or next to it (same name, .pdb) when its id matches the module's. The paths come from a
/// dump, which may be another user's: only local absolute paths (<see cref="IsLocalAbsolute"/>: no
/// UNC or device paths, so nothing reaches the network), non-empty files (FIFOs and devices report
/// 0 bytes) of at most <see cref="MaxFileBytes"/> named .dll, .exe or .pdb are read, and an open
/// that still blocks (a file swapped for a FIFO) is given up after <see cref="OpenTimeout"/>. Any
/// error means no line.
/// </summary>
internal sealed class SourceLines : IDisposable
{
    public const long MaxFileBytes = 64L * 1024 * 1024;

    /// <summary>How long opening a module or PDB may take (local files open at once).</summary>
    public static readonly TimeSpan OpenTimeout = TimeSpan.FromSeconds(2);

    private readonly Dictionary<string, MetadataReaderProvider?> _pdbs = new(StringComparer.Ordinal);

    /// <summary>The line of an IL offset in a method, or null.</summary>
    public SourceLine? Find(string? modulePath, int methodToken, int ilOffset)
    {
        if (modulePath is null || ilOffset < 0 || (methodToken >> 24) != 0x06)
        {
            return null;
        }

        if (!_pdbs.TryGetValue(modulePath, out var provider))
        {
            provider = Open(modulePath);
            _pdbs[modulePath] = provider;
        }

        if (provider is null)
        {
            return null;
        }

        try
        {
            var reader = provider.GetMetadataReader();
            var handle = MetadataTokens.MethodDefinitionHandle(methodToken & 0xffffff);
            SequencePoint? best = null;
            foreach (var sp in reader.GetMethodDebugInformation(handle).GetSequencePoints())
            {
                if (!sp.IsHidden && sp.Offset <= ilOffset && (best is null || sp.Offset >= best.Value.Offset))
                {
                    best = sp;
                }
            }

            if (best is not { } found)
            {
                return null;
            }

            var name = reader.GetString(reader.GetDocument(found.Document).Name);
            return new SourceLine(Names.Cut(name), found.StartLine);
        }
        catch (Exception e) when (e is BadImageFormatException or ArgumentException or InvalidOperationException)
        {
            return null;
        }
    }

    public void Dispose()
    {
        foreach (var p in _pdbs.Values)
        {
            p?.Dispose();
        }
    }

    /// <summary>The module's portable PDB: embedded, else the matching file next to it.</summary>
    internal static MetadataReaderProvider? Open(string modulePath)
    {
        if (!Readable(modulePath, ".dll", ".exe"))
        {
            return null;
        }

        try
        {
            using var module = OpenBounded(modulePath, OpenTimeout);
            if (module is null)
            {
                return null;
            }

            using var pe = new PEReader(module);
            var entries = pe.ReadDebugDirectory();
            foreach (var entry in entries)
            {
                if (entry.Type == DebugDirectoryEntryType.EmbeddedPortablePdb)
                {
                    return pe.ReadEmbeddedPortablePdbDebugDirectoryData(entry);
                }
            }

            var codeView = entries.FirstOrDefault(e => e.Type == DebugDirectoryEntryType.CodeView);
            if (codeView.DataSize == 0 || !codeView.IsPortableCodeView)
            {
                return null;
            }

            var id = pe.ReadCodeViewDebugDirectoryData(codeView).Guid;
            var pdbPath = Path.ChangeExtension(modulePath, ".pdb");
            if (!Readable(pdbPath, ".pdb"))
            {
                return null;
            }

            if (OpenBounded(pdbPath, OpenTimeout) is not { } pdb)
            {
                return null;
            }

            var provider = MetadataReaderProvider.FromPortablePdbStream(pdb);
            try
            {
                if (provider.GetMetadataReader().DebugMetadataHeader is { } header && new BlobContentId(header.Id).Guid == id)
                {
                    return provider;
                }
            }
            catch (BadImageFormatException)
            {
            }

            provider.Dispose();
            return null;
        }
        catch (Exception e) when (e is BadImageFormatException or IOException or UnauthorizedAccessException or InvalidOperationException or ArgumentException)
        {
            return null;
        }
    }

    /// <summary>
    /// Whether path is a local absolute path, by ClrMD's own rule for dump-named paths
    /// (PathUtilities.IsSafeAbsoluteLocalPath): never two leading separators (UNC, \\?\, \\.\);
    /// on Windows a drive root (X:\ or X:/), elsewhere a leading /. Checked before any file system
    /// call: on Windows even a stat of a UNC path connects to its server.
    /// </summary>
    internal static bool IsLocalAbsolute(string path, bool windows)
    {
        static bool Separator(char c) => c is '/' or '\\';

        if (string.IsNullOrWhiteSpace(path) || path.Contains('\0', StringComparison.Ordinal) || (path.Length >= 2 && Separator(path[0]) && Separator(path[1])))
        {
            return false;
        }

        return windows
            ? path.Length >= 3 && char.IsAsciiLetter(path[0]) && path[1] == ':' && Separator(path[2])
            : path[0] == '/';
    }

    /// <summary>
    /// Opens path for reading, or null when that doesn't finish within timeout (a FIFO with no
    /// writer blocks its open). The blocked open is left to a background thread; a stream it may
    /// still produce is closed.
    /// </summary>
    internal static FileStream? OpenBounded(string path, TimeSpan timeout)
    {
        var open = Task.Run(() => File.OpenRead(path));
        try
        {
            if (open.Wait(timeout))
            {
                return open.Result;
            }
        }
        catch (AggregateException e) when (e.InnerException is IOException or UnauthorizedAccessException or ArgumentException or NotSupportedException)
        {
            return null;
        }

        _ = open.ContinueWith(t => t.Result.Dispose(), CancellationToken.None, TaskContinuationOptions.OnlyOnRanToCompletion, TaskScheduler.Default);
        return null;
    }

    private static bool Readable(string path, params string[] extensions)
    {
        if (!IsLocalAbsolute(path, OperatingSystem.IsWindows()) || !extensions.Contains(Path.GetExtension(path), StringComparer.OrdinalIgnoreCase))
        {
            return false;
        }

        var info = new FileInfo(path);
        return info.Exists && info.LinkTarget is null && info.Length is > 0 and <= MaxFileBytes;
    }
}
