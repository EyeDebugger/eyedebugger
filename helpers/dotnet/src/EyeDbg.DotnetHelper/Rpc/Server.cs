// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Diagnostics.CodeAnalysis;
using System.Reflection;
using System.Runtime.InteropServices;
using System.Text.Json;

namespace EyeDbg.DotnetHelper.Rpc;

/// <summary>Runs one method call: its params, a way to send notifications, and cancellation.</summary>
internal delegate Task<Reply> MethodHandler(JsonElement parameters, INotifier notifier, CancellationToken cancellationToken);

/// <summary>
/// The helper's side of protocol 1 (helpers/dotnet/README.md): hello first, then one call.
/// While the call runs, stdin is read only to see it close; its end cancels the call and the
/// helper exits without answering.
/// </summary>
internal sealed class Server
{
    /// <summary>The protocol version this helper speaks.</summary>
    public const int ProtocolVersion = 1;

    /// <summary>Exit code: served (answers include error responses) or canceled by stdin's end.</summary>
    public const int ExitOk = 0;

    /// <summary>Exit code: stdout is gone.</summary>
    public const int ExitOutputFailed = 1;

    /// <summary>Exit code: the input broke the protocol.</summary>
    public const int ExitProtocolError = 2;

    private const int MaxFailureMessage = 500;

    private readonly LineReader _reader;
    private readonly MessageWriter _writer;
    private readonly IReadOnlyDictionary<string, MethodHandler> _methods;
    private bool _lastReadFailed;

    public Server(LineReader reader, MessageWriter writer, IReadOnlyDictionary<string, MethodHandler> methods)
    {
        _reader = reader;
        _writer = writer;
        _methods = methods;
    }

    /// <summary>Serves hello and one call; returns the process exit code.</summary>
    public async Task<int> RunAsync()
    {
        try
        {
            return await ServeAsync().ConfigureAwait(false);
        }
        catch (IOException)
        {
            return ExitOutputFailed;
        }
        catch (ObjectDisposedException)
        {
            return ExitOutputFailed;
        }
    }

    /// <summary>The hello result: protocol, eyedbg version the helper was built with, runtime.</summary>
    internal static HelloResult Hello() => new(
        ProtocolVersion,
        typeof(Server).Assembly.GetCustomAttribute<AssemblyInformationalVersionAttribute>()?.InformationalVersion ?? "unknown",
        RuntimeInformation.FrameworkDescription);

    /// <summary>HELPER_FAILED for an unexpected exception: its type and message, bounded.</summary>
    internal static HelperException Failure(Exception e)
    {
        var message = e.GetType().Name + ": " + e.Message;
        return new HelperException(ErrorCodes.HelperFailed, message.Length <= MaxFailureMessage ? message : message[..MaxFailureMessage]);
    }

    private async Task<int> ServeAsync()
    {
        var hello = await ReadRequestAsync().ConfigureAwait(false);
        if (hello is null)
        {
            return _lastReadFailed ? ExitProtocolError : ExitOk;
        }

        if (hello.Value.Method != "hello")
        {
            _writer.WriteError(hello.Value.Id, new HelperException(ErrorCodes.InvalidRequest, "the first request must be hello"));
            return ExitProtocolError;
        }

        try
        {
            ProtocolJson.ParseParams(hello.Value.Params, ProtocolJson.Default.HelloParams);
        }
        catch (HelperException e)
        {
            _writer.WriteError(hello.Value.Id, e);
            return ExitProtocolError;
        }

        _writer.WriteResult(hello.Value.Id, new Reply(Hello(), ProtocolJson.Default.HelloResult));

        var call = await ReadRequestAsync().ConfigureAwait(false);
        if (call is null)
        {
            return _lastReadFailed ? ExitProtocolError : ExitOk;
        }

        if (!_methods.TryGetValue(call.Value.Method, out var handler))
        {
            _writer.WriteError(call.Value.Id, new HelperException(ErrorCodes.UnknownMethod, "unknown method " + Quote(call.Value.Method)));
            return ExitOk;
        }

        return await RunCallAsync(call.Value, handler).ConfigureAwait(false);
    }

