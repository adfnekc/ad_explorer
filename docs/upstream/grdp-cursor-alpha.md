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
