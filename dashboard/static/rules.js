// ══════════════════════════════════════════════════════════════════════════════
//  Rules Management UI
// ══════════════════════════════════════════════════════════════════════════════

let currentEditingRule = null;
let rulesData = [];
let ruleHitCounts = {};  // ruleID → {count, last_triggered}

// ── Rule Request Log state ────────────────────────────────────────────────────
let ruleLogCurrentId = null;
let ruleLogCurrentOffset = 0;
let ruleLogCurrentLimit = 20;
let ruleLogCurrentTotal = 0;

// ── Helper: Create modern custom dropdown instead of native select ──────────
function createCustomDropdown(options, selectedValue = '', placeholder = 'Select...', className = '') {
    const wrapper = document.createElement('div');
    wrapper.className = 'custom-dropdown-wrapper ' + className;

    const select = document.createElement('select');
    select.innerHTML = options;
    select.value = selectedValue;
    select.style.display = 'none'; // Hide the native select
    wrapper.appendChild(select);

    // Initialize custom dropdown after a brief delay to ensure DOM is ready
    setTimeout(() => {
        const dropdown = new CustomDropdown(select, {
            searchable: options.split('option').length > 10,
            placeholder: placeholder
        });
    }, 0);

    return wrapper;
}

// ── Supported models — derived from the canonical MODEL_GROUPS in models.js ──
// models.js must be loaded before this file (see index.html script order).
const SUPPORTED_MODELS = MODEL_GROUPS.map(g => ({
    group: g.group,
    models: g.models.map(m => ({ id: m.value, label: m.label, cost: m.cost || '' })),
}));

const REDIRECT_PROVIDERS = [
    { id: 'https://api.openai.com/v1/chat/completions',                label: 'OpenAI',    desc: 'api.openai.com' },
    { id: 'https://api.anthropic.com/v1/messages',                     label: 'Anthropic',  desc: 'api.anthropic.com' },
    { id: 'https://generativelanguage.googleapis.com/v1beta/models/',  label: 'Google Gemini', desc: 'generativelanguage.googleapis.com' },
];

// Build model <option> HTML with optgroups (cached once)
function buildModelOptionsHTML(selectedValue) {
    let html = '<option value="">-- Select model --</option>';
    SUPPORTED_MODELS.forEach(group => {
        html += `<optgroup label="${group.group}">`;
        group.models.forEach(m => {
            const sel = m.id === selectedValue ? ' selected' : '';
            html += `<option value="${m.id}"${sel}>${m.label}  (${m.cost})</option>`;
        });
        html += '</optgroup>';
    });
    if (selectedValue && !SUPPORTED_MODELS.some(g => g.models.some(m => m.id === selectedValue))) {
        html += `<option value="${escapeHtml(selectedValue)}" selected>${escapeHtml(selectedValue)}</option>`;
    }
    html += '<option value="__custom__">Other (type manually)...</option>';
    return html;
}

function buildRedirectOptionsHTML(selectedValue) {
    let html = '<option value="">-- Select provider --</option>';
    REDIRECT_PROVIDERS.forEach(p => {
        const sel = p.id === selectedValue ? ' selected' : '';
        html += `<option value="${p.id}"${sel}>${p.label} — ${p.desc}</option>`;
    });
    if (selectedValue && !REDIRECT_PROVIDERS.some(p => p.id === selectedValue)) {
        html += `<option value="${escapeHtml(selectedValue)}" selected>${escapeHtml(selectedValue)}</option>`;
    }
    html += '<option value="__custom__">Other (type manually)...</option>';
    return html;
}

// Generic handler: when "Other" is chosen, swap select for text input
function handleSelectToInput(selectEl, placeholder) {
    if (selectEl.value !== '__custom__') return;
    const input = document.createElement('input');
    input.type = 'text';
    input.id = selectEl.id;
    input.className = selectEl.className;
    input.placeholder = placeholder;
    input.style.cssText = selectEl.style.cssText || '';
    selectEl.replaceWith(input);
    input.focus();
}

// ── Known values helpers ─────────────────────────────────────────────────────
// `agentState` is defined in app.js and tracks all agents from WebSocket events.
function getKnownAgentIds() {
    if (typeof agentState === 'object' && agentState !== null) {
        return Object.keys(agentState).filter(id => id && id !== 'unknown').sort();
    }
    return [];
}

// Build agent options HTML for dropdowns
function buildAgentOptionsHTML(currentValue) {
    const agents = getKnownAgentIds();
    let html = '<option value="">-- Select agent --</option>';
    agents.forEach(id => {
        const selected = id === currentValue ? ' selected' : '';
        html += `<option value="${escapeHtml(id)}"${selected}>${escapeHtml(id)}</option>`;
    });
    // If editing a rule whose agent isn't in the current list, still show it
    if (currentValue && !agents.includes(currentValue)) {
        html += `<option value="${escapeHtml(currentValue)}" selected>${escapeHtml(currentValue)}</option>`;
    }
    html += '<option value="__custom__">Other (type manually)...</option>';
    return html;
}

// Populate a <select> with known agent IDs, preserving a current value.
// Appends an "Other..." option that swaps the select for a text input.
function populateAgentSelect(selectEl, currentValue) {
    selectEl.innerHTML = buildAgentOptionsHTML(currentValue);
}

