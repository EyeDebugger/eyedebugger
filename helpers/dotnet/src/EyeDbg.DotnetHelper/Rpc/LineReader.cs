// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Buffers;

namespace EyeDbg.DotnetHelper.Rpc;

/// <summary>
/// Reads newline-framed messages from a stream, as eyedbg's internal/api Conn frames them:
/// a line ends at '\n', one '\r' before it is dropped, and a line of
/// <see cref="MessageWriter.MaxMessageSize"/> bytes or more (with its '\n') is an error rather
/// than cut. At the end of the stream, a last line without '\n' is returned as it is.
/// Not safe for concurrent use.
/// </summary>
internal sealed class LineReader
{
    /// <summary>The longest line, without its '\n'.</summary>
    public const int MaxLineBytes = MessageWriter.MaxMessageSize - 1;

    private readonly Stream _input;
    private readonly byte[] _buffer;
    private int _start;
    private int _end;

    public LineReader(Stream input, int bufferSize = 64 * 1024)
    {
        _input = input;
        _buffer = new byte[bufferSize];
    }

    /// <summary>
    /// Returns the next line without its line ending, or null at a clean end of the stream.
    /// </summary>
    /// <exception cref="LineTooLongException">The line is too long.</exception>
    public async Task<byte[]?> ReadLineAsync(CancellationToken cancellationToken)
    {
        var line = new ArrayBufferWriter<byte>();

        while (true)
        {
            if (_start < _end)
            {
                var available = _buffer.AsSpan(_start, _end - _start);
                var newline = available.IndexOf((byte)'\n');
                var take = newline >= 0 ? newline : available.Length;
                if (line.WrittenCount + take > MaxLineBytes)
                {
                    throw new LineTooLongException();
                }

                line.Write(available[..take]);
                if (newline >= 0)
                {
                    _start += newline + 1;
                    return TrimCarriageReturn(line.WrittenSpan);
                }

                _start = _end;
            }

            var n = await _input.ReadAsync(_buffer, cancellationToken).ConfigureAwait(false);
            if (n == 0)
            {
                return line.WrittenCount == 0 ? null : TrimCarriageReturn(line.WrittenSpan);
            }

            _start = 0;
            _end = n;
        }
    }

    /// <summary>
    /// Discards everything up to the end of the stream; completes at the end of the stream or
    /// when reading fails.
    /// </summary>
    public async Task DrainAsync()
    {
        _start = _end;
        try
        {
            while (await _input.ReadAsync(_buffer).ConfigureAwait(false) > 0)
            {
            }
        }
        catch (IOException)
        {
        }
        catch (ObjectDisposedException)
        {
        }
    }

    private static byte[] TrimCarriageReturn(ReadOnlySpan<byte> line) =>
        (line.Length > 0 && line[^1] == (byte)'\r' ? line[..^1] : line).ToArray();
}

/// <summary>A line longer than <see cref="LineReader.MaxLineBytes"/>.</summary>
internal sealed class LineTooLongException : Exception
{
    public LineTooLongException()
        : base("message line of 1 MiB or more")
    {
    }
}
