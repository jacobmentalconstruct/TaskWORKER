// Service projection only: never infer transitions or merge mutation responses.
const idPattern = /^[a-f0-9]{32}$/;
const states = new Set(['queued','running','cancelling','succeeded','failed','cancelled','interrupted']);
export const terminal = j => !!j && ['succeeded','failed','cancelled','interrupted'].includes(j.state);
// Display ordering only: derived from the job timestamps, never sent to or inferred by the service.
const compareText = (a, b) => a < b ? -1 : a > b ? 1 : 0;
function compareStamp(a, b) { const d = Date.parse(a) - Date.parse(b); return d ? d : Number.isNaN(d) ? compareText(a, b) : 0; }
// A job's last update is the latest recorded lifecycle stamp; a queued job was last touched when it was created.
export const updatedAt = j => j.finished_at || j.started_at || j.created_at;
const byCreated = (a, b) => compareStamp(a.created_at, b.created_at) || compareText(a.id, b.id);
// Chronological position among the retained jobs (1 = oldest), independent of how the list is sorted.
export function ordinals(jobs) { return new Map([...jobs].sort(byCreated).map((j, i) => [j.id, i + 1])); }
export function sortJobs(jobs, key, descending) {
  const cmp = key === 'updated' ? (a, b) => compareStamp(updatedAt(a), updatedAt(b)) || byCreated(a, b) : byCreated;
  return [...jobs].sort((a, b) => descending ? cmp(b, a) : cmp(a, b));
}
export const faultText = code => code === 'cancelled' ? 'User cancelled' : `Job fault: ${code}`;

// A job's conversation, oldest turn first: itself plus every ancestor reached by explicit branch
// links. A retry duplicates its own parent's turn (same prompt and instructions, copied verbatim
// by the service), so a retry link is stepped through rather than counted as a distinct turn.
// An ancestor no longer retained (a future retention design could evict one) ends the walk there;
// the returned conversation is simply shorter, never wrong.
export function conversationTurns(jobs, startId) {
  const start = jobs.get(startId);
  if (!start) return [];
  const turns = [start];
  let cursor = start;
  while (cursor.lineage) {
    const parent = jobs.get(cursor.lineage.parent_id);
    if (!parent) break;
    if (cursor.lineage.relation === 'retry') { cursor = parent; continue; }
    turns.push(parent);
    cursor = parent;
  }
  return turns.reverse();
}

// A job's original request, reduced to compose-form strings. Never includes the prompt: loading a
// job's settings must not silently overwrite what someone is drafting to send.
export function draftFromRequest(request) {
  const o = request.options || {}, text = v => (v === null || v === undefined ? '' : String(v));
  return {model: request.model, role: request.role || '', system: request.system_prompt || '',
    budget: text(o.max_output_tokens), context: text(o.context_tokens),
    temperature: text(o.temperature), seed: text(o.seed)};
}

const DEFAULT_MODEL_KEY = 'taskworker.defaultModel.v1';
// A remembered model choice is a convenience, never load-bearing: unlike Recovery, which fails
// closed to protect an idempotency key, a lost or unavailable default silently falls back to the
// ordinary model choice, with no notice and no effect on correctness.
export function loadDefaultModel(storage) {
  try { const v = storage.getItem(DEFAULT_MODEL_KEY); return typeof v === 'string' && v && v.length <= 256 ? v : null; }
  catch { return null; }
}
export function saveDefaultModel(storage, id) { try { storage.setItem(DEFAULT_MODEL_KEY, id); } catch {} }
export function clearDefaultModel(storage) { try { storage.removeItem(DEFAULT_MODEL_KEY); } catch {} }

// Which retained jobs are archived: a display-only convenience kept in this browser, never sent
// to or known by the service, and never a delete — the job is untouched server-side and every
// other client still sees it. Membership only, so a plain deduplicated id array is enough; a
// corrupt or blocked store just means nothing is archived, the same fail-open convenience as above.
const ARCHIVE_KEY = 'taskworker.archivedJobs.v1';
export function loadArchived(storage) {
  try {
    const list = JSON.parse(storage.getItem(ARCHIVE_KEY) || '[]');
    return new Set(Array.isArray(list) && list.every(id => idPattern.test(id)) ? list : []);
  } catch { return new Set(); }
}
function saveArchived(storage, set) { try { storage.setItem(ARCHIVE_KEY, JSON.stringify([...set])); } catch {} }
export function archiveJob(storage, id) { const s = loadArchived(storage); s.add(id); saveArchived(storage, s); return s; }
export function unarchiveJob(storage, id) { const s = loadArchived(storage); s.delete(id); saveArchived(storage, s); return s; }

