public static class Orders
{
    public static int Price(int i)
    {
        return i * 10; // marker: price
    }

    public static void Fail(string message)
    {
        throw new InvalidOperationException(message); // marker: fail
    }
}
