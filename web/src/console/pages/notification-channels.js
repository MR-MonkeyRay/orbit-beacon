import {h} from '../../shared/dom.js';
import {formatList, t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {api, session} from '../api.js';
import {busy, button, dataTable, iconButton, panel} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, passwordInput, select, textarea, validateBytes} from '../ui/forms.js';
import {MESSAGE_TEMPLATE_TOKENS, URL_CHANNELS, usesURL, buildChannelPayload, errorCodeMessage, validateChannel, validationError} from '../lib/alert-utils.js';
import {eventTime} from './alert-events.js';

const KINDS = [['webhook', t('alerts.kind.webhook')], ['telegram', 'Telegram'], ['email', t('alerts.kind.email')], ['feishu', t('alerts.kind.feishu')], ['dingtalk', t('alerts.kind.dingtalk')], ['wecom', t('alerts.kind.wecom')], ['bark', 'Bark'], ['ntfy', 'ntfy']];

export function channelEditor(item, {signal, saved}) {
  if (!session.user || !session.csrf || signal?.aborted) return;
  const controls = {};
  const dialog = formDialog({
    title: item ? t('alerts.editNamed', {name: item.name}) : t('alerts.channels.addTitle'), submit: t('alerts.channels.save'),
    build: (form) => {
      const add = (label, name, props = {}, hint) => {
        const control = controls[name] = input({name, ...props});
        const wrapper = field(label, control, {hint});
        return wrapper;
      };
      const nameField = add(t('alerts.column.name'), 'name', {required: true, value: item?.name || ''});
      validateBytes(controls.name, 1, 100, t('alerts.channels.name'));
      const kind = controls.kind = select(KINDS, item?.kind || 'webhook', {name: 'kind', disabled: Boolean(item)});
      const enabled = controls.enabled = input({type: 'checkbox', name: 'enabled', checked: item?.enabled ?? true});
      const replaceURL = controls.replaceURL = input({type: 'checkbox', name: 'replace_url'});
      const urlField = add('URL', 'url', {type: 'url', autocomplete: 'off', required: !item});
      controls.url.dataset.field = 'config.url';
      const urlState = h('p', {class: 'text-note'}, item?.config?.url_set ? t('alerts.channels.urlSet', {url: item.config.url_redacted}) : t('alerts.channels.urlNotSet'));
      const replaceField = field(t('alerts.channels.replaceURL'), replaceURL);
      const webhook = h('div', {class: 'stack'}, item ? urlState : null, item ? replaceField : null, urlField);
      const chat = add(t('alerts.channels.chatID'), 'chat_id', {value: item?.kind === 'telegram' ? item.config.chat_id : ''}, t('alerts.channels.chatHint'));
      controls.chat_id.dataset.field = 'config.chat_id';
      const email = h('div', {class: 'stack'},
        add(t('alerts.channels.smtpHost'), 'host', {value: item?.kind === 'email' ? item.config.host : ''}),
        add(t('alerts.channels.smtpPort'), 'port', {type: 'number', min: 1, max: 65535, step: 1, value: item?.kind === 'email' ? item.config.port : 587}),
        add(t('alerts.channels.smtpUsername'), 'username', {value: item?.kind === 'email' ? item.config.username || '' : '', autocomplete: 'off'}),
        add(t('alerts.channels.from'), 'from', {type: 'email', value: item?.kind === 'email' ? item.config.from : ''}));
      const to = controls.to = textarea({name: 'to'}, item?.kind === 'email' ? item.config.to.join('\n') : '');
      email.append(field(t('alerts.channels.to'), to, {hint: t('alerts.channels.toHint')}));
      for (const key of ['host', 'port', 'username', 'from', 'to']) controls[key].dataset.field = `config.${key}`;
      const template = controls.template = textarea({name: 'template', dataset: {field: 'config.template'}}, item?.config?.template || '');
      const templateField = field(t('alerts.channels.template'), template, {hint: MESSAGE_TEMPLATE_TOKENS.join(' ')});
      const secret = controls.secret = passwordInput({name: 'secret', autocomplete: 'new-password'});
      validateBytes(secret, 0, 4096, t('alerts.channels.secret'));
      const secretField = field(t('alerts.channels.signingSecret'), secret, {inner: secret.wrapper, hint: item ? t('alerts.channels.secretHint') : undefined});
      const secretLabel = secretField.querySelector('label');
      const replaceSecret = controls.replaceSecret = input({type: 'checkbox', name: 'replace_secret'});
      const replaceSecretField = field(t('alerts.channels.replaceSecret'), replaceSecret);
      const secretState = h('p', {class: 'text-note'}, t(item?.secret_set ? 'alerts.channels.secretSet' : 'alerts.channels.secretNotSet'));
      const sync = () => {
        const k = kind.value;
        webhook.hidden = !usesURL(k);
        controls.url.placeholder = URL_CHANNELS[k]?.placeholder || '';
        secretState.hidden = k === 'wecom';
        replaceSecretField.hidden = k === 'wecom';
        chat.hidden = k !== 'telegram';
        email.hidden = k !== 'email';
        template.disabled = k === 'webhook';
        templateField.hidden = template.disabled;
        controls.url.disabled = !usesURL(k) || Boolean(item && !replaceURL.checked);
        controls.url.required = !controls.url.disabled;
        urlField.hidden = controls.url.disabled;
        controls.chat_id.disabled = k !== 'telegram';
        controls.chat_id.required = k === 'telegram';
        for (const key of ['host', 'port', 'username', 'from', 'to']) {
          controls[key].disabled = k !== 'email';
          controls[key].required = k === 'email' && key !== 'username';
        }
        secretLabel.textContent = t(({telegram: 'alerts.channels.botToken', email: 'alerts.channels.smtpPassword', bark: 'alerts.channels.deviceKey', ntfy: 'alerts.channels.accessToken'})[k] || 'alerts.channels.optionalSecret');
        secret.disabled = k === 'wecom' || Boolean(item && !replaceSecret.checked);
        secretField.hidden = secret.disabled;
        secret.required = !secret.disabled && (k === 'telegram' || k === 'bark' || (k === 'email' && Boolean(controls.username.value)));
      };
      kind.addEventListener('change', () => { secret.value = ''; sync(); });
      replaceURL.addEventListener('change', () => { if (!replaceURL.checked) controls.url.value = ''; sync(); if (replaceURL.checked) controls.url.focus(); });
      replaceSecret.addEventListener('change', () => { if (!replaceSecret.checked) secret.value = ''; sync(); if (replaceSecret.checked) secret.focus(); });
      controls.username.addEventListener('input', sync);
      sync();
      // 服务端若报告已保存凭据的错误，须展开替换控件，
      // 以免错误留在隐藏字段中，管理员无法修正。
      const showSecretError = secret.showError;
      secret.showError = (message) => {
        if (message && secret.disabled && item) { replaceSecret.checked = true; sync(); }
        showSecretError(message);
      };
      const configError = h('p', {class: 'field-error', role: 'alert', hidden: true});
      const configGroup = h('div', {class: 'stack', dataset: {field: 'config'}}, webhook, chat, email, templateField, configError);
      configGroup.showError = (message) => { configError.textContent = message; configError.hidden = !message; };
      configGroup.focus = () => (usesURL(kind.value) ? (controls.url.disabled ? replaceURL : controls.url) : kind.value === 'telegram' ? controls.chat_id : controls.host).focus();
      configGroup.addEventListener('input', () => configGroup.showError(''));
      form.append(nameField, field(t('alerts.column.kind'), kind), field(t('alerts.state.enabled'), enabled), configGroup, secretState, ...(item ? [replaceSecretField] : []), secretField);
    },
    action: async () => {
      if (!session.user || !session.csrf || signal?.aborted) throw new DOMException('Cancelled', 'AbortError');
      const values = Object.fromEntries(['name', 'kind', 'url', 'chat_id', 'host', 'port', 'username', 'from', 'to', 'template', 'secret'].map((key) => [key, controls[key].value]));
      values.enabled = controls.enabled.checked;
      const body = buildChannelPayload(values, {editing: Boolean(item), replaceURL: controls.replaceURL.checked, replaceSecret: controls.replaceSecret.checked});
      const errors = validateChannel(body, {kind: values.kind, existing: item});
      if (Object.keys(errors).length) throw validationError(errors);
      await api(item ? `/notification-channels/${encodeURIComponent(item.id)}` : '/notification-channels', {method: item ? 'PATCH' : 'POST', body, signal});
      controls.secret.value = '';
      controls.url.value = '';
      await saved();
      toast(t('alerts.channels.saved'));
    }
  });
  dialog.closed.finally(() => { controls.secret.value = ''; controls.url.value = ''; });
  return dialog;
}

