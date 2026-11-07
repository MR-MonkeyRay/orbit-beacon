import test from 'node:test';
import assert from 'node:assert/strict';
import {panelLifecycle} from '../src/console/ui/polling-panel.js';
import {installDOM} from './console-dom.js';

test('polling panel pauses while detached, resumes when attached, and abort is final', () => {
  const dom = installDOM();
  const signal = new AbortController();
  const root = document.createElement('section');
  let loads = 0;
  let cancels = 0;
  try {
    panelLifecycle({root, signal: signal.signal, load: () => { loads += 1; }, cancel: () => { cancels += 1; }});
    assert.equal(loads, 1);
    assert.equal(dom.timers.size, 1);
    dom.body.append(root); dom.mutations();
    assert.equal(loads, 1);
    root.remove(); dom.mutations();
    assert.equal(cancels, 1);
    assert.equal(dom.timers.size, 0);
    dom.document.hidden = true;
    dom.body.append(root); dom.mutations();
    assert.equal(loads, 1);
    assert.equal(dom.timers.size, 1);
    dom.document.hidden = false;
    [...dom.timers.values()][0]();
    assert.equal(loads, 2);
    signal.abort();
    assert.equal(cancels, 2);
    assert.equal(dom.timers.size, 0);
    root.remove(); dom.mutations(); dom.body.append(root); dom.mutations();
    assert.equal(loads, 2);
  } finally { signal.abort(); dom.restore(); }
});

test('polling panel can reload immediately when reattached while hidden', () => {
  const dom = installDOM();
  const signal = new AbortController();
  const root = document.createElement('section');
  let loads = 0;
  try {
    panelLifecycle({root, signal: signal.signal, load: () => { loads += 1; }, cancel() {}, loadWhenHidden: true});
    dom.body.append(root); dom.mutations(); root.remove(); dom.mutations();
    dom.document.hidden = true;
    dom.body.append(root); dom.mutations();
    assert.equal(loads, 2);
  } finally { signal.abort(); dom.restore(); }
});

test('non-resuming panel disposes permanently when detached', () => {
  const dom = installDOM();
  const signal = new AbortController();
  const root = document.createElement('section');
  let loads = 0;
  let cancels = 0;
  try {
    const lifecycle = panelLifecycle({root, signal: signal.signal, load: () => { loads += 1; },
      cancel: () => { cancels += 1; }, poll: false, resume: false});
    dom.body.append(root); dom.mutations();
    root.remove(); dom.mutations();
    assert.equal(lifecycle.disposed, true);
    assert.equal(cancels, 1);
    dom.body.append(root); dom.mutations();
    assert.equal(loads, 1);
  } finally { signal.abort(); dom.restore(); }
});
