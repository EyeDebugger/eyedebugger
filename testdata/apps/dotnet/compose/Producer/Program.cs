using System.Runtime.CompilerServices;
using Confluent.Kafka;

// Runs before anything else, so a launch under the debugger can stop here.
var startupEnv = $"{Environment.GetEnvironmentVariable("EYEDBG_FIXTURE_ENV")}/{Environment.GetEnvironmentVariable("EYEDBG_FIXTURE_ENVFILE")}"; // marker: startup
Console.WriteLine($"producer start tag={Program.Tag} build={Program.Build} env={startupEnv}");

var config = new ProducerConfig { BootstrapServers = "broker:9092", MessageTimeoutMs = 2000 };
using var producer = new ProducerBuilder<Null, string>(config).Build();
var alive = Path.Combine(Path.GetTempPath(), "alive");
for (var n = 1; ; n++)
{
    File.WriteAllText(alive, n.ToString()); // the compose healthcheck reads this file's age
    Program.Produce(producer, n);
    await Task.Delay(500);
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
    internal static void Produce(IProducer<Null, string> producer, int n)
    {
        var item = n * 10;
        var text = item.ToString(); // marker: produce
        producer.Produce("t", new Message<Null, string> { Value = text }, report =>
        {
            if (report.Error.IsError)
            {
                Console.WriteLine($"produce error: {report.Error.Reason}");
            }
        });
        Console.WriteLine($"produced item {item}");
    }
}
