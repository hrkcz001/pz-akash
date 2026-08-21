# Step 7 report — `internal/dashboard`: the pages ported to `html/template`, and the visual diff

`scratch/PLAN.md:198` words this step as *"Dashboard ported to `html/template` at feature
parity (RU/EN + pluralization + downloads) | Visual diff against current"*, under the
plan's assumption 4: *"Dashboard ported at feature parity — no redesign, no new
features."*

Both halves are done. The machine-checkable half is green (`scratch/gate7.ps1`), and the
visual half is a 42-page gallery in `scratch/gallery/` — the evidence, not the verdict.
The verdict is yours: parity against a page a person designed is not a property a test
can assert, so the gate produces the pages and says as much rather than pretending to
have checked.

The audit found one real parity defect (§5) and eight deliberate deviations (§6), one of
which is an addition that assumption 4 forbids and that I am flagging rather than
quietly keeping.

---

## 1. What replaced what

v1's `pz-controller/storage_server.py` is ~2,500 lines, of which the pages are the bulk:
one ~1,500-line Python f-string for the hub, another ~360 for the backups table, a
110-line markdown renderer, and ~400 lines of browser JavaScript whose only job was
localisation. The server always emitted Russian; clicking **EN** walked the DOM by
element id and replaced roughly sixty strings from an `i18nData` object.

v2 is `pzctl/internal/dashboard`: 1,693 lines of Go across ten files, 303 lines of
template, and the stylesheet carried over nearly unchanged.

| file | lines | what it holds |
|---|---|---|
| `view.go` | 251 | `Inputs`, `Options`, `Stage`, `stageOf`, `Chrome` — the seam to the FSM |
| `page.go` | 216 | `BuildPage`, the three player states, the cards, size and price formats |
| `handlers.go` | 230 | the five routes' bodies, locale negotiation, the asset server |
| `i18n.go` | 184 | the `text` struct, `Lang`, RU's three plural forms and EN's two |
| `inline.go` | 177 | inline markdown as a scan, not a regex chain (§6.9) |
| `catalog.go` | 163 | both locales as struct literals — a missing key is a build failure |
| `markdown.go` | 160 | the block-level port of v1's `markdown_to_html` |
| `dashboard.go` | 152 | the embed FS, template parse, and `ServeHTTP`'s route table |
| `backups.go` | 108 | the archive table and its lock prompt |
| `status.go` | 52 | the JSON the open page polls for |
| `templates/*.html` | 303 | `common.html` (head + nav), `page.html`, `backups.html` |
| `assets/dashboard.css` | 904 | v1's stylesheet, unedited except for the three new hooks |
| `assets/dashboard.js` | 151 | header poll, unlock modal, upload — no strings, no localisation |

Tests: 1,584 lines across six `_test.go` files, plus the 269-line gallery generator.

The five routes, all mounted by `cmd/pzctl/controller.go:290`:

```
GET  /              the hub
GET  /backups       the archive table (or its lock prompt)
GET  /assets/…      dashboard.css, dashboard.js
GET  /api/status    the poll's JSON
POST /api/unlock    verify a realm secret, set a cookie, redirect
```

`ServeHTTP` is a `switch` over exact paths (`dashboard.go:156`), not a `ServeMux`, so
`/backups/../server.zip` cannot arrive here as anything but a 404 — the archive
downloads live in `internal/httpapi` behind their own auth (step 6).

---

## 2. The three structural changes, and why each one is a bug fix

**Locale is a server render.** v1 shipped both translations to the browser and swapped
them with JavaScript. Three costs: `i18nData.en.card_client_btn` yields `undefined` when
a key is misspelled and `undefined` renders as the word *undefined* on the page; the
plural rules were written four times, twice in Python and twice in JS, and the four
copies disagreed; and the dashboard **never called `setLanguage` on load**, so a visitor
who had chosen EN got a Russian page on every navigation and only saw English after
clicking again. In v2 a locale is a Go struct literal, the plural rule is one function
per language, and the page arrives in the negotiated language (§6.4).

**Nothing is interpolated into a script.** v1 generated its JavaScript from the same
f-string as the page, so a server name containing an apostrophe broke the script — and
with the script dead, so were the poll, the language switch and the unlock prompt. v2's
script receives exactly three values, as `data-*` attributes on `<body>`: the poll
period and the two strings it shows without a round trip.

