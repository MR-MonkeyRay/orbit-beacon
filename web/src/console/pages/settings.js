import {t} from '../../shared/i18n.js';
import {errorText} from '../../shared/errors.js';
import {h, icon} from '../../shared/dom.js';
import {allNodes, api, ensureSession, getNavigationSignal, groupsList, invalidate, invalidateSession, nameLookup, session, updateUser, usersList} from '../api.js';
import {when} from '../format.js';
import {ignoreNavigationCancellation, refresh} from '../router.js';
import {busy, button, emptyState, facts, iconButton, link, pageHead, panel} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, onSubmit, passwordInput, trackFormDirty, validateBytes} from '../ui/forms.js';
import {tabSet} from '../ui/tabs.js';
import {issueEnrollment} from './enrollments.js';
import {auditItem} from './shared.js';
import {dataOperationsTab} from './data-operations.js';
import {showTask} from './backup-task.js';

export async function settingsPage() {
  await ensureSession();
  const [users, groups, nodes, audit] = await Promise.all([usersList(), groupsList(), allNodes(), api('/audit?limit=50&offset=0')]);
  const signal = getNavigationSignal();
  const task = new URLSearchParams(location.search).get('task');
  if (task !== null && location.hash === '#data') {
    requestAnimationFrame(() => { if (!signal?.aborted) showTask(task, {signal}); });
  }
  return h('div', null,
    pageHead(t('shell.settings'), t('shell.settingsIntro')),
    tabSet([
      {id: 'account', label: t('shell.account'), icon: 'user', build: accountTab},
      {id: 'administrators', label: t('shell.administrators'), icon: 'users', build: () => administratorsTab(users)},
      {id: 'groups', label: t('shell.groups'), icon: 'folder', build: () => groupsTab(groups, nodes)},
      {id: 'audit', label: t('shell.audit'), icon: 'list', build: () => auditTab(audit, nameLookup(nodes, users, groups))},
      {id: 'data', label: t('shell.data'), icon: 'cloud', build: dataOperationsTab},
      {id: 'public-page', label: t('shell.publicPage'), icon: 'globe', build: publicPageTab}
    ], t('shell.settingsSections')));
}

function publicPageTab() {
  const navigation = getNavigationSignal();
  const control = h('input', {type: 'checkbox', name: 'public_page_enabled', disabled: true});
  const note = h('p', {class: 'hint', role: 'status'}, t('shell.publicLoading'));
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
        title: t('shell.publicDisableTitle'),
        message: t('shell.publicDisableMessage'),
        confirm: t('shell.publicDisable'), danger: true
      }))) return;
      if (!active()) return;
      const settings = await api('/settings/public-page', {method: 'PATCH', body: {enabled: requested}, signal: controller.signal});
      if (!active()) return;
      enabled = settings.enabled;
      note.textContent = enabled ? t('shell.publicEnabled') : t('shell.publicDisabled');
    } catch (error) {
      if (active() && error.name !== 'AbortError') showError(error);
    } finally {
      pending = false;
      element.dataset.busy = 'false';
      control.checked = enabled;
      control.disabled = !active() || !session.csrf;
    }
  });
  const element = panel(t('shell.publicPage'), {},
    h('label', {class: 'switch'}, control, h('span', {class: 'switch-track', 'aria-hidden': 'true'}),
      h('span', {class: 'switch-text'}, h('strong', null, t('shell.publicEnable')), h('span', null, t('shell.publicEnableHint')))),
    h('p', {class: 'lede'}, t('shell.publicRoutingHint')),
    errorBox, note, retry);
  return {element,
    attach() { controller = new AbortController(); void load(); },
    detach() { controller?.abort(); }
  };
}

