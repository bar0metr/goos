// Package lock is the mutual exclusion lock of native userspace libraries
// (docs/development/native-concurrency-audit.md).
//
// A Mutex protects one object: a session, a pool, an output stream. It is
// never a lock for a whole library. The zero value is unlocked, and a Mutex
// must not be copied after first use.
//
// Two implementations share the API. A host build and a program built with
// goroutines (tag goos_goroutines) use sync.Mutex: a contended Lock parks the
// goroutine and the scheduler runs another one on the same thread. A native
// program without goroutines, which may still run several threads started
// with thread.Start, uses a compare-and-swap word: a contended Lock retries a
// few dozen times and then yields its processor with sys.ThreadYield.
//
// Hold a Mutex for a bounded amount of work. Do not hold it across a call
// that waits for another goroutine or thread of the same process to make
// progress: under the spinning implementation a waiter burns its time slice,
// and under either one the holder can deadlock against the thread it waits
// for. The exception is an object that is itself a serialized resource, such
// as a service session with one request outstanding or an output stream that
// writes one record at a time; its documentation says that the lock is held
// across the wait.
package lock
