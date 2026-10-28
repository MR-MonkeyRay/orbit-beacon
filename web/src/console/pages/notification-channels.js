import {h} from '../../shared/dom.js';
import {api, session} from '../api.js';
import {busy, button, iconButton, panel} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, passwordInput, select, textarea, validateBytes} from '../ui/forms.js';
import {buildChannelPayload, errorCodeMessage, SECRET_GUIDANCE, validateChannel, validationError} from './alert-utils.js';
import {eventTime} from './alert-events.js';

const KINDS = [['webhook', 'Webhook'], ['telegram', 'Telegram'], ['email', 'Email']];

export function channelEditor(item, {signal, saved}) {
  if (!session.user || !session.csrf || signal?.aborted) return;
  const controls = {};
  const dialog = formDialog({
    title: item ? `Edit ${item.name}` : 'Add notification channel', submit: 'Save channel',
    build: (form) => {
      const add = (label, name, props = {}, hint) => {
        const control = controls[name] = input({name, ...props});
        const wrapper = field(label, control, {hint});
        return wrapper;
      };
      const nameField = add('Name', 'name', {required: true, value: item?.name || ''});
      validateBytes(controls.name, 1, 100, 'Channel name');
      const kind = controls.kind = select(KINDS, item?.kind || 'webhook', {name: 'kind', disabled: Boolean(item)});
      const enabled = controls.enabled = input({type: 'checkbox', name: 'enabled', checked: item?.enabled ?? true});
      const replaceURL = controls.replaceURL = input({type: 'checkbox', name: 'replace_url'});
      const urlField = add('Webhook URL', 'url', {type: 'url', autocomplete: 'off', required: !item}, 'HTTPS without credentials or a fragment. HTTP literal loopback requires server settings.');
      controls.url.dataset.field = 'config.url';
      const urlState = h('p', {class: 'hint'}, item?.config?.url_set ? `URL set: ${item.config.url_redacted}` : 'URL not set.');
      const replaceField = field('Replace URL', replaceURL);
      const webhook = h('div', {class: 'stack'}, item ? urlState : null, item ? replaceField : null, urlField);
      const chat = add('Telegram chat ID', 'chat_id', {value: item?.kind === 'telegram' ? item.config.chat_id : ''}, 'A numeric chat ID or @channel username.');
      controls.chat_id.dataset.field = 'config.chat_id';
      const email = h('div', {class: 'stack'},
        add('SMTP host', 'host', {value: item?.kind === 'email' ? item.config.host : ''}),
        add('SMTP port', 'port', {type: 'number', min: 1, max: 65535, step: 1, value: item?.kind === 'email' ? item.config.port : 587}),
        add('SMTP username', 'username', {value: item?.kind === 'email' ? item.config.username || '' : '', autocomplete: 'off'}),
        add('From address', 'from', {type: 'email', value: item?.kind === 'email' ? item.config.from : ''}));
      const to = controls.to = textarea({name: 'to'}, item?.kind === 'email' ? item.config.to.join('\n') : '');
      email.append(field('To addresses', to, {hint: '1–20 addresses, one per line or separated by commas.'}));
      for (const key of ['host', 'port', 'username', 'from', 'to']) controls[key].dataset.field = `config.${key}`;
      const secret = controls.secret = passwordInput({name: 'secret', autocomplete: 'new-password'});
      validateBytes(secret, 0, 4096, 'Secret');
      const secretField = field('Signing secret', secret, {inner: secret.wrapper, hint: 'Write-only. It is never returned or prefilled. Leave a replacement empty to clear an optional secret.'});
      const secretLabel = secretField.querySelector('label');
      const replaceSecret = controls.replaceSecret = input({type: 'checkbox', name: 'replace_secret'});
      const replaceSecretField = field('Replace or clear secret', replaceSecret);
      const secretState = h('p', {class: 'hint'}, item?.secret_set ? 'Secret set' : 'Secret not set');
      const sync = () => {
        const k = kind.value;
        webhook.hidden = k !== 'webhook';
        chat.hidden = k !== 'telegram';
        email.hidden = k !== 'email';
        controls.url.disabled = k !== 'webhook' || Boolean(item && !replaceURL.checked);
        controls.url.required = !controls.url.disabled;
        urlField.hidden = controls.url.disabled;
        controls.chat_id.disabled = k !== 'telegram';
        controls.chat_id.required = k === 'telegram';
        for (const key of ['host', 'port', 'username', 'from', 'to']) {
          controls[key].disabled = k !== 'email';
          controls[key].required = k === 'email' && key !== 'username';
        }
        secretLabel.textContent = k === 'webhook' ? 'Signing secret (optional)' : k === 'telegram' ? 'Bot token' : 'SMTP password';
        secret.disabled = Boolean(item && !replaceSecret.checked);
        secretField.hidden = secret.disabled;
        secret.required = !secret.disabled && (k === 'telegram' || (k === 'email' && Boolean(controls.username.value)));
      };
      kind.addEventListener('change', () => { secret.value = ''; sync(); });
      replaceURL.addEventListener('change', () => { if (!replaceURL.checked) controls.url.value = ''; sync(); if (replaceURL.checked) controls.url.focus(); });
      replaceSecret.addEventListener('change', () => { if (!replaceSecret.checked) secret.value = ''; sync(); if (replaceSecret.checked) secret.focus(); });
      controls.username.addEventListener('input', sync);
      sync();
      // A server error about the retained secret must not land in the hidden
      // field: open the replacement control so the admin sees and fixes it.
      const showSecretError = secret.showError;
      secret.showError = (message) => {
        if (message && secret.disabled && item) { replaceSecret.checked = true; sync(); }
        showSecretError(message);
      };
      const configError = h('p', {class: 'field-error', role: 'alert', hidden: true});
      const configGroup = h('div', {class: 'stack', dataset: {field: 'config'}}, webhook, chat, email, configError);
      configGroup.showError = (message) => { configError.textContent = message; configError.hidden = !message; };
      configGroup.focus = () => (kind.value === 'webhook' ? (controls.url.disabled ? replaceURL : controls.url) : kind.value === 'telegram' ? controls.chat_id : controls.host).focus();
      configGroup.addEventListener('input', () => configGroup.showError(''));
      form.append(nameField, field('Kind', kind), field('Enabled', enabled), configGroup, secretState, ...(item ? [replaceSecretField] : []), secretField);
    },
    action: async () => {
      if (!session.user || !session.csrf || signal?.aborted) throw new DOMException('Cancelled', 'AbortError');
      const values = Object.fromEntries(['name', 'kind', 'url', 'chat_id', 'host', 'port', 'username', 'from', 'to', 'secret'].map((key) => [key, controls[key].value]));
      values.enabled = controls.enabled.checked;
      const body = buildChannelPayload(values, {editing: Boolean(item), replaceURL: controls.replaceURL.checked, replaceSecret: controls.replaceSecret.checked});
      const errors = validateChannel(body, {kind: values.kind, existing: item});
      if (Object.keys(errors).length) throw validationError(errors);
      try {
        await api(item ? `/notification-channels/${encodeURIComponent(item.id)}` : '/notification-channels', {method: item ? 'PATCH' : 'POST', body, signal});
      } catch (error) {
        if (error.code === 'secret_unreadable' || error.code === 'notification_channel_capacity_reached') error.message = errorCodeMessage(error.code);
        throw error;
      }
      controls.secret.value = '';
      controls.url.value = '';
      await saved();
      toast('Notification channel saved.');
    }
  });
  dialog.closed.finally(() => { controls.secret.value = ''; controls.url.value = ''; });
  return dialog;
}