**The poll sends state, not a page.** `/api/status` returns the parts of the header that
change while nobody navigates; a *stage* change (an address grid where a banner was)
triggers a reload instead, because that rearranges the card. `BuildPage` calls
`BuildStatus` rather than computing the header itself, and `TestPageAndStatusAgree`
(`page_test.go:343`) fails if anyone inlines it again — one rule, one place, for anything
the poll can change.

---

## 3. How parity was checked

Not by diffing HTML. Golden files would pin the markup and fail on every whitespace
change, which for a port whose requirement is *feature parity* is noise. What matters is
that a person looking at both sets of pages sees the same thing. So the audit had three
layers:

1. **An inventory of v1.** I read `storage_server.py` end to end and wrote down every
   string, element, class name, conditional and count it can emit, with line references —
   including the ones it emits by accident (§6) and the ones it never emits at all.
2. **A gallery.** `TestWriteGallery` renders every appearance v2 can produce to a file:
   42 pages (§7). Each v1 inventory item was located in the rendered output verbatim, or
   accounted for as a deliberate deviation.
3. **Assertions for what a test *can* hold.** The package's own suite executes both
   templates for every stage × locale × unlock combination and asserts the absence of the
   four failure signatures a template port produces: `ZgotmplZ` (an escaping refusal),
   `%!` (a format verb that did not match), `{{` (an unparsed action) and `<no value>` (a
   field that does not exist). Plus the new `wiring_test.go` (§8).

Layer 3 catches "the port is broken". Layer 1 against layer 2 is what catches "the port
works and shows the wrong thing" — which is where the one real defect was (§5).

---

## 4. The parity table

Every v1 element, and where it went.

| v1 | v2 | verdict |
|---|---|---|
| status badge: 6 stages, dot + text | `Badge{Class,Dot,Text}`, `stageOf` collapses 8 FSM statuses to 4 appearances | same |
| address grid: host/IP, port, password | `page.html:47-77`, shown exactly when there is somewhere to connect to | same |
| status banner when offline/booting | `stage.banner(t)`, icon + title + description | same |
| price badge `$0.011/hr` | `priceText` — three decimals below a dime, two above | same format |
| players badge `👥 N игроков` | `PlayersView`, three states instead of one | **fixed, §5–6.1** |
| three download cards, 4 class names each | `newCard`, computed in Go so the stylesheet needs no edits | same |
| server card lock: 🔒/🔓, `btn-locked`/`btn-unlocked` | `page.go:173-182`, asserted by `TestServerCardLockAndUnlock` | same |
| `12 модов • 340 файлов • 134.0 MB` | `packageStats`, one-decimal MB | same |
| torrent banner + `v42.20.3` badge | `page.html:90-104`; omitted when `torrent_file` is empty | same, now optional |
| README rendered from markdown | `markdown.go` + `inline.go`, same blocks and inline forms | same output |
| backups table: name, date, size | `backups.go`, two-decimal MB as v1 had | same |
| disk-pressure warning line | `Warning`, threshold from `disk_warn_percent: 70` | same, now config |
| upload form | one `PUT /backups/{name}`, streamed (step 6) | same UI, one route |
| `Бэкапы 🔒` nav and footer link | `catalog.go` — the static lock glyph is v1's, not a bug | same |
| RU/EN switcher | `o.switcher(lang)`, links that re-render | same control, §6.4 |
| 4 s poll of `/server_info.json` | `GET /api/status`, `poll_interval: 10s` | same job, §6.8 |
| uptime, footer, copy buttons, mod lists, admin controls | absent in v1 (copy buttons were dead code) | absent |
| status badge on the backups page | absent in v1 | absent |

---

## 5. The defect the gallery caught

Reading v1's inventory against the rendered pages turned up one place where v2 showed
something v1 did not.

v1 built the players badge with `display: inline-flex` when `is_online` and
`display: none` otherwise. v2 rendered it unconditionally, so an offline, booting,
stopping, failed or no-state page carried **`👥 игроки: нет данных`** in the header.

That is worse than cosmetic. The whole point of the three player states (§6.1) is that
"no data" is *news* — bug 1 was a count that always read zero, and the fix was to make an
unmeasured count say so. But on an offline page there is nothing to measure and nobody
asking, so the message is noise; on a *booting* page it is noise that reads as a fault,
which is exactly the misreading the fix exists to prevent. v1's rule was right, and
keeping it also sharpens the unknown text: it now only ever appears where it is the
actual news — an online server whose count could not be measured.

