import {settingsTabs} from '../routes.js';
import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h, icon} from '../../shared/dom.js';
import {allNodes, api, getNavigationSignal, getSessionEpoch, groupsList, invalidate, invalidateSession, nameLookup, session, updateUser, usersList} from '../api.js';
import {when} from '../format.js';
import {ignoreNavigationCancellation, refresh} from '../router.js';
import {busy, button, emptyState, facts, iconButton, link, pageHead, panel} from '../ui/components.js';
import {lazyTab} from '../ui/lazy-tab.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, onSubmit, passwordInput, validateBytes} from '../ui/forms.js';
import {apiTokensPanel} from '../ui/api-tokens.js';
import {totpPanel, totpCredentials} from '../ui/totp.js';
import {passwordChangeForm} from '../ui/password-form.js';
import {nextCursor} from '../ui/pager.js';
import {tabSet} from '../ui/tabs.js';
import {issueEnrollment} from './enrollments.js';
import {auditItem} from '../lib/audit-item.js';
import {dataOperationsTab} from './data-operations.js';
import {groupCommandActions} from '../lib/group-commands.js';
import {showTask} from './backup-task.js';
import {announcementsPanel} from './settings-announcements.js';

export async function settingsPage() {
  const signal = getNavigationSignal();
  const task = new URLSearchParams(location.search).get('task');
  if (task !== null && location.hash === '#data') {
    requestAnimationFrame(() => { if (!signal?.aborted) showTask(task, {signal}); });
  }
  const builds = {
    'account': accountTab,
    'administrators': lazyTab(usersList, administratorsTab),
    'groups': lazyTab(
      () => Promise.all([groupsList(), allNodes()]),
      ([groups, nodes]) => groupsTab(groups, nodes)
    ),
    'audit': lazyTab(
      () => Promise.all([api('/audit?limit=50&offset=0'), allNodes(), usersList(), groupsList()]),
      ([audit, nodes, users, groups]) => auditTab(audit, nameLookup(nodes, users, groups))
    ),
    'data': dataOperationsTab,
    'public-page': publicPageTab
  };
  return h('div', null,
    pageHead(t('shell.settings')),
    tabSet(settingsTabs.map((tab) => ({...tab, label: t(tab.label), build: builds[tab.id]})), t('settings.settingsSections')));
}

function publicPageTab() {
  const navigation = getNavigationSignal();
  const control = h('input', {type: 'checkbox', name: 'public_page_enabled', disabled: true});
  const note = h('p', {class: 'text-note', role: 'status'}, t('settings.publicLoading'));
  const errorBox = h('div', {class: 'form-error', role: 'alert', hidden: true});
  const retry = button(t('shell.tryAgain'), {onClick: () => { void load(); }});
  retry.hidden = true;
  let controller;
  let enabled;
  let pending = false;
  const active = () => controller && !controller.signal.aborted && !navigation?.aborted;
  const showError = (error) => {
    errorBox.textContent = errorText(error);
    errorBox.hidden = false;
  };
  const load = async () => {
    if (!active() || pending) return;
    pending = true;
    control.disabled = true;
    retry.hidden = true;
    errorBox.hidden = true;
    element.dataset.busy = 'true';
    try {
      const settings = await api('/settings/public-page', {signal: controller.signal});
      if (!active()) return;
      enabled = settings.enabled;
      control.checked = enabled;
      note.textContent = '';
    } catch (error) {
      if (active() && error.name !== 'AbortError') { showError(error); retry.hidden = false; }
    } finally {
      pending = false;
      element.dataset.busy = 'false';
      control.disabled = !active() || typeof enabled !== 'boolean' || !session.csrf;
    }
  };
  control.addEventListener('change', async () => {
    const requested = control.checked;
    control.checked = enabled;
    if (!active() || pending || requested === enabled || !session.csrf) return;
    pending = true;
    control.disabled = true;
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
      note.textContent = enabled ? t('settings.publicEnabled') : t('settings.publicDisabled');
    } catch (error) {
      if (active() && error.name !== 'AbortError') showError(error);
    } finally {
      pending = false;
      element.dataset.busy = 'false';
      control.checked = enabled;
      control.disabled = !active() || !session.csrf;
    }
  });
  // 面板标题与“公开页”标签页同名，省略。
  const element = panel(null, {},
    h('label', {class: 'switch'}, control, h('span', {class: 'switch-track', 'aria-hidden': 'true'}),
      h('span', {class: 'switch-text'}, h('strong', null, t('settings.publicEnable')))),
    errorBox, note, retry);
  const announcements = announcementsPanel(navigation);
  return {element: h('div', {class: 'stack'}, element, announcements.element),
    attach() { controller = new AbortController(); void load(); announcements.attach(); },
    detach() { controller?.abort(); announcements.detach(); }
  };
}

