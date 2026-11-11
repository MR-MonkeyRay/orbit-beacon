import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {h, reducedMotion} from '../src/shared/dom.js';
import {modal, closeDialogs} from '../src/console/ui/dialogs.js';
import {toast} from '../src/console/ui/feedback.js';
import {liveHarness} from './live-harness.js';
import {matchesConfirmation} from '../src/console/ui/confirmation-match.js';

test('destructive confirmation requires the exact name or phrase', () => {
  assert.equal(matchesConfirmation('edge-1', 'edge-1'), true);
  assert.equal(matchesConfirmation('Edge-1', 'edge-1'), false);
  assert.equal(matchesConfirmation('edge-1 ', 'edge-1'), false);
  assert.equal(matchesConfirmation('', 'edge-1'), false);
  assert.equal(matchesConfirmation('delete 2 nodes', 'delete 2 nodes'), true);
  assert.equal(matchesConfirmation('delete 3 nodes', 'delete 2 nodes'), false);
});

test('toasts follow the topmost modal with their live region and dismiss buttons intact', async (t) => {
  const env = liveHarness(t);
  t.after(closeDialogs);
  const host = h('div', {id: 'toasts', class: 'toasts', role: 'status', 'aria-live': 'polite'});
  env.body.append(host);
  toast('Before dialog');
  const existing = host.firstElementChild;
  const first = modal('First', () => h('input'));
  const outer = document.querySelector('dialog');
  assert.equal(host.parentElement, outer);
  assert.equal(host.firstElementChild, existing);
  const second = modal('Second', () => h('input'));
  const inner = document.querySelectorAll('dialog').at(-1);
  assert.equal(host.parentElement, inner);
  toast('Inside dialog', {error: true});
  assert.equal(host.children.length, 2);
  assert.equal(host.getAttribute('role'), 'status');
  assert.equal(host.getAttribute('aria-live'), 'polite');
  assert.equal(host.lastElementChild.getAttribute('role'), 'alert');
  existing.querySelector('button').click();
  assert.ok(existing.classList.contains('leaving'));
  existing.dispatchEvent(new Event('animationend'));
  assert.equal(host.children.length, 1);
  second.close('done');
  if (!reducedMotion.matches) {
    assert.equal(host.parentElement, inner, 'keep the closing modal interactive until it leaves the top layer');
    const toastAnimation = new Event('animationend');
    Object.defineProperty(toastAnimation, 'target', {value: host.lastElementChild});
    inner.dispatchEvent(toastAnimation);
    assert.equal(inner.open, true, 'a toast animation must not finish the dialog close');
    inner.dispatchEvent(new Event('animationend'));
  }
  await second.closed;
  assert.equal(host.parentElement, outer);
  first.close('done'); outer.dispatchEvent(new Event('animationend')); await first.closed;
  assert.equal(host.parentElement, env.body);
  assert.equal(host.children.length, 1);
  toast('After dialog');
  assert.equal(host.children.length, 2);
});

test('closing a lower modal or all dialogs never discards the toast host', async (t) => {
  const env = liveHarness(t); t.after(closeDialogs);
  const host = h('div', {id: 'toasts'}); env.body.append(host);
  const first = modal('First', () => h('div'));
  const outer = document.querySelector('dialog');
  modal('Second', () => h('div'));
  const inner = document.querySelectorAll('dialog').at(-1);
  first.close('done'); outer.dispatchEvent(new Event('animationend')); await first.closed;
  assert.equal(host.parentElement, inner);
  closeDialogs();
  assert.equal(host.parentElement, env.body);
  assert.equal(document.getElementById('toasts'), host);
});

test('modal animation never creates a containing block for viewport-fixed toasts', () => {
  const css = readFileSync(new URL('../src/console/styles/components.css', import.meta.url), 'utf8');
  for (const name of ['dialog-in', 'dialog-out']) {
    const keyframes = css.slice(css.indexOf(`@keyframes ${name}`)).split('\n}')[0];
    assert.doesNotMatch(keyframes, /transform|filter|perspective/);
  }
});
