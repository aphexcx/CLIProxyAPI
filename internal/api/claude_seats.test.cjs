const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const script = fs.readFileSync(path.join(__dirname, 'claude_seats.html'), 'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
function page(meter = { rule_enabled: true, since: '2026-11-26T19:25:44Z', days: [] }) {
  const element = tag => ({
    tag,
    children: [], hidden: false, value: '', textContent: '',
    append(child) { this.children.push(child); },
    replaceChildren() { this.children = []; }, focus() {}, setAttribute() {}
  });
  const elements = Object.fromEntries(['login', 'key', 'logout', 'seats', 'status', 'cache'].map(id => [id, element()]));
  const timers = new Map();
  let nextTimer = 0, responseStatus = 200, resetStatus = 200, calls = 0;
  const context = vm.createContext({
    document: { getElementById: id => elements[id], createElement: element },
    setInterval(fn) { timers.set(++nextTimer, fn); return nextTimer; },
    clearInterval(id) { timers.delete(id); },
    fetch: async path => {
      calls++;
      const resets = path.endsWith('claude/limit-resets');
      if (responseStatus === 200 && path.endsWith('observability/claude-cache')) return { ok: true, status: 200, json: async () => meter };
      const code = responseStatus !== 200 ? responseStatus : resets ? resetStatus : 200;
      return { ok: code === 200, status: code, json: async () => resets ? { seats: [{ name: 'Seat', credential_generation: '1:1', stale: false, age_seconds: 12, best_click_rank: 1, rank_reason: 'holder action only', cedar_ember: { eligible: true, grants: [{ id: 'fixture', resets_left: 2, usable_now: true }] } }] } : { files: [{ provider: 'claude', name: 'Seat', credential_generation: '1:1', auth_index: 'seat', claude_usage: { windows: {}, holds: [] } }] } };
    }
  });
  vm.runInContext(script, context);
  return { elements, timers, context, get calls() { return calls; }, reject(status) { responseStatus = status; }, failReset(status) { resetStatus = status; } };
}
const settle = () => new Promise(resolve => setImmediate(resolve));

test('recorded surface response is unknown rather than zero and has no rank', async () => {
  const p = page();
  p.elements.key.value = 'fixture-key';
  p.elements.login.onsubmit({ preventDefault() {} });
  await settle();
  p.context.surface = JSON.parse(fs.readFileSync(path.join(__dirname, '../auth/claude/testdata/limit_resets_surface.json'), 'utf8')).cedar_ember;
  vm.runInContext('render(lastFiles, {seats: lastResets.seats.map(seat => ({...seat, cedar_ember: surface}))})', p.context);
  const text = element => element.textContent + element.children.map(text).join(' ');
  assert.match(text(p.elements.seats), /Resets: —/);
  assert.match(text(p.elements.seats), /Not visible on this surface/);
  assert.doesNotMatch(text(p.elements.seats), /Resets: 0|Best click/);
});

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

const cacheMeter = (write5m, clients) => ({
  rule_enabled: true, since: '2026-11-26T19:25:44Z',
  days: [{ day: '2026-11-26', main_clients_with_5m_writes: clients.length,
    totals: { main: { requests: 9, write_5m: write5m, write_1h: 5000, write_unsplit: 0, read: 70000 } },
    rule: { applied: 9, refused: 0 }, clients }]
});
const flatten = element => element.textContent + ' ' + element.children.map(flatten).join(' ');
const tags = (element, tag) => (element.tag === tag ? 1 : 0) + element.children.reduce((sum, child) => sum + tags(child, tag), 0);

test('cache section lists main clients that wrote at five minutes', async () => {
  const p = page(cacheMeter(1200, [
    { client: '1a2b3c4d5e6f', kind: 'main', requests: 3, write_5m: 1200, write_1h: 0, read: 10, last_5m_write_at: '2026-11-26T20:00:00Z' },
    { client: 'ffffffffffff', kind: 'subagent', requests: 4, write_5m: 800, write_1h: 0, read: 0, last_5m_write_at: '2026-11-26T20:00:00Z' }
  ]));
  p.elements.key.value = 'fixture-key';
  p.elements.login.onsubmit({ preventDefault() {} });
  await settle();
  const text = flatten(p.elements.cache);
  assert.match(text, /written at one hour 5,000/);
  assert.match(text, /written at five minutes 1,200/);
  assert.match(text, /read 70,000/);
  assert.equal(tags(p.elements.cache, 'table'), 1);
  assert.match(text, /1a2b3c4d5e6f/);
  assert.doesNotMatch(text, /ffffffffffff/, 'a subagent is not a main conversation');
});

test('cache section shows no table when no main conversation wrote at five minutes', async () => {
  const p = page(cacheMeter(0, []));
  p.elements.key.value = 'fixture-key';
  p.elements.login.onsubmit({ preventDefault() {} });
  await settle();
  const text = flatten(p.elements.cache);
  assert.match(text, /written at five minutes 0/);
  assert.equal(tags(p.elements.cache, 'table'), 0);
});
