// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Reflection;
using System.Reflection.PortableExecutable;
using System.Runtime.CompilerServices;
using EyeDbg.DotnetHelper.Analysis;

namespace EyeDbg.DotnetHelper.Tests;

public sealed class SourceLinesTests : IDisposable
{
    private readonly string _dir = Directory.CreateTempSubdirectory("eyedbg-lines-tests-").FullName;

    public void Dispose() => Directory.Delete(_dir, recursive: true);

    [Fact]
    public void FindsALineInAPdbNextToTheModule() => AssertFindsItself(Where());

    [Fact]
    public void FindsALineInAnEmbeddedPdb()
    {
        // The helper embeds its PDB (DebugType embedded).
        var method = typeof(Names).GetMethod(nameof(Names.Hex))!;
        using var lines = new SourceLines();

        var line = lines.Find(typeof(Names).Assembly.Location, method.MetadataToken, 0);

        Assert.NotNull(line);
        Assert.EndsWith("Names.cs", line.Value.File, StringComparison.Ordinal);
        Assert.True(line.Value.Line > 0);
    }

    [Fact]
    public void NoPdbMeansNoLine()
    {
        var copy = Path.Combine(_dir, Path.GetFileName(typeof(SourceLinesTests).Assembly.Location));
        File.Copy(typeof(SourceLinesTests).Assembly.Location, copy);
        using var lines = new SourceLines();

        Assert.Null(lines.Find(copy, Token(), 0));
    }

    [Fact]
    public void AGarbagePdbMeansNoLine()
    {
        var copy = Path.Combine(_dir, Path.GetFileName(typeof(SourceLinesTests).Assembly.Location));
        File.Copy(typeof(SourceLinesTests).Assembly.Location, copy);
        File.WriteAllBytes(Path.ChangeExtension(copy, ".pdb"), new byte[4096]);
        using var lines = new SourceLines();

        Assert.Null(lines.Find(copy, Token(), 0));
    }

    [Fact]
    public void AnotherModulesPdbMeansNoLine()
    {
        // ClrMD's assembly has a portable CodeView entry and no embedded PDB; this test
        // assembly's PDB (valid, another id) next to a copy of it must not be used.
        var module = typeof(Microsoft.Diagnostics.Runtime.DataTarget).Assembly.Location;
        using (var pe = new PEReader(File.OpenRead(module)))
        {
            var entries = pe.ReadDebugDirectory();
            Assert.Contains(entries, e => e.Type == DebugDirectoryEntryType.CodeView && e.IsPortableCodeView);
            Assert.DoesNotContain(entries, e => e.Type == DebugDirectoryEntryType.EmbeddedPortablePdb);
        }

        var copy = Path.Combine(_dir, Path.GetFileName(module));
        File.Copy(module, copy);
        File.Copy(Path.ChangeExtension(typeof(SourceLinesTests).Assembly.Location, ".pdb"), Path.ChangeExtension(copy, ".pdb"));
        using var lines = new SourceLines();

        Assert.Null(lines.Find(copy, Token(), 0));
    }

    [Theory]
    [InlineData(null, 0x06000001, 0)]
    [InlineData("relative.dll", 0x06000001, 0)]
    [InlineData("/nope/x.txt", 0x06000001, 0)]
    [InlineData("/nope/x.dll", 0x06000001, 0)]
    [InlineData("/nope/x.dll", 0x02000001, 0)] // a type token, not a method
    [InlineData("/nope/x.dll", 0x06000001, -1)] // no IL offset
    public void BadInputsMeanNoLine(string? module, int token, int il)
    {
        using var lines = new SourceLines();

        Assert.Null(lines.Find(module, token, il));
    }

