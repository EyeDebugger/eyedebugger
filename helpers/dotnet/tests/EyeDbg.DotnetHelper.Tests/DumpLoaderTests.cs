// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Runtime.InteropServices;
using EyeDbg.DotnetHelper.Analysis;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Tests;

public sealed class DumpLoaderTests : IDisposable
{
    private readonly string _dir = Directory.CreateTempSubdirectory("eyedbg-loader-tests-").FullName;

    public void Dispose() => Directory.Delete(_dir, recursive: true);

    [Theory]
    [InlineData("10.0.12", true)]
    [InlineData("8.0.0", true)]
    [InlineData("9.0.0-preview.7.24405.7", true)]
    [InlineData("10.0.0-rc.1", true)]
    [InlineData("..", false)]
    [InlineData("10.0.1/../../x", false)]
    [InlineData("10.0", false)]
    [InlineData("10.0.1.2", false)]
    [InlineData("10.0.1-..", false)]
    [InlineData("publish", false)]
    [InlineData("", false)]
    [InlineData("10.0.1\n", false)]
    public void VersionNames(string name, bool ok) => Assert.Equal(ok, DumpLoader.IsVersionName(name));

    public static TheoryData<string?, bool, string[]> CandidateCases => new()
    {
        // (a) only, untrusted dump: the installed runtime of the same version.
        { "/opt/app/shared/Microsoft.NETCore.App/10.0.12", false, ["/dotnet/shared/Microsoft.NETCore.App/10.0.12/dac|a"] },

        // Trusted: (a), then the recorded directory (b).
        { "/opt/app/shared/Microsoft.NETCore.App/10.0.12", true, ["/dotnet/shared/Microsoft.NETCore.App/10.0.12/dac|a", "/opt/app/shared/Microsoft.NETCore.App/10.0.12/dac|b"] },

        // A self-contained app: no version directory, so (b) only, and only when trusted.
        { "/home/u/app/publish", false, [] },
        { "/home/u/app/publish", true, ["/home/u/app/publish/dac|b"] },

        // The recorded directory is this dotnet's own: one candidate.
        { "/dotnet/shared/Microsoft.NETCore.App/10.0.12", true, ["/dotnet/shared/Microsoft.NETCore.App/10.0.12/dac|a"] },

        // A relative recorded directory is never trusted.
        { "app/10.0.12", true, ["/dotnet/shared/Microsoft.NETCore.App/10.0.12/dac|a"] },
        { null, true, [] },
    };

    [Theory]
    [MemberData(nameof(CandidateCases))]
    public void OrdersCandidates(string? recordedDir, bool trusted, string[] want)
    {
        Assert.SkipWhen(OperatingSystem.IsWindows(), "the cases use Unix paths");

        var got = DumpLoader.Candidates("/dotnet/shared/Microsoft.NETCore.App", recordedDir, trusted, "dac");

        Assert.Equal(want, got.Select(c => c.Path + (c.Recorded ? "|b" : "|a")));
    }

    [Fact]
    public void OrdersCandidatesOnWindows()
    {
        Assert.SkipUnless(OperatingSystem.IsWindows(), "Windows paths");

        var got = DumpLoader.Candidates(@"C:\dotnet\shared\Microsoft.NETCore.App", @"C:\app\publish", true, "mscordaccore.dll");

        Assert.Equal([new DumpLoader.Candidate(@"C:\app\publish\mscordaccore.dll", true)], got);
    }

    public static TheoryData<int, bool> Modes => new()
    {
        { 0b111_101_101, false }, // 0755
        { 0b110_100_100, false }, // 0644
        { 0b111_000_000, false }, // 0700
        { 0b111_111_101, true }, // 0775: the group can write
        { 0b111_101_111, true }, // 0757: others can write
        { 0b110_110_100, true }, // 0664
        { 0b110_100_110, true }, // 0646
    };

    [Theory]
    [MemberData(nameof(Modes))]
    public void GroupOrOthersWritableIsRefused(int mode, bool refused)
    {
        if (OperatingSystem.IsWindows())
        {
            Assert.Skip("Unix modes");
            return;
        }

        var dir = Directory.CreateDirectory(Path.Combine(_dir, "d" + mode.ToString(System.Globalization.CultureInfo.InvariantCulture))).FullName;
        var file = Path.Combine(dir, "libmscordaccore.so");
        File.WriteAllBytes(file, [0]);
        File.SetUnixFileMode(file, (UnixFileMode)mode);
        File.SetUnixFileMode(dir, (UnixFileMode)mode | UnixFileMode.UserExecute);

        Assert.Equal(refused, DumpLoader.OthersCanWrite(file));
        Assert.Equal(refused, DumpLoader.OthersCanWrite(dir));
    }

    [Fact]
    public void AnUnreadableModeIsRefused()
    {
        if (OperatingSystem.IsWindows())
        {
            Assert.Skip("Unix modes");
            return;
        }

        Assert.True(DumpLoader.OthersCanWrite(Path.Combine(_dir, "missing")));
    }

    [Theory]
    [InlineData("linux", "x64", "osx", "arm64", "a linux-x64 dump; analyze it on linux-x64")]
    [InlineData("linux", "arm64", "linux", "x64", "a linux-arm64 dump; analyze it on linux-arm64")]
    [InlineData("windows", "x64", "linux", "x64", "a windows-x64 dump; analyze it on windows-x64")]
    [InlineData("osx", "arm64", "osx", "arm64", null)]
    public void RefusesOtherPlatforms(string dump, string dumpArch, string here, string hereArch, string? want)
    {
        var e = DumpLoader.PlatformRefusal(Platform(dump), Enum.Parse<Architecture>(dumpArch, ignoreCase: true), Platform(here), Enum.Parse<Architecture>(hereArch, ignoreCase: true));

        Assert.Equal(want, e?.Message);
        if (e is not null)
        {
            Assert.Equal(ErrorCodes.DumpUnsupported, e.Code);
            Assert.Contains("this machine is " + here + "-" + hereArch, e.Hint, StringComparison.Ordinal);
        }
    }

    [Theory]
    [InlineData("text")]
    [InlineData("minidump")]
    [InlineData("elf")]
    [InlineData("macho")]
    [InlineData("empty")]
    public void UnreadableFilesAreUnsupported(string kind)
    {
        var path = Path.Combine(_dir, kind + ".dmp");
        var data = new byte[4096];
        byte[] magic = kind switch
        {
            "text" => "hello, this is not a dump\n"u8.ToArray(),
            "minidump" => "MDMP"u8.ToArray(),
            "elf" => [0x7f, (byte)'E', (byte)'L', (byte)'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4, 0],
            "macho" => [0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 0x01, 0, 0, 0, 0, 4, 0, 0, 0],
            _ => [],
        };
        magic.CopyTo(data, 0);
        File.WriteAllBytes(path, kind == "empty" ? [] : data);

        var e = Assert.Throws<HelperException>(() => DumpLoader.Open(path, trustRecordedRuntime: false));

        Assert.Equal(ErrorCodes.DumpUnsupported, e.Code);
    }

    private static OSPlatform Platform(string name) => name switch
    {
        "linux" => OSPlatform.Linux,
        "osx" => OSPlatform.OSX,
        _ => OSPlatform.Windows,
    };
}
