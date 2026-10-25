'use strict';
const $ = id => document.getElementById(id);
const state = {videos: [], jobs: [], settings: {}, selected: new Set(), coverCandidates: {}, kind: 'all', page: 'library', folder: '', editor: null, coverPreview: null, directoryCover: null, expandedSeries: new Set(), directoryPath: '.', navigationOpen: false, includeDescendants: false, listPage: 1, pageSize: 25, visibleIDs: new Set(), batch: '', loading: false};
const statusNames = {new: '等待整理', draft: '待确认', saved: '已保存', error: '处理失败', generating: '生成中'};
const jobNames = {queued: '排队中', running: '进行中', cancelling: '正在取消', cancelled: '已取消', completed: '已完成', partial: '部分失败', interrupted: '已中断'};
const escapeHtml = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));
const jobActive = j => ['queued', 'running', 'cancelling'].includes(j.status);
const activeTextJob = () => state.jobs.find(j => j.action !== 'covers' && jobActive(j));
const activeJob = () => activeTextJob() || state.jobs.find(jobActive);
const jobTitle = action => ({generate:'AI 生成资料', save:'保存 NFO', covers:'自动获取封面'})[action] || action;
let coverDetailID = '';
let toastTimer;
let cleanupIDs = [];
let clearRecordsRequest = null;
function toast(message, error = false) {
  const dialog = document.querySelector('dialog[open]');
  if (dialog && error) {
    let feedback = dialog.querySelector('.dialog-message');
    if (!feedback) { feedback = document.createElement('p'); feedback.className = 'error dialog-message'; feedback.setAttribute('role', 'alert'); dialog.querySelector('form').append(feedback); }
    feedback.textContent = message;
    feedback.scrollIntoView({block: 'nearest'});
  }
  $('toast').textContent = message; $('toast').hidden = false; $('toast').classList.toggle('error-toast', error);
  clearTimeout(toastTimer); toastTimer = setTimeout(() => $('toast').hidden = true, error ? 8500 : 5500);
}
async function api(path, body) {
  const options = body === undefined ? {} : {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)};
  let response;
  try { response = await fetch(path, options); } catch { throw new Error('无法连接 NAS 服务，请检查网络后重试'); }
  const data = await response.json();
  if (!response.ok) {
    if (response.status === 401) { $('workspace').hidden = true; $('login').hidden = false; }
    throw new Error(data.error || '操作失败');
  }
  return data;
}
function run(fn) { return async event => { try { await fn(event); } catch (err) { toast(err.message, true); } }; }
function size(bytes) {
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
  if (bytes < 1024 ** 3) return (bytes / 1024 ** 2).toFixed(1) + ' MB';
  return (bytes / 1024 ** 3).toFixed(2) + ' GB';
}
function filtered() {
  const search = $('search').value.trim().toLocaleLowerCase(), filter = $('status-filter').value;
  const descendants = state.includeDescendants || !!search || filter !== 'all';
  const videos = state.videos.filter(v => (state.kind === 'all' || v.kind === state.kind)
    && (state.kind !== 'episode' || inDirectory(v, state.directoryPath, descendants))
    && matchesVideo(v, search, filter));
  return state.kind === 'episode' ? videos.sort(episodeOrder) : videos;
}
function renderSelection() {
  const videos = filtered(), selected = videos.filter(v => state.selected.has(v.id));
  $('select-all').checked = !!videos.length && selected.length === videos.length;
  $('select-all').indeterminate = selected.length > 0 && selected.length < videos.length;
  $('select-all').disabled = !videos.length;
  $('select-results').disabled = !videos.length;
  $('select-results').title = `选择全部 ${videos.length} 条筛选结果，包含其他页`;
  const hidden = [...state.selected].filter(id => !state.visibleIDs.has(id)).length;
  $('selected-count').textContent = state.selected.size ? `已选 ${state.selected.size} 项${hidden ? ` · ${hidden} 项不在当前页` : ''}` : '勾选视频或目录以批量处理';
  $('clear-selection').hidden = !state.selected.size;
  $('selection-toolbar').classList.toggle('has-selection', state.selected.size > 0);
  $('select-all').setAttribute('aria-label', `选择当前筛选结果中的全部 ${videos.length} 个视频（跨页）`);
  $('generate').disabled = !state.selected.size || !!activeTextJob();
  $('auto-covers').disabled = !state.selected.size;
  $('write').disabled = !state.selected.size || !!activeTextJob();
  $('open-cleanup').disabled = !!activeJob() || state.root_available === false;
  $('clear-selected').disabled = !state.selected.size || !!activeJob();
  $('clear-all-records').disabled = !state.videos.length || !!activeJob();
  updateSeriesSelection();
}
function coverLabel(v, scope) { return scope === 'series' ? '系列封面' : v.kind === 'episode' ? '单集封面' : '视频封面'; }
function listCoverThumbnails(v) {
  const covers = (v.covers || []).filter(c => c.scope === 'video');
  if (!covers.length) return `<div class="list-cover-thumbnails"><button type="button" class="list-cover-thumb list-cover-empty" data-cover-preview="${v.id}" data-cover-scope="video" aria-expanded="false" aria-label="查看候选封面：${escapeHtml(v.filename)}"><span class="no-cover">暂无封面</span></button></div>`;
  return '<div class="list-cover-thumbnails">' + covers.map(c => {
    const open = state.coverPreview?.id === v.id && state.coverPreview.scope === c.scope;
    const label = coverLabel(v, c.scope);
    return `<button type="button" class="list-cover-thumb" data-cover-preview="${v.id}" data-cover-scope="${c.scope}" aria-expanded="${open}" aria-controls="cover-preview-${v.id}" aria-label="${open ? '收起' : '查看'}${label}：${escapeHtml(v.filename)}"><img data-list-cover src="${escapeHtml(c.thumbnail_url)}" alt="${label}" loading="lazy" decoding="async" width="88" height="58"><span class="cover-load-error" hidden>图片不可用</span></button>`;
  }).join('') + '</div>';
}
function inlineCoverPreview(v) {
  if (state.coverPreview?.id !== v.id || state.coverPreview.scope !== 'video') return '';
  const c = v.covers?.find(c => c.scope === state.coverPreview.scope);
  const scope = 'video', label = coverLabel(v, scope);
  const cached = state.coverCandidates[v.id + ':' + scope];
  const candidates = Array.isArray(cached) ? inlineCoverCandidatesMarkup(cached, v.id, scope) : '';
  const stage = c ? `<div class="list-cover-stage"><img data-list-cover src="${escapeHtml(c.url)}" alt="${escapeHtml(v.filename)}的${label}大图" decoding="async"><p class="cover-load-error" role="status" hidden>封面无法加载，请检查文件是否仍可访问。</p></div>` : '';
  return `<tr class="cover-preview-row"><td colspan="6"><section class="list-cover-preview" id="cover-preview-${v.id}" aria-label="${label}候选封面"><section class="inline-cover-candidates" aria-label="其他候选封面" id="cover-candidates-${v.id}-${scope}">${candidates}</section>${stage}</section></td></tr>`;
}
function inlineCoverCandidatesMarkup(candidates, id, scope) {
  if (!candidates.length) return '';
  return `<div class="inline-cover-choice-grid">${candidates.map(c => `<button type="button" class="inline-cover-choice" data-cover-candidate="${escapeHtml(c.id)}" data-cover-video="${id}" data-cover-scope="${scope}" title="使用此封面"><img src="${escapeHtml(c.preview_url)}" alt="${escapeHtml(c.title || '候选封面')}" loading="lazy" decoding="async"></button>`).join('')}</div>`;
}
function renderInlineCoverCandidates(id, scope, candidates) {
  const container = $('cover-candidates-' + id + '-' + scope);
  if (!container) return;
  container.innerHTML = inlineCoverCandidatesMarkup(candidates, id, scope);
}
async function loadInlineCoverCandidates(id, scope) {
  const container = $('cover-candidates-' + id + '-' + scope);
  if (!container) return;
  const key = id + ':' + scope;
  if (Array.isArray(state.coverCandidates[key])) { renderInlineCoverCandidates(id, scope, state.coverCandidates[key]); return; }
  try {
    const data = await api('/api/covers?id=' + encodeURIComponent(id) + '&scope=' + encodeURIComponent(scope));
    state.coverCandidates[key] = data.candidates || [];
    renderInlineCoverCandidates(id, scope, state.coverCandidates[key]);
  } catch { container.replaceChildren(); }
}
async function saveInlineCover(button) {
  const scope = button.dataset.coverScope || 'video';
  const buttons = $('cover-candidates-' + button.dataset.coverVideo + '-' + scope)?.querySelectorAll('[data-cover-candidate]') || [];
  buttons.forEach(item => { item.disabled = true; });
  try {
    const result = await api('/api/covers/save', {id: button.dataset.coverVideo, scope, candidate_id: button.dataset.coverCandidate, overwrite: true});
    toast(result.saved ? '封面已替换，原封面已保留备份' : '封面未发生变化');
    delete state.coverCandidates[button.dataset.coverVideo + ':' + scope];
    await refresh();
    if (state.coverPreview?.id === button.dataset.coverVideo) await loadInlineCoverCandidates(button.dataset.coverVideo, state.coverPreview.scope);
    if (scope === 'series' && state.directoryCover) await loadInlineCoverCandidates(button.dataset.coverVideo, scope);
  } finally { buttons.forEach(item => { item.disabled = false; }); }
}
function videoRow(v) {
  const sequence = v.kind === 'episode' ? `S${v.season ?? '?'} E${v.episode ?? '?'} · ${v.path.includes('/') ? v.path.slice(0, v.path.lastIndexOf('/')) : '根目录'}` : v.path;
  return `<tr class="${state.selected.has(v.id) ? 'selected-row' : ''}"><td class="check"><input type="checkbox" data-select="${v.id}" aria-label="选择 ${escapeHtml(v.filename)}" ${state.selected.has(v.id) ? 'checked' : ''}></td><td class="list-cover-cell">${listCoverThumbnails(v)}</td><td><div class="video-info"><span class="video-icon ${v.kind === 'episode' ? 'series' : ''}">${v.kind === 'episode' ? '▤' : '▷'}</span><div><strong title="${escapeHtml(v.path)}">${escapeHtml(v.metadata?.title || v.filename)}</strong><small title="${escapeHtml(v.path)}">${escapeHtml(sequence)}</small></div></div></td><td><span class="badge ${v.status}">${statusNames[v.status] || v.status}</span>${v.has_nfo ? '<span class="existing-note">已有 NFO 文件</span>' : ''}${!v.exists ? '<span class="error">文件不可访问</span>' : ''}${v.error ? `<span class="existing-note" title="${escapeHtml(v.error)}">${escapeHtml(v.error.slice(0, 22))}${v.error.length > 22 ? '…' : ''}</span>` : ''}</td><td>${size(v.size)}</td><td><button class="text-button" data-edit="${v.id}" ${activeJob() ? 'disabled' : ''}>编辑资料 ↗</button><button class="text-button" data-cover="${v.id}">查找封面 ↗</button></td></tr>${inlineCoverPreview(v)}`;
}
function renderVideos() {
  const active = document.activeElement;
  const focusAttributes = ['data-directory', 'data-series-toggle', 'data-directory-cover', 'data-cover-preview', 'data-close-preview', 'data-series-select', 'data-select-directory', 'data-select'];
  const focusAttribute = focusAttributes.find(attr => active?.hasAttribute(attr));
  const focusValue = focusAttribute ? active.getAttribute(focusAttribute) : null;
  const focusScope = active?.dataset.coverScope;
  const descendantFocus = active?.id === 'include-descendants';
  renderDirectoryBrowser();
  const videos = filtered(), paged = libraryPage(videos, state.listPage, state.pageSize);
  state.listPage = paged.page;
  state.visibleIDs = new Set(paged.videos.map(v => v.id));
  if (state.coverPreview && !paged.videos.some(v => v.id === state.coverPreview.id && v.covers?.some(c => c.scope === state.coverPreview.scope))) state.coverPreview = null;
  const filtering = !!$('search').value.trim() || $('status-filter').value !== 'all';
  $('result-count').textContent = state.kind === 'episode' ? `${filtering ? '匹配' : state.includeDescendants ? '含子目录' : '本层'} ${videos.length} 集` : `${videos.length} 个视频`;
  $('reset-filters').hidden = !filtering;
  $('search').placeholder = state.kind === 'episode' ? '搜索当前目录及子目录' : '搜索标题、文件名或目录';
  $('video-table').hidden = !videos.length;
  $('empty').hidden = !!videos.length;
  $('empty').classList.toggle('compact-empty', !!state.videos.length);
  if (!videos.length) {
    const directory = state.kind === 'episode' && state.videos.some(v => v.kind === 'episode');
    $('empty').querySelector('h2').textContent = !state.videos.length ? '从一个视频目录开始' : filtering ? '没有匹配的视频' : directory ? '当前层级没有视频' : '还没有这一类型的视频';
    $('empty').querySelector('p').textContent = !state.videos.length ? '选择已挂载的 NAS 文件夹，开始整理视频资料。' : filtering ? '试着更换关键词或清除筛选。' : directory ? '打开上方子目录，或勾选「同时显示子目录的视频」。' : '添加视频目录时选择对应类型，已有视频也可以重新扫描分类。';
    $('empty-scan').hidden = directory || filtering;
    $('empty').querySelector('.steps').hidden = !!state.videos.length;
  }
  $('video-list').innerHTML = paged.videos.map(videoRow).join('');
  $('list-pagination').hidden = !videos.length;
  $('page-summary').textContent = `第 ${paged.start + 1}–${paged.start + paged.videos.length} 项，共 ${videos.length} 项`;
  $('page-position').textContent = `${paged.page} / ${paged.pages}`;
  $('page-prev').disabled = paged.page === 1; $('page-next').disabled = paged.page === paged.pages;
  renderSelection();
  if (focusAttribute) [...document.querySelectorAll('[' + focusAttribute + ']')].find(el => el.getAttribute(focusAttribute) === focusValue && (!focusScope || el.dataset.coverScope === focusScope))?.focus({preventScroll:true});
  if (descendantFocus) $('include-descendants')?.focus({preventScroll:true});
}
function renderJobs() {
  const job = activeJob();
  $('job-banner').hidden = !job; $('job-count').textContent = String(state.jobs.filter(jobActive).length);
  if (job) {
    $('job-title').textContent = `${jobTitle(job.action)} · ${jobNames[job.status]}`;
    $('job-description').textContent = `${job.done} / ${job.total} 项已处理，${job.skipped} 项跳过，${job.failed} 项失败。可关闭网页，任务在后台继续。`;
    $('job-progress').value = job.total ? 100 * job.done / job.total : 0;
    $('cancel-job').disabled = job.status === 'cancelling';
    $('cancel-job').textContent = job.status === 'cancelling' ? '等待当前请求结束' : '取消任务';
  }
  $('jobs-list').innerHTML = state.jobs.length ? state.jobs.map(j => `<article class="job-card"><header><strong>${jobTitle(j.action)}</strong><span class="badge ${j.status === 'completed' ? 'saved' : j.failed ? 'error' : ''}">${jobNames[j.status] || j.status}</span></header><p>${j.done} / ${j.total} 项已处理 · 成功 ${j.done - j.failed - j.skipped} · 跳过 ${j.skipped} · 失败 ${j.failed}</p><p class="hint">${new Date(j.created * 1000).toLocaleString('zh-CN')}${j.message ? ' · ' + escapeHtml(j.message) : ''}</p><div class="form-actions">${jobActive(j) ? `<button class="secondary small" data-cancel-job="${j.id}" ${j.status === 'cancelling' ? 'disabled' : ''}>取消此任务</button>` : ''}${j.action !== 'covers' && !jobActive(j) && (j.failed || ['cancelled','interrupted'].includes(j.status)) ? `<button class="secondary small" data-retry-job="${j.id}">重试失败 / 未完成项</button>` : ''}${j.action === 'covers' ? `<button class="secondary small" data-cover-detail="${j.id}">查看封面明细</button>${!jobActive(j) && (j.failed || ['cancelled','interrupted'].includes(j.status)) ? `<button class="secondary small" data-retry-covers="${j.id}">重试未完成封面</button>` : ''}` : ''}</div></article>`).join('') : '<div class="jobs-empty">还没有批量任务。在视频库选择视频后即可开始。</div>';
}
async function refresh() {
  if (state.loading) return;
  state.loading = true;
  try {
    const data = await api('/api/state');
    Object.assign(state, data);
    state.selected = new Set([...state.selected].filter(id => state.videos.some(v => v.id === id)));
    $('root-label').textContent = data.media_root; $('settings-root').textContent = data.media_root;
    $('mount-warning').hidden = data.root_available; $('model-notice').hidden = !!(data.settings.base_url && data.settings.model);
    $('nav-count').textContent = state.videos.length; $('stat-total').textContent = state.videos.length;
    ['new', 'draft', 'saved'].forEach(x => $('stat-' + x).textContent = state.videos.filter(v => v.status === x).length);
    renderJobs(); renderVideos();
    if ($('cover-tasks-dialog').open) await loadCoverTasks();
  } finally { state.loading = false; }
}
function fillSettings() {
  const config = state.settings;
  $('concurrency').value = config.concurrency || 1;
  $('retry-count').value = config.retry_count ?? 2;
  $('provider').value = config.provider || 'compatible'; $('base-url').value = config.base_url || ''; $('model').value = config.model || '';
  $('api-key').value = ''; $('api-key').placeholder = config.has_key ? '已保存密钥，留空保持不变' : '填写密钥（本地服务可留空）';
  $('search-provider').value = config.search_provider || 'model'; $('search-url').value = config.search_url || ''; $('search-key').value = ''; $('clear-search-key').checked = false;
  $('search-key-hint').textContent = config.has_search_key ? '已保存搜索密钥，留空保持不变。' : '搜索密钥与文字模型密钥独立，保存在 NAS 本地。';
  searchProviderHint();
  $('json-mode').checked = config.json_mode !== false; $('clear-key').checked = false; $('test-result').textContent = ''; providerHint();
}
function page(name) {
  state.page = name;
  ['library', 'jobs', 'settings'].forEach(p => { $(p + '-page').hidden = p !== name; $('nav-' + p).classList.toggle('active', p === name); });
  $('page-label').textContent = {library: '视频库', jobs: '批量任务', settings: '模型设置'}[name];
  if (name === 'settings') fillSettings();
}
function providerHint() {
  const ollama = $('provider').value === 'ollama';
  $('url-help').textContent = ollama ? '填写 Ollama 的服务地址，程序会追加 /api/chat。容器内 localhost 指容器自身。' : '填写基础地址，程序会追加 /chat/completions。例如服务要求 /v1 时需保留 /v1。';
  $('json-mode').disabled = ollama;
}
async function browse(path = '') {
  const data = await api('/api/directories?path=' + encodeURIComponent(path));
  state.folder = data.path === '.' ? '' : data.path;
  $('folder-path').value = state.folder; $('folder-location').textContent = `${state.media_root}/${state.folder}`.replace(/\/$/, '');
  $('folder-up').disabled = !state.folder;
  $('folder-list').innerHTML = data.directories.length ? data.directories.map(d => `<button type="button" data-folder="${escapeHtml(d.path)}"><span>▱ &nbsp; ${escapeHtml(d.name)}</span><span>›</span></button>`).join('') : '<p>没有下级文件夹，可以直接扫描此目录</p>';
}
async function openScan() {
  $('scan-dialog').querySelector('.dialog-message')?.remove();
  if (state.kind !== 'all') $('scan-kind').value = state.kind;
  $('series-help').hidden = $('scan-kind').value !== 'episode';
  $('scan-feedback').textContent = ''; $('scan-dialog').showModal();
  try { await browse(state.folder); } catch (err) { $('folder-list').innerHTML = '<p>目录不可访问，请检查挂载和权限</p>'; throw err; }
}
function batch(action) {
  state.batch = action;
  const count = state.selected.size, generate = action === 'generate', covers = action === 'covers';
  $('batch-title').textContent = covers ? '自动获取并保存封面' : generate ? '批量生成视频资料' : '将资料保存为 NFO';
  $('batch-description').textContent = covers ? `已选择 ${count} 个视频，同系列公共封面只处理一次。任务在后台运行，全局最多并发 ${state.settings.concurrency || 1} 项。` : `已选择 ${count} 个视频。${generate ? '文字结果先保存为草稿；封面作为独立后台任务获取。' : '将当前草稿写入视频目录。'}`;
  $('batch-notice').textContent = covers ? '按内嵌封面、AI 联网匹配、视频截图的顺序获取，自动选择并保存；已有封面保留。可能产生模型与搜索费用。' : generate ? '文件名、目录、内嵌标签、字幕和简介片段将发送到 AI。自动封面会调用联网搜索并直接写入图片文件，已有封面保留。文字草稿仍需确认保存。' : '已有 NFO 默认保留。封面仍在获取时，NFO 可能尚不包含图片引用，可等待封面完成后再保存。';
  $('overwrite').parentElement.hidden = covers;
  $('overwrite').checked = false;
  $('overwrite-label').textContent = generate ? '重新生成已有草稿（将替换手动修改的内容）' : '覆盖已有 NFO（每次覆盖前先保留 .bak 备份）';
  $('auto-cover-option').hidden = !generate;
  $('batch-auto-covers').checked = true;
  $('batch-submit').textContent = covers ? '开始自动获取' : generate ? '开始生成' : '保存到 NAS';
  $('batch-dialog').showModal();
}

