# Pointer button events reach the server but have no effect, while pointer moves and keyboard input work

Ready to file at <https://github.com/adfnekc/grdp/issues>. Written in English
because it is meant to be posted there; the surrounding documentation in this
repository is not.

## Summary

Against one Windows 10 target, `RdpClient.MouseDown` and `RdpClient.MouseUp` have
no effect on the remote desktop at all — no clicks, no drags, no icon selection —
while `RdpClient.MouseMove` works (the remote pointer follows and hover tooltips
appear) and keyboard input works (`KeyDown`/`KeyUp` are delivered).

`mstsc` and FreeRDP both drive the same target normally, so the target accepts
pointer buttons.

grdp's fast-path pointer PDU is byte for byte what FreeRDP sends, so this does not
look like an encoding error. This issue is filed to ask whether the difference is
the missing initial synchronise event, or something else in the input path.

## Environment

- grdp `v0.8.4` (also observed on `v0.6.0`, `v0.8.0`, `v0.8.1`)
- Go 1.25.2, Linux client, `SecurityProtocol = nla` (the target requires `nla`)
- Target: Windows 10 Enterprise LTSC, Build 19041.vb_release.191206-1406
- Desktop 1024x768 (also tried at 1280x720 and 1600x900)
- Fast-path input enabled (the default); `Setting.NoFastPathInput = true` behaves
  identically

## Reproduction

A minimal client that connects, waits for the session to become ready, then does
the following, reproduces it on the target above:

```go
// The pointer moves: the remote pointer follows and a hover tooltip appears.
s.MouseMove(36, 36)
time.Sleep(1500 * time.Millisecond)

// A double click on a desktop icon: the icon never enters the selected state
// and no window opens, so the server never saw a button event.
s.MouseDown(0, 36, 36)
time.Sleep(70 * time.Millisecond)
s.MouseUp(0, 36, 36)
time.Sleep(70 * time.Millisecond)
s.MouseDown(0, 36, 36)
time.Sleep(70 * time.Millisecond)
s.MouseUp(0, 36, 36)
```

Observations, each compared against a screenshot of the same session:

- The desktop icon under the pointer is **not** selected. Windows selects a
  desktop icon on button **down**, so a delivered press would be visible even if
  the release were lost. Note that Windows 10 also highlights a desktop icon on
  *hover*, so a hover highlight must not be mistaken for a selection; the
  baseline screenshot must be taken with the pointer already resting on the icon.
- A left click on the Start button does not open the Start menu.
- A right click on the desktop does not open the context menu.
- `Ctrl+Esc` through `KeyDown`/`KeyUp` does not open the Start menu either, on
  this particular target, while ordinary keys are delivered.

Verified with `glog` at `TRACE`: the events are written to the socket. A left
click at (512, 320) produces

```
04 80 0a 20 0008 0002 4001    move   (pointerFlags 0x0800)
04 80 0a 20 0098 0002 4001    down   (pointerFlags 0x9800)
04 80 0a 20 0018 0002 4001    up     (pointerFlags 0x1800)
```

## What was ruled out

**The pointer flags.** Removing `PTRFLAGS_MOVE` from the press, from the release,
or from both makes no difference. Five combinations were tried, including
`PTRFLAGS_BUTTON1` moved to bit 0 and to bit 2; none had any effect.

**The event type.** Sending the press and release as `FASTPATH_INPUT_EVENT_MOUSEX`
(header byte `0x40` instead of `0x20`) makes the server answer
`sec ERROR_ALERT and ErrorCode: 7` and drop the connection.

**The slow path.** `Setting.NoFastPathInput = true`, which routes input through
`ClientInputEventPDU`, behaves identically. The `eventTime` and `messageType`
layout was compared against FreeRDP and matches.

**The encoding.** Compared line by line against FreeRDP (`libfreerdp/core/input.c`
and `libfreerdp/core/fastpath.c`), grdp's fast-path pointer PDU is byte for byte
identical:

