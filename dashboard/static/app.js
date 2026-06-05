// ══════════════════════════════════════════════════════════════════════════════
//  Toko-Mo-Co — Dashboard v7
//  Matches the enterprise sidebar layout in index.html + styles.css v7.
// ══════════════════════════════════════════════════════════════════════════════

// ── State ─────────────────────────────────────────────────────────────────────
let ws;
let reconnectInterval;

const costHistory   = [];
const MAX_HISTORY   = 60;   // chart data points
const MAX_FEED_ROWS = 200;  // rows kept in DOM

let feedRowCount = 0;
let errorCount   = 0;
let agentCount   = 0;       // tracks unique agents rendered

// Track totals from all requests (replayed + live)
let totalCostAccumulated = 0;
let totalInputTokens = 0;
let totalOutputTokens = 0;
let totalRequests = 0;

// ── Agent colour palette (deterministic hash → colour) ────────────────────────
// 12 perceptually-distinct accent colours that all read well on dark backgrounds.
const AGENT_PALETTE = [
    '#2f81f7', '#3fb950', '#fb923c', '#f85149',
    '#bc8cff', '#4ade80', '#fbbf24', '#38bdf8',
    '#e879f9', '#34d399', '#f97316', '#a78bfa',
];

function agentColor(agentId) {
    // djb2-style hash — fast, deterministic, good distribution
    let h = 5381;
    for (let i = 0; i < agentId.length; i++) {
        h = (((h << 5) + h) + agentId.charCodeAt(i)) >>> 0;
    }
    return AGENT_PALETTE[h % AGENT_PALETTE.length];
}

// ── WebSocket ─────────────────────────────────────────────────────────────────
function connect() {
    if (ws && ws.readyState !== WebSocket.CLOSED) {
        ws.onclose = null;
        ws.close();
    }
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(`${protocol}//${window.location.host}/ws`);

    ws.onopen  = () => { updateConnectionStatus(true);  clearTimeout(reconnectInterval); };
    ws.onclose = () => { updateConnectionStatus(false); reconnectInterval = setTimeout(connect, 3000); };
    ws.onerror = () => updateConnectionStatus(false);
    ws.onmessage = (event) => {
        try { handleMessage(JSON.parse(event.data)); } catch (err) { console.warn('[WS] message error:', err.message); }
    };
}

function updateConnectionStatus(connected) {
    const dot  = document.getElementById('statusDot');
    const text = document.getElementById('statusText');
    const pill = document.getElementById('statusPill');
    if (!dot) return;
    dot.className  = 'status-dot ' + (connected ? 'live' : 'dead');
    text.textContent = connected ? 'Live' : 'Reconnecting…';
    pill.className = 'status-pill ' + (connected ? 'live' : '');
}

// ── Message router ────────────────────────────────────────────────────────────
function handleMessage(data) {
    switch (data.type) {
        case 'global_stats':
            // Initialize totals from database aggregates (sent on connect)
            totalRequests = data.total_requests || 0;
            totalCostAccumulated = data.total_cost || 0;
            totalInputTokens = data.total_input_tokens || 0;
            totalOutputTokens = data.total_output_tokens || 0;
            errorCount = data.error_count || 0;
            updateAccumulatedMetrics();
            break;

        case 'cost_update':
            updateCostMetrics(data);
            break;

        case 'request_event':
            addFeedRow(data);
            // Only accumulate from LIVE events (not replayed)
            // Replayed events are already counted in global_stats
            if (!data.replayed) {
                totalRequests++;
                // Cache hits are free — don't count their cost as actual spend
                if (data.cost && !data.cache_hit) totalCostAccumulated += data.cost;
                if (data.input_tokens) totalInputTokens += data.input_tokens;
                if (data.output_tokens) totalOutputTokens += data.output_tokens;
                updateAccumulatedMetrics();
                updateChart(totalCostAccumulated);
            }
            break;

        case 'agent_summaries':
            renderAgentCards(data.agents || []); break;
    }
}

function updateAccumulatedMetrics() {
    // Core metrics
    setText('totalCost', `$${totalCostAccumulated.toFixed(4)}`);
    setText('requestCount', totalRequests.toLocaleString());
    setText('inputTokens', totalInputTokens.toLocaleString());
    setText('outputTokens', totalOutputTokens.toLocaleString());

    // Calculated metrics
    const avgCost = totalRequests > 0 ? (totalCostAccumulated / totalRequests) : 0;
    setText('avgCost', `$${avgCost.toFixed(4)}`);

    const errorRate = totalRequests > 0 ? ((errorCount / totalRequests) * 100) : 0;
    setText('errorRate', `${errorRate.toFixed(1)}%`);
}

// ── Cost metrics ──────────────────────────────────────────────────────────────
function updateCostMetrics(data) {
    setText('totalCost',    `$${data.total_cost.toFixed(4)}`);
    setText('requestCount', (data.request_count || 0).toLocaleString());
    setText('inputTokens',  (data.input_tokens  || 0).toLocaleString());
    setText('outputTokens', (data.output_tokens || 0).toLocaleString());

    const id = data.session_id || '';
    setText('sessionId', id.length > 10 ? id.substring(0, 10) + '…' : id || '—');

    const dur = Math.floor(data.duration || 0);
    const m = Math.floor(dur / 60), s = dur % 60;
    setText('duration', m > 0 ? `${m}m ${s}s` : `${s}s`);

    const burnRate = data.duration > 0 ? (data.total_cost / data.duration) * 60 : 0;
    setText('burnRate', `$${burnRate.toFixed(4)}/min`);

    updateChart(data.total_cost);
}

function setText(id, val) {
    const el = document.getElementById(id);
    if (el) el.textContent = val;
}

// ── Chart ─────────────────────────────────────────────────────────────────────
let burnRateChart;

function initChart() {
    const ctx = document.getElementById('burnRateChart');
    if (!ctx) return;
    burnRateChart = new Chart(ctx.getContext('2d'), {
        type: 'line',
        data: {
            labels: [],
            datasets: [{
                label: 'Cumulative Cost ($)',
                data: [],
                borderColor: '#2f81f7',
                backgroundColor: 'rgba(47,129,247,0.07)',
                borderWidth: 1.5,
                tension: 0.35,
                fill: true,
                pointRadius: 2,
                pointHoverRadius: 4,
            }]
        },
        options: {
            responsive: true,
            maintainAspectRatio: false,
            interaction: { intersect: false, mode: 'index' },
            plugins: {
                legend: { display: false },
                tooltip: {
                    backgroundColor: 'rgba(13,17,23,0.95)',
                    borderColor: '#30363d',
                    borderWidth: 1,
                    padding: 10,
                    callbacks: { label: ctx => `$${ctx.parsed.y.toFixed(4)}` }
                }
            },
            scales: {
                y: {
                    beginAtZero: true,
                    ticks: {
                        color: '#6e7681',
                        font: { size: 10 },
                        callback: v => `$${v.toFixed(3)}`
                    },
                    grid: { color: 'rgba(48,54,61,0.6)' }
                },
                x: {
                    ticks: { color: '#6e7681', font: { size: 10 }, maxTicksLimit: 8 },
                    grid: { display: false }
                }
            }
        }
    });
}

function updateChart(cost) {
    costHistory.push({ time: new Date().toLocaleTimeString(), cost });
    if (costHistory.length > MAX_HISTORY) costHistory.shift();

    if (burnRateChart) {
        burnRateChart.data.labels              = costHistory.map(h => h.time);
        burnRateChart.data.datasets[0].data    = costHistory.map(h => h.cost);
        burnRateChart.update('none');
    }
    setText('chartMeta', `${costHistory.length} point${costHistory.length !== 1 ? 's' : ''}`);
}

// Fetch persisted cost-over-time data from the API and seed the chart.
// Called once at page load so the chart is never empty.
function fetchCostHistory() {
    fetch('/api/analytics/cost-over-time?bucket=15&hours=24')
        .then(r => r.json())
        .then(data => {
            const buckets = data.buckets || [];
            if (buckets.length === 0) return;

            // Clear any points that arrived from WebSocket before this fetch completed
            costHistory.length = 0;

            // Seed the chart with persisted cumulative cost data
            buckets.forEach(b => {
                costHistory.push({ time: b.label, cost: b.cumulative_cost });
            });

            // Trim to MAX_HISTORY keeping the most recent points
            while (costHistory.length > MAX_HISTORY) costHistory.shift();

            // Update the running total so live WebSocket events continue from the right value
            const lastBucket = buckets[buckets.length - 1];
            if (lastBucket) totalCostAccumulated = lastBucket.cumulative_cost;

            // Render
            if (burnRateChart) {
                burnRateChart.data.labels           = costHistory.map(h => h.time);
                burnRateChart.data.datasets[0].data = costHistory.map(h => h.cost);
                burnRateChart.update('none');
            }
            setText('chartMeta', `${costHistory.length} point${costHistory.length !== 1 ? 's' : ''}`);
        })
        .catch(() => { /* silently ignore — chart just starts empty like before */ });
}

// ── Agents view ───────────────────────────────────────────────────────────────
const agentState = {};           // last-seen totals per agent for delta / flash
const groupCollapseState = {};   // { appName: true/false } persists collapse across re-renders

