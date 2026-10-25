'use strict';

function seriesTree(videos) {
  const root = {path: '.', name: '视频根目录', children: new Map(), direct: [], members: []};
  for (const video of videos) {
    if (video.kind !== 'episode') continue;
    const parts = video.path.split('/').slice(0, -1);
    let node = root;
    node.members.push(video);
    for (const part of parts) {
      const path = node.path === '.' ? part : node.path + '/' + part;
      if (!node.children.has(part)) node.children.set(part, {path, name: part, children: new Map(), direct: [], members: []});
      node = node.children.get(part);
      node.members.push(video);
    }
    node.direct.push(video);
  }
  return root;
}

function seriesMembers(path) {
  return state.videos.filter(v => v.kind === 'episode' && (path === '.' || v.path.startsWith(path + '/')));
}

function episodeOrder(a, b) {
  return (a.season ?? Infinity) - (b.season ?? Infinity) || (a.episode ?? Infinity) - (b.episode ?? Infinity) || a.path.localeCompare(b.path, 'zh-CN', {numeric:true});
}

function directoryNode(root, path) {
  if (!path || path === '.') return root;
  let node = root;
  for (const part of path.split('/')) { node = node.children.get(part); if (!node) return null; }
  return node;
}

function matchesVideo(v, search, filter) {
  return (filter === 'all' || (filter === 'existing' ? v.has_nfo : filter === 'unavailable' ? !v.exists : v.status === filter))
    && (!search || `${v.path} ${v.metadata?.title || ''} ${v.show?.title || ''}`.toLocaleLowerCase().includes(search));
}

function inDirectory(video, path, descendants) {
  const parent = video.path.includes('/') ? video.path.slice(0, video.path.lastIndexOf('/')) : '.';
  return descendants ? path === '.' || video.path.startsWith(path + '/') : parent === path;
}

function libraryPage(videos, page, pageSize) {
  const pages = Math.max(1, Math.ceil(videos.length / pageSize));
  page = Math.max(1, Math.min(page, pages));
  return {page, pages, start: (page - 1) * pageSize, videos: videos.slice((page - 1) * pageSize, page * pageSize)};
}

function navigateDirectory(path) {
  const root = seriesTree(state.videos);
  if (!directoryNode(root, path)) return;
  state.directoryPath = path; state.navigationOpen = false; state.listPage = 1; state.coverPreview = null; state.directoryCover = null;
  let parent = '';
  for (const part of path === '.' ? [] : path.split('/')) {
    parent = parent ? parent + '/' + part : part; state.expandedSeries.add(parent);
  }
  renderVideos();
  $('directory-title')?.focus({preventScroll:true});
}

function directoryPosterVideo(node) { return node.members.find(v => v.show_path === node.path); }
function directoryPoster(node) { return directoryPosterVideo(node)?.covers.find(c => c.scope === 'series'); }

