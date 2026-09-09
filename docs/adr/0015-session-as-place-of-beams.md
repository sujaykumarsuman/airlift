# ADR 0015 — A session is a place that holds a list of beams

Status: accepted (Phase 6.3, prompt 002)

Amends ADR 0005 (server-owned sessions): a tower session is no longer one
transfer bound to the first MANIFEST it sees. It supersedes the single-sender
binding described in the original `docs/PROTOCOL.md` reassembly step.

## Context

Prompt 002 makes the tower a shared, multi-user service. The operator described
the session model directly: *a session is a place where read beams are kept, like
a list; one address may join many sessions, and a session is just a common
sharing field.* So the unit that a token guards is a **place**, and what
accumulates inside it is a **list of payloads**, not a single transfer.

The prompt-001 tower bound each session to exactly one sender session: the first
MANIFEST won, frames from any other sender were discarded and later ones counted
`bad`, and the whole session carried one `state`, one `name`, one progress bar.
That cannot express a place that receives two folders in a row, or two phones
relaying two different beams into one shared field. The frame header already
carries a per-run sender u32 (`docs/PROTOCOL.md`), so the identity needed to
separate beams is already on the wire — nothing in the protocol has to change.

## Decision

A **session** is a place: identity (`sid`), token, subscribers, TTL, and a list
of **beams** keyed by the sender-session u32 from the frame header. A **beam** is
one payload; its id is `bid` = the eight hex digits of that u32, used in URLs,
downloads, the dashboard and (later) on disk.

- **A beam is born from its MANIFEST.** A MANIFEST for a new sender creates a
  beam in `RECEIVING`; a MANIFEST for a known sender is a re-inserted schedule
  frame, counted `dup`. There is no place-level `WAITING_MANIFEST` state — an
  empty place simply has no beams. Each beam runs
  `RECEIVING → VERIFYING → READY | FAILED` on its own peeling decoder,
  independently of every other beam.
- **A differing sender is a different beam, never an error.** DATA/FOUNTAIN
  frames that arrive before their own MANIFEST are held **per sender** (up to 8
  pending senders, 65 536 frames across them) and adopted when that sender's
  manifest arrives. Adoption drains **only the arriving sender's** held
  bucket — never the whole hold — so one beam's manifest cannot discard another
  beam's pre-manifest frames. (This is the load-bearing invariant: draining the
  wrong bucket silently loses a concurrent beam.)
- **Caps bound an accumulating place.** `max_beams` (default 10, an airlift-admin
  config key) limits beams per place; an over-cap MANIFEST is counted `bad`. A
  per-beam gzip ceiling (`max_gz_bytes`) fails a beam on arrival, before it
  allocates a decoder, so a hostile manifest cannot reserve memory.
- **The snapshot is a place envelope**: `{sid, relays, beams[], expires_at}`,
  with `beams` in arrival order and every progress/verdict/timing field moved
  onto the beam (`docs/API.md`). The frames reply is
  `{accepted, dup, bad, completed_beams}`; `completed_beams` lists the bids that
  filled during that POST.
- **Everything beam-scoped is addressed by bid.** Downloads are
  `?beam=<bid>&as=…` (unknown bid → 404, not-READY → 409); the completion hook
  hands the verifier `(session, beam)`; verified output lands per beam.
- **Activity, not traffic, keeps a place alive.** A POST that accepts or dedups
  at least one frame refreshes the TTL; an all-`bad` noise POST does not.

## Consequences

- **The relay never latches "done".** A place stays open across beams, so the
  scanner keeps relaying whatever the camera decodes; the tower sorts frames into
  beams. The relay reports `completed_beams` for information only.
- **The scan page tracks one active beam** (the last still `RECEIVING`, else the
  most recent) for its progress display, and no longer stops the camera when a
  beam finishes.
- **The dashboard renders a list** of beam cards, each with its own progress,
  verdicts and downloads, under a place header (beam count, relays, link).
- **`internal/replay` feeds one beam** — a dump is a single sender — tracking its
  bid through `completed_beams` and polling that beam in the place snapshot. The
  end-to-end tests now also drive two senders into one place and confirm the two
  beams verify and download independently.
- The air-gapped side is untouched: the beam HTML never talked to the tower, and
  the sender u32 it already stamps is exactly the key the place needs.
- Per-beam on-disk layout, client registry, passwords and the session lifecycle
  build on this list-of-beams shape and are specified separately (ADR 0013).
