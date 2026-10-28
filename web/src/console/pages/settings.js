import {h, icon} from '../../shared/dom.js';
import {allNodes, api, ensureSession, getNavigationSignal, groupsList, invalidate, nameLookup, session, updateUser, usersList} from '../api.js';
import {when} from '../format.js';
import {refresh} from '../router.js';
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
    pageHead('Settings', 'Your account, people with console access, node groups and the audit trail.'),
    tabSet([
      {id: 'account', label: 'Account', icon: 'user', build: accountTab},
      {id: 'users', label: 'Users', icon: 'users', build: () => usersTab(users)},
      {id: 'groups', label: 'Groups', icon: 'folder', build: () => groupsTab(groups, nodes)},
      {id: 'audit', label: 'Audit log', icon: 'list', build: () => auditTab(audit, nameLookup(nodes, users, groups))},
      {id: 'data', label: 'Data operations', icon: 'cloud', build: dataOperationsTab}
    ], 'Settings sections'));
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
  onSubmit(form, submit, async () => {
    try {
      await api('/me/password', {method: 'PATCH', body: {old_password: oldPassword.value, new_password: newPassword.value}});
      session.csrf = '';
      location.assign('/login?password-changed');
    } catch (error) {
      if (error.status === 400 && error.code === 'invalid_old_password') {
        oldPassword.showError('The current password is incorrect.');
        oldPassword.focus();
      } else if (error.status === 429 && error.code === 'rate_limited') {
        oldPassword.showError('Too many password change attempts. Wait a minute and try again.');
        oldPassword.focus();
      } else throw error;
    } finally {
      oldPassword.value = '';
      newPassword.value = '';
      confirmPassword.value = '';
    }
  });
  return h('div', {class: 'split'},
    panel('Your account', {}, facts([
      ['Username', user.username],
      ['Account created', when(user.created_at)], ['Session expires', session.expires ? when(session.expires) : null]
    ])),
    panel('Change password', {}, h('p', {class: 'lede'}, 'Changing your password signs out every session, including this one.'), form));
}

function usersTab(users) {
  const self = (item) => item.id === session.user.id;

  const editUser = (item) => {
    let username;
    formDialog({
      title: `Edit ${item.username}`,
      submit: 'Save user',
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
      title: `Delete ${item.username}?`,
      message: self(item) ? 'You are deleting your own account. Every session ends and you will be signed out.' : 'Their sessions end immediately. This cannot be undone.',
      confirm: 'Delete user', danger: true
    });
    if (!confirmed) return;
    try {
      await api(`/users/${encodeURIComponent(item.id)}`, {method: 'DELETE'});
      if (self(item)) { location.assign('/login'); return; }
      invalidate('users');
      await refresh();
      toast(`Deleted ${item.username}.`);
    } catch (error) { toastError(error); }
  };

  const createUser = () => {
    let username;
    let password;
    formDialog({
      title: 'Add user',
      submit: 'Add user',
      build: (form) => {
        username = input({name: 'username', required: true, autocomplete: 'off'});
        password = passwordInput({name: 'password', required: true, autocomplete: 'new-password'});
        validateBytes(username, 1, 128, 'Username');
        validateBytes(password, 8, 1024, 'Password');
        form.append(field('Username', username),
          field('Initial password', password, {inner: password.wrapper, hint: '8–1024 UTF-8 bytes. Share it with the user through a secure channel.'}));
      },
      action: async () => {
        try {
          const created = await api('/users', {method: 'POST', body: {username: username.value, password: password.value}});
          invalidate('users');
          await refresh();
          toast(`Added ${created.username}.`);
        } finally { password.value = ''; }
      }
    });
  };

  const rows = users.map((item) => h('tr', null,
    h('td', null, h('div', {class: 'who'}, h('span', {class: 'avatar', 'aria-hidden': 'true'}, item.username.slice(0, 1)),
      h('div', {class: 'who-text'}, h('div', {class: 'who-name'}, item.username, self(item) ? h('span', {class: 'muted'}, ' (you)') : null)))),
    h('td', {class: 'muted hide-sm'}, when(item.created_at)),
    h('td', {class: 'actions'}, iconButton('edit', `Edit ${item.username}`, () => editUser(item)), iconButton('trash', `Delete ${item.username}`, () => deleteUser(item)))));

  return h('section', {class: 'panel', 'aria-labelledby': 'users-title'},
    h('div', {class: 'panel-head'}, h('h2', {id: 'users-title'}, 'Users'), button('Add user', {icon: 'plus', variant: 'primary', small: true, onClick: createUser})),
    h('div', {class: 'panel-body flush'}, h('p', {class: 'lede'}, 'All administrators have full access. The last administrator cannot be removed.')),
    users.length ? h('div', {class: 'table-wrap'}, h('table', {class: 'table'},
      h('thead', null, h('tr', null, h('th', {scope: 'col'}, 'User'), h('th', {scope: 'col', class: 'hide-sm'}, 'Created'),
        h('th', {scope: 'col'}, h('span', {class: 'sr'}, 'Actions')))),
      h('tbody', null, rows)))
      : emptyState('No users returned', null));
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
