import test from 'node:test';
import assert from 'node:assert/strict';
import {errorText, fieldErrorText} from '../src/shared/errors.js';
import {setLocalePreference} from '../src/console/i18n.js';

test('error text never renders server or network error messages', () => {
  const original = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  let preference = 'en';
  Object.defineProperty(globalThis, 'localStorage', {configurable: true, value: {getItem: () => preference, setItem(_key, value) { preference = value; }}});
  try {
    setLocalePreference('zh-CN');
    const hostile = {code: 'unknown_server_code', message: '<img src=x onerror=alert(1)> private server detail', requestID: 'request-123'};
    const text = errorText(hostile);
    assert.ok(text.includes('request-123'));
    assert.ok(!text.includes(hostile.message));
    assert.ok(!text.includes(hostile.code));
    assert.equal(errorText({code: 'password_unchanged'}), '请选择与当前密码不同的新密码。');
    assert.equal(errorText({code: 'public_page_disabled'}), '状态页已关闭。');
    assert.equal(errorText({code: 'password_change_required'}), '请先修改密码，再访问控制台。');
    assert.notEqual(fieldErrorText('must be valid UTF-8 without control characters and at most 255 bytes'), fieldErrorText('must be valid UTF-8 without control characters and at most 128 bytes'));
    assert.equal(fieldErrorText('arbitrary English server text'), '请检查此值。');
    assert.ok(!errorText(new Error('TLS secret details')).includes('TLS secret details'));
  } finally { if (original) Object.defineProperty(globalThis, 'localStorage', original); else delete globalThis.localStorage; }
});