// When "Other..." is selected, replace the <select> with a text <input>.
function handleAgentSelectChange(selectEl) {
    if (selectEl.value !== '__custom__') return;
    const input = document.createElement('input');
    input.type = 'text';
    input.className = selectEl.className;
    input.id = selectEl.id;
    input.placeholder = 'Type agent ID...';
    input.style.cssText = selectEl.style.cssText || '';
    selectEl.replaceWith(input);
    input.focus();
}

// ── Template icon map ─────────────────────────────────────────────────────────
const TEMPLATE_ICONS = {
    shield:   '🛡️',
    alert:    '⚠️',
    clock:    '⏱️',
    route:    '🔀',
    stop:     '🛑',
    lock:     '🔒',
    filter:   '🔍',
    switch:   '↔️',
    gauge:    '📊',
    calendar: '📅',
};

// ── Initialize ────────────────────────────────────────────────────────────────
let _rulesUIInitialized = false;

function initRulesUI() {
    // Guard against multiple initializations (prevents listener accumulation)
    if (_rulesUIInitialized) return;
    _rulesUIInitialized = true;

    // Load rules when Rules view is opened
    const rulesNavBtn = document.querySelector('.nav-item[data-view="rules"]');
    if (rulesNavBtn) {
        rulesNavBtn.addEventListener('click', () => {
            loadRules();
        });
    }

    // Rule Editor Modal
    document.getElementById('createRuleBtn')?.addEventListener('click', () => openRuleEditor(null));
    document.getElementById('ruleEditorClose')?.addEventListener('click', closeRuleEditor);
    document.getElementById('ruleEditorOverlay')?.addEventListener('click', closeRuleEditor);
    document.getElementById('cancelRuleBtn')?.addEventListener('click', closeRuleEditor);
    document.getElementById('saveRuleBtn')?.addEventListener('click', saveRule);
    document.getElementById('addConditionBtn')?.addEventListener('click', () => addConditionRow(null));

    // Template picker
    document.getElementById('showTemplatesBtn')?.addEventListener('click', toggleTemplatePicker);
    document.getElementById('closeTemplatesBtn')?.addEventListener('click', () => {
        document.getElementById('templatePicker').style.display = 'none';
    });

    // Scope selector — initialize custom dropdown and show/hide agent dropdown
    const scopeSelect = document.getElementById('ruleScope');
    if (scopeSelect) {
        new CustomDropdown(scopeSelect, { searchable: false });
        scopeSelect.addEventListener('change', (e) => {
            const scopeField = document.getElementById('scopeAgentField');
            if (scopeField) {
                scopeField.style.display = e.target.value === 'scoped' ? 'block' : 'none';
                if (e.target.value === 'scoped') {
                    const agentSelect = document.getElementById('ruleScopeAgent');
                    if (agentSelect && agentSelect.tagName === 'SELECT') {
                        populateAgentSelect(agentSelect, '');
                        // Initialize custom dropdown for agent select
                        setTimeout(() => {
                            new CustomDropdown(agentSelect, { searchable: false });
                        }, 0);
                        agentSelect.onchange = () => handleAgentSelectChange(agentSelect);
                    }
                }
            }
        });
    }

    // Action selector - initialize custom dropdown
    const actionSelect = document.getElementById('ruleAction');
    if (actionSelect) {
        new CustomDropdown(actionSelect, { searchable: false });
        actionSelect.addEventListener('change', () => updateActionParams());
    }

    // Width toggle buttons
    document.querySelectorAll('.width-toggle-btn').forEach(btn => {
        btn.addEventListener('click', (e) => {
            const width = e.target.getAttribute('data-width');
            const modal = document.getElementById('ruleEditorDrawer');

            // Remove all width classes
            modal.classList.remove('width-narrow', 'width-medium', 'width-wide', 'width-fullscreen');

            // Remove active from all buttons
            document.querySelectorAll('.width-toggle-btn').forEach(b => b.classList.remove('active'));

            // Add the selected width class and active state
            if (width) {
                modal.classList.add('width-' + width);
                e.target.classList.add('active');
            }
        });
    });

    // Initialize action params
    updateActionParams();
}

// ── Load Rules ────────────────────────────────────────────────────────────────
async function loadRules() {
    try {
        const [rulesRes, hitsRes] = await Promise.all([
            fetch('/api/rules'),
            fetch('/api/rules/hit-counts?days=30'),
        ]);
        if (!rulesRes.ok) throw new Error('Failed to load rules');
        rulesData = await rulesRes.json();

        if (hitsRes.ok) {
            ruleHitCounts = await hitsRes.json();
        } else {
            ruleHitCounts = {};
        }

        renderRules(rulesData);
    } catch (error) {
        console.error('Error loading rules:', error);
        showError('Failed to load rules');
    }
}