function accountTab() {
  const {user} = session;
  const disabled = !session.csrf;
  const oldPassword = passwordInput({name: 'old_password', autocomplete: 'current-password', required: true, disabled, id: 'pw-old'});
  const newPassword = passwordInput({name: 'new_password', autocomplete: 'new-password', required: true, disabled, id: 'pw-new'});
  const confirmPassword = input({type: 'password', name: 'confirm_password', autocomplete: 'new-password', required: true, disabled, id: 'pw-confirm'});
  validateBytes(oldPassword, 1, 1024, t('shell.currentPassword'));
  validateBytes(newPassword, 8, 1024, t('shell.newPassword'));
  const checkMatch = () => confirmPassword.setCustomValidity(confirmPassword.value && confirmPassword.value !== newPassword.value ? t('shell.passwordMismatch') : '');
  confirmPassword.addEventListener('input', checkMatch);
  newPassword.addEventListener('input', checkMatch);
  const submit = button(t('shell.changeAndSignOut'), {type: 'submit', variant: 'primary', disabled, id: 'pw-save'});
  const form = h('form', {class: 'form'},
    field(t('shell.currentPassword'), oldPassword, {inner: oldPassword.wrapper}),
    field(t('shell.newPassword'), newPassword, {inner: newPassword.wrapper, hint: t('shell.passwordBytes')}),
    field(t('shell.confirmPassword'), confirmPassword),
    h('div', {class: 'form-actions'}, submit));
  const clearPasswords = () => { oldPassword.value = ''; newPassword.value = ''; confirmPassword.value = ''; };
  trackFormDirty(form);
  getNavigationSignal()?.addEventListener('abort', clearPasswords, {once: true});
  window.addEventListener('pagehide', clearPasswords, {once: true, signal: getNavigationSignal() || undefined});
  onSubmit(form, submit, async () => {
    try {
      await api('/me/password', {method: 'PATCH', body: {old_password: oldPassword.value, new_password: newPassword.value}});
      clearPasswords();
      form.markClean();
      invalidateSession('password');
    } catch (error) {
      if (error.status === 400 && error.code === 'invalid_old_password') {
        oldPassword.showError(errorText(error));
        oldPassword.focus();
      } else if (error.status === 400 && error.code === 'password_unchanged') {
        newPassword.showError(errorText(error));
        newPassword.focus();
      } else if (error.status === 429 && error.code === 'rate_limited') {
        oldPassword.showError(errorText(error));
        oldPassword.focus();
      } else throw error;
    } finally {
      clearPasswords();
      form.syncDirty();
    }
  });
  return h('div', {class: 'split'},
    panel(t('shell.yourAccount'), {}, facts([
      [t('shell.username'), user.username],
      [t('shell.accountCreated'), when(user.created_at)], [t('shell.sessionExpires'), session.expires ? when(session.expires) : null]
    ])),
    panel(t('shell.changePassword'), {}, h('p', {class: 'lede'}, t('shell.passwordSignOutHint')), form));
}

function administratorsTab(users) {
  const self = (item) => item.id === session.user.id;
  const lastAdministrator = users.length === 1;

  const editUser = (item) => {
    let username;
    formDialog({
      title: t('shell.renameAdministrator'),
      submit: t('shell.save'),
      build: (form) => {
        username = input({name: 'username', required: true, value: item.username, autocomplete: 'off'});
        validateBytes(username, 1, 128, t('shell.username'));
        form.append(field(t('shell.username'), username));
      },
      action: async () => {
        const patch = {};
        if (username.value !== item.username) patch.username = username.value;
        if (Object.keys(patch).length === 0) return;
        await api(`/users/${encodeURIComponent(item.id)}`, {method: 'PATCH', body: patch});
        invalidate('users');
        if (self(item)) updateUser(patch);
        await refresh();
        toast(t('shell.savedName', {name: patch.username || item.username}));
      }
    });
  };

  const deleteUser = async (item) => {
    const confirmed = await confirmDialog({
      title: self(item) ? t('shell.deleteOwn') : t('shell.deleteAdministrator'),
      message: self(item) ? t('shell.deleteOwnMessage') : t('shell.deleteAdministratorMessage', {name: item.username}),
      confirm: self(item) ? t('shell.deleteMyAccount') : t('shell.deleteAdministrator'), danger: true
    });
    if (!confirmed) return;
    try {
      await api(`/users/${encodeURIComponent(item.id)}`, {method: 'DELETE'});
      if (self(item)) { invalidateSession('deleted'); return; }
      invalidate('users');
      await refresh();
      toast(t('shell.deletedName', {name: item.username}));
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
      title: t('shell.addAdministrator'),
      submit: t('shell.addAdministrator'),
      build: (form) => {
        username = input({name: 'username', required: true, autocomplete: 'off'});
        password = passwordInput({name: 'password', required: true, autocomplete: 'new-password'});
        mustChangePassword = input({type: 'checkbox', name: 'must_change_password', checked: false});
        validateBytes(username, 1, 128, t('shell.username'));
        validateBytes(password, 8, 1024, t('shell.password'));
        form.append(field(t('shell.username'), username),
          field(t('shell.initialPassword'), password, {inner: password.wrapper, hint: t('shell.initialPasswordHint')}),
          h('label', {class: 'data-check'}, mustChangePassword, h('span', null, t('shell.requirePasswordChange'))));
      },
      action: async () => {
        try {
          const created = await api('/users', {method: 'POST', body: {username: username.value, password: password.value, must_change_password: mustChangePassword.checked}});
          invalidate('users');
          await refresh();
          toast(t('shell.addedName', {name: created.username}));
        } finally { password.value = ''; }
      }
    });
  };

  const rows = users.map((item) => {
    const remove = iconButton('trash', t('shell.deleteAdministratorLabel', {name: item.username}), () => deleteUser(item));
    remove.disabled = lastAdministrator;
    return h('tr', null,
      h('td', null, h('div', {class: 'who administrator-who'}, h('span', {class: 'avatar', 'aria-hidden': 'true'}, item.username.slice(0, 1)),
        h('div', {class: 'administrator-name'}, h('span', {class: 'administrator-username', title: item.username}, item.username),
          self(item) ? h('span', {class: 'muted administrator-self'}, t('shell.you')) : null))),
      h('td', {class: 'muted hide-compact'}, when(item.created_at)),
      h('td', {class: 'actions'}, iconButton('edit', t('shell.renameAdministratorLabel', {name: item.username}), () => editUser(item)), remove));
  });

  return h('section', {class: 'panel', 'aria-labelledby': 'administrators-title'},
    h('div', {class: 'panel-head'}, h('h2', {id: 'administrators-title'}, t('shell.administrators')), button(t('shell.addAdministrator'), {icon: 'plus', variant: 'primary', small: true, onClick: createUser})),
    h('div', {class: 'panel-body flush'}, h('p', {class: 'lede'}, t('shell.fullAccess'))),
    h('div', {class: 'table-wrap'}, h('table', {class: 'table administrators-table'},
      h('thead', null, h('tr', null, h('th', {scope: 'col'}, t('shell.username')), h('th', {scope: 'col', class: 'hide-compact'}, t('shell.created')),
        h('th', {scope: 'col'}, h('span', {class: 'sr'}, t('shell.actions'))))),
      h('tbody', null, users.length ? rows : h('tr', null, h('td', {colspan: 3}, t('shell.noAdministrators')))))),
    lastAdministrator ? h('p', {class: 'administrator-protection muted'}, t('shell.lastAdministrator')) : null);
}

