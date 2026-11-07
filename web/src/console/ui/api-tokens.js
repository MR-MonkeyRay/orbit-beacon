import {h} from '../../shared/dom.js';
import {copyElementText} from '../../shared/clipboard.js';
import {errorText} from '../../shared/errors.js';
import {t} from '../i18n.js';
import {api, getNavigationSignal, session} from '../api.js';
import {when} from '../format.js';
import {button, dataTable, panel} from './components.js';
import {confirmDialog, formDialog, modal} from './dialogs.js';
import {field, input, passwordInput, select, validateBytes, onSubmit} from './forms.js';
import {toastError} from './feedback.js';

function tokenDialog(token) {
  const value = h('pre', {class: 'recovery-secret', tabindex: '0'}, token);
  token = '';
  const {closed} = modal(t('account.apiTokens'), (close) => h('div', {class: 'dialog-body'}, value,
    h('div', {class: 'dialog-foot'}, button(t('account.copyAPIToken'), {onClick: () => copyElementText(value)}),
      button(t('account.recoverySaved'), {onClick: () => close('done')}))));
  void closed.then(() => { value.textContent = ''; });
}

export function apiTokensPanel() {
  const body = h('div', {class: 'stack'});
  const navigation = getNavigationSignal();
  const alive = () => !navigation?.aborted;
  const load = async () => {
    const [{api_tokens: tokens}, settings] = await Promise.all([api('/api-tokens'), api('/settings/api-tokens')]);
    if (!alive()) return;
    const globalLimit = input({type: 'number', name: 'rate_limit_per_minute', min: 1, max: 100000, step: 1, required: true, value: settings.rate_limit_per_minute});
    const save = button(t('settings.save'), {type: 'submit', disabled: !session.csrf});
    const error = h('div', {class: 'form-error', role: 'alert', hidden: true});
    const form = h('form', {class: 'form form-compact'}, field(t('account.apiTokenGlobalRate'), globalLimit), error, h('div', {class: 'actions'}, save));
    onSubmit(form, save, async () => {
      await api('/settings/api-tokens', {method: 'PATCH', body: {rate_limit_per_minute: Number(globalLimit.value)}});
      if (alive()) await load();
    }, error);
    body.replaceChildren(form, dataTable({columns: ['apiTokenName', 'apiTokenCreator', 'apiTokenCreated', 'apiTokenLastUsed', 'apiTokenExpires', 'apiTokenRate', 'apiTokenActions'].map((key) => t(`account.${key}`)),
      body: h('tbody', null, tokens.map((token) => h('tr', null,
        h('td', null, token.name), h('td', null, token.created_by_username), h('td', null, when(token.created_at)),
        h('td', null, token.last_used_at ? when(token.last_used_at) : '—'), h('td', null, token.expires_at ? when(token.expires_at) : t('account.apiTokenNever')),
        h('td', null, token.rate_limit_per_minute ?? t('account.apiTokenDefaultRate', {limit: settings.rate_limit_per_minute})),
        h('td', {class: 'actions'}, button(t('account.apiTokenEditRate'), {small: true, disabled: !session.csrf, onClick: () => {
          let limit;
          formDialog({title: t('account.apiTokenEditRate'), submit: t('settings.save'), build(form) {
            limit = input({type: 'number', name: 'rate_limit_per_minute', min: 1, max: 100000, step: 1, placeholder: String(settings.rate_limit_per_minute), value: token.rate_limit_per_minute ?? ''});
            form.append(field(t('account.apiTokenRate'), limit));
          }, action: async () => {
            await api(`/api-tokens/${encodeURIComponent(token.id)}`, {method: 'PATCH', body: {rate_limit_per_minute: limit.value === '' ? null : Number(limit.value)}});
            if (alive()) await load();
          }});
        }}), button(t('account.revokeAPIToken'), {small: true, disabled: !session.csrf, onClick: async () => {
          if (!(await confirmDialog({title: t('account.revokeAPIToken'), confirm: t('account.revokeAPIToken'), danger: true})) || !alive()) return;
          try { await api(`/api-tokens/${encodeURIComponent(token.id)}`, {method: 'DELETE'}); await load(); }
          catch (error) { if (alive()) toastError(error); }
        }})))))
    }));
  };
  const create = () => {
    let name; let days; let password; let token = '';
    const dialog = formDialog({title: t('account.createAPIToken'), submit: t('account.createAPIToken'), build(form) {
      name = input({name: 'name', required: true, autocomplete: 'off'});
      validateBytes(name, 1, 100, t('account.apiTokenName'));
      days = select([['30', t('account.apiToken30')], ['90', t('account.apiToken90')], ['365', t('account.apiToken365')], ['', t('account.apiTokenNever')]], '30', {name: 'expires_in_days'});
      password = passwordInput({name: 'password', required: true, autocomplete: 'current-password', dataset: {transient: ''}});
      validateBytes(password, 8, 1024, t('account.password'));
      form.append(field(t('account.apiTokenName'), name), field(t('account.apiTokenValidity'), days), field(t('account.currentPassword'), password, {inner: password.wrapper}));
    }, action: async () => {
      try {
        const result = await api('/api-tokens', {method: 'POST', body: {name: name.value, expires_in_days: days.value ? Number(days.value) : null, password: password.value}});
        token = result.token;
        result.token = '';
        void load().catch((error) => { if (alive()) toastError(error); });
      } finally { password.value = ''; }
    }});
    void dialog.closed.then((result) => {
      password.value = '';
      if (result === 'done' && token && alive()) tokenDialog(token);
      token = '';
    });
  };
  void load().catch((error) => { if (alive()) body.replaceChildren(h('div', {class: 'form-error', role: 'alert'}, errorText(error)), button(t('common.retry'), {onClick: () => { void load().catch(toastError); }})); });
  const section = panel(t('account.apiTokens'), {action: button(t('account.createAPIToken'), {small: true, disabled: !session.csrf, onClick: create})}, body);
  section.classList.add('panel-wide');
  return section;
}
