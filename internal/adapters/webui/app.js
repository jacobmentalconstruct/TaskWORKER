import {Projection, Recovery, cursor, terminal, parse, integer, stream, check, ordinals, sortJobs, updatedAt, faultText, draftFromRequest, loadDefaultModel, saveDefaultModel, clearDefaultModel, loadRoleLibrary, loadSystemLibrary, addRole, addSystemPrompt, removeRole, removeSystemPrompt, libraryLabel, conversationTurns, loadArchived, archiveJob, unarchiveJob} from './state.js';
const $ = id => document.getElementById(id);
const projection = new Projection();
let selected = null, branchParent = null, connected = false, busy = false, storageOK = true, recovery, createReady = true;
let sortKey = 'created', newestFirst = true, jobFilter = '', showArchived = false;
// A display-only convenience, like the default model below: never sent to the service, and a
// blocked or missing store just means nothing is archived, with no other effect on correctness.
let archived = new Set();
try { archived = loadArchived(localStorage); } catch {}
let renderedJob = null, renderedLength = 0, renderedObject = null, metadataJob = null, modelRevision = 0;
const rows = new Map();
// Reused across renders rather than recreated, like each job row below: only its own textContent
// changes. Visual emphasis only — the literal lowercase state string is never altered, including
// everywhere else it appears (job-info's raw JSON, job list rows, faultText).
const detailState = document.createElement('span'); detailState.className = 'state';
const stamp = t => { const d = new Date(t); return Number.isNaN(d.getTime()) ? String(t) : d.toLocaleString(); };
function notice(text) { $('notice').textContent = text; }
try { recovery = new Recovery(sessionStorage); } catch { storageOK = false; notice('Recovery storage is unavailable or invalid. Creates are blocked; preserve any existing recovery data before repairing browser storage.'); }
// A remembered model choice is a convenience only; unlike Recovery above, a blocked or missing
// store just falls back to the ordinary model choice, silently.
let defaultModel = null;
try { defaultModel = loadDefaultModel(localStorage); } catch {}
// Saved role/system-prompt libraries: rebuilt only on an explicit save or delete, never on a
// live-update render, so an open dropdown is never disturbed by an unrelated event arriving.
// Deleting requires a second click within 5s (the button relabels to ask for it); anything else
// - picking a different entry, or letting the window lapse - disarms it.
function renderLibrary(select, list, placeholder) {
  const current = select.value;
  select.replaceChildren();
  const blank = document.createElement('option'); blank.value = ''; blank.textContent = placeholder; select.append(blank);
  for (const text of list) { const o = document.createElement('option'); o.value = text; o.textContent = libraryLabel(text); select.append(o); }
  select.value = list.includes(current) ? current : '';
}
function librarySetup(fieldId, libraryId, saveId, deleteId, load, add, remove, placeholder) {
  let armed = null, timer;
  const disarm = () => { armed = null; clearTimeout(timer); };
  try { renderLibrary($(libraryId), load(localStorage), placeholder); } catch {}
  $(libraryId).onchange = () => { disarm(); const v = $(libraryId).value; if (v) $(fieldId).value = v; createReady = true; render(); };
  $(saveId).onclick = () => {
    const text = $(fieldId).value.trim(); if (!text) return;
    renderLibrary($(libraryId), add(localStorage, text), placeholder);
    $(libraryId).value = text; disarm(); render();
  };
  $(deleteId).onclick = () => {
    const sel = $(libraryId).value; if (!sel) return;
    if (armed === sel) { disarm(); renderLibrary($(libraryId), remove(localStorage, sel), placeholder); }
    else { armed = sel; clearTimeout(timer); timer = setTimeout(() => { armed = null; render(); }, 5000); }
    render();
  };
  return () => { // called from render(): cheap attribute-only sync, safe every tick
    $(saveId).disabled = !$(fieldId).value.trim();
    $(deleteId).disabled = !$(libraryId).value;
    $(deleteId).textContent = armed && armed === $(libraryId).value ? 'Confirm Delete' : '−';
  };
}
const syncRoleLibrary = librarySetup('role', 'role-library', 'role-save', 'role-delete', loadRoleLibrary, addRole, removeRole, 'Saved roles…');
const syncSystemLibrary = librarySetup('system', 'system-library', 'system-save', 'system-delete', loadSystemLibrary, addSystemPrompt, removeSystemPrompt, 'Saved system prompts…');
function loadSettings(request) {
  const d = draftFromRequest(request);
  if ([...$('model').options].some(o => o.value === d.model)) $('model').value = d.model;
  $('role').value = d.role; $('system').value = d.system; $('budget').value = d.budget;
  $('context').value = d.context; $('temperature').value = d.temperature; $('seed').value = d.seed;
}
function pendingUI() {
  const p = recovery?.pending;
  $('recovery').hidden = !p;
  $('recovery-note').textContent = p ? `Saved ${p.operation} command. Acceptance may be uncertain. Draft edits do not change it. ${projection.cursor && p.store !== projection.cursor.store_id ? 'Different service store: recovery is blocked. Reconnect to the original store.' : 'Resend uses the same command and key.'}` : '';
  $('recover').disabled = !storageOK || busy || !connected || !p || p.store !== projection.cursor?.store_id;
  $('submit').disabled = !createReady || busy || !storageOK || !!p || !connected;
  $('another').hidden = createReady || !!p;
}
// A plain client-side filter, never sent to or evaluated by the service: matches the job's id,
// state, model, origin and its own new prompt (not accumulated output, which can reach 8 MiB).
function jobMatches(j, needle) {
  return j.id.includes(needle) || j.state.includes(needle) || j.request.model.toLowerCase().includes(needle)
    || (j.origin || '').toLowerCase().includes(needle) || j.request.prompt.toLowerCase().includes(needle);
}
function render() {
  pendingUI();
  syncRoleLibrary(); syncSystemLibrary();
  const q = projection.queue;
  $('queue-toggle').disabled = !connected;
  if (q) {
    $('queue-toggle').textContent = q.paused ? 'Resume Dispatch' : 'Pause Dispatch';
    $('queue-info').textContent = `${q.paused ? 'Dispatch paused' : 'Dispatch enabled'} · ${(q.pending || []).length}/${q.max_pending} queued · Active: ${q.active || 'none'}`;
  }
  const order = sortJobs(projection.jobs.values(), sortKey, newestFirst), number = ordinals(projection.jobs.values());
  // Ordinal numbers come from the full retained set above, never the filtered view: a job's
  // "Job N" position must not shift just because a filter or archiving is narrowing what's shown.
  const filtered = order.filter(j => (showArchived || !archived.has(j.id)) && (!jobFilter || jobMatches(j, jobFilter)));
  const liveIDs = new Set(filtered.map(j=>j.id));
  for (const [id,row] of rows) if (!liveIDs.has(id)) {row.remove(); rows.delete(id);}
  const list = $('jobs');
  filtered.forEach((j, i) => {
    let row = rows.get(j.id);
    if (!row) {
      row = document.createElement('div'); row.className='job-row'; row.setAttribute('role','button'); row.tabIndex=0;
      row.onclick=()=>{if(selected!==j.id) createReady=true;selected=j.id; render();};
      row.onkeydown=e=>{if (e.key==='Enter' || e.key===' ') {e.preventDefault(); row.onclick();}};
      row.label=document.createElement('span'); row.append(row.label);
      row.chain=document.createElement('button'); row.chain.type='button'; row.chain.className='chain';
      row.chain.textContent='Continue Convo'; row.chain.title='Start a new draft that branches from this job, carrying its prompt and output into history.';
      // The row's own id never changes once created, so closing over it here (rather than the
      // per-render job object, which is replaced wholesale on every state-changing event) stays
      // correct across re-renders; terminal-ness is re-checked fresh at click time regardless.
      row.chain.onclick=e=>{e.stopPropagation(); if (terminal(projection.jobs.get(j.id))) startBranch(j.id); render();};
      row.append(row.chain);
      rows.set(j.id,row);
    }
    const at = list.children[i];
    if (at !== row) { if (at) list.insertBefore(row, at); else list.append(row); }
    const position = (q?.pending || []).indexOf(j.id);
    const parentNum = j.lineage?.relation === 'branch' ? number.get(j.lineage.parent_id) : null;
    const isArchived = archived.has(j.id);
    const label = `Job ${number.get(j.id)} · ${position >= 0 ? '#'+(position+1)+' queued · ' : ''}${j.state} · ${j.request.model} · ${j.origin || 'origin unknown'} · ${j.id}
Created ${stamp(j.created_at)} · Updated ${stamp(updatedAt(j))}${parentNum ? `\n↳ continues Job ${parentNum}` : ''}${isArchived ? '\n· Archived' : ''}`;
    if (row.label.textContent !== label) row.label.textContent=label;
    row.setAttribute('aria-pressed',String(j.id === selected));
    row.className = isArchived ? 'job-row archived' : 'job-row';
    row.chain.disabled = !terminal(j);
  });
  $('sort-dir').textContent = newestFirst ? 'Newest First' : 'Oldest First';
  const j = projection.jobs.get(selected);
  $('cancel').disabled = !connected || !j || terminal(j);
  $('retry').disabled = !createReady || !connected || !terminal(j) || busy || !!recovery?.pending || !storageOK;
  $('branch').disabled = !connected || !terminal(j);
  $('load-settings').disabled = !j;
  $('archive-toggle').disabled = !j;
  $('archive-toggle').textContent = j && archived.has(j.id) ? 'Unarchive' : 'Archive';
  $('copy-id').disabled = !j;
  $('copy-conversation').disabled = !j;
  $('download-conversation').disabled = !j;
  $('copy-output').disabled = !j || !j.result.text;
  if (j) { $('detail-title').textContent = 'Job · '; detailState.textContent = j.state; $('detail-title').append(detailState); }
  else $('detail-title').textContent = 'Select a Job';
  const active = j && (j.state === 'running' || j.state === 'cancelling');
  $('activity').hidden = !active;
  if (active) $('activity').textContent = j.state === 'cancelling' ? '● Cancelling…' : '● Generating…';
  $('job-meta').textContent = j ? `Job ${number.get(j.id)} · ID ${j.id} · Created ${stamp(j.created_at)} · Updated ${stamp(updatedAt(j))} · Origin ${j.origin || 'unknown'} · Parent ${j.lineage ? j.lineage.parent_id+' ('+j.lineage.relation+')' : 'none — fresh job'}` : '';
  $('job-error').textContent = j?.error ? faultText(j.error.code) : '';
  $('prompt-count').textContent = `${$('prompt').value.length.toLocaleString()} characters · Ctrl/Cmd+Enter to submit`;
  const text = j?.result.text || '', out = $('output');
  if (renderedObject !== j) {const top=renderedJob===j?.id ? out.scrollTop : 0;out.textContent=text;out.scrollTop=top;}
  else if (text.length > renderedLength) {const delta=text.slice(renderedLength);if(out.firstChild) out.firstChild.appendData(delta);else out.textContent=delta;}
  // Do not move the reader on live chunks. Navigation to the end is explicit.
  renderedJob=j?.id; renderedLength=text.length; renderedObject=j;
  if (metadataJob !== j) {metadataJob=j; renderMetadata();}
}
function renderMetadata() {
  if (!$('metadata').open) return;
  const j = projection.jobs.get(selected);
  if (!j) {$('job-info').textContent='';return;}
  const {text,...measurements} = j.result;
  $('job-info').textContent = JSON.stringify({original_request:j.request,composed_instructions:j.instructions,measurements,unknown_fields:'Absent values are unknown, not zero.',created_at:j.created_at,started_at:j.started_at,finished_at:j.finished_at},null,2);
}
$('metadata').ontoggle=renderMetadata;
let scheduled=false;
function scheduleRender() {if (!scheduled) {scheduled=true; setTimeout(()=>{scheduled=false;render();},80);}}
function status(text, live=false) {connected=live; $('connection').textContent=text; render();}
let stopping=false, controller, modelController, syncGeneration=0;
const requests=new Set();
addEventListener('pagehide',()=>{stopping=true;syncGeneration++;controller?.abort();for(const request of requests) request.abort();});
addEventListener('pageshow',event=>{if(event.persisted){stopping=false;void synchronize();void models();}});
async function synchronize() {
  const generation=++syncGeneration;
  status('Connecting to service · displayed state is stale');
  let fresh=true, failures=0;
  while (!stopping && generation===syncGeneration) {
    const syncController=new AbortController();controller=syncController;
    let timer=setTimeout(()=>syncController.abort(),45000);
    try {
      const path='/v1/events'+(!fresh && projection.cursor ? '?after='+encodeURIComponent(cursor(projection.cursor)) : '');
      let needsSnapshot=fresh;
      const response=await fetch(path,{signal:syncController.signal,cache:'no-store'});
      check(!stopping && generation===syncGeneration);
      if (!response.ok) { const fault=await response.json(); const error=new Error('Stream refused'); error.code=fault.code; throw error; }
      if (!needsSnapshot) status('Service connected · shared state live',true);
      await stream(response,async (kind,data,id)=>{
        check(!stopping && generation===syncGeneration);clearTimeout(timer);timer=setTimeout(()=>syncController.abort(),45000);
        if (kind === 'snapshot') {check(needsSnapshot);projection.snapshot(data,id);needsSnapshot=false;fresh=false;if (!projection.jobs.has(selected)) selected=null;}
        else {check(!needsSnapshot);projection.event(data,id);}
        failures=0; connected=true;$('connection').textContent='Service connected · shared state live';scheduleRender();
      },syncController.signal,()=>{clearTimeout(timer);timer=setTimeout(()=>syncController.abort(),45000);});
    } catch (error) {
      if (stopping || generation!==syncGeneration) break;
      if (['cursor_invalid','cursor_expired'].includes(error.code) || error.resync || error.message.startsWith('Invalid shared state') || error instanceof SyntaxError || error instanceof TypeError) fresh=true;
      status(`${fresh ? 'Resynchronizing from a fresh snapshot' : 'Disconnected · reconnecting'} · displayed state is stale`);
      failures=Math.min(failures+1,6);
    } finally {clearTimeout(timer);syncController.abort();}
    await new Promise(resolve=>setTimeout(resolve,Math.min(1000*2**(failures-1),15000)));
  }
}
// All ordinary requests are bounded; mutation results never overwrite projection.
async function request(path,body,externalSignal) {
  const controller=new AbortController();requests.add(controller);
  const abort=()=>controller.abort();externalSignal?.addEventListener('abort',abort,{once:true});
  const timer=setTimeout(abort,35000);
  try {
  const response=await fetch('/v1/'+path,{method:body === undefined ? 'GET':'POST',headers:body === undefined ? {}:{'Content-Type':'application/json'},body,signal:controller.signal,cache:'no-store'});
  const value=parse(await response.text());
  if (!response.ok) {const error=new Error(value.code || 'service_error');error.status=response.status;throw error;}
  return value;
  } finally {clearTimeout(timer);requests.delete(controller);externalSignal?.removeEventListener('abort',abort);}
}
async function models() {
  const revision=++modelRevision, previous=$('model').value;
  modelController?.abort();modelController=new AbortController();
  $('backend').textContent='Checking backend metadata…';
  try {
    const data=await request('models',undefined,modelController.signal);if (revision!==modelRevision) return;
    check(Array.isArray(data));
    $('model').replaceChildren();
    for (const model of data) {const option=document.createElement('option');option.value=model.id;option.textContent=model.id+(model.capabilities.text_generation?' · supported':' · unsupported');option.disabled=!model.capabilities.text_generation;$('model').append(option);}
    if (data.some(m=>m.id===previous)) $('model').value=previous;
    else if (defaultModel && data.some(m=>m.id===defaultModel && m.capabilities.text_generation)) $('model').value=defaultModel;
    else $('model').value=data.find(m=>m.capabilities.text_generation)?.id || '';
    const chosen=data.find(m=>m.id===$('model').value);
    $('model-info').textContent=JSON.stringify(chosen || {context:'unknown',capabilities:'unknown'},null,2);
    $('backend').textContent=data.some(m=>m.capabilities.text_generation)?'Backend available · installed models only':'Backend available · no supported text model';
    $('default-model').disabled=!$('model').value;
    $('default-model').checked=!!defaultModel && $('model').value===defaultModel;
  } catch {if (revision===modelRevision) {$('backend').textContent='Backend unavailable or metadata request failed · previous metadata is stale';}}
}
$('refresh').onclick=models;$('model').onchange=models;
$('default-model').onchange=()=>{
  if ($('default-model').checked) {const id=$('model').value;if (id) {defaultModel=id;saveDefaultModel(localStorage,id);} else $('default-model').checked=false;}
  else {defaultModel=null;clearDefaultModel(localStorage);}
};
function draftRequest() {
  const options={max_output_tokens:integer($('budget').value,1)};
  if ($('context').value) options.context_tokens=integer($('context').value,1);
  if ($('seed').value) options.seed=integer($('seed').value,0,4294967295);
  if ($('temperature').value) {const t=Number($('temperature').value);if (!Number.isFinite(t) || t<0 || t>3.4028234663852886e38) throw new Error('Temperature must be finite, nonnegative and within float32 range.');options.temperature=t;}
  return {model:$('model').value,role:$('role').value,system_prompt:$('system').value,prompt:$('prompt').value,options};
}
async function sendSaved() {
  if (!storageOK || busy || !recovery?.pending || !connected) return;
  const p=recovery.pending;if (p.store!==projection.cursor.store_id) return;
  busy=true;createReady=false;pendingUI();
  try {
    const j=await request(p.operation,p.body);check(j && /^[a-f0-9]{32}$/.test(j.id));
    selected=j.id;
    try {recovery.clear();} catch {storageOK=false;throw new Error('storage');}
    notice(`Accepted job ${j.id}. Draft retained. Shared state arrives through the service stream.`);
  } catch(error) {
    if ([400,404,413,415,422,429].includes(error.status)) {
      try {recovery.clear();createReady=true;notice(`Command rejected: ${error.message}. Draft retained; correct it before sending again.`);} catch {storageOK=false;notice('Recovery storage failed. Creates blocked.');}
    } else notice('Acceptance unresolved. The saved original command and key remain available for recovery.');
  } finally {busy=false;render();}
}
function create(operation,extra={}) {
  if (!createReady || busy || !connected || !storageOK || recovery.pending) return;
  let saving=false;
  try {
    const command={idempotency_key:crypto.randomUUID(),origin:'browser',...extra};
    if (operation!=='retry') command.request=draftRequest();
    saving=true;recovery.save(operation,command,projection.cursor.store_id);createReady=false;render();void sendSaved();
  } catch(error) {if(saving) storageOK=false;notice(error.message.startsWith('Enter') || error.message.startsWith('Temperature') ? error.message : 'Cannot save or validate command. Nothing sent. Check integer fields and browser storage.');render();}
}
function submitDraft(e) {
  e.preventDefault();
  if (branchParent && !terminal(projection.jobs.get(branchParent))) {notice('Branch requires a retained terminal parent.');return;}
  create(branchParent?'branch':'submit',branchParent?{parent_id:branchParent}:{});
}
$('draft').onsubmit=submitDraft;
// Ctrl/Cmd+Enter submits from the prompt textarea, where plain Enter must stay a newline.
$('prompt').onkeydown=e=>{if ((e.ctrlKey||e.metaKey) && e.key==='Enter') submitDraft(e);};
$('recover').onclick=sendSaved;
$('another').onclick=()=>{createReady=true;render();};
$('draft').oninput=()=>{if(!busy && !recovery?.pending) {createReady=true;render();}};
$('retry').onclick=()=>{if(terminal(projection.jobs.get(selected))) create('retry',{parent_id:selected});};
// Shared by the detail panel's Branch button and each terminal job row's quick Continue action:
// both just start the same explicit branch, from whichever job id they were given.
function startBranch(id) {
  branchParent=id;selected=id;createReady=true;
  $('mode-note').textContent=`Branch from ${id}. Retained parent prompt and output (including partial output) enter history. All new settings come from this draft.`;
  $('compose-title').textContent='Explicit Branch';$('submit').textContent='Submit Branch';$('fresh').hidden=false;$('prompt').focus();
}
$('branch').onclick=()=>{if (terminal(projection.jobs.get(selected))) startBranch(selected);};
$('fresh').onclick=()=>{branchParent=null;$('compose-title').textContent='Prompt Workbench';$('mode-note').textContent='Fresh submission · no prior job history.';$('submit').textContent='Submit Fresh Job';$('fresh').hidden=true;};
$('load-settings').onclick=()=>{const j=projection.jobs.get(selected);if (j) {loadSettings(j.request);createReady=true;render();}};
async function control(path) {try {await request(path,'{}');notice('Control accepted; observing shared state.');}catch {notice('Control response unavailable. Inspect shared state before repeating.');}}
$('sort-key').onchange=()=>{sortKey=$('sort-key').value==='updated'?'updated':'created';render();};
$('sort-dir').onclick=()=>{newestFirst=!newestFirst;render();};
$('job-filter').oninput=()=>{jobFilter=$('job-filter').value.trim().toLowerCase();render();};
$('show-archived').onchange=()=>{showArchived=$('show-archived').checked;render();};
$('archive-toggle').onclick=()=>{
  if (!selected) return;
  archived = archived.has(selected) ? unarchiveJob(localStorage,selected) : archiveJob(localStorage,selected);
  render();
};
// A transient "Copied" relabel, like the saved-text libraries' "Confirm delete": never a separate
// toast, so it can't pile up or outlive the element it's describing.
function copyToClipboard(button,text,label) {
  navigator.clipboard.writeText(text).then(()=>{button.textContent='Copied ✓';setTimeout(()=>{button.textContent=label;},1500);})
    .catch(()=>notice('Clipboard access failed. Copy manually instead.'));
}
// This job plus every ancestor reached by explicit branch links (state.js conversationTurns),
// rendered as a readable transcript. A single unlinked job still yields a one-turn conversation.
function conversationText(id) {
  const turns=conversationTurns(projection.jobs,id);
  if (!turns.length) return '';
  const number=ordinals(projection.jobs.values());
  const parts=[`# TaskWorker conversation`,`${turns.length} turn${turns.length===1?'':'s'} · exported ${new Date().toISOString()}`,''];
  turns.forEach((t,i)=>{
    parts.push(`## Turn ${i+1} — Job ${number.get(t.id)} · ${t.state} · ${t.id}`);
    if (t.instructions?.system) parts.push(`Role/system: ${t.instructions.system}`,'');
    parts.push('Prompt:',t.request.prompt,'','Response:',t.result.text || '(no output)','');
  });
  return parts.join('\n');
}
$('copy-id').onclick=()=>{if (selected) copyToClipboard($('copy-id'),selected,'Copy Job ID');};
$('copy-output').onclick=()=>{const j=projection.jobs.get(selected);if (j) copyToClipboard($('copy-output'),j.result.text,'Copy Response');};
$('copy-conversation').onclick=()=>{if (selected) copyToClipboard($('copy-conversation'),conversationText(selected),'Copy Convo');};
$('download-conversation').onclick=()=>{
  if (!selected) return;
  const blob=new Blob([conversationText(selected)],{type:'text/markdown;charset=utf-8'});
  const url=URL.createObjectURL(blob), a=document.createElement('a');
  a.href=url;a.download=`taskworker-conversation-${selected.slice(0,8)}.md`;a.click();
  URL.revokeObjectURL(url);
};
$('cancel').onclick=()=>{if (selected && connected) void control('jobs/'+selected+'/cancel');};
$('queue-toggle').onclick=()=>{if (connected) void control('queue/'+(projection.queue.paused?'resume':'pause'));};
$('latest').onclick=()=>{$('output').scrollTop=$('output').scrollHeight;};
render();void models();void synchronize();