// Saved role/system-prompt libraries: browser-local text snippets a person reuses across jobs.
// A convenience like the default model above, never load-bearing, and never sent to the service.
// Entries are plain strings, deduplicated by exact trimmed text (re-saving moves an entry to the
// most-recently-used end); the newest LIBRARY_LIMIT are kept, oldest dropped first.
const ROLE_LIBRARY_KEY = 'taskworker.roleLibrary.v1', SYSTEM_LIBRARY_KEY = 'taskworker.systemLibrary.v1';
const LIBRARY_LIMIT = 100;
function loadLibrary(storage, key) {
  try {
    const list = JSON.parse(storage.getItem(key) || '[]');
    return Array.isArray(list) && list.every(t => typeof t === 'string') ? list : [];
  } catch { return []; }
}
function saveLibrary(storage, key, list) { try { storage.setItem(key, JSON.stringify(list)); } catch {} }
function addToLibrary(storage, key, text) {
  const trimmed = text.trim();
  if (!trimmed) return loadLibrary(storage, key);
  const list = loadLibrary(storage, key).filter(t => t !== trimmed);
  list.push(trimmed);
  while (list.length > LIBRARY_LIMIT) list.shift();
  saveLibrary(storage, key, list);
  return list;
}
function removeFromLibrary(storage, key, text) {
  const list = loadLibrary(storage, key).filter(t => t !== text);
  saveLibrary(storage, key, list);
  return list;
}
export const loadRoleLibrary = storage => loadLibrary(storage, ROLE_LIBRARY_KEY);
export const loadSystemLibrary = storage => loadLibrary(storage, SYSTEM_LIBRARY_KEY);
export const addRole = (storage, text) => addToLibrary(storage, ROLE_LIBRARY_KEY, text);
export const addSystemPrompt = (storage, text) => addToLibrary(storage, SYSTEM_LIBRARY_KEY, text);
export const removeRole = (storage, text) => removeFromLibrary(storage, ROLE_LIBRARY_KEY, text);
export const removeSystemPrompt = (storage, text) => removeFromLibrary(storage, SYSTEM_LIBRARY_KEY, text);
// A dropdown label derived from the saved text itself: the first six words, or 60 characters,
// whichever is shorter, with an ellipsis if anything was cut. Two entries can derive the same
// label; the dropdown's value is always the full text, so which one loads is never ambiguous.
export function libraryLabel(text) {
  const trimmed = text.trim();
  if (!trimmed) return '(empty)';
  const words = trimmed.split(/\s+/);
  let label = words.slice(0, 6).join(' ');
  const cut = words.length > 6;
  if (label.length > 60) return label.slice(0, 60).trimEnd() + '…';
  return label + (cut ? '…' : '');
}
export function check(ok) { if (!ok) throw new Error('Invalid shared state; fresh snapshot required'); }
export function parse(text) {
  return JSON.parse(text, (key, value, context) => {
    if (key === 'sequence') check(typeof value === 'string');
    // Temperature is protocol float64, unlike integer counts/seeds/durations.
    if (key === 'temperature' && typeof value === 'number') {check(Number.isFinite(value));return value;}
    if (typeof value === 'number' && Number.isInteger(value) && !Number.isSafeInteger(value)) {
      check(context && /^-?\d+$/.test(context.source));
      return context.source; // preserve protocol int64 metadata, never round it
    }
    return value;
  });
}
export function cursor(c) {
  check(c && idPattern.test(c.store_id) && typeof c.sequence === 'string' && /^(0|[1-9]\d*)$/.test(c.sequence));
  check(c.sequence.length <= 20 && BigInt(c.sequence) <= 18446744073709551615n);
  return c.store_id + ':' + c.sequence;
}
function job(j) {
  check(j && idPattern.test(j.id) && states.has(j.state) && j.request && typeof j.request.model === 'string' && typeof j.request.prompt === 'string' && j.request.options && j.instructions && typeof j.instructions.system === 'string' && typeof j.instructions.prompt === 'string' && j.result && typeof j.result.text === 'string' && j.result.context && j.result.usage);
  check(new TextEncoder().encode(j.result.text).length <= 8*1024*1024);
  return j;
}
function queue(q) { check(q && typeof q.paused === 'boolean' && (q.pending === null || Array.isArray(q.pending)) && (q.pending || []).every(id => idPattern.test(id)) && (!q.active || idPattern.test(q.active)) && Number.isSafeInteger(q.max_pending)); return q; }
export class Projection {
  constructor() { this.cursor = null; this.jobs = new Map(); this.bytes = new Map(); this.queue = null; }
  snapshot(s, frameID) {
    check(cursor(s.cursor) === frameID && (s.jobs === null || Array.isArray(s.jobs)));
    const jobs = new Map(), bytes = new Map();
    for (const j of s.jobs || []) { job(j); check(!jobs.has(j.id)); jobs.set(j.id,j); bytes.set(j.id,new TextEncoder().encode(j.result.text).length); }
    queue(s.queue);
    this.jobs = jobs; this.bytes = bytes; this.queue = s.queue; this.cursor = s.cursor;
  }
  event(e, frameID) {
    check(cursor(e.cursor) === frameID && this.cursor && e.cursor.store_id === this.cursor.store_id);
    const n = BigInt(e.cursor.sequence), last = BigInt(this.cursor.sequence);
    if (n <= last) return false;
    check(n === last + 1n);
    if (['job.accepted','job.state','job.result'].includes(e.kind)) {
      const j = job(e.job); check(e.job_id === j.id && !e.output && !e.queue);
      check(e.kind === 'job.accepted' ? !this.jobs.has(j.id) : this.jobs.has(j.id));
      this.jobs.set(j.id,j); this.bytes.set(j.id,new TextEncoder().encode(j.result.text).length);
    } else if (e.kind === 'job.output') {
      const j = this.jobs.get(e.job_id), o = e.output;
      check(j && o && !e.job && !e.queue && typeof o.text === 'string' && Number.isSafeInteger(o.offset_bytes) && this.bytes.get(e.job_id) === o.offset_bytes);
      const size = o.offset_bytes + new TextEncoder().encode(o.text).length; check(size <= 8*1024*1024);
      j.result.text += o.text; this.bytes.set(j.id,size);
    } else if (e.kind === 'queue.state') {
      check(!e.job && !e.output && !e.job_id); this.queue = queue(e.queue);
    } else check(false);
    this.cursor = e.cursor; return true;
  }
}
export function integer(value, min, max = Number.MAX_SAFE_INTEGER) {
  check(/^-?\d+$/.test(value)); const n = Number(value);
  if (!Number.isSafeInteger(n) || n < min || n > max) throw new Error('Enter an integer within the displayed range (maximum 9007199254740991).');
  return n;
}
export class Recovery {
  constructor(storage) { this.storage = storage; this.key = 'taskworker.pending.v1'; this.pending = null; this.load(); }
  load() {
    const raw = this.storage.getItem(this.key);
    if (raw) { const p = JSON.parse(raw); check(p && ['submit','retry','branch'].includes(p.operation) && idPattern.test(p.store) && typeof p.body === 'string'); const c = JSON.parse(p.body); check(typeof c.idempotency_key === 'string' && c.idempotency_key.length > 0); this.pending = p; }
  }
  save(operation, command, store) {
    check(!this.pending && idPattern.test(store));
    const p = {operation, body:JSON.stringify(command), store};
    const raw = JSON.stringify(p); this.pending = p;
    this.storage.setItem(this.key,raw);
    check(this.storage.getItem(this.key) === raw); return p;
  }
  clear() { this.storage.removeItem(this.key); check(this.storage.getItem(this.key) === null); this.pending = null; }
}
// Fetch stream instead of EventSource so pre-header faults are inspectable.
export async function stream(response, apply, signal, activity = ()=>{}) {
  check(response.headers.get('Content-Type')?.startsWith('text/event-stream'));
  const reader = response.body.getReader(), decoder = new TextDecoder('utf-8',{fatal:true});
  let buffer = '';
  try {
    while (!signal.aborted) {
      const {value,done} = await reader.read();
      if (done) throw new Error('Service stream disconnected');
      activity();
      buffer += decoder.decode(value,{stream:true});
      check(buffer.length <= 256*1024*1024);
      let end;
      while ((end = buffer.indexOf('\n\n')) !== -1) {
        const raw = buffer.slice(0,end); buffer = buffer.slice(end+2);
        const frame = {};
        for (const line of raw.split('\n')) {
          if (line.startsWith(':')) continue;
          const colon = line.indexOf(': '), k = line.slice(0,colon);
          check(colon !== -1 && ['event','id','data'].includes(k) && !(k in frame)); frame[k] = line.slice(colon+2);
        }
        if (!frame.event) { check(Object.keys(frame).length === 0); continue; }
        check(frame.data);
        const data = parse(frame.data);
        if (frame.event === 'error') { const err = new Error('Service stream fault'); err.code = data.code; throw err; }
        check(['snapshot','event'].includes(frame.event));
        try { await apply(frame.event,data,frame.id); }
        catch (error) { error.resync = true; throw error; }
      }
    }
  } finally { await reader.cancel().catch(()=>{}); reader.releaseLock(); }
}