function renderAgentCards(agents) {
    const container = document.getElementById('agentsGroupContainer');
    const empty     = document.getElementById('agentEmpty');
    if (!container) return;

    if (!agents || agents.length === 0) {
        if (empty) empty.style.display = '';
        return;
    }
    if (empty) empty.style.display = 'none';

    // ── 1. Group agents by app_name ──────────────────────────────────────
    const groups = {};
    agents.forEach(agent => {
        const app = agent.app_name || '';
        if (!groups[app]) groups[app] = [];
        groups[app].push(agent);
    });

    // ── 2. Sort agents within each group by request_count desc ───────────
    Object.values(groups).forEach(list => {
        list.sort((a, b) => (b.request_count || 0) - (a.request_count || 0));
    });

    // ── 3. Sort groups by total cost desc; "Ungrouped" last ──────────────
    const groupKeys = Object.keys(groups).sort((a, b) => {
        const aUngrouped = a === '';
        const bUngrouped = b === '';
        if (aUngrouped && !bUngrouped) return 1;
        if (!aUngrouped && bUngrouped) return -1;
        const aCost = groups[a].reduce((s, ag) => s + (ag.total_cost || 0), 0);
        const bCost = groups[b].reduce((s, ag) => s + (ag.total_cost || 0), 0);
        return bCost - aCost;
    });

    // ── 4. Build DOM ─────────────────────────────────────────────────────
    let newAgentAdded = false;
    const fragment = document.createDocumentFragment();

    groupKeys.forEach(appKey => {
        const appAgents = groups[appKey];
        const displayName = appKey || 'Ungrouped';
        const isCollapsed = groupCollapseState[displayName] === true;

        // Aggregate stats for the group
        let groupCost = 0, groupRequests = 0, groupIn = 0, groupOut = 0;
        appAgents.forEach(a => {
            groupCost     += a.total_cost    || 0;
            groupRequests += a.request_count || 0;
            groupIn       += a.input_tokens  || 0;
            groupOut      += a.output_tokens || 0;
        });
        const groupCostStr = groupCost < 0.0001
            ? '<$0.0001' : `$${groupCost.toFixed(4)}`;

        // ── Group section ────────────────────────────────────────────────
        const section = document.createElement('div');
        section.className = 'app-group' + (isCollapsed ? ' collapsed' : '');

        // ── Group header ─────────────────────────────────────────────────
        section.innerHTML = `
            <div class="app-group-header">
                <svg class="app-group-chevron" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2">
                    <polyline points="4,2 12,8 4,14"/>
                </svg>
                <span class="app-group-name">${escapeHtml(displayName)}</span>
                <span class="app-group-agent-count">${appAgents.length}</span>
                <div class="app-group-stats">
                    <span class="app-group-stat">
                        <span class="app-group-stat-label">Reqs</span>
                        <span class="app-group-stat-value">${groupRequests.toLocaleString()}</span>
                    </span>
                    <span class="app-group-stat">
                        <span class="app-group-stat-label">Cost</span>
                        <span class="app-group-stat-value cost">${groupCostStr}</span>
                    </span>
                </div>
            </div>
            <div class="app-group-body">
                <table class="agents-table">
                    <thead>
                        <tr>
                            <th>Agent</th>
                            <th class="col-num">Requests</th>
                            <th class="col-cost">Cost</th>
                            <th class="col-num col-tokens-in">In Tokens</th>
                            <th class="col-num col-tokens-out">Out Tokens</th>
                            <th class="col-time">Last Seen</th>
                        </tr>
                    </thead>
                    <tbody></tbody>
                </table>
            </div>`;

        // ── Collapse toggle ──────────────────────────────────────────────
        const header = section.querySelector('.app-group-header');
        header.addEventListener('click', () => {
            const collapsed = section.classList.toggle('collapsed');
            groupCollapseState[displayName] = collapsed;
        });

        // ── Agent rows ───────────────────────────────────────────────────
        const tbody = section.querySelector('tbody');

        appAgents.forEach(agent => {
            const id       = agent.agent_id || 'unknown';
            const color    = agentColor(id);
            const costStr  = agent.total_cost < 0.0001
                ? '<$0.0001' : `$${agent.total_cost.toFixed(4)}`;
            const inTok    = (agent.input_tokens  || 0).toLocaleString();
            const outTok   = (agent.output_tokens || 0).toLocaleString();
            const reqs     = (agent.request_count || 0).toLocaleString();
            const lastTs   = agent.last_seen || 0;

            const prev   = agentState[id];
            const isNew  = !prev;
            const costUp = prev && agent.total_cost > prev.total_cost;
            agentState[id] = { total_cost: agent.total_cost };

            const row = document.createElement('tr');
            row.className = 'agent-row'
                + (isNew  ? ' agent-row-new' : '')
                + (costUp ? ' agent-row-cost-flash' : '');
            row.dataset.agentId = id;

            row.innerHTML = `
                <td>
                    <div class="agent-id-cell">
                        <span class="agent-accent-dot" style="background:${color}"></span>
                        <span class="agent-id-text">${escapeHtml(id)}</span>
                    </div>
                </td>
                <td class="col-num">${reqs}</td>
                <td class="col-cost">${costStr}</td>
                <td class="col-num col-tokens-in">${inTok}</td>
                <td class="col-num col-tokens-out">${outTok}</td>
                <td class="col-time" data-ts="${lastTs}">${relativeTime(lastTs)}</td>`;

            row.addEventListener('click', () => showAgentDetail(agent));

            if (isNew) {
                setTimeout(() => row.classList.remove('agent-row-new'), 400);
                newAgentAdded = true;
            }
            if (costUp) {
                setTimeout(() => row.classList.remove('agent-row-cost-flash'), 600);
            }

            tbody.appendChild(row);
        });

        // ── Summary row ──────────────────────────────────────────────────
        const summaryRow = document.createElement('tr');
        summaryRow.className = 'agent-summary-row';
        summaryRow.innerHTML = `
            <td><span class="agent-summary-label">Total (${appAgents.length} agent${appAgents.length !== 1 ? 's' : ''})</span></td>
            <td class="col-num">${groupRequests.toLocaleString()}</td>
            <td class="col-cost">${groupCostStr}</td>
            <td class="col-num col-tokens-in">${groupIn.toLocaleString()}</td>
            <td class="col-num col-tokens-out">${groupOut.toLocaleString()}</td>
            <td class="col-time"></td>`;
        tbody.appendChild(summaryRow);

        fragment.appendChild(section);
    });

    // ── 5. Swap DOM ──────────────────────────────────────────────────────
    container.querySelectorAll('.app-group').forEach(el => el.remove());
    container.appendChild(fragment);

    // ── 6. Re-apply search filter if active ──────────────────────────────
    const searchInput = document.getElementById('agentSearchInput');
    if (searchInput && searchInput.value.trim()) {
        filterAgentTable(searchInput.value.trim().toLowerCase());
    }

    // ── 7. Update nav badge ──────────────────────────────────────────────
    if (newAgentAdded) {
        agentCount = Object.keys(agentState).length;
        updateNavBadge('nav-badge-agents', agentCount);
    }
}

// ── Live feed ─────────────────────────────────────────────────────────────────
const PROVIDER_LABELS = { openai: 'OpenAI', anthropic: 'Anthropic', gemini: 'Gemini' };

function relativeTime(unixTs) {
    const diff = Math.floor(Date.now() / 1000 - unixTs);
    if (diff < 5)    return 'just now';
    if (diff < 60)   return `${diff}s ago`;
    if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
    return `${Math.floor(diff / 3600)}h ago`;
}

