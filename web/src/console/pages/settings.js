import {h, icon} from '../../shared/dom.js';
import {allNodes, api, ensureSession, getNavigationSignal, groupsList, invalidate, invalidateSession, nameLookup, session, updateUser, usersList} from '../api.js';
import {when} from '../format.js';
import {ignoreNavigationCancellation, refresh} from '../router.js';
import {busy, button, emptyState, facts, iconButton, link, pageHead, panel} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, onSubmit, passwordInput, validateBytes} from '../ui/forms.js';
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
    pageHead('Settings', 'Manage your account, administrators, node groups, audit history and backups.'),
    tabSet([
      {id: 'account', label: 'Account', icon: 'user', build: accountTab},
      {id: 'administrators', label: 'Administrators', icon: 'users', build: () => administratorsTab(users)},
      {id: 'groups', label: 'Groups', icon: 'folder', build: () => groupsTab(groups, nodes)},
      {id: 'audit', label: 'Audit log', icon: 'list', build: () => auditTab(audit, nameLookup(nodes, users, groups))},
      {id: 'data', label: 'Data operations', icon: 'cloud', build: dataOperationsTab},
      {id: 'public-page', label: 'Public page', icon: 'globe', build: publicPageTab}
    ], 'Settings sections'));
}

function publicPageTab() {
  const navigation = getNavigationSignal();
  const control = h('input', {type: 'checkbox', name: 'public_page_enabled', disabled: true});
  const note = h('p', {class: 'hint', role: 'status'}, 'Loading public page settings…');
  const errorBox = h('div', {class: 'form-error', role: 'alert', hidden: true});
  const retry = button('Try again', {onClick: () => { void load(); }});
  retry.hidden = true;
  let controller;
  let enabled;
  let pending = false;
  const active = () => controller && !controller.signal.aborted && !navigation?.aborted;
  const showError = (error) => {
    errorBox.textContent = error.message;
    errorBox.hidden = false;
  };
  const load = async () => {
    if (!active() || pending) return;
    pending = true;
    control.disabled = true;
    retry.hidden = true;
    errorBox.hidden = true;
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
      control.disabled = !active() || typeof enabled !== 'boolean' || !session.csrf;
    }
  };
  control.addEventListener('change', async () => {
    const requested = control.checked;
    control.checked = enabled;
    if (!active() || pending || requested === enabled || !session.csrf) return;
    pending = true;
    control.disabled = true;
    errorBox.hidden = true;
    note.textContent = '';
    try {
      if (!requested && !(await confirmDialog({
        title: 'Disable public page?',
        message: 'Anonymous access, public APIs and live streams will stop. Signed-in visitors will enter the console; visitors who must change their password will enter the password change page.',
        confirm: 'Disable public page', danger: true
      }))) return;
      if (!active()) return;
      const settings = await api('/settings/public-page', {method: 'PATCH', body: {enabled: requested}, signal: controller.signal});
      if (!active()) return;
      enabled = settings.enabled;
      note.textContent = enabled ? 'Public page enabled.' : 'Public page disabled.';
    } catch (error) {
      if (active() && error.name !== 'AbortError') showError(error);
    } finally {
      pending = false;
      control.checked = enabled;
      control.disabled = !active() || !session.csrf;
    }
  });
  const element = panel('Public page', {},
    h('label', {class: 'switch'}, control, h('span', {class: 'switch-track', 'aria-hidden': 'true'}),
      h('span', {class: 'switch-text'}, h('strong', null, 'Enable public page'), h('span', null, 'Allow anonymous access to the public status page, public APIs and live streams.'))),
    h('p', {class: 'lede'}, 'When disabled, signed-in visitors enter the console and anonymous visitors go to sign in. Accounts that require a password change go to the password change page. Enabling this setting does not publish private nodes.'),
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
  validateBytes(oldPassword, 1, 1024, 'Current password');
  validateBytes(newPassword, 8, 1024, 'New password');
  const checkMatch = () => confirmPassword.setCustomValidity(confirmPassword.value && confirmPassword.value !== newPassword.value ? 'Passwords do not match.' : '');
  confirmPassword.addEventListener('input', checkMatch);
  newPassword.addEventListener('input', checkMatch);
  const submit = button('Change password and sign out', {type: 'submit', variant: 'primary', disabled, id: 'pw-save'});
  const form = h('form', {class: 'form'},
    field('Current password', oldPassword, {inner: oldPassword.wrapper}),
    field('New password', newPassword, {inner: newPassword.wrapper, hint: '8–1024 UTF-8 bytes.'}),
    field('Confirm new password', confirmPassword),
    h('div', {class: 'form-actions'}, submit));
  const clearPasswords = () => { oldPassword.value = ''; newPassword.value = ''; confirmPassword.value = ''; };
  getNavigationSignal()?.addEventListener('abort', clearPasswords, {once: true});
  window.addEventListener('pagehide', clearPasswords, {once: true, signal: getNavigationSignal() || undefined});
  onSubmit(form, submit, async () => {
    try {
      await api('/me/password', {method: 'PATCH', body: {old_password: oldPassword.value, new_password: newPassword.value}});
      clearPasswords();
      invalidateSession('password');
    } catch (error) {
      if (error.status === 400 && error.code === 'invalid_old_password') {
        oldPassword.showError('The current password is incorrect.');
        oldPassword.focus();
      } else if (error.status === 400 && error.code === 'password_unchanged') {
        newPassword.showError('Choose a password different from your current password.');
        newPassword.focus();
      } else if (error.status === 429 && error.code === 'rate_limited') {
        oldPassword.showError('Too many password change attempts. Wait a minute and try again.');
        oldPassword.focus();
      } else throw error;
    } finally {
      clearPasswords();
    }
  });
  return h('div', {class: 'split'},
    panel('Your account', {}, facts([
      ['Username', user.username],
      ['Account created', when(user.created_at)], ['Session expires', session.expires ? when(session.expires) : null]
    ])),
    panel('Change password', {}, h('p', {class: 'lede'}, 'Changing your password signs out every session, including this one.'), form));
}

