import {settingsTabs} from '../routes.js';
import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h, icon} from '../../shared/dom.js';
import {allNodes, api, getNavigationSignal, groupsList, invalidate, invalidateSession, nameLookup, session, updateUser, usersList} from '../api.js';
import {ACTIONS, when} from '../format.js';
import {ignoreNavigationCancellation, refresh} from '../router.js';
import {busy, button, dataTable, emptyState, facts, iconButton, pageHead, panel} from '../ui/components.js';
import {lazyTab} from '../ui/lazy-tab.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, onSubmit, passwordInput, select, validateBytes} from '../ui/forms.js';
import {apiTokensPanel} from '../ui/api-tokens.js';
import {totpPanel, totpCredentials} from '../ui/totp.js';
import {passwordChangeForm} from '../ui/password-form.js';
import {nextCursor} from '../ui/pager.js';
import {tabSet} from '../ui/tabs.js';
import {auditItem} from '../lib/audit-item.js';
import {dataManagementPage} from './data-management.js';
import {dataOperationsTab} from './data-operations.js';
import {showTask} from './backup-task.js';

export async function settingsPage() {
  const signal = getNavigationSignal();
  const task = new URLSearchParams(location.search).get('task');
  if (task !== null && location.hash === '#data') {
    requestAnimationFrame(() => { if (!signal?.aborted) showTask(task, {signal}); });
  }
  const builds = {
    'account': accountTab,
    'api-tokens': apiTokensPanel,
    'administrators': lazyTab(usersList, administratorsTab),
    'audit': lazyTab(
      () => Promise.all([api('/audit?limit=50&offset=0'), allNodes(), usersList(), groupsList()]),
      ([audit, nodes, users, groups]) => auditTab(audit, nameLookup(nodes, users, groups), users)
    ),
    'data': dataOperationsTab,
    'data-storage': () => dataManagementPage({embedded: true}),
    'public-page': publicPageTab
  };
  return h('div', null,
    pageHead(t('shell.settings')),
    tabSet(settingsTabs.map((tab) => ({...tab, label: t(tab.label), build: builds[tab.id]})), t('settings.settingsSections')));
}

function publicPageTab() {
  const navigation = getNavigationSignal();
  const control = h('input', {type: 'checkbox', name: 'public_page_enabled', disabled: true});
  const groupsControl = h('input', {type: 'checkbox', name: 'public_page_show_groups', disabled: true});
  const note = h('p', {class: 'text-note', role: 'status'}, t('settings.publicLoading'));
  const errorBox = h('div', {class: 'form-error', role: 'alert', hidden: true});
  const retry = button(t('shell.tryAgain'), {onClick: () => { void load(); }});
  retry.hidden = true;
  let controller;
  let enabled;
  let showGroups;
  let pending = false;
  // 两个开关共用一个请求锁，结束时按各自的已知状态恢复可用。
  const settle = () => {
    control.checked = enabled === true;
    groupsControl.checked = showGroups === true;
    control.disabled = !active() || typeof enabled !== 'boolean' || !session.csrf;
    groupsControl.disabled = !active() || typeof showGroups !== 'boolean' || !session.csrf;
  };
  const active = () => controller && !controller.signal.aborted && !navigation?.aborted;
  const showError = (error) => {
    errorBox.textContent = errorText(error);
    errorBox.hidden = false;
  };
  const load = async () => {
    if (!active() || pending) return;
    pending = true;
    control.disabled = true;
    groupsControl.disabled = true;
    retry.hidden = true;
    errorBox.hidden = true;
    element.dataset.busy = 'true';
    try {
      const settings = await api('/settings/public-page', {signal: controller.signal});
      if (!active()) return;
      enabled = settings.enabled;
      showGroups = settings.show_groups;
      note.textContent = '';
    } catch (error) {
      if (active() && error.name !== 'AbortError') { showError(error); retry.hidden = false; }
    } finally {
      pending = false;
      element.dataset.busy = 'false';
      settle();
    }
  };
  control.addEventListener('change', async () => {
    const requested = control.checked;
    control.checked = enabled;
    if (!active() || pending || requested === enabled || !session.csrf) return;
    pending = true;
    control.disabled = true;
    groupsControl.disabled = true;
    element.dataset.busy = 'true';
    errorBox.hidden = true;
    note.textContent = '';
    try {
      if (!requested && !(await confirmDialog({
        title: t('settings.publicDisableTitle'),
        message: t('settings.publicDisableMessage'),
        confirm: t('settings.publicDisable'), danger: true
      }))) return;
      if (!active()) return;
      const settings = await api('/settings/public-page', {method: 'PATCH', body: {enabled: requested}, signal: controller.signal});
      if (!active()) return;
      enabled = settings.enabled;
      showGroups = settings.show_groups;
      note.textContent = enabled ? t('settings.publicEnabled') : t('settings.publicDisabled');
    } catch (error) {
      if (active() && error.name !== 'AbortError') showError(error);
    } finally {
      pending = false;
      element.dataset.busy = 'false';
      settle();
    }
  });
  groupsControl.addEventListener('change', async () => {
    const requested = groupsControl.checked;
    groupsControl.checked = showGroups === true;
    if (!active() || pending || requested === showGroups || !session.csrf) return;
    pending = true;
    control.disabled = true;
    groupsControl.disabled = true;
    element.dataset.busy = 'true';
    errorBox.hidden = true;
    note.textContent = '';
    try {
      const settings = await api('/settings/public-page', {method: 'PATCH', body: {show_groups: requested}, signal: controller.signal});
      if (!active()) return;
      enabled = settings.enabled;
      showGroups = settings.show_groups;
    } catch (error) {
      if (active() && error.name !== 'AbortError') showError(error);
    } finally {
      pending = false;
      element.dataset.busy = 'false';
      settle();
    }
  });
  // 面板标题与“公开页”标签页同名，省略；打开状态页的入口与开关同行，不再放在概览页头。
  const element = panel(null, {},
    h('div', {class: 'public-page-row'},
      h('label', {class: 'switch'}, control, h('span', {class: 'switch-track', 'aria-hidden': 'true'}),
        h('span', {class: 'switch-text'}, h('strong', null, t('settings.publicEnable')))),
      h('a', {class: 'btn btn-sm', href: '/'}, icon('external'), t('nodes.openPublic'))),
    h('label', {class: 'switch public-page-groups'}, groupsControl, h('span', {class: 'switch-track', 'aria-hidden': 'true'}),
      h('span', {class: 'switch-text'}, t('settings.publicGroups'))),
    errorBox, note, retry);
  return {element,
    attach() { controller = new AbortController(); void load(); },
    detach() { controller?.abort(); }
  };
}