function renderDirectoryBrowser() {
  const enabled = state.kind === 'episode';
  $('library-browser').classList.toggle('has-directory-nav', enabled);
  $('library-browser').classList.toggle('nav-open', state.navigationOpen);
  $('toggle-directory-nav').setAttribute('aria-expanded', String(state.navigationOpen));
  $('toggle-directory-nav').textContent = state.navigationOpen ? '收起目录导航 ▴' : '浏览目录 ▾';
  ['series-sidebar', 'directory-header', 'directory-cards'].forEach(id => $(id).hidden = !enabled);
  $('directory-preview').hidden = true;
  if (!enabled) return;
  const root = seriesTree(state.videos);
  let current = directoryNode(root, state.directoryPath);
  if (!current) { state.directoryPath = '.'; current = root; state.listPage = 1; }
  const children = node => [...node.children.values()].sort((a, b) => a.path.localeCompare(b.path, 'zh-CN', {numeric:true}));
  const scroll = $('directory-tree').scrollTop;
  const treeRow = node => {
    const expanded = state.expandedSeries.has(node.path), active = node.path === state.directoryPath;
    return `<li><div class="directory-nav-row${active ? ' active' : ''}">${node.children.size ? `<button type="button" class="tree-disclosure" data-series-toggle="${escapeHtml(node.path)}" aria-expanded="${expanded}" aria-label="${expanded ? '收起' : '展开'}子目录：${escapeHtml(node.path)}">${expanded ? '▾' : '▸'}</button>` : '<span class="tree-spacer"></span>'}<button type="button" class="directory-link" data-directory="${escapeHtml(node.path)}" ${active ? 'aria-current="location"' : ''} title="${escapeHtml(node.path)}"><span>${escapeHtml(node.name)}</span><small>${node.members.length}</small></button></div>${expanded && node.children.size ? `<ul>${children(node).map(treeRow).join('')}</ul>` : ''}</li>`;
  };
  $('directory-tree').innerHTML = `<button type="button" class="directory-root${state.directoryPath === '.' ? ' active' : ''}" data-directory="." ${state.directoryPath === '.' ? 'aria-current="location"' : ''}>全部剧集 <small>${root.members.length}</small></button><ul>${children(root).map(treeRow).join('')}</ul>`;
  $('directory-tree').scrollTop = scroll;
  const parts = state.directoryPath === '.' ? [] : state.directoryPath.split('/');
  const crumbs = [{path:'.', name:'全部剧集'}, ...parts.map((name, i) => ({name, path:parts.slice(0,i+1).join('/')}))];
  const search = $('search').value.trim().toLocaleLowerCase(), filter = $('status-filter').value, filtering = !!search || filter !== 'all';
  const posterVideo = directoryPosterVideo(current), poster = posterVideo?.covers.find(c => c.scope === 'series'), parentPath = parts.length > 1 ? parts.slice(0,-1).join('/') : '.';
  const posterButton = poster
    ? `<button type="button" class="directory-poster" data-directory-cover="${escapeHtml(current.path)}" aria-label="查看当前剧集海报和候选"><img data-list-cover src="${escapeHtml(poster.thumbnail_url)}" alt="剧集海报"><span class="cover-load-error" hidden>图片不可用</span></button>`
    : posterVideo
      ? `<button type="button" class="directory-poster directory-poster-missing" data-directory-cover="${escapeHtml(current.path)}" aria-label="加载当前剧集海报候选"><span class="no-cover">暂无剧集海报</span></button>`
      : '<span class="directory-symbol" aria-hidden="true">▱</span>';
  const breadcrumbs = crumbs.map((c, i) => {
    const separator = i ? '<span aria-hidden="true">/</span>' : '';
    const item = i === crumbs.length - 1
      ? `<span aria-current="location">${escapeHtml(c.name)}</span>`
      : `<button type="button" data-directory="${escapeHtml(c.path)}">${escapeHtml(c.name)}</button>`;
    return separator + item;
  }).join('');
  const parentButton = current.path !== '.' ? `<button type="button" class="secondary small" data-directory="${escapeHtml(parentPath)}">↑ 上一级</button>` : '';
  const coverAction = posterVideo ? `<button type="button" class="secondary small" data-directory-cover-action="${escapeHtml(posterVideo.id)}">识别剧集海报</button>` : '';
  $('directory-header').innerHTML = `<nav class="directory-breadcrumbs" aria-label="当前位置">${breadcrumbs}</nav><div class="directory-detail">${posterButton}<div class="directory-description"><h2 id="directory-title" tabindex="-1">${escapeHtml(current.path === '.' ? '全部剧集' : current.name)}</h2><p>${current.children.size} 个子目录 · 本层 ${current.direct.length} 集 · 共 ${current.members.length} 集</p></div>${parentButton}</div><div class="directory-options"><label class="checkbox-label"><input id="include-descendants" type="checkbox" ${state.includeDescendants || filtering ? 'checked' : ''} ${filtering ? 'disabled' : ''}>${filtering ? '筛选范围：当前目录及子目录' : '同时显示子目录的视频'}</label><div class="directory-options-actions">${coverAction}<button type="button" class="text-button" data-select-directory="${escapeHtml(current.path)}" ${!current.members.length ? 'disabled' : ''}>选择整个目录（${current.members.length} 集）</button></div></div>`;
  const childNodes = children(current).filter(node => !filtering || node.members.some(v => matchesVideo(v,search,filter)));
  $('directory-cards').hidden = !childNodes.length;
  $('directory-cards').innerHTML = childNodes.map(node => {
    const selected = node.members.filter(v => state.selected.has(v.id)).length;
    const saved = node.members.filter(v => v.status === 'saved').length;
    const failed = node.members.filter(v => v.status === 'error').length;
    const drafts = node.members.filter(v => v.status === 'draft').length;
    return `<article class="directory-card${selected ? ' selected' : ''}"><input type="checkbox" data-series-select="${escapeHtml(node.path)}" aria-label="选择目录及子目录：${escapeHtml(node.path)}（${node.members.length} 集）" ${selected === node.members.length ? 'checked' : ''}><button type="button" class="directory-card-open" data-directory="${escapeHtml(node.path)}" aria-label="打开目录：${escapeHtml(node.path)}"><span class="folder-symbol" aria-hidden="true">▱</span><strong>${escapeHtml(node.name)}</strong><span>${node.members.length} 集 · ${node.children.size} 个子目录</span><small>${failed ? `${failed} 集失败` : saved ? `${saved} / ${node.members.length} 集已保存` : drafts ? `${drafts} 集待确认` : '等待整理'} <span aria-hidden="true">→</span></small></button></article>`;
  }).join('');
  if (posterVideo && state.directoryCover === current.path) {
    $('directory-preview').hidden = false;
    const cached = state.coverCandidates[posterVideo.id + ':series'];
    const candidates = Array.isArray(cached) ? inlineCoverCandidatesMarkup(cached, posterVideo.id, 'series') : '';
    const stage = poster ? `<div class="list-cover-stage"><img data-list-cover src="${escapeHtml(poster.url)}" alt="${escapeHtml(current.name)}的剧集海报大图"><p class="cover-load-error" hidden>海报无法加载，请检查文件是否仍可访问。</p></div>` : '<div class="list-cover-stage"><p class="no-cover">暂无剧集海报</p></div>';
    $('directory-preview').innerHTML = `<section class="list-cover-preview" aria-label="目录海报大图"><section class="inline-cover-candidates" aria-label="其他候选海报" id="cover-candidates-${posterVideo.id}-series">${candidates}</section>${stage}</section>`;
    loadInlineCoverCandidates(posterVideo.id, 'series');
  }
}

function updateSeriesSelection() {
  document.querySelectorAll('[data-series-select]').forEach(input => {
    const members = seriesMembers(input.dataset.seriesSelect), count = members.filter(v => state.selected.has(v.id)).length;
    input.checked = members.length > 0 && count === members.length;
    input.indeterminate = count > 0 && count < members.length;
    input.closest('.directory-card')?.classList.toggle('selected', count > 0);
  });
}