export function channelsPanel({channels, signal, saved}) {
  const edit = (channel) => channelEditor(channel, {signal, saved});
  const remove = async (channel) => {
    if (!session.user || !session.csrf || signal?.aborted) return;
    if (!(await confirmDialog({title: `Delete ${channel.name}?`, message: 'This removes the channel, its rule routing and its delivery records.', confirm: 'Delete channel', danger: true}))) return;
    if (signal?.aborted) return;
    try {
      await api(`/notification-channels/${encodeURIComponent(channel.id)}`, {method: 'DELETE', signal});
      await saved();
      toast('Notification channel deleted.');
    } catch (error) { if (!signal?.aborted) toastError(error); }
  };
  const row = (channel) => {
    const status = h('p', {class: 'hint', role: 'status'});
    const test = button('Send test', {small: true, onClick: async () => {
      if (!session.user || !session.csrf || signal?.aborted) return;
      busy(test, true);
      status.textContent = 'Sending test…';
      try {
        const result = await api(`/notification-channels/${encodeURIComponent(channel.id)}/test`, {method: 'POST', signal});
        if (!signal?.aborted) status.textContent = result.ok ? 'Test sent successfully.' : `Test failed: ${errorCodeMessage(result.error_code)}${result.error_code ? ` (${result.error_code})` : ''}`;
      } catch (error) {
        if (!signal?.aborted) status.textContent = error.code === 'secret_unreadable' ? SECRET_GUIDANCE : `Test failed: ${error.code ? `${errorCodeMessage(error.code)} (${error.code})` : error.message}`;
      } finally { busy(test, false); }
    }});
    const destination = channel.kind === 'webhook' ? channel.config.url_redacted || 'URL not set'
      : channel.kind === 'telegram' ? channel.config.chat_id : `${channel.config.host}:${channel.config.port} → ${channel.config.to.join(', ')}`;
    return h('tr', null, h('td', null, channel.name), h('td', null, KINDS.find(([kind]) => kind === channel.kind)?.[1] || channel.kind),
      h('td', {class: 'alert-destination'}, destination, h('div', {class: 'hint'}, channel.secret_set ? 'Secret set' : 'Secret not set'), status),
      h('td', null, channel.enabled ? 'Enabled' : 'Disabled'),
      h('td', {class: 'actions'}, test, iconButton('edit', `Edit ${channel.name}`, () => edit(channel)), iconButton('trash', `Delete ${channel.name}`, () => remove(channel))));
  };
  return panel('Channels', {action: button('Add channel', {small: true, variant: 'primary', onClick: () => edit(null)})},
    h('p', {class: 'lede'}, 'Credentials and full webhook URLs are write-only. Test a channel after saving it.'),
    channels.length ? h('div', {class: 'table-wrap'}, h('table', {class: 'table'},
      h('caption', {class: 'sr'}, 'Notification channels'),
      h('thead', null, h('tr', null, ['Name', 'Kind', 'Destination / secret', 'State', 'Actions'].map((label) => h('th', {scope: 'col'}, label)))),
      h('tbody', null, channels.map(row)))) : h('p', {class: 'muted'}, 'No notification channels yet.'));
}

