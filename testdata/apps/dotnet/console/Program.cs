var items = new List<int> { 7, 9 };
var total = 0;
for (var i = 1; i <= 3; i++)
{
    total += i;
    Console.WriteLine($"i={i} total={total}");
}
var name = "eyedbg";
Console.WriteLine($"done {name} {total}");
