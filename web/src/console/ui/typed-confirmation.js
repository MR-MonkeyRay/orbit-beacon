import {h} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {button} from './components.js';
import {modal} from './dialogs.js';
import {field, input} from './forms.js';
import {matchesConfirmation} from './confirmation-match.js';

export function typedConfirmation({title, message, expected, confirm}) {
  const {closed} = modal(title, (close) => {
    const entry = input({required: true, autocomplete: 'off', spellcheck: false, name: 'confirmation', dataset: {transient: ''}});
    const submit = button(confirm, {type: 'submit', variant: 'danger', className: 'solid', disabled: true});
    entry.addEventListener('input', () => { submit.disabled = !matchesConfirmation(entry.value, expected); });
    return h('form', {class: 'form', onsubmit: (event) => {
      event.preventDefault();
      if (matchesConfirmation(entry.value, expected)) close('confirm');
    }},
    h('div', {class: 'dialog-body stack'}, message ? h('p', null, message) : null, field(t('controls.confirmation.type', {expected}), entry)),
    h('div', {class: 'dialog-foot'}, button(t('controls.cancel'), {onClick: () => close(), id: 'typed-confirm-cancel'}), submit));
  });
  return closed.then((value) => value === 'confirm');
}

// 取消时返回 null；modal 的关闭值不是版本号。
export function upgradeConfirmation({title, expected}) {
  let target = null;
  const {closed} = modal(title, (close) => {
    const version = input({required: true, autocomplete: 'off', spellcheck: false, name: 'target_version', maxlength: 64, placeholder: '1.2.3', dataset: {transient: ''}});
    const confirmation = expected === undefined ? null : input({required: true, autocomplete: 'off', spellcheck: false, name: 'confirmation', dataset: {transient: ''}});
    const valid = () => /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(version.value)
      && (!confirmation || matchesConfirmation(confirmation.value, expected));
    const submit = button(title, {type: 'submit', className: 'solid', disabled: true});
    const check = () => { submit.disabled = !valid(); };
    version.addEventListener('input', check);
    confirmation?.addEventListener('input', check);
    return h('form', {class: 'form', onsubmit: (event) => {
      event.preventDefault();
      if (valid()) { target = version.value; close('confirm'); }
    }},
    h('div', {class: 'dialog-body stack'}, field(t('nodes.command.targetVersion'), version),
      confirmation ? field(t('controls.confirmation.type', {expected}), confirmation) : null),
    h('div', {class: 'dialog-foot'}, button(t('controls.cancel'), {onClick: () => close()}), submit));
  });
  return closed.then((value) => value === 'confirm' ? target : null);
}