function addFeedRow(data) {
    const isReplayed = !!data.replayed;
    const isError    = !!(data.is_error || (data.status_code && data.status_code >= 400));

    const body  = document.getElementById('feedBody');
    const empty = body && body.querySelector('.feed-empty');
    if (empty) empty.remove();

    // ── Counts ──────────────────────────────────────────────────────────────
    feedRowCount++;
    updateNavBadge('nav-badge-feed', feedRowCount);
    setText('feedCount', `${feedRowCount.toLocaleString()} request${feedRowCount !== 1 ? 's' : ''}`);

    if (isError) {
        errorCount++;
        setText('errorCount', errorCount.toLocaleString());
    }

    // ── Cell contents ────────────────────────────────────────────────────────
    const provider = (data.provider || 'openai').toLowerCase();
    const providerLabel = PROVIDER_LABELS[provider] || provider;

    // Agent badge
    const agentId    = data.agent_id && data.agent_id !== 'unknown' ? data.agent_id : null;
    const agentBadge = agentId
        ? `<span class="badge badge-agent" style="border-color:${agentColor(agentId)};color:${agentColor(agentId)}">${escapeHtml(agentId)}</span>`
        : `<span class="badge badge-agent-unknown">—</span>`;

    // Loop badge
    let loopBadge = '';
    if (data.loop_detected) {
        const sev = data.loop_severity || 'low';
        loopBadge = `<span class="badge badge-loop-${sev}">${sev}</span>`;
    }

    // Error badge (for error rows in the flags column)
    const errorBadge = isError ? `<span class="badge badge-error">err</span>` : '';

    // Stream icon (small text node — no badge needed)
    const streamIcon = data.is_streaming
        ? `<span class="stream-icon" title="Streaming">▶</span>` : '';

    // Retry badge
    const retryBadge = (data.retry_count && data.retry_count > 0)
        ? `<span class="badge badge-retry" title="Retried ${data.retry_count} times">${data.retry_count}× retry</span>`
        : '';

    // Fallback badge
    const fallbackBadge = data.fallback_used
        ? `<span class="badge badge-fallback" title="Fallback: ${data.fallback_from} → ${data.fallback_to}">fallback</span>`
        : '';

    // Cache hit badge
    const cacheBadge = data.cache_hit
        ? `<span class="badge badge-cached" title="Served from cache">cached</span>`
        : '';

    // PII redacted badge
    const piiBadge = (data.pii_redacted || 0) > 0
        ? `<span class="badge badge-pii" title="${data.pii_redacted} PII item(s) redacted">PII (${data.pii_redacted})</span>`
        : '';
    const jailbreakBadge = data.jailbreak_detected
        ? `<span class="badge badge-jailbreak" title="Jailbreak detected (NeMo Guard)">Jailbreak</span>`
        : '';

    const costStr    = (data.cost || 0) < 0.0001 ? '<$0.0001' : `$${data.cost.toFixed(4)}`;
    const latencyStr = (data.latency_ms || 0) >= 1000
        ? `${(data.latency_ms / 1000).toFixed(1)}s` : `${data.latency_ms || 0}ms`;

    // Status code cell — colour-coded
    const status = data.status_code || 0;
    let statusClass = '';
    if      (status >= 500) statusClass = 'status-5xx';
    else if (status >= 400) statusClass = 'status-4xx';
    else if (status >= 200) statusClass = 'status-2xx';
    const statusCell = status ? `<span class="col-status ${statusClass}">${status}</span>` : '—';

    // ── Build row ────────────────────────────────────────────────────────────
    const row = document.createElement('tr');
    row.className = 'feed-row'
        + (isReplayed ? ' replayed-row' : ' live-row')
        + (isError    ? ' error-row'    : '');
    row.dataset.ts = data.timestamp || 0;

    row.innerHTML = `
        <td class="col-ts"      data-ts="${data.timestamp || 0}">${relativeTime(data.timestamp || 0)}</td>
        <td class="col-agent">${agentBadge}</td>
        <td class="col-provider"><span class="badge badge-${provider}">${providerLabel}</span></td>
        <td class="col-model">${escapeHtml(data.model || '')}</td>
        <td class="col-prompt"><span class="prompt-text">${escapeHtml(data.prompt_preview || '')}</span></td>
        <td class="col-num">${(data.input_tokens  || 0).toLocaleString()}</td>
        <td class="col-num">${(data.output_tokens || 0).toLocaleString()}</td>
        <td class="col-cost">${costStr}</td>
        <td class="col-latency">${latencyStr}</td>
        <td class="col-status">${statusCell}</td>
        <td class="col-flags">${cacheBadge}${piiBadge}${jailbreakBadge}${loopBadge}${errorBadge}${retryBadge}${fallbackBadge}${streamIcon}</td>`;

    if (isReplayed) {
        // Replayed events arrive oldest-first, append them
        // This builds the list with oldest at bottom, newest at top
        body.appendChild(row);
    } else {
        // Live events insert at top (newest first)
        body.insertBefore(row, body.firstChild);
        // Remove animation class after transition ends
        requestAnimationFrame(() => {
            requestAnimationFrame(() => row.classList.remove('live-row'));
        });
    }

    // Cap DOM rows
    const rows = body.querySelectorAll('tr.feed-row');
    if (rows.length > MAX_FEED_ROWS) rows[rows.length - 1].remove();
}

// ── Nav badge helper ──────────────────────────────────────────────────────────
function updateNavBadge(id, count) {
    const el = document.getElementById(id);
    if (el) el.textContent = count > 999 ? '999+' : String(count);
}

// ── Refresh relative timestamps every 15 s ────────────────────────────────────
setInterval(() => {
    document.querySelectorAll('.col-ts[data-ts], .col-time[data-ts]').forEach(cell => {
        cell.textContent = relativeTime(parseInt(cell.dataset.ts, 10));
    });
    document.querySelectorAll('.agent-last-seen[data-ts]').forEach(el => {
        el.textContent = relativeTime(parseInt(el.dataset.ts, 10));
    });
}, 15_000);

// ── Sidebar navigation ────────────────────────────────────────────────────────
const VIEW_TITLES = {
    feed:     'Request Feed',
    agents:   'Agents & Applications',
    sessions: 'Sessions',
    rules:    'Rules & Policies',
    alerts:   'Alerts',
    security: 'Security Analytics',
};

// Central view-switching function — called by click handlers and hash routing
function switchView(view) {
    if (!view || !VIEW_TITLES[view]) view = 'feed';

    // Update sidebar active state
    document.querySelectorAll('.nav-item[data-view]').forEach(b => b.classList.remove('active'));
    const navBtn = document.querySelector(`.nav-item[data-view="${view}"]`);
    if (navBtn) navBtn.classList.add('active');

    // Update content area
    document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
    const viewEl = document.getElementById('view-' + view);
    if (viewEl) viewEl.classList.add('active');
    setText('topbarTitle', VIEW_TITLES[view] || view);

    // Persist in URL hash (without triggering hashchange)
    if (location.hash !== '#' + view) {
        history.replaceState(null, '', '#' + view);
    }

    // Load data for view-specific content
    if (view === 'sessions') {
        loadSessions();
    }
    if (view === 'security') {
        loadSecurityStats();
        startSecurityRefresh();
    } else {
        stopSecurityRefresh();
    }
}

document.querySelectorAll('.nav-item[data-view]').forEach(btn => {
    // Disabled items (Alerts) are visual-only — skip binding
    if (btn.style.cursor === 'not-allowed') return;

    btn.addEventListener('click', () => {
        switchView(btn.dataset.view);
    });
});

// Restore view from URL hash (e.g. /#sessions coming back from settings page)
function restoreViewFromHash() {
    const hash = location.hash.replace('#', '');
    if (hash && VIEW_TITLES[hash]) {
        switchView(hash);
    }
}
window.addEventListener('hashchange', restoreViewFromHash);

// ── Connect drawer ────────────────────────────────────────────────────────────
function openConnectDrawer() {
    document.getElementById('setupDrawer').classList.add('open');
    document.getElementById('setupOverlay').classList.add('open');
}
function closeConnectDrawer() {
    document.getElementById('setupDrawer').classList.remove('open');
    document.getElementById('setupOverlay').classList.remove('open');
}

// Sidebar "Connect App" button
const connectBtn = document.getElementById('connectBtn');
if (connectBtn) connectBtn.addEventListener('click', openConnectDrawer);

// Topbar "Connect" button
const connectBtnTop = document.getElementById('connectBtnTop');
if (connectBtnTop) connectBtnTop.addEventListener('click', openConnectDrawer);

// Close button + overlay
const setupClose = document.getElementById('setupClose');
if (setupClose) setupClose.addEventListener('click', closeConnectDrawer);
const setupOverlay = document.getElementById('setupOverlay');
if (setupOverlay) setupOverlay.addEventListener('click', closeConnectDrawer);

// ── Settings drawer ───────────────────────────────────────────────────────────
function openSettings() {
    document.getElementById('settingsDrawer').classList.add('open');
    document.getElementById('settingsOverlay').classList.add('open');
}
function closeSettings() {
    document.getElementById('settingsDrawer').classList.remove('open');
    document.getElementById('settingsOverlay').classList.remove('open');
}

const settingsBtn   = document.getElementById('settingsBtn');
const settingsClose = document.getElementById('settingsClose');
const settingsOverlay = document.getElementById('settingsOverlay');
if (settingsBtn)     settingsBtn.addEventListener('click', openSettings);
if (settingsClose)   settingsClose.addEventListener('click', closeSettings);
if (settingsOverlay) settingsOverlay.addEventListener('click', closeSettings);

// Similarity slider — live label update
const simSlider = document.getElementById('similarityThreshold');
if (simSlider) {
    simSlider.addEventListener('input', e => {
        setText('similarityValue', `${e.target.value}%`);
    });
}

// ── Connect drawer — code tabs ────────────────────────────────────────────────
document.querySelectorAll('.tab-btn').forEach(btn => {
    btn.addEventListener('click', () => {
        const tab = btn.dataset.tab;
        // Scope tab switching to the drawer the button is inside
        const container = btn.closest('.setup-drawer') || document;
        container.querySelectorAll('.tab-btn').forEach(b => b.classList.remove('active'));
        container.querySelectorAll('.tab-content').forEach(c => c.classList.remove('active'));
        btn.classList.add('active');
        const content = document.getElementById('tab-' + tab);
        if (content) content.classList.add('active');
    });
});

