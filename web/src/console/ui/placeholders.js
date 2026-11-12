import {h} from '../../shared/dom.js';
import {t} from '../i18n.js';

// 与真实页面同构的加载占位：沿用页面、面板、表格的布局类名，只把数值换成微光条，
// 数据到达后路由器原位替换，用户看到的是数值填入而不是整页闪现。
// 占位不调用 uid()（静默刷新依赖稳定 ID），也不含可聚焦控件。

export const skText = (width = '60%') => h('span', {class: 'sk sk-text', style: `width:${width}`, 'aria-hidden': 'true'});

export const skValue = () => h('span', {class: 'sk sk-value', 'aria-hidden': 'true'});

export const skLines = (...widths) => h('div', {class: 'sk-lines', 'aria-hidden': 'true'}, widths.map((width) => skText(width)));

// 页面头部：已知标题直接显示文字；未知标题（节点名）用微光条，保持同样的行高。
export function placeholderHead(title, {status = true, actions = 1} = {}) {
  return h('header', {class: 'page-head'},
    h('div', {class: 'page-title'}, h('h1', null, title || skText('9em'))),
    status ? h('div', {class: 'head-status'}, h('span', {class: 'live-status'}, t('controls.live.connecting'))) : null,
    actions ? h('div', {class: 'head-actions'}, Array.from({length: actions}, () => h('span', {class: 'sk sk-button', 'aria-hidden': 'true'}))) : null);
}

export function placeholderPanel(title, ...body) {
  return h('section', {class: 'panel'},
    title ? h('div', {class: 'panel-head'}, h('h2', null, title)) : null,
    h('div', {class: 'panel-body'}, body));
}

const CELL_WIDTHS = ['72%', '48%', '60%', '36%', '54%'];

export function placeholderRows(columns, rows = 6) {
  return h('tbody', null, Array.from({length: rows}, (_, row) => h('tr', null, columns.map((column, index) => {
    const className = typeof column === 'object' && column ? column.className : '';
    return h('td', {class: className || null}, skText(CELL_WIDTHS[(row + index) % CELL_WIDTHS.length]));
  }))));
}

// columns 为表头文字，或 {label, className} 以沿用真实表格的列隐藏类。
export function placeholderTable(columns, {rows = 6, className = ''} = {}) {
  return h('div', {class: 'table-wrap'}, h('table', {class: `table ${className}`.trim()},
    h('thead', null, h('tr', null, columns.map((column) => typeof column === 'object' && column
      ? h('th', {scope: 'col', class: column.className || null}, column.label) : h('th', {scope: 'col'}, column)))),
    placeholderRows(columns, rows)));
}

// 与历史图表同比例（640×240）的空图框：几条网格线加微光。
export function placeholderChart() {
  return h('div', {class: 'sk-chart', 'aria-hidden': 'true'}, h('i'), h('i'), h('i'), h('i'));
}

export function pagePlaceholder(...children) {
  return h('div', {class: 'page-placeholder', 'aria-busy': 'true', 'aria-label': t('controls.loading')}, children);
}