function editor(id) {
  $('editor-dialog').querySelector('.dialog-message')?.remove();
  const row = state.videos.find(v => v.id === id); if (!row) return;
  state.editor = row;
  const data = row.metadata || {title: row.filename.replace(/\.[^.]+$/, ''), plot: '', genres: [], tags: [], notes: ''};
  $('edit-title').value = data.title; $('edit-plot').value = data.plot; $('edit-genres').value = (data.genres || []).join('，'); $('edit-tags').value = (data.tags || []).join('，');
  $('edit-directors').value = (data.directors || []).join('，');
  $('editor-path').textContent = row.path;
  $('series-fields').hidden = row.kind !== 'episode'; $('edit-show-title').required = row.kind === 'episode';
  $('edit-show-title').value = row.show?.title || ''; $('edit-show-plot').value = row.show?.plot || '';
  $('edit-show-directors').value = (row.show?.directors || []).join('，');
  $('edit-season').value = row.season ?? ''; $('edit-episode').value = row.episode ?? '';
  $('editor-warning').hidden = !(row.error || row.has_nfo);
  $('editor-warning').textContent = row.error || '此视频已有 NFO。本页面编辑的是工作台草稿，不会自动导入原 NFO；默认保存时会跳过已有文件。';
  $('editor-notes').textContent = data.notes ? '资料说明：' + data.notes : '请核对简介是否符合实际内容；不确定的信息可以留空。';
  $('nfo-preview').hidden = true; $('nfo-preview').replaceChildren();
  $('source-preview').textContent = '展开后读取文件名、内嵌标签、字幕和简介。'; $('source-preview').parentElement.open = false;
  $('editor-dialog').showModal();
}
const splitTags = value => value.split(/[,，]/).map(x => x.trim()).filter(Boolean);
async function saveDraft() {
  if (!$('editor-form').reportValidity()) return false;
  const row = state.editor;
  const body = {id: row.id, metadata: {title: $('edit-title').value, plot: $('edit-plot').value, directors: splitTags($('edit-directors').value), genres: splitTags($('edit-genres').value), tags: splitTags($('edit-tags').value), notes: row.metadata?.notes || ''}};
  if (row.kind === 'episode') {
    body.season = $('edit-season').value === '' ? null : Number($('edit-season').value);
    body.episode = $('edit-episode').value === '' ? null : Number($('edit-episode').value);
    body.show = {...row.show, title: $('edit-show-title').value, plot: $('edit-show-plot').value, directors: splitTags($('edit-show-directors').value)};
  }
  state.editor = await api('/api/edit', body); await refresh(); return true;
}
async function saveSettings() {
  if (!$('settings-form').reportValidity()) return false;
  state.settings = await api('/api/settings', {retry_count: Number($('retry-count').value), concurrency: Number($('concurrency').value), provider: $('provider').value, base_url: $('base-url').value, model: $('model').value, api_key: $('api-key').value, json_mode: $('json-mode').checked, clear_key: $('clear-key').checked, search_provider: $('search-provider').value, search_url: $('search-url').value, search_key: $('search-key').value, clear_search_key: $('clear-search-key').checked});
  fillSettings(); await refresh(); return true;
}

