// 控制台在本地留下“曾登录”标记，公开状态页只在有标记时才查询 /api/v1/me，
// 匿名访客因此不会多发请求。标记不含凭据，只是“值得一问”的提示；
// 存储被禁用或抛错时视为没有标记。
const KEY = 'orbitbeacon.signed-in';

export function rememberSignedIn(signedIn) {
  try {
    if (signedIn) localStorage.setItem(KEY, '1');
    else localStorage.removeItem(KEY);
  } catch { /* 隐私模式或测试替身：放弃标记 */ }
}

export function maybeSignedIn() {
  try { return localStorage.getItem(KEY) === '1'; } catch { return false; }
}