function accountTab() {
  const {user} = session;
  const disabled = !session.csrf;
  const {form} = passwordChangeForm({disabled, hint: t('account.passwordBytes'),
    ids: {old: 'pw-old', new: 'pw-new', confirm: 'pw-confirm', submit: 'pw-save'}, wrapActions: true});
  return h('div', {class: 'stack settings-account'},
    panel(t('account.yourAccount'), {}, facts([
      [t('account.username'), user.username],
      [t('account.accountCreated'), when(user.created_at)], [t('account.sessionExpires'), session.expires ? when(session.expires) : null]
    ])),
    panel(t('account.changePassword'), {}, h('p', {class: 'text-note'}, t('account.passwordSignOutHint')), form), totpPanel());
}

function administratorsTab(users) {
  const self = (item) => item.id === session.user.id;
  const lastAdministrator = users.length === 1;

  const editUser = (item) => {
    let username;
    formDialog({
      title: t('settings.renameAdministrator'),
      submit: t('settings.save'),
      build: (form) => {
        username = input({name: 'username', required: true, value: item.username, autocomplete: 'off'});
        validateBytes(username, 1, 128, t('account.username'));
        form.append(field(t('account.username'), username));
      },
      action: async () => {
        const patch = {};
        if (username.value !== item.username) patch.username = username.value;
        if (Object.keys(patch).length === 0) return;
        await api(`/users/${encodeURIComponent(item.id)}`, {method: 'PATCH', body: patch});
        invalidate('users');
        if (self(item)) updateUser(patch);
        await refresh();
        toast(t('settings.savedName', {name: patch.username || item.username}));
      }
    });
  };

  const deleteUser = async (item) => {
    const confirmed = await confirmDialog({
      title: self(item) ? t('settings.deleteOwn') : t('settings.deleteAdministrator'),
      message: self(item) ? t('settings.deleteOwnMessage') : t('settings.deleteAdministratorMessage', {name: item.username}),
      confirm: self(item) ? t('settings.deleteMyAccount') : t('settings.deleteAdministrator'), danger: true
    });
    if (!confirmed) return;
    try {
      await api(`/users/${encodeURIComponent(item.id)}`, {method: 'DELETE'});
      if (self(item)) { invalidateSession('deleted'); return; }
      invalidate('users');
      await refresh();
      toast(t('settings.deletedName', {name: item.username}));
    } catch (error) {
      toastError(error);
      if (error.code === 'last_administrator') {
        invalidate('users');
        await refresh().catch(ignoreNavigationCancellation);
      }
    }
  };

  const createUser = () => {
    let username;
    let password;
    let mustChangePassword;
    formDialog({
      title: t('settings.addAdministrator'),
      submit: t('settings.addAdministrator'),
      build: (form) => {
        username = input({name: 'username', required: true, autocomplete: 'off'});
        password = passwordInput({name: 'password', required: true, autocomplete: 'new-password'});
        mustChangePassword = input({type: 'checkbox', name: 'must_change_password', checked: false});
        validateBytes(username, 1, 128, t('account.username'));
        validateBytes(password, 8, 1024, t('account.password'));
        form.append(field(t('account.username'), username),
          field(t('settings.initialPassword'), password, {inner: password.wrapper, hint: t('account.passwordBytes')}),
          h('label', {class: 'check-row'}, mustChangePassword, h('span', null, t('settings.requirePasswordChange'))));
      },
      action: async () => {
        try {
          const created = await api('/users', {method: 'POST', body: {username: username.value, password: password.value, must_change_password: mustChangePassword.checked}});
          invalidate('users');
          await refresh();
          toast(t('settings.addedName', {name: created.username}));
        } finally { password.value = ''; }
      }
    });
  };

  const rows = users.map((item) => {
    const remove = iconButton('trash', t('settings.deleteAdministratorLabel', {name: item.username}), () => deleteUser(item));
    remove.disabled = lastAdministrator;
    return h('tr', null,
      h('td', null, h('div', {class: 'who administrator-who'}, h('span', {class: 'avatar', 'aria-hidden': 'true'}, item.username.slice(0, 1)),
        h('div', {class: 'administrator-name'}, h('span', {class: 'administrator-username', title: item.username}, item.username),
          self(item) ? h('span', {class: 'muted administrator-self'}, t('settings.you')) : null))),
      h('td', {class: 'muted hide-compact'}, when(item.created_at)),
 h('td', null, t(item.totp_enabled ? 'account.totpEnabled' : 'account.totpDisabled')),
      h('td', {class: 'actions'}, !self(item) && item.totp_enabled ? button(t('account.totpReset'), {small: true, onClick: () => totpCredentials({
 title: t('account.totpReset'), secondFactor: false, action: async (body) => {
 await api(`/users/${encodeURIComponent(item.id)}/totp/reset`, {method: 'POST', body});
 invalidate('users'); await refresh();
 }
 })}) : null, iconButton('edit', t('settings.renameAdministratorLabel', {name: item.username}), () => editUser(item)), remove));
  });

  // 标题与“管理员”标签页同名，省略；操作按钮靠右。
  return h('section', {class: 'panel'},
    h('div', {class: 'panel-head panel-head-end'}, button(t('settings.addAdministrator'), {icon: 'plus', variant: 'primary', small: true, onClick: createUser})),
    h('div', {class: 'table-wrap'}, h('table', {class: 'table administrators-table'},
      h('thead', null, h('tr', null, h('th', {scope: 'col'}, t('account.username')), h('th', {scope: 'col', class: 'hide-compact'}, t('settings.created')),
        h('th', {scope: 'col'}, t('account.totp')), h('th', {scope: 'col'}, h('span', {class: 'sr'}, t('settings.actions'))))),
      h('tbody', null, users.length ? rows : h('tr', null, h('td', {colspan: 4}, t('settings.noAdministrators')))))),
    lastAdministrator ? h('p', {class: 'text-note'}, t('settings.lastAdministrator')) : null);
}

