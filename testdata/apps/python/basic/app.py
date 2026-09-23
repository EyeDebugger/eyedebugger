# A sample debuggee for eyedbg's Python end-to-end tests (drivers/generic).
# Lines the tests find end in "# marker: NAME".
#
#   python app.py loop     a loop calling price(); prints "total 150"
#   python app.py raise    raises and catches a ValueError, then raises a
#                          RuntimeError nothing catches (exit 1)
#   python app.py child    runs a child Python process; prints "child ok"
#   python app.py wait     sleeps for a minute
import subprocess  # marker: entry
import sys
import time


def price(item):
    return item * 10  # marker: price


def compute(items):
    total = 0
    seen = []
    for i in range(len(items)):
        total += price(items[i])  # marker: loop-body
        seen.append(i)  # marker: append
    return total


def fail(message):
    raise RuntimeError(message)  # marker: fail


def main(mode):
    if mode == "loop":
        items = [1, 2, 3, 4, 5]
        total = compute(items)
        print("total", total)  # marker: print
    elif mode == "raise":
        try:
            raise ValueError("caught")  # marker: caught
        except ValueError:
            pass
        fail("uncaught")
    elif mode == "child":
        out = subprocess.run([sys.executable, "-c", "print('child ok')"], capture_output=True, text=True, check=True)
        print(out.stdout.strip())
    elif mode == "wait":
        for _ in range(600):
            time.sleep(0.1)


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "loop")
