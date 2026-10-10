# Colour pointer shapes are drawn as a black and white checkerboard

Ready to file at <https://github.com/adfnekc/grdp/issues>, alongside
`grdp-mouse-buttons.md`. Written in English because it is meant to be posted
there.

## Summary

A 32bpp pointer shape comes out with its white pixels replaced by a black and
white checkerboard. On Windows 10 the arrow and the I beam are both 32bpp shapes
whose body is white and whose AND mask is mostly set, so almost the whole pointer
is drawn as a dither pattern instead of a solid shape. The I beam in particular
is unrecognisable.

## Measured on the failing target

`OnCursor` reports these shapes on Windows 10 LTSC 19041, 1024x768. Only the
I beam matters here:

| shape | XorBpp | size | hotspot | xor bytes | and bytes |
|---|---|---|---|---|---|
| arrow | 4 | 41x39 | (3,3) | 6396 | 234 |
| **I beam** | **3** | **32x32** | **(8,9)** | **3072** | **128** |
| pointer over a link | 4 | 32x32 | (0,0) | 4096 | 128 |

Both masks are the full size the depth and dimensions require, so nothing is
truncated. The I beam is the **24bpp** case, not the 32bpp one: it has no alpha
channel, which is what makes `colourCursorPixel` decide its pixels at all.

Hotspot (8,9) on a 32x32 shape is the middle of the left hand side, which is
where an I beam's bar sits, so the shape really is the I beam and it is the
decoding of it that is wrong.

The client receives a 32x32 PNG with 40 opaque pixels: four small squares. An
I beam is a vertical bar with a cap at each end, which is far more than that.

## Environment

- grdp `v0.8.5-0.20261009062140-c4137bbd983a` (also seen on `v0.8.4`)
- Go 1.25.2, Linux client, `SecurityProtocol = nla`
- Target: Windows 10 Enterprise LTSC, Build 19041, 1024x768
- `OnCursor` reports the shapes as `XorBpp = 4`, 41x39 (arrow) and 32x32 (I beam),
  so the colour path is the one being taken

## The code

`client/cursor.go`, `colourCursorPixel`:

```go
func colourCursorPixel(px [4]byte, andBit byte, xorBpp int, x, y int) [4]byte {
	black := px[0] == 0 && px[1] == 0 && px[2] == 0
	white := px[0] == 0xff && px[1] == 0xff && px[2] == 0xff
	if andBit == 1 {
		switch {
		case xorBpp > 3 && black:
			return [4]byte{}
		case white:
			// The inverse of the background, which is not knowable here.
			return invertedCursorPixel(x, y)
		case xorBpp <= 3:
			return [4]byte{} // 24bpp: a colour other than white is a hole
		}
	}
	...
}
```

`invertedCursorPixel` returns white for `(x+y)&1 == 0` and black otherwise, which
is where the checkerboard comes from.

## Why it is wrong for 32bpp

A 32bpp pointer carries an alpha channel, and on Windows 10 that channel is what
describes the shape: the body is opaque white, the outside is transparent, and
the AND mask is set almost everywhere. The AND mask's "white means invert the
background" rule belongs to the two colour and 24bpp forms, where there is no
alpha to say what the shape is and the masks are all there is.

Applying it to a 32bpp shape discards the alpha that was already correct and
replaces the body of the pointer with a pattern that exists only because XOR
cannot be written into a bitmap.

## Reproduction

```go
client.OnCursor(func(cur *client.Cursor) {
	// cur.XorBpp == 4, cur.Image is 41x39 or 32x32
	png.Encode(f, cur.Image)
})
```

Hover the desktop and then the taskbar's search box. The arrow and the I beam
both come out with a checkerboard body. Attached below is what the three shapes
look like on a magenta background, where magenta is transparency.

## Suggested fix

For a shape that has alpha, use it and do not consult the AND mask:

```go
if xorBpp > 3 {
	// 32bpp states the shape in the alpha channel; the AND mask is set across
	// the body and applying its invert rule replaces the body with a dither.
	px[3] = alpha   // as decoded
	return px
}
```

The AND mask rule stays for `XorBpp <= 3`, which has no alpha and is where it
was needed in the first place.

## Acceptance

- A 32bpp pointer shape encodes to a PNG whose body is a flat colour rather than
  a checkerboard.
- The Windows 10 arrow is solid white with a black outline, and the I beam over a
  text field is an I beam.

## Caller-side note

This cannot be worked around where the shape is consumed: the alpha is already
gone by the time `Cursor.Image` is handed out. `XorBpp` is exposed on the cursor
so a caller can at least tell the two cases apart.
