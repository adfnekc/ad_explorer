# Every received PDU is hex encoded for a log line that is usually off

Ready to file at <https://github.com/adfnekc/grdp/issues>. English because it is
meant to be posted there.

## Summary

Copying a file from a remote runs at about 6 MB/s, and a CPU profile of the
client doing the reading says why: each received PDU is turned into a hexadecimal
string for a `glog.Trace` line, at five layers of the stack, whether or not
tracing is enabled. The argument is built before `glog` is called, so the cost is
paid on every packet at all times.

For a file transfer this is the dominant cost, and it scales with the payload,
which is why a larger read is slower rather than faster.

## Environment

- grdp `v0.8.6-0.20261010090221-f9f62edce731`
- Go 1.25.2, Linux client, `SecurityLevel = nla`
- Target: Windows 10 LTSC 19041, 1.0 ms round trip, no packet loss
- A 214 MiB file read from the remote in 1 MiB ranges

## The profile

Taken in the process doing the reading, so the whole path is in it.

```
Duration: 59.21s, Total samples = 49.15s (83.01%)

     flat  flat%   cum   cum%
   12.98s 26.41%     12.98s 26.41%  runtime.futex
    4.43s  9.01%      4.43s  9.01%  runtime.memmove
    4.01s  8.16%      4.01s  8.16%  encoding/hex.Encode
    3.03s  6.16%      3.03s  6.16%  internal/runtime/syscall.Syscall6
    1.89s  3.85%      1.89s  3.85%  runtime.memclrNoHeapPointers
```

and the callers of `hex.EncodeToString`:

```
     cum   cum%   context
    2.44s 30.58% |   cliprdr.(*CliprdrClient).Process
    1.46s 18.30% |   t125.(*MCSClient).recvData
    1.40s 17.54% |   x224.(*X224).recvData
    1.35s 16.92% |   tpkt.(*TPKT).recvData
    1.25s 15.66% |   sec.(*Client).recvData
```

`hex.EncodeToString` is 16.24% of all samples on its own, and of its own cost
28.45% is `runtime.slicebytetostring`.

## Why it is worse than that percentage suggests

One megabyte read from the remote passes through five of those layers and is
encoded five times, each time into a string twice the size of the data, each time
followed by a conversion that copies it again. That is ten megabytes of
allocation and copy for every megabyte transferred, and it is what the
`memmove`, `memclrNoHeapPointers` and GC entries are.

The tell is in the throughput: 64 KiB reads move 10.5 MB/s and 1 MiB reads move
6.2 MB/s with the same reader. A cost that grows with the buffer, not a cost per
round trip, is what that means.

## What was ruled out first

- **The remote's disk**: a second pass over the same file, fully cached, is 1.14
  times faster (6.70 against 5.89 MB/s), so nothing waits on storage.
- **Latency**: the round trip is 1.0 ms and a 1 MiB range takes 170 ms, so
  almost none of it is waiting.
- **The calling gateway**: its own chunk framing runs at 2-3.7 GB/s and the
  parse side does not copy.
- **The read size**: raising it made the transfer slower, not faster.

## Suggested fix

Do not build the string unless it will be used. Either a level check at the call
site:

```go
if glog.Level() <= glog.TRACE {
	glog.Trace("TPKT recvData", hex.EncodeToString(s))
}
```

or a `glog.Tracef`-style call that takes a function, or a helper that takes the
bytes and does the encoding itself:

```go
glog.TraceBytes("TPKT recvData", s)
```

The last is the smallest change at the call sites and cannot be got wrong by a
later caller. Whatever is chosen, the encoding of a payload belongs behind the
level check rather than in front of it.

## Acceptance

- A CPU profile of a file transfer shows no `encoding/hex` samples while tracing
  is off.
- Reading the same 214 MiB file in 1 MiB ranges gets faster, and reading it in
  larger ranges gets faster still rather than slower.

## Second item: pointer shapes without alpha

Fixed in `2000e769`: a 24bpp shape was having the AND mask's invert rule applied
to it, which replaced the body of the I beam with a black and white checkerboard.
Confirming only, no action needed.

## Third item: the wheel's rotation, for information

Not a defect report so much as a question about the encoding's intent.

Measured against the target, with the client sending what it was given: scrolling
down produced 8 notches over 51 wheel events, and scrolling up produced 481
notches over 381 events, while the remote scrolled the same distance each way.

`MouseWheel` puts the magnitude in `pointerFlags & WheelRotationMask` and sets
`PTRFLAGS_WHEEL_NEGATIVE` for a negative rotation. `WheelRotationMask` is
`0x01FF`, which includes the bit that `PTRFLAGS_WHEEL_NEGATIVE` occupies, so a
magnitude of 255 or more and a negative rotation share a bit. It is not clear
from the specification whether the field is meant to be read as a nine bit two's
complement value or as eight bits of magnitude beside a sign bit, and the two
differ for exactly the values this produces. Worth documenting which one it is,
because a server that reads it the other way will scroll a long way.