    [Theory]
    [InlineData(@"C:\app\app.dll", true, true)]
    [InlineData("C:/app/app.dll", true, true)]
    [InlineData(@"\\attacker\share\app.dll", true, false)] // UNC: would connect to the server
    [InlineData("//attacker/share/app.dll", true, false)]
    [InlineData(@"\\?\C:\app\app.dll", true, false)]
    [InlineData(@"\\.\pipe\x.dll", true, false)]
    [InlineData(@"\app\app.dll", true, false)] // rooted, no drive
    [InlineData(@"C:app.dll", true, false)] // drive-relative
    [InlineData("app.dll", true, false)]
    [InlineData("/app/app.dll", false, true)]
    [InlineData("//attacker/share/app.dll", false, false)]
    [InlineData(@"\\attacker\share\app.dll", false, false)]
    [InlineData("app.dll", false, false)]
    [InlineData("", false, false)]
    [InlineData("/app/a\0.dll", false, false)]
    public void OnlyLocalAbsolutePaths(string path, bool windows, bool want) =>
        Assert.Equal(want, SourceLines.IsLocalAbsolute(path, windows));

    [Fact]
    public void AUncModulePathIsNeverOpened()
    {
        // On Windows a stat of it would connect to the host (".invalid" never resolves anyway).
        using var lines = new SourceLines();

        Assert.Null(lines.Find(@"\\eyedbg-test.invalid\share\app.dll", Token(), 0));
        Assert.Null(SourceLines.Open("//eyedbg-test.invalid/share/app.dll"));
    }

    [Fact]
    public async Task AFifoIsNeitherReadNorWaitedOn()
    {
        if (OperatingSystem.IsWindows())
        {
            Assert.Skip("FIFOs are Unix files");
            return;
        }

        var fifo = Path.Combine(_dir, "app.dll");
        using (var mkfifo = System.Diagnostics.Process.Start("mkfifo", [fifo]))
        {
            await mkfifo.WaitForExitAsync(TestContext.Current.CancellationToken);
            Assert.Equal(0, mkfifo.ExitCode);
        }

        // The size check refuses it (a FIFO reports 0 bytes) before any open.
        var find = Task.Run(() => SourceLines.Open(fifo), TestContext.Current.CancellationToken);
        Assert.Null(await find.WaitAsync(TimeSpan.FromSeconds(10), TestContext.Current.CancellationToken));

        // And an open that blocks (a file swapped for a FIFO after the check) is given up.
        var open = Task.Run(() => SourceLines.OpenBounded(fifo, TimeSpan.FromMilliseconds(200)), TestContext.Current.CancellationToken);
        Assert.Null(await open.WaitAsync(TimeSpan.FromSeconds(10), TestContext.Current.CancellationToken));
    }

    [Fact]
    public void AnEmptyModuleIsSkipped()
    {
        var empty = Path.Combine(_dir, "empty.dll");
        File.WriteAllBytes(empty, []);

        Assert.Null(SourceLines.Open(empty));
    }

    [Fact]
    public void OpenBoundedReadsAFile()
    {
        using var stream = SourceLines.OpenBounded(typeof(SourceLinesTests).Assembly.Location, SourceLines.OpenTimeout);

        Assert.NotNull(stream);
        Assert.True(stream.Length > 0);
    }

    private static void AssertFindsItself((int Line, int Token) at)
    {
        using var lines = new SourceLines();

        var line = lines.Find(typeof(SourceLinesTests).Assembly.Location, at.Token, 0);

        Assert.NotNull(line);
        Assert.EndsWith("SourceLinesTests.cs", line.Value.File, StringComparison.Ordinal);
        Assert.InRange(line.Value.Line, at.Line - 3, at.Line + 1);
    }

    private static int Token() => typeof(SourceLinesTests).GetMethod(nameof(Where), BindingFlags.NonPublic | BindingFlags.Static)!.MetadataToken;

    [MethodImpl(MethodImplOptions.NoInlining)]
    private static (int Line, int Token) Where() => (Here(), Token());

    private static int Here([CallerLineNumber] int line = 0) => line;
}
