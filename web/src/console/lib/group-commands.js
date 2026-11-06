import {t} from '../i18n.js';
import {api, getNavigationSignal} from '../api.js';
import {busy, button} from '../ui/components.js';
import {typedConfirmation} from '../ui/typed-confirmation.js';
import {toast, toastError} from '../ui/feedback.js';

export function groupCommandActions(group, signal = getNavigationSignal()) {
  let running = false;
  const controls = ['restart', 'stop'].map((kind) => button(t(`nodes.command.${kind}All`), {
    small: true, variant: kind === 'stop' ? 'danger' : undefined, onClick: async () => {
      if (running || signal?.aborted) return;
      running = true;
      controls.forEach((control) => busy(control, true));
      try {
        if (!await typedConfirmation({title: t(`nodes.command.${kind}All`), expected: group.name,
          message: kind === 'stop' ? t('nodes.command.stopConsequence') : undefined,
          confirm: t(`nodes.command.${kind}All`)}) || signal?.aborted) return;
        const result = await api(`/groups/${encodeURIComponent(group.id)}/commands`, {
          method: 'POST', body: {kind, confirm: group.name}, signal
        });
        if (!signal?.aborted) toast(t('nodes.command.bulkResult', result));
      } catch (error) { if (!signal?.aborted && error.name !== 'AbortError') toastError(error); }
      finally { running = false; controls.forEach((control) => busy(control, false)); }
    }
  }));
  return controls;
}