// ── Copy buttons ──────────────────────────────────────────────────────────────
document.querySelectorAll('.copy-btn').forEach(btn => {
    btn.addEventListener('click', () => {
        const target = document.getElementById(btn.dataset.target);
        if (!target) return;
        navigator.clipboard.writeText(target.textContent.trim()).then(() => {
            const orig = btn.textContent;
            btn.textContent = 'Copied!';
            btn.classList.add('copied');
            setTimeout(() => { btn.textContent = orig; btn.classList.remove('copied'); }, 2000);
        }).catch(() => {
            // Fallback for insecure contexts
            const ta = document.createElement('textarea');
            ta.value = target.textContent.trim();
            ta.style.position = 'fixed'; ta.style.opacity = '0';
            document.body.appendChild(ta);
            ta.select();
            document.execCommand('copy');
            document.body.removeChild(ta);
            btn.textContent = 'Copied!';
            setTimeout(() => { btn.textContent = 'Copy'; }, 2000);
        });
    });
});

// ── Clear feed ────────────────────────────────────────────────────────────────
const clearFeedBtn = document.getElementById('clearFeedBtn');
if (clearFeedBtn) {
    clearFeedBtn.addEventListener('click', () => {
        const body = document.getElementById('feedBody');
        if (!body) return;
        body.innerHTML = `<tr class="feed-empty"><td colspan="11">Feed cleared. New requests will appear here in real-time.</td></tr>`;
        feedRowCount = 0;
        errorCount   = 0;
        setText('feedCount',  '0 requests');
        setText('errorCount', '0');
        updateNavBadge('nav-badge-feed', 0);
    });
}

// ── Agent Detail Modal ────────────────────────────────────────────────────────
function showAgentDetail(agent) {
    const modal = document.getElementById('agentDetailModal');
    const overlay = document.getElementById('agentDetailOverlay');
    if (!modal || !overlay) return;

    const agentId = agent.agent_id || 'unknown';
    const appName = agent.app_name || '—';
    const color = agentColor(agentId);
    const costStr = agent.total_cost < 0.0001 ? '<$0.0001' : `$${agent.total_cost.toFixed(4)}`;

    // Update modal content
    document.getElementById('agentDetailName').textContent = agentId;
    document.getElementById('agentDetailApp').textContent = appName;
    document.getElementById('agentDetailCost').textContent = costStr;
    document.getElementById('agentDetailRequests').textContent = (agent.request_count || 0).toLocaleString();
    document.getElementById('agentDetailInputTokens').textContent = (agent.input_tokens || 0).toLocaleString();
    document.getElementById('agentDetailOutputTokens').textContent = (agent.output_tokens || 0).toLocaleString();
    document.getElementById('agentDetailLastSeen').textContent = relativeTime(agent.last_seen || 0);

    // Set color
    document.getElementById('agentDetailHeader').style.borderLeftColor = color;

    // Filter and show recent requests for this agent
    filterAgentRequests(agentId);

    // Load prompt version history
    loadPromptVersions(agentId);

    // Show modal
    modal.classList.add('open');
    overlay.classList.add('open');
}

function closeAgentDetail() {
    const modal = document.getElementById('agentDetailModal');
    const overlay = document.getElementById('agentDetailOverlay');
    if (modal) modal.classList.remove('open');
    if (overlay) overlay.classList.remove('open');
}

function filterAgentRequests(agentId) {
    const list = document.getElementById('agentRequestsList');
    if (!list) return;

    list.innerHTML = '<div style="text-align:center;color:var(--text-muted);padding:20px;">Loading\u2026</div>';

    // Fetch latest requests from the database API (not DOM)
    fetch(`/api/agents/${encodeURIComponent(agentId)}/requests?limit=20`)
        .then(r => r.ok ? r.json() : null)
        .then(data => {
            const requests = (data && data.requests) || [];
            if (requests.length === 0) {
                list.innerHTML = '<div style="text-align:center;color:var(--text-muted);padding:20px;">No requests yet</div>';
                return;
            }
            list.innerHTML = requests.map(req => {
                const provider = (req.provider || 'openai').toLowerCase();
                const providerLabel = PROVIDER_LABELS[provider] || provider;
                const costStr = (req.cost || 0) < 0.0001 ? '<$0.0001' : `$${req.cost.toFixed(4)}`;
                const latencyStr = (req.latency_ms || 0) >= 1000
                    ? `${(req.latency_ms / 1000).toFixed(1)}s` : `${req.latency_ms || 0}ms`;
                const status = req.status_code || 0;
                const statusClass = status >= 500 ? 'status-5xx' : status >= 400 ? 'status-4xx' : '';
                const statusBadge = statusClass
                    ? `<span class="badge" style="font-size:9px;color:var(--red);">${status}</span>` : '';
                const cacheBadge = req.cache_hit ? '<span class="badge badge-cached" style="font-size:9px;">cached</span>' : '';

                return `
                    <div class="agent-request-item">
                        <div class="agent-request-header">
                            <span class="agent-request-time">${relativeTime(req.timestamp || 0)}</span>
                            <span class="agent-request-cost">${escapeHtml(costStr)}</span>
                        </div>
                        <div class="agent-request-model">${escapeHtml(providerLabel)} \u00B7 ${escapeHtml(req.model || '')} \u00B7 ${escapeHtml(latencyStr)} ${statusBadge}${cacheBadge}</div>
                        <div class="agent-request-prompt">${escapeHtml(req.prompt_preview || '')}</div>
                    </div>
                `;
            }).join('');
        })
        .catch(() => {
            list.innerHTML = '<div style="text-align:center;color:var(--text-muted);padding:20px;">Failed to load requests</div>';
        });
}

// ── Escape helper ─────────────────────────────────────────────────────────────
function escapeHtml(str) {
    return String(str)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;');
}

// Format session agents: renders comma-separated agent IDs as individual badges
function formatSessionAgents(agentIdStr) {
    if (!agentIdStr) return '\u2014';
    const agents = agentIdStr.split(',').filter(Boolean);
    if (agents.length === 0) return '\u2014';
    if (agents.length === 1) return escapeHtml(agents[0]);
    return agents.map(a =>
        `<span class="badge badge-agent" style="font-size:10px;margin-right:3px;">${escapeHtml(a.trim())}</span>`
    ).join('');
}

// ── Chart toggle ──────────────────────────────────────────────────────────────
let chartCollapsed = false;

function toggleChartSection() {
    chartCollapsed = !chartCollapsed;
    const chartWrap = document.getElementById('chartWrap');
    const chevron = document.getElementById('chartChevron');

    if (chartCollapsed) {
        chartWrap.style.display = 'none';
        if (chevron) chevron.textContent = '▶';
    } else {
        chartWrap.style.display = 'block';
        if (chevron) chevron.textContent = '▼';
    }
}

// ── Agent search filter ──────────────────────────────────────────────────────
function filterAgentTable(query) {
    const container = document.getElementById('agentsGroupContainer');
    if (!container) return;

    container.querySelectorAll('.app-group').forEach(group => {
        const rows = group.querySelectorAll('tr.agent-row');
        let visibleCount = 0;

        rows.forEach(row => {
            const agentId = (row.dataset.agentId || '').toLowerCase();
            const match = !query || agentId.includes(query);
            row.classList.toggle('hidden', !match);
            if (match) visibleCount++;
        });

        // Hide entire group if no rows match
        group.classList.toggle('hidden', visibleCount === 0);
    });
}

// ── Cache stats ───────────────────────────────────────────────────────────────
function fetchCacheStats() {
    fetch('/api/cache')
        .then(r => r.ok ? r.json() : null)
        .then(data => {
            if (!data) return;
            const el = document.getElementById('cacheHitRate');
            if (!el) return;

            // API returns nested { exact: {...}, semantic: {...} }
            const exact = data.exact || data; // fallback for flat format
            const semantic = data.semantic || {};

            if (!exact.enabled) {
                el.textContent = 'off';
                el.title = 'Cache disabled';
                return;
            }

            const exactHits = exact.hits || 0;
            const exactMisses = exact.misses || 0;
            const semHits = semantic.hits || 0;
            const semMisses = semantic.misses || 0;
            const totalHits = exactHits + semHits;
            const totalRequests = exactHits + exactMisses; // misses are counted once (semantic miss = exact miss)

            if (totalRequests === 0) {
                el.textContent = '—';
                el.title = 'No requests yet';
            } else {
                const rate = ((totalHits / totalRequests) * 100).toFixed(1);
                el.textContent = `${rate}%`;
                const costSaved = (exact.cost_saved || 0);
                el.title = `Exact: ${exactHits} hits · Semantic: ${semHits} hits · ${exact.entries} entries · $${costSaved.toFixed(4)} saved`;
            }
        })
        .catch(err => console.warn('[Dashboard] API error:', err.message));
}

