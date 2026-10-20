# Show HN

Post as `Show HN: <title>` with the URL `https://github.com/adfnekc/ad_explorer`.
The text below is the **first comment**, which is where the detail belongs: the
link itself should be the repository, not a landing page.

## Title (79 characters)

```
Show HN: A Web RDP gateway in one Go binary, with no credentials on the server
```

Alternatives, in case that one reads as too much of a claim:

```
Show HN: Self-hosted Web gateway for RDP sessions, in one Go binary
Show HN: A browser RDP client where the passwords never leave the browser
```

## First comment

> I wanted RDP in a browser without a control plane. Everything that does this
> well is a product: an agent, a database, an account, a place where your
> credentials end up. This is one static Go binary, a WebSocket per session, and
> a frontend with no build step.
>
> The shape of it: the browser holds the host list and the host passwords in
> `localStorage`. It sends a password once, in the request that opens a session,
> and the gateway keeps it for that session and writes it nowhere. There is no
> server-side session store, so a restart logs everyone out and surprises
> nobody. The console password (the one that guards the gateway itself) comes
> from a file with mode 0600.
>
> What it does: the desktop picture, pointer shapes, keyboard and mouse including
> IME composition, clipboard both ways, file transfer both ways (ranges are
> fetched from the browser on demand, so a 4 GB file never has to fit in memory
> anywhere), live desktop resizing, and full screen. There is a `-demo` flag that
> draws a synthetic desktop answering real input, so you can try it without a
> Windows machine.
>
> Two things I got wrong and measured, because the second one is the interesting
> part:
>
> **PNG was the wrong codec, and I had a benchmark saying otherwise.** On a
> synthetic desktop of flat colour and text, PNG is both sharper and smaller than
> JPEG. On a real desktop with a wallpaper it cost 200 KB per redraw and 10 MB/s
> at fifty redraws a second — thirty times the estimate — and the browser spent
> 13.7 ms a redraw decoding it against a 28.5 ms gap, worst case 642 ms. The
> session was unusable and the benchmark was measuring a desktop nobody has.
>
> **Torn frames were a presentation-order bug, and every fix that sounded right
> made it worse.** Waiting for the display's next refresh, or for the stream to
> go quiet, both let the *next* redraw land in the surface being shown. The only
> instant the surface is whole is when the redraw completes, so that is when it
> is shown. I reproduced it offline first — 0 torn frames in four runs of ten
> rounds afterwards — which is the only reason I trust it.
>
> It still tears occasionally on a fast-moving screen. I would rather say so than
> claim otherwise: it is the ordering of presentation against composition, and it
> is the next thing I am working on.
>
> Go and GPL-3.0, because it links grdp, which is (also mine).
>
> Questions I expect and will answer honestly: why not Guacamole (it is a good
> answer; this is a single binary and a much smaller surface), why not EGFX/H.264
> (it carries frame boundaries properly and would fix the tearing, but it needs
> cgo and I want the static binary), and whether `localStorage` for passwords is
> acceptable (it is the trust boundary I chose, and the defence is a strict CSP —
> tell me if you think that is wrong).

## Before posting

- [ ] The repository is public and the README renders with its screenshot.
- [ ] `-demo` runs from a clean clone in under a minute on Linux.
- [ ] Someone else's machine has tried it (a second laptop, a VM, a container).
- [ ] You can sit with the thread for the first two hours. HN rewards replies and
      punishes drive-by posting.
- [ ] No marketing words: "seamless", "powerful", "revolutionary" do not appear.

## Timing

Tuesday to Thursday, 08:00–10:00 US Eastern (20:00–22:00 China Standard Time) is
when the front page has the most readers and the fewest submissions per minute.
One post, no reposting the same day; if it dies, wait at least a week.
