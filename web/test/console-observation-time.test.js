import test from 'node:test';
import assert from 'node:assert/strict';
import {agentSettingsTab} from '../src/console/pages/node-agent-settings.js';
import {installDOM} from './console-dom.js';

test('quiet offline Agent observations retain unambiguous reception time after the browser clock advances', () => {
  const dom = installDOM();
  const originalNow = Date.now;
  const received = '2026-01-15T12:34:56Z';
  Date.now = () => Date.parse(received) + 181000;
  try {
    const view = agentSettingsTab({presence: 'offline', last_seen: received, agent_version: 'test'}, {
      report_interval_seconds: 10, disk_interval_seconds: 60,
      interfaces: [], disk_paths: [], probe_targets: []
    });
    dom.body.append(view);
    Date.now = () => Date.parse(received) + 86400000;
    const times = view.querySelectorAll('time');
    assert.equal(times.length, 2, 'offline explanation and Last reported identify the same observation');
    for (const time of times) {
      assert.equal(Date.parse(time.textContent), Date.parse(received));
      assert.equal(time.getAttribute('datetime'), received);
    }
  } finally { Date.now = originalNow; dom.restore(); }
});
