# Responsive TUI and archive views

Owner authorized implementation on 21 October 2026 after the guided bundle test.
Root owns the contract; cb-engineer owns implementation and focused tests;
cb-tester-reviewer owns independent review and final checks after explicit freeze.

## Responsive layout

Use initial terminal dimensions and each WindowSizeMsg. Budget the entire screen,
including borders, tabs, scope, notices, editor and help, within terminal cells
and rows. Remove the arbitrary 140-column cap. Long content must not push controls
outside the visible area. Preserve Unicode cell handling and terminal sanitization.
At dimensions too small for useful interaction, show a bounded resize hint and
retain state; quit/cancel must work. Do not issue network requests on resize.

Provide a scrolling content viewport, keeping the selected row visible as the
terminal shrinks or grows. Preserve selection by immutable ID across filtering
and refresh where possible; clamp when an item disappears. Long selected details,
message bodies and diagnostics must remain inspectable through scrolling. Selected
project, session and message details include their complete sanitized immutable
IDs; list previews or timestamps do not replace these inspectable identifiers. Resize
must preserve drafts, scope, confirmation state and actor identity. Reserve visible
input/cancel controls while editing. Cover all six screens, not only tables.

## Archive semantics

Archive is a TUI-only, derived view filter over authorized records. It does not
delete, rewrite, retire, mark read, resume or change retention. Existing CLI/MCP,
HTTP, Store and offline-mirror contracts remain compatible; no schema migration,
background job, new credential access or server deployment is needed.

- A session is in Archive when status is retired or moved and its updated_at is
  at least 24 hours old. This means 24 hours without a change after retirement;
  using existing updated_at conservatively delays archival if metadata changes.
  Active/stale sessions never archive merely because their host is offline.
- An inbox delivery is in Archive when its nonempty read_at is at least 24 hours
  old. Classification belongs to that recipient, not globally to the message.
  Unread deliveries stay Current regardless of message age. Recently read messages
  remain Current until the threshold. Retention/expiry still limits availability.
- Use one injected current time per classification pass; equality at 24 hours
  archives. Missing, malformed or future timestamps remain Current. Reevaluate on
  refresh, entering the view and a bounded local minute tick, without extra polling.

Sessions and Inbox expose Current (default), Archive and All using `v` to cycle
when not editing/confirming. Show the selected filter in the view and contextual
help. Archive rows retain details and existing authorized actions; browsing does
not change read state. This increment has no manual archive/unarchive mutation:
All exposes retained data without altering the derived classification.

Filtering must preserve raw loaded records so toggling never discards data.
Do not claim exhaustive archive/search coverage from a single page: expose
server paging with `]` for next page and `[` for a previously visited page, and
show page/filter plus whether additional server pages exist. Apply archive filters
to each loaded page; an empty filtered page must retain next-page navigation.
Reset paging on explicit refresh; preserve server sequence cursors and scope.
Do not replace server paging with unbounded fetching. Offline mirrors retain
their existing scoped/read-only behavior and must label incomplete coverage.
No global search is introduced or claimed by this increment.

## Acceptance

Test whole-screen cell and row bounds across narrow/wide/short terminals, all six
screens, selected long details, editing, confirmations, CJK/combining/emoji and
untrusted control text. Test repeated resize without mutation/network activity,
selection visibility and draft preservation. Test exact 24-hour boundaries,
unread and active exclusions, invalid timestamps, independent recipient reads,
filter changes, empty filtered pages, paging cursor integrity and offline limits.
Publish exact additive public TUI APIs before reviewer probes; no unrelated
implementation changes. Native Linux live resizing remains an owner observation.
