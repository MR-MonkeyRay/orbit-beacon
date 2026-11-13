import {t} from '../i18n.js';
import {api, getNavigationSignal} from '../api.js';
import {busy} from '../ui/components.js';
import {typedConfirmation, upgradeConfirmation} from '../ui/typed-confirmation.js';
import {toast, toastError} from '../ui/feedback.js';

// 分组批量指令的菜单项；执行期间 controls() 返回的按钮全部置忙。
export function groupCommandItems(group, {signal = getNavigationSignal(), controls = () => []} = {}) {
  let running = false;
  return ['upgrade', 'restart', 'stop'].map((kind) => ({
    label: t(`nodes.command.${kind}All`), danger: kind === 'stop', onSelect: async () => {
      if (running || signal?.aborted) return;
      running = true;
      const targets = [...controls()];
      targets.forEach((control) => busy(control, true));
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
      finally { running = false; targets.forEach((control) => busy(control, false)); }
    }
  }));
}
