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
  let nextTimer = 0, responseStatus = 200, calls = 0;
  const context = vm.createContext({
    document: { getElementById: id => elements[id], createElement: element },
    setInterval(fn) { timers.set(++nextTimer, fn); return nextTimer; },
    clearInterval(id) { timers.delete(id); },
    fetch: async () => {
      calls++;
      return { ok: responseStatus === 200, status: responseStatus, json: async () => ({ files: [{ provider: 'claude', name: 'Seat', auth_index: 'seat', claude_usage: { windows: {}, holds: [] } }] }) };
    }
  });
  vm.runInContext(script, context);
  return { elements, timers, context, get calls() { return calls; }, reject(status) { responseStatus = status; } };
}
const settle = () => new Promise(resolve => setImmediate(resolve));

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
