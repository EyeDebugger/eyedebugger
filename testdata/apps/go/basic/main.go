// A sample debuggee for eyedbg's Go end-to-end tests (drivers/generic).
// Lines the tests find end in "// marker: NAME".
//
//	go run . loop     a loop calling price(); prints "total N" (and "env
//	                   VALUE" if EYEDBG_SAMPLE is set)
//	go run . panic     panics; unhandled, the process exits 2
//	go run . wait      prints "pid N", then sleeps for about a minute
package main

import (
	"fmt"
	"os"
	"time"
)

func price(item int) int {
	return item * 10 // marker: price
}

func compute(items []int) int {
	total := 0
	seen := make([]int, 0, len(items))

	for i, item := range items {
		total += price(item)   // marker: loop-body
		seen = append(seen, i) // marker: append
	}

	return total
}

func loop() {
	items := []int{1, 2, 3, 4, 5}
	total := compute(items)
	fmt.Println("total", total) // marker: print

	if sample, ok := os.LookupEnv("EYEDBG_SAMPLE"); ok {
		fmt.Println("env", sample)
	}
}

func wait() {
	fmt.Println("pid", os.Getpid())

	for range 600 {
		time.Sleep(100 * time.Millisecond)
	}
}

func doPanic() {
	panic("boom") // marker: panic
}

func main() {
	mode := "loop"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "wait":
		wait()
	case "panic":
		doPanic()
	default:
		loop()
	}
}