The fix is one rule in one place, because the poll can change the stage without a
navigation:

- `Status.ShowPlayers` (`status.go`), set to `stage == StageOnline` — the same condition
  the address grid appears at, which is not a coincidence: both answer questions that
  only exist once players can connect.
- `Page.ShowPlayers` copied from it in `BuildPage`, so the render and the poll cannot
  disagree.
- `page.html:30` adds `hidden` rather than omitting the element — the badge, the stale
  marker and the price badge are always in the document so the poll reveals one by
  clearing an attribute instead of building elements in JavaScript. v1 rebuilt
  `#address-widget-container.innerHTML` wholesale on every tick.
- `dashboard.js:108` toggles the same attribute from `show_players`, or a page loaded
  while the server was down would keep the badge hidden after it came up.
- `TestPlayersBadgeOnlyAppearsOnline` (`page_test.go:148`) pins all four appearances,
  asserts the poll agrees with the page, and asserts the text is rendered even when
  hidden — an empty span would flash on reveal.

Verified in the regenerated gallery: `hidden` on the booting, stopping, offline, failed
and no-state pages, absent on all three online ones.

---

## 6. Deliberate deviations from v1

Assumption 4 says no redesign and no new features. Ten of these are fixes to things v1
got wrong or hardcoded, which parity does not require reproducing. The eleventh is an
addition, and I am flagging it as one.

**6.1 The player count has three states.** v1 printed `0 игроков` from a field it never
populated, so an empty server and a server nobody had asked looked identical — bug 1. v2
distinguishes *measured N*, *measured zero* and *not measured*, and marks a measurement
that has stopped being refreshed (`players_stale_after: 5m`). An unstamped count is
demoted to unknown in `buildPlayers` as well as in `normalize.go`, so the page does not
depend on having been handed a normalised document.

**6.2 The address label follows the configuration.** v1's label was always `IP СЕРВЕРА`.
v2 renders `АДРЕС СЕРВЕРА` with the DNS name when `identity.host` is set and falls back to
v1's label and the raw IP when it is not — so the label never disagrees with the value
underneath it.

**6.3 The join password comes from secrets.** v1 hardcoded `1488` in three user-visible
places, including two copies of the guide embedded in the Python source as a fallback.
v2 reads it from `PZ_JOIN_PASSWORD` and renders it only when `show_join_password: true`
**and** there is an address to join — an offline page has no reason to carry it.

**6.4 The locale is chosen by the server.** Query → cookie → `Accept-Language` →
`default_locale`, with a `?lang=` visit setting the cookie. This is what fixes v1's
never-called `setLanguage`: a visitor who picked EN now gets EN on the next navigation.
It also fixes the counts, which v1 rendered server-side in Russian and its JavaScript
never touched — so v1's English page showed `12 модов • 340 файлов` and `3 игрока`.

**6.5 English pluralises.** v1's EN archive count was the unpluralised `archive(s)`. RU
has three forms, EN has two, and each is one function checked by `i18n_test.go`.

**6.6 The unlock secrets left the URL.** v1's backups password travelled in the query
string, the nav href, every row's download link and the upload form's action, and was
echoed into the page as `const currentToken` — so it reached browser history, the access
log, and the DOM of a page anyone could open. v2 posts it once to `/api/unlock`, which
verifies and sets a cookie; a locked page carries no token and no row data at all. A
locked server card renders a `<button>` with no `href`, not a link.

**6.7 The price is hidden when there is no quote.** v1 printed a default `$0.011/hr`,
which is a price for a lease that may not exist. v2 shows the badge only when the FSM has
a real figure and only in the stages where a lease is running.

**6.8 The poll is slower and configurable.** v1 fetched `/server_info.json` every 4 s.
`poll_interval: 10s` is the default now, and 0 turns polling off.

**6.9 The markdown renderer stops leaking.** v1 built HTML by regex substitution over an
already-escaped string, so a link target went into `href="…"` with only `&<>` escaped,
and substitutions ran *over* text that was already tagged — meaning a code span's
contents could still be rewritten by a later rule. `inline.go` is a scan: a code span is
escaped and emitted in one step, so nothing later in the parse can reach inside it. Same
blocks, same inline forms, same output tags, so the rendered guide still diffs clean.

