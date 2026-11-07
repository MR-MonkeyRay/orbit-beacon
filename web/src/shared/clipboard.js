export async function copyElementText(element) {
  try {
    if (!navigator.clipboard?.writeText) throw new Error('Clipboard API unavailable');
    await navigator.clipboard.writeText(element.textContent);
    return 'clipboard';
  } catch {
    const range = document.createRange();
    range.selectNodeContents(element);
    const selection = getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    try { return document.execCommand('copy') ? 'fallback' : false; }
    catch { return false; }
  }
}