    [SuppressMessage("Reliability", "CA2000:Dispose objects before losing scope", Justification = "The stdin-drain continuation may cancel it at any time until the process exits; it has no timer to release.")]
    private async Task<int> RunCallAsync(Request call, MethodHandler handler)
    {
        var canceled = new CancellationTokenSource();
        _ = _reader.DrainAsync().ContinueWith(_ => canceled.Cancel(), CancellationToken.None, TaskContinuationOptions.None, TaskScheduler.Default);

        Reply reply;
        try
        {
            reply = await handler(call.Params, _writer, canceled.Token).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (canceled.IsCancellationRequested)
        {
            return ExitOk;
        }
        catch (HelperException e)
        {
            if (!canceled.IsCancellationRequested)
            {
                _writer.WriteError(call.Id, e);
            }

            return ExitOk;
        }
        catch (Exception e) when (e is not (IOException or ObjectDisposedException))
        {
            if (!canceled.IsCancellationRequested)
            {
                _writer.WriteError(call.Id, Failure(e));
            }

            return ExitOk;
        }

        if (!canceled.IsCancellationRequested)
        {
            _writer.WriteResult(call.Id, reply);
        }

        return ExitOk;
    }

    /// <summary>
    /// Reads and checks one request. Null at the end of the input, or after answering a request
    /// that broke the protocol (then _lastReadFailed is set).
    /// </summary>
    private async Task<Request?> ReadRequestAsync()
    {
        byte[]? line;
        try
        {
            line = await _reader.ReadLineAsync(CancellationToken.None).ConfigureAwait(false);
        }
        catch (LineTooLongException e)
        {
            _lastReadFailed = true;
            _writer.WriteError(null, new HelperException(ErrorCodes.InvalidRequest, e.Message));
            return null;
        }

        if (line is null)
        {
            return null;
        }

        var (request, id, error) = Request.Parse(line);
        if (error is not null)
        {
            _lastReadFailed = true;
            _writer.WriteError(id, new HelperException(ErrorCodes.InvalidRequest, error));
            return null;
        }

        return request;
    }

    private static string Quote(string s) => JsonSerializer.Serialize(s.Length <= 100 ? s : s[..100], ProtocolJson.Default.String);
}

/// <summary>A parsed request.</summary>
internal readonly record struct Request(long Id, string Method, JsonElement Params)
{
    /// <summary>
    /// Parses one line: a JSON-RPC 2.0 request with an integer id and a method. On error, the
    /// id if it could be read.
    /// </summary>
    public static (Request? Request, long? Id, string? Error) Parse(byte[] line)
    {
        try
        {
            return ParseDocument(line);
        }
        catch (JsonException)
        {
            return (null, null, "invalid JSON");
        }
        catch (InvalidOperationException)
        {
            // A string that isn't valid UTF-8 (the reader checks it on GetString).
            return (null, null, "invalid JSON");
        }
    }

    private static (Request? Request, long? Id, string? Error) ParseDocument(byte[] line)
    {
        using (var doc = JsonDocument.Parse(line))
        {
            var root = doc.RootElement;
            if (root.ValueKind != JsonValueKind.Object)
            {
                return (null, null, "a request must be a JSON object");
            }

            long? id = root.TryGetProperty("id", out var idElement) && idElement.ValueKind == JsonValueKind.Number && idElement.TryGetInt64(out var n)
                ? n
                : null;
            if (!root.TryGetProperty("jsonrpc", out var version) || version.ValueKind != JsonValueKind.String || version.GetString() != "2.0")
            {
                return (null, id, "jsonrpc must be \"2.0\"");
            }

            if (id is null)
            {
                return (null, null, "a request needs an integer id");
            }

            if (!root.TryGetProperty("method", out var method) || method.ValueKind != JsonValueKind.String || string.IsNullOrEmpty(method.GetString()))
            {
                return (null, id, "a request needs a method");
            }

            var parameters = root.TryGetProperty("params", out var p) ? p.Clone() : default;
            return (new Request(id.Value, method.GetString()!, parameters), id, null);
        }
    }
}
