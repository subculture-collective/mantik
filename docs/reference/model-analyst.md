# Callable model analyst

Issue #93 adds a bounded runtime analyst workflow and a deterministic strategy
adapter. It does not submit orders. `internal/analyst.Runner.Run` is the callable
service boundary; `mantik model-analyst` supplies the same runner through the CLI.
`internal/strategy/analyst` contains only the offline compiled strategy and typed
output parser. Its source passes the existing strategy import/clock audit.

The fixed workflow makes at most four subscription calls: a trend analyst and an
independent countercase analyst read the same frozen daily bars, a debate sees
both typed views, and a trader sees the views and debate. Every role uses the
predeclared provider/model. There is no native model delegation, tool dispatch,
research generation, or model-selected call graph.

Every response must contain exactly `action` and `asset`:

| Response | Meaning |
| --- | --- |
| `{"action":"long","asset":"SPY"}` | Select a frozen asset; code assigns sleeve weight `1`. |
| `{"action":"flat","asset":""}` | Explicit complete exit target. |
| `{"action":"hold","asset":""}` | No target; preserve actual exposure without resizing. |

Unknown fields, duplicate or differently cased keys, nulls, missing fields,
trailing JSON, markdown, unknown assets and unsupported actions fail closed.
Models cannot supply weights, money, quantity, stops, orders, health or activation.
Categorical views are advisory observations, not finalized decision explanations.

## Frozen identity and forward evidence

The owner supplies `Config` before declaring the strategy. It contains provider,
exact model, inclusive training cutoff, provider documentation URL, reviewer and
review clock, frozen assets, daily call limit and per-call timeout. The external
review receipt hash must equal `Config.MetadataHash()`. `Config.Spec()` returns a
canonical `ModelDriven` identity binding all of those settings and protocol
version `analyst-v1`. A changed model, cutoff, budget, timeout or asset set changes
the strategy identity. No cutoff is guessed or obtained from model output.

The input names an already retained forward declaration, whose exact spec,
strategy, family, gate, account, book and sleeve must agree. The gate must already
be `forward_only`; the runner writes no gates or forward declarations. Current
family totals come from storage before every call and before producing a target.
Unreconciled family batches, new search starts, expired entry/holding clocks,
retired or blocked sleeves and missing budget all stop further model calls.

The bounded manifest retains raw daily bars, original revision/publication/
observation/availability clocks and adjustment inputs. All data and adjustment
knowledge must be after the inclusive cutoff and visible at the explicit input
clock. Only completed prior-day bars are accepted. Input clocks more than ten
minutes old are refused. The request hash binds the entire manifest, while the
view hash binds the actual model input. A receipt records `InputAt` separately
from `At`, the completion clock: model output cannot be consumed at the earlier
input clock. A compiled receipt is valid only for its exact completion-time view
over the unchanged frozen data.

`EvidenceClass` and `SourceSHA256` stay attached to the result and receipt.
Synthetic fixtures remain `synthetic_fixture`; runtime market observations remain
`prospective_market_unqualified`. Neither receives qualification credit, a
historical performance claim, or protective-exit proof. Qualification #92 and
real protective proof #107 are separate work.

This runner has no research tool path and refuses
`historical_market_unqualified` inputs. It cannot backtest, compose or evolve a
strategy, select research winners, or publish trial returns. Research attempts
must use the research desk and its actual #103 family-start/batch accounting;
an analyst runtime cycle cannot stand in for those records. A model reply that
tries to propose a strategy is rejected, never dispatched or evaluated.

## Budget, custody and recovery

The runner reads the authoritative capped book and uses the existing distributor
with exposure holds to preview its budget. The fixed incubation pool remains 5%
of local equity, shared among incubating owners, with at most 1% per model version.
Existing oversight reductions and ceilings apply. This preview does not reserve
capital, place orders or assert operational health. Final code sizing and every
ordinary admission/submission guard remain necessary when consuming a target.