| Field | FreeRDP | grdp |
|---|---|---|
| Header byte | `FASTPATH_INPUT_ACTION_FASTPATH \| (iNumEvents << 2)` | `FASTPATH_ACTION_FASTPATH \| ((numEvents & 0x0F) << 2)` |
| Length | `Stream_Write_UINT16_BE(s, 0x8000 \| length)` | `core.WriteUInt16BE(uint16(len(data)+3) \| 0x8000, buff)` |
| Event header | `Stream_Write_UINT8(s, eventFlags \| (eventCode << 5))` | `buff.WriteByte(eventCode << 5)` |
| Pointer payload | `Stream_Write_UINT16(s, flags); x; y` | `core.WriteUInt16LE(flags); x; y` |
| Button flags | `PTR_FLAGS_BUTTON1 0x1000`, `PTR_FLAGS_DOWN 0x8000` | `PTRFLAGS_BUTTON1 0x1000`, `PTRFLAGS_DOWN 0x8000` |

For a one-event pointer PDU both produce `04 80 0a 20 ...`. Packet capture cannot
help here: the target requires NLA, so everything after the CredSSP exchange is
inside TLS.

**A configuration or session problem on the target.** `mstsc` from a Windows
client drives the same host normally, including clicks on the same desktop icons
that grdp cannot select.

## The one structural difference found

Both `mstsc` and FreeRDP send an input preamble once the session is up. FreeRDP's
`input_send_synchronize_event` even says so in a comment — *"send a tab up like
mstsc.exe"* — and it puts **three events of two different types into one PDU**:

```c
/* libfreerdp/core/input.c */
eventFlags = FASTPATH_INPUT_KBDFLAGS_RELEASE | FASTPATH_INPUT_EVENT_SCANCODE << 5;
Stream_Write_UINT8(s, eventFlags); /* Key Release event (1 byte) */
Stream_Write_UINT8(s, 0x0f);       /* keyCode (1 byte) */
/* send the toggle key states */
eventFlags = (toggleStates & 0x1F) | FASTPATH_INPUT_EVENT_SYNC << 5;
Stream_Write_UINT8(s, eventFlags); /* toggle state (1 byte) */
/* send another tab up like mstsc.exe */
eventFlags = FASTPATH_INPUT_KBDFLAGS_RELEASE | FASTPATH_INPUT_EVENT_SCANCODE << 5;
Stream_Write_UINT8(s, eventFlags); /* Key Release event (1 byte) */
Stream_Write_UINT8(s, 0x0f);       /* keyCode (1 byte) */
return fastpath_send_multiple_input_pdu(rdp->fastpath, s, 3, sec_flags);
```

grdp never sends a synchronise event. `INPUT_EVENT_SYNC` and `SynchronizeEvent`
exist in `protocol/pdu` but have no call site anywhere in the client.

grdp also cannot express that PDU:

```go
func (c *Client) SendInputEvents(msgType uint16, events []InputEventsInterface)
```

Every event in the slice shares one `msgType`, and `sendFastPathInput` writes
`eventCode << 5` for each event using that same `eventCode`. A keyboard event and
a synchronise event therefore cannot travel in the same PDU.

**Being explicit about the uncertainty:** sending a synchronise event as its own
single-event PDU, 200 ms after the session became ready, did **not** make the
pointer buttons work on this target. It may need to be the multi-event PDU above,
it may need to be sent before any other input, or the synchronise event may be
unrelated. I could not test the multi-event form without patching this library,
which is why this is an issue rather than a pull request.

## Request

1. A public way to send the synchronise event, for example
   `func (c *RdpClient) SendSynchronize(toggleFlags uint32) error`.
2. A way to send a PDU whose events have different event codes — either let
   `sendFastPathInput` derive the code per event, or add an entry point that takes
   the events and works each one out.
3. Ideally `SetFastPathSender`'s wiring should send the preamble itself when the
   session becomes ready, so that callers do not have to know about it.

## Acceptance

- Against a Windows target, after the session is ready, sending
  tab-up + synchronise + tab-up and then double clicking a desktop icon opens a
  window.
- A single `MouseDown` puts a desktop icon into the selected state — a stronger
  highlight than hover, which must not be used as the signal on its own.
