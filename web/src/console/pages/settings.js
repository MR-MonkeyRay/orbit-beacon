import {h, icon} from '../../shared/dom.js';
import {allNodes, api, ensureSession, groupsList, invalidate, isAdmin, nameLookup, session, updateUser, usersList} from '../api.js';
import {ROLE, when} from '../format.js';
import {refresh} from '../router.js';
import {busy, button, callout, emptyState, facts, iconButton, link, pageHead, panel} from '../ui/components.js';
import {confirmDialog, dialogActionStale, formDialog, modal} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, onSubmit, passwordInput, select, validateBytes} from '../ui/forms.js';
import {tabSet} from '../ui/tabs.js';
import {issueEnrollment} from './enrollments.js';
import {auditItem} from './shared.js';
import {apiTokenExpiry, apiTokenState} from './api-token-utils.js';

const ROLE_CHOICES = [['viewer', 'Viewer'], ['admin', 'Administrator']];
const roleBadge = (role) => h('span', {class: `badge ${role}`}, ROLE[role]);

export async function settingsPage() {
  await ensureSession();
  const [users, groups, nodes, audit] = await Promise.all([usersList(), groupsList(), allNodes(), api('/audit?limit=50&offset=0')]);
  return h('div', null,
    pageHead('Settings', 'Your account, people with console access, node groups and the audit trail.'),
    tabSet([
      {id: 'account', label: 'Account', icon: 'user', build: accountTab},
      {id: 'users', label: 'Users', icon: 'users', build: () => usersTab(users)},
      {id: 'groups', label: 'Groups', icon: 'folder', build: () => groupsTab(groups, nodes)},
      ...(isAdmin() ? [{id: 'api-tokens', label: 'API tokens', icon: 'key', build: apiTokensTab}] : []),
      {id: 'audit', label: 'Audit log', icon: 'list', build: () => auditTab(audit, nameLookup(nodes, users, groups))}
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
      ['Username', user.username], ['Role', roleBadge(user.role)],
      ['Account created', when(user.created_at)], ['Session expires', session.expires ? when(session.expires) : null]
    ])),
    panel('Change password', {}, h('p', {class: 'lede'}, 'Changing your password signs out every session, including this one.'), form));
}

