# Product Hunt

Product Hunt wants a listing rather than a link: a name, a one-line pitch, a
description, up to three topics, a gallery, and a first comment. Everything below
is meant to be pasted in, except the gallery, which is the pictures in
`docs/screenshots/`.

## Name

```
AD_EXPLORER
```

## Tagline (58 characters)

```
RDP in the browser from one binary, no credentials on the server
```

Two alternatives, if that one feels long:

```
A self-hosted Web gateway for RDP, in a single Go binary
Browser RDP without an agent, a database or a control plane
```

## Description

```
AD_EXPLORER puts a Windows desktop in a browser tab without putting anything
between you and it.

Most ways to do this are a product: an agent to install, a database of hosts, an
account, and a place where your passwords end up. This is one static binary. The
browser holds the host list and the host passwords, sends a password once when it
opens a session, and the gateway keeps it for that session and writes it nowhere.
No server-side session store, nothing to dump, nothing to back up.

Point it at a Windows machine and you get the desktop, the pointer shapes,
keyboard and mouse including IME composition, clipboard both ways, and file
transfer both ways — with ranges fetched from the browser on demand, so a 4 GB
file never has to fit in memory anywhere. Resize the desktop live, or go full
screen.

It draws itself like Windows 95 on purpose. A remote console is a tool, and a
tool that looks like the thing it is showing you is easier to trust than one that
looks like a startup.

Go, one binary, no cgo, GPL-3.0. There is a demo mode that draws a synthetic
desktop answering real input, so you can try it without a Windows machine.

English by default; Chinese from the title bar, remembered per browser.
```

## Topics

```
Developer Tools
Linux
Open Source
```

(Product Hunt allows three. Developer Tools and Open Source are the ones that
carry; the third is a guess between Linux and Productivity.)

## Gallery

Order matters: the first image is the thumbnail.

| Order | File | Why |
| --- | --- | --- |
| 1 | `docs/screenshots/session.png` | the whole console with a session in it |
| 2 | `docs/screenshots/login.png` | the door, and that there is only one |
| 3 | `docs/screenshots/host-form.png` | where the credentials live, which is the point |
| 4 | `docs/screenshots/session-zh.png` | it is not English-only |

Images are 2880×1800 at 2× scale. Product Hunt wants 1270×760 as a minimum for
the thumbnail and will crop to a square for the list, so the first image should
still read when the sides are gone: it is a full window, and the middle of it is
the desktop, which is the part worth seeing.

## First comment

> I built this because I wanted RDP in a browser and everything that does it
> well wanted to hold my credentials.
>
> The design decision worth arguing about is that the host passwords live in the
> browser's `localStorage` and never on the server. That is a real trade: the
> browser is the trust boundary, anything that can run script in the page can
> read them, and the defence is a strict Content Security Policy rather than
> encryption. The alternative — a credential store on the server — means the
> gateway becomes the thing worth attacking and the thing worth backing up. I
> preferred the gateway that is worth nothing if it is stolen.
>
> Two more, both from measuring rather than reasoning: I shipped PNG because it
> was smaller than JPEG on a synthetic desktop, and it cost 10 MB/s on a real one
> until I measured a real one; and torn frames turned out to be about *when* a
> finished redraw is shown, not about missing updates. Both are in the README,
> with numbers.
>
> It still tears occasionally on fast motion. The README says so.
>
> Happy to answer anything about the RDP side — grdp is a pure Go client I also
> maintain, so the whole stack is readable.

## Before launching

- [ ] A video. Product Hunt rewards a 30–60 s screen recording more than any
      amount of copy, and there is no substitute for watching a drag stay smooth.
- [ ] The gallery images are uploaded, in the order above.
- [ ] The repository link is in the listing and in the first comment.
- [ ] Someone is watching the comments for the first six hours.
- [ ] The launch is scheduled for 00:01 Pacific, which is when the day's board
      opens.