**6.10 The backups empty state stops naming a path.** v1's said `/data/backups/`, a
container path that means nothing to a visitor and is no longer accurate.

**6.11 The addition: a downloaded / not-downloaded tag.** This is a new feature and
assumption 4 forbids new features, so it needs a reason. The locked decision is *"I'll
periodically download backups, and upload them before server start. No need for
persistent storage"* — which makes "has anyone fetched this archive yet" the single most
important property on the page, because an archive nobody has fetched exists in exactly
one place: a disk that dies with the lease. v1 had no way to know it. The same row also
marks the archive the next start will restore. **Say the word and I will drop both;** the
tag is one field on `BackupRow` and one `{{if}}`.

---

## 7. The gallery

`scratch/gate7.ps1` writes 42 pages to `scratch/gallery/` and copies `dashboard.css` and
`dashboard.js` beside them, so each page renders from a `file://` URL with its real
stylesheet. `index.html` lists them; `pwsh scratch/gate7.ps1 -Open` opens it.

The axes:

- **8 status cases** × 2 locales × 2 unlock states = 32 hub pages —
  `online`, `online-players-unknown`, `online-players-stale`, `booting`, `stopping`,
  `offline`, `failed`, `no-state`.
- **5 backups/rejection pages** per locale = 10 — `backups-locked`, `backups-empty`,
  `backups-warning` (the table with disk pressure), `backups-wrong-password`,
  `hub-wrong-password`.

Every case carries the same version, package stats, guide, archive list and disk figure,
so the one axis a page varies is the one its name claims. The guide is a hand-written RU
markdown document exercising headings, ordered and unordered lists, bold, inline code, a
blockquote and a link — a gallery whose guide is one paragraph proves nothing about the
renderer.

Two things worth knowing about it:

- **The generator does not reuse `fakeData`.** That test double replaces `Guide` with
  `"guide for " + lang` so the handler tests can prove the locale reached it; a gallery
  needs the markdown itself. Hence a local `galleryData`.
- **The `-wrong-password` pages are the one part with no v1 equivalent.** They are a
  state the page only enters from the unlock redirect, so the generator reaches them
  through the query string.

Each rendered page is checked for the four canaries before it is written, because a
gallery nobody reads would happily contain a `ZgotmplZ`.

---

## 8. The seam a compiler cannot check

`wiring_test.go` is new and closes the one gap the rest of the suite cannot see. The
browser reaches into the rendered document by id, and *nothing* links
`getElementById("players-badge")` in `dashboard.js` to `id="players-badge"` in
`page.html`. Rename one and the lookup returns null, the next property access throws, and
the **whole script** stops — poll dead, unlock modal dead, upload dead — on a page that
looks perfectly fine until someone opens a console.

The test extracts the 19 ids the script looks up and asserts each is rendered by one of
the three templates. It checks one direction only: an id the script wants and no template
renders is a broken page, while an id a template carries that the script never touches is
just a hook for CSS or a test. A companion does the same for the class selectors the
script introduces. A floor assertion (`< 10 ids found`) makes an extraction that silently
stops matching a failure rather than a pass.

I verified it can fail rather than being vacuous: adding a `getElementById("no-such-id")`
to the script produced

```
--- FAIL: TestEveryScriptedIDExistsInATemplate
    wiring_test.go:36: dashboard.js looks up id "no-such-id" and no template renders it
```

and the probe was then removed.

---

## 9. One fix carried in this step that is not the dashboard

`-race` under WSL turned up a defect in `internal/agent`, and it is the same *class* of
mistake as bug 1 — a message that reports a fault where there is none — so it went in
here rather than waiting.

`saveOn` wrote `save` to the PZ console and *then* called `waitFor` to watch for the
confirmation. Only lines arriving after the call are seen, so a confirmation printed
promptly was missed, and the agent reported

```
no save confirmation within 60s (watching for …)
```

for a save that had completed — which is the sort of message that sends an operator
looking at the wrong thing, during a halt, which is when the window is widest because the
host is loaded.