// ── Render Rules ──────────────────────────────────────────────────────────────
function renderRules(rules) {
    const container = document.getElementById('rulesTable');
    const emptyMsg = document.getElementById('rulesEmpty');
    const badge = document.getElementById('nav-badge-rules');
    const count = document.getElementById('rulesCount');

    if (badge) badge.textContent = rules.length;
    if (count) count.textContent = `${rules.length} ${rules.length === 1 ? 'rule' : 'rules'}`;

    if (rules.length === 0) {
        if (emptyMsg) emptyMsg.style.display = 'block';
        container.innerHTML = '<div class="feed-empty" id="rulesEmpty">No rules defined yet. Create your first rule to control proxy behavior.</div>';
        return;
    }

    if (emptyMsg) emptyMsg.style.display = 'none';

    // Sort by priority DESC
    rules.sort((a, b) => b.priority - a.priority);

    const html = `
        <table class="feed-table" style="font-size:13px;">
            <thead>
                <tr>
                    <th style="width:40px;"></th>
                    <th>Name</th>
                    <th>Priority</th>
                    <th>Conditions</th>
                    <th>Action</th>
                    <th>Scope</th>
                    <th style="text-align:center;">Hits</th>
                    <th style="text-align:right;">Actions</th>
                </tr>
            </thead>
            <tbody>
                ${rules.map(rule => renderRuleRow(rule)).join('')}
            </tbody>
        </table>
    `;
    container.innerHTML = html;

    // Bind action buttons
    rules.forEach(rule => {
        document.getElementById(`toggle-${rule.id}`)?.addEventListener('click', () => toggleRule(rule.id, !rule.enabled));
        document.getElementById(`edit-${rule.id}`)?.addEventListener('click', () => editRule(rule));
        document.getElementById(`delete-${rule.id}`)?.addEventListener('click', () => deleteRule(rule.id, rule.name));
        document.getElementById(`log-${rule.id}`)?.addEventListener('click', () => openRuleRequestLog(rule.id, rule.name));
    });
}

function renderRuleRow(rule) {
    const statusColor = rule.enabled ? 'green' : 'muted';
    const statusText = rule.enabled ? 'ON' : 'OFF';
    const conditionsSummary = rule.conditions.length > 0
        ? rule.conditions.map(c => c.type).join(', ')
        : 'Always';
    const scopeText = rule.scope_agent_id ? `Agent: ${rule.scope_agent_id}` : 'Global';

    // Hit count badge
    const hitData = ruleHitCounts[String(rule.id)];
    const hitCount = hitData ? hitData.count : 0;
    const hitBadgeClass = hitCount > 0 ? 'badge-hit-count' : 'badge-hit-count-zero';
    const hitLabel = hitCount > 0 ? `${hitCount} hit${hitCount !== 1 ? 's' : ''}` : '0 hits';
    let hitTitle = 'No hits in the last 30 days';
    if (hitData && hitData.last_triggered) {
        const lastDate = new Date(hitData.last_triggered * 1000);
        hitTitle = `Last triggered: ${lastDate.toLocaleString()}`;
    }

    return `
        <tr style="opacity:${rule.enabled ? 1 : 0.6}">
            <td>
                <button id="toggle-${rule.id}" class="btn btn-ghost btn-icon" title="${rule.enabled ? 'Disable' : 'Enable'}">
                    <div style="width:10px;height:10px;border-radius:50%;background:var(--${statusColor})"></div>
                </button>
            </td>
            <td>
                <div style="font-weight:600;">${escapeHtml(rule.name)}</div>
                ${rule.description ? `<div style="font-size:11px;color:var(--text-muted);margin-top:2px;">${escapeHtml(rule.description)}</div>` : ''}
                ${rule.evidence ? `<div class="rule-evidence"><svg class="rule-evidence-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M2 14l3-3m0 0l5-5 3-3M5 11l-1.5-1.5"/><circle cx="12" cy="4" r="2"/></svg>${escapeHtml(rule.evidence)}</div>` : ''}
            </td>
            <td style="color:var(--text-muted);">${rule.priority}</td>
            <td style="font-size:11px;color:var(--text-muted);">${conditionsSummary}</td>
            <td><span class="badge">${rule.action.type.replace('_', ' ')}</span></td>
            <td style="font-size:11px;color:var(--text-muted);">${scopeText}</td>
            <td style="text-align:center;">
                <span class="${hitBadgeClass}" title="${hitTitle}">${hitLabel}</span>
            </td>
            <td style="text-align:right;white-space:nowrap;">
                <button id="log-${rule.id}" class="btn-icon" title="View request log" ${hitCount === 0 ? 'disabled style="opacity:0.35;pointer-events:none;"' : ''}>
                    <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
                        <path d="M2 2h12v12H2z"/><path d="M5 5h6M5 8h6M5 11h4"/>
                    </svg>
                </button>
                <button id="edit-${rule.id}" class="btn-icon" title="Edit rule">
                    <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
                        <path d="M11.5 1.5l3 3L5 14H2v-3z"/>
                    </svg>
                </button>
                <button id="delete-${rule.id}" class="btn-icon btn-icon-danger" title="Delete rule">
                    <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
                        <polyline points="3,4 13,4"/><path d="M5 4V2h6v2"/><path d="M4 4l1 10h6l1-10"/>
                    </svg>
                </button>
            </td>
        </tr>
    `;
}