['library', 'jobs', 'settings'].forEach(name => $('nav-' + name).onclick = () => page(name));
$('setup-model').onclick = () => page('settings'); $('provider').onchange = providerHint;
['open-scan', 'empty-scan'].forEach(id => $(id).onclick = run(openScan));
document.querySelectorAll('[data-close]').forEach(button => button.onclick = () => $(button.dataset.close).close());
document.querySelectorAll('[data-kind]').forEach(button => button.onclick = () => {
  state.kind = button.dataset.kind; state.listPage = 1; state.coverPreview = null; document.querySelectorAll('[data-kind]').forEach(b => b.classList.toggle('selected', b === button)); renderVideos();
});
const filterChanged = () => { state.listPage = 1; renderVideos(); };
$('search').oninput = filterChanged; $('status-filter').onchange = filterChanged;
$('reset-filters').onclick = () => { $('search').value = ''; $('status-filter').value = 'all'; filterChanged(); };
$('select-results').onclick = () => { filtered().forEach(v => state.selected.add(v.id)); renderVideos(); };
$('clear-selection').onclick = () => { state.selected.clear(); renderVideos(); };
$('toggle-directory-nav').onclick = () => { state.navigationOpen = !state.navigationOpen; renderVideos(); };
$('collapse-series').onclick = () => { state.expandedSeries.clear(); renderVideos(); };
$('page-size').onchange = () => { state.pageSize = Number($('page-size').value); state.listPage = 1; renderVideos(); };
$('page-prev').onclick = () => { state.listPage--; renderVideos(); $('video-table').scrollIntoView({block:'start'}); };
$('page-next').onclick = () => { state.listPage++; renderVideos(); $('video-table').scrollIntoView({block:'start'}); };
$('library-browser').onclick = event => {
  const directory = event.target.closest('[data-directory]');
  if (directory) { navigateDirectory(directory.dataset.directory); return; }
  const select = event.target.closest('[data-select-directory]');
  if (select) { seriesMembers(select.dataset.selectDirectory).forEach(v => state.selected.add(v.id)); renderVideos(); return; }
  const toggle = event.target.closest('[data-series-toggle]');
  if (toggle) { const path = toggle.dataset.seriesToggle; state.expandedSeries.has(path) ? state.expandedSeries.delete(path) : state.expandedSeries.add(path); renderVideos(); return; }
  const cover = event.target.closest('[data-directory-cover]');
  if (cover) { state.directoryCover = state.directoryCover === cover.dataset.directoryCover ? null : cover.dataset.directoryCover; renderVideos(); }
};
$('directory-preview').onclick = run(async event => {
  const candidate = event.target.closest('[data-cover-candidate]');
  if (candidate) await saveInlineCover(candidate);
});
$('library-browser').onchange = event => {
  if (event.target.id === 'include-descendants') { state.includeDescendants = event.target.checked; state.listPage = 1; renderVideos(); return; }
  const path = event.target.dataset.seriesSelect;
  if (path != null) { seriesMembers(path).forEach(v => event.target.checked ? state.selected.add(v.id) : state.selected.delete(v.id)); renderVideos(); }
};
$('select-all').onchange = event => { filtered().forEach(v => event.target.checked ? state.selected.add(v.id) : state.selected.delete(v.id)); renderVideos(); };
$('video-list').onchange = event => {
  const id = event.target.dataset.select; if (!id) return;
  event.target.checked ? state.selected.add(id) : state.selected.delete(id);
  event.target.closest('tr').classList.toggle('selected-row', event.target.checked); renderSelection();
};
$('video-list').onclick = run(async event => {
  const candidate = event.target.closest('[data-cover-candidate]');
  if (candidate) { await saveInlineCover(candidate); return; }
  const preview = event.target.closest('[data-cover-preview]'), close = event.target.closest('[data-close-preview]');
  if (preview || close) {
    const button = preview || close, id = button.dataset.coverPreview || button.dataset.closePreview, scope = button.dataset.coverScope;
    const wasOpen = state.coverPreview?.id === id && state.coverPreview.scope === scope;
    state.coverPreview = close || wasOpen ? null : {id, scope};
    renderVideos();
    if (state.coverPreview) {
      $('cover-preview-' + id)?.scrollIntoView({block:'nearest', behavior:'smooth'});
      await loadInlineCoverCandidates(id, scope);
    }
    if (close) [...$('video-list').querySelectorAll('[data-cover-preview]')].find(b => b.dataset.coverPreview === id && b.dataset.coverScope === scope)?.focus({preventScroll:true});
    return;
  }
  const edit = event.target.closest('[data-edit]'); if (edit) editor(edit.dataset.edit);
});
$('library-browser').addEventListener('error', event => {
  const img = event.target;
  if (!img.matches?.('img[data-list-cover]')) return;
  img.hidden = true;
  const message = img.parentElement.querySelector('.cover-load-error'); if (message) message.hidden = false;
}, true);
$('folder-list').onclick = run(async event => { const button = event.target.closest('[data-folder]'); if (button) await browse(button.dataset.folder); });
$('folder-go').onclick = run(() => browse($('folder-path').value.trim()));
$('folder-path').onkeydown = run(async event => { if (event.key === 'Enter') { event.preventDefault(); await browse($('folder-path').value.trim()); } });
$('folder-up').onclick = run(() => browse(state.folder.split('/').slice(0, -1).join('/')));
$('scan-kind').onchange = () => $('series-help').hidden = $('scan-kind').value !== 'episode';
$('scan-form').onsubmit = run(async event => {
  event.preventDefault(); $('scan-submit').disabled = true; $('scan-feedback').textContent = '正在扫描…';
  try {
    const kind = $('scan-kind').value;
    const data = await api('/api/scan', {path: $('folder-path').value.trim(), kind, group_by_directory: kind === 'episode' && $('series-grouping').value === 'directory', recursive: $('recursive').checked});
    $('scan-dialog').close(); state.selected = new Set(data.ids); state.kind = kind; state.directoryPath = '.'; state.listPage = 1; $('search').value = ''; $('status-filter').value = 'all';
    document.querySelectorAll('[data-kind]').forEach(b => b.classList.toggle('selected', b.dataset.kind === kind));
    await refresh(); toast(`扫描完成，找到 ${data.count} 个视频。${data.reclassified ? `已调整 ${data.reclassified} 个视频的分类或所属系列。` : ''}${data.warnings.join('；')}`, !!data.warnings.length);
  } finally { $('scan-submit').disabled = false; $('scan-feedback').textContent = ''; }
});
$('open-cleanup').onclick = run(async () => {
  $('open-cleanup').disabled = true;
  try {
    const data = await api('/api/unavailable');
    if (!data.videos.length) { await refresh(); toast('没有需要清理的不可访问视频'); return; }
    cleanupIDs = data.videos.map(v => v.id);
    $('cleanup-dialog').querySelector('.dialog-message')?.remove();
    $('cleanup-summary').textContent = '发现 ' + data.total + ' 条不可访问记录，本次将清理 ' + cleanupIDs.length + ' 条。';
    $('cleanup-list').innerHTML = data.videos.map(v => '<li>' + escapeHtml(v.path) + '</li>').join('');
    $('cleanup-dialog').showModal();
  } finally { renderSelection(); }
});
$('cleanup-form').onsubmit = run(async event => {
  event.preventDefault(); $('cleanup-submit').disabled = true;
  try {
    const result = await api('/api/cleanup', {ids: cleanupIDs});
    cleanupIDs = []; $('cleanup-dialog').close(); await refresh();
    toast('已清理 ' + result.removed + ' 条记录。' + (result.skipped ? result.skipped + ' 条已恢复访问或已移除，本次跳过。' : ''));
  } finally { $('cleanup-submit').disabled = false; }
});
function openClearRecords(all) {
  const videos = all ? state.videos : state.videos.filter(v => state.selected.has(v.id));
  if (!videos.length || activeJob()) return;
  if (!all && videos.length > 5000) { toast('单次最多清除 5000 条所选记录；如需清空视频库，请使用「清空全部记录」。', true); return; }
  clearRecordsRequest = all ? {all: true} : {ids: videos.map(v => v.id)};
  $('clear-records-dialog').querySelector('.dialog-message')?.remove();
  $('clear-records-title').textContent = all ? '清空全部记录' : '清除所选记录';
  $('clear-records-summary').textContent = all
    ? `将清空整个视频库的 ${videos.length} 条记录，不受当前筛选和勾选影响。`
    : `将清除已勾选的 ${videos.length} 条记录，包含筛选后隐藏的已选项。`;
  $('clear-records-list').innerHTML = videos.slice(0, 100).map(v => '<li>' + escapeHtml(v.path) + '</li>').join('')
    + (videos.length > 100 ? `<li>另有 ${videos.length - 100} 条记录未展开</li>` : '');
  $('clear-records-submit').textContent = all ? '确认清空全部记录' : '确认清除所选记录';
  $('clear-records-dialog').showModal();
}
$('clear-selected').onclick = () => openClearRecords(false);
$('clear-all-records').onclick = () => openClearRecords(true);
$('clear-records-form').onsubmit = run(async event => {
  event.preventDefault();
  if (!clearRecordsRequest || $('clear-records-submit').disabled) return;
  $('clear-records-submit').disabled = true;
  try {
    const result = await api('/api/records/clear', clearRecordsRequest);
    clearRecordsRequest = null; $('clear-records-dialog').close(); await refresh();
    toast(`已清除 ${result.removed} 条记录。` + (result.skipped ? `${result.skipped} 条记录已不存在，本次跳过。` : ''));
  } finally { $('clear-records-submit').disabled = false; }
});
$('generate').onclick = () => { if (!state.settings.base_url || !state.settings.model) { page('settings'); toast('请先配置 AI 接口地址和模型名称'); } else batch('generate'); };
$('write').onclick = () => batch('save');
$('auto-covers').onclick = () => batch('covers');
$('batch-form').onsubmit = run(async event => {
  event.preventDefault(); $('batch-submit').disabled = true;
  try { const result = await api('/api/jobs', {action: state.batch, ids: [...state.selected], overwrite: $('overwrite').checked, auto_covers: state.batch === 'generate' && $('batch-auto-covers').checked}); $('batch-dialog').close(); await refresh(); toast(result.id ? '任务已启动，可在批量任务中查看各自进度' : '这些封面已在队列中，无需重复启动'); }
  finally { $('batch-submit').disabled = false; }
});
$('cancel-job').onclick = run(async () => { const job = activeJob(); if (job) { await api('/api/cancel', {id: job.id}); await refresh(); toast('将在当前文件或 AI 请求处理结束后停止'); } });
$('editor-form').onsubmit = run(async event => { event.preventDefault(); if (await saveDraft()) { $('editor-dialog').close(); toast('草稿已保存，还未写入视频目录'); } });
$('read-embedded').onclick = run(async () => {
  const row = state.editor; if (!row) return;
  $('read-embedded').disabled = true;
  try {
    const context = await api('/api/context?id=' + row.id);
    if (state.editor !== row || !$('editor-dialog').open) return;
    const tags = context.embedded || {};
    let filled = 0;
    if (tags.title && (!$('edit-title').value.trim() || (!row.metadata && $('edit-title').value === row.filename.replace(/\.[^.]+$/, '')))) { $('edit-title').value = tags.title; filled++; }
    const plot = tags.description || tags.synopsis || tags.comment;
    if (plot && !$('edit-plot').value.trim()) { $('edit-plot').value = plot; filled++; }
    if (tags.genre && !$('edit-genres').value.trim()) { $('edit-genres').value = tags.genre; filled++; }
    if (tags.director && !$('edit-directors').value.trim()) { $('edit-directors').value = tags.director; filled++; }
    $('source-preview').textContent = JSON.stringify(context, null, 2);
    $('editor-notes').textContent = (filled ? `已填入 ${filled} 项内嵌资料，请核对后保存草稿。` : '没有可补充的内嵌资料，已编辑字段保留。') + (context.warnings || []).join('；');
  } finally { $('read-embedded').disabled = false; }
});
$('preview-nfo').onclick = run(async () => {
  if (!await saveDraft()) return;
  const {files} = await api('/api/preview?id=' + state.editor.id);
  $('nfo-preview').innerHTML = files.map(f => `<p class="nfo-file">${escapeHtml(f.path)}</p><pre>${escapeHtml(f.xml)}</pre>`).join(''); $('nfo-preview').hidden = false;
});
$('source-preview').parentElement.ontoggle = run(async event => {
  if (!event.target.open || !state.editor) return;
  $('source-preview').textContent = '正在读取…';
  const context = await api('/api/context?id=' + state.editor.id); $('source-preview').textContent = JSON.stringify(context, null, 2);
});
$('settings-form').onsubmit = run(async event => { event.preventDefault(); if (await saveSettings()) toast('模型配置已保存'); });
$('test-model').onclick = run(async () => {
  if (!await saveSettings()) return;
  $('test-model').disabled = true; $('test-result').classList.remove('error'); $('test-result').textContent = '正在请求模型…';
  try { await api('/api/test-ai', {}); $('test-result').textContent = '连接成功，已收到有效元数据'; }
  catch (err) { $('test-result').textContent = err.message; $('test-result').classList.add('error'); }
  finally { $('test-model').disabled = false; }
});
$('login-form').onsubmit = async event => {
  event.preventDefault();
  try { await api('/api/login', {password: $('password').value}); $('password').value = ''; $('login-error').textContent = ''; await initialize(); }
  catch (err) { $('login-error').textContent = err.message; }
};
$('logout').onclick = run(async () => { await api('/api/logout', {}); await initialize(); });
async function initialize() {
  const session = await api('/api/session');
  $('login').hidden = session.authenticated; $('workspace').hidden = !session.authenticated; $('logout').hidden = !session.password_required;
  if (session.authenticated) await refresh();
}
setInterval(() => { if (activeJob() && !$('workspace').hidden) refresh().catch(err => toast(err.message, true)); }, 2200);
initialize().catch(err => toast(err.message, true));

