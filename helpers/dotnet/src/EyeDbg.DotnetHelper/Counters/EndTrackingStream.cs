// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

namespace EyeDbg.DotnetHelper.Counters;

/// <summary>
/// A read-only view of a stream that records whether the stream itself ended (a read returned
/// 0 or failed with an I/O error). A parser that fails after that failed because its input was
/// cut off (the target went away), not because the input was malformed.
/// </summary>
internal sealed class EndTrackingStream : Stream
{
    private readonly Stream _inner;
    private volatile bool _ended;

    public EndTrackingStream(Stream inner)
    {
        _inner = inner;
    }

    /// <summary>Whether the underlying stream reached its end or failed.</summary>
    public bool Ended => _ended;

    public override bool CanRead => true;

    public override bool CanSeek => false;

    public override bool CanWrite => false;

    public override long Length => throw new NotSupportedException();

    public override long Position
    {
        get => throw new NotSupportedException();
        set => throw new NotSupportedException();
    }

    public override int Read(byte[] buffer, int offset, int count) => Read(buffer.AsSpan(offset, count));

    public override int Read(Span<byte> buffer)
    {
        try
        {
            var n = _inner.Read(buffer);
            if (n == 0 && buffer.Length > 0)
            {
                _ended = true;
            }

            return n;
        }
        catch (Exception e) when (e is IOException or ObjectDisposedException)
        {
            _ended = true;
            throw;
        }
    }

    public override void Flush()
    {
    }

    public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();

    public override void SetLength(long value) => throw new NotSupportedException();

    public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
}