// ── Cost Savings Analytics ────────────────────────────────────────────────────
function fetchSavingsData() {
    Promise.all([
        fetch('/api/analytics/savings').then(r => r.ok ? r.json() : null),
        fetch('/api/settings').then(r => r.ok ? r.json() : null),
    ]).then(([data, settings]) => {
        if (!data) return;

        const totalEl = document.getElementById('savingsTotal');
        if (totalEl) totalEl.textContent = `$${(data.total_cost_saved || 0).toFixed(4)}`;

        const cacheEl = document.getElementById('savingsCache');
        if (cacheEl) cacheEl.textContent = `$${(data.cache_cost_saved || 0).toFixed(4)}`;

        const cacheDetail = document.getElementById('savingsCacheDetail');
        if (cacheDetail) cacheDetail.textContent = `${(data.cache_hits || 0).toLocaleString()} cache hits`;

        const rulesEl = document.getElementById('savingsRules');
        if (rulesEl) rulesEl.textContent = `$${(data.rules_cost_saved || 0).toFixed(4)}`;

        const rulesDetail = document.getElementById('savingsRulesDetail');
        if (rulesDetail) rulesDetail.textContent = `${(data.rules_blocked || 0).toLocaleString()} blocked`;

        const fallbackEl = document.getElementById('savingsFallback');
        if (fallbackEl) fallbackEl.textContent = `${(data.fallback_count || 0).toLocaleString()}`;

        const fallbackDetail = document.getElementById('savingsFallbackDetail');
        if (fallbackDetail) fallbackDetail.textContent = 'requests rescued';

        const piiEl = document.getElementById('savingsPII');
        if (piiEl) piiEl.textContent = `${(data.pii_items_redacted || 0).toLocaleString()}`;

        const piiDetail = document.getElementById('savingsPIIDetail');
        if (piiDetail) piiDetail.textContent = `${(data.pii_request_count || 0).toLocaleString()} requests`;

        // Memory stats
        if (data.memory) {
            const memEl = document.getElementById('savingsMemory');
            if (memEl) memEl.textContent = `${(data.memory.hits || 0).toLocaleString()} hits`;

            const memDetail = document.getElementById('savingsMemoryDetail');
            if (memDetail) {
                const rate = data.memory.hit_rate ? (data.memory.hit_rate * 100).toFixed(0) + '% hit rate' : '0 lookups';
                memDetail.textContent = `${(data.memory.lookups || 0).toLocaleString()} lookups, ${rate}`;
            }
        }

        // Render cost-saving recommendations based on disabled features
        renderSavingsTips(settings);
    }).catch(err => console.warn('[Dashboard] API error:', err.message));
}

function renderSavingsTips(settings) {
    const container = document.getElementById('savingsTips');
    if (!container || !settings) return;

    const tips = [];

    if (!settings.cache_enabled) {
        tips.push({
            text: 'Response cache is disabled. Enable it to avoid paying for repeated identical requests.',
            section: 'intelligence',
        });
    }

    if (!settings.semantic_cache_enabled) {
        tips.push({
            text: 'Semantic cache is off. It detects similar prompts and serves cached responses — can save 20-40% on repetitive workloads.',
            section: 'intelligence',
        });
    }

    if (!settings.memory_enabled) {
        tips.push({
            text: 'Agent memory is disabled. It remembers context across requests so your prompts can be shorter and more efficient.',
            section: 'intelligence',
        });
    }

    if (!settings.fallback_enabled) {
        tips.push({
            text: 'Provider fallback is off. When a provider fails, fallback routes to an alternative — avoiding wasted spend on retries.',
            section: 'reliability',
        });
    }

    if (!settings.pii_enabled) {
        tips.push({
            text: 'PII redaction is disabled. Enable it to prevent sensitive data from being sent to LLM providers.',
            section: 'security',
        });
    }

    if (tips.length === 0) {
        container.style.display = 'none';
        return;
    }

    container.style.display = '';
    container.innerHTML = tips.map(tip =>
        `<div class="savings-tip">
            <div class="savings-tip-dot"></div>
            <div class="savings-tip-text">${tip.text}</div>
            <a class="savings-tip-action" href="/settings#${tip.section}">Enable</a>
        </div>`
    ).join('');
}

// ── Security Analytics ────────────────────────────────────────────────────────
let securityRefreshTimer = null;
let secTimelineChart = null;
let secDonutChart = null;

const SEC_CAT_PALETTE = [
    '#2f81f7', '#3fb950', '#fb923c', '#f85149',
    '#bc8cff', '#4ade80', '#fbbf24', '#38bdf8',
    '#e879f9', '#34d399', '#f97316', '#a78bfa',
];

const SEC_CAT_BADGE_CLASS = {
    email: 'badge-cat-email',
    phone: 'badge-cat-phone',
    ssn: 'badge-cat-ssn',
    credit_card: 'badge-cat-credit_card',
    person_name: 'badge-cat-person_name',
    iban: 'badge-cat-iban',
    ip_address: 'badge-cat-ip_address',
};

function loadSecurityStats() {
    Promise.all([
        fetch('/api/security/pii').then(r => r.ok ? r.json() : null),
        fetch('/api/security/pii/details').then(r => r.ok ? r.json() : null),
        fetch('/api/security/nemoguard').then(r => r.ok ? r.json() : null),
        fetch('/api/security/nemoguard/details').then(r => r.ok ? r.json() : null),
    ]).then(([summary, details, ng, ngDetails]) => {
        renderNemoGuard(ng, ngDetails);
        if (!summary) return;

        // ── Status badge ──────────────────────────────────────────
        const statusBadge = document.getElementById('securityStatusBadge');
        if (statusBadge) {
            if (summary.pii_enabled) {
                statusBadge.textContent = `${summary.pii_mode || 'redact'} mode`;
                statusBadge.style.background = 'var(--green-subtle)';
                statusBadge.style.color = 'var(--green)';
                statusBadge.style.borderColor = 'rgba(63,185,80,0.3)';
            } else {
                statusBadge.textContent = 'disabled';
                statusBadge.style.background = 'var(--red-subtle)';
                statusBadge.style.color = 'var(--red)';
                statusBadge.style.borderColor = 'rgba(248,81,73,0.3)';
            }
        }

        // ── Stat cards ────────────────────────────────────────────
        const totalItems = summary.total_items_redacted || 0;
        const flagged = summary.requests_with_redactions || 0;
        const scanned = summary.total_requests_scanned || 0;
        const cats = summary.categories || {};
        const catEntries = Object.entries(cats).sort((a, b) => b[1] - a[1]);
        const rate = scanned > 0 ? ((flagged / scanned) * 100) : 0;

        setText('secTotalItems', totalItems.toLocaleString());
        setText('secTotalRequests', flagged.toLocaleString());
        setText('secFlaggedPct', scanned > 0 ? `${rate.toFixed(1)}% of ${scanned.toLocaleString()} scanned` : '');
        setText('secDetectionRate', rate.toFixed(1) + '%');
        setText('secCategoryCountCard', catEntries.length.toString());
        setText('secTopCategory', catEntries.length > 0 ? catEntries[0][0].replace(/_/g, ' ') : '\u2014');
        setText('secTopCategoryCount', catEntries.length > 0 ? catEntries[0][1].toLocaleString() + ' detections' : '');
        setText('secCategoryCount', `${catEntries.length} categor${catEntries.length === 1 ? 'y' : 'ies'}`);

        // ── Category donut chart ──────────────────────────────────
        renderSecCategoryDonut(catEntries);

        // ── Details (timeline, agents, recent) ────────────────────
        if (details) {
            renderSecTimeline(details.timeline || []);
            renderSecAgentBreakdown(details.by_agent || []);
            renderSecRecentDetections(details.recent || []);
        }
    }).catch(err => console.warn('[Dashboard] API error:', err.message));
}

function renderSecTimeline(timeline) {
    const ctx = document.getElementById('secTimelineChart');
    if (!ctx) return;

    const labels = timeline.map(b => {
        const d = new Date(b.hour * 1000);
        return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) + ' ' +
               d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
    });
    const data = timeline.map(b => b.count);

    if (secTimelineChart) {
        secTimelineChart.data.labels = labels;
        secTimelineChart.data.datasets[0].data = data;
        secTimelineChart.update('none');
        return;
    }

    secTimelineChart = new Chart(ctx.getContext('2d'), {
        type: 'bar',
        data: {
            labels,
            datasets: [{
                label: 'Items Redacted',
                data,
                backgroundColor: 'rgba(47,129,247,0.5)',
                borderColor: '#2f81f7',
                borderWidth: 1,
                borderRadius: 2,
            }],
        },
        options: {
            responsive: true,
            maintainAspectRatio: false,
            plugins: {
                legend: { display: false },
                tooltip: {
                    backgroundColor: '#161b22',
                    borderColor: '#30363d',
                    borderWidth: 1,
                    titleColor: '#e6edf3',
                    bodyColor: '#e6edf3',
                    padding: 8,
                    cornerRadius: 6,
                },
            },
            scales: {
                x: {
                    grid: { display: false },
                    ticks: {
                        color: '#484f58',
                        font: { size: 10 },
                        maxTicksLimit: 12,
                        maxRotation: 0,
                    },
                    border: { color: '#21262d' },
                },
                y: {
                    beginAtZero: true,
                    grid: { color: 'rgba(48,54,61,0.4)' },
                    ticks: {
                        color: '#484f58',
                        font: { size: 10 },
                        precision: 0,
                    },
                    border: { display: false },
                },
            },
        },
    });
}

