// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Buffers.Binary;

namespace EyeDbg.DotnetHelper.Analysis;

/// <summary>
/// Reads the GNU build id of a little-endian ELF file (Linux libcoreclr.so): the check that a
/// runtime directory on this machine holds the runtime a dump was taken with. Bounded: at most
/// <see cref="MaxProgramHeaders"/> program headers and <see cref="MaxNoteSegment"/> bytes per note
/// segment; anything malformed is null.
/// </summary>
internal static class ElfBuildId
{
    public const int MaxProgramHeaders = 256;
    public const int MaxNoteSegment = 64 * 1024;

    private const uint PtNote = 4;
    private const uint NtGnuBuildId = 3;

    /// <summary>The build id of the ELF file at path, or null.</summary>
    public static byte[]? Read(string path)
    {
        try
        {
            using var file = new FileStream(path, FileMode.Open, FileAccess.Read, FileShare.Read);
            return Read(file);
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            return null;
        }
    }

    /// <summary>The build id of the ELF image in a seekable stream, or null.</summary>
    public static byte[]? Read(Stream stream)
    {
        Span<byte> header = stackalloc byte[64];
        if (!ReadAt(stream, 0, header[..52]))
        {
            return null;
        }

        // ELF magic, class 1 (32-bit) or 2 (64-bit), data 1 (little-endian).
        if (header[0] != 0x7f || header[1] != (byte)'E' || header[2] != (byte)'L' || header[3] != (byte)'F' || header[4] is not (1 or 2) || header[5] != 1)
        {
            return null;
        }

        bool is64 = header[4] == 2;
        if (is64 && !ReadAt(stream, 0, header))
        {
            return null;
        }

        ulong phoff = is64 ? BinaryPrimitives.ReadUInt64LittleEndian(header[0x20..]) : BinaryPrimitives.ReadUInt32LittleEndian(header[0x1c..]);
        int phentsize = BinaryPrimitives.ReadUInt16LittleEndian(header[(is64 ? 0x36 : 0x2a)..]);
        int phnum = BinaryPrimitives.ReadUInt16LittleEndian(header[(is64 ? 0x38 : 0x2c)..]);
        int minEntry = is64 ? 56 : 32;
        if (phnum is 0 or > MaxProgramHeaders || phentsize < minEntry || phentsize > 1024)
        {
            return null;
        }

        var length = (ulong)stream.Length;
        if (phoff > length || (ulong)phnum * (ulong)phentsize > length - phoff)
        {
            return null;
        }

        var entry = new byte[phentsize];
        for (int i = 0; i < phnum; i++)
        {
            if (!ReadAt(stream, (long)phoff + ((long)i * phentsize), entry))
            {
                return null;
            }

            if (BinaryPrimitives.ReadUInt32LittleEndian(entry) != PtNote)
            {
                continue;
            }

            ulong offset = is64 ? BinaryPrimitives.ReadUInt64LittleEndian(entry.AsSpan(8)) : BinaryPrimitives.ReadUInt32LittleEndian(entry.AsSpan(4));
            ulong size = is64 ? BinaryPrimitives.ReadUInt64LittleEndian(entry.AsSpan(32)) : BinaryPrimitives.ReadUInt32LittleEndian(entry.AsSpan(16));
            if (size > MaxNoteSegment || offset > length || size > length - offset)
            {
                return null;
            }

            var notes = new byte[size];
            if (!ReadAt(stream, (long)offset, notes))
            {
                return null;
            }

            if (FindGnuBuildId(notes) is { } id)
            {
                return id;
            }
        }

        return null;
    }

    /// <summary>The desc of the first GNU build-id note in a note segment, or null.</summary>
    internal static byte[]? FindGnuBuildId(ReadOnlySpan<byte> notes)
    {
        int at = 0;
        while (notes.Length - at >= 12)
        {
            uint namesz = BinaryPrimitives.ReadUInt32LittleEndian(notes[at..]);
            uint descsz = BinaryPrimitives.ReadUInt32LittleEndian(notes[(at + 4)..]);
            uint type = BinaryPrimitives.ReadUInt32LittleEndian(notes[(at + 8)..]);
            long nameEnd = at + 12L + Align4(namesz);
            long descEnd = nameEnd + Align4(descsz);
            if (nameEnd > notes.Length || descEnd > notes.Length)
            {
                return null;
            }

            if (type == NtGnuBuildId && namesz == 4 && notes.Slice(at + 12, 4).SequenceEqual("GNU\0"u8) && descsz is > 0 and <= 64)
            {
                return notes.Slice((int)nameEnd, (int)descsz).ToArray();
            }

            at = (int)descEnd;
        }

        return null;
    }

    private static long Align4(uint n) => (n + 3L) & ~3L;

    private static bool ReadAt(Stream stream, long offset, Span<byte> buffer)
    {
        if (offset < 0 || offset > stream.Length - buffer.Length)
        {
            return false;
        }

        stream.Position = offset;
        stream.ReadExactly(buffer);
        return true;
    }
}
