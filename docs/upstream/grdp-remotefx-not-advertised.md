# grdp never advertises RemoteFX, so a server cannot send it

Ready to file at <https://github.com/adfnekc/grdp/issues>. English because it is
meant to be posted there.

## Summary

`NewRemoteFXCapability()` exists in `protocol/pdu/caps.go` and builds a
conforming capability set, but nothing ever calls it: `protocol/pdu/pdu.go`
constructs the client's capability list with
`CAPSETTYPE_BITMAP_CODECS: NewNSCodecCapability()` and that is the only bitmap
codec a grdp client ever offers.

The consequence is not that RemoteFX decoding is missing — it is not. A client
decodes RemoteFX bitmaps already, through `surfaceBitmaps` in `client/client.go`
and `codec.Decompress`. The consequence is that the server is never told the
client can, so it sends plain bitmap updates instead, and everything that only
exists on the RemoteFX path is unreachable. `NewRemoteFXCapability` even says so
in its own comment: "This has not been exercised against a server that speaks
RemoteFX".

## Why it matters, beyond the capability itself

This is the part worth reading first, because it is why the omission is worth
fixing rather than noting.

RemoteFX brackets every frame it sends. A message is
`TS_RFX_FRAME_BEGIN` … `TS_RFX_REGION` … `TS_RFX_TILESET` … `TS_RFX_FRAME_END`
(`libfreerdp/codec/rfx.c` walks exactly that block list), so a client knows when
a frame is whole. A client that receives plain `FASTPATH_UPDATETYPE_BITMAP`
updates does not: the regions of one screen arrive from the server at different
times and nothing marks where one picture ends and the next begins. That is
visible as a screen assembled out of pieces from more than one moment, which is
what a person calls tearing, and no amount of client-side work removes it,
because the client cannot tell which pieces belong together.

With RemoteFX advertised, the frame boundary is in the data, and a client can
present a frame as a frame.

## Evidence

Capability list construction, `protocol/pdu/pdu.go`:

```go
CAPSETTYPE_BITMAP_CODECS: NewNSCodecCapability(),
```

`NewRemoteFXCapability`, defined and unused — a search for it across the module
matches its own definition and nothing else:

- `protocol/pdu/caps.go:815` — `func NewRemoteFXCapability() *BitmapCodecsCapability { return NewBitmapCodecsCapability(RDPCodecIDRemoteFX) }`
- no call sites

The two constructor helpers it would use are already variadic and already merge
codec entries, so advertising both needs no new machinery:

```go
func NewBitmapCodecsCapability(codecIDs ...uint8) *BitmapCodecsCapability {
	for _, id := range codecIDs {
		switch id {
		case RDPCodecIDNSCodec:
			…
		case RDPCodecIDRemoteFX:
			…
		}
	}
	…
}
```

The receive path is not the problem either — `client/client.go` decodes both
codecs already:

```go
// surfaceBitmaps converts a surface bits command, whose codec may be NSCodec or
// RemoteFX, into the same Bitmap shape so that consumers only ever see one
// shape.
pixels, err := codec.Decompress(b.CodecID, b.BitmapData, w, h, int(b.Bpp))
```

## The change

One line in `protocol/pdu/pdu.go`:

```go
CAPSETTYPE_BITMAP_CODECS: NewBitmapCodecsCapability(RDPCodecIDNSCodec, RDPCodecIDRemoteFX),
```

Whether it belongs behind a `Settings` flag is worth deciding: advertising a
codec the server then uses changes the shape of every bitmap update, so a client
that cannot decode RemoteFX must not advertise it. `NewRemoteFXCapability` being
unused suggests the intent was for something to opt in.

## Verified offline

The capability container and the GUID were compared field by field against the
FreeRDP 3.x reference, since the comment says this path has never been exercised
against a real server.

The GUID is byte-identical, `protocol/pdu/caps.go` `codecGUIDRemoteFX`:

```
FreeRDP: {0x76772F12, 0xBD72, 0x4463, {0xAF, 0xB3, 0xB7, 0x3C, 0x9C, 0x6F, 0x78, 0x86}}
grdp    : 12 2F 77 76 | 72 BD | 63 44 | AF B3 B7 3C 9C 6F 78 86
```