function administratorsTab(users) {
  const self = (item) => item.id === session.user.id;
  const lastAdministrator = users.length === 1;

  const editUser = (item) => {
    let username;
    formDialog({
      title: 'Rename administrator',
      submit: 'Save',
      build: (form) => {
        username = input({name: 'username', required: true, value: item.username, autocomplete: 'off'});
        validateBytes(username, 1, 128, 'Username');
        form.append(field('Username', username));
      },
      action: async () => {
        const patch = {};
        if (username.value !== item.username) patch.username = username.value;
        if (Object.keys(patch).length === 0) return;
        await api(`/users/${encodeURIComponent(item.id)}`, {method: 'PATCH', body: patch});
        invalidate('users');
        if (self(item)) updateUser(patch);
        await refresh();
        toast(`Saved ${patch.username || item.username}.`);
      }
    });
  };

  const deleteUser = async (item) => {
    const confirmed = await confirmDialog({
      title: self(item) ? 'Delete your account' : 'Delete administrator',
      message: self(item) ? 'You are deleting your own account. Every session ends and you will be signed out.' : `Delete "${item.username}"? Their sessions will end immediately. This cannot be undone.`,
      confirm: self(item) ? 'Delete my account' : 'Delete administrator', danger: true
    });
    if (!confirmed) return;
    try {
      await api(`/users/${encodeURIComponent(item.id)}`, {method: 'DELETE'});
      if (self(item)) { invalidateSession('deleted'); return; }
      invalidate('users');
      await refresh();
      toast(`Deleted ${item.username}.`);
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
      title: 'Add administrator',
      submit: 'Add administrator',
      build: (form) => {
        username = input({name: 'username', required: true, autocomplete: 'off'});
        password = passwordInput({name: 'password', required: true, autocomplete: 'new-password'});
        mustChangePassword = input({type: 'checkbox', name: 'must_change_password', checked: false});
        validateBytes(username, 1, 128, 'Username');
        validateBytes(password, 8, 1024, 'Password');
        form.append(field('Username', username),
          field('Initial password', password, {inner: password.wrapper, hint: '8–1024 UTF-8 bytes. Share it with the administrator through a secure channel.'}),
          h('label', {class: 'data-check'}, mustChangePassword, h('span', null, 'Require password change on first sign-in')));
      },
      action: async () => {
        try {
          const created = await api('/users', {method: 'POST', body: {username: username.value, password: password.value, must_change_password: mustChangePassword.checked}});
          invalidate('users');
          await refresh();
          toast(`Added ${created.username}.`);
        } finally { password.value = ''; }
      }
    });
  };

  const rows = users.map((item) => {
    const remove = iconButton('trash', `Delete administrator "${item.username}"`, () => deleteUser(item));
    remove.disabled = lastAdministrator;
    return h('tr', null,
      h('td', null, h('div', {class: 'who administrator-who'}, h('span', {class: 'avatar', 'aria-hidden': 'true'}, item.username.slice(0, 1)),
        h('div', {class: 'administrator-name'}, h('span', {class: 'administrator-username', title: item.username}, item.username),
          self(item) ? h('span', {class: 'muted administrator-self'}, '(you)') : null))),
      h('td', {class: 'muted hide-compact'}, when(item.created_at)),
      h('td', {class: 'actions'}, iconButton('edit', `Rename administrator "${item.username}"`, () => editUser(item)), remove));
  });

  return h('section', {class: 'panel', 'aria-labelledby': 'administrators-title'},
    h('div', {class: 'panel-head'}, h('h2', {id: 'administrators-title'}, 'Administrators'), button('Add administrator', {icon: 'plus', variant: 'primary', small: true, onClick: createUser})),
    h('div', {class: 'panel-body flush'}, h('p', {class: 'lede'}, 'All administrators have full access.')),
    h('div', {class: 'table-wrap'}, h('table', {class: 'table administrators-table'},
      h('thead', null, h('tr', null, h('th', {scope: 'col'}, 'Username'), h('th', {scope: 'col', class: 'hide-compact'}, 'Created'),
        h('th', {scope: 'col'}, h('span', {class: 'sr'}, 'Actions')))),
      h('tbody', null, users.length ? rows : h('tr', null, h('td', {colspan: 3}, 'No administrators.'))))),
    lastAdministrator ? h('p', {class: 'administrator-protection muted'}, 'At least one administrator is required.') : null);
}