Every call uses `internal/model.Journal.Call` with restricted tools and the frozen
daily limit and timeout. Subscription authentication stays with its existing
owner; this command accepts no API keys. Use the same stable mode-0700 subscription
journal and consistent daily limit as the other subscription callers. Creating a
new journal creates another budget; this adapter is not host-global accounting.
The journal counts attempts, including failures, before process dispatch, and
enforces daily exhaustion, contention and persisted backoff.

The existing append-only research event journal retains the request, each call
intent, exact private reply bytes, parsed views and terminal result under an
`analyst-` session prefix. It uses existing event kinds and needs no new schema.
A call intent is written before dispatch. Exact terminal retries reuse evidence
without another call. Any interrupted workflow is closed with no target; it is
never automatically relaunched, even after an ambiguous evidence-write
acknowledgement. A new observation requires a new cycle ID. Raw replies are
private evidence and are omitted from normal command output.

Model outages, daily exhaustion, backoff and invalid replies yield
`Status: no_target` with `Target.HoldExposure: true`, no targets and no model error
trade. Storage errors remain errors and must not be treated as usable decisions;
a failed terminal evidence write also clears the returned target. An explicit
flat result has `HoldExposure: false` and an empty target set. These meanings are
deliberately different.

## CLI and service integration

`mantik model-analyst --help` lists the flags. `--action run` requires:

- `--id`: an immutable runtime cycle ID, at most 64 characters.
- `--database-env`: the explicit environment variable holding the bootstrap
  PostgreSQL URL, containing the existing book, gate and forward records.
- `--input`: a mode-0600 regular JSON file encoding `internal/analyst.Request`.
- `--input-sha256`: the independently reviewed SHA-256 of those exact file bytes.
- `--journal`: the stable private subscription budget directory.

`--action show` requires only the ID and database environment flag. It returns
the retained terminal result or an interrupted status, without model calls.
Neither action reads paper-provider credentials or constructs a broker client.
The source-review receipt and exact input pin are supplied by the invoking owner;
self-consistency of a hash is not independent source approval.

The coordinator must connect the result to incubation at its existing safety
boundary. This change deliberately leaves the shared health hooks and worker
contracts untouched:

1. Invoke the runner outside `Strategy.Decide`, with a frozen input clock and
   separately retained gate, forward declaration and budget sources.
2. For operation, require a prospective market result and the exact retained
   receipt/request identity. Use the receipt completion clock for the decision,
   then obtain post-decision quotes through the ordinary incubation path.
3. Preserve `Target.HoldExposure` directly. Do not translate it into empty flat
   targets or `KeepPositions`, which can resize old weights after a deposit.
   The compiled adapter returns `ErrNoTarget` for missing, stale, unavailable or
   hold receipts so direct use through the legacy strategy interface fails
   before producing orders. The runner itself returns exposure holds normally.
4. Run current family, forward-clock, health, reconciliation, risk and final
   hard-sizing checks independently of model availability. Retained results are
   evidence, not renewed execution authority. Protective work stays independent.

The existing `incubation` CLI recipe selector does not automatically load analyst
receipts. That hook must preserve the hold distinction and source provenance
above; there is no unchecked result import or model-enabled worker bypass.

## Focused verification

The in-process tests cover the real memory gate/forward/book readers, four-role
prompt flow, constrained outputs, transport failure, current family accounting,
budget refusal, call/evidence interruption and concurrent exact retries. A test
opens the actual subscription journal with an exhausted budget and proves that
it returns before dispatching any model CLI. Distributor fixtures check the 5%
shared pool, 1% version cap, unchanged exposure after a deposit during an outage,
and explicit flat liquidation. Clock fixtures reject backdating a completed
model response to its earlier input time.

Run focused unit/race tests for `internal/analyst`,
`internal/strategy/analyst` and the `TestModelAnalyst` CLI test, plus `go vet` on
those packages with the pinned Go 1.26.9 toolchain. These synthetic checks do not
prove a real subscription response, broker behavior, deployment, returns,
qualification, or protective execution.