// ── Rule Editor ───────────────────────────────────────────────────────────────
function openRuleEditor(rule = null) {
    currentEditingRule = rule;
    const title = document.getElementById('ruleEditorTitle');
    if (title) title.textContent = rule ? 'Edit Rule' : 'Create Rule';

    // Reset form
    document.getElementById('ruleName').value = rule?.name || '';
    document.getElementById('ruleDescription').value = rule?.description || '';
    document.getElementById('ruleEvidence').value = rule?.evidence || '';
    document.getElementById('rulePriority').value = rule?.priority || 10;
    document.getElementById('ruleScope').value = rule?.scope_agent_id ? 'scoped' : '';
    document.getElementById('scopeAgentField').style.display = rule?.scope_agent_id ? 'block' : 'none';

    // Populate the scope agent dropdown with known agents
    const scopeAgentEl = document.getElementById('ruleScopeAgent');
    if (scopeAgentEl && scopeAgentEl.tagName === 'SELECT') {
        populateAgentSelect(scopeAgentEl, rule?.scope_agent_id || '');
        scopeAgentEl.onchange = () => handleAgentSelectChange(scopeAgentEl);
    } else if (scopeAgentEl) {
        scopeAgentEl.value = rule?.scope_agent_id || '';
    }

    // Clear existing conditions
    document.getElementById('conditionsContainer').innerHTML = '';

    // Add conditions or empty row
    if (rule?.conditions && rule.conditions.length > 0) {
        rule.conditions.forEach(cond => addConditionRow(cond));
    } else {
        addConditionRow();
    }

    // Set action
    document.getElementById('ruleAction').value = rule?.action?.type || 'block';
    updateActionParams(rule?.action);

    // Show modal
    document.getElementById('ruleEditorDrawer').classList.add('open');
    document.getElementById('ruleEditorOverlay').classList.add('open');
}

function closeRuleEditor() {
    document.getElementById('ruleEditorDrawer').classList.remove('open');
    document.getElementById('ruleEditorOverlay').classList.remove('open');
    currentEditingRule = null;
}

// ── Conditions ────────────────────────────────────────────────────────────────
function addConditionRow(condition = null) {
    const container = document.getElementById('conditionsContainer');
    const row = document.createElement('div');
    row.className = 'condition-row';

    // Create custom dropdown for condition type
    const typeOptions = `
        <option value="">-- Select --</option>
        <option value="agent_id">Agent ID</option>
        <option value="app_name">App Name</option>
        <option value="model">Model</option>
        <option value="provider">Provider</option>
        <option value="input_tokens">Input Tokens</option>
        <option value="cost_session">Session Cost</option>
        <option value="cost_daily">Daily Cost</option>
        <option value="cost_monthly">Monthly Cost</option>
        <option value="request_count">Request Count</option>
        <option value="prompt_content">Prompt Content</option>
        <option value="loop_detected">Loop Detected</option>
    `;
    const typeDropdown = createCustomDropdown(typeOptions, condition?.type || '', 'Select condition type', 'condition-type');
    typeDropdown.style.cssText = 'flex:0 0 180px;';

    const paramsDiv = document.createElement('div');
    paramsDiv.className = 'condition-params';
    paramsDiv.style.cssText = 'flex:1;display:flex;gap:8px;';

    const removeBtn = document.createElement('button');
    removeBtn.className = 'btn btn-ghost btn-icon';
    removeBtn.innerHTML = '✕';
    removeBtn.style.cssText = 'flex-shrink:0;';
    removeBtn.onclick = () => row.remove();

    row.appendChild(typeDropdown);
    row.appendChild(paramsDiv);
    row.appendChild(removeBtn);
    container.appendChild(row);

    // Get the underlying select element for change event
    const typeSelect = typeDropdown.querySelector('select');
    typeSelect.addEventListener('change', () => updateConditionParams(typeSelect, condition));
    updateConditionParams(typeSelect, condition);
}