function groupsTab(groups, nodes) {
  const batchEnrollment = async (item, trigger) => {
    busy(trigger, true);
    try {
      await issueEnrollment({group_id: item.id, mode: 'batch'}, `Batch enrollment ticket for ${item.name}`);
    } catch (error) { toastError(error); }
    finally { busy(trigger, false); }
  };
  const groupForm = (item) => {
    let name;
    formDialog({
      title: item ? `Rename ${item.name}` : 'Add group',
      submit: item ? 'Rename group' : 'Add group',
      build: (form) => {
        name = input({name: 'name', required: true, value: item ? item.name : '', autocomplete: 'off'});
        validateBytes(name, 1, 255, 'Group name');
        form.append(field('Group name', name));
      },
      action: async () => {
        const saved = item
          ? await api(`/groups/${encodeURIComponent(item.id)}`, {method: 'PATCH', body: {name: name.value}})
          : await api('/groups', {method: 'POST', body: {name: name.value}});
        invalidate('groups');
        await refresh();
        toast(item ? `Renamed to ${saved.name}.` : `Added ${saved.name}.`);
      }
    });
  };
  const deleteGroup = async (item, size) => {
    const confirmed = await confirmDialog({
      title: `Delete ${item.name}?`,
      message: size ? `Its ${size} ${size === 1 ? 'node becomes' : 'nodes become'} ungrouped. The nodes themselves are kept.` : 'This group has no nodes.',
      confirm: 'Delete group', danger: true
    });
    if (!confirmed) return;
    try {
      await api(`/groups/${encodeURIComponent(item.id)}`, {method: 'DELETE'});
      invalidate('groups', 'nodes');
      await refresh();
      toast(`Deleted ${item.name}.`);
    } catch (error) { toastError(error); }
  };
  const addButton = button('Add group', {icon: 'plus', variant: 'primary', small: !!groups.length, onClick: () => groupForm()});
  if (!groups.length) return panel('Groups', {}, emptyState('No groups yet', 'Groups organize nodes in the console. They do not change agent behaviour.', addButton));
  return panel('Groups', {action: addButton},
    h('p', {class: 'lede'}, 'Batch enrollment tickets expire after 30 minutes and can enroll up to 100 agents. Deleting a group keeps its nodes and leaves them ungrouped.'),
    h('ul', {class: 'rows'}, groups.map((item) => {
      const size = nodes.filter((node) => node.group_id === item.id).length;
      const enrollment = button('Batch enrollment ticket', {small: true});
      enrollment.addEventListener('click', () => batchEnrollment(item, enrollment));
      return h('li', null, icon('folder', 'muted'),
        h('div', {class: 'grow'}, h('div', {class: 'title'}, item.name), h('div', {class: 'sub'}, link(`/nodes?group=${item.id}`, size === 1 ? '1 node' : `${size} nodes`))),
        [enrollment, iconButton('edit', `Rename ${item.name}`, () => groupForm(item)), iconButton('trash', `Delete ${item.name}`, () => deleteGroup(item, size))]);
    })));
}

function auditTab(page, names) {
  const {events} = page;
  let cursor = page.next_cursor;
  const eventIDs = new Set(events.map((event) => event.id));
  const list = h('ol', {class: 'timeline'}, events.map((event) => auditItem(event, names, true)));
  const more = button('Load older events', {onClick: async () => {
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
  return panel('Audit log', {}, h('p', {class: 'lede'}, 'Newest first. Event details never contain credentials.'),
    events.length ? [list, more] : emptyState('No audit events', 'Changes made in the console or API are recorded here.'));
}