export function channelsPanel({channels, signal, saved}) {
  const edit = (channel) => channelEditor(channel, {signal, saved});
  const remove = async (channel) => {
    if (!session.user || !session.csrf || signal?.aborted) return;
    if (!(await confirmDialog({title: t('alerts.deleteNamed', {name: channel.name}), message: t('alerts.channels.deleteHint'), confirm: t('alerts.channels.delete'), danger: true}))) return;
    if (signal?.aborted) return;
    try {
      await api(`/notification-channels/${encodeURIComponent(channel.id)}`, {method: 'DELETE', signal});
      await saved();
      toast(t('alerts.channels.deleted'));
    } catch (error) { if (!signal?.aborted) toastError(error); }
  };
  const row = (channel) => {
    const status = h('p', {class: 'text-note', role: 'status'});
    const test = button(t('alerts.channels.sendTest'), {small: true, onClick: async () => {
      if (!session.user || !session.csrf || signal?.aborted) return;
      busy(test, true);
      status.textContent = t('alerts.channels.sendingTest');
      try {
        const result = await api(`/notification-channels/${encodeURIComponent(channel.id)}/test`, {method: 'POST', signal});
        if (!signal?.aborted) status.textContent = result.ok ? t('alerts.channels.testSent') : t('alerts.channels.testFailed', {error: errorCodeMessage(result.error_code)});
      } catch (error) {
        if (!signal?.aborted && error.name !== 'AbortError') status.textContent = t('alerts.channels.testFailed', {error: errorText(error)});
      } finally { busy(test, false); }
    }});
    const destination = usesURL(channel.kind) ? channel.config.url_redacted || t('alerts.channels.urlNotSet')
      : channel.kind === 'telegram' ? channel.config.chat_id : `${channel.config.host}:${channel.config.port} → ${formatList(channel.config.to)}`;
    return h('tr', null, h('td', null, channel.name), h('td', null, KINDS.find(([kind]) => kind === channel.kind)?.[1] || t('alerts.kind.unknown')),
      h('td', {class: 'alert-destination'}, destination, channel.kind === 'wecom' ? null : h('div', {class: 'text-note'}, t(channel.secret_set ? 'alerts.channels.secretSet' : 'alerts.channels.secretNotSet')), status),
      h('td', null, t(channel.enabled ? 'alerts.state.enabled' : 'alerts.state.disabled')),
      h('td', {class: 'actions'}, test, iconButton('edit', t('alerts.editNamed', {name: channel.name}), () => edit(channel)), iconButton('trash', t('alerts.deleteAction', {name: channel.name}), () => remove(channel))));
  };
  return panel(t('alerts.tab.channels'), {action: button(t('alerts.channels.add'), {small: true, variant: 'primary', onClick: () => edit(null)})},
    h('p', {class: 'text-note'}, t('alerts.channels.writeOnlyHint')),
    channels.length ? dataTable({caption: t('alerts.channels.title'),
      columns: ['name', 'kind', 'destinationSecret', 'state', 'actions'].map((key) => t(`alerts.column.${key}`)),
      body: h('tbody', null, channels.map(row))}) : h('p', {class: 'text-note'}, t('alerts.channels.empty')));
}