function renderSecCategoryDonut(catEntries) {
    const ctx = document.getElementById('secCategoryDonut');
    if (!ctx) return;

    const labels = catEntries.map(([cat]) => cat.replace(/_/g, ' '));
    const data = catEntries.map(([, count]) => count);
    const colors = catEntries.map((_, i) => SEC_CAT_PALETTE[i % SEC_CAT_PALETTE.length]);

    if (secDonutChart) {
        secDonutChart.data.labels = labels;
        secDonutChart.data.datasets[0].data = data;
        secDonutChart.data.datasets[0].backgroundColor = colors;
        secDonutChart.update('none');
    } else if (catEntries.length > 0) {
        secDonutChart = new Chart(ctx.getContext('2d'), {
            type: 'doughnut',
            data: {
                labels,
                datasets: [{
                    data,
                    backgroundColor: colors,
                    borderWidth: 0,
                    hoverBorderWidth: 2,
                    hoverBorderColor: '#e6edf3',
                }],
            },
            options: {
                responsive: true,
                maintainAspectRatio: false,
                cutout: '65%',
                plugins: {
                    legend: { display: false },
                    tooltip: {
                        backgroundColor: '#161b22',
                        borderColor: '#30363d',
                        borderWidth: 1,
                        titleColor: '#e6edf3',
                        bodyColor: '#e6edf3',
                        padding: 8,
                        cornerRadius: 6,
                    },
                },
            },
        });
    }

    // Legend
    const legend = document.getElementById('secCategoryLegend');
    if (legend) {
        let html = '<div class="sec-legend">';
        catEntries.forEach(([cat, count], i) => {
            const color = SEC_CAT_PALETTE[i % SEC_CAT_PALETTE.length];
            html += `<div class="sec-legend-item">
                <span class="sec-legend-dot" style="background:${color};"></span>
                <span>${escapeHtml(cat.replace(/_/g, ' '))}</span>
                <span class="sec-legend-count">${count.toLocaleString()}</span>
            </div>`;
        });
        html += '</div>';
        legend.innerHTML = html;
    }
}

function renderSecAgentBreakdown(agents) {
    const container = document.getElementById('secAgentBreakdown');
    const empty = document.getElementById('secAgentEmpty');
    if (!container) return;

    setText('secAgentCount', `${agents.length} agent${agents.length !== 1 ? 's' : ''}`);

    if (agents.length === 0) {
        if (empty) empty.style.display = '';
        return;
    }
    if (empty) empty.style.display = 'none';

    const maxVal = agents[0].items_redacted;
    let html = '';
    agents.forEach((a, i) => {
        const pct = maxVal > 0 ? (a.items_redacted / maxVal) * 100 : 0;
        const color = SEC_CAT_PALETTE[i % SEC_CAT_PALETTE.length];
        const name = a.agent_id || 'unknown';
        html += `<div class="sec-agent-bar">
            <span class="sec-agent-name" title="${escapeHtml(name)}">${escapeHtml(name)}</span>
            <div class="sec-agent-track">
                <div class="sec-agent-fill" style="width:${pct.toFixed(1)}%;background:${color};"></div>
            </div>
            <span class="sec-agent-count">${a.items_redacted.toLocaleString()}</span>
        </div>`;
    });
    container.innerHTML = html;
}

function renderSecRecentDetections(recent) {
    const body = document.getElementById('secRecentBody');
    if (!body) return;

    setText('secRecentCount', `${recent.length} detection${recent.length !== 1 ? 's' : ''}`);

    if (recent.length === 0) {
        body.innerHTML = '<tr class="feed-empty"><td colspan="6">No PII detections yet. Enable PII redaction in Settings, then send requests through the proxy.</td></tr>';
        return;
    }

    let html = '';
    recent.forEach(r => {
        const d = new Date(r.timestamp * 1000);
        const time = d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) + ' ' +
                     d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
        const agent = r.agent_id || 'unknown';
        const preview = r.prompt_preview.length > 60 ? r.prompt_preview.slice(0, 60) + '\u2026' : r.prompt_preview;
        const cats = r.pii_categories || {};
        let badges = '';
        Object.keys(cats).forEach(cat => {
            const cls = SEC_CAT_BADGE_CLASS[cat] || 'badge-cat-default';
            badges += `<span class="badge-cat ${cls}">${escapeHtml(cat.replace(/_/g, ' '))}</span>`;
        });

        html += `<tr class="feed-row">
            <td style="white-space:nowrap;font-size:11px;color:var(--text-muted);">${escapeHtml(time)}</td>
            <td><span class="badge badge-agent" style="font-size:10px;">${escapeHtml(agent)}</span></td>
            <td style="font-size:11px;color:var(--text-secondary);">${escapeHtml(r.model)}</td>
            <td style="max-width:200px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:11px;color:var(--text-muted);" title="${escapeHtml(r.prompt_preview)}">${escapeHtml(preview)}</td>
            <td><div class="sec-cat-badges">${badges}</div></td>
            <td style="text-align:right;font-family:var(--font-mono);font-size:12px;">${r.pii_redacted_count}</td>
        </tr>`;
    });
    body.innerHTML = html;
}

// ── NeMo Guard (jailbreak) panel ──────────────────────────────────────────
function renderNemoGuard(summary, details) {
    if (summary) {
        setText('secJailbreaks', (summary.jailbreaks_detected || 0).toLocaleString());
        setText('secJailbreaksBlocked', (summary.blocked || 0).toLocaleString());
        const st = document.getElementById('nemoGuardStatus');
        if (st) st.textContent = summary.enabled ? `${summary.mode || 'block'} mode` : 'disabled';
    }
    renderNemoGuardRecent((details && details.recent) || []);
}

function renderNemoGuardRecent(recent) {
    const body = document.getElementById('nemoGuardRecentBody');
    if (!body) return;
    setText('nemoGuardRecentCount', `${recent.length} flagged`);
    if (recent.length === 0) {
        body.innerHTML = '<tr class="feed-empty"><td colspan="6">No jailbreak attempts. Set CONFIG_NEMOGUARD_URL to enable detection.</td></tr>';
        return;
    }
    let html = '';
    recent.forEach(r => {
        const d = new Date(r.timestamp * 1000);
        const time = d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) + ' ' +
                     d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
        const agent = r.agent_id || 'unknown';
        const pp = r.prompt_preview || '';
        const preview = pp.length > 60 ? pp.slice(0, 60) + '…' : pp;
        const blocked = r.status_code === 403;
        const statusBadge = blocked
            ? '<span class="badge badge-jailbreak">blocked</span>'
            : '<span class="badge" style="font-size:10px;">flagged</span>';
        const score = (typeof r.score === 'number' && r.score > 0) ? r.score.toFixed(2) : '—';
        html += `<tr class="feed-row">
            <td style="white-space:nowrap;font-size:11px;color:var(--text-muted);">${escapeHtml(time)}</td>
            <td><span class="badge badge-agent" style="font-size:10px;">${escapeHtml(agent)}</span></td>
            <td style="font-size:11px;color:var(--text-secondary);">${escapeHtml(r.model || '')}</td>
            <td style="max-width:200px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:11px;color:var(--text-muted);" title="${escapeHtml(pp)}">${escapeHtml(preview)}</td>
            <td style="text-align:right;font-family:var(--font-mono);font-size:12px;">${score}</td>
            <td style="text-align:right;">${statusBadge}</td>
        </tr>`;
    });
    body.innerHTML = html;
}

function startSecurityRefresh() {
    stopSecurityRefresh();
    securityRefreshTimer = setInterval(loadSecurityStats, 30_000);
}

function stopSecurityRefresh() {
    if (securityRefreshTimer) {
        clearInterval(securityRefreshTimer);
        securityRefreshTimer = null;
    }
}

// ── Sessions view ─────────────────────────────────────────────────────────────
let sessionsPage   = 0;
let sessionsLimit  = 50;
let sessionsTotal  = 0;
let sessionsFilter = '';
let expandedSessionId = null;

function loadSessions() {
    const params = new URLSearchParams({ limit: sessionsLimit, offset: sessionsPage * sessionsLimit });
    if (sessionsFilter) params.set('app_name', sessionsFilter);

    fetch(`/api/sessions?${params}`)
        .then(r => r.ok ? r.json() : null)
        .then(data => {
            if (!data) return;
            sessionsTotal = data.total || 0;
            renderSessionsTable(data.sessions || []);
            updateSessionsPagination();
            updateNavBadge('nav-badge-sessions', sessionsTotal);
        })
        .catch(err => console.warn('[Dashboard] API error:', err.message));
}