export function deliveriesPanel(channels, signal) {
  const content = h('div');
  const status = h('p', {class: 'muted', role: 'status'}, 'Loading deliveries…');
  const reload = async () => {
    if (signal?.aborted) return;
    busy(retry, true);
    status.textContent = 'Loading deliveries…';
    try {
      const {deliveries} = await api('/notification-deliveries?limit=10', {signal});
      if (signal?.aborted) return;
      content.replaceChildren(deliveries.length ? h('div', {class: 'table-wrap'}, h('table', {class: 'table'},
        h('caption', {class: 'sr'}, 'Recent notification deliveries'),
        h('thead', null, h('tr', null, ['State', 'Channel', 'Attempts', 'Last error code', 'Time'].map((label) => h('th', {scope: 'col'}, label)))),
        h('tbody', null, deliveries.map((delivery) => h('tr', null,
          h('td', null, delivery.state, delivery.coalesced_count > 1 ? h('div', {class: 'hint'}, `${delivery.coalesced_count} notifications coalesced`) : null),
          h('td', null, channels.find((channel) => channel.id === delivery.channel_id)?.name || delivery.channel_id),
          h('td', null, String(delivery.attempts)), h('td', {title: delivery.last_error_code ? errorCodeMessage(delivery.last_error_code) : ''}, delivery.last_error_code || 'None'),
          h('td', null, eventTime(delivery.updated_at))))))) : h('p', {class: 'muted'}, 'No deliveries yet.'));
      status.textContent = `${deliveries.length} recent deliveries.`;
    } catch (error) { if (!signal?.aborted) status.textContent = `Could not load deliveries: ${error.message} Use Refresh to retry.`; }
    finally { busy(retry, false); }
  };
  const retry = button('Refresh', {small: true, icon: 'refresh', onClick: reload});
  const root = panel('Recent deliveries', {action: retry}, status, content);
  reload();
  return root;
}