function updateConditionParams(selectElement, existingCondition = null) {
    if (!selectElement) return;
    const type = selectElement.value;
    const paramsDiv = selectElement.parentElement.querySelector('.condition-params');
    if (!paramsDiv) return;

    paramsDiv.innerHTML = '';

    const stringTypes = ['agent_id', 'app_name', 'model', 'provider', 'prompt_content'];
    const numericTypes = ['input_tokens', 'cost_session', 'cost_daily', 'cost_monthly'];
    const quotaTypes = ['request_count'];

    if (stringTypes.includes(type)) {
        // String matching - use custom dropdown for mode
        const modeOptions = `
            <option value="exact">Exact</option>
            <option value="glob">Glob</option>
            <option value="regex">Regex</option>
        `;
        const modeDropdown = createCustomDropdown(modeOptions, existingCondition?.mode || 'exact', 'Match mode', 'param-mode');
        modeDropdown.style.cssText = 'flex:0 0 100px;';
        const modeSelect = modeDropdown.querySelector('select');

        paramsDiv.appendChild(modeDropdown);

        // For exact mode, show dropdowns for agent_id, model, provider
        const currentMode = existingCondition?.mode || 'exact';
        const dropdownTypes = ['agent_id', 'model', 'provider'];
        const useDropdown = dropdownTypes.includes(type) && (currentMode === 'exact' || currentMode === '');

        if (useDropdown) {
            const curVal = existingCondition?.value || '';
            let valueOptions = '';
            let placeholder = 'Select value';

            if (type === 'agent_id') {
                valueOptions = buildAgentOptionsHTML(curVal);
                placeholder = 'Select agent';
            } else if (type === 'model') {
                valueOptions = buildModelOptionsHTML(curVal);
                placeholder = 'Select model';
            } else if (type === 'provider') {
                valueOptions = `
                    <option value="">-- Select provider --</option>
                    <option value="openai"${curVal === 'openai' ? ' selected' : ''}>OpenAI</option>
                    <option value="anthropic"${curVal === 'anthropic' ? ' selected' : ''}>Anthropic</option>
                    <option value="gemini"${curVal === 'gemini' ? ' selected' : ''}>Google Gemini</option>
                `;
                placeholder = 'Select provider';
            }

            const valueDropdown = createCustomDropdown(valueOptions, curVal, placeholder, 'param-value');
            valueDropdown.style.flex = '1';
            const valueSelect = valueDropdown.querySelector('select');

            if (type === 'agent_id') {
                valueSelect.onchange = () => handleAgentSelectChange(valueSelect);
            } else if (type === 'model') {
                valueSelect.onchange = () => handleSelectToInput(valueSelect, 'e.g., gpt-4o-2024-11-20');
            }

            paramsDiv.appendChild(valueDropdown);
        } else {
            const valueInput = document.createElement('input');
            valueInput.className = 'param-value';
            valueInput.placeholder = 'Value';
            valueInput.style.flex = '1';
            if (existingCondition) valueInput.value = existingCondition.value || '';
            paramsDiv.appendChild(valueInput);
        }

        // When mode changes, swap between dropdown and text input
        modeSelect.addEventListener('change', () => {
            if (!dropdownTypes.includes(type)) return;
            const oldValueEl = paramsDiv.querySelector('.param-value, .custom-dropdown-wrapper.param-value');
            // Get current value from either select or input
            const currentVal = oldValueEl?.querySelector('select')?.value || oldValueEl?.value || '';

            if (modeSelect.value === 'exact') {
                // Swap to custom dropdown
                let valueOptions = '';
                let placeholder = 'Select value';

                if (type === 'agent_id') {
                    valueOptions = buildAgentOptionsHTML(currentVal);
                    placeholder = 'Select agent';
                } else if (type === 'model') {
                    valueOptions = buildModelOptionsHTML(currentVal);
                    placeholder = 'Select model';
                } else if (type === 'provider') {
                    valueOptions = `
                        <option value="">-- Select provider --</option>
                        <option value="openai"${currentVal === 'openai' ? ' selected' : ''}>OpenAI</option>
                        <option value="anthropic"${currentVal === 'anthropic' ? ' selected' : ''}>Anthropic</option>
                        <option value="gemini"${currentVal === 'gemini' ? ' selected' : ''}>Google Gemini</option>
                    `;
                    placeholder = 'Select provider';
                }

                const valueDropdown = createCustomDropdown(valueOptions, currentVal, placeholder, 'param-value');
                valueDropdown.style.flex = '1';
                const valueSelect = valueDropdown.querySelector('select');

                if (type === 'agent_id') {
                    valueSelect.onchange = () => handleAgentSelectChange(valueSelect);
                } else if (type === 'model') {
                    valueSelect.onchange = () => handleSelectToInput(valueSelect, 'e.g., gpt-4o-2024-11-20');
                }

                oldValueEl.replaceWith(valueDropdown);
            } else {
                // Swap to text input for glob/regex
                const valueInput = document.createElement('input');
                valueInput.className = 'param-value';
                valueInput.placeholder = modeSelect.value === 'glob' ? 'e.g., claude-*' : 'e.g., ^claude-.*';
                valueInput.style.flex = '1';
                valueInput.value = currentVal;
                oldValueEl.replaceWith(valueInput);
            }
        });

    } else if (numericTypes.includes(type)) {
        // Numeric threshold - use custom dropdown
        const opOptions = `
            <option value="gt">&gt;</option>
            <option value="gte">&gt;=</option>
            <option value="lt">&lt;</option>
            <option value="lte">&lt;=</option>
            <option value="eq">=</option>
        `;
        const opDropdown = createCustomDropdown(opOptions, existingCondition?.op || 'gte', 'Operator', 'param-op');
        opDropdown.style.cssText = 'flex:0 0 80px;';
        const opSelect = opDropdown.querySelector('select');

        const thresholdInput = document.createElement('input');
        thresholdInput.className = 'param-threshold';
        thresholdInput.type = 'number';
        thresholdInput.step = type.includes('cost') ? '0.01' : '1';
        thresholdInput.placeholder = 'Threshold';
        thresholdInput.style.flex = '1';
        if (existingCondition) thresholdInput.value = existingCondition.threshold || '';

        paramsDiv.appendChild(opDropdown);
        paramsDiv.appendChild(thresholdInput);

    } else if (quotaTypes.includes(type)) {
        // Request count with window
        const thresholdInput = document.createElement('input');
        thresholdInput.className = 'param-threshold';
        thresholdInput.type = 'number';
        thresholdInput.placeholder = 'Max requests';
        thresholdInput.style.cssText = 'flex:1;';
        if (existingCondition) thresholdInput.value = existingCondition.threshold || '';

        const windowInput = document.createElement('input');
        windowInput.className = 'param-window';
        windowInput.type = 'number';
        windowInput.placeholder = 'Window (sec)';
        windowInput.style.cssText = 'flex:1;';
        if (existingCondition) windowInput.value = existingCondition.window_sec || 60;

        paramsDiv.appendChild(thresholdInput);
        paramsDiv.appendChild(windowInput);

    } else if (type === 'loop_detected') {
        // No parameters
        paramsDiv.innerHTML = '<span style="color:var(--text-muted);padding:8px;">No parameters</span>';
    }
}

