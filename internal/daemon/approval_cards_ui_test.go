package daemon

import (
	"os/exec"
	"testing"
)

// Exercise the actual shared renderer and orb collapse timer with a small DOM.
func TestApprovalCardRenderer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is needed for the approval card renderer test")
	}
	out, err := exec.Command(node, "-e", `
const fs = require('fs'), assert = require('assert');
const html = fs.readFileSync('../api/ui.html', 'utf8');
let sent = '', streaming = false, composerOpened = false, notice = '', refreshed = 0;
let send = text => { sent = text; };
const may = () => true, refreshNeeds = () => { refreshed++; };
const showComposer = () => { composerOpened = true; }, toast = text => { notice = text; };
const esc = s => String(s).replaceAll('<','&lt;'), ago = () => '', channelName = () => '', nm = () => 'Mirrin';
function element() {
 return {className:'', children:[], elements:{}, firstChild:{},
  set innerHTML(value) { this.html=value; this.children=[]; this.elements={}; }, get innerHTML(){return this.html || '';},
  set textContent(value) { this.text=value; this.children=[]; }, get textContent(){return this.text;},
  appendChild(child) {this.children.push(child); child.parent=this;},
  remove() {if(this.parent) this.parent.children=this.parent.children.filter(c=>c!==this);},
  get classList() {const el=this; return {
   add(...names){el.className += ' ' + names.join(' ');},
   remove(...names){el.className=el.className.split(' ').filter(n=>!names.includes(n)).join(' ');},
   contains(name){return el.className.split(' ').includes(name);},
   toggle(name, on){if(on===undefined) on=!this.contains(name); if(on) this.add(name); else this.remove(name); return on;}
  };},
  querySelector(selector) {
   const cls=selector.slice(1);
   const child=this.children.find(c=>c.classList.contains(cls)); if(child) return child;
   if(!this.innerHTML.includes('class="'+cls+'"')) return null;
   return this.elements[cls] ||= {};
  }
 };
}
const document = {createElement:element, body:element()};
const nodes = {}, $ = id => nodes[id] ||= element();
const seenNeeds = new Set(), firstRender = true;
let ORB = false, data = {}, shownState, bubbleTimer, collapseTimer, bOpen = false;
const busState = 'idle', STATE_LABEL = {idle:'Idle'};
const renderPill = () => {}, renderStateWords = () => {}, touch = () => {}, pausedNow = () => false, renderSayOnline = () => {};
const timers = new Map(); let timerID=0;
const setTimeout = fn => {timers.set(++timerID,fn); return timerID;};
const clearTimeout = id => timers.delete(id);
const needsCount = () => (data.approvals||[]).filter(a=>a.status==='pending').length;
eval(html.slice(html.indexOf('function applyState('), html.indexOf('function setState(')));
eval(html.slice(html.indexOf('function approvalCard('), html.indexOf('/* ---------- today')));
const pending = {id:7,summary:'Send it',tool:'send',risk:'write',status:'pending',always_allow:true};
const done = {...pending,status:'approved',by:'My <phone> (screen)'};
const question = {id:9,title:'Task',status:'waiting_user',question:'Which day?'};
renderNeeds({approvals:[done],tasks:[question]});
assert.equal($('#needN').textContent,1);
assert($('#needs').children[0].innerHTML.includes('Which day?'));
assert($('#needs').children[0].classList.contains('hero'));
assert(!$('#needs').children[1].classList.contains('hero'));
renderNeeds({approvals:[done,pending],tasks:[question]});
assert($('#needs').children[0].innerHTML.includes('Always allow'));
assert($('#needs').children[1].innerHTML.includes('Which day?'));
renderNeeds({approvals:[done]});
assert.equal($('#needN').textContent,0);
assert.equal($('#hNeeds').firstChild.textContent,'Recent approvals ');
assert(!$('#needs').children[0].classList.contains('hero'));
for (const compact of [false,true]) {
 const card = approvalCard(pending,compact);
 assert(card.innerHTML.includes('write'));
 if(compact) { assert.equal(card.querySelector('.always'),null); }
 else {
  streaming=true; sent=''; card.querySelector('.always').onclick();
  assert.equal(sent,''); assert(notice.includes('Please wait'));
  streaming=false; card.querySelector('.always').onclick();
  // the wire words stay; the card asks once more instead of opening the text box
  assert.equal(sent,'yes 7, always'); assert(!composerOpened);
  assert(card.children.some(c=>c.classList.contains('confirmrow')));
  assert(card.classList.contains('confirming'));
  // a refresh keeps the confirm row
  assert(approvalCard(pending,false).children.some(c=>c.classList.contains('confirmrow')));
  const row = () => card.children.find(c=>c.classList.contains('confirmrow'));
  const pick = cls => row().children.find(c=>c.classList.contains(cls));
  // Keep asking says no to the twin's check and brings back Approve and Deny
  streaming=true; sent=''; pick('keep').onclick();
  assert.equal(sent,''); assert(card.classList.contains('confirming'));
  streaming=false; pick('keep').onclick();
  assert.equal(sent,'no'); assert(!card.classList.contains('confirming')); assert.equal(row(),undefined);
  assert(!approvalCard(pending,false).children.some(c=>c.classList.contains('confirmrow')));
  // Just this once answers the check too ("yes 7"), so a later "yes" can't stop the asking
  card.querySelector('.always').onclick(); assert(row());
  pick('once').onclick();
  assert.equal(sent,'yes 7'); assert(card.classList.contains('deciding'));
  assert(!approvalCard(pending,false).children.some(c=>c.classList.contains('confirmrow')));
  card.querySelector('.always').onclick(); pick('confirm').onclick();
  assert.equal(sent,'yes');
 }
 const floor = approvalCard({...pending,tool:'check_spend',risk:'dangerous',always_allow:false},compact);
 assert(floor.innerHTML.includes('dangerous')); assert.equal(floor.querySelector('.always'),null);
 const bare = approvalCard({...pending,tool:'check_spend',summary:'check_spend(amount=389)',details:[{name:'Amount',value:'389'},{name:'Purpose',value:'<b>x'}]},compact);
 assert(bare.innerHTML.includes('<b>Make a payment</b>')); assert(bare.innerHTML.includes('Amount:</span> 389')); assert(bare.innerHTML.includes('&lt;b>x')); assert(!bare.innerHTML.includes('check_spend(amount'));
 const resolved = approvalCard(done,compact);
 assert(resolved.innerHTML.includes('approved by My &lt;phone> (screen)'));
 assert.equal(resolved.querySelector('.yes'),null); assert.equal(resolved.querySelector('.always'),null);
 for(const [status,label] of [['expired','lapsed'],['superseded','replaced']]) {
  const card=approvalCard({...done,status,by:''},compact);
  assert(card.innerHTML.includes(label)); assert(!card.innerHTML.includes(status));
 }
}
// In the browser sheet, beside the page: no screenshot, no Always allow, no "via".
const inSheet = approvalCard({...pending,screenshot:'/screen/shot?path=x',chat:'whatsapp:1'},false,{});
assert(!inSheet.innerHTML.includes('class="shot"')); assert(!inSheet.innerHTML.includes('class="always"')); assert(!inSheet.innerHTML.includes('via '));
assert(inSheet.innerHTML.includes('class="yes"')); assert(inSheet.innerHTML.includes('class="no"'));
// A request about the page offers it, on a full card only.
assert(approvalCard({...pending,page:true},false).innerHTML.includes('class="apage"'));
assert(!approvalCard({...pending,page:true},true).innerHTML.includes('class="apage"'));
assert(!approvalCard({...pending,page:true},false,{}).innerHTML.includes('class="apage"'));
assert(!approvalCard(pending,false).innerHTML.includes('class="apage"'));
ORB=true;
for(const status of ['approved','denied','expired','superseded']) {
 data={approvals:[pending]}; renderNeeds(data);
 assert($('#bubble').classList.contains('show'));
 data={approvals:[{...done,status}]}; renderNeeds(data);
 assert(!$('#bubble').classList.contains('show'));
 assert(timers.has(collapseTimer));
 timers.get(collapseTimer)();
 assert(document.body.classList.contains('collapsed'));
 // A later refresh must not reopen a decided card or cancel collapse.
 const scheduled=collapseTimer; renderNeeds(data);
 assert(!$('#bubble').classList.contains('show')); assert.equal(collapseTimer,scheduled);
}
// A reply caption has its own hold timer: loading decisions leaves it alone.
$('#bubble').textContent='Done.'; $('#bubble').className='bubble show';
bubbleTimer=setTimeout(()=>{}); const held=bubbleTimer;
renderNeeds(data);
assert.equal($('#bubble').textContent,'Done.'); assert(timers.has(held));
// The page's own send, with the network and the transcript stubbed: fetch answers
// with the next of hows ('ok', 'refused' before the twin, 'down', or 'cut' mid-reply).
ORB=false;
const hows = [];
const realSend = (() => {
 const box = {value:'', focus(){}, blur(){}, setAttribute(){}, removeAttribute(){}, contains:()=>false, classList:{contains:()=>false}};
 const sayInput = box, composer = box, $ = () => box, LINES = [], CLIENT = 'page', online = true;
 const pushLine = () => ({}), showCaption = () => {}, announce = () => {}, revealLatest = () => {}, renderTranscript = () => {}, fadeCaption = () => {};
 const pinned = () => true, armHide = () => {}, load = () => {}, offlineText = () => 'offline', showUnpaired = () => {}, problem = async () => ({text:'refused'});
 const thanked = () => false, react = () => {};
 const fetch = async () => { const how = hows.shift(); if (how === 'down') throw new TypeError('down'); return {ok: how !== 'refused', status: how === 'refused' ? 503 : 200, body: how}; };
 async function* sse(body){ if (body !== 'cut') yield {event:'done', data:{reply:'Done.'}}; }
 eval(html.slice(html.indexOf('async function send('), html.indexOf("const composer = $('#composer')")));
 return send;
})();
let last; send = (text, opts) => { sent = text; return last = realSend(text, opts); };
const ask = id => approvalCard({...pending,id},false);
const hasRow = id => ask(id).children.some(c=>c.classList.contains('confirmrow'));
const answerRow = (id, cls) => { const c = ask(id); c.children.find(c=>c.classList.contains('confirmrow')).children.find(c=>c.classList.contains(cls)).onclick(); };
(async () => {
 // The twin takes one answer to its "stop asking?", whatever it is (alwaysallow.go):
 // a row it no longer holds would answer nothing, or deny what it asked about last.
 hows.push('ok'); ask(8).querySelector('.always').onclick(); assert(hasRow(8)); await last;
 assert.equal(sent,'yes 8, always'); assert(hasRow(8));
 // a composer send takes the twin's check, so the row goes
 hows.push('ok'); let r = await send('hello', {fromComposer:true});
 assert(r.ok && r.reached); assert(!hasRow(8));
 // words that never reached the twin leave it as it was
 hows.push('ok'); ask(8).querySelector('.always').onclick(); await last;
 hows.push('down'); r = await send('hello', {fromComposer:true});
 assert(!r.reached); assert(hasRow(8));
 // Always allow shows its row only while the question is with the twin
 for (const how of ['refused','down']) {
  hows.push(how); ask(9).querySelector('.always').onclick(); await last;
  assert(!hasRow(9), how); assert(hasRow(8), how);
 }
 // cut off mid-reply, it reached the twin: the old check is gone, and the new one may not be there
 hows.push('cut'); ask(9).querySelector('.always').onclick(); await last;
 assert(!hasRow(9)); assert(!hasRow(8));
 // so a second Always allow is the twin's one question: only its row stays
 hows.push('ok'); ask(8).querySelector('.always').onclick(); await last;
 hows.push('ok'); ask(9).querySelector('.always').onclick(); await last;
 assert(hasRow(9)); assert(!hasRow(8));
 // an answer that never reached the twin brings its row back, one that did doesn't
 const before = refreshed;
 hows.push('refused'); answerRow(9,'keep'); await last;
 assert.equal(sent,'no'); assert(hasRow(9)); assert(refreshed > before);
 hows.push('cut'); answerRow(9,'keep'); await last;
 assert(!hasRow(9));
})().catch(e => { console.error(e); process.exit(1); });
`).CombinedOutput()
	if err != nil {
		t.Fatalf("renderer: %v\n%s", err, out)
	}
}
