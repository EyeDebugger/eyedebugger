// A sample debuggee for eyedbg's end-to-end tests: breakpoint kinds,
// exceptions and attaching. Lines the tests find end in "// marker: NAME".
//
//   dotnet breadth.dll loop           a loop calling Orders.Price
//   dotnet breadth.dll throw          throws and catches an exception
//   dotnet breadth.dll crash          throws an exception nothing catches
//   dotnet breadth.dll wait MARKER    ticks until the file MARKER exists
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
