// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

namespace EyeDbg.DotnetHelper.Tracing;

/// <summary>
/// Copies an EventPipe stream into a file on a dedicated background thread until the stream
/// ends. The stream is always drained: a target whose events go unread blocks writing them
/// (seen on Windows), and then can't stop the session. Past the byte limit, or once writing
/// failed, it keeps reading (and, after a failure, drops what it reads) and signals
/// <see cref="Full"/> so the caller stops the session. It owns the file and closes it at the end.
/// It keeps write errors (the result's failure); a read error just ends the copy.
/// </summary>
internal sealed class StreamCopier
{
    private const int BufferSize = 81920;

    private readonly TaskCompletionSource _done = new(TaskCreationOptions.RunContinuationsAsynchronously);
    private readonly TaskCompletionSource _full = new(TaskCreationOptions.RunContinuationsAsynchronously);
    private long _bytes;
    private volatile Exception? _writeError;

    private StreamCopier()
    {
    }

    /// <summary>Completes when the stream has ended (or failed) and the file is closed.</summary>
    public Task Done => _done.Task;

    /// <summary>Completes when the file reached the byte limit or writing it failed.</summary>
    public Task Full => _full.Task;

    /// <summary>Bytes written to the file so far.</summary>
    public long Bytes => Interlocked.Read(ref _bytes);

    /// <summary>What made writing (or closing) the file fail; from then on nothing is written.</summary>
    public Exception? WriteError => _writeError;

    public static StreamCopier Start(Stream source, Stream destination, long limit)
    {
        var copier = new StreamCopier();
        var thread = new Thread(() => copier.Run(source, destination, limit))
        {
            IsBackground = true,
            Name = "eyedbg-trace-copy",
        };
        thread.Start();
        return copier;
    }

    private void Run(Stream source, Stream destination, long limit)
    {
        var buffer = new byte[BufferSize];
        try
        {
            while (true)
            {
                int n;
                try
                {
                    n = source.Read(buffer, 0, buffer.Length);
                }
                catch (Exception)
                {
                    // The connection closed (the target went away) or was closed under us after a
                    // stop that didn't finish: either way the stream is over. Which one it was is the
                    // collector's stop outcome; a file cut this way fails to parse at the summary.
                    // Anything else ends the copy too: an exception must not escape this thread.
                    break;
                }

                if (n == 0)
                {
                    break;
                }

                if (_writeError is null)
                {
                    try
                    {
                        destination.Write(buffer, 0, n);
                        Interlocked.Add(ref _bytes, n);
                    }
                    catch (Exception e) when (e is IOException or UnauthorizedAccessException or NotSupportedException or ObjectDisposedException)
                    {
                        _writeError = e;
                        _full.TrySetResult();
                    }
                }

                if (Bytes >= limit)
                {
                    _full.TrySetResult();
                }
            }
        }
        finally
        {
            try
            {
                // Flushes what the file still buffers: a full disk shows here too.
                destination.Dispose();
            }
            catch (IOException e)
            {
                _writeError ??= e;
            }

            _done.TrySetResult();
        }
    }
}
