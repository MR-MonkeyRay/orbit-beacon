import {h} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {button} from './components.js';
import {modal} from './dialogs.js';
import {field, input} from './forms.js';
import {matchesConfirmation} from './confirmation-match.js';

export function typedConfirmation({title, message, expected, confirm}) {
  const {closed} = modal(title, (close) => {
    const entry = input({required: true, autocomplete: 'off', spellcheck: false, name: 'confirmation'});
    const submit = button(confirm, {type: 'submit', variant: 'danger', className: 'solid', disabled: true});
    entry.addEventListener('input', () => { submit.disabled = !matchesConfirmation(entry.value, expected); });
    return h('form', {class: 'form', onsubmit: (event) => {
      event.preventDefault();
      if (matchesConfirmation(entry.value, expected)) close('confirm');
    }},
    h('div', {class: 'dialog-body stack'}, h('p', null, message), field(t('controls.confirmation.type', {expected}), entry)),
    h('div', {class: 'dialog-foot'}, button(t('controls.cancel'), {onClick: () => close(), id: 'typed-confirm-cancel'}), submit));
  });
  return closed.then((value) => value === 'confirm');
}
