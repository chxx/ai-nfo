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

function renderSeriesRows(videos) {
  const root = seriesTree(state.videos), matchingIDs = new Set(videos.map(v => v.id));
  const children = node => [...node.children.values()].sort((a, b) => a.path.localeCompare(b.path, 'zh-CN', {numeric: true}));
  let directoryCount = 0;
  const count = node => { if (!node.members.some(v => matchingIDs.has(v.id))) return; directoryCount++; children(node).forEach(count); };
  const roots = root.direct.length ? [root] : children(root);
  roots.forEach(count);
  $('result-count').textContent = `${directoryCount} 个目录 · ${videos.length} 集`;
  const render = (node, depth) => {
    const matching = node.members.filter(v => matchingIDs.has(v.id));
    if (!matching.length) return '';
    const {path, name, members} = node, open = state.expandedSeries.has(path);
    // Only display a directory's own series cover, not a descendant's poster.
    const owner = members.find(v => v.show_path === path && v.covers?.some(c => c.scope === 'series'));
    const cover = owner?.covers.find(c => c.scope === 'series');
    const selected = members.filter(v => state.selected.has(v.id)).length;
    const counts = Object.entries(statusNames).map(([status, label]) => {
      const count = members.filter(v => v.status === status).length;
      return count ? `${label} ${count}` : '';
    }).filter(Boolean).join(' · ');
    const image = cover ? `<button type="button" class="list-cover-thumb" data-directory-cover="${escapeHtml(path)}" aria-label="查看目录封面：${escapeHtml(path)}"><img data-list-cover src="${escapeHtml(cover.thumbnail_url)}" alt="${escapeHtml(name)}的系列封面" loading="lazy" decoding="async" width="88" height="58"><span class="cover-load-error" hidden>图片不可用</span></button>` : '<span class="no-cover">暂无系列封面</span>';
    const summary = `<tr class="series-summary${selected ? ' selected-row' : ''}"><td class="check"><input type="checkbox" data-series-select="${escapeHtml(path)}" aria-label="选择目录及子目录：${escapeHtml(path)}（${members.length} 集）" ${selected === members.length ? 'checked' : ''}></td><td class="list-cover-cell">${image}</td><td><div class="series-info"><strong><span class="directory-depth" aria-hidden="true">${'│　'.repeat(Math.min(depth, 8))}</span><button type="button" class="text-button directory-toggle" data-series-toggle="${escapeHtml(path)}" aria-expanded="${open}" aria-label="${open ? '收起' : '展开'}目录：${escapeHtml(path)}">${open ? '▾' : '▸'} ${escapeHtml(name)}</button></strong><small>${escapeHtml(path)}</small><span>共 ${members.length} 集 · 本层 ${node.direct.length} 集 · ${node.children.size} 个子目录${matching.length !== members.length ? ` · 筛选命中 ${matching.length} 集` : ''}</span></div></td><td><span class="series-status">${escapeHtml(counts)}</span></td><td>${size(members.reduce((sum, v) => sum + v.size, 0))}</td><td><button type="button" class="text-button" data-series-toggle="${escapeHtml(path)}" aria-expanded="${open}">${open ? '收起目录' : '展开目录'}</button><span class="existing-note">勾选包含所有下级视频</span></td></tr>`;
    let preview = '';
    if (cover && state.directoryCover === path) {
      preview = `<tr class="cover-preview-row"><td colspan="6"><section class="list-cover-preview" aria-label="目录封面大图"><header><div><h3>${escapeHtml(name)} · 系列封面</h3><p>${escapeHtml(cover.path)}</p></div><button type="button" class="secondary small" data-directory-cover="${escapeHtml(path)}">收起目录封面</button></header><div class="list-cover-stage"><img data-list-cover src="${escapeHtml(cover.url)}" alt="${escapeHtml(path)}的系列封面大图"><p class="cover-load-error" hidden>封面无法加载，请检查文件是否仍可访问。</p></div></section></td></tr>`;
    }
    return summary + preview + (open ? node.direct.filter(v => matchingIDs.has(v.id)).sort(episodeOrder).map(videoRow).join('') + children(node).map(child => render(child, depth + 1)).join('') : '');
  };
  return roots.map(node => render(node, 0)).join('');
}

function updateSeriesSelection() {
  document.querySelectorAll('[data-series-select]').forEach(input => {
    const members = seriesMembers(input.dataset.seriesSelect), count = members.filter(v => state.selected.has(v.id)).length;
    input.checked = members.length > 0 && count === members.length;
    input.indeterminate = count > 0 && count < members.length;
    input.closest('tr').classList.toggle('selected-row', count > 0);
  });
}
