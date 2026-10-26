'use strict';

const content = document.getElementById('content');
const message = document.getElementById('message');
let csrf = '';
const utf8 = new TextEncoder();

async function api(path, options = {}) {
  const headers = {'Content-Type': 'application/json', ...options.headers};
  if (options.method && options.method !== 'GET') headers['X-CSRF-Token'] = csrf;
  const response = await fetch(`/api/v1${path}`, {...options, headers, credentials: 'same-origin'});
  if (response.status === 204) return null;
  const value = await response.json();
  if (!response.ok) {
    if (response.status === 401 && location.pathname !== '/login') location.assign('/login');
    throw new Error(`${value.message} (${value.request_id})`);
  }
  return value;
}

function showError(error) { message.textContent = error.message; }
function text(tag, value) {
  const element = document.createElement(tag);
  element.textContent = value;
  return element;
}

function panel(title) {
  const section = document.createElement('section');
  section.append(text('h2', title));
  content.append(section);
  return section;
}

function byteLimit(input, min, max, perLine = false) {
  const validate = () => {
    const values = perLine ? input.value.split('\n').filter((value) => value !== '') : [input.value];
    const invalid = values.some((value) => {
      const size = utf8.encode(value).length;
      return size < min || size > max;
    });
    input.setCustomValidity(invalid ? `${perLine ? 'Each entry' : 'Value'} must use ${min}–${max} UTF-8 bytes.` : '');
  };
  input.addEventListener('input', validate);
  validate();
}

function field(form, labelText, name, value = '', options = {}) {
  const label = text('label', labelText);
  const input = document.createElement(options.multiline ? 'textarea' : 'input');
  input.name = name;
  input.value = value;
  if (!options.multiline) input.type = options.type || 'text';
  for (const key of ['required', 'autocomplete', 'disabled']) {
    if (options[key] !== undefined) input[key] = options[key];
  }
  if (options.multiline) input.rows = 4;
  if (options.maxBytes !== undefined) byteLimit(input, options.minBytes ?? 0, options.maxBytes, options.perLine);
  label.append(input);
  form.append(label);
  return input;
}

function selection(form, labelText, name, choices, value) {
  const label = text('label', labelText);
  const select = document.createElement('select');
  select.name = name;
  for (const [id, title] of choices) {
    const option = text('option', title);
    option.value = id;
    select.append(option);
  }
  select.value = value;
  label.append(select);
  form.append(label);
  return select;
}

function mutation(form, title, action) {
  const button = text('button', title);
  button.type = 'submit';
  form.append(button);
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    button.disabled = true;
    message.textContent = '';
    try { await action(); }
    catch (error) { showError(error); }
    finally { button.disabled = false; }
  });
}

function relogin() {
  csrf = '';
  location.assign('/login');
}

function passwordSettings() {
  const section = panel('Change your password');
  section.append(text('p', 'Your current password is required. Changing it signs out every session, including this one. Sign in again with your new password.'));
  const form = document.createElement('form');
  const oldPassword = field(form, 'Current password', 'old_password', '', {type: 'password', autocomplete: 'current-password', required: true, maxBytes: 1024, disabled: !csrf});
  const newPassword = field(form, 'New password (8–1024 UTF-8 bytes)', 'new_password', '', {type: 'password', autocomplete: 'new-password', required: true, minBytes: 8, maxBytes: 1024, disabled: !csrf});
  mutation(form, 'Change password and sign out', async () => {
    try {
      await api('/me/password', {method: 'PATCH', body: JSON.stringify({old_password: oldPassword.value, new_password: newPassword.value})});
      relogin();
    } finally {
      oldPassword.value = '';
      newPassword.value = '';
    }
  });
  form.querySelector('button').disabled = !csrf;
  section.append(form);
}

