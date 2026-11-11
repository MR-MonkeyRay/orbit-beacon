import test from 'node:test';
import assert from 'node:assert/strict';
import {getNavigationSignal, setNavigationSignal} from '../src/console/api.js';
import {dataOperationsTab} from '../src/console/pages/data-operations.js';
import {copy, elements, flush, installDOM} from './console-dom.js';

test('task and plan pagers retain independent cursors and terminal-button behavior', async () => {
  const dom = installDOM();
  const fetch = globalThis.fetch;
  const navigation = getNavigationSignal();
  const lifetime = new AbortController();
  const calls = [];
  let tab;
  setNavigationSignal(lifetime.signal);
  globalThis.fetch = async (url) => {
    calls.push(url);
    const path = new URL(url, 'https://console.example').pathname;
    const plans = path.endsWith('/backup-plans');
    const continuation = new URL(url, 'https://console.example').searchParams.get('before');
    return new Response(JSON.stringify({
      [plans ? 'plans' : 'tasks']: [], has_more: true,
      // 缺少后续游标时保留已加载的页面，并禁用“下一页”。
      ...(!continuation ? {next_cursor: plans ? 'plan/+' : 'task/+'} : {}),
      timezone_name: 'UTC', utc_offset_seconds: 0
    }), {headers: {'Content-Type': 'application/json'}});
  };
  try {
    tab = dataOperationsTab();
    dom.body.append(tab.element);
    const categories = elements(tab.element).filter((element) => element.name === 'categories');
    assert.equal(categories.length, 3);
    assert.equal(elements(tab.element).some((element) => element.textContent === copy('data.management.compact')), false, 'compaction belongs to storage, not backup operations');
    assert.deepEqual(categories.map((element) => element.value), ['configuration', 'monitoring', 'events']);
    for (const category of categories) {
      assert.equal(category.classList.contains('input'), false);
      assert.equal(category.parentElement.tagName, 'label');
      assert.equal(category.parentElement.className, 'check-row');
      assert.equal(category.parentElement.children[0], category);
      const hint = category.parentElement.querySelector('small');
      assert.equal(hint?.textContent ?? null, category.value === 'events' ? copy('data.category.eventsHint') : null);
      category.checked = false;
      category.dispatchEvent(new Event('change'));
    }
    assert.equal(categories[0].validationMessage, copy('data.category.required'));
    categories[1].checked = true;
    categories[1].dispatchEvent(new Event('change'));
    assert.equal(categories[0].validationMessage, '');
    tab.attach();
    await flush();
    const find = (label) => elements(tab.element).find((element) => element.tagName === 'button' && element.textContent === label);
    const taskNext = find(copy('data.tasks.older'));
    const planNext = find(copy('data.plans.older'));
    assert.equal(taskNext.disabled, false);
    assert.equal(planNext.disabled, false);
    taskNext.click();
    await flush();
    assert.equal(calls.at(-1), '/api/v1/tasks?limit=20&before=task%2F%2B');
    assert.equal(taskNext.disabled, true);
    assert.equal(planNext.disabled, false);
    assert.equal(find(copy('data.tasks.newer')).disabled, false);
    assert.equal(tab.element.querySelector('.data-stale'), null);
    planNext.click();
    await flush();
    assert.equal(calls.at(-1), '/api/v1/backup-plans?limit=20&before=plan%2F%2B');
    assert.equal(planNext.disabled, true);
    assert.equal(tab.element.querySelector('.data-stale'), null);
    find(copy('data.tasks.newer')).click();
    await flush();
    assert.equal(calls.at(-1), '/api/v1/tasks?limit=20');
    assert.equal(find(copy('data.tasks.newer')).disabled, true);
    assert.equal(taskNext.disabled, false);
    tab.detach();
    const detachedCalls = calls.length;
    taskNext.click();
    await flush();
    assert.equal(calls.length, detachedCalls);
  } finally {
    tab?.detach();
    lifetime.abort();
    setNavigationSignal(navigation);
    globalThis.fetch = fetch;
    dom.restore();
  }
});

test('settings task deep link still opens the requested task independently of the task list', async () => {
  const dom = installDOM();
  const originals = new Map(['window', 'fetch', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  globalThis.window = new EventTarget();
  const frames = []; globalThis.requestAnimationFrame = (callback) => { frames.push(callback); };
  const {reducedMotion} = await import('../src/shared/dom.js');
  const motion = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => true});
  const scope = new AbortController(); setNavigationSignal(scope.signal);
  const id = '00000000-0000-4000-8000-000000000099';
  Object.assign(location, {pathname: '/settings', search: `?task=${id}`, hash: '#data'});
  const calls = [];
  globalThis.fetch = async (url) => {
    calls.push(url);
    if (url === `/api/v1/tasks/${id}`) return Response.json({id, kind: 'backup_export', categories: ['configuration'], state: 'failed', error_code: 'interrupted', destination: {}, created_at: '2026-01-01T00:00:00Z'});
    if (url.includes('/tasks?')) return Response.json({tasks: [], has_more: false});
    if (url.includes('/backup-plans?')) return Response.json({plans: [], has_more: false});
    return Response.json({before: '2026-01-01T00:00:00Z', databases: [], categories: []});
  };
  try {
    const {settingsPage} = await import('../src/console/pages/settings.js');
    const page = await settingsPage(); dom.body.append(page); dom.mutations();
    frames.shift()(); await flush();
    assert.equal(page.querySelector('#tab-data').getAttribute('aria-selected'), 'true');
    assert.ok(calls.includes(`/api/v1/tasks/${id}`));
    assert.ok(document.querySelector('dialog').textContent.includes(id));
    assert.equal(location.hash, '#data');
  } finally {
    scope.abort(); setNavigationSignal(null); await flush();
    Object.defineProperty(reducedMotion, 'matches', motion); dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});