function groupsTab(groups, nodes) {
  const batchEnrollment = async (item, trigger) => {
    busy(trigger, true);
    try {
      await issueEnrollment({group_id: item.id, mode: 'batch'}, t('shell.batchEnrollmentFor', {name: item.name}));
    } catch (error) { toastError(error); }
    finally { busy(trigger, false); }
  };
  const groupForm = (item) => {
    let name;
    formDialog({
      title: item ? t('shell.renameName', {name: item.name}) : t('shell.addGroup'),
      submit: item ? t('shell.renameGroup') : t('shell.addGroup'),
      build: (form) => {
        name = input({name: 'name', required: true, value: item ? item.name : '', autocomplete: 'off'});
        validateBytes(name, 1, 255, t('shell.groupName'));
        form.append(field(t('shell.groupName'), name));
      },
      action: async () => {
        const saved = item
          ? await api(`/groups/${encodeURIComponent(item.id)}`, {method: 'PATCH', body: {name: name.value}})
          : await api('/groups', {method: 'POST', body: {name: name.value}});
        invalidate('groups');
        await refresh();
        toast(t(item ? 'shell.renamedName' : 'shell.addedName', {name: saved.name}));
      }
    });
  };
  const deleteGroup = async (item, size) => {
    const confirmed = await confirmDialog({
      title: t('shell.deleteNameQuestion', {name: item.name}),
      message: size ? t('shell.ungroupNodes', {count: size}) : t('shell.emptyGroup'),
      confirm: t('shell.deleteGroup'), danger: true
    });
    if (!confirmed) return;
    try {
      await api(`/groups/${encodeURIComponent(item.id)}`, {method: 'DELETE'});
      invalidate('groups', 'nodes');
      await refresh();
      toast(t('shell.deletedName', {name: item.name}));
    } catch (error) { toastError(error); }
  };
  const addButton = button(t('shell.addGroup'), {icon: 'plus', variant: 'primary', small: !!groups.length, onClick: () => groupForm()});
  if (!groups.length) return panel(t('shell.groups'), {}, emptyState(t('shell.noGroups'), t('shell.groupsIntro'), addButton));
  return panel(t('shell.groups'), {action: addButton},
    h('p', {class: 'lede'}, t('shell.groupsHint')),
    h('ul', {class: 'rows'}, groups.map((item) => {
      const size = nodes.filter((node) => node.group_id === item.id).length;
      const enrollment = button(t('shell.batchEnrollment'), {small: true});
      enrollment.addEventListener('click', () => batchEnrollment(item, enrollment));
      return h('li', null, icon('folder', 'muted'),
        h('div', {class: 'grow'}, h('div', {class: 'title'}, item.name), h('div', {class: 'sub'}, link(`/nodes?group=${item.id}`, t('shell.nodeCount', {count: size})))),
        [enrollment, iconButton('edit', t('shell.renameName', {name: item.name}), () => groupForm(item)), iconButton('trash', t('shell.deleteName', {name: item.name}), () => deleteGroup(item, size))]);
    })));
}

function auditTab(page, names) {
  const {events} = page;
  let cursor = page.next_cursor;
  const eventIDs = new Set(events.map((event) => event.id));
  const list = h('ol', {class: 'timeline'}, events.map((event) => auditItem(event, names, true)));
  const more = button(t('shell.olderEvents'), {onClick: async () => {
    busy(more, true);
    try {
      const nextPage = await api(`/audit?limit=50&before=${encodeURIComponent(cursor)}`);
      cursor = nextPage.next_cursor;
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
  return panel(t('shell.audit'), {}, h('p', {class: 'lede'}, t('shell.auditHint')),
    events.length ? [list, more] : emptyState(t('shell.noAudit'), t('shell.noAuditHint')));
}
