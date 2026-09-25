// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

using System.Buffers;
using System.Text.Json;
using System.Text.Json.Serialization.Metadata;

namespace EyeDbg.DotnetHelper.Rpc;

/// <summary>
/// Writes JSON-RPC 2.0 messages to eyedbg, one UTF-8 JSON document per '\n'-ended line, each
/// shorter than <see cref="MaxMessageSize"/> (eyedbg's internal/api framing). Safe for
/// concurrent use: a lock keeps each line whole.
/// </summary>
internal sealed class MessageWriter : INotifier
{
    /// <summary>The bound on one line, its '\n' included (internal/api.MaxMessageSize).</summary>
    public const int MaxMessageSize = 1 << 20;

    /// <summary>The JSON-RPC error code of every error; the stable code travels in data.</summary>
    public const int ApplicationErrorCode = -32000;

    private readonly Stream _output;
    private readonly object _lock = new();

    public MessageWriter(Stream output)
    {
        _output = output;
    }

    /// <summary>Writes a success response; a result too large to send becomes HELPER_FAILED.</summary>
    public void WriteResult(long id, Reply reply)
    {
        var line = Encode(w =>
        {
            w.WriteNumber("id", id);
            w.WritePropertyName("result");
            JsonSerializer.Serialize(w, reply.Value, reply.TypeInfo);
        });
        if (line is null)
        {
            WriteError(id, new HelperException(ErrorCodes.HelperFailed, "result too large for one message"));
            return;
        }

        Send(line);
    }

    /// <summary>Writes an error response; a null id when the request's id isn't known.</summary>
    public void WriteError(long? id, HelperException error)
    {
        var data = new ErrorData(error.Code, Truncate(error.Message), error.Hint is null ? null : Truncate(error.Hint));
        var line = Encode(w =>
        {
            if (id is { } value)
            {
                w.WriteNumber("id", value);
            }
            else
            {
                w.WriteNull("id");
            }

            w.WriteStartObject("error");
            w.WriteNumber("code", ApplicationErrorCode);
            w.WriteString("message", data.Message);
            w.WritePropertyName("data");
            JsonSerializer.Serialize(w, data, ProtocolJson.Default.ErrorData);
            w.WriteEndObject();
        });

        // Two truncated strings of at most 4 KiB each always fit.
        Send(line!);
    }

    /// <inheritdoc/>
    public void Notify<T>(string method, T parameters, JsonTypeInfo<T> typeInfo)
    {
        var line = Encode(w =>
        {
            w.WriteString("method", method);
            w.WritePropertyName("params");
            JsonSerializer.Serialize(w, parameters, typeInfo);
        }) ?? throw new HelperException(ErrorCodes.HelperFailed, "notification " + method + " too large for one message");

        Send(line);
    }

    /// <summary>
    /// Encodes one message with its '\n'; null if it would reach <see cref="MaxMessageSize"/>.
    /// </summary>
    internal static byte[]? Encode(Action<Utf8JsonWriter> body)
    {
        var buffer = new ArrayBufferWriter<byte>();
        using (var writer = new Utf8JsonWriter(buffer))
        {
            writer.WriteStartObject();
            writer.WriteString("jsonrpc", "2.0");
            body(writer);
            writer.WriteEndObject();
        }

        if (buffer.WrittenCount >= MaxMessageSize)
        {
            return null;
        }

        buffer.Write("\n"u8);
        return buffer.WrittenSpan.ToArray();
    }

    private static string Truncate(string s) => s.Length <= 4096 ? s : s[..4096];

    private void Send(byte[] line)
    {
        lock (_lock)
        {
            _output.Write(line);
            _output.Flush();
        }
    }
}

/// <summary>Sends notifications while a call runs.</summary>
internal interface INotifier
{
    /// <summary>Sends a notification; throws if it can't be sent.</summary>
    void Notify<T>(string method, T parameters, JsonTypeInfo<T> typeInfo);
}
