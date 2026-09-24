/* A sample debuggee for eyedbg's C end-to-end tests (drivers/generic).
 * Lines the tests find end in "// marker: NAME".
 *
 *   ./app loop     a loop calling price(); prints "total N" (and "env
 *                  VALUE" if EYEDBG_SAMPLE is set)
 *   ./app wait     prints "pid N", then sleeps for about a minute
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static int price(int item) {
    return item * 10; // marker: price
}

static int compute(const int *items, int n) {
    int total = 0;
    int seen[8];

    for (int i = 0; i < n; i++) {
        total += price(items[i]); // marker: loop-body
        seen[i] = i;              // marker: append
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

    int items[] = {1, 2, 3, 4, 5};
    int total = compute(items, 5);
    printf("total %d\n", total); // marker: print

    const char *sample = getenv("EYEDBG_SAMPLE");
    if (sample != NULL) {
        printf("env %s\n", sample);
    }

    return 0;
}
