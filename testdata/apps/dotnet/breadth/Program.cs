// A sample debuggee for eyedbg's end-to-end tests: breakpoint kinds,
// exceptions and attaching. Lines the tests find end in "// marker: NAME".
//
//   dotnet breadth.dll loop           a loop calling Orders.Price
//   dotnet breadth.dll throw          throws and catches an exception
//   dotnet breadth.dll crash          throws an exception nothing catches
//   dotnet breadth.dll wait MARKER    ticks until the file MARKER exists
//   dotnet breadth.dll hold MARKER    like wait, keeping 5000 objects and a contended lock (Hold.cs)
//   dotnet breadth.dll burn MARKER    a busy main thread and an allocating one until MARKER exists (Burn.cs)
//   dotnet breadth.dll display        a [DebuggerDisplay] value and a list (Display.cs)
var scenario = args.Length > 0 ? args[0] : "loop";

switch (scenario)
{
    case "loop":
        Loop();
        break;
    case "throw":
        Throw();
        break;
    case "crash":
        Orders.Fail("no orders: crash"); // marker: crash-call
        break;
    case "wait":
        Wait(args[1]);
        break;
    case "hold":
        Hold.Run(args[1]);
        break;
    case "burn":
        Burn.Run(args[1]);
        break;
    case "display":
        Display.Run();
        break;
    default:
        Console.Error.WriteLine($"unknown scenario {scenario}");
        return 2;
}

return 0;

static void Loop()
{
    var total = 0;
    for (var i = 1; i <= 5; i++)
    {
        total += Orders.Price(i); // marker: loop-body
        Console.WriteLine($"i={i} total={total}");
    }

    Console.WriteLine($"done total={total}"); // marker: loop-done
}

static void Throw()
{
    try
    {
        Orders.Fail("no orders: caught"); // marker: throw-call
    }
    catch (InvalidOperationException e)
    {
        Console.WriteLine($"caught {e.Message}");
    }

    Console.WriteLine("after throw");
}

static void Wait(string marker)
{
    Console.WriteLine($"ready {Environment.ProcessId}");
    var ticks = 0;
    while (!File.Exists(marker))
    {
        ticks++; // marker: wait-tick
        Thread.Sleep(20);
    }

    Console.WriteLine($"done after {ticks} ticks");
}
