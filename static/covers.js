'use strict';
const coverState = {video: null, candidates: [], busy: false};
const coverMatchNames = {likely: 'AI：可能匹配', uncertain: 'AI：待核对', unlikely: 'AI：可能不匹配', unreviewed: '待手动核对', local: '本地截图', embedded: '文件内嵌海报'};
function coverBusy(busy) {
  coverState.busy = busy;
  ['cover-search', 'cover-embedded', 'cover-snapshot', 'cover-source', 'cover-query', 'cover-ai', 'cover-seconds', 'cover-overwrite'].forEach(id => $(id).disabled = busy);
  $('cover-save').disabled = busy || !document.querySelector('input[name="cover-choice"]:checked');
}
function renderCoverCandidates(candidates) {
  coverState.candidates = candidates;
  $('cover-candidates').innerHTML = candidates.map(c => `<label class="cover-card"><input type="radio" name="cover-choice" value="${escapeHtml(c.id)}"><img src="${escapeHtml(c.preview_url)}" alt="${escapeHtml(c.title || '候选海报')}" loading="lazy"><strong>${escapeHtml(c.title || '来源未提供标题')}</strong><span>${c.width} × ${c.height} · ${escapeHtml(coverMatchNames[c.match] || '待核对')}</span><small>${escapeHtml(c.reason)}</small>${c.source_url ? `<a href="${escapeHtml(c.source_url)}" target="_blank" rel="noopener noreferrer">查看来源网页 ↗</a>` : ''}</label>`).join('');
  $('cover-save').disabled = true;
}
async function loadCurrentCover() {
  const data = await api('/api/covers?id=' + coverState.video.id + '&scope=' + $('cover-scope').value);
  $('cover-target').textContent = '保存位置：' + data.path + '。保存海报后，下一次保存 NFO 会包含图片引用。';
  $('cover-current').hidden = !data.exists;
  $('cover-current').innerHTML = data.exists ? `<img src="${escapeHtml(data.preview_url)}&t=${Date.now()}" alt="当前海报"><div><strong>当前海报</strong><p>${escapeHtml(data.path)}</p>${data.current?.source_url ? `<a href="${escapeHtml(data.current.source_url)}" target="_blank" rel="noopener noreferrer">查看来源 ↗</a>` : ''}</div>` : '';
  const aiCandidates = (data.candidates || []).filter(candidate => candidate.kind === 'web' && ['likely', 'uncertain', 'unlikely'].includes(candidate.match));
  if (aiCandidates.length) {
    renderCoverCandidates(aiCandidates);
    $('cover-feedback').textContent = `已加载 ${aiCandidates.length} 张 AI 识别候选海报，可直接选择。`;
  }
  return data;
}
async function openCovers(id, scope = 'video', missing = false) {
  if (coverState.busy) { toast('上一项海报操作尚未结束，请稍后重试', true); return; }
  const video = state.videos.find(v => v.id === id); if (!video) return;
  coverState.video = video;
  $('cover-dialog').querySelector('.dialog-message')?.remove();
  $('cover-current').hidden = true; $('cover-current').replaceChildren();
  const series = video.kind === 'episode' && scope === 'series';
  $('cover-title').textContent = series ? '识别剧集海报' : '识别本集海报';
  $('cover-video').textContent = series && video.show_path ? '剧集目录：' + video.show_path : video.path;
  $('cover-scope').value = series ? 'series' : 'video';
  $('cover-source').value = ''; $('cover-query').value = ''; $('cover-overwrite').checked = false;
  $('cover-ai').checked = !!(state.settings.model && state.settings.base_url);
  $('cover-feedback').textContent = $('cover-scope').value === 'series' ? '填写剧集来源链接或直接联网查找剧集海报。' : '填写本集来源链接或直接联网查找海报。';
  renderCoverCandidates([]); $('cover-dialog').showModal();
  coverBusy(true); try { await loadCurrentCover(); if (!missing) await loadEmbeddedCovers(); } finally { coverBusy(false); }
}
async function loadEmbeddedCovers() {
  $('cover-feedback').textContent = '正在读取视频内嵌海报…';
  try {
    const data = await api('/api/covers/embedded', {id: coverState.video.id, scope: $('cover-scope').value});
    const existing = coverState.candidates || [];
    const candidates = [...existing];
    for (const candidate of (data.candidates || [])) {
      if (!candidates.some(item => item.id === candidate.id)) candidates.push(candidate);
    }
    renderCoverCandidates(candidates);
    $('cover-feedback').textContent = `找到 ${data.candidates.length} 张内嵌海报。${data.warnings.join('；')}`;
  } catch (err) { $('cover-feedback').textContent = err.message + '；可继续联网查找或截图。'; }
}
$('cover-embedded').onclick = run(async () => { coverBusy(true); try { await loadEmbeddedCovers(); } finally { coverBusy(false); } });
$('video-list').addEventListener('click', run(async event => { const button = event.target.closest('[data-cover]'); if (button) await openCovers(button.dataset.cover, 'video', button.dataset.coverMissing === 'true'); }));
$('cover-candidates').onchange = () => coverBusy(coverState.busy);
$('cover-form').onsubmit = run(async event => {
  event.preventDefault(); $('cover-dialog').querySelector('.dialog-message')?.remove(); coverBusy(true); renderCoverCandidates([]); $('cover-feedback').textContent = '正在检索来源、提取图片并判断匹配度，最多等待约 105 秒…';
  try {
    const data = await api('/api/covers/search', {id: coverState.video.id, scope: $('cover-scope').value, source_url: $('cover-source').value.trim(), query: $('cover-query').value.trim(), use_ai: $('cover-ai').checked});
    renderCoverCandidates(data.candidates);
    $('cover-feedback').textContent = `找到 ${data.candidates.length} 张候选海报。搜索词：${data.query}。${data.warnings.join('；')}`;
  } catch (err) { $('cover-feedback').textContent = err.message; throw err; }
  finally { coverBusy(false); }
});
$('cover-snapshot').onclick = run(async () => {
  const seconds = Number($('cover-seconds').value);
  if (!Number.isFinite(seconds) || seconds < 0 || seconds > 86400) throw new Error('截图时间应在 0–86400 秒之间');
  coverBusy(true); $('cover-feedback').textContent = '正在从当前视频提取画面…';
  try {
    const candidate = await api('/api/covers/snapshot', {id: coverState.video.id, scope: $('cover-scope').value, seconds});
    renderCoverCandidates([...coverState.candidates, candidate]); $('cover-feedback').textContent = '已提取画面，选择图片后点击保存。';
  } catch (err) { $('cover-feedback').textContent = err.message; throw err; }
  finally { coverBusy(false); }
});
$('cover-save').onclick = run(async () => {
  const selected = document.querySelector('input[name="cover-choice"]:checked'); if (!selected) return;
  coverBusy(true);
  try {
    const result = await api('/api/covers/save', {id: coverState.video.id, scope: $('cover-scope').value, candidate_id: selected.value, overwrite: $('cover-overwrite').checked});
    $('cover-feedback').textContent = result.saved ? '海报已保存：' + result.path : '已有海报已保留。如需替换，请勾选覆盖后再保存。';
    await loadCurrentCover();
    await refresh();
  } finally { coverBusy(false); }
});
function searchProviderHint() {
  $('external-search-settings').hidden = !['tavily', 'searxng'].includes($('search-provider').value);
}
$('search-provider').onchange = () => {
  searchProviderHint();
  if ($('search-provider').value === 'tavily' && !$('search-url').value) $('search-url').value = 'https://api.tavily.com';
};