function usersTab(users) {
  const editable = isAdmin();
  const self = (item) => item.id === session.user.id;

  const editUser = (item) => {
    let username;
    let role;
    formDialog({
      title: `Edit ${item.username}`,
      intro: self(item) ? 'Changing your own role signs you out.' : 'Changing the role signs this user out of every session.',
      submit: 'Save user',
      build: (form) => {
        username = input({name: 'username', required: true, value: item.username, autocomplete: 'off'});
        validateBytes(username, 1, 128, 'Username');
        role = select(ROLE_CHOICES, item.role, {name: 'role'});
        form.append(field('Username', username), field('Role', role));
      },
      action: async () => {
        const patch = {};
        if (username.value !== item.username) patch.username = username.value;
        if (role.value !== item.role) patch.role = role.value;
        if (Object.keys(patch).length === 0) return;
        await api(`/users/${encodeURIComponent(item.id)}`, {method: 'PATCH', body: patch});
        if (self(item) && patch.role) { location.assign('/login'); return; }
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
    let role;
    formDialog({
      title: 'Add user',
      submit: 'Add user',
      build: (form) => {
        username = input({name: 'username', required: true, autocomplete: 'off'});
        password = passwordInput({name: 'password', required: true, autocomplete: 'new-password'});
        role = select(ROLE_CHOICES, 'viewer', {name: 'role'});
        validateBytes(username, 1, 128, 'Username');
        validateBytes(password, 8, 1024, 'Password');
        form.append(field('Username', username),
          field('Initial password', password, {inner: password.wrapper, hint: '8–1024 UTF-8 bytes. Share it with the user through a secure channel.'}),
          field('Role', role, {hint: 'Viewers can read everything; administrators can also make changes.'}));
      },
      action: async () => {
        try {
          const created = await api('/users', {method: 'POST', body: {username: username.value, password: password.value, role: role.value}});
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
    h('td', null, roleBadge(item.role)),
    h('td', {class: 'muted hide-sm'}, when(item.created_at)),
    editable ? h('td', {class: 'actions'}, iconButton('edit', `Edit ${item.username}`, () => editUser(item)), iconButton('trash', `Delete ${item.username}`, () => deleteUser(item))) : null));

  return h('section', {class: 'panel', 'aria-labelledby': 'users-title'},
    h('div', {class: 'panel-head'}, h('h2', {id: 'users-title'}, 'Users'), editable ? button('Add user', {icon: 'plus', variant: 'primary', small: true, onClick: createUser}) : null),
    h('div', {class: 'panel-body flush'}, h('p', {class: 'lede'}, 'Viewers can read everything. Administrators can also make changes. The last administrator cannot be removed or demoted.')),
    users.length ? h('div', {class: 'table-wrap'}, h('table', {class: 'table'},
      h('thead', null, h('tr', null, h('th', {scope: 'col'}, 'User'), h('th', {scope: 'col'}, 'Role'), h('th', {scope: 'col', class: 'hide-sm'}, 'Created'),
        editable ? h('th', {scope: 'col'}, h('span', {class: 'sr'}, 'Actions')) : null)),
      h('tbody', null, rows)))
      : emptyState('No users returned', null));
}

function groupsTab(groups, nodes) {
  const editable = isAdmin();
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
  const addButton = editable ? button('Add group', {icon: 'plus', variant: 'primary', small: !!groups.length, onClick: () => groupForm()}) : null;
  if (!groups.length) return panel('Groups', {}, emptyState('No groups yet', 'Groups organize nodes in the console. They do not change agent behaviour.', addButton));
  return panel('Groups', {action: addButton},
    h('p', {class: 'lede'}, editable
      ? 'Batch enrollment tickets expire after 30 minutes and can enroll up to 100 agents. Deleting a group keeps its nodes and leaves them ungrouped.'
      : 'Deleting a group keeps its nodes and leaves them ungrouped.'),
    h('ul', {class: 'rows'}, groups.map((item) => {
      const size = nodes.filter((node) => node.group_id === item.id).length;
      const enrollment = editable ? button('Batch enrollment ticket', {small: true}) : null;
      if (enrollment) enrollment.addEventListener('click', () => batchEnrollment(item, enrollment));
      return h('li', null, icon('folder', 'muted'),
        h('div', {class: 'grow'}, h('div', {class: 'title'}, item.name), h('div', {class: 'sub'}, link(`/nodes?group=${item.id}`, size === 1 ? '1 node' : `${size} nodes`))),
        editable ? [enrollment, iconButton('edit', `Rename ${item.name}`, () => groupForm(item)), iconButton('trash', `Delete ${item.name}`, () => deleteGroup(item, size))] : null);
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

function apiTokensTab() {
  const list = h('div', {'aria-live': 'polite'});
  const error = h('p', {class: 'callout', role: 'alert', hidden: true});
  let loading = false;
  const reload = async () => {
    if (loading) return;
    loading = true;
    error.hidden = true;
    busy(retry, true);
    try {
      const {api_tokens: tokens} = await api('/api-tokens');
      list.replaceChildren(tokens.length ? h('div', {class: 'table-wrap'},
        h('table', {class: 'table'}, h('caption', {class: 'sr'}, 'API tokens'),
          h('thead', null, h('tr', null, ['Name', 'Prefix', 'Created by / at', 'Expires', 'Last used', 'State', 'Actions'].map((label) => h('th', {scope: 'col'}, label)))),
          h('tbody', null, tokens.map((token) => h('tr', null,
            h('td', null, token.name), h('td', {class: 'mono'}, token.prefix),
            h('td', null, token.created_by_username || token.created_by, h('div', {class: 'muted'}, when(token.created_at))),
            h('td', null, token.expires_at ? when(token.expires_at) : 'Never'),
            h('td', null, token.last_used_at ? when(token.last_used_at) : 'Never'),
            h('td', null, apiTokenState(token)),
            h('td', null, apiTokenState(token) === 'Active' ? button('Revoke', {small: true, variant: 'danger', onClick: async (event) => {
              const control = event.currentTarget;
              if (!(await confirmDialog({title: `Revoke ${token.name}?`, message: 'Automation using this token will lose access immediately.', confirm: 'Revoke token', danger: true}))) return;
              busy(control, true);
              try { await api(`/api-tokens/${encodeURIComponent(token.id)}`, {method: 'DELETE'}); await reload(); }
              catch (failure) { toastError(failure); }
              finally { busy(control, false); }
            }}) : null)))))) : emptyState('No API tokens', 'Create a token for scripts or monitoring tools.'));
    } catch (failure) {
      if (failure.name !== 'AbortError') { error.textContent = failure.message; error.hidden = false; }
    } finally { loading = false; busy(retry, false); }
  };
  const retry = button('Refresh tokens', {small: true, icon: 'refresh', onClick: reload});
  const create = () => {
    let name, expiry, created;
    const {closed} = formDialog({
      title: 'Create API token', submit: 'Create token',
      intro: 'Read-only access to management data. Account, user, enrollment and token management endpoints are excluded. Maximum 50 active tokens.',
      build: (form) => {
        name = input({name: 'name', required: true, autocomplete: 'off'});
        validateBytes(name, 1, 100, 'Token name');
        expiry = select([['1', '1 day'], ['7', '7 days'], ['30', '30 days'], ['90', '90 days'], ['365', '365 days'], ['never', 'Never']], '30', {name: 'expiry'});
        form.append(field('Name', name), field('Expires after', expiry));
      },
      action: async () => {
        created = await api('/api-tokens', {method: 'POST', body: {name: name.value, expires_in_days: apiTokenExpiry(expiry.value)}});
        if (dialogActionStale()) { delete created.token; created = null; }
      }
    });
    closed.then(async (result) => {
      if (!created) return;
      if (result !== 'done') { delete created.token; return; }
      const secret = h('code', null, created.token);
      delete created.token;
      created = null;
      const status = h('p', {class: 'hint', role: 'status'});
      const {closed: secretClosed} = modal('API token created', (close) => h('div', null,
        h('div', {class: 'dialog-body stack'},
          callout('This token is shown only once. Copy it now; closing this dialog clears it.'),
          h('pre', {class: 'code enrollment-token'}, secret),
          button('Copy token', {icon: 'copy', onClick: async () => {
            try {
              if (!navigator.clipboard?.writeText) throw new Error('Clipboard API unavailable');
              await navigator.clipboard.writeText(secret.textContent);
              status.textContent = 'Copied.';
            } catch {
              const range = document.createRange(); range.selectNodeContents(secret);
              const selection = getSelection(); selection.removeAllRanges(); selection.addRange(range);
              let copied = false;
              try { copied = document.execCommand('copy'); } catch {}
              status.textContent = copied ? 'Copied using the browser fallback.' : 'The token is selected. Press Ctrl+C or Command+C to copy it.';
            }
          }}), status,
          h('p', {class: 'lede'}, 'Send it as Authorization: Bearer <token> over HTTPS. Never put it in a URL.')),
        h('div', {class: 'dialog-foot'}, button('Close', {onClick: () => close('done')}))));
      try { await secretClosed; } finally { secret.textContent = ''; }
      await reload();
    }).catch(toastError);
  };
  const content = panel('API tokens', {action: button('Create token', {variant: 'primary', icon: 'plus', onClick: create})},
    h('p', {class: 'lede'}, 'Read-only credentials for automation. Tokens are shown only at creation. Last used updates at most once per minute and may lag while storage is busy.'),
    retry, error, list);
  reload();
  return content;
}
