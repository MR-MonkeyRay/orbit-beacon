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