function userSettings(user, users) {
  const section = panel('Users');
  const editable = user.role === 'admin' && csrf;
  section.append(text('p', 'Both roles can view users. Administrators can manage them. Role changes revoke the affected user’s sessions; the last administrator cannot be removed or demoted.'));
  for (const item of users) {
    const entry = document.createElement('article');
    entry.append(text('h3', item.username), text('p', `Role: ${item.role} · ID: ${item.id} · Created: ${item.created_at}`));
    if (editable) {
      const form = document.createElement('form');
      const username = field(form, 'Username', 'username', item.username, {required: true, maxBytes: 128, autocomplete: 'off'});
      const role = selection(form, 'Role', 'role', [['viewer', 'Viewer'], ['admin', 'Administrator']], item.role);
      mutation(form, 'Save user', async () => {
        const patch = {};
        if (username.value !== item.username) patch.username = username.value;
        if (role.value !== item.role) patch.role = role.value;
        if (Object.keys(patch).length === 0) { message.textContent = 'No user changes to save.'; return; }
        await api(`/users/${encodeURIComponent(item.id)}`, {method: 'PATCH', body: JSON.stringify(patch)});
        if (item.id === user.id && patch.role) { relogin(); return; }
        await render();
        message.textContent = 'User saved.';
      });
      const remove = text('button', 'Delete user');
      remove.type = 'button';
      remove.addEventListener('click', async () => {
        if (!window.confirm(`Delete user ${item.username}? Their sessions will be revoked.`)) return;
        remove.disabled = true;
        try {
          await api(`/users/${encodeURIComponent(item.id)}`, {method: 'DELETE'});
          if (item.id === user.id) { relogin(); return; }
          await render();
          message.textContent = 'User deleted.';
        } catch (error) { showError(error); }
        finally { remove.disabled = false; }
      });
      form.append(remove);
      entry.append(form);
    }
    section.append(entry);
  }
  if (users.length === 0) section.append(text('p', 'No users returned.'));
  if (editable) {
    section.append(text('h3', 'Create user'));
    const form = document.createElement('form');
    const username = field(form, 'New username', 'username', '', {required: true, maxBytes: 128, autocomplete: 'off'});
    const password = field(form, 'Initial password (8–1024 UTF-8 bytes)', 'password', '', {type: 'password', required: true, minBytes: 8, maxBytes: 1024, autocomplete: 'new-password'});
    const role = selection(form, 'Role', 'role', [['viewer', 'Viewer'], ['admin', 'Administrator']], 'viewer');
    mutation(form, 'Create user', async () => {
      try {
        await api('/users', {method: 'POST', body: JSON.stringify({username: username.value, password: password.value, role: role.value})});
        await render();
        message.textContent = 'User created.';
      } finally { password.value = ''; }
    });
    section.append(form);
  }
}

function groupSettings(user, groups) {
  const section = panel('Groups');
  const editable = user.role === 'admin' && csrf;
  section.append(text('p', 'Both roles can view groups. Administrators can manage them. Deleting a group detaches its nodes without deleting those nodes.'));
  for (const item of groups) {
    const entry = document.createElement('article');
    entry.append(text('h3', item.name), text('p', `ID: ${item.id}`));
    if (editable) {
      const form = document.createElement('form');
      const name = field(form, 'Group name', 'name', item.name, {required: true, maxBytes: 255});
      mutation(form, 'Save group', async () => {
        await api(`/groups/${encodeURIComponent(item.id)}`, {method: 'PATCH', body: JSON.stringify({name: name.value})});
        await render();
        message.textContent = 'Group saved.';
      });
      const remove = text('button', 'Delete group');
      remove.type = 'button';
      remove.addEventListener('click', async () => {
        if (!window.confirm(`Delete group ${item.name}? Its nodes will become ungrouped.`)) return;
        remove.disabled = true;
        try {
          await api(`/groups/${encodeURIComponent(item.id)}`, {method: 'DELETE'});
          await render();
          message.textContent = 'Group deleted; its nodes are now ungrouped.';
        } catch (error) { showError(error); }
        finally { remove.disabled = false; }
      });
      form.append(remove);
      entry.append(form);
    }
    section.append(entry);
  }
  if (groups.length === 0) section.append(text('p', 'No groups have been created.'));
  if (editable) {
    section.append(text('h3', 'Create group'));
    const form = document.createElement('form');
    const name = field(form, 'New group name', 'name', '', {required: true, maxBytes: 255});
    mutation(form, 'Create group', async () => {
      await api('/groups', {method: 'POST', body: JSON.stringify({name: name.value})});
      await render();
      message.textContent = 'Group created.';
    });
    section.append(form);
  }
}