// ── Action Params ─────────────────────────────────────────────────────────────
function updateActionParams(existingAction = null) {
    const actionType = document.getElementById('ruleAction')?.value;
    const container = document.getElementById('actionParams');
    if (!container) return;

    container.innerHTML = '';

    if (actionType === 'block' || actionType === 'rate_limit') {
        container.innerHTML = `
            <div class="field">
                <label class="field-label">HTTP Status</label>
                <input type="number" id="actionBlockStatus" value="${existingAction?.block_status || (actionType === 'block' ? 403 : 429)}" min="400" max="599">
            </div>
            <div class="field">
                <label class="field-label">Message</label>
                <input type="text" id="actionBlockMessage" value="${existingAction?.block_message || (actionType === 'block' ? 'Request blocked' : 'Rate limit exceeded')}" placeholder="Response message">
            </div>
        `;

        if (actionType === 'rate_limit') {
            container.innerHTML += `
                <div class="field">
                    <label class="field-label">Max Requests</label>
                    <input type="number" id="actionRateLimitReq" value="${existingAction?.rate_limit_requests || 10}" min="1">
                </div>
                <div class="field">
                    <label class="field-label">Window (seconds)</label>
                    <input type="number" id="actionRateLimitWindow" value="${existingAction?.rate_limit_window_sec || 60}" min="1">
                </div>
                <div class="field">
                    <label class="field-label">Scope</label>
                    <select id="actionRateLimitScope">
                        <option value="agent" ${existingAction?.rate_limit_scope === 'agent' ? 'selected' : ''}>Per Agent</option>
                        <option value="global" ${existingAction?.rate_limit_scope === 'global' ? 'selected' : ''}>Global</option>
                    </select>
                </div>
            `;
            // Convert native select to custom dropdown
            setTimeout(() => {
                const scopeSelect = document.getElementById('actionRateLimitScope');
                if (scopeSelect) new CustomDropdown(scopeSelect, { searchable: false });
            }, 0);
        }

    } else if (actionType === 'override_model') {
        const currentModel = existingAction?.override_model || '';
        container.innerHTML = `
            <div class="field">
                <label class="field-label">Override Model</label>
                <select id="actionOverrideModel" style="width:100%;">
                    ${buildModelOptionsHTML(currentModel)}
                </select>
                <span style="font-size:11px;color:var(--text-muted);margin-top:4px;display:block;">Cost shown as input / output per 1M tokens</span>
            </div>
        `;
        // Convert native select to custom dropdown
        setTimeout(() => {
            const modelSelect = document.getElementById('actionOverrideModel');
            if (modelSelect) {
                new CustomDropdown(modelSelect, { searchable: true });
                modelSelect.addEventListener('change', function() {
                    handleSelectToInput(this, 'e.g., gpt-4o-2024-11-20');
                });
            }
        }, 0);

    } else if (actionType === 'inject_prompt') {
        container.innerHTML = `
            <div class="field">
                <label class="field-label">System Prompt</label>
                <textarea id="actionInjectPrompt" rows="4" placeholder="System message to inject">${existingAction?.injected_system_prompt || ''}</textarea>
            </div>
        `;

    } else if (actionType === 'redirect') {
        const currentURL = existingAction?.redirect_url || '';
        container.innerHTML = `
            <div class="field">
                <label class="field-label">Redirect Provider</label>
                <select id="actionRedirectURL" style="width:100%;">
                    ${buildRedirectOptionsHTML(currentURL)}
                </select>
            </div>
        `;
        // Convert native select to custom dropdown
        setTimeout(() => {
            const redirectSelect = document.getElementById('actionRedirectURL');
            if (redirectSelect) {
                new CustomDropdown(redirectSelect, { searchable: false });
                redirectSelect.addEventListener('change', function() {
                    handleSelectToInput(this, 'https://alternative-provider.com/v1/...');
                });
            }
        }, 0);
    }
}

// ── Save Rule ─────────────────────────────────────────────────────────────────
async function saveRule() {
    const name = document.getElementById('ruleName')?.value;
    if (!name) {
        alert('Rule name is required');
        return;
    }

    const rule = {
        name,
        description: document.getElementById('ruleDescription')?.value || '',
        evidence: document.getElementById('ruleEvidence')?.value || '',
        enabled: true,
        priority: parseInt(document.getElementById('rulePriority')?.value) || 10,
        scope_agent_id: document.getElementById('ruleScope')?.value === 'scoped'
            ? document.getElementById('ruleScopeAgent')?.value || ''
            : '',
        conditions: collectConditions(),
        action: collectAction(),
    };

    console.log('Saving rule payload:', JSON.stringify(rule, null, 2));

    try {
        const url = currentEditingRule ? `/api/rules/${currentEditingRule.id}` : '/api/rules';
        const method = currentEditingRule ? 'PUT' : 'POST';

        const response = await fetch(url, {
            method,
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(rule),
        });

        if (!response.ok) {
            const errText = await response.text();
            console.error('Save rule failed:', response.status, errText);
            alert(`Failed to save rule: ${response.status} — ${errText}`);
            return;
        }

        closeRuleEditor();
        loadRules();
    } catch (error) {
        console.error('Error saving rule:', error);
        alert('Failed to save rule: ' + error.message);
    }
}