function renderSessionsTable(sessions) {
    const body = document.getElementById('sessionsBody');
    if (!body) return;

    const countEl = document.getElementById('sessionsCount');
    if (countEl) countEl.textContent = `${sessionsTotal} session${sessionsTotal !== 1 ? 's' : ''}`;

    if (sessions.length === 0) {
        body.innerHTML = '<tr class="feed-empty"><td colspan="10">No sessions found.</td></tr>';
        return;
    }

    body.innerHTML = sessions.map(s => {
        const sid = s.session_id || '';
        const shortId = sid.length > 12 ? sid.substring(0, 12) + '\u2026' : sid;
        const costStr = (s.total_cost || 0) < 0.0001 ? '<$0.0001' : `$${s.total_cost.toFixed(4)}`;
        const dur = s.duration_sec || 0;
        const durStr = dur >= 3600 ? `${Math.floor(dur/3600)}h ${Math.floor((dur%3600)/60)}m`
                     : dur >= 60 ? `${Math.floor(dur/60)}m ${dur%60}s`
                     : `${dur}s`;
        const isExpanded = expandedSessionId === sid;

        return `
            <tr class="feed-row session-row${isExpanded ? ' session-expanded' : ''}" data-session-id="${escapeHtml(sid)}" style="cursor:pointer;">
                <td style="width:24px;text-align:center;">
                    <span class="session-chevron" style="display:inline-block;transition:transform 0.15s;transform:rotate(${isExpanded ? '90' : '0'}deg);color:var(--text-muted);font-size:10px;">\u25B6</span>
                </td>
                <td><span class="badge badge-agent" style="border-color:var(--accent);color:var(--accent);font-family:var(--font-mono);font-size:10px;" title="${escapeHtml(sid)}">${escapeHtml(shortId)}</span></td>
                <td>${escapeHtml(s.app_name || '\u2014')}</td>
                <td>${formatSessionAgents(s.agent_id)}</td>
                <td class="col-num">${(s.request_count || 0).toLocaleString()}</td>
                <td class="col-cost">${costStr}</td>
                <td class="col-num">${(s.input_tokens || 0).toLocaleString()}</td>
                <td class="col-num">${(s.output_tokens || 0).toLocaleString()}</td>
                <td class="col-time">${durStr}</td>
                <td class="col-time" data-ts="${s.last_seen || 0}">${relativeTime(s.last_seen || 0)}</td>
            </tr>`;
    }).join('');

    // Bind click handlers
    body.querySelectorAll('.session-row').forEach(row => {
        row.addEventListener('click', () => {
            const sid = row.dataset.sessionId;
            if (expandedSessionId === sid) {
                closeSessionDetail();
            } else {
                expandSessionDetail(sid);
            }
        });
    });
}

function expandSessionDetail(sessionId) {
    expandedSessionId = sessionId;

    // Highlight the row
    document.querySelectorAll('.session-row').forEach(r => {
        const isTarget = r.dataset.sessionId === sessionId;
        r.classList.toggle('session-expanded', isTarget);
        const chev = r.querySelector('.session-chevron');
        if (chev) chev.style.transform = isTarget ? 'rotate(90deg)' : 'rotate(0deg)';
    });

    // Show session detail modal
    const modal = document.getElementById('sessionDetailCard');
    const overlay = document.getElementById('sessionDetailOverlay');
    if (modal) modal.classList.add('open');
    if (overlay) overlay.classList.add('open');

    const shortId = sessionId.length > 20 ? sessionId.substring(0, 20) + '\u2026' : sessionId;
    setText('sessionDetailTitle', `Session: ${shortId}`);
    setText('sessionDetailMeta', 'Loading\u2026');
    document.getElementById('sessionDetailBody').innerHTML =
        '<tr><td colspan="12" style="text-align:center;padding:20px;color:var(--text-muted);">Loading requests\u2026</td></tr>';

    fetch(`/api/sessions/${encodeURIComponent(sessionId)}/requests`)
        .then(r => r.ok ? r.json() : null)
        .then(data => {
            if (!data) return;
            renderSessionDetail(data);
        })
        .catch(err => console.warn('[Dashboard] API error:', err.message));
}

function renderSessionDetail(data) {
    const session = data.session || {};
    const requests = data.requests || [];

    setText('sessionDetailMeta', `${requests.length} request${requests.length !== 1 ? 's' : ''}`);
    setText('sdRequests', requests.length.toLocaleString());
    setText('sdCost', (session.total_cost || 0) < 0.0001 ? '<$0.0001' : `$${(session.total_cost || 0).toFixed(4)}`);
    setText('sdInTokens', (session.input_tokens || 0).toLocaleString());
    setText('sdOutTokens', (session.output_tokens || 0).toLocaleString());

    const body = document.getElementById('sessionDetailBody');
    if (!body) return;

    if (requests.length === 0) {
        body.innerHTML = '<tr><td colspan="12" style="text-align:center;padding:20px;color:var(--text-muted);">No requests in this session.</td></tr>';
        return;
    }

    body.innerHTML = requests.map((req, idx) => {
        const provider = (req.provider || 'openai').toLowerCase();
        const providerLabel = PROVIDER_LABELS[provider] || provider;
        const agentId = req.agent_id && req.agent_id !== 'unknown' ? req.agent_id : null;
        const agentBadge = agentId
            ? `<span class="badge badge-agent" style="border-color:${agentColor(agentId)};color:${agentColor(agentId)}">${escapeHtml(agentId)}</span>`
            : '<span class="badge badge-agent-unknown">\u2014</span>';

        const costStr = (req.cost || 0) < 0.0001 ? '<$0.0001' : `$${req.cost.toFixed(4)}`;
        const latencyStr = (req.latency_ms || 0) >= 1000
            ? `${(req.latency_ms / 1000).toFixed(1)}s` : `${req.latency_ms || 0}ms`;

        const status = req.status_code || 0;
        let statusClass = '';
        if      (status >= 500) statusClass = 'status-5xx';
        else if (status >= 400) statusClass = 'status-4xx';
        else if (status >= 200) statusClass = 'status-2xx';
        const statusCell = status ? `<span class="col-status ${statusClass}">${status}</span>` : '\u2014';

        const cacheBadge = req.cache_hit ? '<span class="badge badge-cached">cached</span>' : '';
        const piiBadge = (req.pii_redacted || 0) > 0 ? `<span class="badge badge-pii">PII (${req.pii_redacted})</span>` : '';
        const jailbreakBadge = req.jailbreak_detected ? '<span class="badge badge-jailbreak">Jailbreak</span>' : '';
        const loopBadge = req.loop_detected ? `<span class="badge badge-loop-${req.loop_severity || 'low'}">${req.loop_severity || 'loop'}</span>` : '';
        const errorBadge = req.error_message ? '<span class="badge badge-error">err</span>' : '';
        const fallbackBadge = req.fallback_used ? '<span class="badge badge-fallback">fallback</span>' : '';

        return `
            <tr class="feed-row">
                <td style="color:var(--text-muted);font-size:11px;">${idx + 1}</td>
                <td class="col-ts" data-ts="${req.timestamp || 0}">${relativeTime(req.timestamp || 0)}</td>
                <td class="col-agent">${agentBadge}</td>
                <td class="col-provider"><span class="badge badge-${provider}">${providerLabel}</span></td>
                <td class="col-model">${escapeHtml(req.model || '')}</td>
                <td class="col-prompt"><span class="prompt-text">${escapeHtml(req.prompt_preview || '')}</span></td>
                <td class="col-num">${(req.input_tokens || 0).toLocaleString()}</td>
                <td class="col-num">${(req.output_tokens || 0).toLocaleString()}</td>
                <td class="col-cost">${costStr}</td>
                <td class="col-latency">${latencyStr}</td>
                <td class="col-status">${statusCell}</td>
                <td class="col-flags">${cacheBadge}${piiBadge}${jailbreakBadge}${loopBadge}${errorBadge}${fallbackBadge}</td>
            </tr>`;
    }).join('');
}

function closeSessionDetail() {
    expandedSessionId = null;
    document.querySelectorAll('.session-row').forEach(r => {
        r.classList.remove('session-expanded');
        const chev = r.querySelector('.session-chevron');
        if (chev) chev.style.transform = 'rotate(0deg)';
    });
    const modal = document.getElementById('sessionDetailCard');
    const overlay = document.getElementById('sessionDetailOverlay');
    if (modal) modal.classList.remove('open');
    if (overlay) overlay.classList.remove('open');
}

function updateSessionsPagination() {
    const totalPages = Math.ceil(sessionsTotal / sessionsLimit);
    const prevBtn = document.getElementById('sessionsPrevBtn');
    const nextBtn = document.getElementById('sessionsNextBtn');
    const info = document.getElementById('sessionsPageInfo');
    const wrap = document.getElementById('sessionsPagination');

    if (wrap) wrap.style.display = sessionsTotal > sessionsLimit ? 'flex' : 'none';
    if (prevBtn) prevBtn.disabled = sessionsPage <= 0;
    if (nextBtn) nextBtn.disabled = sessionsPage >= totalPages - 1;
    if (info) info.textContent = `Page ${sessionsPage + 1} of ${totalPages || 1}`;
}

// ── Prompt Version Tracking ────────────────────────────────────────────────────
let promptVersionsCache = []; // current agent's versions (for diff navigation)
let promptVersionsAgentId = '';

