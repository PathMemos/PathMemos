
const _MAX_MD_LENGTH = 5000;

// 降级路径（超长/解析异常）：转义后仅保留换行结构，不做 markdown 解析。
function _escapePlainText(text: string): string {
  return escapeHtml(text).replace(/\n/g, '<br>');
}

export function markdownToHtml(md: string): string {
  try {
    return _markdownToHtml(md);
  } catch (e) {
    console.error('markdown parse error', e);
    return _escapePlainText(md || '');
  }
}

function _markdownToHtml(md: string): string {
  if (!md) return '';

  
  if (md.length > _MAX_MD_LENGTH) {
    // 超长降级：不做 markdown 解析，但仍需转义（rich-text 标签白名单兜底之外的
    // 意外标签渲染/样式注入面），与正常路径的 escapeHtml 同语义。
    return _escapePlainText(md);
  }

  let html = md;

  
  // 占位符加随机后缀，防止用户文本/AI 输出碰撞导致内容错乱。
  const placeholderSuffix = Math.random().toString(36).slice(2, 8);
  const codeBlocks: string[] = [];
  html = html.replace(/```([\s\S]*?)```/g, (_, code) => {
    codeBlocks.push(code);
    return `\0CB_${codeBlocks.length - 1}_${placeholderSuffix}\0`;
  });

  
  const inlineCodes: string[] = [];
  html = html.replace(/`([^`]+)`/g, (_, code) => {
    inlineCodes.push(code);
    return `\0IC_${inlineCodes.length - 1}_${placeholderSuffix}\0`;
  });

  
  html = escapeHtml(html);

  
  html = html.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');

  
  html = html.replace(/\n/g, '<br>');

  
  html = html.replace(new RegExp(`\\0CB_(\\d+)_${placeholderSuffix}\\0`, 'g'), (_, idx) => {
    const code = escapeHtml(codeBlocks[Number(idx)]);
    return `<pre style="background:#f6f8fa;padding:10px;border-radius:6px;overflow-x:auto;font-family:monospace;font-size:13px;line-height:1.4;">${code}</pre>`;
  });

  
  html = html.replace(new RegExp(`\\0IC_(\\d+)_${placeholderSuffix}\\0`, 'g'), (_, idx) => {
    const code = escapeHtml(inlineCodes[Number(idx)]);
    return `<code style="background:#f1f3f4;padding:2px 5px;border-radius:3px;font-family:monospace;font-size:13px;">${code}</code>`;
  });

  return html;
}

function escapeHtml(text: string): string {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}
