# ADR 0022 — A client per device, resumed by its id

Status: accepted (Phase 12, prompt 002)

Amends ADR 0017 ("a client per address"). Keeps ADR 0017's tiers, header,
eviction-bars-an-address and per-address rate limits.

## Context

ADR 0017 keyed a session's clients by the caller's address: `POST …/clients`
returned "the client bound to your address". Behind a home or office NAT every
device shares one public address, so a phone joining the session its laptop
created was handed the laptop's client — the same name, the same admin flag,
and a Participants list of one. The dashboard promised "everyone shares the
same beams" and then showed nobody had joined.

The address was doing two jobs: identifying a participant (wrong — a
participant is a device) and anchoring eviction and rate limits (right — an
address is what a misbehaving stranger cannot easily change).

## Decision

- **A client is a device.** `POST /api/sessions/{sid}/clients` mints a new
  client — unique name, the caller's address recorded on it — every time, unless
  the caller presents `X-Airlift-Client` naming a client of this session bound to
  the **same address**, in which case that client is returned (its name kept,
  its admin flag upgradable but never downgraded). A reload or a second tab
  therefore keeps its identity; a second device becomes a second participant.
  Create and password-join mint without resuming.
- **A resume from another address is refused.** The client id is public (it is
  in the snapshot), so honouring it from elsewhere would let one participant
  become another; the caller simply gets a fresh client, and the `client` tier's
  existing `c.Addr == addr` check stands unchanged.
- **The scanner resumes the dashboard's client.** The dashboard's *Scan a beam*
  link carries `&c=<client_id>`; the scanner registers with it, so one device is
  one participant with roles `viewer · relay`, not two entries.
- **Eviction still bars an address**, dropping every client there — except when
  the target shares the evicting session admin's own address: barring it would
  evict the admin, so only that one client is dropped and the address stays open
  (the other devices behind that NAT are the admin's). An airlift-admin
  eviction always bars.
- Rate limits stay per address (ADR 0017).

## Amendment (2026-09-14): the resume key

The address turned out to be a poor stand-in for "the same device": a phone's
address changes whenever its screen sleeps (carrier NAT, rotating IPv6
privacy addresses, Wi-Fi to cellular), so the resume was refused and every
reload minted a new participant, which then lingered. Identity now rests on a
**resume key**: a random 128-bit secret minted with the client and returned
only in the create/join/register replies, held by the device (localStorage)
and presented as `X-Airlift-Client-Key` on every client-tier call and as
`resume_key` on a resume. A matching key proves the device whatever the
address and re-binds the client to it (so eviction still bars where the device
is now); a keyless call is honoured only from the bound address, for pages
built before the key — and a keyless resume never returns the key, so a
shared address cannot be turned into a permanent identity. The public id is no longer what stops a takeover — the
key is. A client with no open stream and no activity for `ClientIdleTTL`
(10 min) is *parked*: hidden from the participants list, its record kept, and
un-parked by its next keyed call — so nothing dangles, and nothing is lost.
The dashboard's scan link still carries `c=`; the scanner takes the key from
the dashboard's stored record on the same device.

## Consequences

- `Session.RegisterClient(addr, name, admin, resume)`;
  `Session.EvictClientByID(cid, byAddr)`; the `byAddr` index is gone.
- Web: `registerClient(…, {resume})` sends the stored id; `parseJoin` reads `c=`.
- Docs: CLAUDE.md item 15 amended, `docs/API.md` client text.
- Not changed: the four tiers, the header, the knock flow (still keyed by
  address — a knocker has no client yet), the admin `addresses` map.