async function loadPromptVersions(agentId) {
    promptVersionsAgentId = agentId;
    const container = document.getElementById('promptVersionsList');
    const countEl = document.getElementById('promptVersionCount');
    if (!container) return;

    container.innerHTML = '<div style="text-align:center;color:var(--text-muted);padding:16px;">Loading…</div>';

    try {
        const resp = await fetch(`/api/prompts/${encodeURIComponent(agentId)}/history?limit=20`);
        if (!resp.ok) throw new Error('fetch failed');
        const data = await resp.json();
        const versions = data.versions || [];
        promptVersionsCache = versions;
        if (countEl) countEl.textContent = `${data.total || 0} version${data.total === 1 ? '' : 's'}`;

        if (versions.length === 0) {
            container.innerHTML = '<div style="text-align:center;color:var(--text-muted);padding:20px;font-size:12px;">No prompt versions tracked yet.<br>System prompts are automatically versioned on each request.</div>';
            return;
        }

        container.innerHTML = '';
        versions.forEach((v, idx) => {
            const num = data.total - (data.offset || 0) - idx;
            const time = relativeTime(v.created_at);
            const hash = (v.content_hash || '').substring(0, 8);
            const isFirst = v.previous_hash === '';
            const item = document.createElement('div');
            item.className = 'prompt-version-item';
            item.innerHTML = `
                <div class="prompt-version-badge">v${num}</div>
                <div class="prompt-version-info">
                    <div class="prompt-version-label">
                        ${isFirst ? 'Initial version' : 'Prompt updated'}
                        ${isFirst ? '<span class="prompt-version-new">initial</span>' : ''}
                    </div>
                    <div class="prompt-version-meta">
                        <span>${time}</span>
                        <span class="prompt-version-hash">${hash}</span>
                        <span>${v.provider || ''}${v.model ? ' / ' + v.model : ''}</span>
                        <span>${v.content_len || 0} chars</span>
                    </div>
                </div>
            `;
            item.addEventListener('click', () => openPromptVersion(agentId, v.id, idx));
            container.appendChild(item);
        });
    } catch (e) {
        container.innerHTML = '<div style="text-align:center;color:var(--text-muted);padding:16px;">Failed to load prompt versions</div>';
    }
}

async function openPromptVersion(agentId, versionId, idx) {
    const overlay = document.getElementById('promptDiffOverlay');
    const modal = document.getElementById('promptDiffModal');
    const titleEl = document.getElementById('promptDiffTitle');
    const content = document.getElementById('promptDiffContent');
    const metaEl = document.getElementById('promptDiffMeta');
    if (!modal || !content) return;

    const version = promptVersionsCache[idx];
    if (!version) return;

    // Check if there's a previous version to diff against
    const prevIdx = idx + 1; // versions are newest-first
    const hasPrev = prevIdx < promptVersionsCache.length;
    const hasNewer = idx > 0;

    // Update navigation
    const prevBtn = document.getElementById('promptDiffPrevBtn');
    const nextBtn = document.getElementById('promptDiffNextBtn');
    if (prevBtn) { prevBtn.disabled = !hasNewer; }
    if (nextBtn) { nextBtn.disabled = !hasPrev; }

    // Remove old listeners and add new
    if (prevBtn) {
        const newPrev = prevBtn.cloneNode(true);
        prevBtn.parentNode.replaceChild(newPrev, prevBtn);
        newPrev.disabled = !hasNewer;
        if (hasNewer) newPrev.addEventListener('click', () => openPromptVersion(agentId, promptVersionsCache[idx - 1].id, idx - 1));
    }
    if (nextBtn) {
        const newNext = nextBtn.cloneNode(true);
        nextBtn.parentNode.replaceChild(newNext, nextBtn);
        newNext.disabled = !hasPrev;
        if (hasPrev) newNext.addEventListener('click', () => openPromptVersion(agentId, promptVersionsCache[prevIdx].id, prevIdx));
    }

    // Show modal
    if (overlay) overlay.style.display = 'block';
    modal.style.display = 'block';

    content.innerHTML = '<div style="text-align:center;color:var(--text-muted);padding:24px;">Loading…</div>';

    try {
        // Fetch full version content
        const resp = await fetch(`/api/prompts/${encodeURIComponent(agentId)}/versions/${versionId}`);
        if (!resp.ok) throw new Error('fetch failed');
        const v = await resp.json();

        const totalVersions = promptVersionsCache.length;
        const vNum = totalVersions - idx;
        const time = new Date(v.created_at * 1000).toLocaleString();

        if (titleEl) titleEl.textContent = `Prompt v${vNum} — ${agentId}`;
        if (metaEl) metaEl.textContent = `${time} · ${v.provider || ''} / ${v.model || ''} · ${(v.content || '').length} chars`;

        if (hasPrev) {
            // Show diff view
            const prevVersion = promptVersionsCache[prevIdx];
            const diffResp = await fetch(`/api/prompts/${encodeURIComponent(agentId)}/diff/${prevVersion.id}/${versionId}`);
            if (diffResp.ok) {
                const diff = await diffResp.json();
                renderDiff(content, diff, v.content);
                return;
            }
        }

        // No previous version — show full content
        content.innerHTML = `
            <div class="diff-summary">
                <span>Initial version</span>
                <span class="diff-stat diff-stat-added">+${(v.content || '').split('\\n').length} lines</span>
            </div>
            <div class="prompt-content-viewer">${escapeHtml(v.content || '')}</div>
        `;
    } catch (e) {
        content.innerHTML = '<div style="text-align:center;color:var(--text-muted);padding:24px;">Failed to load version</div>';
    }
}

function renderDiff(container, diff, fullContent) {
    const added = (diff.diff || []).filter(d => d.type === 'added' || d.type === 'changed').length;
    const removed = (diff.diff || []).filter(d => d.type === 'removed' || d.type === 'changed').length;

    let html = `<div class="diff-summary">
        <span>${diff.changes || 0} change${diff.changes === 1 ? '' : 's'}</span>
        <span class="diff-stat diff-stat-added">+${added}</span>
        <span class="diff-stat diff-stat-removed">-${removed}</span>
        <span>${diff.lines_a || 0} → ${diff.lines_b || 0} lines</span>
    </div>`;

    if (!diff.diff || diff.diff.length === 0) {
        html += '<div style="padding:16px;color:var(--text-muted);text-align:center;">No differences found</div>';
    } else {
        html += '<div style="border-top:1px solid var(--border-subtle);">';
        for (const d of diff.diff) {
            if (d.type === 'changed') {
                html += `<div class="diff-line diff-line-removed">${escapeHtml(d.old || '')}</div>`;
                html += `<div class="diff-line diff-line-added">${escapeHtml(d.new || '')}</div>`;
            } else if (d.type === 'added') {
                html += `<div class="diff-line diff-line-added">${escapeHtml(d.new || '')}</div>`;
            } else if (d.type === 'removed') {
                html += `<div class="diff-line diff-line-removed">${escapeHtml(d.old || '')}</div>`;
            }
        }
        html += '</div>';
    }

    // Also show the full content below
    html += `<div style="margin-top:16px;">
        <div style="font-size:11px;font-weight:600;color:var(--text-secondary);margin-bottom:6px;">Full Content</div>
        <div class="prompt-content-viewer">${escapeHtml(fullContent || '')}</div>
    </div>`;

    container.innerHTML = html;
}

function closePromptDiff() {
    const overlay = document.getElementById('promptDiffOverlay');
    const modal = document.getElementById('promptDiffModal');
    if (overlay) overlay.style.display = 'none';
    if (modal) modal.style.display = 'none';
}

// ── Boot ──────────────────────────────────────────────────────────────────────
window.addEventListener('load', () => {
    initChart();
    fetchCostHistory();   // seed chart from DB before WebSocket events arrive
    connect();
    restoreViewFromHash(); // restore view from URL hash (e.g. /#sessions from settings page)

    // Bind agent search input
    const agentSearchInput = document.getElementById('agentSearchInput');
    if (agentSearchInput) {
        agentSearchInput.addEventListener('input', () => {
            filterAgentTable(agentSearchInput.value.trim().toLowerCase());
        });
    }

    // Fetch cache stats and savings data on load and periodically
    fetchCacheStats();
    fetchSavingsData();
    setInterval(fetchCacheStats, 30_000);
    setInterval(fetchSavingsData, 30_000);

    // Sessions view bindings
    const sessionSearchInput = document.getElementById('sessionSearchInput');
    if (sessionSearchInput) {
        let sessionSearchTimer;
        sessionSearchInput.addEventListener('input', () => {
            clearTimeout(sessionSearchTimer);
            sessionSearchTimer = setTimeout(() => {
                sessionsFilter = sessionSearchInput.value.trim();
                sessionsPage = 0;
                closeSessionDetail();
                loadSessions();
            }, 300);
        });
    }

    const sessionRefreshBtn = document.getElementById('sessionRefreshBtn');
    if (sessionRefreshBtn) sessionRefreshBtn.addEventListener('click', loadSessions);

    const sessionsPrevBtn = document.getElementById('sessionsPrevBtn');
    if (sessionsPrevBtn) sessionsPrevBtn.addEventListener('click', () => {
        if (sessionsPage > 0) { sessionsPage--; closeSessionDetail(); loadSessions(); }
    });
    const sessionsNextBtn = document.getElementById('sessionsNextBtn');
    if (sessionsNextBtn) sessionsNextBtn.addEventListener('click', () => {
        const totalPages = Math.ceil(sessionsTotal / sessionsLimit);
        if (sessionsPage < totalPages - 1) { sessionsPage++; closeSessionDetail(); loadSessions(); }
    });

    const sessionDetailClose = document.getElementById('sessionDetailClose');
    if (sessionDetailClose) sessionDetailClose.addEventListener('click', closeSessionDetail);

    const sessionDetailOverlay = document.getElementById('sessionDetailOverlay');
    if (sessionDetailOverlay) sessionDetailOverlay.addEventListener('click', closeSessionDetail);
});
