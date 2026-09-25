// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Buffers.Binary;
using System.Runtime.InteropServices;
using EyeDbg.DotnetHelper.Analysis;

namespace EyeDbg.DotnetHelper.Tests;

public class ElfBuildIdTests
{
    private static readonly byte[] _id = [0xde, 0xad, 0xbe, 0xef, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16];

    public static TheoryData<string, bool> Images => new()
    {
        { "elf64", true },
        { "elf32", true },
        { "other-note-first", true },
        { "big-endian", false },
        { "not-elf", false },
        { "truncated-header", false },
        { "phnum-beyond-file", false },
        { "phoff-beyond-file", false },
        { "too-many-headers", false },
        { "oversized-note", false },
        { "note-beyond-file", false },
        { "no-note", false },
        { "desc-beyond-segment", false },
        { "empty", false },
    };

    [Theory]
    [MemberData(nameof(Images))]
    public void ReadsTheGnuBuildId(string image, bool found)
    {
        using var stream = new MemoryStream(Build(image));

        var got = ElfBuildId.Read(stream);

        if (found)
        {
            Assert.Equal(_id, got);
        }
        else
        {
            Assert.Null(got);
        }
    }

    [Fact]
    public void MissingFileIsNull() =>
        Assert.Null(ElfBuildId.Read(Path.Combine(Path.GetTempPath(), "eyedbg-no-such-file-" + Guid.NewGuid().ToString("N"))));

    [Fact]
    public void ReadsThisRuntimesCoreclrOnLinux()
    {
        Assert.SkipUnless(OperatingSystem.IsLinux(), "libcoreclr.so is a Linux file");

        var id = ElfBuildId.Read(Path.Combine(RuntimeEnvironment.GetRuntimeDirectory(), "libcoreclr.so"));

        Assert.NotNull(id);
        Assert.Equal(20, id.Length);
    }

    /// <summary>A crafted ELF image: header, program headers, then a note segment.</summary>
    private static byte[] Build(string kind)
    {
        if (kind == "empty")
        {
            return [];
        }

        bool is64 = kind != "elf32";
        int ehsize = is64 ? 64 : 52, phentsize = is64 ? 56 : 32;
        var notes = new List<byte>();
        if (kind == "other-note-first")
        {
            notes.AddRange(Note("GNU\0"u8.ToArray(), 1, [1, 2, 3, 4]));
        }

        if (kind != "no-note")
        {
            var desc = kind == "desc-beyond-segment" ? _id.Concat(new byte[40]).ToArray() : _id;
            var note = Note("GNU\0"u8.ToArray(), 3, desc);
            if (kind == "desc-beyond-segment")
            {
                note = note[..(note.Length - 40)]; // descsz says 60, the segment ends after 20
            }

            notes.AddRange(note);
        }

        int phnum = kind == "too-many-headers" ? 300 : 2;
        int phoff = ehsize;
        int noteOffset = phoff + (phnum * phentsize);
        var image = new byte[noteOffset + notes.Count];
        var s = image.AsSpan();
        s[0] = 0x7f;
        s[1] = (byte)'E';
        s[2] = (byte)'L';
        s[3] = (byte)'F';
        s[4] = (byte)(is64 ? 2 : 1);
        s[5] = (byte)(kind == "big-endian" ? 2 : 1);
        if (kind == "not-elf")
        {
            s[1] = (byte)'X';
        }

        long writtenPhoff = kind == "phoff-beyond-file" ? image.Length + 10 : phoff;
        int writtenPhnum = kind == "phnum-beyond-file" ? 1000 : phnum;
        if (is64)
        {
            BinaryPrimitives.WriteUInt64LittleEndian(s[0x20..], (ulong)writtenPhoff);
            BinaryPrimitives.WriteUInt16LittleEndian(s[0x36..], (ushort)phentsize);
            BinaryPrimitives.WriteUInt16LittleEndian(s[0x38..], (ushort)writtenPhnum);
        }
        else
        {
            BinaryPrimitives.WriteUInt32LittleEndian(s[0x1c..], (uint)writtenPhoff);
            BinaryPrimitives.WriteUInt16LittleEndian(s[0x2a..], (ushort)phentsize);
            BinaryPrimitives.WriteUInt16LittleEndian(s[0x2c..], (ushort)writtenPhnum);
        }

        // Program header 0: PT_LOAD; the last one: PT_NOTE over the notes.
        ulong noteSize = kind == "oversized-note" ? (ulong)ElfBuildId.MaxNoteSegment + 1 : (ulong)notes.Count;
        ulong noteOff = kind == "note-beyond-file" ? (ulong)image.Length : (ulong)noteOffset;
        var ph = s.Slice(phoff + ((phnum - 1) * phentsize), phentsize);
        BinaryPrimitives.WriteUInt32LittleEndian(s[phoff..], 1);
        BinaryPrimitives.WriteUInt32LittleEndian(ph, 4);
        if (is64)
        {
            BinaryPrimitives.WriteUInt64LittleEndian(ph[8..], noteOff);
            BinaryPrimitives.WriteUInt64LittleEndian(ph[32..], noteSize);
        }
        else
        {
            BinaryPrimitives.WriteUInt32LittleEndian(ph[4..], (uint)noteOff);
            BinaryPrimitives.WriteUInt32LittleEndian(ph[16..], (uint)noteSize);
        }

        notes.ToArray().CopyTo(s[noteOffset..]);
        if (kind == "truncated-header")
        {
            return image[..40];
        }

        if (kind == "oversized-note")
        {
            Array.Resize(ref image, noteOffset + ElfBuildId.MaxNoteSegment + 10);
        }

        return image;
    }

    private static byte[] Note(byte[] name, uint type, byte[] desc)
    {
        var note = new byte[12 + Align4(name.Length) + Align4(desc.Length)];
        BinaryPrimitives.WriteUInt32LittleEndian(note, (uint)name.Length);
        BinaryPrimitives.WriteUInt32LittleEndian(note.AsSpan(4), (uint)desc.Length);
        BinaryPrimitives.WriteUInt32LittleEndian(note.AsSpan(8), type);
        name.CopyTo(note, 12);
        desc.CopyTo(note, 12 + Align4(name.Length));
        return note;
    }

    private static int Align4(int n) => (n + 3) & ~3;
}
