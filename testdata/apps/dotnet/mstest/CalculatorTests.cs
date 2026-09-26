using Microsoft.VisualStudio.TestTools.UnitTesting;

// A self-hosting MSTest (Microsoft.Testing.Platform runner) test app for
// eyedbg's end-to-end tests (launched directly under the adapter, not
// attached to as VSTest is): Adds passes, Fails fails. Lines the tests find
// end in "// marker: NAME".
[TestClass]
public class CalculatorTests
{
    [TestMethod]
    public void Adds()
    {
        var a = 2;
        var b = 3;
        var sum = a + b;
        Assert.AreEqual(5, sum); // marker: adds-assert
    }

    [TestMethod]
    public void Fails()
    {
        var sum = 2 + 2;
        Assert.IsTrue(sum == 5); // marker: fails-assert
    }
}
