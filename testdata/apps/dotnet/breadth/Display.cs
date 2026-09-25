// The 'display' scenario: a [DebuggerDisplay] type and a list LINQ has run
// over (so System.Linq is loaded where it stops), for eval under SharpDbg.

using System.Diagnostics;

static class Display
{
    public static void Run()
    {
        var p = new Point(3, 4);
        var items = new List<int> { 4, 9, 16 };
        var sum = items.Sum();
        Console.WriteLine($"display {p.X},{p.Y} sum={sum}"); // marker: display
    }
}

[DebuggerDisplay("Point({X}, {Y})")]
sealed class Point(int x, int y)
{
    public int X { get; } = x;
    public int Y { get; } = y;
}
