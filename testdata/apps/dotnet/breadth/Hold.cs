// The 'hold' scenario: memory kept alive by a static field and a contended lock, for
// 'eyedbg dotnet heap' and 'eyedbg dotnet threads'.

static class Hold
{
    public static void Run(string marker)
    {
        for (var i = 0; i < 5000; i++)
        {
            Holder.Keep.Add(new Retained { A = i, B = i, C = i, D = i });
        }

        var gate = new object();
        using var owned = new ManualResetEventSlim();
        var owner = new Thread(() => Own(gate, owned, marker)) { Name = "holder" };
        owner.Start();
        owned.Wait();

        // Blocked in Monitor.Enter: the runtime gives the gate a sync block with an owner and a waiter.
        var waiter = new Thread(() => Enter(gate)) { Name = "waiter" };
        waiter.Start();
        while ((waiter.ThreadState & ThreadState.WaitSleepJoin) == 0)
        {
            Thread.Sleep(1);
        }

        Console.WriteLine($"ready {Environment.ProcessId}");
        var ticks = 0;
        while (!File.Exists(marker))
        {
            ticks++; // marker: hold-tick
            Thread.Sleep(20);
        }

        owner.Join();
        waiter.Join();
        Console.WriteLine($"done after {ticks} ticks, {Holder.Keep.Count} kept");
    }

    static void Own(object gate, ManualResetEventSlim owned, string marker)
    {
        lock (gate)
        {
            owned.Set();
            while (!File.Exists(marker))
            {
                Thread.Sleep(20);
            }
        }
    }

    static void Enter(object gate)
    {
        lock (gate)
        {
        }
    }
}

static class Holder
{
    public static readonly List<Retained> Keep = new();
}

sealed class Retained
{
    public long A, B, C, D;
}