export function deliveriesPanel(channels, signal) {
  const content = h('div');
  const status = h('p', {class: 'text-note', role: 'status'}, t('alerts.deliveries.loading'));
  const reload = async () => {
    if (signal?.aborted) return;
    busy(retry, true);
    status.textContent = t('alerts.deliveries.loading');
    try {
      const {deliveries} = await api('/notification-deliveries?limit=10', {signal});
      if (signal?.aborted) return;
      content.replaceChildren(deliveries.length ? dataTable({caption: t('alerts.deliveries.caption'),
        columns: ['state', 'channel', 'attempts', 'lastErrorCode', 'time'].map((key) => t(`alerts.column.${key}`)),
        body: h('tbody', null, deliveries.map((delivery) => h('tr', null,
          h('td', null, t(['pending', 'sent', 'failed'].includes(delivery.state) ? `alerts.deliveryState.${delivery.state}` : 'alerts.unknown'), delivery.coalesced_count > 1 ? h('div', {class: 'text-note'}, t('alerts.deliveries.coalesced', {count: delivery.coalesced_count})) : null),
          h('td', null, channels.find((channel) => channel.id === delivery.channel_id)?.name || delivery.channel_id),
          h('td', null, String(delivery.attempts)), h('td', {title: delivery.last_error_code ? errorCodeMessage(delivery.last_error_code) : ''}, delivery.last_error_code || t('common.none')),
          h('td', null, eventTime(delivery.updated_at)))))}) : h('p', {class: 'text-note'}, t('alerts.deliveries.empty')));
      status.textContent = t('alerts.deliveries.loaded', {count: deliveries.length});
    } catch (error) { if (!signal?.aborted && error.name !== 'AbortError') status.textContent = t('alerts.deliveries.loadFailed', {error: errorText(error)}); }
    finally { busy(retry, false); }
  };
  const retry = button(t('common.refresh'), {small: true, icon: 'refresh', onClick: reload});
  const root = panel(t('alerts.deliveries.title'), {action: retry}, status, content);
  reload();
  return root;
}
