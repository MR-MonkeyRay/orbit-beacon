import {t} from '../i18n.js';
import {api, getNavigationSignal} from '../api.js';
import {busy, button} from '../ui/components.js';
import {menuButton} from '../ui/menu.js';
import {typedConfirmation, upgradeConfirmation} from '../ui/typed-confirmation.js';
import {toast, toastError} from '../ui/feedback.js';

export function groupCommandActions(group, signal = getNavigationSignal(), {menu = false} = {}) {
  let running = false;
  let controls;
  const items = ['upgrade', 'restart', 'stop'].map((kind) => ({
    label: t(`nodes.command.${kind}All`), danger: kind === 'stop', onSelect: async () => {
      if (running || signal?.aborted) return;
      running = true;
      controls.forEach((control) => busy(control, true));
      try {
        const targetVersion = kind === 'upgrade' ? await upgradeConfirmation({title: t('nodes.command.upgradeAll'), expected: group.name}) : undefined;
        if (!(kind === 'upgrade' ? targetVersion : await typedConfirmation({title: t(`nodes.command.${kind}All`), expected: group.name,
          message: kind === 'stop' ? t('nodes.command.stopConsequence') : undefined,
          confirm: t(`nodes.command.${kind}All`)})) || signal?.aborted) return;
        const result = await api(`/groups/${encodeURIComponent(group.id)}/commands`, {
          method: 'POST', body: {kind, confirm: group.name, ...(kind === 'upgrade' ? {target_version: targetVersion} : {})}, signal
        });
        if (!signal?.aborted) toast(t('nodes.command.bulkResult', result));
      } catch (error) { if (!signal?.aborted && error.name !== 'AbortError') toastError(error); }
      finally { running = false; controls.forEach((control) => busy(control, false)); }
    }
  }));
  if (menu) {
    const root = menuButton({label: t('nodes.more'), items, signal});
    controls = root.querySelectorAll('button');
    return [root];
  }
  controls = items.map((item) => button(item.label, {small: true, variant: item.danger ? 'danger' : undefined, onClick: item.onSelect}));
  return controls;
}
