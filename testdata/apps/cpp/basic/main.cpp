// A sample debuggee for eyedbg's C++ end-to-end tests (drivers/generic).
// Lines the tests find end in "// marker: NAME".
//
//   ./app loop     a loop calling price(); prints "total N" (and "env
//                  VALUE" if EYEDBG_SAMPLE is set)
//   ./app throw    throws an uncaught std::runtime_error
//   ./app wait     prints "pid N", then sleeps for about a minute
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <stdexcept>
#include <unistd.h>
#include <vector>

static int price(int item) {
    return item * 10; // marker: price
}

static int compute(const std::vector<int> &items) {
    int total = 0;
    std::vector<int> seen;
    int n = static_cast<int>(items.size());

    for (int i = 0; i < n; i++) {
        total += price(items[i]); // marker: loop-body
        seen.push_back(i);        // marker: append
    }

    return total;
}

int main(int argc, char **argv) {
    const char *mode = argc > 1 ? argv[1] : "loop";

    if (strcmp(mode, "wait") == 0) {
        printf("pid %d\n", getpid());
        fflush(stdout);

        for (int i = 0; i < 600; i++) {
            usleep(100000);
        }

        return 0;
    }

    if (strcmp(mode, "throw") == 0) {
        throw std::runtime_error("boom"); // marker: throw
    }

    std::vector<int> items{1, 2, 3, 4, 5};
    int total = compute(items);
    printf("total %d\n", total); // marker: print

    const char *sample = getenv("EYEDBG_SAMPLE");
    if (sample != nullptr) {
        printf("env %s\n", sample);
    }

    return 0;
}
