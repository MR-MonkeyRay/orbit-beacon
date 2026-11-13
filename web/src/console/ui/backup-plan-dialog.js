import {h} from '../../shared/dom.js';
import {errorText, fieldErrorText} from '../../shared/errors.js';
import {t} from '../i18n.js';
import {api} from '../api.js';
import {button} from './components.js';
import {formDialog} from './dialogs.js';
import {field, input, select, validateBytes} from './forms.js';

const CRON_PRESETS = [['0 * * * *', 'data.cron.hourly'], ['0 3 * * *', 'data.cron.daily'], ['0 3 * * 0', 'data.cron.weekly']];
const CATEGORIES = [
  ['configuration', 'common.configuration'],
  ['monitoring', 'common.monitoring'],
  ['events', 'common.events', 'data.category.eventsHint']
];
export const backupProviders = [['s3_compatible', 'data.provider.compatible'], ['cos', 'data.provider.cos'], ['oss', 'data.provider.oss'], ['r2', 'data.provider.r2'], ['s3', 'data.provider.s3']];

export function backupCategoriesField(selected = ['configuration']) {
  const controls = CATEGORIES.map(([value]) => input({type: 'checkbox', name: 'categories', value, checked: selected.includes(value)}));
  controls[0].dataset.field = 'categories';
  const check = () => controls[0].setCustomValidity(controls.some((control) => control.checked) ? '' : t('data.category.required'));
  controls.forEach((control) => control.addEventListener('change', () => {
    check();
    if (controls[0].validity.valid) controls[0].showError?.('');
  }));
  check();
  const choices = h('div', {class: 'data-category-choices'}, CATEGORIES.map(([, title, hint], index) =>
    h('label', {class: 'check-row', title: hint ? t(hint) : null}, controls[index], h('span', null, t(title)))));
  return {element: field(t('data.categories'), controls[0], {inner: choices}), value: () => controls.filter((control) => control.checked).map((control) => control.value)};
}

function checkField(label, name, checked) {
  const control = input({type: 'checkbox', name, checked});
  return {control, element: h('label', {class: 'check-row'}, control, h('span', null, label))};
}

export function timezoneText(info) {
  const offset = info.utc_offset_seconds;
  const sign = offset < 0 ? '−' : '+';
  const minutes = Math.abs(offset) / 60;
  const formattedOffset = `${sign}${String(Math.floor(minutes / 60)).padStart(2, '0')}:${String(Math.floor(minutes % 60)).padStart(2, '0')}`;
  return t('data.timezone.value', {timezone: info.timezone_name, offset: formattedOffset});
}

// 服务器本地时间按“日期 时:分”显示，与 Cron 表达式的小时一致；悬停可见时区或 UTC 时间。
const wallClock = (value) => /^\d{4}-\d\d-\d\dT\d\d:\d\d/.test(value) ? value.slice(0, 16).replace('T', ' ') : value;
export function localRun(run, zone) {
  return h('time', {datetime: run.at, title: zone || `UTC ${run.at}`}, wallClock(run.local));
}

function suggestedEndpoint(provider, region) {
  if (!region) return '';
  if (provider === 'cos') return `https://cos.${region}.myqcloud.com`;
  if (provider === 'oss') return `https://s3.oss-${region}.aliyuncs.com`;
  if (provider === 's3') return `https://s3.${region}.amazonaws.com`;
  return '';
}

