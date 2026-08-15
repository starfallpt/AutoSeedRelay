/* ============================================================
   AutoSeedRelay v3 Web 管理面板 — 前端交互逻辑
   无外部 CDN 依赖，纯原生 JS
   ============================================================ */

(function () {
    'use strict';

    // ==========================================================
    // 全局状态
    // ==========================================================
    const App = {
        refreshTimer: null,
        logTimer: null,
        currentPage: 'dashboard',
        setupComplete: false,
    };

    // ==========================================================
    // 工具函数
    // ==========================================================

    /** 选择单个元素 */
    function $(selector, parent) {
        return (parent || document).querySelector(selector);
    }

    /** 选择多个元素 */
    function $$(selector, parent) {
        return Array.from((parent || document).querySelectorAll(selector));
    }

    /** 格式化字节大小 */
    function formatSize(bytes) {
        if (bytes == null || bytes === 0) return '0 B';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = Math.floor(Math.log(bytes) / Math.log(1024));
        i = Math.min(i, units.length - 1);
        return (bytes / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1) + ' ' + units[i];
    }

    /** 格式化时间 */
    function formatTime(ts) {
        if (!ts) return '-';
        var d = new Date(ts);
        var pad = function (n) { return n < 10 ? '0' + n : '' + n; };
        return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
    }

    /** 相对时间 */
    function relativeTime(ts) {
        if (!ts) return '-';
        var now = Date.now();
        var diff = now - new Date(ts).getTime();
        var sec = Math.floor(diff / 1000);
        if (sec < 60) return sec + '秒前';
        var min = Math.floor(sec / 60);
        if (min < 60) return min + '分钟前';
        var hr = Math.floor(min / 60);
        if (hr < 24) return hr + '小时前';
        return Math.floor(hr / 24) + '天前';
    }

    /** 状态文字映射 */
    function statusLabel(s) {
        var map = {
            pending: '待处理',
            downloaded: '已下载',
            added_to_qb: '已添加qB',
            seeded: '做种中',
            uploaded: '已发布',
            cross_seeded: '已辅种',
            skipped_existing: '跳过(已存在)',
            failed: '失败',
            skipped: '已跳过',
        };
        return map[s] || s;
    }

    /** 状态样式类 */
    function statusBadgeClass(s) {
        switch (s) {
            case 'uploaded':
            case 'cross_seeded':
            case 'seeded':
                return 'badge-success';
            case 'pending':
            case 'added_to_qb':
            case 'downloaded':
                return 'badge-info';
            case 'failed':
                return 'badge-danger';
            case 'skipped':
            case 'skipped_existing':
                return 'badge-warning';
            default:
                return 'badge-muted';
        }
    }

    /** HTTP 请求封装 */
    function api(method, url, data) {
        var opts = {
            method: method,
            headers: { 'Content-Type': 'application/json' },
        };
        if (data) {
            opts.body = JSON.stringify(data);
        }
        return fetch(url, opts).then(function (r) {
            if (r.status === 401) {
                // Session expired or not logged in — redirect to login.
                window.location.href = '/login';
                throw new Error('unauthorized');
            }
            if (!r.ok) {
                return r.json().then(function (e) {
                    throw new Error(e.error || '请求失败 (' + r.status + ')');
                }).catch(function (errInner) {
                    // If JSON parse failed, just throw the status error.
                    if (errInner.message && errInner.message.indexOf('unauthorized') === 0) throw errInner;
                    throw new Error('请求失败 (' + r.status + ')');
                });
            }
            return r.json();
        });
    }

    /** 显示 Toast */
    function showToast(msg, type) {
        type = type || 'info';
        var container = $('#toast-container');
        if (!container) {
            container = document.createElement('div');
            container.id = 'toast-container';
            container.className = 'toast-container';
            document.body.appendChild(container);
        }
        var el = document.createElement('div');
        el.className = 'toast toast-' + type;
        el.textContent = msg;
        container.appendChild(el);
        setTimeout(function () {
            el.style.opacity = '0';
            el.style.transition = 'opacity 0.3s';
            setTimeout(function () { el.remove(); }, 300);
        }, 3000);
    }

    // ==========================================================
    // 导航
    // ==========================================================

    /** 切换页面（无刷新 SPA） */
    function navigateTo(page) {
        App.currentPage = page;
        // 更新侧边栏激活状态
        $$('.sidebar-nav a').forEach(function (a) {
            a.classList.toggle('active', a.dataset.page === page);
        });
        // 加载页面内容
        loadPage(page);
    }

    /** 加载页面内容 */
    function loadPage(page) {
        var content = $('#main-content');
        if (!content) return;

        // 显示加载状态
        content.innerHTML = '<div class="empty-state"><div class="spinner"></div><div class="empty-text mt-1">加载中...</div></div>';

        // 根据页面类型渲染
        switch (page) {
            case 'dashboard':
                renderDashboard(content);
                break;
            case 'seeds':
                renderSeeds(content);
                break;
            case 'config':
                renderConfig(content);
                break;
            case 'logs':
                renderLogs(content);
                break;
            case 'setup':
                renderSetup(content);
                break;
            default:
                content.innerHTML = '<div class="empty-state"><div class="empty-icon">?</div><div class="empty-text">页面未找到</div></div>';
        }
    }

    // ==========================================================
    // 仪表盘
    // ==========================================================

    var dashboardTimer = null;

    function renderDashboard(container) {
        container.innerHTML = getDashboardHTML();
        refreshDashboard();
        // 每 30 秒自动刷新
        if (dashboardTimer) clearInterval(dashboardTimer);
        dashboardTimer = setInterval(refreshDashboard, 30000);
    }

    function getDashboardHTML() {
        return '<div class="stats-row" id="stats-row">' +
            '<div class="stat-card"><div class="stat-value green" id="stat-total-publish">-</div><div class="stat-label">累计发布</div></div>' +
            '<div class="stat-card"><div class="stat-value blue" id="stat-total-cross">-</div><div class="stat-label">累计辅种</div></div>' +
            '<div class="stat-card"><div class="stat-value blue" id="stat-seeding">-</div><div class="stat-label">当前做种</div></div>' +
            '<div class="stat-card"><div class="stat-value green" id="stat-disk">-</div><div class="stat-label">磁盘剩余</div></div>' +
            '<div class="stat-card"><div class="stat-value green" id="stat-today-publish">-</div><div class="stat-label">今日发布</div></div>' +
            '<div class="stat-card"><div class="stat-value blue" id="stat-today-cross">-</div><div class="stat-label">今日辅种</div></div>' +
            '</div>' +
            '<div class="card">' +
            '<div class="card-header"><span class="card-title">异常队列</span></div>' +
            '<div id="alert-queue"><div class="empty-state"><div class="empty-text">暂无异常</div></div></div>' +
            '</div>' +
            '<div class="card">' +
            '<div class="card-header"><span class="card-title">最近活动</span><span class="text-sm text-muted">每 30s 刷新</span></div>' +
            '<div class="table-container">' +
            '<table><thead><tr><th>时间</th><th>标题</th><th>目标站</th><th>动作</th><th>结果</th></tr></thead>' +
            '<tbody id="recent-activity"></tbody></table>' +
            '</div></div>';
    }

    function refreshDashboard() {
        api('GET', '/api/status').then(function (data) {
            // 统计卡片
            setStat('stat-total-publish', data.total_published || 0);
            setStat('stat-total-cross', data.total_cross_seeded || 0);
            setStat('stat-seeding', data.current_seeding || 0);
            setStat('stat-disk', data.disk_free_gb ? data.disk_free_gb.toFixed(0) + ' GB' : '-');
            setStat('stat-today-publish', data.today_published || 0);
            setStat('stat-today-cross', data.today_cross_seeded || 0);

            // 异常队列
            var alertEl = $('#alert-queue');
            alertEl.innerHTML = '<div class="empty-state"><div class="empty-text">暂无异常</div></div>';

            // 最近活动
            var tbody = $('#recent-activity');
            tbody.innerHTML = '<tr><td colspan="5" class="text-center text-muted">暂无活动</td></tr>';
        }).catch(function (err) {
            console.error('仪表盘刷新失败:', err);
        });
    }

    function setStat(id, value) {
        var el = $(id);
        if (el) el.textContent = value;
    }

    // ==========================================================
    // 种子列表
    // ==========================================================

    var seedsPage = 1;
    var seedsFilters = { status: '', target_site: '', search: '' };

    function renderSeeds(container) {
        container.innerHTML = getSeedsHTML();
        loadSeeds();
        // 绑定筛选事件
        $('#seed-status-filter').addEventListener('change', function () {
            seedsFilters.status = this.value;
            seedsPage = 1;
            loadSeeds();
        });
        $('#seed-target-filter').addEventListener('change', function () {
            seedsFilters.target_site = this.value;
            seedsPage = 1;
            loadSeeds();
        });
        $('#seed-search').addEventListener('input', debounce(function () {
            seedsFilters.search = this.value;
            seedsPage = 1;
            loadSeeds();
        }, 300));
    }

    function getSeedsHTML() {
        return '<div class="search-bar">' +
            '<select class="form-select" id="seed-status-filter" style="min-width:130px">' +
            '<option value="">全部状态</option>' +
            '<option value="pending">待处理</option>' +
            '<option value="downloaded">已下载</option>' +
            '<option value="added_to_qb">已添加qB</option>' +
            '<option value="seeded">做种中</option>' +
            '<option value="uploaded">已发布</option>' +
            '<option value="cross_seeded">已辅种</option>' +
            '<option value="failed">失败</option>' +
            '<option value="skipped">已跳过</option>' +
            '<option value="skipped_existing">跳过(已存在)</option>' +
            '</select>' +
            '<select class="form-select" id="seed-target-filter" style="min-width:140px">' +
            '<option value="">全部目标站</option>' +
            '</select>' +
            '<input type="text" class="form-input" id="seed-search" placeholder="搜索标题..." style="max-width:260px">' +
            '</div>' +
            '<div class="table-container">' +
            '<table><thead><tr>' +
            '<th>状态</th><th>源站</th><th>标题</th><th>大小</th><th>促销</th><th>目标站</th><th>做种数</th><th>操作</th>' +
            '</tr></thead>' +
            '<tbody id="seeds-tbody"></tbody></table>' +
            '</div>' +
            '<div class="pagination" id="seeds-pagination"></div>';
    }

    function loadSeeds() {
        var params = new URLSearchParams();
        params.set('page', seedsPage);
        params.set('limit', '20');
        if (seedsFilters.status) params.set('status', seedsFilters.status);
        if (seedsFilters.target_site) params.set('target', seedsFilters.target_site);
        if (seedsFilters.search) params.set('q', seedsFilters.search);

        api('GET', '/api/seeds?' + params.toString()).then(function (data) {
            var seeds = data.seeds || [];
            var total = data.total || 0;
            var tbody = $('#seeds-tbody');
            if (seeds.length === 0) {
                tbody.innerHTML = '<tr><td colspan="8" class="text-center text-muted">暂无种子</td></tr>';
            } else {
                tbody.innerHTML = seeds.map(function (s) {
                    return '<tr>' +
                        '<td><span class="badge ' + statusBadgeClass(s.status) + '">' + statusLabel(s.status) + '</span></td>' +
                        '<td>' + escapeHtml(s.source_site || '-') + '</td>' +
                        '<td class="truncate" title="' + escapeHtml(s.title || '') + '">' + escapeHtml(s.title || '-') + '</td>' +
                        '<td>' + formatSize(s.source_size) + '</td>' +
                        '<td>' + escapeHtml(s.discount || '-') + '</td>' +
                        '<td>' + escapeHtml(s.target_site || '-') + '</td>' +
                        '<td>' + (s.seeders != null ? s.seeders : '-') + '</td>' +
                        '<td>' +
                        '<button class="btn btn-outline btn-xs" onclick="App.retireSeed(' + s.id + ')">撤种</button> ' +
                        '<button class="btn btn-outline btn-xs" onclick="App.retrySeed(' + s.id + ')">重试</button> ' +
                        '<button class="btn btn-outline btn-xs" onclick="App.viewSeedDetail(' + s.id + ')">详情</button>' +
                        '</td>' +
                        '</tr>';
                }).join('');
            }
            // 分页
            renderPagination('seeds-pagination', seedsPage, Math.ceil(total / 20), function (p) {
                seedsPage = p;
                loadSeeds();
            });
            // 填充目标站下拉
            var targetFilter = $('#seed-target-filter');
            if (targetFilter && targetFilter.options.length <= 1 && data.target_sites) {
                data.target_sites.forEach(function (ts) {
                    var opt = document.createElement('option');
                    opt.value = ts;
                    opt.textContent = ts;
                    targetFilter.appendChild(opt);
                });
            }
        }).catch(function (err) {
            showToast('加载种子列表失败: ' + err.message, 'error');
        });
    }

    function renderPagination(containerId, current, totalPages, onPage) {
        var el = $('#' + containerId);
        if (!el || totalPages <= 1) {
            if (el) el.innerHTML = '';
            return;
        }
        var html = '';
        html += '<button class="page-btn" ' + (current <= 1 ? 'disabled' : '') + ' data-page="' + (current - 1) + '">上一页</button>';
        for (var i = 1; i <= totalPages; i++) {
            if (i === 1 || i === totalPages || (i >= current - 2 && i <= current + 2)) {
                html += '<button class="page-btn' + (i === current ? ' active' : '') + '" data-page="' + i + '">' + i + '</button>';
            } else if (i === current - 3 || i === current + 3) {
                html += '<span class="text-muted" style="padding:0 4px">...</span>';
            }
        }
        html += '<button class="page-btn" ' + (current >= totalPages ? 'disabled' : '') + ' data-page="' + (current + 1) + '">下一页</button>';
        el.innerHTML = html;
        $$('.page-btn', el).forEach(function (btn) {
            btn.addEventListener('click', function () {
                var p = parseInt(this.dataset.page);
                if (!isNaN(p) && p > 0 && p <= totalPages && p !== current) {
                    onPage(p);
                }
            });
        });
    }

    // 暴露到全局供 onclick 使用
    window.App = window.App || {};
    window.App.logout = function () {
        fetch('/api/logout', { method: 'POST' })
            .then(function () {
                window.location.href = '/login';
            })
            .catch(function () {
                window.location.href = '/login';
            });
    };
    window.App.retireSeed = function (id) {
        if (!confirm('确定撤除种子 #' + id + ' 吗？')) return;
        api('POST', '/api/seeds/' + id + '/retire').then(function () {
            showToast('已发送撤种指令', 'success');
            loadSeeds();
        }).catch(function (err) {
            showToast('撤种失败: ' + err.message, 'error');
        });
    };
    window.App.retrySeed = function (id) {
        api('POST', '/api/seeds/' + id + '/retry').then(function () {
            showToast('已重新加入队列', 'success');
            loadSeeds();
        }).catch(function (err) {
            showToast('重试失败: ' + err.message, 'error');
        });
    };
    window.App.viewSeedDetail = function (id) {
        api('GET', '/api/seeds/' + id).then(function (data) {
            showModal('种子详情 #' + id, '<pre style="font-size:12px;max-height:50vh;overflow:auto">' + escapeHtml(JSON.stringify(data, null, 2)) + '</pre>');
        }).catch(function (err) {
            showToast('加载详情失败: ' + err.message, 'error');
        });
    };

    // ==========================================================
    // 初始化向导（Setup Wizard）
    // ==========================================================

    var Setup = window.Setup = {
        currentStep: 1,
        totalSteps: 4,
        qbTested: false,
        sourceSaved: false,
        targetsSaved: false,

        // Form data persisted across steps.
        fd: {
            qb_password: '',
            src_cookie: '',
            src_passkey: '',
            src_token: '',
            targets: {
                'dev.internal-source.org': { enabled: false, cookie: '', passkey: '' },
                'luckpt.de':           { enabled: false, api_token: '' },
                'M-Team':             { enabled: false, api_token: '' },
            },
            web_password: '',
            sync_qb_password: true,
        },

        presetSites: [
            { name: 'dev.internal-source.org', type: 'classic', typeLabel: 'classic 表单', fields: [
                { id: 'cookie', label: 'Cookie', placeholder: '目标站 Cookie' },
                { id: 'passkey', label: 'Passkey', placeholder: '目标站 Passkey' },
            ]},
            { name: 'luckpt.de', type: 'nexusphp', typeLabel: 'NexusPHP API', fields: [
                { id: 'api_token', label: 'API Token', placeholder: 'NexusPHP API Token' },
            ]},
            { name: 'M-Team', type: 'mteam', typeLabel: 'M-Team API', fields: [
                { id: 'api_token', label: 'API Token', placeholder: 'M-Team x-api-key' },
            ]},
        ],

        /** 检查是否需要初始化 */
        check: function () {
            return api('GET', '/api/setup/status').then(function (data) {
                App.setupComplete = data.initialized;
                return App.setupComplete;
            }).catch(function () {
                App.setupComplete = true;
                return true;
            });
        },

        /** 进入向导模式 */
        init: function () {
            Setup.currentStep = 1;
            Setup.qbTested = false;
            Setup.sourceSaved = false;
            Setup.targetsSaved = false;

            var sidebar = $('#sidebar');
            if (sidebar) sidebar.style.display = 'none';
            var topbar = $('.topbar');
            if (topbar) topbar.style.display = 'none';

            navigateTo('setup');
        },

        /** 生成随机密码 */
        generatePassword: function () {
            var chars = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789@#$%';
            var pw = '';
            for (var i = 0; i < 16; i++) pw += chars[Math.floor(Math.random() * chars.length)];
            Setup.fd.web_password = pw;
            var inp = $('#su-web-password');
            if (inp) inp.value = pw;
            var disp = $('#su-pw-display');
            if (disp) { disp.style.display = ''; disp.textContent = pw; }
        },

        /** 跳转到步骤 */
        goToStep: function (step) {
            if (step < 1 || step > Setup.totalSteps) return;

            Setup.currentStep = step;

            // Update step indicators.
            $$('.setup-step').forEach(function (el) {
                var s = parseInt(el.dataset.step);
                el.classList.remove('active', 'done');
                if (s < step) el.classList.add('done');
                if (s === step) el.classList.add('active');
            });

            // Update body panels.
            $$('.step-body').forEach(function (el) { el.classList.remove('active'); });
            var body = $('#step-body-' + step);
            if (body) body.classList.add('active');

            // Update nav buttons.
            var prevBtn = $('#btn-prev');
            var nextBtn = $('#btn-next');
            var progress = $('#step-progress');

            if (prevBtn) prevBtn.style.visibility = step === 1 ? 'hidden' : 'visible';
            if (nextBtn) {
                if (step === Setup.totalSteps) {
                    nextBtn.style.display = 'none';
                } else {
                    nextBtn.style.display = '';
                }
            }
            if (progress) progress.textContent = '步骤 ' + step + ' / ' + Setup.totalSteps;

            // Render step-specific content.
            if (step === 1) Setup.renderStep1Content();
            if (step === 3) Setup.renderTargetCards();
            if (step === 4) Setup.renderSummary();
        },

        /** 渲染 Step 1 内容（qBittorrent） */
        renderStep1Content: function () {
            var body = $('#step-body-1');
            if (!body) return;
            body.innerHTML =
                '<p class="step-desc">测试 qBittorrent WebUI 的连接。主机、端口、用户名已预设，只需输入密码。</p>' +
                '<div class="form-group-inline"><label>主机地址</label><input type="text" class="form-input" value="qbittorrent" readonly></div>' +
                '<div class="form-group-inline"><label>端口</label><input type="text" class="form-input" value="8080" readonly></div>' +
                '<div class="form-group-inline"><label>用户名</label><input type="text" class="form-input" value="admin" readonly></div>' +
                '<div class="form-group-inline"><label>密码</label><input type="password" id="su-qb-pass" class="form-input" placeholder="CHANGE_ME" value="' + escapeAttr(Setup.fd.qb_password) + '"></div>' +
                '<div class="step-actions"><button class="btn btn-outline" id="btn-test-qb">测试连接</button><span class="test-result" id="test-result-qb"></span></div>';

            var pwInp = $('#su-qb-pass');
            if (pwInp) pwInp.addEventListener('input', function () { Setup.fd.qb_password = this.value; });

            var testBtn = $('#btn-test-qb');
            if (testBtn) testBtn.addEventListener('click', Setup.testQB);
        },

        /** 测试 qB 连接 */
        testQB: function () {
            var btn = $('#btn-test-qb');
            var resultEl = $('#test-result-qb');
            if (btn) { btn.disabled = true; btn.textContent = '测试中...'; }
            if (resultEl) { resultEl.className = 'test-result loading'; resultEl.innerHTML = '<span class="spinner"></span>正在连接...'; }

            api('POST', '/api/setup/qb', {
                host: 'qbittorrent',
                port: 8080,
                username: 'admin',
                password: Setup.fd.qb_password || '',
                use_ssl: false,
            }).then(function (data) {
                if (data.ok) {
                    Setup.qbTested = true;
                    if (resultEl) {
                        resultEl.className = 'test-result success';
                        resultEl.textContent = '连接成功！' + (data.version ? 'v' + escapeHtml(data.version) : '') +
                            (data.disk_free_gb ? ' · 磁盘 ' + data.disk_free_gb.toFixed(0) + 'GB' : '');
                    }
                    showToast('qBittorrent 连接成功', 'success');
                } else {
                    Setup.qbTested = false;
                    if (resultEl) { resultEl.className = 'test-result error'; resultEl.textContent = '连接失败: ' + (data.error || '未知错误'); }
                    showToast('qBittorrent 连接失败: ' + (data.error || '未知错误'), 'error');
                }
            }).catch(function (err) {
                Setup.qbTested = false;
                if (resultEl) { resultEl.className = 'test-result error'; resultEl.textContent = '连接失败: ' + err.message; }
                showToast('qBittorrent 连接失败: ' + err.message, 'error');
            }).finally(function () {
                if (btn) { btn.disabled = false; btn.textContent = '测试连接'; }
            });
        },

        /** 验证并执行"下一步" */
        validateAndNext: function () {
            switch (Setup.currentStep) {
                case 1:
                    if (!Setup.qbTested) { showToast('请先测试 qBittorrent 连接', 'error'); return; }
                    break;
                case 2:
                    var cookie = (getVal('su-source-cookie') || '').trim();
                    if (!cookie) { showToast('请输入源站 Cookie', 'error'); return; }
                    var passkey = (getVal('su-source-passkey') || '').trim();
                    if (!passkey) { showToast('请输入源站 Passkey', 'error'); return; }
                    Setup.saveSource();
                    return; // saveSource handles navigation.
                case 3:
                    Setup.saveTargets();
                    return; // saveTargets handles navigation.
            }
            Setup.goToStep(Setup.currentStep + 1);
        },

        /** 保存源站配置 */
        saveSource: function () {
            var btn = $('#btn-next');
            var resultEl = $('#test-result-source');
            if (btn) { btn.disabled = true; btn.textContent = '保存中...'; }
            if (resultEl) { resultEl.className = 'test-result loading'; resultEl.innerHTML = '<span class="spinner"></span>正在保存...'; }

            Setup.fd.src_cookie = (getVal('su-source-cookie') || '').trim();
            Setup.fd.src_passkey = (getVal('su-source-passkey') || '').trim();
            Setup.fd.src_token = (getVal('su-source-token') || '').trim();

            api('POST', '/api/setup/source', {
                cookie: Setup.fd.src_cookie,
                passkey: Setup.fd.src_passkey,
                token: Setup.fd.src_token,
            }).then(function (data) {
                if (data.ok) {
                    Setup.sourceSaved = true;
                    if (resultEl) { resultEl.className = 'test-result success'; resultEl.textContent = '源站配置已保存: ' + (data.name || '星陨阁'); }
                    showToast('源站配置已保存', 'success');
                    Setup.goToStep(3);
                } else {
                    if (resultEl) { resultEl.className = 'test-result error'; resultEl.textContent = '保存失败: ' + (data.error || '未知错误'); }
                    showToast('保存失败: ' + (data.error || '未知错误'), 'error');
                }
            }).catch(function (err) {
                if (resultEl) { resultEl.className = 'test-result error'; resultEl.textContent = '保存失败: ' + err.message; }
                showToast('保存失败: ' + err.message, 'error');
            }).finally(function () {
                if (btn) { btn.disabled = false; btn.textContent = '下一步'; }
            });
        },

        /** 渲染目标站卡片 */
        renderTargetCards: function () {
            var container = $('#target-cards-container');
            if (!container) return;

            var html = '';
            Setup.presetSites.forEach(function (site, idx) {
                var t = Setup.fd.targets[site.name] || { enabled: false };
                html += '<div class="target-card' + (t.enabled ? ' enabled' : ' disabled') + '" data-site-idx="' + idx + '" id="target-card-' + idx + '">' +
                    '<div class="target-card-header">' +
                        '<span class="target-card-name">' + escapeHtml(site.name) +
                            '<span class="target-card-type">' + escapeHtml(site.typeLabel) + '</span>' +
                        '</span>' +
                        '<label class="target-card-toggle" style="display:flex;align-items:center;gap:8px;cursor:pointer;font-size:13px;">' +
                            '<span class="toggle-switch">' +
                                '<input type="checkbox" class="target-enable" data-idx="' + idx + '" ' + (t.enabled ? 'checked' : '') + '>' +
                                '<span class="toggle-slider"></span>' +
                            '</span>' +
                            (t.enabled ? '已启用' : '已禁用') +
                        '</label>' +
                    '</div>' +
                    '<div class="target-card-fields">';

                site.fields.forEach(function (field) {
                    html += '<div class="form-group-inline" style="margin-bottom:8px">' +
                        '<label>' + escapeHtml(field.label) + '</label>' +
                        '<input type="text" class="form-input target-field" data-idx="' + idx + '" data-field="' + field.id + '" placeholder="' + escapeAttr(field.placeholder) + '" value="' + escapeAttr(t[field.id] || '') + '">' +
                    '</div>';
                });

                html += '</div></div>';
            });

            container.innerHTML = html;

            // Bind toggle events.
            $$('.target-enable').forEach(function (cb) {
                cb.addEventListener('change', function () {
                    var idx = parseInt(this.dataset.idx);
                    var site = Setup.presetSites[idx];
                    var card = $('#target-card-' + idx);
                    Setup.fd.targets[site.name].enabled = this.checked;
                    if (card) {
                        card.classList.toggle('enabled', this.checked);
                        card.classList.toggle('disabled', !this.checked);
                    }
                    // Update label text.
                    var toggleLabel = this.closest('.target-card-toggle');
                    if (toggleLabel) {
                        var lastChild = toggleLabel.lastChild;
                        if (lastChild && lastChild.nodeType === 3) lastChild.textContent = this.checked ? '已启用' : '已禁用';
                    }
                    // Clear fields when disabling.
                    if (!this.checked) {
                        var fields = card ? card.querySelectorAll('.target-field') : [];
                        fields.forEach(function (f) { f.value = ''; });
                        var s = Setup.fd.targets[site.name];
                        if (s) {
                            site.fields.forEach(function (fld) { s[fld.id] = ''; });
                        }
                    }
                });
            });

            // Bind field input events.
            $$('.target-field').forEach(function (input) {
                input.addEventListener('input', function () {
                    var idx = parseInt(this.dataset.idx);
                    var field = this.dataset.field;
                    var site = Setup.presetSites[idx];
                    if (!Setup.fd.targets[site.name]) Setup.fd.targets[site.name] = { enabled: false };
                    Setup.fd.targets[site.name][field] = this.value;
                });
            });
        },

        /** 保存目标站 */
        saveTargets: function () {
            var btn = $('#btn-next');
            var resultEl = $('#test-result-targets');
            if (btn) { btn.disabled = true; btn.textContent = '保存中...'; }
            if (resultEl) { resultEl.className = 'test-result loading'; resultEl.innerHTML = '<span class="spinner"></span>正在保存...'; }

            var payload = { targets: [] };
            Setup.presetSites.forEach(function (site) {
                var t = Setup.fd.targets[site.name];
                if (!t || !t.enabled) return;
                var item = { name: site.name, enabled: true };
                site.fields.forEach(function (f) {
                    item[f.id] = (t[f.id] || '').trim();
                });
                payload.targets.push(item);
            });

            if (payload.targets.length === 0) {
                if (resultEl) { resultEl.className = 'test-result error'; resultEl.textContent = '请至少启用一个目标站并填写凭据'; }
                if (btn) { btn.disabled = false; btn.textContent = '下一步'; }
                showToast('请至少启用一个目标站并填写凭据', 'error');
                return;
            }

            api('POST', '/api/setup/targets', payload).then(function (data) {
                if (data.ok) {
                    Setup.targetsSaved = true;
                    if (resultEl) { resultEl.className = 'test-result success'; resultEl.textContent = '目标站配置已保存 (' + (data.count || 0) + ' 个站点)'; }
                    showToast('目标站配置已保存', 'success');
                    Setup.goToStep(4);
                } else {
                    if (resultEl) { resultEl.className = 'test-result error'; resultEl.textContent = '保存失败: ' + (data.error || '未知错误'); }
                    showToast('保存失败: ' + (data.error || '未知错误'), 'error');
                }
            }).catch(function (err) {
                if (resultEl) { resultEl.className = 'test-result error'; resultEl.textContent = '保存失败: ' + err.message; }
                showToast('保存失败: ' + err.message, 'error');
            }).finally(function () {
                if (btn) { btn.disabled = false; btn.textContent = '下一步'; }
            });
        },

        /** 渲染完成摘要 */
        renderSummary: function () {
            var container = $('#setup-summary');
            if (!container) return;

            var html = '';
            // Web password input.
            html += '<div class="form-group-inline" style="margin-bottom:12px">' +
                '<label for="su-web-password">Web 面板密码</label>' +
                '<input type="password" id="su-web-password" class="form-input" placeholder="留空使用默认密码 admin" value="' + escapeAttr(Setup.fd.web_password) + '">' +
                '<button class="btn btn-outline btn-sm" id="btn-gen-password" style="white-space:nowrap;flex-shrink:0">🎲 生成</button>' +
            '</div>';

            if (Setup.fd.web_password) {
                html += '<div class="pw-display" style="background:var(--primary-light);border:1px solid var(--border);border-radius:8px;padding:8px 14px;margin-bottom:12px;font-family:monospace;">' +
                    '已生成密码: <code id="su-pw-display">' + escapeHtml(Setup.fd.web_password) + '</code></div>';
            }

            // Sync qB checkbox.
            html += '<div class="checkbox-group" style="margin-bottom:16px;padding:8px 12px;background:var(--input-bg);border-radius:8px">' +
                '<input type="checkbox" id="su-sync-qb" ' + (Setup.fd.sync_qb_password ? 'checked' : '') + '>' +
                '<label for="su-sync-qb" style="cursor:pointer">同步更改 qBittorrent 密码</label>' +
            '</div>';

            // Summary items.
            html += '<div class="summary-section">' +
                '<div class="summary-item"><span class="summary-icon">&#x2705;</span><span class="summary-text">qBittorrent 已连接</span><span class="summary-detail">admin@qbittorrent:8080</span></div>' +
                '<div class="summary-item"><span class="summary-icon">&#x2705;</span><span class="summary-text">源站: 星陨阁</span><span class="summary-detail">pt.internal-source.org</span></div>';

            var enabledCount = 0;
            Setup.presetSites.forEach(function (site) {
                var t = Setup.fd.targets[site.name];
                if (!t || !t.enabled) return;
                enabledCount++;
                html += '<div class="summary-item"><span class="summary-icon">&#x2705;</span><span class="summary-text">目标站: ' + escapeHtml(site.name) + '</span><span class="summary-detail">' + escapeHtml(site.typeLabel) + '</span></div>';
            });

            var disabledCount = Setup.presetSites.length - enabledCount;
            if (disabledCount > 0) {
                html += '<div class="summary-item"><span class="summary-icon" style="opacity:0.5">&#x26AA;</span><span class="summary-text" style="color:var(--text-muted)">' + disabledCount + ' 个目标站未启用</span></div>';
            }

            html += '</div>';

            container.innerHTML = html;

            // Bind events.
            var pwInp = $('#su-web-password');
            if (pwInp) pwInp.addEventListener('input', function () { Setup.fd.web_password = this.value; });

            var genBtn = $('#btn-gen-password');
            if (genBtn) genBtn.addEventListener('click', Setup.generatePassword);

            var syncCb = $('#su-sync-qb');
            if (syncCb) syncCb.addEventListener('change', function () { Setup.fd.sync_qb_password = this.checked; });
        },

        /** 完成初始化 */
        saveAndStart: function () {
            var btn = $('#btn-save-start');
            var resultEl = $('#test-result-save');
            if (btn) { btn.disabled = true; btn.textContent = '保存中...'; }
            if (resultEl) { resultEl.className = 'test-result loading'; resultEl.innerHTML = '<span class="spinner"></span>正在保存配置...'; }

            api('POST', '/api/setup/complete', {
                web_password: Setup.fd.web_password || '',
                sync_qb_password: Setup.fd.sync_qb_password,
            }).then(function (data) {
                if (data.ok) {
                    App.setupComplete = true;
                    showToast('初始化完成！正在进入仪表盘...', 'success');
                    var sidebar = $('#sidebar');
                    if (sidebar) sidebar.style.display = '';
                    var topbar = $('.topbar');
                    if (topbar) topbar.style.display = '';
                    setTimeout(function () { navigateTo('dashboard'); }, 500);
                } else {
                    if (resultEl) { resultEl.className = 'test-result error'; resultEl.textContent = '保存失败: ' + (data.error || '未知错误'); }
                    showToast('保存失败: ' + (data.error || '未知错误'), 'error');
                }
            }).catch(function (err) {
                if (resultEl) { resultEl.className = 'test-result error'; resultEl.textContent = '保存失败: ' + err.message; }
                showToast('保存失败: ' + err.message, 'error');
            }).finally(function () {
                if (btn) { btn.disabled = false; btn.textContent = '保存并开始'; }
            });
        },
    };

    // ==========================================================
    // Setup 页面渲染（SPA 模式）
    // ==========================================================

    function renderSetup(container) {
        container.innerHTML = getSetupHTML();
        bindSetupEvents();
        Setup.goToStep(1);
    }

    function getSetupHTML() {
        return '<div class="setup-page"><div class="setup-card">' +
            '<div class="setup-header"><h2 class="setup-logo">AutoSeedRelay</h2><p class="setup-subtitle">首次使用，请完成以下配置</p></div>' +

            '<div class="setup-steps" id="setup-steps">' +
                '<div class="setup-step active" data-step="1"><div class="step-dot">1</div><div class="step-line"></div></div>' +
                '<div class="setup-step" data-step="2"><div class="step-dot">2</div><div class="step-line"></div></div>' +
                '<div class="setup-step" data-step="3"><div class="step-dot">3</div><div class="step-line"></div></div>' +
                '<div class="setup-step" data-step="4"><div class="step-dot">4</div></div>' +
            '</div>' +

            '<div class="setup-body">' +
                /* Step 1: qBittorrent */
                '<div class="step-body active" id="step-body-1">' +
                    '<p class="step-desc">测试 qBittorrent WebUI 的连接。主机、端口、用户名已预设，只需输入密码。</p>' +
                    '<div class="form-group-inline"><label>主机地址</label><input type="text" class="form-input" value="qbittorrent" readonly></div>' +
                    '<div class="form-group-inline"><label>端口</label><input type="text" class="form-input" value="8080" readonly></div>' +
                    '<div class="form-group-inline"><label>用户名</label><input type="text" class="form-input" value="admin" readonly></div>' +
                    '<div class="form-group-inline"><label>密码</label><input type="password" id="su-qb-pass" class="form-input" placeholder="CHANGE_ME"></div>' +
                    '<div class="step-actions"><button class="btn btn-outline" id="btn-test-qb">测试连接</button><span class="test-result" id="test-result-qb"></span></div>' +
                '</div>' +

                /* Step 2: Source */
                '<div class="step-body" id="step-body-2">' +
                    '<p class="step-desc">源站固定为 星陨阁 (pt.internal-source.org)</p>' +
                    '<div class="source-preset"><div class="preset-header">星陨阁</div><div class="preset-url">https://pt.internal-source.org</div></div>' +
                    '<div class="form-group-inline"><label for="su-source-cookie">Cookie <span class="required">*</span></label><input type="text" id="su-source-cookie" class="form-input" placeholder="登录会话Cookie"><span class="form-hint">用于访问详情页和下载种子</span></div>' +
                    '<div class="form-group-inline"><label for="su-source-passkey">Passkey <span class="required">*</span></label><input type="text" id="su-source-passkey" class="form-input" placeholder="990794a23160ba..."><span class="form-hint">用于 RSS 抓取和种子下载</span></div>' +
                    '<div class="form-group-inline"><label for="su-source-token">API Token <span class="optional">(可选)</span></label><input type="text" id="su-source-token" class="form-input" placeholder="92|Dg7w5hdyIr0hr..."><span class="form-hint">Sanctum API Token，留空则使用 Cookie 爬取</span></div>' +
                    '<div class="step-actions"><span class="test-result" id="test-result-source"></span></div>' +
                '</div>' +

                /* Step 3: Targets */
                '<div class="step-body" id="step-body-3">' +
                    '<p class="step-desc">选择要转发到的目标站点。不需要的站点关闭开关即可跳过。</p>' +
                    '<div id="target-cards-container"><div class="empty-state"><div class="empty-text">加载中...</div></div></div>' +
                    '<div class="step-actions"><span class="test-result" id="test-result-targets"></span></div>' +
                '</div>' +

                /* Step 4: Complete */
                '<div class="step-body" id="step-body-4">' +
                    '<p class="step-desc">确认所有配置无误后保存并进入仪表盘。</p>' +
                    '<div id="setup-summary"><div class="empty-state"><div class="empty-text">等待配置...</div></div></div>' +
                    '<div class="step-actions" style="justify-content:center;margin-top:20px">' +
                        '<button class="btn btn-primary btn-lg" id="btn-save-start">保存并开始</button>' +
                        '<span class="test-result" id="test-result-save"></span>' +
                    '</div>' +
                '</div>' +
            '</div>' +

            '<div class="setup-footer">' +
                '<button class="btn btn-outline" id="btn-prev">上一步</button>' +
                '<span class="step-progress" id="step-progress">步骤 1 / 4</span>' +
                '<button class="btn btn-primary" id="btn-next">下一步</button>' +
            '</div>' +
        '</div></div>';
    }

    function bindSetupEvents() {
        var prevBtn = $('#btn-prev');
        if (prevBtn) prevBtn.addEventListener('click', function () { Setup.goToStep(Setup.currentStep - 1); });

        var nextBtn = $('#btn-next');
        if (nextBtn) nextBtn.addEventListener('click', Setup.validateAndNext);

        var saveBtn = $('#btn-save-start');
        if (saveBtn) saveBtn.addEventListener('click', Setup.saveAndStart);
    }

    // ==========================================================
    // 配置页（卡片布局，每节独立保存，目标站卡片与 setup Step 3 一致）
    // ==========================================================

    // ---- 预设目标站卡片定义（标签映射来自 sites/*.yml） ----

    var presetTargetCards = [
        {
            name: 'dev.internal-source.org',
            type: 'classic',
            typeLabel: 'classic 表单',
            fields: [
                { id: 'cookie', label: 'Cookie', placeholder: '目标站 Cookie' },
                { id: 'passkey', label: 'Passkey', placeholder: '目标站 Passkey' },
            ],
            tagMappings: [
                { src: '官方', dst: '--' },
                { src: '国语', dst: '5' },
                { src: '中字', dst: '6' },
                { src: 'HDR', dst: '7' },
                { src: '完结', dst: '11' },
                { src: '首发', dst: '2' },
                { src: '禁转', dst: '1' },
                { src: 'DIY', dst: '4' },
                { src: '特效', dst: '18' },
                { src: '杜比视界', dst: '8' },
                { src: '粤语', dst: '12' },
                { src: '特效字幕', dst: '9' },
                { src: '去头尾广告纯净版', dst: '25' },
                { src: '分集', dst: '10' },
                { src: '英字', dst: '15' },
                { src: '美剧', dst: '13' },
                { src: '韩剧', dst: '14' },
                { src: '应求', dst: '16' },
                { src: '大包', dst: '17' },
                { src: '高码率', dst: '27' },
                { src: '高帧率', dst: '26' },
                { src: '原生', dst: '24' },
                { src: '补帧', dst: '20' },
                { src: '超分', dst: '19' },
                { src: '零魔', dst: '22' },
                { src: '驻站', dst: '23' },
            ],
        },
        {
            name: 'luckpt.de',
            type: 'nexusphp',
            typeLabel: 'NexusPHP API',
            fields: [
                { id: 'api_token', label: 'API Token', placeholder: 'NexusPHP API Token' },
            ],
            tagMappings: [
                { src: '官方', dst: '--' },
                { src: '国语', dst: '5' },
                { src: '中字', dst: '6' },
                { src: 'HDR', dst: '19' },
                { src: '完结', dst: '10' },
                { src: '去头尾广告纯净版', dst: '--' },
                { src: '分集', dst: '--' },
            ],
        },
        {
            name: 'M-Team',
            type: 'mteam',
            typeLabel: 'M-Team API',
            fields: [
                { id: 'api_token', label: 'API Token', placeholder: 'M-Team x-api-key' },
            ],
            tagMappings: [
                { src: '国语', dst: 'labels' },
                { src: '中字', dst: 'labels' },
                { src: 'HDR', dst: 'tags' },
            ],
        },
    ];

    // ---- 渲染入口 ----

    function renderConfig(container) {
        container.innerHTML = getConfigHTML();
        renderTargetCards();
        loadAllConfig();
        bindConfigEvents();
    }

    function getConfigHTML() {
        return '<div class="config-page">' +

            /* ---- 标签栏 ---- */
            '<div class="config-tabs">' +
            '<button class="config-tab active" data-tab="source">源站</button>' +
            '<button class="config-tab" data-tab="targets">目标站</button>' +
            '<button class="config-tab" data-tab="qb">qBittorrent</button>' +
            '<button class="config-tab" data-tab="filter">筛选策略</button>' +
            '<button class="config-tab" data-tab="retire">撤种策略</button>' +
            '</div>' +

            '<div class="config-content">' +

            /* ---- 源站 ---- */
            '<div class="config-section active" id="sec-source">' +
            '<div class="config-card">' +
            '<div class="config-card-header"><span class="config-card-icon">&#x1F4E1;</span><span class="config-card-title">源站 — 星陨阁</span></div>' +
            '<div class="source-preset">' +
            '<span class="preset-header">星陨阁</span>' +
            '<span class="preset-url" style="margin-left:12px">https://pt.internal-source.org</span>' +
            '<span class="text-muted" style="margin-left:8px;font-size:12px">固定</span>' +
            '</div>' +
            '<div class="form-group-inline"><label for="cfg-src-cookie">Cookie</label><input type="text" class="form-input" id="cfg-src-cookie" placeholder="登录会话Cookie"></div>' +
            '<div class="form-group-inline"><label for="cfg-src-passkey">Passkey</label><input type="text" class="form-input" id="cfg-src-passkey" placeholder="990794a23160ba..."></div>' +
            '<div class="form-group-inline"><label for="cfg-src-token">API Token</label><input type="text" class="form-input" id="cfg-src-token" placeholder="92|Dg7w5hdyIr0hr..."></div>' +
            '<div class="form-actions"><button class="btn btn-primary config-save-btn" data-section="source">保存</button></div>' +
            '</div>' +
            '</div>' +

            /* ---- 目标站 ---- */
            '<div class="config-section" id="sec-targets">' +
            '<div class="config-card">' +
            '<div class="config-card-header"><span class="config-card-icon">&#x1F3AF;</span><span class="config-card-title">目标站</span></div>' +
            '<div id="config-targets-list"><div class="empty-state"><div class="empty-text">加载中...</div></div></div>' +
            '</div>' +
            '</div>' +

            /* ---- qBittorrent ---- */
            '<div class="config-section" id="sec-qb">' +
            '<div class="config-card">' +
            '<div class="config-card-header"><span class="config-card-icon">&#x1F5C4;</span><span class="config-card-title">qBittorrent</span></div>' +
            '<div class="form-group-inline"><label for="cfg-qb-host">地址</label><input type="text" id="cfg-qb-host" class="form-input" placeholder="http://qbittorrent:8080"></div>' +
            '<div class="form-group-inline"><label for="cfg-qb-user">用户名</label><input type="text" id="cfg-qb-user" class="form-input" placeholder="admin"></div>' +
            '<div class="form-group-inline"><label for="cfg-qb-pass">密码</label><input type="password" id="cfg-qb-pass" class="form-input" placeholder="qB 密码"></div>' +
            '<div class="form-group-inline"><label for="cfg-qb-dl-path">下载路径</label><input type="text" id="cfg-qb-dl-path" class="form-input" placeholder="/downloads"></div>' +
            '<div class="form-group-inline"><label for="cfg-qb-max-active">最大并发</label><input type="number" id="cfg-qb-max-active" class="form-input" value="3" min="1" max="99" style="width:100px"></div>' +
            '<div class="form-actions">' +
            '<button class="btn btn-outline test-conn-btn" data-type="qb">测试连接</button>' +
            '<button class="btn btn-primary config-save-btn" data-section="qbittorrent">保存</button>' +
            '</div></div>' +
            '</div>' +

            /* ---- 筛选策略 ---- */
            '<div class="config-section" id="sec-filter">' +
            '<div class="config-card">' +
            '<div class="config-card-header"><span class="config-card-icon">&#x1F50D;</span><span class="config-card-title">筛选策略</span></div>' +
            '<div class="form-group-inline"><label>促销类型</label><div class="form-row-inline">' +
            '<label class="form-check"><input type="checkbox" id="cfg-discount-free" checked> FREE</label>' +
            '<label class="form-check"><input type="checkbox" id="cfg-discount-2xfree" checked> 2X Free</label>' +
            '<label class="form-check"><input type="checkbox" id="cfg-discount-50"> 50%</label>' +
            '<label class="form-check"><input type="checkbox" id="cfg-discount-30"> 30%</label>' +
            '<label class="form-check"><input type="checkbox" id="cfg-discount-normal"> 普通</label>' +
            '</div></div>' +
            '<div class="form-group-inline"><label for="cfg-keywords">关键词</label><input type="text" id="cfg-keywords" class="form-input" placeholder="用逗号分隔，如 StarfallWeb, LongWeb"></div>' +
            '<div class="form-group-inline"><label for="cfg-min-size">最小大小</label><div class="form-row-inline"><input type="number" id="cfg-min-size" class="form-input" value="0" style="width:100px"><span class="form-suffix">MB</span></div></div>' +
            '<div class="form-group-inline"><label for="cfg-max-size">最大大小</label><div class="form-row-inline"><input type="number" id="cfg-max-size" class="form-input" value="0" style="width:100px"><span class="form-suffix">GB (0=不限)</span></div></div>' +
            '<div class="form-group-inline"><label>角色</label><div class="form-row-inline">' +
            '<label class="form-check"><input type="radio" name="cfg-role" value="publisher" checked> 发布者</label>' +
            '<label class="form-check"><input type="radio" name="cfg-role" value="crossseeder"> 辅种者</label>' +
            '</div></div>' +
            '<div class="form-actions"><button class="btn btn-primary config-save-btn" data-section="filter">保存</button></div>' +
            '</div>' +
            '</div>' +

            /* ---- 撤种策略 ---- */
            '<div class="config-section" id="sec-retire">' +
            '<div class="config-card">' +
            '<div class="config-card-header"><span class="config-card-icon">&#x1F5D1;</span><span class="config-card-title">撤种策略 <span style="font-size:12px;color:var(--text-muted);font-weight:400">满足任一即撤种</span></span></div>' +
            '<div class="retire-row"><label class="form-check"><input type="checkbox" id="cfg-retire-seeders"></label><span class="retire-label">做种人数</span><span class="retire-label">&ge;</span><input type="number" id="cfg-retire-seeders-val" class="form-input retire-input" value="5" min="1"><span class="retire-label">撤种</span></div>' +
            '<div class="retire-row"><label class="form-check"><input type="checkbox" id="cfg-retire-ratio"></label><span class="retire-label">分享率</span><span class="retire-label">&ge;</span><input type="number" id="cfg-retire-ratio-val" class="form-input retire-input" value="2.0" step="0.1" min="0"><span class="retire-label">撤种</span></div>' +
            '<div class="retire-row"><label class="form-check"><input type="checkbox" id="cfg-retire-days"></label><span class="retire-label">天数</span><span class="retire-label">&ge;</span><input type="number" id="cfg-retire-days-val" class="form-input retire-input" value="14" min="1"><span class="retire-label">撤种</span></div>' +
            '<div class="retire-row"><label class="form-check"><input type="checkbox" id="cfg-retire-delete-files"></label><span class="retire-label">撤种时删除文件</span></div>' +
            '<div class="form-actions"><button class="btn btn-primary config-save-btn" data-section="retire">保存</button></div>' +
            '</div>' +
            '</div>' +

            '</div>' +  /* .config-content */
            '</div>';  /* .config-page */
    }

    // ---- 目标站卡片渲染 ----

    function renderTargetCards() {
        var container = $('#config-targets-list');
        if (!container) return;

        var html = '';
        presetTargetCards.forEach(function (site, idx) {
            // Determine initial enabled state from running in-memory (will be overwritten by loadAllConfig).
            var enabled = false;

            html += '<div class="target-card disabled" data-target="' + escapeAttr(site.name) + '" id="cfg-target-card-' + idx + '">' +
                '<div class="target-card-header">' +
                    '<span class="target-card-name">' + escapeHtml(site.name) +
                        '<span class="target-card-type">' + escapeHtml(site.typeLabel) + '</span>' +
                    '</span>' +
                    '<label class="target-card-toggle" style="display:flex;align-items:center;gap:8px;cursor:pointer">' +
                        '<span class="toggle-switch">' +
                            '<input type="checkbox" class="cfg-target-enable" data-idx="' + idx + '">' +
                            '<span class="toggle-slider"></span>' +
                        '</span>' +
                        '<span class="cfg-target-status">已禁用</span>' +
                    '</label>' +
                '</div>' +
                '<div class="target-card-fields">';

            // Credential fields.
            site.fields.forEach(function (f) {
                html += '<div class="form-group-inline" style="margin-bottom:8px">' +
                    '<label for="cfg-tgt-' + idx + '-' + f.id + '">' + escapeHtml(f.label) + '</label>' +
                    '<input type="text" class="form-input cfg-target-field" id="cfg-tgt-' + idx + '-' + f.id + '" data-idx="' + idx + '" data-field="' + f.id + '" placeholder="' + escapeAttr(f.placeholder) + '">' +
                '</div>';
            });

            // Tag mapping preview: show Chinese names only, filter out skipped (dst === '--').
            var validTags = site.tagMappings.filter(function (t) { return t.dst !== '--'; });
            if (validTags.length > 0) {
                var tagPreview = validTags.map(function (t) {
                    return '<span class="tag-chip">' + escapeHtml(t.src) + '</span>';
                }).join('');
                html += '<div class="tag-mapping-preview">' +
                    '<span class="tag-mapping-label">标签映射:</span>' +
                    '<div class="tag-mapping-chips">' + tagPreview + '</div>' +
                '</div>';
            } else {
                html += '<div class="tag-mapping-preview"><span class="text-muted" style="font-size:12px">暂无标签映射数据</span></div>';
            }

            // Per-card save button + result.
            html += '<div class="target-card-actions">' +
                '<button class="btn btn-primary btn-sm cfg-target-save-btn" data-idx="' + idx + '">保存凭证</button>' +
                '<span class="test-result cfg-target-result" id="cfg-target-result-' + idx + '"></span>' +
            '</div>';

            html += '</div></div>';
        });

        container.innerHTML = html;
    }

    // ---- 事件绑定 ----

    function bindConfigEvents() {
        // Tab switching.
        $$('.config-tab').forEach(function (tab) {
            tab.addEventListener('click', function () {
                var tabName = this.dataset.tab;
                // Update tab active state.
                $$('.config-tab').forEach(function (t) { t.classList.remove('active'); });
                this.classList.add('active');
                // Update section visibility.
                $$('.config-section').forEach(function (s) { s.classList.remove('active'); });
                var section = $('#sec-' + tabName);
                if (section) section.classList.add('active');
            });
        });

        // Section-level save buttons.
        $$('.config-save-btn').forEach(function (btn) {
            btn.addEventListener('click', function () {
                saveConfig(btn.dataset.section);
            });
        });

        // QB test connection.
        $$('.test-conn-btn').forEach(function (btn) {
            btn.addEventListener('click', function () {
                testConnection(btn.dataset.type);
            });
        });

        // Target card toggle switches.
        $$('.cfg-target-enable').forEach(function (cb) {
            cb.addEventListener('change', function () {
                var idx = parseInt(this.dataset.idx);
                var card = $('#cfg-target-card-' + idx);
                if (!card) return;
                var enabled = this.checked;
                card.classList.toggle('enabled', enabled);
                card.classList.toggle('disabled', !enabled);
                // Update status label.
                var statusSpan = card.querySelector('.cfg-target-status');
                if (statusSpan) statusSpan.textContent = enabled ? '已启用' : '已禁用';
                // Clear fields when disabling.
                if (!enabled) {
                    var inputs = card.querySelectorAll('.cfg-target-field');
                    inputs.forEach(function (inp) { inp.value = ''; });
                }
            });
        });

        // Per-card save buttons.
        $$('.cfg-target-save-btn').forEach(function (btn) {
            btn.addEventListener('click', function () {
                var idx = parseInt(this.dataset.idx);
                saveTargetCard(idx);
            });
        });
    }

    // ---- 加载配置数据 ----

    function loadAllConfig() {
        api('GET', '/api/config').then(function (data) {
            if (!data) return;

            // Source.
            var src = (data.sources && data.sources[0]) || {};
            setVal('cfg-src-cookie', src.cookie || '');
            setVal('cfg-src-passkey', src.passkey || '');
            setVal('cfg-src-token', src.token || '');

            // qB.
            if (data.qb) {
                setVal('cfg-qb-host', data.qb.host || '');
                setVal('cfg-qb-user', data.qb.username || '');
                setVal('cfg-qb-pass', '');
                setVal('cfg-qb-dl-path', data.qb.download_path || '');
                setVal('cfg-qb-max-active', data.qb.max_concurrent || 3);
            }

            // Strategy.
            if (data.strategy) {
                var promos = data.strategy.promotions || [];
                setCheck('cfg-discount-free', promos.indexOf('free') !== -1);
                setCheck('cfg-discount-2xfree', promos.indexOf('2x_free') !== -1);
                setCheck('cfg-discount-50', promos.indexOf('50%') !== -1);
                setCheck('cfg-discount-30', promos.indexOf('30%') !== -1);
                setCheck('cfg-discount-normal', promos.indexOf('neutral') !== -1);
                setVal('cfg-keywords', (data.strategy.keywords || data.keywords || []).join(', '));
                setVal('cfg-min-size', data.strategy.min_size || 0);
                setVal('cfg-max-size', data.strategy.max_size || 0);
                var roleRadio = document.querySelector('input[name="cfg-role"][value="' + (data.strategy.role || 'publisher') + '"]');
                if (roleRadio) roleRadio.checked = true;
            }

            // Retire.
            if (data.retire) {
                setCheck('cfg-retire-seeders', data.retire.min_seeders > 0);
                setVal('cfg-retire-seeders-val', data.retire.min_seeders || 5);
                setCheck('cfg-retire-ratio', data.retire.min_ratio > 0);
                setVal('cfg-retire-ratio-val', data.retire.min_ratio || 2.0);
                setCheck('cfg-retire-days', data.retire.min_days > 0);
                setVal('cfg-retire-days-val', data.retire.min_days || 14);
                setCheck('cfg-retire-delete-files', data.retire.delete_files || false);
            }

            // Target cards: parse saved target profiles.
            if (data.targets && data.targets.length > 0) {
                presetTargetCards.forEach(function (site, idx) {
                    var found = null;
                    for (var i = 0; i < data.targets.length; i++) {
                        var t = data.targets[i];
                        if (t.name === site.name || (t.base_url && t.base_url.indexOf(site.name) !== -1)) {
                            found = t;
                            break;
                        }
                    }
                    if (found) {
                        // Site is enabled.
                        var card = $('#cfg-target-card-' + idx);
                        if (card) {
                            card.classList.remove('disabled');
                            card.classList.add('enabled');
                            var cb = card.querySelector('.cfg-target-enable');
                            if (cb) cb.checked = true;
                            var statusSpan = card.querySelector('.cfg-target-status');
                            if (statusSpan) statusSpan.textContent = '已启用';
                        }
                        // Fill credentials.
                        site.fields.forEach(function (f) {
                            var val = '';
                            if (f.id === 'api_token') val = found.token || '';
                            else if (f.id === 'cookie') val = found.cookie || '';
                            else if (f.id === 'passkey') val = found.passkey || '';
                            setVal('cfg-tgt-' + idx + '-' + f.id, val);
                        });
                    }
                });
            }
        }).catch(function () {});
    }

    // ---- 保存逻辑 ----

    function saveConfig(section) {
        if (section === 'targets') {
            // This is handled by saveTargetCard individually.
            return;
        }
        api('GET', '/api/config').then(function (currentCfg) {
            var payload = mergeConfigPayload(section, currentCfg || {});
            return api('POST', '/api/config', payload);
        }).then(function () {
            showToast('配置已保存', 'success');
        }).catch(function (err) {
            showToast('保存失败: ' + err.message, 'error');
        });
    }

    function saveTargetCard(idx) {
        var site = presetTargetCards[idx];
        if (!site) return;
        var card = $('#cfg-target-card-' + idx);
        if (!card) return;
        var enabled = card.classList.contains('enabled');
        var resultEl = $('#cfg-target-result-' + idx);
        var saveBtn = card.querySelector('.cfg-target-save-btn');

        if (!enabled) {
            showToast('请先启用目标站', 'error');
            return;
        }

        if (saveBtn) { saveBtn.disabled = true; saveBtn.textContent = '保存中...'; }
        if (resultEl) { resultEl.className = 'test-result loading'; resultEl.innerHTML = '<span class="spinner"></span>'; }

        // Read credential values from card inputs.
        var creds = {};
        site.fields.forEach(function (f) {
            var inp = $('#cfg-tgt-' + idx + '-' + f.id);
            creds[f.id] = inp ? inp.value.trim() : '';
        });

        api('GET', '/api/config').then(function (cfg) {
            cfg = cfg || {};
            cfg.targets = cfg.targets || [];

            // Find or create entry for this site.
            var found = null;
            for (var i = 0; i < cfg.targets.length; i++) {
                if (cfg.targets[i].name === site.name) {
                    found = cfg.targets[i];
                    break;
                }
            }
            if (!found) {
                // Base URL from YAML presets.
                var baseURLs = {
                    'dev.internal-source.org': 'https://dev.internal-source.org',
                    'luckpt.de': 'https://pt.luckpt.de',
                    'M-Team': 'https://api.m-team.cc',
                };
                found = {
                    name: site.name,
                    role: 'target',
                    base_url: baseURLs[site.name] || '',
                    type: site.type,
                };
                cfg.targets.push(found);
            }

            // Update credentials.
            site.fields.forEach(function (f) {
                if (f.id === 'api_token') {
                    found.token = creds[f.id] || found.token || '';
                } else if (f.id === 'cookie') {
                    found.cookie = creds[f.id] || found.cookie || '';
                } else if (f.id === 'passkey') {
                    found.passkey = creds[f.id] || found.passkey || '';
                }
            });

            return api('POST', '/api/config', cfg);
        }).then(function () {
            if (resultEl) { resultEl.className = 'test-result success'; resultEl.textContent = '已保存'; }
            showToast(site.name + ' 凭证已保存', 'success');
        }).catch(function (err) {
            if (resultEl) { resultEl.className = 'test-result error'; resultEl.textContent = '保存失败'; }
            showToast('保存失败: ' + err.message, 'error');
        }).finally(function () {
            if (saveBtn) { saveBtn.disabled = false; saveBtn.textContent = '保存凭证'; }
        });
    }

    function mergeConfigPayload(section, cfg) {
        switch (section) {
            case 'source':
                cfg.sources = [{
                    name: '星陨阁',
                    base_url: 'https://pt.internal-source.org',
                    rss_url: 'https://pt.internal-source.org/torrentrss.php?passkey=' + encodeURIComponent(getVal('cfg-src-passkey')),
                    cookie: getVal('cfg-src-cookie'),
                    passkey: getVal('cfg-src-passkey'),
                    token: getVal('cfg-src-token'),
                    role: 'source',
                }];
                break;
            case 'qbittorrent':
                cfg.qb = cfg.qb || {};
                cfg.qb.host = getVal('cfg-qb-host');
                cfg.qb.username = getVal('cfg-qb-user');
                cfg.qb.password = getVal('cfg-qb-pass');
                cfg.qb.port = 8080;
                cfg.qb.use_ssl = false;
                break;
            case 'filter':
                cfg.strategy = cfg.strategy || {};
                var role = (document.querySelector('input[name="cfg-role"]:checked') || {}).value || 'publisher';
                var promotions = [];
                if (isChecked('cfg-discount-free')) promotions.push('free');
                if (isChecked('cfg-discount-2xfree')) promotions.push('2x_free');
                if (isChecked('cfg-discount-50')) promotions.push('50%');
                if (isChecked('cfg-discount-30')) promotions.push('30%');
                if (isChecked('cfg-discount-normal')) promotions.push('neutral');
                cfg.strategy.promotions = promotions;
                cfg.strategy.keywords = getVal('cfg-keywords').split(',').map(function (s) { return s.trim(); }).filter(Boolean);
                cfg.strategy.min_size = parseInt(getVal('cfg-min-size')) || 0;
                cfg.strategy.max_size = parseInt(getVal('cfg-max-size')) || 0;
                cfg.strategy.role = role;
                cfg.keywords = cfg.strategy.keywords;
                break;
            case 'retire':
                cfg.retire = cfg.retire || {};
                cfg.retire.min_seeders = isChecked('cfg-retire-seeders') ? (parseInt(getVal('cfg-retire-seeders-val')) || 5) : 0;
                cfg.retire.min_ratio = isChecked('cfg-retire-ratio') ? (parseFloat(getVal('cfg-retire-ratio-val')) || 2.0) : 0;
                cfg.retire.min_days = isChecked('cfg-retire-days') ? (parseInt(getVal('cfg-retire-days-val')) || 14) : 0;
                cfg.retire.delete_files = isChecked('cfg-retire-delete-files');
                break;
        }
        return cfg;
    }

    function testConnection(type) {
        showToast('正在测试连接...', 'info');
        var payload = {};
        if (type === 'qb') {
            payload = { host: getVal('cfg-qb-host'), user: getVal('cfg-qb-user'), pass: getVal('cfg-qb-pass') };
        }
        api('POST', '/api/test-connection/' + type, payload).then(function (data) {
            showToast('连接成功: ' + (data.version || data.message || 'OK'), 'success');
        }).catch(function (err) {
            showToast('连接失败: ' + err.message, 'error');
        });
    }

    // ==========================================================
    // 日志页
    // ==========================================================

    var logLevelFilter = '';
    var logTimer = null;

    function renderLogs(container) {
        container.innerHTML = getLogsHTML();
        if (logTimer) clearInterval(logTimer);
        loadLogs();
        logTimer = setInterval(loadLogs, 3000);
        // 筛选事件
        $('#log-level-filter').addEventListener('change', function () {
            logLevelFilter = this.value;
            loadLogs();
        });
        $('#log-search').addEventListener('input', debounce(function () {
            loadLogs();
        }, 300));
    }

    function getLogsHTML() {
        return '<div class="search-bar">' +
            '<select class="form-select" id="log-level-filter" style="min-width:110px">' +
            '<option value="">全部级别</option>' +
            '<option value="DEBUG">DEBUG</option>' +
            '<option value="INFO">INFO</option>' +
            '<option value="WARN">WARN</option>' +
            '<option value="ERROR">ERROR</option>' +
            '</select>' +
            '<input type="text" class="form-input" id="log-search" placeholder="搜索日志内容..." style="max-width:280px">' +
            '<button class="btn btn-outline btn-sm" id="btn-clear-logs">清屏</button>' +
            '</div>' +
            '<div class="log-container" id="log-container"><div class="empty-state"><div class="empty-text">等待日志...</div></div></div>';
    }

    function loadLogs() {
        var params = new URLSearchParams();
        if (logLevelFilter) params.set('level', logLevelFilter);
        var search = $('#log-search');
        if (search && search.value) params.set('search', search.value);

        api('GET', '/api/logs?' + params.toString()).then(function (data) {
            var lines = data.logs || [];
            var container = $('#log-container');
            if (lines.length === 0) {
                container.innerHTML = '<div class="empty-state"><div class="empty-text">暂无日志</div></div>';
                return;
            }
            container.innerHTML = lines.map(function (line) {
                var level = (line.level || 'INFO').toLowerCase();
                return '<div class="log-line ' + level + '">' +
                    '<span class="log-ts">' + escapeHtml(line.time || '') + '</span>' +
                    '<span class="log-level">[' + escapeHtml(line.level || 'INFO') + ']</span> ' +
                    escapeHtml(line.message || '') +
                    '</div>';
            }).join('');
            container.scrollTop = container.scrollHeight;
        }).catch(function () {});
    }

    // 清屏按钮
    document.addEventListener('click', function (e) {
        if (e.target && e.target.id === 'btn-clear-logs') {
            var container = $('#log-container');
            if (container) {
                container.innerHTML = '<div class="empty-state"><div class="empty-text">日志已清屏</div></div>';
            }
        }
    });

    // ==========================================================
    // 模态框
    // ==========================================================

    function showModal(title, bodyHTML) {
        var overlay = $('#modal-overlay');
        if (!overlay) {
            overlay = document.createElement('div');
            overlay.id = 'modal-overlay';
            overlay.className = 'modal-overlay';
            overlay.innerHTML = '<div class="modal"><div class="modal-title" id="modal-title"></div><div id="modal-body"></div><div class="modal-actions"><button class="btn btn-outline" id="modal-close">关闭</button></div></div>';
            document.body.appendChild(overlay);
            $('#modal-close').addEventListener('click', closeModal);
            overlay.addEventListener('click', function (e) {
                if (e.target === overlay) closeModal();
            });
        }
        $('#modal-title').textContent = title;
        $('#modal-body').innerHTML = bodyHTML;
        overlay.classList.add('active');
    }

    function closeModal() {
        var overlay = $('#modal-overlay');
        if (overlay) overlay.classList.remove('active');
    }

    // ==========================================================
    // qB 面板跳转
    // ==========================================================

    window.App.openQB = function () {
        window.open('/qb/', '_blank');
    };

    // ==========================================================
    // 顶部状态栏
    // ==========================================================

    function updateClock() {
        var el = $('#topbar-clock');
        if (el) {
            var now = new Date();
            el.textContent = now.toLocaleString('zh-CN');
        }
    }

    setInterval(updateClock, 1000);
    updateClock();

    // 刷新按钮
    document.addEventListener('click', function (e) {
        if (e.target && e.target.id === 'btn-refresh') {
            if (App.currentPage === 'dashboard') {
                refreshDashboard();
            } else if (App.currentPage === 'seeds') {
                loadSeeds();
            } else if (App.currentPage === 'logs') {
                loadLogs();
            }
            showToast('已刷新', 'info');
        }
    });

    // 侧边栏移动端切换
    document.addEventListener('click', function (e) {
        if (e.target && e.target.id === 'menu-toggle') {
            var sidebar = $('#sidebar');
            if (sidebar) sidebar.classList.toggle('open');
        }
    });

    // ==========================================================
    // 初始化
    // ==========================================================

    function init() {
        // 主题系统: 4 套预设皮肤，localStorage > 系统偏好 > 深色默认
        var validThemes = ['dark', 'light', 'black', 'blue'];
        var saved = localStorage.getItem('theme');
        var theme;
        if (saved && validThemes.indexOf(saved) !== -1) {
            theme = saved;
        } else {
            theme = window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
        }
        var root = document.documentElement;
        root.setAttribute('data-theme', theme);

        // 主题选择器同步
        var picker = $('#theme-picker');
        if (picker) {
            picker.value = theme;
            picker.addEventListener('change', function () {
                var val = this.value;
                root.setAttribute('data-theme', val);
                localStorage.setItem('theme', val);
            });
        }

        // 监听系统主题变化（仅在无手动设置时跟随）
        if (window.matchMedia) {
            window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function (e) {
                if (!localStorage.getItem('theme')) {
                    var sysTheme = e.matches ? 'dark' : 'light';
                    root.setAttribute('data-theme', sysTheme);
                    if (picker) picker.value = sysTheme;
                }
            });
        }

        // 绑定侧边栏导航
        $$('.sidebar-nav a').forEach(function (a) {
            a.addEventListener('click', function (e) {
                e.preventDefault();
                var page = this.dataset.page;
                if (page) navigateTo(page);
                var sidebar = $('#sidebar');
                if (sidebar) sidebar.classList.remove('open');
            });
        });

        // 绑定退出登录按钮
        var logoutBtn = $('#btn-logout');
        if (logoutBtn) {
            logoutBtn.addEventListener('click', function (e) {
                e.preventDefault();
                App.logout();
            });
        }

        // 检查是否需要初始化向导
        Setup.check().then(function (isSetupDone) {
            if (isSetupDone) {
                navigateTo('dashboard');
            } else {
                Setup.init();
            }
        }).catch(function () {
            navigateTo('dashboard');
        });
    }

    // ==========================================================
    // 辅助函数
    // ==========================================================

    function escapeHtml(str) {
        if (!str) return '';
        return String(str)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;');
    }

    function escapeAttr(str) {
        if (!str) return '';
        return String(str).replace(/'/g, "\\'").replace(/"/g, '&quot;');
    }

    function getVal(id) {
        var el = $('#' + id);
        return el ? el.value : '';
    }

    function setVal(id, value) {
        var el = $('#' + id);
        if (el) el.value = value || '';
    }

    function isChecked(id) {
        var el = $('#' + id);
        return el ? el.checked : false;
    }

    function setCheck(id, value) {
        var el = $('#' + id);
        if (el) el.checked = !!value;
    }

    function debounce(fn, delay) {
        var timer;
        return function () {
            var ctx = this, args = arguments;
            clearTimeout(timer);
            timer = setTimeout(function () { fn.apply(ctx, args); }, delay);
        };
    }

    // 启动应用
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }
})();
