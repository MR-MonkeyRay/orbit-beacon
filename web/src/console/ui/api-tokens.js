import {h, icon} from '../../shared/dom.js';
import {copyElementText} from '../../shared/clipboard.js';
import {errorText} from '../../shared/errors.js';
import {t} from '../i18n.js';
import {api, getNavigationSignal, session} from '../api.js';
import {when} from '../format.js';
import {button, dataTable, emptyState, iconButton, panel} from './components.js';
import {confirmDialog, formDialog, modal} from './dialogs.js';
import {field, input, passwordInput, select, validateBytes} from './forms.js';
import {toastError} from './feedback.js';
import {skLines} from './placeholders.js';

// 新令牌只显示一次：等宽框显示，复制按钮与“已保存”同排，关闭后立即清空。
function tokenDialog(token) {
  const value = h('pre', {class: 'recovery-secret token-secret', tabindex: '0'}, token);
  token = '';
  const {closed} = modal(t('account.apiTokenCreatedTitle'), (close) => h('div', {class: 'dialog-body stack'},
    h('p', {class: 'token-once'}, icon('alert'), h('span', null, t('account.apiTokenOnce'))), value,
    h('div', {class: 'dialog-foot'}, button(t('account.copyAPIToken'), {icon: 'copy', onClick: () => copyElementText(value)}),
      button(t('account.recoverySaved'), {variant: 'primary', onClick: () => close('done')}))));
  void closed.then(() => { value.textContent = ''; });
}

function rateDialog({title, placeholder, value, required, save}) {
  let limit;
  formDialog({title, submit: t('settings.save'), build(form) {
    limit = input({type: 'number', name: 'rate_limit_per_minute', min: 1, max: 100000, step: 1, required, placeholder, value});
    form.append(field(t('account.apiTokenRate'), limit));
  }, action: () => save(limit.value)});
}

export function apiTokensPanel() {
  const body = h('div', {class: 'stack'}, skLines('44%', '70%', '56%'));
  const navigation = getNavigationSignal();
  const alive = () => !navigation?.aborted;
  const load = async () => {
    const [{api_tokens: tokens}, settings] = await Promise.all([api('/api-tokens'), api('/settings/api-tokens')]);
    if (!alive()) return;
    const defaultLimit = settings.rate_limit_per_minute;
    const editDefault = button(t('account.apiTokenChangeDefault'), {small: true, disabled: !session.csrf, onClick: () => rateDialog({
      title: t('account.apiTokenGlobalRate'), value: defaultLimit, required: true,
      save: async (value) => {
        await api('/settings/api-tokens', {method: 'PATCH', body: {rate_limit_per_minute: Number(value)}});
        if (alive()) await load();
      }})});
    const footer = h('div', {class: 'token-default'}, h('span', null, t('account.apiTokenDefaultLimit', {limit: defaultLimit})), editDefault);
    const row = (token) => {
      const edit = iconButton('edit', t('account.apiTokenEditRate'), () => rateDialog({
        title: t('account.apiTokenEditRate'), placeholder: String(defaultLimit), value: token.rate_limit_per_minute ?? '', required: false,
        save: async (value) => {
          await api(`/api-tokens/${encodeURIComponent(token.id)}`, {method: 'PATCH', body: {rate_limit_per_minute: value === '' ? null : Number(value)}});
          if (alive()) await load();
        }}));
      const revoke = iconButton('trash', t('account.revokeAPIToken'), async () => {
        if (!(await confirmDialog({title: t('account.revokeAPIToken'), message: t('account.revokeAPITokenMessage'), confirm: t('account.revokeAPIToken'), danger: true})) || !alive()) return;
        try { await api(`/api-tokens/${encodeURIComponent(token.id)}`, {method: 'DELETE'}); await load(); }
        catch (error) { if (alive()) toastError(error); }
      });
      for (const [control, key] of [[edit, 'account.apiTokenEditRate'], [revoke, 'account.revokeAPIToken']]) {
        control.disabled = !session.csrf;
        control.setAttribute('aria-label', `${t(key)}: ${token.name}`);
      }
      revoke.classList.add('icon-danger');
      const cell = (key, ...content) => h('td', {dataset: {label: t(`account.${key}`)}}, content);
      return h('tr', null,
        h('td', {class: 'token-name'}, token.name), cell('apiTokenCreator', token.created_by_username), cell('apiTokenCreated', when(token.created_at)),
        cell('apiTokenLastUsed', token.last_used_at ? when(token.last_used_at) : h('span', {class: 'muted'}, '—')),
        cell('apiTokenExpires', token.expires_at ? when(token.expires_at) : t('account.apiTokenNever')),
        cell('apiTokenRate', token.rate_limit_per_minute ?? h('span', {class: 'muted'}, t('account.apiTokenDefaultRate', {limit: defaultLimit}))),
        h('td', {class: 'actions'}, h('div', {class: 'row-actions'}, edit, revoke)));
    };
    const columns = ['apiTokenName', 'apiTokenCreator', 'apiTokenCreated', 'apiTokenLastUsed', 'apiTokenExpires', 'apiTokenRate'].map((key) => t(`account.${key}`));
    body.replaceChildren(tokens.length
      ? dataTable({className: 'token-table', columns: [...columns, h('span', {class: 'sr'}, t('account.apiTokenActions'))], body: h('tbody', null, tokens.map(row))})
      : emptyState(t('account.apiTokensEmpty')), footer);
  };
  const create = () => {
    let name; let days; let password; let token = '';
    const dialog = formDialog({title: t('account.createAPIToken'), submit: t('account.createAPIToken'), build(form) {
      name = input({name: 'name', required: true, autocomplete: 'off'});
      validateBytes(name, 1, 100, t('account.apiTokenName'));
      days = select([['30', t('account.apiToken30')], ['90', t('account.apiToken90')], ['365', t('account.apiToken365')], ['', t('account.apiTokenNever')]], '30', {name: 'expires_in_days'});
      password = passwordInput({name: 'password', required: true, autocomplete: 'current-password', dataset: {transient: ''}});
      validateBytes(password, 8, 1024, t('account.password'));
      form.append(h('div', {class: 'form-row'}, field(t('account.apiTokenName'), name), field(t('account.apiTokenValidity'), days)),
        field(t('account.currentPassword'), password, {inner: password.wrapper}));
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
  const section = panel(t('account.apiTokens'), {action: button(t('account.createAPIToken'), {icon: 'plus', variant: 'primary', disabled: !session.csrf, onClick: create})}, body);
  section.classList.add('panel-wide', 'api-tokens');
  return section;
}
