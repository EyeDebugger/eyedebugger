using System.Runtime.CompilerServices;
using Confluent.Kafka;

Console.WriteLine($"consumer start tag={Program.Tag} build={Program.Build}");

var config = new ConsumerConfig
{
    BootstrapServers = "broker:9092",
    GroupId = "eyedbg-fixture",
    AutoOffsetReset = AutoOffsetReset.Earliest,
    SessionTimeoutMs = 6000,
    MaxPollIntervalMs = 10000,
};
using var consumer = new ConsumerBuilder<Ignore, string>(config).Build();
consumer.Subscribe("t");
for (var n = 1; ; n++)
{
    Program.Tick(n);
    ConsumeResult<Ignore, string>? result;
    try
    {
        result = consumer.Consume(TimeSpan.FromMilliseconds(500));
    }
    catch (ConsumeException e)
    {
        Console.WriteLine($"consume error: {e.Error.Reason}");
        Thread.Sleep(100);
        continue;
    }
    if (result is not null)
    {
        Console.WriteLine(Program.Handle(int.Parse(result.Message.Value)));
    }
}

partial class Program
{
    internal static readonly string Build =
#if DEBUG
        "debug";
#else
        "release";
#endif
    internal static string Tag = "v1";

    [MethodImpl(MethodImplOptions.NoInlining)]
    internal static void Tick(int n)
    {
        var ticks = n;
        var report = ticks % 10 == 0; // marker: tick
        if (report)
        {
            Console.WriteLine($"tick {ticks}");
        }
    }

    [MethodImpl(MethodImplOptions.NoInlining)]
    internal static string Handle(int item)
    {
        var doubled = item * 2;
        var line = $"handled item {item} -> {doubled}"; // marker: handle
        return line;
    }
}
