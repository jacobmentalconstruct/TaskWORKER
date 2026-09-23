import test from 'node:test';
import assert from 'node:assert/strict';
import {Projection,Recovery,cursor,integer,parse,stream,ordinals,sortJobs,updatedAt,faultText,draftFromRequest,loadDefaultModel,saveDefaultModel,clearDefaultModel,loadRoleLibrary,addRole,removeRole,libraryLabel,conversationTurns,loadArchived,archiveJob,unarchiveJob} from './state.js';
const store='a'.repeat(32), id='b'.repeat(32);
const c=n=>({store_id:store,sequence:String(n)});
const j=text=>({id,state:'running',request:{model:'fake',prompt:'hi',options:{max_output_tokens:20}},instructions:{system:'',prompt:'hi'},result:{text,context:{},usage:{}}});
const q={paused:false,pending:[],max_pending:64};
function initial(n='9007199254740993',text='é😀') {const p=new Projection();p.snapshot({cursor:c(n),queue:q,jobs:[j(text)]},cursor(c(n)));return p;}
function output(n,offset,text) {return {cursor:c(n),kind:'job.output',job_id:id,output:{offset_bytes:offset,text}};}
function apply(p,e) {return p.event(e,cursor(e.cursor));}
test('uint64 exact beyond MAX_SAFE_INTEGER, UTF-8 offsets, duplicate and replacement',()=>{
 const p=initial();const e=output('9007199254740994',6,'漢');apply(p,e);assert.equal(p.jobs.get(id).result.text,'é😀漢');assert.equal(apply(p,e),false);
 apply(p,{cursor:c('9007199254740995'),kind:'job.result',job_id:id,job:j('é😀漢')});assert.equal(p.jobs.get(id).result.text,'é😀漢');assert.equal(p.bytes.get(id),9);
});
test('invalid event never advances cursor or mutates state',()=>{for(const e of [output('9007199254740995',6,'x'),output('9007199254740994',3,'x'),{cursor:c('9007199254740994'),kind:'nope'}, {...output('9007199254740994',6,'x'),cursor:{store_id:'f'.repeat(32),sequence:'9007199254740994'}}]) {const p=initial();assert.throws(()=>apply(p,e));assert.equal(p.cursor.sequence,'9007199254740993');assert.equal(p.jobs.get(id).result.text,'é😀');}});
test('snapshot is atomic, replaces store and retained output',()=>{const p=initial();assert.throws(()=>p.snapshot({cursor:c(1),queue:null,jobs:[]},cursor(c(1))));assert.equal(p.jobs.size,1);const s={cursor:{store_id:'f'.repeat(32),sequence:'0'},queue:q,jobs:[]};p.snapshot(s,cursor(s.cursor));assert.equal(p.jobs.size,0);assert.equal(p.cursor.store_id,s.cursor.store_id);});
test('cursor canonical uint64 boundary and frame identity',()=>{for(const sequence of ['01','-1','18446744073709551616',1]) assert.throws(()=>cursor({store_id:store,sequence}));assert.equal(cursor(c('18446744073709551615')),store+':18446744073709551615');assert.throws(()=>initial().event(output('9007199254740994',6,'x'),store+':1'));});
test('exact large metadata and explicit UI integer limits',()=>{assert.equal(parse('{"seed":9223372036854775807}').seed,'9223372036854775807');assert.throws(()=>integer('9007199254740993',1));assert.throws(()=>integer('4294967296',0,4294967295));assert.equal(integer('4294967295',0,4294967295),4294967295);});
test('float64 temperature remains numeric while numeric cursor sequences fail',()=>{assert.equal(parse('{"temperature":1e20}').temperature,1e20);assert.throws(()=>parse('{"sequence":9007199254740993}'));});
function storage() {const data=new Map();return {getItem:k=>data.get(k)??null,setItem:(k,v)=>data.set(k,v),removeItem:k=>data.delete(k)};}
test('uncertain acceptance survives reload with exact operation origin command and key',()=>{const s=storage(),r=new Recovery(s);const cmd={idempotency_key:'fixed-key',origin:'browser',request:{prompt:'original\n😀'}};const saved=r.save('submit',cmd,store);cmd.request.prompt='edited draft';assert.throws(()=>r.save('submit',cmd,store));const reload=new Recovery(s);assert.deepEqual(reload.pending,saved);assert.equal(JSON.parse(reload.pending.body).request.prompt,'original\n😀');reload.clear();assert.equal(new Recovery(s).pending,null);});
test('storage quota blocks sending and corrupt recovery fails closed',()=>{assert.throws(()=>new Recovery({getItem:()=>'{broken'}));const r=new Recovery({getItem:()=>null,setItem:()=>{throw Error('quota');}});assert.throws(()=>r.save('retry',{idempotency_key:'x',parent_id:id},store));});
function response(frames){return new Response(new ReadableStream({start(controller){for(const f of frames)controller.enqueue(new TextEncoder().encode(f));controller.close();}}),{headers:{'Content-Type':'text/event-stream'}});}
const frame=e=>`id: ${cursor(e.cursor)}\nevent: event\ndata: ${JSON.stringify(e)}\n\n`;
test('split stream, replay after EOF, duplicates and later events',async()=>{const p=initial();const e=output('9007199254740994',6,'漢'),f=frame(e);const applyFrame=(kind,data,id)=>p.event(data,id);await assert.rejects(stream(response([f.slice(0,10),f.slice(10)]),applyFrame,new AbortController().signal),/disconnected/);await assert.rejects(stream(response([frame(e),frame(output('9007199254740995',9,'!'))]),applyFrame,new AbortController().signal),/disconnected/);assert.equal(p.jobs.get(id).result.text,'é😀漢!');});
test('post-header fault and invalid SSE detected',async()=>{await assert.rejects(stream(response(['event: error\ndata: {"code":"cursor_expired"}\n\n']),()=>{},new AbortController().signal),e=>e.code==='cursor_expired');await assert.rejects(stream(response(['event: event\ndata: {}\ndata: {}\n\n']),()=>{},new AbortController().signal));});
const at=(n,extra={})=>({id:String(n).repeat(32),created_at:`2026-09-21T10:0${n}:00Z`,...extra});
test('job list sorts by created or last updated, in either direction, with stable chronological numbers',()=>{
 const a=at(1,{finished_at:'2026-09-21T10:09:00Z'}),b=at(2,{started_at:'2026-09-21T10:05:00Z'}),d=at(3);
 const ids=(k,desc)=>sortJobs([b,d,a],k,desc).map(x=>x.id[0]);
 assert.deepEqual(ids('created',false),['1','2','3']);assert.deepEqual(ids('created',true),['3','2','1']);
 assert.deepEqual(ids('updated',false),['3','2','1']);assert.deepEqual(ids('updated',true),['1','2','3']);
 assert.equal(updatedAt(a),a.finished_at);assert.equal(updatedAt(b),b.started_at);assert.equal(updatedAt(d),d.created_at);
 const n=ordinals([b,d,a]);assert.deepEqual([a,b,d].map(x=>n.get(x.id)),[1,2,3]);
 const tie=[{id:'b'.repeat(32),created_at:'2026-09-21T10:00:00Z'},{id:'a'.repeat(32),created_at:'2026-09-21T10:00:00Z'}];
 assert.equal(sortJobs(tie,'created',false)[0].id[0],'a');
 assert.equal(sortJobs([{id:'x',created_at:'2026-09-21T10:00:00+02:00'},{id:'y',created_at:'2026-09-21T09:30:00Z'}],'created',false)[0].id,'x');
});
test('a user cancel is worded as such and other faults keep their code',()=>{assert.equal(faultText('cancelled'),'User cancelled');assert.equal(faultText('backend_error'),'Job fault: backend_error');});
test("a job's original request reduces to compose-form strings, never its prompt",()=>{
 const full=draftFromRequest({model:'m',role:'r',system_prompt:'s',prompt:'ignored',options:{max_output_tokens:64,context_tokens:2048,temperature:0.5,seed:7}});
 assert.deepEqual(full,{model:'m',role:'r',system:'s',budget:'64',context:'2048',temperature:'0.5',seed:'7'});
 const bare=draftFromRequest({model:'m',prompt:'x',options:{max_output_tokens:10}});
 assert.deepEqual(bare,{model:'m',role:'',system:'',budget:'10',context:'',temperature:'',seed:''});
});
test('a default model is remembered across a session and a blocked store fails silently',()=>{
 const store=new Map();
 const storage={getItem:k=>store.has(k)?store.get(k):null,setItem:(k,v)=>store.set(k,v),removeItem:k=>store.delete(k)};
 assert.equal(loadDefaultModel(storage),null);
 saveDefaultModel(storage,'qwen2.5:7b');assert.equal(loadDefaultModel(storage),'qwen2.5:7b');
 clearDefaultModel(storage);assert.equal(loadDefaultModel(storage),null);
 const broken={getItem(){throw Error('blocked');},setItem(){throw Error('blocked');},removeItem(){throw Error('blocked');}};
 assert.equal(loadDefaultModel(broken),null);saveDefaultModel(broken,'x');clearDefaultModel(broken);
});
function memoryStorage() {const m=new Map();return {getItem:k=>m.has(k)?m.get(k):null,setItem:(k,v)=>m.set(k,v),removeItem:k=>m.delete(k)};}
test('a saved-text library dedupes, moves a re-save to the end, and caps size',()=>{
 const storage=memoryStorage();
 assert.deepEqual(loadRoleLibrary(storage),[]);
 addRole(storage,'  Be concise.  ');addRole(storage,'Write like a pirate.');
 assert.deepEqual(loadRoleLibrary(storage),['Be concise.','Write like a pirate.']);
 addRole(storage,'Be concise.');
 assert.deepEqual(loadRoleLibrary(storage),['Write like a pirate.','Be concise.']);
 addRole(storage,'   ');assert.deepEqual(loadRoleLibrary(storage),['Write like a pirate.','Be concise.']);
 const list=removeRole(storage,'Write like a pirate.');
 assert.deepEqual(list,['Be concise.']);assert.deepEqual(loadRoleLibrary(storage),['Be concise.']);
 for (let i=0;i<105;i++) addRole(storage,'entry '+i);
 const capped=loadRoleLibrary(storage);
 assert.equal(capped.length,100);assert.equal(capped[0],'entry 5');assert.equal(capped.at(-1),'entry 104');
});
test('a corrupt or blocked library store is treated as empty, never throws',()=>{
 assert.deepEqual(loadRoleLibrary({getItem:()=>'not json'}),[]);
 assert.deepEqual(loadRoleLibrary({getItem:()=>'{"not":"a list"}'}),[]);
 const broken={getItem(){throw Error('blocked');},setItem(){throw Error('blocked');}};
 assert.deepEqual(loadRoleLibrary(broken),[]);
 addRole(broken,'x');removeRole(broken,'x'); // the save/remove itself never throws; persistence just silently fails
 assert.deepEqual(loadRoleLibrary(broken),[]);
});
test('a library label is the first six words or 60 characters, whichever is shorter',()=>{
 assert.equal(libraryLabel('  '),'(empty)');
 assert.equal(libraryLabel('Be concise.'),'Be concise.');
 assert.equal(libraryLabel('One two three four five six seven eight'),'One two three four five six…');
 assert.equal(libraryLabel('x'.repeat(90)),'x'.repeat(60)+'…');
});
test('conversation turns walk branch ancestry oldest-first, stepping through retry hops without counting them',()=>{
 const a={id:'a'.repeat(32)}, b={id:'b'.repeat(32),lineage:{parent_id:a.id,relation:'branch'}};
 const c={id:'c'.repeat(32),lineage:{parent_id:b.id,relation:'retry'}};
 const jobs=new Map([[a.id,a],[b.id,b],[c.id,c]]);
 assert.deepEqual(conversationTurns(jobs,c.id).map(j=>j.id),[a.id,c.id]); // retry of b: b itself is not a turn
 assert.deepEqual(conversationTurns(jobs,b.id).map(j=>j.id),[a.id,b.id]);
 assert.deepEqual(conversationTurns(jobs,a.id).map(j=>j.id),[a.id]);
 assert.deepEqual(conversationTurns(jobs,'missing'),[]);
 assert.deepEqual(conversationTurns(new Map([[b.id,b]]),b.id).map(j=>j.id),[b.id]); // parent evicted: shorter, not wrong
});
test('archiving is a membership set, tolerant of a corrupt or blocked store',()=>{
 const storage=memoryStorage(), id='d'.repeat(32);
 assert.deepEqual(loadArchived(storage),new Set());
 archiveJob(storage,id);assert.deepEqual(loadArchived(storage),new Set([id]));
 archiveJob(storage,id);assert.deepEqual(loadArchived(storage),new Set([id])); // re-archiving is a no-op, not a duplicate
 unarchiveJob(storage,id);assert.deepEqual(loadArchived(storage),new Set());
 assert.deepEqual(loadArchived({getItem:()=>'not json'}),new Set());
 assert.deepEqual(loadArchived({getItem:()=>'["not-a-valid-id"]'}),new Set());
 const broken={getItem(){throw Error('blocked');},setItem(){throw Error('blocked');}};
 archiveJob(broken,id);assert.deepEqual(loadArchived(broken),new Set());
});
