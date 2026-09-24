// A sample debuggee for eyedbg's Rust end-to-end tests (drivers/generic).
// Lines the tests find end in "// marker: NAME".
//
//   ./app loop     a loop calling price(); prints "total N" (and "env
//                  VALUE" if EYEDBG_SAMPLE is set)
//   ./app wait     prints "pid N", then sleeps for about a minute
use std::env;
use std::process;
use std::thread;
use std::time::Duration;

fn price(item: i32) -> i32 {
    item * 10 // marker: price
}

fn compute(items: &[i32]) -> i32 {
    let mut total = 0;
    let mut seen = Vec::new();

    for i in 0..items.len() {
        total += price(items[i]); // marker: loop-body
        seen.push(i); // marker: append
    }

    total
}

fn main() {
    let mode = env::args().nth(1).unwrap_or_else(|| "loop".to_string());

    if mode == "wait" {
        println!("pid {}", process::id());

        for _ in 0..600 {
            thread::sleep(Duration::from_millis(100));
        }

        return;
    }

    let items = [1, 2, 3, 4, 5];
    let total = compute(&items);
    println!("total {}", total); // marker: print

    if let Ok(sample) = env::var("EYEDBG_SAMPLE") {
        println!("env {}", sample);
    }
}