function collectConditions() {
    const conditions = [];
    document.querySelectorAll('.condition-row').forEach(row => {
        const type = row.querySelector('.condition-type')?.value;
        if (!type) return;

        const condition = { type };

        if (['agent_id', 'app_name', 'model', 'provider', 'prompt_content'].includes(type)) {
            condition.mode = row.querySelector('.param-mode')?.value || 'exact';
            condition.value = row.querySelector('.param-value')?.value || '';
        } else if (['input_tokens', 'cost_session', 'cost_daily', 'cost_monthly'].includes(type)) {
            condition.op = row.querySelector('.param-op')?.value || 'gte';
            condition.threshold = parseFloat(row.querySelector('.param-threshold')?.value) || 0;
        } else if (type === 'request_count') {
            condition.threshold = parseFloat(row.querySelector('.param-threshold')?.value) || 0;
            condition.window_sec = parseInt(row.querySelector('.param-window')?.value) || 60;
        }

        conditions.push(condition);
    });
    return conditions;
}

function collectAction() {
    const type = document.getElementById('ruleAction')?.value;
    const action = { type };

    if (type === 'block' || type === 'rate_limit') {
        action.block_status = parseInt(document.getElementById('actionBlockStatus')?.value) || (type === 'block' ? 403 : 429);
        action.block_message = document.getElementById('actionBlockMessage')?.value || '';

        if (type === 'rate_limit') {
            action.rate_limit_requests = parseInt(document.getElementById('actionRateLimitReq')?.value) || 10;
            action.rate_limit_window_sec = parseInt(document.getElementById('actionRateLimitWindow')?.value) || 60;
            action.rate_limit_scope = document.getElementById('actionRateLimitScope')?.value || 'agent';
        }

    } else if (type === 'override_model') {
        action.override_model = document.getElementById('actionOverrideModel')?.value || '';

    } else if (type === 'inject_prompt') {
        action.injected_system_prompt = document.getElementById('actionInjectPrompt')?.value || '';

    } else if (type === 'redirect') {
        action.redirect_url = document.getElementById('actionRedirectURL')?.value || '';
    }

    return action;
}

// ── Toggle / Delete ───────────────────────────────────────────────────────────
async function toggleRule(id, enabled) {
    try {
        const response = await fetch(`/api/rules/${id}/toggle`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled }),
        });
        if (!response.ok) throw new Error('Failed to toggle rule');
        loadRules();
    } catch (error) {
        console.error('Error toggling rule:', error);
        alert('Failed to toggle rule');
    }
}

async function deleteRule(id, name) {
    if (!confirm(`Delete rule "${name}"?`)) return;

    try {
        const response = await fetch(`/api/rules/${id}`, { method: 'DELETE' });
        if (!response.ok) throw new Error('Failed to delete rule');
        loadRules();
    } catch (error) {
        console.error('Error deleting rule:', error);
        alert('Failed to delete rule');
    }
}

function editRule(rule) {
    openRuleEditor(rule);
}

// ── Rule Request Log Modal ─────────────────────────────────────────────────────
function openRuleRequestLog(ruleId, ruleName) {
    ruleLogCurrentId = ruleId;
    ruleLogCurrentOffset = 0;

    const overlay = document.getElementById('ruleRequestLogOverlay');
    const modal = document.getElementById('ruleRequestLogModal');
    const title = document.getElementById('ruleRequestLogTitle');

    if (title) title.textContent = `Request Log — ${ruleName}`;
    if (overlay) overlay.style.display = 'block';
    if (modal) modal.style.display = 'flex';

    loadRuleRequestLog();
}

function closeRuleRequestLog() {
    const overlay = document.getElementById('ruleRequestLogOverlay');
    const modal = document.getElementById('ruleRequestLogModal');
    if (overlay) overlay.style.display = 'none';
    if (modal) modal.style.display = 'none';
    ruleLogCurrentId = null;
}

async function loadRuleRequestLog() {
    const tbody = document.getElementById('ruleRequestLogBody');
    const paginationEl = document.getElementById('ruleRequestLogPagination');
    if (!tbody) return;

    tbody.innerHTML = '<tr><td colspan="10" class="request-log-loading">Loading…</td></tr>';

    try {
        const res = await fetch(`/api/rules/${ruleLogCurrentId}/requests?limit=${ruleLogCurrentLimit}&offset=${ruleLogCurrentOffset}`);
        if (!res.ok) throw new Error('Failed to fetch request log');
        const data = await res.json();

        ruleLogCurrentTotal = data.total || 0;
        const requests = data.requests || [];

        if (requests.length === 0) {
            tbody.innerHTML = '<tr><td colspan="10" class="request-log-empty">No requests found for this rule.</td></tr>';
        } else {
            tbody.innerHTML = requests.map(renderRuleRequestLogRow).join('');
        }

        // Pagination
        if (paginationEl) {
            const page = Math.floor(ruleLogCurrentOffset / ruleLogCurrentLimit) + 1;
            const totalPages = Math.ceil(ruleLogCurrentTotal / ruleLogCurrentLimit);
            paginationEl.innerHTML = `
                <button class="btn btn-ghost btn-sm" onclick="ruleLogPage(-1)" ${ruleLogCurrentOffset === 0 ? 'disabled' : ''}>← Prev</button>
                <span style="font-size:12px;color:var(--text-muted);font-family:var(--font-mono);">
                    Page ${page} of ${totalPages} (${ruleLogCurrentTotal} total)
                </span>
                <button class="btn btn-ghost btn-sm" onclick="ruleLogPage(1)" ${ruleLogCurrentOffset + ruleLogCurrentLimit >= ruleLogCurrentTotal ? 'disabled' : ''}>Next →</button>
            `;
        }
    } catch (err) {
        console.error('Error loading rule request log:', err);
        tbody.innerHTML = '<tr><td colspan="10" class="request-log-empty">Error loading request log.</td></tr>';
    }
}