function auditSettings(events) {
  const section = panel('Audit log');
  section.append(text('p', 'Both roles can read audit events. Showing up to the first 50 events (limit=50, offset=0), not the complete history. Use the paginated API for additional events.'));
  if (events.length === 0) { section.append(text('p', 'No audit events returned.')); return; }
  const list = document.createElement('ol');
  for (const event of events) {
    const entry = document.createElement('li');
    entry.append(text('h3', event.action), text('p', `Created (UTC): ${event.created_at}`), text('p', `Event: ${event.id} · Actor: ${event.actor_id || 'No actor'} · Target: ${event.target_id || 'No target'}`), text('pre', JSON.stringify(event.details, null, 2)));
    list.append(entry);
  }
  section.append(list);
}

function nodeMetadata(panel, node, groups, editable) {
  panel.append(text('h3', 'Node metadata'));
  const group = groups.find((item) => item.id === node.group_id);
  const values = [
    ['ID', node.id], ['Created (UTC)', node.created_at], ['Notes', node.notes || 'None'],
    ['Region', node.region || 'None'], ['Provider', node.provider || 'None'],
    ['Group', group ? `${group.name} (${group.id})` : node.group_id || 'Ungrouped'],
    ['Tags', node.tags.length ? node.tags.join('\n') : 'None']
  ];
  const details = document.createElement('dl');
  for (const [label, value] of values) details.append(text('dt', label), text('dd', value));
  panel.append(details);
  if (!editable) return;
  const form = document.createElement('form');
  const name = field(form, 'Node name', 'name', node.name, {required: true, maxBytes: 255});
  const notes = field(form, 'Notes', 'notes', node.notes, {multiline: true});
  const region = field(form, 'Region', 'region', node.region);
  const provider = field(form, 'Provider', 'provider', node.provider);
  const choices = [['', 'Ungrouped'], ...groups.map((item) => [item.id, item.name])];
  if (node.group_id && !group) choices.push([node.group_id, node.group_id]);
  const groupID = selection(form, 'Group (Ungrouped clears assignment)', 'group_id', choices, node.group_id || '');
  const tags = field(form, 'Tags (one per line; empty clears all tags)', 'tags', node.tags.join('\n'), {multiline: true, perLine: true, minBytes: 1, maxBytes: 255});
  mutation(form, 'Save node metadata', async () => {
    await api(`/nodes/${encodeURIComponent(node.id)}`, {method: 'PATCH', body: JSON.stringify({name: name.value, notes: notes.value, region: region.value, provider: provider.value, group_id: groupID.value || null, tags: tags.value.split('\n').filter((tag) => tag !== '')})});
    await render();
    message.textContent = 'Node metadata saved.';
  });
  panel.append(form);
}

