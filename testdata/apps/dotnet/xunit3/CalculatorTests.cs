using Xunit;

// A self-hosting xUnit v3 test app for eyedbg's end-to-end tests (launched
// directly under the adapter, not attached to as VSTest is): Adds passes,
// Fails fails. Lines the tests find end in "// marker: NAME".
public class CalculatorTests
{
    [Fact]
    public void Adds()
    {
        var a = 2;
        var b = 3;
        var sum = a + b;
        Assert.Equal(5, sum); // marker: adds-assert
    }

    [Fact]
    public void Fails()
    {
        var sum = 2 + 2;
        Assert.True(sum == 5); // marker: fails-assert
    }
}
