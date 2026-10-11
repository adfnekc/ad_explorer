# Every receive step is called through reflection

Ready to file at <https://github.com/adfnekc/grdp/issues>. English because it is
meant to be posted there.

## Summary

A CPU profile of a client reading a large file from a remote puts
`reflect.Value.call` at 26.84% of samples, reached from every layer of the
receive path — `t125.MCSClient.recvData`, `plugin.(*Channels).process`,
`sec.(*Client).recvData`, `x224.(*X224).recvData` — on every PDU. A quarter of
that is `runtime.newstack`, because a reflective call forces the goroutine's
stack to be copied.

## Environment

- grdp `v0.8.7-0.20261010102933-522cd640828a`
- Go 1.25.2, Linux client
- Target: Windows 10 LTSC 19041, 1.0 ms round trip, no packet loss
- A 214 MiB file read in 1 MiB ranges

## The profile

```
Duration: 37.10s, Total samples = 16.99s (45.80%)

     flat  flat%   cum   cum%
    5410ms 31.84%     5410ms 31.84%  runtime.futex
    1930ms 11.36%     1930ms 11.36%  internal/runtime/syscall.Syscall6
     570ms  3.35%      570ms  3.35%  runtime.memclrNoHeapPointers
     370ms  2.18%      440ms  2.59%  runtime.step
     330ms  1.94%      330ms  1.94%  runtime.write1
     270ms  1.59%      270ms  1.59%  runtime.memmove
```

and what calls the reflection:

```
      flat  flat%   cum   cum%
     0.21s  1.24%     4.56s 26.84%  reflect.Value.call
                                             1.19s 26.10% |   runtime.newstack
                                                1s 21.93% |   t125.(*MCSClient).recvData
                                             0.69s 15.13% |   plugin.(*Channels).process
                                             0.64s 14.04% |   sec.(*Client).recvData
                                             0.54s 11.84% |   x224.(*X224).recvData
```

## Why it costs more than the flat number

`reflect.Value.Call` does more than an indirect call. It packs arguments into a
`[]reflect.Value`, checks each one against the function's type, and then arranges
for the call to be made, which `runtime.newstack` at 26.10% of its cost shows can
mean growing the stack — a copy of the whole goroutine stack when it happens. The
`runtime.pcvalue`, `runtime.step`, `runtime.findfunc` and
`runtime.(*stkframe).getStackMap` entries elsewhere in the profile are the same
activity: unwinding frames to describe the call.

This is per PDU, at every layer, and unlike a data copy it does not shrink when
the payload is small: it is a fixed cost multiplied by the packet rate, so it is
worst for exactly the small-packet traffic an RDP session has most of.

## Suggested fix

Keep the layer-to-layer call as an ordinary function call. Where a handler has to
be stored and invoked, store a typed function value rather than a
`reflect.Value`:

```go
type pduHandler func([]byte) error

var handlers = map[uint16]pduHandler{ ... }

if h, ok := handlers[type]; ok {
	return h(data)
}
```

If the reflection exists to support arbitrary signatures, an interface with one
method per call shape would remove it without losing the flexibility. If it is
there for something else that is not obvious from the profile, that would be
worth a comment, because a reader looking for the cost of a layer will not think
to look for reflection.

## Acceptance

- A CPU profile of a file transfer shows `reflect.Value.call` gone, or reduced to
  a few samples at startup rather than one per PDU.
- The same 214 MiB file read in 1 MiB ranges takes less time, and the improvement
  is larger for a workload with many small PDUs than for one large transfer.

## How this was found

It showed up while looking for something else. The first profile, taken before
`75ba270`, had `encoding/hex.Encode` at 16.24% cumulative with the same five
layers in its caller list, and that fix has since made the transfer faster
(5.89 to 13.50 MB/s on the same file, range size and host). Removing it left the
next per-PDU cost visible, which is this.