export function backupPlanDialog(plan, {signal, onSaved}) {
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal.addEventListener('abort', abort, {once: true});
  let previewController = null;
  let previewTimer = null;
  let previewEpoch = 0;
  let controls;
  const dialog = formDialog({
    title: plan ? t('data.plan.editTitle', {name: plan.name}) : t('data.plan.addTitle'),
    submit: t(plan ? 'data.plan.save' : 'data.plan.add'),
    build: (form) => {
      form.classList.add('data-plan-form');
      const destination = plan?.destination || {};
      const name = input({name: 'name', required: true, value: plan?.name || '', autocomplete: 'off'});
      validateBytes(name, 1, 100, t('data.plan.name'));
      const kind = {value: plan?.kind || 'local'};
      const kindButtons = [['local', 'data.plan.localDirectory'], ['object', 'data.plan.objectStorage']].map(([value, label]) =>
        h('button', {type: 'button', dataset: {kind: value}, disabled: Boolean(plan), onclick: () => { kind.value = value; updateKind(); }}, t(label)));
      const kindControl = h('div', {class: 'segmented data-kind', role: 'group', 'aria-label': t('data.plan.destinationKind')}, kindButtons);
      const enabled = input({type: 'checkbox', name: 'enabled', checked: plan?.enabled || false});
      const categories = backupCategoriesField(plan?.categories);
      const cron = input({name: 'cron', value: plan?.cron || '0 3 * * *', required: true, autocomplete: 'off', spellcheck: false, class: 'input mono'});
      const presets = h('div', {class: 'data-cron-presets'}, CRON_PRESETS.map(([expression, label]) => {
        const control = button(t(label), {small: true, onClick: () => {
          cron.value = expression;
          cron.dispatchEvent(new Event('input', {bubbles: true}));
        }});
        control.dataset.cron = expression;
        return control;
      }));
      const syncPresets = () => {
        for (const control of presets.children) control.setAttribute('aria-pressed', String(control.dataset.cron === cron.value.trim()));
      };
      const preview = h('div', {class: 'data-cron-preview', role: 'status'}, t('data.preview.initial'));
      const path = input({name: 'path', 'data-field': 'destination.path', value: destination.path || '', required: true, placeholder: '/var/lib/orbitbeacon/backups', autocomplete: 'off'});
      const retain = input({name: 'retain_count', type: 'number', min: 1, max: 1000, step: 1, value: plan?.retain_count ?? 7, required: true});
      const local = h('fieldset', {class: 'data-destination'}, h('div', {class: 'data-local-row'},
        field(t('data.plan.directory'), path, {hint: t('data.plan.directoryHint')}),
        field(t('data.plan.retain'), retain)));
      const provider = select(backupProviders.map(([value, key]) => [value, t(key)]), destination.provider || 's3_compatible', {name: 'provider', 'data-field': 'destination.provider'});
      const endpoint = input({name: 'endpoint', 'data-field': 'destination.endpoint', value: destination.endpoint || '', type: 'url', required: true, autocomplete: 'off'});
      const region = input({name: 'region', 'data-field': 'destination.region', value: destination.region || '', required: true, autocomplete: 'off'});
      const bucket = input({name: 'bucket', 'data-field': 'destination.bucket', value: destination.bucket || '', required: true, autocomplete: 'off'});
      const prefix = input({name: 'prefix', 'data-field': 'destination.prefix', value: destination.prefix || '', autocomplete: 'off', placeholder: 'orbitbeacon/'});
      const pathStyle = checkField(t('data.plan.pathStyle'), 'path_style', destination.path_style ?? true);
      const replace = checkField(t('data.plan.replaceCredentials'), 'replace_credentials', !plan);
      const keyID = input({name: 'access_key_id', 'data-field': 'credentials.access_key_id', autocomplete: 'off', spellcheck: false});
      const secret = input({name: 'secret_access_key', 'data-field': 'credentials.secret_access_key', type: 'password', autocomplete: 'new-password'});
      const token = input({name: 'session_token', 'data-field': 'credentials.session_token', type: 'password', autocomplete: 'new-password'});
      const credentialFields = h('fieldset', {class: 'data-destination'},
        h('div', {class: 'form-row'}, field(t('data.plan.keyID'), keyID), field(t('data.plan.secretKey'), secret)),
        field(t('data.plan.sessionToken'), token, {hint: plan ? t('data.plan.tokenHint') : undefined}));
      // 编辑时凭据只显示是否已保存，勾选“更换”才展开输入框。
      const savedCredentials = plan ? h('div', {class: 'saved-credential'},
        h('p', {class: 'text-note'}, t('data.plan.credentialsStatus', {credentials: t(plan.credentials_set ? 'data.plan.set' : 'data.plan.notSet'), token: t(plan.session_token_set ? 'data.plan.set' : 'data.plan.notSet')})),
        replace.element) : null;
      const allowPrivate = checkField(t('data.plan.allowPrivate'), 'allow_private_networks', destination.allow_private_networks || false);
      const recommendation = button(t('data.plan.useEndpoint'), {small: true, onClick: () => {
        const suggested = suggestedEndpoint(provider.value, region.value.trim());
        if (suggested) { endpoint.value = suggested; endpoint.dispatchEvent(new Event('input', {bubbles: true})); }
      }});
      const remote = h('fieldset', {class: 'data-destination'},
        h('div', {class: 'form-row'}, field(t('data.plan.provider'), provider), field(t('data.plan.region'), region)),
        field(t('data.plan.endpoint'), endpoint, {inner: h('div', {class: 'data-endpoint'}, endpoint, recommendation)}),
        h('div', {class: 'form-row'}, field(t('data.plan.bucket'), bucket), field(t('data.plan.prefix'), prefix)),
        h('div', {class: 'data-checks'}, pathStyle.element, allowPrivate.element),
        savedCredentials, credentialFields);
      const updateCredentials = () => {
        const providerChanged = Boolean(plan && provider.value !== destination.provider);
        if (providerChanged) replace.control.checked = true;
        replace.control.disabled = !plan || providerChanged;
        const active = kind.value === 'object' && replace.control.checked;
        credentialFields.disabled = !active;
        credentialFields.hidden = !active;
        keyID.required = active;
        secret.required = active;
      };
      const updateProvider = (changed = false) => {
        const value = provider.value;
        if (value === 'r2') region.value = 'auto';
        region.readOnly = value === 'r2';
        pathStyle.control.disabled = value !== 's3_compatible';
        if (value !== 's3_compatible') pathStyle.control.checked = value === 'r2';
        else if (changed) pathStyle.control.checked = true;
        const suggested = suggestedEndpoint(value, region.value.trim());
        endpoint.placeholder = value === 'r2' ? 'https://<account>.r2.cloudflarestorage.com' : suggested || 'https://storage.example.com';
        bucket.placeholder = value === 'cos' ? 'examplebucket-1250000000' : '';
        recommendation.hidden = !['cos', 'oss', 's3'].includes(value);
        recommendation.disabled = !suggested;
        recommendation.title = suggested;
        if (changed && plan && value !== destination.provider) replace.control.checked = true;
        updateCredentials();
      };
      const updateKind = () => {
        const isLocal = kind.value === 'local';
        for (const control of kindButtons) control.setAttribute('aria-pressed', String(control.dataset.kind === kind.value));
        local.hidden = !isLocal;
        local.disabled = !isLocal;
        remote.hidden = isLocal;
        remote.disabled = isLocal;
        updateCredentials();
      };
      const previewCron = async () => {
        const epoch = ++previewEpoch;
        previewController?.abort();
        previewController = new AbortController();
        const previewSignal = previewController.signal;
        const cancel = () => previewController?.abort();
        controller.signal.addEventListener('abort', cancel, {once: true});
        preview.textContent = t('data.preview.checking');
        try {
          const result = await api('/backup-plans/preview', {method: 'POST', body: {cron: cron.value}, signal: previewSignal});
          if (controller.signal.aborted || previewSignal.aborted || epoch !== previewEpoch) return;
          cron.showError('');
          preview.replaceChildren(h('p', null, t('data.timezone.label', {timezone: timezoneText(result)})), h('ol', null, result.next_runs.map((run) => h('li', null, localRun(run)))));
        } catch (error) {
          if (controller.signal.aborted || previewSignal.aborted || epoch !== previewEpoch) return;
          const reason = error.fields?.cron;
          if (reason) cron.showError(fieldErrorText(reason));
          preview.textContent = reason ? t('data.preview.invalid') : t('data.preview.unavailable', {error: errorText(error)});
        } finally { controller.signal.removeEventListener('abort', cancel); }
      };
      cron.addEventListener('input', () => {
        syncPresets();
        clearTimeout(previewTimer);
        previewEpoch += 1;
        previewController?.abort();
        preview.textContent = t('data.preview.waiting');
        previewTimer = setTimeout(previewCron, 500);
      });
      provider.addEventListener('change', () => updateProvider(true));
      region.addEventListener('input', () => updateProvider());
      replace.control.addEventListener('change', updateCredentials);
      form.append(h('div', {class: 'plan-name-row'}, field(t('data.plan.name'), name), field(t('data.plan.enabled'), enabled)),
        categories.element,
        h('h3', {class: 'data-form-section'}, t('data.plan.schedule')),
        presets, field(t('data.plan.cron'), cron, {hint: t('data.plan.cronHint')}), preview,
        h('h3', {class: 'data-form-section'}, t('data.plan.destination')),
        kindControl, local, remote);
      syncPresets();
      controls = {name, kind, enabled, categories, cron, path, retain, provider, endpoint, region, bucket, prefix, pathStyle: pathStyle.control, replace: replace.control, keyID, secret, token, allowPrivate: allowPrivate.control};
      updateProvider();
      updateKind();
      previewTimer = setTimeout(previewCron, 0);
    },
    action: async () => {
      const c = controls;
      const body = {name: c.name.value.trim(), enabled: c.enabled.checked, categories: c.categories.value(), cron: c.cron.value.trim()};
      if (!plan) body.kind = c.kind.value;
      if (c.kind.value === 'local') {
        body.destination = {path: c.path.value.trim()};
        body.retain_count = Number(c.retain.value);
      } else {
        body.destination = {provider: c.provider.value, endpoint: c.endpoint.value.trim(), region: c.region.value.trim(), bucket: c.bucket.value.trim(), prefix: c.prefix.value.trim(), path_style: c.pathStyle.checked, allow_private_networks: c.allowPrivate.checked};
        if (!plan || c.replace.checked) body.credentials = {access_key_id: c.keyID.value, secret_access_key: c.secret.value, ...(c.token.value ? {session_token: c.token.value} : {})};
      }
      try {
        await api(plan ? `/backup-plans/${encodeURIComponent(plan.id)}` : '/backup-plans', {method: plan ? 'PATCH' : 'POST', body, signal: controller.signal});
        if (!controller.signal.aborted) await onSaved();
      } catch (error) {
        if (error.fields?.destination) error.fields = {...error.fields, [c.kind.value === 'local' ? 'destination.path' : 'destination.endpoint']: error.fields.destination};
        if (error.fields?.credentials) error.fields = {...error.fields, 'credentials.access_key_id': error.fields.credentials};
        throw error;
      }
    }
  });
  dialog.closed.finally(() => {
    clearTimeout(previewTimer);
    previewEpoch += 1;
    previewController?.abort();
    controller.abort();
    signal.removeEventListener('abort', abort);
    if (controls) { controls.keyID.value = ''; controls.secret.value = ''; controls.token.value = ''; }
    controls = null;
  });
  return dialog;
}