FreeRDP writes the first three GUID components little-endian
(`rdp_write_bitmap_codec_guid`), so `0x76772F12` becomes `12 2F 77 76`,
`0xBD72` becomes `72 BD` and `0x4463` becomes `63 44`. The last eight bytes are
copied as they are. grdp matches.

The 49-byte `TS_RFX_CLNT_CAPS_CONTAINER` built by `rfxClientCapabilityContainer`
matches `rdp_write_rfx_client_capability_container` field for field:

| field | FreeRDP | grdp |
| --- | --- | --- |
| codecPropertiesLength | 49 | 49 (`rfxClientCapsLength`) |
| length | 49 | 49 |
| captureFlags | `CARDP_CAPS_CAPTURE_NON_CAC` | `rfxCaptureNonCAC` |
| capsLength | 37 | 37 |
| blockType (caps) | `CBY_CAPS` | `rfxBlockCaps` |
| blockLen | 8 | 8 |
| numCapsets | 1 | 1 |
| blockType (capset) | `CBY_CAPSET` | `rfxBlockCapset` |
| blockLen | 29 | 29 |
| codecId | 0x01 | 0x01 |
| capsetType | `CLY_CAPSET` | `rfxCapsetType` |
| numIcaps | 2 | 2 |
| icapLen | 8 | 8 |
| ICAP RLGR1 | version, tileSize, flags, colConv, xform, entropy | same six |
| ICAP RLGR3 | version, tileSize, flags, colConv, xform, entropy | same six |

`flags` is the only field a reader could question: FreeRDP writes
`settings->RemoteFxCodecMode`, whose default is `0x00`, and grdp writes `0x00`.
FreeRDP's `+rfx-mode` command line option sets `0x02` to request video mode.
`0x00` is image mode and is what a client should send unless it has a reason.

## What still needs a real server

1. That a Windows server actually switches to RemoteFX when offered — which is
   the whole point of the change, and which cannot be checked offline.
2. That the RFX message arrives whole in one payload, so that one call to
   `DecodeRFX` is one frame. A frame split across several
   `TS_BITMAP_DATA_EX` structures would need reassembly, and `surfaceBitmaps`
   currently decodes each bitmap independently.
3. `codec.RFXMode` is a package variable defaulting to `RLGR1` and is never
   updated from the negotiated entropy coder. Both ICAPs are advertised, so the
   server may pick `RLGR3`, and decoding RLGR3 data with RLGR1 will produce
   garbage rather than an error.
4. What the updated `Update.Regions` a session sees look like, for a consumer
   that treats a region as part of a redraw.

Point 3 in particular looks like a real defect in its own right, and it is
reachable the moment RemoteFX is switched on.

## Measured afterwards: advertising alone changes nothing here

This was tried against a Windows 10 LTSC 19041 target after the report was first
written, and the result argues that the missing call site is not what stands
between a client and RemoteFX frames.

The server does offer it. Its Demand Active PDU carries the RemoteFX GUID in the
Bitmap Codecs capability with `codecId` 0x00, which is the server-mode form
meaning the client picks the id. The client sent back the matching entry:

```
122f777672bd6344 afb3b73c9c6f7886 03 3100 31000000 01000000 25000000 c0cb 08000000 0100 c1cb 1d000000 01 c0cf
GUID                             id len=49 length=49 capFlags=1 capsLen=37 ...
```

That is `codecId` 0x0003, a 49-byte container and the block list, which is what
FreeRDP writes. Both sides offered RemoteFX, and the server then sent plain
updates anyway: 3369 fast-path `FASTPATH_UPDATETYPE_BITMAP`, 6 cached, 4 pointer,
no surface commands, no orders, and `codec.Decompress` never called once.

So on this target the server offers RemoteFX and does not use it. FreeRDP would
be in the same position, because its decision to use the codec
(`settings->RemoteFxCodec = guidRemoteFx` when the server's set is parsed) is
also driven by the server's offer rather than by anything the client can send.

RemoteFX was deprecated and removed from Windows 10 1809 and Server 2019 onward,
which is the likely reason a capability is still advertised by an operating
system that no longer encodes with it. That part is an inference; what was
measured is the list of update types above.

The conclusion for a client is that advertising RemoteFX is worth doing and is
not sufficient, and that a frame boundary cannot be obtained this way on a
target that has removed it. The EGFX path is the one that has them now.