async function loadCoverTasks() {
  const items = await api('/api/cover-tasks?id=' + encodeURIComponent(coverDetailID));
  const labels = {queued:'排队中', running:'获取中', saved:'已保存', skipped:'已保留', failed:'失败', cancelled:'已取消', interrupted:'已中断'};
  $('cover-task-list').innerHTML = items.map(t => '<li><strong>' + escapeHtml(t.scope === 'series' ? '系列封面' : '视频封面') + ' · ' + escapeHtml(labels[t.status] || t.status) + '</strong><br>' + escapeHtml(t.path) + (t.error ? '<br>' + escapeHtml(t.error) : '') + '</li>').join('');
}
$('jobs-list').onclick = run(async event => {
  const cancel = event.target.closest('[data-cancel-job]'), retry = event.target.closest('[data-retry-covers]'), detail = event.target.closest('[data-cover-detail]'), retryJob = event.target.closest('[data-retry-job]');
  if (cancel) { await api('/api/cancel', {id: cancel.dataset.cancelJob}); await refresh(); }
  if (retry) { retry.disabled = true; try { const result = await api('/api/cover-jobs/retry', {id: retry.dataset.retryCovers}); await refresh(); toast(result.id ? '未完成封面已加入重试队列' : '没有需要重试的封面，或已在队列中'); } finally { retry.disabled = false; } }
  if (retryJob) { retryJob.disabled = true; try { const result = await api('/api/jobs/retry', {id: retryJob.dataset.retryJob}); await refresh(); toast(result.id ? '失败或未完成项已加入重试队列' : '没有可重试项：可能已重试、记录已删除，或旧任务没有逐项记录'); } finally { retryJob.disabled = false; } }
  if (detail) { coverDetailID = detail.dataset.coverDetail; await loadCoverTasks(); $('cover-tasks-dialog').showModal(); }
});