function auditTab(page, names, users) {
  const {events} = page;
  let cursor = page.next_cursor;
  const eventIDs = new Set(events.map((event) => event.id));
  const resources = {
    service: 'shell.services', service_group: 'shell.services', node: 'shell.nodes', enrollment: 'audit.category.enrollment', command: 'audit.category.command',
    group: 'shell.nodes', user: 'settings.account', session: 'settings.account',
    alert_rule: 'shell.alerts', alert_silence: 'shell.alerts', notification_channel: 'audit.category.notification',
    backup: 'settings.backups', api_token: 'audit.category.token',
    settings: 'audit.category.token', public_page: 'settings.publicPage'
  };
  const categories = [...new Set(Object.keys(ACTIONS).map((action) => resources[action.split('.')[0]]))];
  const category = select([['', t('nodes.all')], ...categories.map((key) => [key, t(key)])], '', {name: 'audit_category'});
  const actor = select([['', t('nodes.all')], ...users.map((user) => [user.id, user.username])], '', {name: 'audit_actor'});
  const list = h('tbody');
  const render = () => {
    const filtered = events.filter((event) => (!category.value || resources[event.action.split('.')[0]] === category.value)
      && (!actor.value || event.actor_id === actor.value));
    list.replaceChildren(...(filtered.length ? filtered.map((event) => auditItem(event, names, true, {compact: true}))
      : [h('tr', null, h('td', {colspan: 5}, emptyState(t('settings.noAudit'))))]));
  };
  category.addEventListener('change', render);
  actor.addEventListener('change', render);
  render();
  const more = button(t('settings.olderEvents'), {onClick: async () => {
    busy(more, true);
    try {
      const nextPage = await api(`/audit?limit=50&before=${encodeURIComponent(cursor)}`);
      cursor = nextCursor(nextPage, cursor);
      const newEvents = nextPage.events.filter((event) => {
        if (eventIDs.has(event.id)) return false;
        eventIDs.add(event.id);
        return true;
      });
      events.push(...newEvents);
      render();
      more.hidden = !nextPage.has_more;
    } catch (error) { toastError(error); }
    finally { busy(more, false); }
  }});
  more.hidden = !page.has_more;
  return panel(null, {},
    h('div', {class: 'audit-filters'}, field(t('audit.filterAction'), category), field(t('nodes.audit.actor'), actor)),
    dataTable({caption: t('settings.audit'), className: 'audit-table',
      columns: [t('audit.time'), t('nodes.audit.actor'), t('nodes.audit.action'), t('nodes.audit.target'), t('nodes.audit.details')], body: list}), more);
}
