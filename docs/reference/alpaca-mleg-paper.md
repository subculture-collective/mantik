# Alpaca multi-leg paper lifecycle

Issue #85 adds a manual, durable paper route for two to four standard option
legs. `mantik alpaca-paper --action mleg-open --input` opens a structure;
`--action mleg-close --input` closes the journal-owned fills. Both actions use
the existing required `--journal`, `--credentials-file` and `--account-id`
flags. The endpoint is fixed to Alpaca paper. No model is called.

This is implementation and synthetic transport evidence. No real iron condor
has been opened or closed by this change. The coordinator must retain that
regular-session paper evidence separately before accepting the real-paper AC.
It is not live trading, profitability evidence or forward qualification.

## Opening input and admission

The opening JSON has four fields, matching `alpaca.MLegOpen`:

- `Order`: `client_order_id`, integer-string strategy `qty`, `type: "limit"`,
  `time_in_force: "day"` or `"gtc"`, signed decimal-string `limit_price`,
  `order_class: "mleg"`, and `legs`. Each leg has `symbol`, positive
  integer-string `ratio_qty`, `side`, and matching `position_intent` of
  `buy_to_open` or `sell_to_open`. Ratios must have GCD one. Parent symbol,
  parent side, fractional/notional quantities and extended hours are absent.
  Positive package limits mean debit; negative limits mean credit. Zero limits
  are refused by this route. Stock limit validation remains positive-only.
- `Structure`: the #84 `options.Structure`, with `Kind` and `Legs`. Signed
  `Contracts` must equal wire strategy units times each signed ratio, in the
  same leg order. `Multiplier` is 100. `Limit` is dollars per underlying share.
  The package limit must not spend more than these admitted per-leg limits.
  Supported kinds are `long_options`, `vertical`, `iron_condor` and
  `iron_butterfly`. Stock/option combinations, covered calls and cash-secured
  single puts are outside this multi-leg route.
- `Chain`: the complete `options.Chain` used for that admission, including spot,
  standard contract identities, source clocks, price observations and Greeks.
  Build it with `options.Read` from retained references and observations, or
  supply an explicitly identified operator-prepared chain. These are indicative
  estimates; the JSON is not a claim of authenticated live quotes. The route
  validates the chain and refuses unavailable Greeks, future evidence and
  evidence older than 24 hours or the declared maximum age. Alpaca must confirm
  options trading level 3 and active, tradable 100-share contracts.
