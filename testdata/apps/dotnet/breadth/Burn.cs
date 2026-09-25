// The 'burn' scenario: a busy main thread and an allocating one, for 'eyedbg dotnet trace'.

using System.Diagnostics;
using System.Runtime.CompilerServices;

static class Burn
{
    static readonly byte[]?[] Ring = new byte[]?[256];

    public static void Run(string marker)
    {
        var churn = new Thread(Churn) { IsBackground = true, Name = "churn" };
        churn.Start();
        Console.WriteLine($"ready {Environment.ProcessId}");

        // The marker is checked every 50 ms only: checked on every pass, File.Exists outranks Spin.
        long total = 0;
        var check = Stopwatch.StartNew();
        while (true)
        {
            total += Spin(100_000); // marker: burn-tick
            if (check.ElapsedMilliseconds >= 50)
            {
                if (File.Exists(marker))
                {
                    break;
                }

                check.Restart();
            }
        }

        Console.WriteLine($"done {total & 0xff}");
    }

    [MethodImpl(MethodImplOptions.NoInlining)]
    public static long Spin(int n)
    {
        long s = 0;
        for (var i = 0; i < n; i++)
        {
            s = unchecked(s * 31 + (i ^ (s >> 7)));
        }

        return s;
    }

    // Garbage: a ring of 512-byte arrays, with a pause every 65,536 so the main thread keeps a core.
    static void Churn()
    {
        var i = 0;
        while (true)
        {
            Ring[i++ & 255] = new byte[512];
            if ((i & 0xFFFF) == 0)
            {
                Thread.Sleep(1);
            }
        }
    }
}
