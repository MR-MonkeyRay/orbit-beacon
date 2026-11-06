import test from 'node:test';
import assert from 'node:assert/strict';
import {validResponse, validAnnouncements} from '../src/public/validate.js';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, elements, flush, installDOM} from './console-dom.js';

const id = '00000000-0000-4000-8000-000000000001';
const entry = {level: 'warning', title: '<img onerror=alert(1)>', body: '<script>bad</script>\nhttps://example.test', starts_at: '2030-01-01T00:00:00Z', ends_at: null};
const context = () => ({signal: new AbortController().signal, saved: async () => {}});
const control = (root, name) => elements(root).find((element) => element.name === name);
const admin = () => { setNavigationSignal(null); setSession({csrf_token: 'csrf', user: {id, username: 'admin', must_change_password: false}, expires_at: ''}); };

test('announcement validator enforces exact projection and retains previous valid response', () => {
  assert.equal(validAnnouncements([entry]), true);
  for (const value of [undefined, null, {}, Array(6).fill(entry), [null], [{...entry, id}], [{...entry, level: 'bad'}], [{...entry, title: ''}], [{...entry, body: ' '}], [{...entry, starts_at: 'bad'}], [{...entry, ends_at: false}], [Object.fromEntries(Object.entries(entry).filter(([key]) => key !== 'ends_at'))]]) assert.equal(validAnnouncements(value), false);
  const page = {announcements: [entry], nodes: [], has_more: false, next_offset: null, generated_at: '2030-01-01T00:00:00Z'};
  let retained = page;
  for (const announcements of [null, [{...entry, id}], [{...entry, level: 'bad'}]]) {
    const incoming = {...page, announcements};
    if (validResponse(incoming)) retained = incoming;
    assert.equal(retained, page);
  }
  assert.equal(validResponse(page, 10), true);
});

test('public announcements render plain text, preserve unchanged DOM and hide empty content', async () => {
  const dom = installDOM();
  try {
    const {announcementRenderer} = await import('../src/public/announcements.js');
    const container = document.createElement('section'); dom.body.append(container);
    const render = announcementRenderer(container);
    render([entry]); const article = container.children[0]; const title = article.querySelector('h2');
    assert.equal(container.hidden, false);
    assert.equal(title.textContent, entry.title);
    assert.equal(article.querySelector('.announcement-body').textContent, entry.body);
    assert.ok(article.textContent.includes(copy('alerts.severity.warning')));
    assert.equal(article.querySelector('img'), null); assert.equal(article.querySelector('script'), null); assert.equal(article.querySelector('a'), null);
    assert.equal(container.getAttribute('aria-live'), null);
    let replacements = 0; const replace = container.replaceChildren.bind(container); container.replaceChildren = (...nodes) => { replacements++; replace(...nodes); };
    render([{...entry}]); assert.equal(replacements, 0); assert.equal(container.children[0], article); assert.equal(article.querySelector('h2'), title);
    render([entry, {...entry, title: 'other'}]); assert.equal(container.children[0], article);
    render([{...entry, body: 'changed'}]); assert.notEqual(container.children[0], article);
    render([]); assert.equal(container.hidden, true); assert.equal(container.children.length, 0);
  } finally { dom.restore(); }
});

test('announcement console list, edit prefill and clear end submits null', async () => {
  const dom = installDOM(), original = globalThis.fetch; admin();
  const requests = [];
  globalThis.fetch = async (url, options) => { requests.push({url, ...options}); return new Response(JSON.stringify({id})); };
  try {
    const {announcementList, announcementEditor} = await import('../src/console/pages/settings-announcements.js');
    const item = {...entry, id, title: 'Maintenance', body: 'Content', ends_at: '2030-01-01T02:00:00Z', active: true};
    assert.ok(announcementList([], context()).textContent.includes(copy('announcements.empty')));
    const list = announcementList([item, {...item, active: false}], context()); dom.body.append(list);
    for (const key of ['announcements.title', 'announcements.level', 'announcements.heading', 'alerts.silences.active', 'alerts.silences.scheduled']) assert.ok(list.textContent.includes(copy(key)));
    const dialog = announcementEditor(item, context());
    assert.equal(control(dom.body, 'title').value, item.title); assert.equal(control(dom.body, 'body').value, item.body); assert.equal(control(dom.body, 'level').value, item.level);
    assert.equal(new Date(control(dom.body, 'starts_at').value).toISOString(), new Date(item.starts_at).toISOString());
    assert.equal(new Date(control(dom.body, 'ends_at').value).toISOString(), new Date(item.ends_at).toISOString());
    control(dom.body, 'ends_at').value = ''; control(dom.body, 'title').value = 'Updated';
    elements(dom.body).find((element) => element.tagName === 'form').dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(requests.length, 1); assert.equal(requests[0].method, 'PATCH'); assert.equal(JSON.parse(requests[0].body).ends_at, null);
    await dialog.closed;
  } finally { globalThis.fetch = original; dom.restore(); }
});

test('announcement add, validation, audit labels and delete cancel', async () => {
  const dom = installDOM(), original = globalThis.fetch; admin(); let requests = 0, payload;
  globalThis.fetch = async (_url, options) => { requests++; payload = JSON.parse(options.body || '{}'); return new Response(JSON.stringify({id})); };
  try {
    const {announcementList, announcementEditor} = await import('../src/console/pages/settings-announcements.js');
    const {describeAction} = await import('../src/console/format.js');
    const {errorText, fieldErrorText} = await import('../src/shared/errors.js');
    assert.equal(errorText({code: 'announcement_capacity_reached'}), copy('errors.validation_failed'));
    for (const [reason, key] of [['title', 'invalidTitle'], ['body', 'invalidBody'], ['end', 'invalidEnd']]) assert.equal(fieldErrorText(`invalid announcement ${reason}`), copy(`announcements.${key}`));
    for (const [action, key] of [['create', 'Create'], ['update', 'Update'], ['delete', 'Delete']]) assert.equal(describeAction(`announcement.${action}`), copy(`audit.announcement${key}`));
    const list = announcementList([{...entry, id, active: true}], context()); dom.body.append(list);
    elements(list).find((element) => element.getAttribute('aria-label') === copy('announcements.delete')).click(); await flush();
    elements(dom.body).find((element) => element.tagName === 'button' && element.textContent === copy('controls.cancel')).click();
    await new Promise((resolve) => setTimeout(resolve, 280)); await flush(); assert.equal(requests, 0);
    const dialog = announcementEditor(null, context());
    control(dom.body, 'title').value = 'T'; control(dom.body, 'body').value = 'body';
    control(dom.body, 'starts_at').value = '2030-01-01T00:00:00'; control(dom.body, 'ends_at').value = '2029-01-01T00:00:00';
    const form = elements(dom.body).find((element) => element.tagName === 'form');
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush(); assert.equal(requests, 0); assert.ok(dom.body.textContent.includes(copy('announcements.invalidEnd')));
    control(dom.body, 'ends_at').value = ''; control(dom.body, 'body').value = 'one\r\ntwo';
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush(); assert.equal(requests, 1); assert.equal(payload.body, 'one\ntwo'); assert.equal(payload.ends_at, null); await dialog.closed;
  } finally { globalThis.fetch = original; dom.restore(); }
});