- `Funding`: an `account.CappedState` for a **dedicated** local budget bound to
  the expected paper account. Its opening cash must be `"2000"`, deposit amount
  `"500"` and deposit interval two weeks. Only valid scheduled deposit events
  are accepted. Prior trading, reservations, stock, controls and other event
  kinds are refused. `account.SnapshotCapped` replays the exact events at the
  admission cutoff. This file is an operator-declared local funding record,
  not independent verification of a bank transfer or service authorization.
  Preserve the immutable `FundingMode`: `owner_receipts` requires confirmed
  owner-attested deposits; elapsed slots alone add no cash. Omitted mode and
  `synthetic_schedule` retain legacy simulated funding and supply no real-cash
  or qualification evidence. See [owner receipt rules](owner-book-controls.md#real-deposits).

The callable product entry is `mlegpaper.Open(ctx, trader, input)`. It binds
`PaperTrader.OpenMLeg` to actual `engine.OptionBook.Submit` admission immediately
before the initial POST, after preparatory GETs and durable intent retention.
Use this wrapper for integrations; the adapter's required callback is a trusted
internal boundary, not strategy authority. No simulated fill is treated as a
broker fill. Risk fractions, minimum margin equity, gross/position/sleeve caps,
cash floor, dedicated reserves and gross Greek caps all remain enforced.
The POST clock rechecks contract expiry, spot age and each traded option's
price age against the declared maximum. The original source cutoff and
observation clocks remain retained unchanged.

Each journal owns exactly one lifecycle and one immutable funding snapshot.
Other paper intents or stock abort fences in a new opening journal cause refusal.
The route reserves that budget for the entire lifecycle, including after close;
it does not release or recycle it. Do not allocate the same funding ledger to another journal or
another book. This is a manual dedicated-budget path, not integration into the
shared incubation allocator or a service-side capital authorization protocol.

The initial $2,000 margin book can refuse spreads: pending fees lower projected
equity below the $2,000 margin threshold, invoking cash-account permissions.
Only genuinely recorded scheduled deposits may increase that local funding.
An ample Alpaca paper balance never changes a refusal. Find a structure that
passes the existing caps; do not raise limits or invent deposits to force proof.
For a real paper exercise, retain the actual opening clock and capital evidence
and use receipt-mode funding. Never backdate an opening to manufacture elapsed
deposit slots or relabel a legacy synthetic ledger as real funding.

## Intent, recovery and audit custody

The adapter retains the exact lifecycle, wire requests, confirmed refusals and
close plan under `paper-intent-*` names. The existing journal process lock,
write-once publication and fsync fence the only POST for each client ID. Exact
retries are GET-only, including after restart, a 404, rollover, or a crash between
retaining the lifecycle and sending the wire intent. Changed requests conflict.
A refused admission consumes that identity; no subsequent retry can submit it.

All raw HTTP responses use the existing `paper-response-*` records: method,
path, status, receipt clock, SHA-256, completeness flag and original bytes.
Parent and child venue IDs, symbols, sides, quantities, cumulative filled
quantities and prices must match the retained request. Child ordering may vary;
it is normalized to opening leg order. Duplicate IDs, missing children,
fractional/overfilled quantities and missing positive child fill prices fail
closed. Parent status alone never establishes a fill.

Use the existing `fills` action with an explicit lower time bound to retain
original activity records and venue fee fields. Order cumulative fill quantities
control close sizing; this route does not assert complete fee reconciliation or
ledger equity from those order snapshots. Archive the journal and retain its
archive SHA-256 before handing evidence off. Rollover exports raw responses and
keeps every lifecycle, request, refusal and close-plan fence in the active
journal. No migration or development/production database is required.

## Whole close and fallback

Closing JSON has `OpenClientOrderID`, signed package `LimitPrice`, and positive
string `LegLimits` in opening leg order. The whole-close and each fallback client
ID are derived from the opening identity. There is one immutable close plan;
limits cannot be changed under a fresh identity to bypass an uncertain order.
The CLI emits `MLegCloseResult` even when reconciliation returns an error, so
operators can inspect known orders and remaining quantities.

1. Recover the opener, cancel if pending, and GET until both parent and children
   are terminal. No polling is hidden: a pending result requires repeating the
   exact command. Only proportional, actually filled opening quantities can
   enter a whole close. An opening with no fills needs no close order.
2. Check dedicated settled funding for the whole-close debit and every short
   buyback limit, including conservative execution fees. Opening long premiums
   reduce available cash. Unsettled short premiums and future long-sale proceeds
   do not fund buybacks. Other orders' reserves and unowned stock never do.
3. Submit the reversed mleg with explicit per-leg closing intents. Before any
   new close POST, check exact owned option inventory and overlapping pending
   orders. Other symbols in the account are never adjusted. A full 500-order
   pending-order page is ambiguous and blocks execution.
4. If the whole close remains pending, request cancellation and GET its terminal
   parent and child fills before fallback. Cancellation acknowledgement alone
   is insufficient. A terminal rejection, expiry or cancellation permits
   fallback. A retained HTTP 422 with numeric code 42210000 plus a subsequent
   client-ID GET 404 also establishes a confirmed API validation refusal.
   Mutable message text, timeout, 5xx, malformed/mismatched replies and other
   API codes never establish refusal.
5. Subtract each whole-close child fill from the owned opening quantities.
   Recover any earlier fallback requests before comparing live inventory.
   Before every new short buyback, deduct actual whole-close child buy debits
   and prior fallback buy debits from dedicated cash, keeping the fee bound
   reserved and sale proceeds unavailable. An insufficient balance stops
   further buys and retains the remaining protection, including after restart.
   Buy all remaining shorts closed, confirming their exact filled quantities
   and terminal status. Only when **every** short liability is zero may the
   route sell remaining long protection. A failed, partial or ambiguous short
   close stops progress with every long untouched.
6. Reconcile all scoped positions to zero before reporting `closed`. Exact
   retries inspect the existing orders and never issue replacement POSTs.

A terminal partially filled or rejected fallback leg cannot be replaced through
this route. The remaining protection stays held and the same command reports
pending; operator reconciliation is required. Changed inventory, another pending
order on these symbols, assignment/exercise, a non-proportional opening fill,
unknown venue state or insufficient funding also stops automatic progress.
Preserve the journal and original IDs when resolving those conditions.

## Coordinator's real-paper preflight

Before the real regular-session test, verify the combined source head and its
checks; inspect the expected paper account, options level, current permissions,
standard contracts, session and current prices. Prepare an attributable chain,
a genuinely funded dedicated capped ledger and immutable opening/close inputs.
Check actual OptionBook admission before attempting execution. An initial $2,000
spread refusal is a blocker, not permission to bypass the cap. Reserve cash for
short buybacks and fees without relying on selling the protective longs.

Retain the exact open request, parent/child venue IDs, raw fills, whole-close
request and result, any cancellation/partial/fallback sequence, fee evidence and
final scoped inventory. Preserve complete account snapshots separately when
other books are present. The implementation tests use only fake transports;
account permissions, actual contract availability, fillability, regular-session
execution and real fee custody remain coordinator checks.

## Callable commands and offline checks

`mantik alpaca-paper --help` lists `mleg-open` and `mleg-close`. The opening
action calls `mlegpaper.Open`; the closing action calls `PaperTrader.CloseMLeg`.
Supply the exact opening or closing JSON described above through `--input` and
retain the same journal and inputs for recovery. Neither action belongs to
the shared `incubation` allocator or the compiled qualification collector.

The focused checks use fake transports and synthetic evidence only:

```sh
GOFLAGS=-p=2 GOMAXPROCS=2 mise exec -- go test -count=1 ./internal/alpaca ./internal/mlegpaper ./internal/options ./internal/engine ./internal/account ./internal/risk ./internal/cli
GOFLAGS=-p=2 GOMAXPROCS=2 mise exec -- go test -race -count=1 ./internal/alpaca ./internal/mlegpaper ./internal/options ./internal/engine ./internal/account ./internal/risk ./internal/cli
```

Provider contracts rechecked November 16, 2026:
[create order](https://docs.alpaca.markets/us/reference/postorder),
[level 3 multi-leg trading](https://docs.alpaca.markets/us/docs/options-level-3-trading),
and [options restrictions](https://docs.alpaca.markets/us/docs/options-trading).
