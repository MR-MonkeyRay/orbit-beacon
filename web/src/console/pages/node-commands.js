import {t} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {api, getNavigationSignal, getSessionSignal} from '../api.js';
import {absoluteWhen} from '../format.js';
import {button, dataTable, panel} from '../ui/components.js';
import {confirmDialog} from '../ui/dialogs.js';
import {typedConfirmation} from '../ui/typed-confirmation.js';
import {toastError} from '../ui/feedback.js';
import {panelLifecycle} from '../ui/polling-panel.js';

const activeCommand = (command) => ['pending', 'delivered'].includes(command.state);
const resultCodes = ['disabled_locally', 'unsupported', 'interrupted', 'failed'];

export function nodeCommandsPanel(node, runtime, signal = getNavigationSignal()) {
  const path = `/nodes/${encodeURIComponent(node.id)}/commands`;
  const sessionSignal = getSessionSignal();
  let records = [];
  let loaded = false;
  let loading;
  let running = false;
  let controller;
  let lifecycle;
  const active = () => controller && !controller.signal.aborted && !signal?.aborted && !sessionSignal.aborted;
  const controls = ['restart', 'stop'].map((kind) => button(t(`nodes.command.${kind}`), {
    variant: kind === 'stop' ? 'danger' : undefined, disabled: true, onClick: () => create(kind)
  }));
  const body = h('tbody');
  const table = dataTable({columns: [t('nodes.command.kind'), t('nodes.command.status'), t('nodes.command.requestedBy'), t('settings.created'), h('span', {class: 'sr'}, t('settings.actions'))], body, hidden: true});
  const element = panel(t('nodes.remoteCommands'), {}, h('div', {class: 'head-actions'}, controls), table);
  const patch = () => {
    controls.forEach((control, index) => {
      const kind = ['restart', 'stop'][index];
      control.disabled = !active() || !loaded || running || node.deleted || node.status === 'disabled' || node.stopped
        || !runtime?.remote_commands.includes(kind) || records.some(activeCommand);
    });
    lifecycle?.syncPolling();
  };
  const render = () => {
    table.hidden = records.length === 0;
    body.replaceChildren(...records.map((command) => {
      const code = resultCodes.includes(command.result_code) ? ` · ${t(`nodes.command.result.${command.result_code}`)}` : '';
      return h('tr', null,
        h('td', null, t(`nodes.command.${command.kind}`)),
        h('td', null, `${t(`nodes.command.state.${command.state}`)}${code}`),
        h('td', null, command.requested_by), h('td', null, absoluteWhen(command.created_at)),
        h('td', null, command.state === 'pending' ? button(t('controls.cancel'), {small: true, disabled: running || node.deleted,
          onClick: () => mutate(() => api(`${path}/${encodeURIComponent(command.id)}/cancel`, {method: 'POST', signal: controller.signal}))}) : null));
    }));
    patch();
  };
  const load = async (force = false) => {
    if (!active() || node.deleted || loading && !force) return;
    const current = controller;
    const request = {};
    loading = request;
    try {
      const result = await api(path, {signal: current.signal});
      if (!active() || current !== controller || loading !== request) return;
      records = result.commands;
      loaded = true;
      render();
    } catch (error) { if (active() && current === controller && loading === request && error.name !== 'AbortError') toastError(error); }
    finally { if (loading === request) loading = undefined; }
  };
  const mutate = async (action) => {
    if (!active() || running || node.deleted) return;
    running = true;
    render();
    try { await action(); if (active()) await load(true); }
    catch (error) { if (active() && error.name !== 'AbortError') toastError(error); }
    finally { running = false; if (active()) render(); }
  };
  async function create(kind) {
    const control = controls[kind === 'restart' ? 0 : 1];
    if (control.disabled) return;
    const current = controller;
    const expected = node.name;
    await mutate(async () => {
      const approved = kind === 'stop'
        ? await typedConfirmation({title: t('nodes.command.stopTitle'), message: t('nodes.command.stopConsequence'), expected, confirm: t('nodes.command.stop')})
        : await confirmDialog({title: t('nodes.command.restartTitle'), confirm: t('nodes.command.restart')});
      if (!approved || !active() || current !== controller || node.deleted || node.stopped || node.status === 'disabled' || !runtime?.remote_commands.includes(kind) || records.some(activeCommand)) return;
      await api(path, {method: 'POST', body: {kind, ...(kind === 'stop' ? {confirm: expected} : {})}, signal: current.signal});
    });
  }
  const detach = () => {
    lifecycle?.dispose(); lifecycle = undefined;
    controller?.abort(); controller = undefined;
    loading = undefined;
    patch();
  };
  const attach = () => {
    if (active() || signal?.aborted || sessionSignal.aborted) return;
    controller = new AbortController();
    loaded = false;
    patch();
    lifecycle = panelLifecycle({root: element, signal: controller.signal, load, interval: 3000,
      shouldPoll: () => records.some(activeCommand) && !node.deleted});
  };
  signal?.addEventListener('abort', () => { detach(); sessionSignal.removeEventListener('abort', detach); }, {once: true});
  sessionSignal.addEventListener('abort', detach, {once: true});
  return {element, attach, detach, snapshot(current, observed) { node = current; runtime = observed; patch(); }};
}