function renderRuleRequestLogRow(req) {
    const ts = new Date(req.timestamp * 1000);
    const timeStr = ts.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
    const dateStr = ts.toLocaleDateString([], { month: 'short', day: 'numeric' });

    const statusClass = req.status_code >= 400 ? 'color:var(--red)' : 'color:var(--green)';
    const promptText = req.prompt_preview
        ? (req.prompt_preview.length > 60 ? req.prompt_preview.substring(0, 60) + '…' : req.prompt_preview)
        : '—';

    return `
        <tr>
            <td class="col-ts">
                <div style="font-weight:500;">${timeStr}</div>
                <div style="font-size:10px;color:var(--text-muted);">${dateStr}</div>
            </td>
            <td>${escapeHtml(req.agent_id || '—')}</td>
            <td>${escapeHtml(req.provider || '—')}</td>
            <td class="col-model-sm">${escapeHtml(req.model || '—')}</td>
            <td class="col-prompt"><span class="prompt-text" title="${escapeHtml(req.prompt_preview || '')}">${escapeHtml(promptText)}</span></td>
            <td class="text-right">${(req.input_tokens || 0).toLocaleString()}</td>
            <td class="text-right">${(req.output_tokens || 0).toLocaleString()}</td>
            <td class="text-right">$${(req.cost || 0).toFixed(4)}</td>
            <td class="text-right">${req.latency_ms || 0}ms</td>
            <td class="text-right" style="${statusClass};font-weight:600;">${req.status_code || '—'}</td>
        </tr>
    `;
}

function ruleLogPage(direction) {
    const newOffset = ruleLogCurrentOffset + (direction * ruleLogCurrentLimit);
    if (newOffset < 0 || newOffset >= ruleLogCurrentTotal) return;
    ruleLogCurrentOffset = newOffset;
    loadRuleRequestLog();
}

// ── Template Picker ───────────────────────────────────────────────────────────

let _templatesCache = null;

async function toggleTemplatePicker() {
    const picker = document.getElementById('templatePicker');
    if (!picker) return;

    if (picker.style.display === 'none') {
        picker.style.display = 'block';
        await loadTemplates();
    } else {
        picker.style.display = 'none';
    }
}

async function loadTemplates() {
    const grid = document.getElementById('templateGrid');
    if (!grid) return;

    if (_templatesCache) {
        renderTemplates(_templatesCache);
        return;
    }

    try {
        const res = await fetch('/api/rules/templates');
        if (!res.ok) throw new Error('Failed to load templates');
        const data = await res.json();
        _templatesCache = data;
        renderTemplates(data);
    } catch (err) {
        console.error('Error loading templates:', err);
        grid.innerHTML = '<div style="padding:24px;text-align:center;color:var(--text-muted);">Failed to load templates.</div>';
    }
}

function renderTemplates(data) {
    const grid = document.getElementById('templateGrid');
    if (!grid) return;

    const groups = data.groups || [];
    const categoryLabels = { cost: 'Cost Controls', safety: 'Safety & Guardrails', routing: 'Routing & Optimization', performance: 'Performance' };

    let html = '';
    for (const group of groups) {
        const label = categoryLabels[group.category] || group.category;
        html += `<div class="template-category-label">${escapeHtml(label)}</div>`;

        for (const tmpl of group.templates) {
            const icon = TEMPLATE_ICONS[tmpl.icon] || '';
            const actionLabel = (tmpl.action?.type || '').replace(/_/g, ' ');
            html += `
                <div class="template-card" onclick="useTemplate('${escapeHtml(tmpl.id)}')">
                    <div class="template-card-header">
                        <div class="template-card-icon ${escapeHtml(group.category)}">${icon}</div>
                        <div class="template-card-name">${escapeHtml(tmpl.name)}</div>
                    </div>
                    <div class="template-card-desc">${escapeHtml(tmpl.description)}</div>
                    <div class="template-card-meta">
                        <span class="template-card-badge">${escapeHtml(actionLabel)}</span>
                        <span class="template-card-badge">P${tmpl.priority}</span>
                    </div>
                </div>
            `;
        }
    }

    grid.innerHTML = html;
}

function useTemplate(templateId) {
    if (!_templatesCache) return;

    const tmpl = (_templatesCache.templates || []).find(t => t.id === templateId);
    if (!tmpl) return;

    // Open the rule editor pre-filled with template data
    const fakeRule = {
        name: tmpl.name,
        description: tmpl.description,
        evidence: 'Created from template: ' + tmpl.id,
        priority: tmpl.priority,
        scope_agent_id: '',
        conditions: tmpl.conditions || [],
        action: tmpl.action || { type: 'block' },
    };

    openRuleEditor(fakeRule);
    // Clear the ID so it creates a new rule rather than editing
    currentEditingRule = null;

    // Hide the template picker
    document.getElementById('templatePicker').style.display = 'none';
}

// ── Helpers ───────────────────────────────────────────────────────────────────
function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

function showError(message) {
    alert(message); // Simple error handling for now
}

// ── Initialize on load ────────────────────────────────────────────────────────
if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initRulesUI);
} else {
    initRulesUI();
}
