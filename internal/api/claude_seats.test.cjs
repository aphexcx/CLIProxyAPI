const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const script = fs.readFileSync(path.join(__dirname, 'claude_seats.html'), 'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
function page() {
  const element = () => ({
    children: [], hidden: false, value: '', textContent: '',
    append(child) { this.children.push(child); },
    replaceChildren() { this.children = []; }, focus() {}, setAttribute() {}
  });
  const elements = Object.fromEntries(['login', 'key', 'logout', 'seats', 'status'].map(id => [id, element()]));
  const timers = new Map();
  let nextTimer = 0, responseStatus = 200, resetStatus = 200, calls = 0;
  const context = vm.createContext({
    document: { getElementById: id => elements[id], createElement: element },
    setInterval(fn) { timers.set(++nextTimer, fn); return nextTimer; },
    clearInterval(id) { timers.delete(id); },
    fetch: async path => {
      calls++;
      const resets = path.endsWith('claude/limit-resets');
      const code = responseStatus !== 200 ? responseStatus : resets ? resetStatus : 200;
      return { ok: code === 200, status: code, json: async () => resets ? { seats: [{ name: 'Seat', credential_generation: '1:1', stale: false, age_seconds: 12, best_click_rank: 1, rank_reason: 'holder action only', cedar_ember: { eligible: true, grants: [{ id: 'fixture', resets_left: 2, usable_now: true }] } }] } : { files: [{ provider: 'claude', name: 'Seat', credential_generation: '1:1', auth_index: 'seat', claude_usage: { windows: {}, holds: [] } }] } };
    }
  });
  vm.runInContext(script, context);
  return { elements, timers, context, get calls() { return calls; }, reject(status) { responseStatus = status; }, failReset(status) { resetStatus = status; } };
}
const settle = () => new Promise(resolve => setImmediate(resolve));

test('same-name credential replacement hides the previous generation reset row', async () => {
  const p = page();
  p.elements.key.value = 'fixture-key';
  p.elements.login.onsubmit({ preventDefault() {} });
  await settle();
  vm.runInContext("render(lastFiles.map(file => ({ ...file, credential_generation: '1:2' })), lastResets)", p.context);
  const text = element => element.textContent + element.children.map(text).join(' ');
  assert.match(text(p.elements.seats), /Resets: —/);
  assert.doesNotMatch(text(p.elements.seats), /Best click|Resets: 2/);
});

test('a failed reset refresh retains counts, advances age and suppresses recommendations', async () => {
  const p = page();
  vm.runInContext('Date.now = () => 50000', p.context);
  p.elements.key.value = 'fixture-key';
  p.elements.login.onsubmit({ preventDefault() {} });
  await settle();
  const text = element => element.textContent + element.children.map(text).join(' ');
  assert.match(text(p.elements.seats), /Best click #1/);
  p.failReset(503);
  vm.runInContext('Date.now = () => 140000', p.context);
  await vm.runInContext('load()', p.context);
  const retained = text(p.elements.seats);
  assert.match(retained, /Resets: 2/);
  assert.match(retained, /Reset status unavailable/);
  assert.match(retained, /102s ago · stale/);
  assert.match(retained, /not usable now/);
  assert.doesNotMatch(retained, /Best click/);
});

for (const status of [401, 403]) {
  for (const stage of ['login', 'timer', 'refresh']) {
    test(`${status} during ${stage} locks the view and stops automatic retries`, async () => {
      const p = page();
      if (stage === 'login') p.reject(status);
      p.elements.key.value = 'fixture-key';
      p.elements.login.onsubmit({ preventDefault() {} });
      await settle();
      if (stage !== 'login') {
        p.reject(status);
        if (stage === 'timer') await [...p.timers.values()][0]();
        else await p.elements.seats.children[0].children.find(child => child.onclick).onclick();
      }
      await settle();
      const rejectedCalls = p.calls;
      for (const tick of p.timers.values()) await tick();
      await vm.runInContext('load()', p.context);
      assert.equal(p.calls, rejectedCalls, 'rejected key was retried');
      assert.equal(p.timers.size, 0);
      assert.equal(vm.runInContext('key', p.context), '');
      assert.equal(p.elements.key.value, '');
      assert.equal(p.elements.login.hidden, false);
      assert.equal(p.elements.logout.hidden, true);
      assert.equal(p.elements.seats.children.length, 0);
      assert.match(p.elements.status.textContent, /key|access/i);
    });
  }
}