async function render() {
  if (location.pathname === '/login') {
    content.innerHTML = '<section><h2>Sign in</h2><form id="login"><label>Username<input name="username" autocomplete="username" required></label><label>Password (8–1024 UTF-8 bytes)<input name="password" type="password" autocomplete="current-password" required></label><button>Sign in</button></form></section>';
    const loginForm = document.getElementById('login');
    byteLimit(loginForm.elements.namedItem('username'), 1, 128);
    byteLimit(loginForm.elements.namedItem('password'), 8, 1024);
    loginForm.addEventListener('submit', async (event) => {
      event.preventDefault();
      const form = event.currentTarget;
      const button = form.querySelector('button');
      button.disabled = true;
      try {
        await api('/session', {method: 'POST', body: JSON.stringify({username: form.username.value, password: form.password.value})});
        form.password.value = '';
        location.assign('/overview');
      } catch (error) { showError(error); } finally { form.password.value = ''; button.disabled = false; }
    });
    return;
  }
  const session = await api('/me');
  csrf = session.csrf_token;
  const user = session.user;
  const section = document.createElement('section');
  content.replaceChildren(section);
  section.append(text('p', `Signed in as ${user.username} (${user.role})`));
  const logout = text('button', 'Sign out');
  logout.addEventListener('click', async () => {
    try {
      await api('/session', {method: 'DELETE'});
      relogin();
    } catch (error) { showError(error); }
  });
  section.append(logout);
  if (location.pathname === '/nodes' || location.pathname === '/overview') {
    const result = await api('/nodes?limit=100');
    const panel = document.createElement('section');
    panel.append(text('h2', 'Node inventory'));
    const wrapper = document.createElement('div');
    wrapper.className = 'table-wrap';
    const table = document.createElement('table');
    table.innerHTML = '<thead><tr><th scope="col">Name</th><th scope="col">Status</th><th scope="col">Created (UTC)</th></tr></thead>';
    const body = document.createElement('tbody');
    for (const node of result.nodes) {
      const row = document.createElement('tr');
      const cell = document.createElement('td');
      const link = text('a', node.name);
      link.href = `/nodes/${encodeURIComponent(node.id)}`;
      cell.append(link);
      row.append(cell, text('td', node.status), text('td', node.created_at));
      body.append(row);
    }
    table.append(body);
    wrapper.append(table);
    panel.append(wrapper);
    if (result.nodes.length === 0) panel.append(text('p', 'No nodes have been created.'));
    if (result.nodes.length === 100) panel.append(text('p', 'Showing the first 100 nodes. Use the paginated API for additional nodes.'));
    if (user.role === 'admin' && csrf) {
      const form = document.createElement('form');
      form.innerHTML = '<label>New pending node name<input name="name" required></label><button>Create pending node</button>';
      byteLimit(form.elements.namedItem('name'), 1, 255);
      form.addEventListener('submit', async (event) => {
        event.preventDefault();
        try { await api('/nodes', {method: 'POST', body: JSON.stringify({name: form.elements.namedItem('name').value})}); await render(); }
        catch (error) { showError(error); }
      });
      panel.append(form);
    }
    content.append(panel);
  } else if (location.pathname.startsWith('/nodes/')) {
    const id = location.pathname.slice('/nodes/'.length);
    const node = await api(`/nodes/${encodeURIComponent(id)}`);
    const config = await api(`/nodes/${encodeURIComponent(id)}/config`);
    const groups = await api('/groups');
    const panel = document.createElement('section');
    panel.append(text('h2', node.name), text('p', `Status: ${node.status}`));
    nodeMetadata(panel, node, groups.groups, user.role === 'admin' && csrf);
    panel.append(text('h3', 'Desired configuration'), text('pre', JSON.stringify(config, null, 2)));
    if (user.role === 'admin' && csrf) {
      const form = document.createElement('form');
      form.innerHTML = '<label>Report interval (seconds)<input name="report" type="number" min="5" max="60" required></label><label>Disk interval (seconds)<input name="disk" type="number" min="60" max="3600" required></label><label>Billing day<input name="billing" type="number" min="1" max="28" required></label><button>Save desired configuration</button>';
      form.report.value = config.report_interval_seconds;
      form.disk.value = config.disk_interval_seconds;
      form.billing.value = config.billing_day;
      form.addEventListener('submit', async (event) => {
        event.preventDefault();
        try {
          await api(`/nodes/${encodeURIComponent(id)}/config`, {method: 'PATCH', body: JSON.stringify({report_interval_seconds: Number(form.report.value), disk_interval_seconds: Number(form.disk.value), billing_day: Number(form.billing.value)})});
          await render();
          message.textContent = 'Desired configuration saved.';
        } catch (error) { showError(error); }
      });
      panel.append(form);
    }
    content.append(panel);
  } else if (location.pathname === '/settings') {
    passwordSettings();
    const [users, groups, audit] = await Promise.all([api('/users'), api('/groups'), api('/audit?limit=50&offset=0')]);
    userSettings(user, users.users);
    groupSettings(user, groups.groups);
    auditSettings(audit.events);
  } else if (location.pathname === '/status') {
    section.append(text('h2', 'Public status unavailable'), text('p', 'A public status page is not available in this console. Backend metric ingestion does not publish private telemetry.'));
  } else if (location.pathname === '/alerts') {
    section.append(text('h2', 'Alerts unavailable'), text('p', 'Alert rules, notifications and delivery are not available in this foundation slice.'));
  } else {
    section.append(text('h2', 'Foundation capabilities'), text('p', 'Use Settings to manage users and groups, change your password and read the audit log. Administrators can also manage node inventory and desired configuration. Agent enrollment is supported by the backend API, but its browser wizard and telemetry display are not yet available. Alerts, public status and backup are not available.'));
  }
}

render().catch(showError);
