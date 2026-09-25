// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Text;
using EyeDbg.DotnetHelper.Rpc;

namespace EyeDbg.DotnetHelper.Tests;

public class LineReaderTests
{
    public static TheoryData<string, string[]> Lines => new()
    {
        { "a\nb\n", ["a", "b"] },
        { "a\r\nb\r\n", ["a", "b"] },
        { "a\r\r\n", ["a\r"] },
        { "\n\n", ["", ""] },
        { "last-without-newline", ["last-without-newline"] },
        { "a\nb", ["a", "b"] },
        { "", [] },
    };

    [Theory]
    [MemberData(nameof(Lines))]
    public async Task ReadsLines(string input, string[] want)
    {
        var reader = new LineReader(new MemoryStream(Encoding.UTF8.GetBytes(input)), bufferSize: 3);
        var got = new List<string>();
        while (await reader.ReadLineAsync(TestContext.Current.CancellationToken) is { } line)
        {
            got.Add(Encoding.UTF8.GetString(line));
        }

        Assert.Equal(want, got);
    }

    [Theory]
    [InlineData(LineReader.MaxLineBytes, "\n", true)]
    [InlineData(LineReader.MaxLineBytes + 1, "\n", false)]
    [InlineData(LineReader.MaxLineBytes - 1, "\r\n", true)]
    [InlineData(LineReader.MaxLineBytes, "\r\n", false)]
    [InlineData(LineReader.MaxLineBytes, "", true)]
    [InlineData(LineReader.MaxLineBytes + 1, "", false)]
    public async Task BoundsLinesLikeInternalApi(int length, string ending, bool ok)
    {
        // internal/api: a line with its '\n' may be at most 1 MiB; '\r' counts.
        var input = new byte[length + ending.Length];
        input.AsSpan(0, length).Fill((byte)'x');
        Encoding.ASCII.GetBytes(ending).CopyTo(input, length);
        var reader = new LineReader(new MemoryStream(input));

        if (ok)
        {
            var line = await reader.ReadLineAsync(TestContext.Current.CancellationToken);
            Assert.Equal(length, line!.Length);
            Assert.Null(await reader.ReadLineAsync(TestContext.Current.CancellationToken));
        }
        else
        {
            await Assert.ThrowsAsync<LineTooLongException>(() => reader.ReadLineAsync(TestContext.Current.CancellationToken));
        }
    }

    [Fact]
    public async Task DrainReadsToTheEnd()
    {
        var input = new MemoryStream(Encoding.UTF8.GetBytes("a\nrest of the input\nmore"));
        var reader = new LineReader(input, bufferSize: 4);
        Assert.Equal("a"u8.ToArray(), await reader.ReadLineAsync(TestContext.Current.CancellationToken));

        await reader.DrainAsync();

        Assert.Equal(input.Length, input.Position);
    }
}
