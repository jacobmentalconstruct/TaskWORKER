// DOM-free adapter orchestration tests complement, never replace, real browser evidence.
import test from 'node:test';
import assert from 'node:assert/strict';
class Element {
 constructor(){this.value='';this.textContent='';this.hidden=false;this.disabled=false;this.children=[];this.scrollTop=0;this.open=false;}
 append(e){this.children.push(e);}
 replaceChildren(){this.children=[];}
 setAttribute(k,v){this[k]=v;}
 remove(){}
 focus(){}
}
const elements=new Map();globalThis.document={getElementById:id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);},createElement:()=>new Element()};
const saved=new Map();globalThis.sessionStorage={getItem:k=>saved.get(k)??null,setItem:(k,v)=>saved.set(k,v),removeItem:k=>saved.delete(k)};
let stop,show;globalThis.addEventListener=(name,fn)=>{if(name==='pagehide')stop=fn;if(name==='pageshow')show=fn;};
const store='a'.repeat(32),id='b'.repeat(32),c=n=>({store_id:store,sequence:String(n)});
const q={paused:true,pending:[],max_pending:64};
const job=text=>({id,state:'succeeded',request:{model:'fake',prompt:'original',options:{max_output_tokens:10}},instructions:{system:'',prompt:'original'},result:{text,context:{},usage:{}}});
const streams=[],creates=[],paths=[];
let controller;
globalThis.fetch=async(path,options)=>{
 paths.push(path);
 if(path==='/v1/models') return new Response(JSON.stringify([{id:'fake',capabilities:{text_generation:true},context:{}}]));
 if(path.startsWith('/v1/events')) {
  const body=new ReadableStream({start(cn){controller=cn;streams.push(cn);options.signal.addEventListener('abort',()=>{try{cn.error(new DOMException('Aborted','AbortError'));}catch{}});}});
  return new Response(body,{headers:{'Content-Type':'text/event-stream'}});
 }
 return new Promise(resolve=>creates.push({path,body:options.body,resolve}));
};
const delay=ms=>new Promise(r=>setTimeout(r,ms));
function frame(kind,data){controller.enqueue(new TextEncoder().encode(`id: ${data.cursor.store_id}:${data.cursor.sequence}\nevent: ${kind}\ndata: ${JSON.stringify(data)}\n\n`));}
test('actual adapter ignores stale mutation response, latches double click, reconnects at last applied cursor',async(t)=>{
 t.after(()=>stop?.());
 await import('./app.js');await delay(20);
 frame('snapshot',{cursor:c('9007199254740993'),queue:q,jobs:[]});await delay(100);
 elements.get('model').value='fake';document.getElementById('budget').value='10';document.getElementById('prompt').value='original';
 elements.get('draft').onsubmit({preventDefault(){}});elements.get('draft').onsubmit({preventDefault(){}});
 assert.equal(creates.length,1);const original=creates[0].body;assert.equal(saved.size,1);
 frame('event',{cursor:c('9007199254740994'),kind:'job.accepted',job_id:id,job:job('latest event text')});await delay(100);
 creates[0].resolve(new Response(JSON.stringify(job('stale response'))));await delay(100);
 assert.equal(elements.get('output').textContent,'latest event text');assert.equal(saved.size,0);
 elements.get('draft').onsubmit({preventDefault(){}});assert.equal(creates.length,1);assert.equal(elements.get('submit').disabled,true);
 controller.close();await delay(1250);
 assert.equal(streams.length,2);assert.equal(paths.at(-1),'/v1/events?after='+encodeURIComponent(store+':9007199254740994'));
 assert.equal(JSON.parse(original).request.prompt,'original');
 frame('event',{cursor:c('9007199254740996'),kind:'queue.state',queue:q});await delay(2250);
 assert.equal(paths.at(-1),'/v1/events');
 frame('snapshot',{cursor:{store_id:'f'.repeat(32),sequence:'0'},queue:q,jobs:[]});await delay(100);
 assert.equal(elements.get('detail-title').textContent,'Select a Job');assert.equal(elements.get('output').textContent,'');
 stop();await delay(30);const before=streams.length;show({persisted:true});await delay(30);assert.equal(streams.length,before+1);assert.equal(paths.filter(p=>p.startsWith('/v1/events')).at(-1),'/v1/events');stop();await delay(30);
});



