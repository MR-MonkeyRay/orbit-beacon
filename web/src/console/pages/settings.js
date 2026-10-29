import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h, icon} from '../../shared/dom.js';
import {allNodes, api, getNavigationSignal, groupsList, invalidate, invalidateSession, nameLookup, session, updateUser, usersList} from '../api.js';
import {when} from '../format.js';
import {ignoreNavigationCancellation, refresh} from '../router.js';
import {busy, button, emptyState, facts, iconButton, link, pageHead, panel, skeleton} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, onSubmit, passwordInput, validateBytes} from '../ui/forms.js';
import {passwordChangeForm} from '../ui/password-form.js';
import {nextCursor} from '../ui/pager.js';
import {tabSet} from '../ui/tabs.js';
import {issueEnrollment} from './enrollments.js';
import {auditItem} from '../lib/audit-item.js';
import {dataOperationsTab} from './data-operations.js';
import {showTask} from './backup-task.js';

export async function settingsPage() {
  const signal = getNavigationSignal();
  const task = new URLSearchParams(location.search).get('task');
  if (task !== null && location.hash === '#data') {
    requestAnimationFrame(() => { if (!signal?.aborted) showTask(task, {signal}); });
  }
  return h('div', null,
    pageHead(t('shell.settings'), t('settings.settingsIntro')),
    tabSet([
      {id: 'account', label: t('settings.account'), icon: 'user', build: accountTab},
      {id: 'administrators', label: t('settings.administrators'), icon: 'users', build: lazyTab(usersList, administratorsTab)},
      {id: 'groups', label: t('settings.groups'), icon: 'folder', build: lazyTab(
        () => Promise.all([groupsList(), allNodes()]),
        ([groups, nodes]) => groupsTab(groups, nodes)
      )},
      {id: 'audit', label: t('settings.audit'), icon: 'list', build: lazyTab(
        () => Promise.all([api('/audit?limit=50&offset=0'), allNodes(), usersList(), groupsList()]),
        ([audit, nodes, users, groups]) => auditTab(audit, nameLookup(nodes, users, groups))
      )},
      {id: 'data', label: t('settings.data'), icon: 'cloud', build: dataOperationsTab},
      {id: 'public-page', label: t('settings.publicPage'), icon: 'globe', build: publicPageTab}
    ], t('settings.settingsSections')));
}

function lazyTab(load, render) {
  const navigation = getNavigationSignal();
  let result;
  let hasResult = false;
  let pending;
  let requestEpoch = 0;
  return () => {
    const element = h('div');
    const start = () => {
      if (navigation?.aborted) return;
      element.replaceChildren(skeleton());
      if (!pending) { pending = load(); requestEpoch += 1; }
      const request = pending;
      const epoch = requestEpoch;
      request.then((value) => {
        if (navigation?.aborted || epoch !== requestEpoch) return;
        if (!hasResult) {
          result = value;
          hasResult = true;
          pending = null;
        }
        element.replaceChildren(render(value));
      }, (error) => {
        if (navigation?.aborted || epoch !== requestEpoch) return;
        pending = null;
        element.replaceChildren(h('div', {class: 'form-error', role: 'alert'},
          h('p', null, errorText(error)),
          button(t('common.retry'), {onClick: start})));
      });
    };
    if (hasResult) element.replaceChildren(render(result));
    else start();
    return element;
  };
}

function publicPageTab() {
  const navigation = getNavigationSignal();
  const control = h('input', {type: 'checkbox', name: 'public_page_enabled', disabled: true});
  const note = h('p', {class: 'hint', role: 'status'}, t('settings.publicLoading'));
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
  const element = panel(t('settings.publicPage'), {},
    h('label', {class: 'switch'}, control, h('span', {class: 'switch-track', 'aria-hidden': 'true'}),
      h('span', {class: 'switch-text'}, h('strong', null, t('settings.publicEnable')), h('span', null, t('settings.publicEnableHint')))),
    h('p', {class: 'lede'}, t('settings.publicRoutingHint')),
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
  return h('div', {class: 'split'},
    panel(t('account.yourAccount'), {}, facts([
      [t('account.username'), user.username],
      [t('account.accountCreated'), when(user.created_at)], [t('account.sessionExpires'), session.expires ? when(session.expires) : null]
    ])),
    panel(t('account.changePassword'), {}, h('p', {class: 'lede'}, t('account.passwordSignOutHint')), form));
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
          field(t('settings.initialPassword'), password, {inner: password.wrapper, hint: t('settings.initialPasswordHint')}),
          h('label', {class: 'data-check'}, mustChangePassword, h('span', null, t('settings.requirePasswordChange'))));
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
      h('td', {class: 'actions'}, iconButton('edit', t('settings.renameAdministratorLabel', {name: item.username}), () => editUser(item)), remove));
  });

  return h('section', {class: 'panel', 'aria-labelledby': 'administrators-title'},
    h('div', {class: 'panel-head'}, h('h2', {id: 'administrators-title'}, t('settings.administrators')), button(t('settings.addAdministrator'), {icon: 'plus', variant: 'primary', small: true, onClick: createUser})),
    h('div', {class: 'panel-body flush'}, h('p', {class: 'lede'}, t('settings.fullAccess'))),
    h('div', {class: 'table-wrap'}, h('table', {class: 'table administrators-table'},
      h('thead', null, h('tr', null, h('th', {scope: 'col'}, t('account.username')), h('th', {scope: 'col', class: 'hide-compact'}, t('settings.created')),
        h('th', {scope: 'col'}, h('span', {class: 'sr'}, t('settings.actions'))))),
      h('tbody', null, users.length ? rows : h('tr', null, h('td', {colspan: 3}, t('settings.noAdministrators')))))),
    lastAdministrator ? h('p', {class: 'administrator-protection muted'}, t('settings.lastAdministrator')) : null);
}

function groupsTab(groups, nodes) {
  const batchEnrollment = async (item, trigger) => {
    busy(trigger, true);
    try {
      await issueEnrollment({group_id: item.id, mode: 'batch'}, t('settings.batchEnrollmentFor', {name: item.name}));
    } catch (error) { toastError(error); }
    finally { busy(trigger, false); }
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
  if (!groups.length) return panel(t('settings.groups'), {}, emptyState(t('settings.noGroups'), t('settings.groupsIntro'), addButton));
  return panel(t('settings.groups'), {action: addButton},
    h('p', {class: 'lede'}, t('settings.groupsHint')),
    h('ul', {class: 'rows'}, groups.map((item) => {
      const size = nodes.filter((node) => node.group_id === item.id).length;
      const enrollment = button(t('settings.batchEnrollment'), {small: true});
      enrollment.addEventListener('click', () => batchEnrollment(item, enrollment));
      return h('li', null, icon('folder', 'muted'),
        h('div', {class: 'grow'}, h('div', {class: 'title'}, item.name), h('div', {class: 'sub'}, link(`/nodes?group=${item.id}`, t('settings.nodeCount', {count: size})))),
        [enrollment, iconButton('edit', t('settings.renameName', {name: item.name}), () => groupForm(item)), iconButton('trash', t('settings.deleteName', {name: item.name}), () => deleteGroup(item, size))]);
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
  return panel(t('settings.audit'), {}, h('p', {class: 'lede'}, t('settings.auditHint')),
    events.length ? [list, more] : emptyState(t('settings.noAudit'), t('settings.noAuditHint')));
}