function accountTab() {
  const {user} = session;
  const disabled = !session.csrf;
  const {form} = passwordChangeForm({disabled, hint: t('account.passwordBytes'),
    ids: {old: 'pw-old', new: 'pw-new', confirm: 'pw-confirm', submit: 'pw-save'}, wrapActions: true});
  return h('div', {class: 'split'},
    panel(t('account.yourAccount'), {}, facts([
      [t('account.username'), user.username],
      [t('account.accountCreated'), when(user.created_at)], [t('account.sessionExpires'), session.expires ? when(session.expires) : null]
    ])),
    panel(t('account.changePassword'), {}, h('p', {class: 'text-note'}, t('account.passwordSignOutHint')), form), totpPanel(), apiTokensPanel());
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

function groupsTab(groups, nodes) {
  const deployAgent = async (item, trigger) => {
    const navigation = getNavigationSignal();
    const epoch = getSessionEpoch();
    busy(trigger, true);
    try {
      await issueEnrollment({group_id: item.id}, t('settings.deployAgentFor', {name: item.name}));
    } catch (error) { toastError(error); }
    finally {
      busy(trigger, false);
      if (trigger.isConnected && !trigger.disabled && !navigation?.aborted && navigation === getNavigationSignal() && epoch === getSessionEpoch()) trigger.focus({preventScroll: true});
    }
  };
  const groupForm = (item) => {
    let name;
    formDialog({
      title: item ? t('settings.renameName', {name: item.name}) : t('settings.addGroup'),
      submit: item ? t('settings.renameGroup') : t('settings.addGroup'),
      build: (form) => {
        name = input({name: 'name', required: true, value: item ? item.name : '', autocomplete: 'off'});
        validateBytes(name, 1, 255, t('settings.groupName'));
        form.append(field(t('settings.groupName'), name));
      },
      action: async () => {
        const saved = item
          ? await api(`/groups/${encodeURIComponent(item.id)}`, {method: 'PATCH', body: {name: name.value}})
          : await api('/groups', {method: 'POST', body: {name: name.value}});
        invalidate('groups');
        await refresh();
        toast(t(item ? 'settings.renamedName' : 'settings.addedName', {name: saved.name}));
      }
    });
  };
  const deleteGroup = async (item, size) => {
    const confirmed = await confirmDialog({
      title: t('settings.deleteNameQuestion', {name: item.name}),
      message: size ? t('settings.ungroupNodes', {count: size}) : t('settings.emptyGroup'),
      confirm: t('settings.deleteGroup'), danger: true
    });
    if (!confirmed) return;
    try {
      await api(`/groups/${encodeURIComponent(item.id)}`, {method: 'DELETE'});
      invalidate('groups', 'nodes');
      await refresh();
      toast(t('settings.deletedName', {name: item.name}));
    } catch (error) { toastError(error); }
  };
  const addButton = button(t('settings.addGroup'), {icon: 'plus', variant: 'primary', small: !!groups.length, onClick: () => groupForm()});
  if (!groups.length) return panel(null, {}, emptyState(t('settings.noGroups'), null, addButton));
  return panel(null, {action: addButton},
    h('ul', {class: 'rows'}, groups.map((item) => {
      const size = nodes.filter((node) => node.group_id === item.id).length;
      const enrollment = button(t('settings.deployAgent'), {small: true});
      enrollment.addEventListener('click', () => deployAgent(item, enrollment));
      return h('li', null, icon('folder', 'muted'),
        h('div', {class: 'grow'}, h('div', {class: 'title'}, item.name), h('div', {class: 'sub'}, link(`/nodes?group=${item.id}`, t('settings.nodeCount', {count: size})))),
        [enrollment, ...groupCommandActions(item), iconButton('edit', t('settings.renameName', {name: item.name}), () => groupForm(item)), iconButton('trash', t('settings.deleteName', {name: item.name}), () => deleteGroup(item, size))]);
    })));
}

function auditTab(page, names) {
  const {events} = page;
  let cursor = page.next_cursor;
  const eventIDs = new Set(events.map((event) => event.id));
  const list = h('ol', {class: 'timeline'}, events.map((event) => auditItem(event, names, true)));
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
      list.append(...newEvents.map((event) => auditItem(event, names, true)));
      more.hidden = !nextPage.has_more;
    } catch (error) { toastError(error); }
    finally { busy(more, false); }
  }});
  more.hidden = !page.has_more;
  return panel(null, {},
    events.length ? [list, more] : emptyState(t('settings.noAudit')));
}
