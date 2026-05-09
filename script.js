let currentPath = decodeURIComponent(window.location.hash.substring(1) || '');
let currentView = localStorage.getItem('galleryView') === 'gallery' ? 'gallery' : 'list';

function setView(view) {
    currentView = view;
    localStorage.setItem('galleryView', view);
    document.getElementById('listViewBtn').classList.toggle('active', view === 'list');
    document.getElementById('galleryViewBtn').classList.toggle('active', view === 'gallery');
    const container = document.getElementById('fileList');
    container.classList.remove('list-view', 'gallery-view');
    container.classList.add(view === 'list' ? 'list-view' : 'gallery-view');
    // Re-render current contents if already loaded
    if (window.lastContents) renderFileList(window.lastContents, window.lastCurrentDir);
}

async function fetchDirectory(path) {
    try {
        const params = new URLSearchParams({ dir: path });
        const resp = await fetch(`/api/list?${params}`);
        const data = await resp.json();
        if (data.error) throw new Error(data.error);
        window.lastContents = data.contents;
        window.lastCurrentDir = data.currentDir;
        renderFileList(data.contents, data.currentDir);
        renderBreadcrumb(data.currentDir, data.basePathForDisplay);
        window.location.hash = encodeURIComponent(path);
        currentPath = path;
    } catch (err) {
        showError(err.message);
    }
}

function renderFileList(contents, currentDir) {
    const container = document.getElementById('fileList');
    if (!contents.length) {
        container.innerHTML = '<div class="empty">📭 This folder is empty</div>';
        return;
    }

    let html = '';
    if (currentDir !== '') {
        const parentPath = currentDir.substring(0, currentDir.lastIndexOf('/'));
        if (currentView === 'list') {
            html += `<div class="item"><div class="item-icon">📁</div><div class="item-name"><a href="#" class="folder-link" data-path="${escapeAttr(parentPath)}">..</a></div><div class="item-size">—</div></div>`;
        } else {
            html += `<div class="item"><div class="item-icon">📁</div><div class="item-name"><a href="#" class="folder-link" data-path="${escapeAttr(parentPath)}">..</a></div></div>`;
        }
    }

    for (const item of contents) {
        if (item.type === 'dir') {
            if (currentView === 'list') {
                html += `<div class="item"><div class="item-icon">📁</div><div class="item-name"><a href="#" class="folder-link" data-path="${escapeAttr(item.path)}">${escapeHtml(item.name)}</a></div><div class="item-size">—</div></div>`;
            } else {
                html += `<div class="item"><div class="item-icon">📁</div><div class="item-name"><a href="#" class="folder-link" data-path="${escapeAttr(item.path)}">${escapeHtml(item.name)}</a></div></div>`;
            }
        } else {
            const downloadUrl = `/api/download?file=${encodeURIComponent(item.path)}`;
            if (currentView === 'list') {
                html += `<div class="item"><div class="item-icon">${item.isImage ? '🖼️' : '📄'}</div><div class="item-name"><a href="${downloadUrl}" download>${escapeHtml(item.name)}</a> ${item.size ? `<small>(${item.size})</small>` : ''}</div><div class="item-size"></div></div>`;
            } else {
                // Gallery view: show thumbnail for images, generic icon for others
                if (item.isImage) {
                    const thumbUrl = `/api/thumbnail?file=${encodeURIComponent(item.path)}`;
                    html += `<div class="item"><img class="thumbnail" src="${thumbUrl}" loading="lazy" alt="${escapeHtml(item.name)}"><div class="item-name"><a href="${downloadUrl}" download>${escapeHtml(item.name)}</a></div><div class="item-size">${item.size || ''}</div></div>`;
                } else {
                    html += `<div class="item"><div class="item-icon">📄</div><div class="item-name"><a href="${downloadUrl}" download>${escapeHtml(item.name)}</a></div><div class="item-size">${item.size || ''}</div></div>`;
                }
            }
        }
    }
    container.innerHTML = html;

    // Attach folder click handlers
    document.querySelectorAll('.folder-link').forEach(link => {
        link.addEventListener('click', (e) => {
            e.preventDefault();
            fetchDirectory(link.getAttribute('data-path'));
        });
    });
}

function renderBreadcrumb(currentRelative, baseDisplay) {
    const breadDiv = document.getElementById('breadcrumb');
    if (!currentRelative) {
        breadDiv.innerHTML = `<span>📁 ${escapeHtml(baseDisplay)}</span>`;
        return;
    }
    const parts = currentRelative.split('/').filter(p => p);
    let html = `<a href="#" data-bread-path="">${escapeHtml(baseDisplay)}</a>`;
    let accumulated = '';
    for (let i = 0; i < parts.length; i++) {
        accumulated += (accumulated ? '/' : '') + parts[i];
        if (i === parts.length - 1) {
            html += ` / <span>${escapeHtml(parts[i])}</span>`;
        } else {
            html += ` / <a href="#" data-bread-path="${escapeAttr(accumulated)}">${escapeHtml(parts[i])}</a>`;
        }
    }
    breadDiv.innerHTML = html;
    breadDiv.querySelectorAll('a[data-bread-path]').forEach(link => {
        link.addEventListener('click', (e) => {
            e.preventDefault();
            fetchDirectory(link.getAttribute('data-bread-path'));
        });
    });
}

function showError(msg) {
    document.getElementById('fileList').innerHTML = `<div class="error">⚠️ ${escapeHtml(msg)}</div>`;
}

function escapeHtml(str) {
    return str.replace(/[&<>]/g, m => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' }[m]));
}

function escapeAttr(str) {
    return str.replace(/&/g, '&amp;').replace(/"/g, '&quot;');
}

// Event listeners
document.getElementById('listViewBtn').addEventListener('click', () => setView('list'));
document.getElementById('galleryViewBtn').addEventListener('click', () => setView('gallery'));

window.addEventListener('hashchange', () => {
    const newPath = decodeURIComponent(window.location.hash.substring(1) || '');
    if (newPath !== currentPath) fetchDirectory(newPath);
});

// Initial load
setView(currentView);
fetchDirectory(currentPath);