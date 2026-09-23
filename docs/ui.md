# Browser interface

Start `taskworker serve`, then open its loopback URL (by default
`http://127.0.0.1:7433/`) in a current browser. The executable embeds the page,
stylesheet and JavaScript modules. No frontend server, CDN, npm install or network
asset request is required. JavaScript, BigInt, streaming fetch, session storage,
and JSON.parse reviver source support are required for full int64 metadata.
Unrepresentable values fail closed on older browsers instead of being rounded.

The browser and CLI use the same service. Submit a fresh job with an installed
supported model and positive output budget. Role, system prompt and user prompt
are separate. Optional context, temperature and seed remain unspecified when
blank. Integer entry is limited to JavaScript's exact safe range; seeds are
further limited to the Ollama adapter's supported 0..4294967295 range. The service
still validates resource and model limits. Refresh (also performed on model
selection) reads inventory/metadata only; it never downloads or loads a model.
Ctrl/Cmd+Enter in the prompt field submits the current draft (plain Enter stays a
newline); a live character count sits under the field, a display convenience only,
never a token estimate.

Role and system prompt each have their own saved-text library, kept in this
browser only and never sent to the service. **+** saves the current text (named in
its dropdown by its own first few words; saving identical text again just moves
it to the top rather than duplicating it); the dropdown loads a saved entry back
into the field; **−** deletes the selected entry, asking for a second click within
five seconds to confirm, and reverting to a plain **−** if that window lapses or a
different entry is chosen meanwhile.

The job list is sorted newest first by default. Choose to sort by created time or by
last-updated time (the finish time, else the start time, else the creation time) and
use the button beside the sort choice to reverse the order. Each job shows its
created and updated times and a `Job N` number, its chronological position among the
retained jobs; the number does not change with the sort — including under the filter
below — but can shift if retained jobs are ever removed. A job you cancel reads
*User cancelled*. A job reached by an explicit branch also shows *↳ continues Job N*,
naming the parent it branched from.

The filter box above the list narrows it to jobs whose ID, state, model, origin or
own new prompt contains the typed text (case-insensitive); it never searches
accumulated output, and it is a display filter only, never sent to the service.

**Archive**, in the detail panel, and **Show Archived**, beside the filter box, are
also a display-only convenience local to this browser: archiving a job just removes
it from the default list view (marked *· Archived* and dimmed when *Show Archived*
reveals it again) and never touches the job itself. The service, other clients and
other browsers still see it exactly as before; there is no delete. Membership is
kept in `localStorage`, not `sessionStorage`, so it persists across restarts of this
browser; a blocked or cleared store just means nothing is archived.

Each terminal job's row also carries a **Continue Convo** button: a one-click
shortcut for the same explicit branch the detail panel's Branch button starts, from
that row without first selecting the job. It is disabled until the job reaches a
terminal state.

Checking *Remember as default model* saves the currently selected model for this browser
origin (`localStorage`) and preselects it on a later visit whenever it is still installed and
supported; unchecking it, or a browser that blocks storage, silently falls back to the ordinary
choice.

Select a retained job to inspect its origin, ID, lineage, original request,
composed instructions, output, faults, usage and context provenance. Missing
measurements mean unknown. Advertised capacity, requested allocation, timestamped
loaded observation and approximate input estimate are distinct. Open the *Original
Request, Instructions, & Measurements* disclosure for the complete recorded fields.
Output is plain text and never runs markup or tools. Live updates do not scroll the
reader; use *Jump to Latest Text*. The heading above shows the selected job's state
in capitals (`Job · SUCCEEDED`) for emphasis only — the literal lowercase value is
unchanged everywhere it is actually data: the job list rows, the raw JSON in the
disclosure above, and every fault code. A running or cancelling selected job also
shows a small *Generating…*/*Cancelling…* indicator beside its heading, reflecting
its existing state only.

Below the output box, at the far right of the *Original Request, Instructions, &
Measurements* disclosure: **Copy Job ID**, **Copy Response** and **Copy Convo** place
plain text on the system clipboard, relabeling themselves *Copied ✓* for 1.5 seconds;
a blocked clipboard leaves a notice instead. **Copy Convo** and **Download Convo**
(a `.md` file) both cover this job plus every ancestor reached by explicit branch
links, oldest turn first — a job with no such ancestry still yields its own one-turn
conversation. A retry does not add a turn: it duplicates its own parent's request, so
the walk steps through it to whatever that parent branched from. None of this is a
service concept; it is assembled in the browser from jobs already retained locally.

Cancel is explicit and retains committed partial output. Queue pause prevents
new dispatch; active inference continues. Retry copies the terminal parent's
accepted settings and materialized instructions. Branch requires a terminal
parent and uses the complete current draft as its new request; retained parent
prompt/output, including partial output, enter history. Fresh submissions have
no prior-job history. No exact inference pause, checkpoint or rewind is offered.
*Load job settings* loads the selected job's original role, system prompt, output
budget, context and temperature/seed into the compose form so a new job can reuse
them; it never touches the user prompt, and sets the model only if it is still
installed. It is available for any selected job, terminal or not, and does not by
itself submit anything or change lineage — selecting a job to view it never
changes the compose form on its own.
After acceptance, editing the draft or Prepare another attempt enables another
create. This prevents rapid repeat clicks from silently creating two attempts.

Connection state is separate from backend discovery and job state. Disconnected
views remain visible and are labeled stale. One stream replays from the last
successfully applied store/decimal cursor. Invalid state, cursor gaps or expired/
replaced stores require a fresh snapshot. Reload always starts with a snapshot;
no saved cursor is applied to an empty view. Mutation responses identify accepted
jobs but never replace newer event-derived state. Closing a page does not cancel
accepted work.

## Local recovery storage

Before submit/retry/branch, the browser saves the operation, complete original
JSON command (including prompts, options, origin and caller key), and store ID in
**sessionStorage for this origin and tab**, under `taskworker.pending.v1`.
Reload preserves this recovery record. No external service receives it. Browser
storage is plaintext and subject to the user's browser policies. Closing the tab
normally clears it; keep the tab open through uncertain acceptance. Ordinary draft
edits and job/output projections are not persisted in browser storage.

A failed/timed-out response may still have accepted inference. The recovery panel
resends exactly the saved command/key. New creates are blocked until resolved;
draft edits cannot alter the saved recovery command. Recovery is blocked against
a different store ID: reconnect to the original service data directory. Definite
validation/queue rejection clears the pending record; uncertain errors retain it.
Confirmed acceptance removes it. Storage read/write/quota failures block creates;
no command is sent without a verified recovery copy. A corrupt or inaccessible
record requires preserving/repairing browser storage before further creates.

## Development checks

Runtime has no Node dependency. Browser adapter tests use Node 22+ only during
verification:

```text
node --experimental-default-type=module internal/adapters/webui/state.test.mjs
node --experimental-default-type=module internal/adapters/webui/app.test.mjs
```

Go tests cover exact embedded routes, content/cache/security headers and existing
Host/Origin protections. Product source isolation includes go.mod, cmd/**/*.go,
internal/**/*.go and these exact assets: internal/adapters/webui/index.html,
app.css, app.js and state.js. Test modules are optional verification inputs and
are not embedded. No workspace filesystem is served. Release packaging is a
separate later tranche.