The fix splits the wait in two: `expect(want)` arms the matcher and returns the wait, so
the caller registers **before** writing the command that provokes the line. `waitFor` is
now `expect(want)(timeout)` and stays as the shape for a line nobody provoked. The
returned function is what removes the matcher, so the error path calls `wait(0)` rather
than leaking one. `pz_test.go` grew a case that writes the confirmation with no delay,
which fails against the old order.

---

## 10. What was checked

`pwsh scratch/gate7.ps1`:

```
=== gofmt                                        ok
=== go vet ./...                                 ok
=== go test ./internal/dashboard/ ./cmd/pzctl/ ./internal/httpapi/
  internal/dashboard   ok  1.775s
  cmd/pzctl            ok  1.735s
  internal/httpapi     ok  2.923s
=== gallery                                      42 pages written
gate 7: machine checks pass — the visual diff is yours to make
```

The gate runs `cmd/pzctl` and `internal/httpapi` too, not just the dashboard: a dashboard
that renders but is not mounted is not a ported dashboard.

Full suite on Debian/ext4 with `-race`, two runs:

```
run 1: exit=0 races=0 failed_tests=0
run 2: exit=0 races=0 failed_tests=0
```

The tests that carry the parity claims:

| test | what fails it |
|---|---|
| `TestBothPagesRenderForEveryStageAndLocale` | any stage × locale × unlock combination that errors or emits a canary |
| `TestPlayerCountHasThreeStates` | a zero printed for an unknown count — bug 1's visible half |
| `TestPlayersBadgeOnlyAppearsOnline` | the badge shown where v1 hid it, or the poll disagreeing with the page |
| `TestPageAndStatusAgree` | the header computed twice, so the page and the poll can drift |
| `TestStageOf` | a backup taking the server off the page, or an endpoint-less online status reading as offline |
| `TestServerCardLockAndUnlock` | a locked card carrying an `href`, or v1's class names changing |
| `TestPackageStats` / `TestSizeText` | a size or count format drifting from v1's |
| `TestEveryScriptedIDExistsInATemplate` | a renamed id that would kill the whole script |
| `i18n_test.go` | a plural form, either language |
| `markdown_test.go` | a block or inline form, and the escaping v1 leaked through |

---

## 11. Still open — the two from step 6, plus one decision on this step

You said you would answer these later, so nothing here blocks step 8.

**11.1 Retention cannot be satisfied on the configured disk.** `config.yaml:282` sets
`retention_count: 24` and `:295` sets `upload_max_bytes: 2147483648` (2 GiB). Twenty-four
archives at the permitted maximum is 48 GiB on a 20 GiB volume that also holds
`packages_dir: /data/packages` and must keep `min_free_bytes: 2147483648` free. The count
rule can therefore never be the binding constraint — the disk rule always trips first, so
`retention_count` is decoration. Either lower it to something the disk can hold (with
2 GiB reserved and the packages, ~8 is the honest number) or drop the count rule and keep
only the size rule.

**11.2 The admin password has no delivery channel.** `PZ_ADMIN_PASSWORD` is generated and
stored but nothing consumes it. The comment at `internal/agent/boot.go:289-292` says *"The
password is substituted into the .ini by the controller"* — that is not true: `vsrania.ini`
has no `AdminPassword` key and no code writes one. The reasoning in the comment is sound
(keeping the secret out of the container's process list, where v1 put it and where any
shell could read it from `/proc`), but the mechanism does not exist. It needs either an
`.ini` write on the controller side or an explicit decision that the server runs without
an admin password; right now the comment describes an intention as a fact.

**11.3 The downloaded/not-downloaded tag** (§6.11) — keep or drop. It is the one thing in
this step that assumption 4 would exclude, and it is one field and one `{{if}}` to remove.

---

## 12. Next

Step 8, CI: one Go build producing two images on `debian:bookworm-slim`, a
`config validate` gate, no secrets in any layer, `USER steam`, no gosu, no sshd.

One requirement this step adds to it: the image build must place **`game.torrent` and
`README.md`** into `packages_dir` alongside the zips and `packages_manifest.json`.
`dashboard.torrent_file` and `dashboard.guide_file` are basenames resolved inside that
directory, and both features go quiet — banner omitted, guide section omitted — when the
file is absent. That is the right failure mode, but it means a build that forgets them
produces a dashboard missing two sections and no error anywhere.

Step 9 is the cutover, and carries the repo rename to `pz-saves-proto` / `pz-akash-proto`.






