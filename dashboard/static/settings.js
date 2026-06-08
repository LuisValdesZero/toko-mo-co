// Settings page logic
let fallbackConfigs = [];
let editingConfigId = null;
let hitCounts = {};          // configID → {count, last_triggered}

// Request log modal state
let requestLogConfigId = null;
let requestLogOffset = 0;
const REQUEST_LOG_LIMIT = 20;

// API Keys state
let apiKeys = [];

// Pricing state
let pricingEntries = [];
let editingPricingId = null;
let unknownModels = [];
let activePricingProvider = 'all';

// ═══════════════════════════════════════════════════════════════════════════════
// Tab-style Section Navigation (one panel visible at a time)
// ═══════════════════════════════════════════════════════════════════════════════

let activeSection = 'providers'; // default

function showSection(sectionId) {
    if (!sectionId) return;
    activeSection = sectionId;

    // Hide all panels, show selected
    document.querySelectorAll('.settings-panel').forEach(panel => {
        panel.style.display = panel.dataset.section === sectionId ? '' : 'none';
    });

    // Update sidebar active state
    document.querySelectorAll('.nav-item[data-section-link]').forEach(link => {
        link.classList.toggle('active', link.dataset.sectionLink === sectionId);
    });

    // Persist in URL and localStorage
    history.replaceState(null, '', '#' + sectionId);
    try { localStorage.setItem('settings_active_section', sectionId); } catch (e) { /* ignore */ }
}

function initSidebarSectionLinks() {
    document.querySelectorAll('.nav-item[data-section-link]').forEach(link => {
        link.addEventListener('click', (e) => {
            e.preventDefault();
            showSection(link.dataset.sectionLink);
        });
    });
}

// ═══════════════════════════════════════════════════════════════════════════════
// Initialization
// ═══════════════════════════════════════════════════════════════════════════════

document.addEventListener('DOMContentLoaded', () => {
    initSidebarSectionLinks();

    // Determine initial section: URL hash > localStorage > default
    const hash = window.location.hash.replace('#', '');
    let initialSection = hash || null;
    if (!initialSection) {
        try { initialSection = localStorage.getItem('settings_active_section'); } catch (e) { /* ignore */ }
    }
    showSection(initialSection || 'providers');

    loadSettings();
    loadFallbackConfigs();
    loadAPIKeys();
    loadPricingData();
    loadCacheStats();
    loadProviders();
    // Load custom provider models for dropdowns (from models.js)
    if (typeof loadCustomProviderModels === 'function') loadCustomProviderModels();

    // Toggle bge-m3-only fields (base URL + sparse weight) when the embedding
    // provider changes, and refresh the API-key help text accordingly.
    const ep = document.getElementById('embeddingProvider');
    if (ep) ep.addEventListener('change', updateEmbeddingProviderFields);
});

// Shows the Aratiri-bge-m3-specific settings (base URL + sparse weight) only
// when that provider is selected, and updates the embedding key help text.
function updateEmbeddingProviderFields() {
    const provider = document.getElementById('embeddingProvider').value;
    const isAratiri = provider === 'aratiri-bge-m3';
    const sparseItem = document.getElementById('semanticCacheSparseWeightItem');
    if (sparseItem) sparseItem.style.display = isAratiri ? '' : 'none';

    // Model options: bge-m3 for Aratiri, the OpenAI models otherwise. (The bge-m3
    // /embed endpoint ignores the model field, so this is a display choice.)
    const modelSel = document.getElementById('embeddingModel');
    if (modelSel) {
        modelSel.querySelectorAll('.openai-model').forEach(o => { o.hidden = isAratiri; });
        modelSel.querySelectorAll('.aratiri-model').forEach(o => { o.hidden = !isAratiri; });
        if (isAratiri) {
            modelSel.value = 'BAAI/bge-m3';
        } else if (modelSel.value === 'BAAI/bge-m3') {
            modelSel.value = 'text-embedding-3-small';
        }
    }

    // Provider-aware API-key help + env-var name.
    const keyHelp = document.getElementById('embeddingKeyHelp');
    const keyEnv = document.getElementById('embeddingKeyEnvVar');
    if (keyHelp) keyHelp.textContent = isAratiri ? 'Aratiri bge-m3 embedding key (sent as X-API-Key).' : 'OpenAI API key for embeddings.';
    if (keyEnv) keyEnv.textContent = isAratiri ? 'SEMANTIC_CACHE_ARATIRI_API_KEY' : 'OPENAI_API_KEY';

    const keyInput = document.getElementById('embeddingAPIKey');
    if (keyInput && !keyInput.placeholder.includes('•')) {
        keyInput.placeholder = isAratiri ? 'platform API key (X-API-Key)' : 'sk-...';
    }
}

// Load current settings from API
async function loadSettings() {
    try {
        const response = await fetch('/api/settings');
        if (!response.ok) throw new Error('Failed to load settings');
        const settings = await response.json();

        document.getElementById('retryEnabled').checked = settings.retry_enabled;
        document.getElementById('retryMaxAttempts').value = settings.retry_max_attempts;
        document.getElementById('retryInitialDelay').value = settings.retry_initial_delay_ms;
        document.getElementById('retryMaxDelay').value = settings.retry_max_delay_ms;
        document.getElementById('fallbackEnabled').checked = settings.fallback_enabled;
        document.getElementById('fallbackStrategy').value = settings.fallback_strategy;
        document.getElementById('loopThreshold').value = settings.loop_threshold;
        document.getElementById('loopSimilarity').value = Math.round(settings.loop_similarity * 100);
        document.getElementById('loopWindow').value = settings.loop_window_minutes;
        document.getElementById('injectionMode').value = settings.injection_mode;
        document.getElementById('contentThreshold').value = settings.content_threshold_usd;

        // Cache settings
        document.getElementById('cacheEnabled').checked = settings.cache_enabled;
        document.getElementById('cacheMaxEntries').value = settings.cache_max_entries;
        document.getElementById('cacheTTLMinutes').value = settings.cache_ttl_minutes;
        document.getElementById('cacheOnlyTemp0').checked = settings.cache_only_temp0;

        // Security — PII settings
        document.getElementById('piiEnabled').checked = settings.pii_enabled;
        document.getElementById('piiMode').value = settings.pii_mode || 'redact';
        renderPIICategories(settings.pii_categories || '');

        // Semantic Cache settings
        document.getElementById('semanticCacheEnabled').checked = settings.semantic_cache_enabled;
        document.getElementById('semanticCacheThreshold').value = settings.semantic_cache_threshold;
        document.getElementById('semanticCacheMaxVectors').value = settings.semantic_cache_max_vectors;

        // Embedding settings
        if (settings.embedding_provider) {
            document.getElementById('embeddingProvider').value = settings.embedding_provider;
        }
        if (settings.embedding_model) {
            document.getElementById('embeddingModel').value = settings.embedding_model;
        }
        if (typeof settings.semantic_cache_sparse_weight === 'number') {
            document.getElementById('semanticCacheSparseWeight').value = settings.semantic_cache_sparse_weight;
        }
        updateEmbeddingProviderFields();
        // Configured status uses the server's effective-key signal (config OR provider env fallback).
        const keyStatus = document.getElementById('embeddingKeyStatus');
        if (settings.embedding_key_configured) {
            keyStatus.textContent = '✓ Configured';
            keyStatus.style.color = 'var(--green)';
            if (settings.embedding_api_key) document.getElementById('embeddingAPIKey').placeholder = '••••••••';
        } else {
            keyStatus.textContent = '⚠ Not configured — semantic cache inactive';
            keyStatus.style.color = 'var(--red)';
        }

        // Memory Layer settings
        document.getElementById('memoryEnabled').checked = settings.memory_enabled;
        document.getElementById('memoryThreshold').value = settings.memory_threshold;
        document.getElementById('memoryMaxEntries').value = settings.memory_max_entries;
        document.getElementById('memoryMaxResults').value = settings.memory_max_results;
        document.getElementById('memoryRecencyLambda').value = settings.memory_recency_lambda || 0.01;
        document.getElementById('memoryConflictThreshold').value = settings.memory_conflict_threshold || 0.85;
        document.getElementById('memoryTTLDays').value = settings.memory_ttl_days || 90;

        // Load memory stats for the stats bar
        loadMemoryStats();
    } catch (error) {
        console.error('Error loading settings:', error);
        // Fall back to safe defaults if API not available
        document.getElementById('retryEnabled').checked = true;
        document.getElementById('retryMaxAttempts').value = 3;
        document.getElementById('retryInitialDelay').value = 1000;
        document.getElementById('retryMaxDelay').value = 30000;
        document.getElementById('fallbackEnabled').checked = false;
        document.getElementById('loopThreshold').value = 3;
        document.getElementById('loopSimilarity').value = 80;
        document.getElementById('loopWindow').value = 5;
        document.getElementById('injectionMode').value = 'metadata';
        document.getElementById('contentThreshold').value = 10.0;
        document.getElementById('cacheEnabled').checked = true;
        document.getElementById('cacheMaxEntries').value = 1000;
        document.getElementById('cacheTTLMinutes').value = 60;
        document.getElementById('cacheOnlyTemp0').checked = true;
        document.getElementById('piiEnabled').checked = false;
        document.getElementById('piiMode').value = 'redact';
        renderPIICategories('');
        document.getElementById('semanticCacheEnabled').checked = false;
        document.getElementById('semanticCacheThreshold').value = 0.95;
        document.getElementById('semanticCacheMaxVectors').value = 10000;
        document.getElementById('embeddingProvider').value = 'openai';
        document.getElementById('embeddingModel').value = 'text-embedding-3-small';
        document.getElementById('memoryEnabled').checked = false;
        document.getElementById('memoryThreshold').value = 0.7;
        document.getElementById('memoryMaxEntries').value = 10000;
        document.getElementById('memoryMaxResults').value = 5;
        document.getElementById('memoryRecencyLambda').value = 0.01;
        document.getElementById('memoryConflictThreshold').value = 0.85;
        document.getElementById('memoryTTLDays').value = 90;
    }

    // Sync all custom dropdowns after values are set from API
    CustomDropdown.refreshAll();
}

// Load fallback configurations (global defaults only)
async function loadFallbackConfigs() {
    try {
        const response = await fetch('/api/fallback-configs?agent_id=');
        if (!response.ok) throw new Error('Failed to load fallback configs');

        fallbackConfigs = await response.json();
        renderFallbackConfigs();
        loadHitCounts(); // fetch hit counts after configs are rendered
    } catch (error) {
        console.error('Error loading fallback configs:', error);
    }
}

// Load hit counts for all fallback configs (30-day window)
async function loadHitCounts() {
    try {
        const response = await fetch('/api/fallback-configs/hit-counts?days=30');
        if (!response.ok) return;
        hitCounts = await response.json();
        renderFallbackConfigs(); // re-render with hit count badges
    } catch (e) {
        console.error('Failed to load hit counts:', e);
    }
}

// Render fallback configs table
function renderFallbackConfigs() {
    const container = document.getElementById('fallbackConfigsList');

    if (fallbackConfigs.length === 0) {
        container.innerHTML = `
            <div class="fallback-empty-state">
                <p>No global fallback mappings configured</p>
                <p class="fallback-empty-hint">Click "Add Mapping" to create default fallback behavior</p>
            </div>
        `;
        return;
    }

    container.innerHTML = `
        <div class="fallback-configs-table">
            ${fallbackConfigs.map(config => renderFallbackConfigRow(config)).join('')}
        </div>
    `;
}

// Render single fallback config row
function renderFallbackConfigRow(config) {
    const fallbackItems = config.fallback_chain.map(opt =>
        `<span class="fallback-chain-item">${getProviderLabel(opt.provider)}/${getModelInfo(opt.provider, opt.model).label}</span>`
    ).join('<span class="fallback-chain-arrow">\u2192</span>');

    const statusBadge = config.enabled
        ? '<span class="badge badge-success">Enabled</span>'
        : '<span class="badge badge-muted">Disabled</span>';

    // Hit count badge
    const configHits = hitCounts[String(config.id)];
    const hitCountBadge = configHits && configHits.count > 0
        ? `<span class="badge badge-hit-count" title="Triggered ${configHits.count} times in last 30 days">${configHits.count} hits</span>`
        : '<span class="badge badge-hit-count-zero">0 hits</span>';

    const sourceLabel = `${getProviderLabel(config.source_provider)}/${getModelInfo(config.source_provider, config.source_model).label}`;

    return `
        <div class="fallback-config-row">
            <div class="fallback-config-top">
                <div class="fallback-config-chain">
                    <span class="fallback-chain-source">${sourceLabel}</span>
                    <span class="fallback-chain-label">falls back to</span>
                    ${fallbackItems}
                </div>
                <div class="fallback-config-actions">
                    <button class="btn-icon" onclick="openRequestLog(${config.id})" title="View Request Log">
                        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="2" y="2" width="12" height="12" rx="1.5"/><path d="M5 5.5h6M5 8h6M5 10.5h4"/></svg>
                    </button>
                    <button class="btn-icon" onclick="editFallbackConfig(${config.id})" title="Edit">
                        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="8" cy="8" r="2.5"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M3.2 12.8l1.4-1.4M11.4 4.6l1.4-1.4"/></svg>
                    </button>
                    <button class="btn-icon" onclick="toggleFallbackConfig(${config.id}, ${!config.enabled})" title="${config.enabled ? 'Disable' : 'Enable'}">
                        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">${config.enabled
                            ? '<rect x="4" y="3" width="2.5" height="10" rx="1"/><rect x="9.5" y="3" width="2.5" height="10" rx="1"/>'
                            : '<polygon points="4,2 14,8 4,14"/>'
                        }</svg>
                    </button>
                    <button class="btn-icon btn-icon-danger" onclick="deleteFallbackConfig(${config.id})" title="Delete">
                        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M3 4h10M5.5 4V3a1 1 0 0 1 1-1h3a1 1 0 0 1 1 1v1M4 4l.8 9a1.5 1.5 0 0 0 1.5 1.4h3.4a1.5 1.5 0 0 0 1.5-1.4L12 4"/></svg>
                    </button>
                </div>
            </div>
            <div class="fallback-config-status">${statusBadge} ${hitCountBadge}</div>
        </div>
    `;
}

// Open fallback editor modal
function openFallbackEditor(configId = null) {
    editingConfigId = configId;
    const modal = document.getElementById('fallbackEditorModal');
    const title = document.getElementById('editorTitle');
    const providerSelect = document.getElementById('editorSourceProvider');
    const modelSelect = document.getElementById('editorSourceModel');

    if (configId) {
        const config = fallbackConfigs.find(c => c.id === configId);
        if (!config) return;

        title.textContent = `Edit Fallback: ${config.source_provider}/${config.source_model}`;
        providerSelect.value = config.source_provider;
        updateSourceModelOptions();
        modelSelect.value = config.source_model;
        document.getElementById('editorEnabled').checked = config.enabled;

        // Populate fallback chain
        const chainEditor = document.getElementById('fallbackChainEditor');
        chainEditor.innerHTML = '';
        config.fallback_chain.forEach((opt, index) => {
            appendFallbackOptionEditor(index, opt.provider, opt.model);
        });
    } else {
        title.textContent = 'Add Global Fallback Mapping';
        providerSelect.value = '';
        modelSelect.innerHTML = '<option value="">Select provider first...</option>';
        document.getElementById('editorEnabled').checked = true;
        document.getElementById('fallbackChainEditor').innerHTML = '';
    }

    // Refresh the modal dropdowns after values are set
    CustomDropdown.refresh(providerSelect);
    CustomDropdown.refresh(modelSelect);

    modal.style.display = 'flex';
    document.getElementById('fallbackEditorOverlay').style.display = 'block';
}

// Close fallback editor modal
function closeFallbackEditor() {
    document.getElementById('fallbackEditorModal').style.display = 'none';
    document.getElementById('fallbackEditorOverlay').style.display = 'none';
    editingConfigId = null;
}

// Update source model options based on selected provider
function updateSourceModelOptions() {
    const provider = document.getElementById('editorSourceProvider').value;
    const modelSelect = document.getElementById('editorSourceModel');

    if (!provider) {
        modelSelect.innerHTML = '<option value="">Select provider first...</option>';
    } else {
        const models = getModelsForProvider(provider);
        modelSelect.innerHTML = '<option value="">Select model...</option>' +
            models.map(m => `<option value="${m.value}">${m.label}</option>`).join('');
    }

    // Refresh the custom dropdown to reflect new options
    CustomDropdown.refresh(modelSelect);
}

// Add fallback option to chain
function addFallbackOption() {
    const chainEditor = document.getElementById('fallbackChainEditor');
    const index = chainEditor.querySelectorAll('.fallback-option-editor').length;
    appendFallbackOptionEditor(index, '', '');
}

// Append a fallback option editor row (builds DOM + upgrades dropdowns)
function appendFallbackOptionEditor(index, provider = '', model = '') {
    const chainEditor = document.getElementById('fallbackChainEditor');
    const providers = getAllProviders();
    const models = provider ? getModelsForProvider(provider) : [];

    const row = document.createElement('div');
    row.className = 'fallback-option-editor';
    row.dataset.index = index;

    row.innerHTML = `
        <span class="fallback-option-priority">${index + 1}.</span>
        <select data-custom-dropdown data-role="provider">
            <option value="">Provider\u2026</option>
            ${providers.map(p => `<option value="${p}" ${p === provider ? 'selected' : ''}>${getProviderLabel(p)}</option>`).join('')}
        </select>
        <select data-custom-dropdown data-role="model">
            <option value="">Model\u2026</option>
            ${models.map(m => `<option value="${m.value}" ${m.value === model ? 'selected' : ''}>${m.label}</option>`).join('')}
        </select>
        <button class="btn-icon btn-icon-danger" title="Remove">
            <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2"><line x1="3" y1="3" x2="13" y2="13"/><line x1="13" y1="3" x2="3" y2="13"/></svg>
        </button>
    `;

    chainEditor.appendChild(row);

    // Upgrade the two selects inside this row
    const provSelect = row.querySelector('[data-role="provider"]');
    const modSelect  = row.querySelector('[data-role="model"]');
    CustomDropdown.upgrade(provSelect);
    CustomDropdown.upgrade(modSelect);

    // Wire events
    provSelect.addEventListener('change', () => {
        updateFallbackModelOptionsForRow(row);
    });

    row.querySelector('.btn-icon-danger').addEventListener('click', () => {
        // Destroy dropdowns before removing row
        CustomDropdown.destroy(provSelect);
        CustomDropdown.destroy(modSelect);
        row.remove();
        renumberFallbackOptions();
    });
}

// Update model options when provider changes within a chain row
function updateFallbackModelOptionsForRow(row) {
    const providerSelect = row.querySelector('[data-role="provider"]');
    const modelSelect = row.querySelector('[data-role="model"]');
    const provider = providerSelect.value;

    if (!provider) {
        modelSelect.innerHTML = '<option value="">Select provider first\u2026</option>';
    } else {
        const models = getModelsForProvider(provider);
        modelSelect.innerHTML = '<option value="">Model\u2026</option>' +
            models.map(m => `<option value="${m.value}">${m.label}</option>`).join('');
    }

    CustomDropdown.refresh(modelSelect);
}

// Renumber remaining fallback option rows after deletion
function renumberFallbackOptions() {
    document.querySelectorAll('.fallback-option-editor').forEach((el, idx) => {
        el.dataset.index = idx;
        el.querySelector('.fallback-option-priority').textContent = `${idx + 1}.`;
    });
}

// Save fallback configuration
async function saveFallbackConfig() {
    const sourceProvider = document.getElementById('editorSourceProvider').value;
    const sourceModel = document.getElementById('editorSourceModel').value;
    const enabled = document.getElementById('editorEnabled').checked;

    if (!sourceProvider || !sourceModel) {
        alert('Please select source provider and model');
        return;
    }

    // Collect fallback chain
    const chain = [];
    document.querySelectorAll('.fallback-option-editor').forEach((editor, index) => {
        const provider = editor.querySelector('[data-role="provider"]').value;
        const model = editor.querySelector('[data-role="model"]').value;
        if (provider && model) {
            chain.push({ provider, model, priority: index + 1 });
        }
    });

    if (chain.length === 0) {
        alert('Please add at least one fallback option');
        return;
    }

    const config = {
        agent_id: '',  // Global default
        source_provider: sourceProvider,
        source_model: sourceModel,
        fallback_chain: chain,
        enabled: enabled,
        priority: 0
    };

    try {
        let response;
        if (editingConfigId) {
            config.id = editingConfigId;
            response = await fetch(`/api/fallback-configs/${editingConfigId}`, {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(config)
            });
        } else {
            response = await fetch('/api/fallback-configs', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(config)
            });
        }

        if (!response.ok) throw new Error('Failed to save fallback config');

        closeFallbackEditor();
        await loadFallbackConfigs();
    } catch (error) {
        console.error('Error saving fallback config:', error);
        alert('Failed to save fallback configuration');
    }
}

// Edit fallback configuration
function editFallbackConfig(id) {
    openFallbackEditor(id);
}

// Toggle fallback configuration
async function toggleFallbackConfig(id, enabled) {
    try {
        const response = await fetch(`/api/fallback-configs/${id}/toggle`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled })
        });

        if (!response.ok) throw new Error('Failed to toggle fallback config');

        await loadFallbackConfigs();
    } catch (error) {
        console.error('Error toggling fallback config:', error);
        alert('Failed to toggle fallback configuration');
    }
}

// Delete fallback configuration
async function deleteFallbackConfig(id) {
    if (!confirm('Are you sure you want to delete this fallback mapping?')) return;

    try {
        const response = await fetch(`/api/fallback-configs/${id}`, { method: 'DELETE' });
        if (!response.ok) throw new Error('Failed to delete fallback config');

        await loadFallbackConfigs();
    } catch (error) {
        console.error('Error deleting fallback config:', error);
        alert('Failed to delete fallback configuration');
    }
}

// Save all settings
async function saveSettings() {
    const saveButton = document.getElementById('saveButton');
    const saveStatus = document.getElementById('saveStatus');

    saveButton.disabled = true;
    saveButton.innerHTML = 'Saving\u2026';
    saveStatus.textContent = '';

    const settings = {
        retry_enabled: document.getElementById('retryEnabled').checked,
        retry_max_attempts: parseInt(document.getElementById('retryMaxAttempts').value, 10),
        retry_initial_delay_ms: parseInt(document.getElementById('retryInitialDelay').value, 10),
        retry_max_delay_ms: parseInt(document.getElementById('retryMaxDelay').value, 10),
        fallback_enabled: document.getElementById('fallbackEnabled').checked,
        fallback_strategy: document.getElementById('fallbackStrategy').value,
        loop_threshold: parseInt(document.getElementById('loopThreshold').value, 10),
        loop_similarity: parseInt(document.getElementById('loopSimilarity').value, 10) / 100,
        loop_window_minutes: parseInt(document.getElementById('loopWindow').value, 10),
        injection_mode: document.getElementById('injectionMode').value,
        content_threshold_usd: parseFloat(document.getElementById('contentThreshold').value),
        cache_enabled: document.getElementById('cacheEnabled').checked,
        cache_max_entries: parseInt(document.getElementById('cacheMaxEntries').value, 10),
        cache_ttl_minutes: parseInt(document.getElementById('cacheTTLMinutes').value, 10),
        cache_only_temp0: document.getElementById('cacheOnlyTemp0').checked,
        pii_enabled: document.getElementById('piiEnabled').checked,
        pii_mode: document.getElementById('piiMode').value,
        pii_categories: collectPIICategories(),
        // Semantic Cache & Embeddings
        semantic_cache_enabled: document.getElementById('semanticCacheEnabled').checked,
        semantic_cache_threshold: parseFloat(document.getElementById('semanticCacheThreshold').value),
        semantic_cache_max_vectors: parseInt(document.getElementById('semanticCacheMaxVectors').value, 10),
        embedding_provider: document.getElementById('embeddingProvider').value,
        embedding_model: document.getElementById('embeddingModel').value,
        embedding_api_key: document.getElementById('embeddingAPIKey').value || '',
        semantic_cache_sparse_weight: parseFloat(document.getElementById('semanticCacheSparseWeight').value) || 0,
        // Memory Layer
        memory_enabled: document.getElementById('memoryEnabled').checked,
        memory_threshold: parseFloat(document.getElementById('memoryThreshold').value),
        memory_max_entries: parseInt(document.getElementById('memoryMaxEntries').value, 10),
        memory_max_results: parseInt(document.getElementById('memoryMaxResults').value, 10),
        memory_recency_lambda: parseFloat(document.getElementById('memoryRecencyLambda').value),
        memory_conflict_threshold: parseFloat(document.getElementById('memoryConflictThreshold').value),
        memory_ttl_days: parseInt(document.getElementById('memoryTTLDays').value, 10),
    };

    const svgIcon = '<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M12 2H4a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V4.5L12 2z"/><path d="M10 2v3H6V2"/><path d="M5 10h6"/><path d="M5 13h6"/></svg>';

    try {
        const response = await fetch('/api/settings', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(settings)
        });

        if (!response.ok) {
            const errText = await response.text();
            throw new Error(errText);
        }

        // Re-fetch settings to get server-side state (masked key, etc.)
        const updated = await response.json();
        const keyStatus = document.getElementById('embeddingKeyStatus');
        if (updated.embedding_key_configured) {
            keyStatus.textContent = updated.embedding_api_key
                ? '✓ Configured (' + updated.embedding_api_key + ')'
                : '✓ Configured (from env)';
            keyStatus.style.color = 'var(--green)';
            document.getElementById('embeddingAPIKey').value = '';
            if (updated.embedding_api_key) document.getElementById('embeddingAPIKey').placeholder = updated.embedding_api_key;
        } else {
            keyStatus.textContent = '⚠ Not configured — semantic cache inactive';
            keyStatus.style.color = 'var(--red)';
        }

        saveButton.disabled = false;
        saveButton.innerHTML = `${svgIcon} Save Settings`;
        saveStatus.innerHTML = '<span style="color: var(--green);">Saved</span>';

        setTimeout(() => { saveStatus.textContent = ''; }, 5000);
    } catch (error) {
        console.error('Error saving settings:', error);
        saveButton.disabled = false;
        saveButton.innerHTML = `${svgIcon} Save Settings`;
        saveStatus.innerHTML = `<span style="color: var(--red);">Failed: ${escapeHtml(error.message)}</span>`;
    }
}

// ── Request Log Modal ─────────────────────────────────────────────────────────

function openRequestLog(configId) {
    requestLogConfigId = configId;
    requestLogOffset = 0;

    const config = fallbackConfigs.find(c => c.id === configId);
    const titleEl = document.getElementById('requestLogTitle');
    if (config) {
        titleEl.textContent = `Request Log: ${getProviderLabel(config.source_provider)}/${getModelInfo(config.source_provider, config.source_model).label}`;
    } else {
        titleEl.textContent = 'Request Log';
    }

    document.getElementById('requestLogModal').style.display = 'flex';
    document.getElementById('requestLogOverlay').style.display = 'block';
    loadRequestLog();
}

function closeRequestLog() {
    document.getElementById('requestLogModal').style.display = 'none';
    document.getElementById('requestLogOverlay').style.display = 'none';
    requestLogConfigId = null;
}

async function loadRequestLog() {
    const body = document.getElementById('requestLogBody');
    body.innerHTML = '<div class="request-log-loading">Loading\u2026</div>';

    try {
        const response = await fetch(
            `/api/fallback-configs/${requestLogConfigId}/requests?limit=${REQUEST_LOG_LIMIT}&offset=${requestLogOffset}`
        );
        if (!response.ok) throw new Error('Failed to load');
        const data = await response.json();

        if (!data.requests || data.requests.length === 0) {
            body.innerHTML = '<div class="request-log-empty">No requests have triggered this fallback rule yet.</div>';
            updateRequestLogPagination(0, 0);
            return;
        }

        body.innerHTML = `
            <div class="request-log-table-wrap">
                <table class="request-log-table">
                    <thead>
                        <tr>
                            <th>Time</th>
                            <th>Agent</th>
                            <th>Original</th>
                            <th>Fallback To</th>
                            <th>Prompt</th>
                            <th class="text-right">Tokens</th>
                            <th class="text-right">Cost</th>
                            <th class="text-right">Latency</th>
                            <th>Status</th>
                        </tr>
                    </thead>
                    <tbody>
                        ${data.requests.map(renderRequestLogRow).join('')}
                    </tbody>
                </table>
            </div>
        `;

        updateRequestLogPagination(data.total, data.offset);
    } catch (e) {
        body.innerHTML = '<div class="request-log-empty" style="color: var(--red);">Failed to load request log</div>';
    }
}

function renderRequestLogRow(req) {
    const ts = new Date(req.timestamp * 1000);
    const timeStr = ts.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
    const costStr = req.cost < 0.0001 ? '<$0.0001' : `$${req.cost.toFixed(4)}`;
    const latencyStr = req.latency_ms >= 1000
        ? `${(req.latency_ms / 1000).toFixed(1)}s`
        : `${req.latency_ms}ms`;

    let statusClass = 'status-2xx';
    if (req.status_code >= 500) statusClass = 'status-5xx';
    else if (req.status_code >= 400) statusClass = 'status-4xx';

    const totalTokens = (req.input_tokens + req.output_tokens).toLocaleString();

    return `
        <tr>
            <td class="col-ts">${escapeHtml(timeStr)}</td>
            <td><span class="badge badge-agent">${escapeHtml(req.agent_id || '\u2014')}</span></td>
            <td class="col-model-sm">${escapeHtml(req.original_provider)}/${escapeHtml(req.original_model)}</td>
            <td class="col-model-sm">${escapeHtml(req.provider)}/${escapeHtml(req.model)}</td>
            <td class="col-prompt"><span class="prompt-text">${escapeHtml(req.prompt_preview || '')}</span></td>
            <td class="text-right">${totalTokens}</td>
            <td class="text-right">${costStr}</td>
            <td class="text-right">${latencyStr}</td>
            <td><span class="${statusClass}">${req.status_code}</span></td>
        </tr>
    `;
}

function updateRequestLogPagination(total, currentOffset) {
    const pagination = document.getElementById('requestLogPagination');
    const prevBtn = document.getElementById('requestLogPrev');
    const nextBtn = document.getElementById('requestLogNext');

    if (total === 0) {
        pagination.textContent = '0 results';
        prevBtn.disabled = true;
        nextBtn.disabled = true;
        return;
    }

    const from = currentOffset + 1;
    const to = Math.min(currentOffset + REQUEST_LOG_LIMIT, total);
    pagination.textContent = `${from}\u2013${to} of ${total}`;

    prevBtn.disabled = currentOffset <= 0;
    nextBtn.disabled = currentOffset + REQUEST_LOG_LIMIT >= total;
}

function requestLogPage(direction) {
    requestLogOffset += direction * REQUEST_LOG_LIMIT;
    if (requestLogOffset < 0) requestLogOffset = 0;
    loadRequestLog();
}

// ═══════════════════════════════════════════════════════════════════════════════
// API Key Management
// ═══════════════════════════════════════════════════════════════════════════════

async function loadAPIKeys() {
    try {
        const [keysRes, statusRes] = await Promise.all([
            fetch('/api/keys'),
            fetch('/api/keys/auth-status'),
        ]);

        if (keysRes.ok) {
            apiKeys = await keysRes.json();
        } else {
            apiKeys = [];
        }

        if (statusRes.ok) {
            const status = await statusRes.json();
            const toggle = document.getElementById('authEnabled');
            if (toggle) toggle.checked = status.auth_enabled;

            const badge = document.getElementById('authKeyCount');
            if (badge) {
                const count = status.key_count || 0;
                badge.textContent = `${count} key${count !== 1 ? 's' : ''}`;
                badge.className = count > 0 ? 'badge badge-hit-count' : 'badge badge-muted';
            }
        }

        renderAPIKeys();
    } catch (err) {
        console.error('Failed to load API keys:', err);
    }
}

function renderAPIKeys() {
    const container = document.getElementById('apiKeysList');
    if (!container) return;

    if (!apiKeys || apiKeys.length === 0) {
        container.innerHTML = `
            <div class="fallback-empty-state">
                <p>No API keys created yet</p>
                <p class="fallback-empty-hint">Create a key to enable authenticated access to proxy endpoints</p>
            </div>`;
        return;
    }

    container.innerHTML = `
        <div class="api-keys-table">
            ${apiKeys.map(renderAPIKeyRow).join('')}
        </div>`;
}

function renderAPIKeyRow(key) {
    const createdDate = key.created_at > 0
        ? new Date(key.created_at * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' })
        : '\u2014';

    const lastUsedStr = key.last_used > 0
        ? timeAgo(key.last_used * 1000)
        : 'Never';

    let expiryStr = 'Never';
    let expiryClass = '';
    if (key.expires_at > 0) {
        const expiresDate = new Date(key.expires_at * 1000);
        const now = new Date();
        if (expiresDate < now) {
            expiryStr = 'Expired';
            expiryClass = 'color:var(--red);font-weight:600;';
        } else {
            const daysLeft = Math.ceil((expiresDate - now) / (1000 * 60 * 60 * 24));
            expiryStr = daysLeft <= 7
                ? `${daysLeft}d left`
                : expiresDate.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
            if (daysLeft <= 7) expiryClass = 'color:var(--yellow);font-weight:600;';
        }
    }

    const statusBadge = key.enabled
        ? '<span class="badge badge-success">Active</span>'
        : '<span class="badge badge-muted">Disabled</span>';

    return `
        <div class="api-key-row" style="opacity:${key.enabled ? 1 : 0.6}">
            <div class="api-key-info">
                <div class="api-key-name">${escapeHtml(key.name)}</div>
                <div class="api-key-meta">
                    <code class="api-key-prefix">${escapeHtml(key.prefix)}</code>
                    ${statusBadge}
                    <span class="api-key-scope">${escapeHtml(key.scopes)}</span>
                </div>
            </div>
            <div class="api-key-dates">
                <div class="api-key-date">
                    <span class="api-key-date-label">Created</span>
                    <span class="api-key-date-value">${createdDate}</span>
                </div>
                <div class="api-key-date">
                    <span class="api-key-date-label">Last Used</span>
                    <span class="api-key-date-value">${lastUsedStr}</span>
                </div>
                <div class="api-key-date">
                    <span class="api-key-date-label">Expires</span>
                    <span class="api-key-date-value" style="${expiryClass}">${expiryStr}</span>
                </div>
            </div>
            <div class="fallback-config-actions">
                <button class="btn-icon" onclick="toggleAPIKey(${key.id}, ${!key.enabled})" title="${key.enabled ? 'Disable' : 'Enable'}">
                    <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">${key.enabled
                        ? '<rect x="4" y="3" width="2.5" height="10" rx="1"/><rect x="9.5" y="3" width="2.5" height="10" rx="1"/>'
                        : '<polygon points="4,2 14,8 4,14"/>'
                    }</svg>
                </button>
                <button class="btn-icon btn-icon-danger" onclick="deleteAPIKey(${key.id}, '${escapeHtml(key.name)}')" title="Revoke key">
                    <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
                        <polyline points="3,4 13,4"/><path d="M5 4V2h6v2"/><path d="M4 4l1 10h6l1-10"/>
                    </svg>
                </button>
            </div>
        </div>`;
}

function timeAgo(ts) {
    const diff = Date.now() - ts;
    const minutes = Math.floor(diff / 60000);
    if (minutes < 1) return 'Just now';
    if (minutes < 60) return `${minutes}m ago`;
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `${hours}h ago`;
    const days = Math.floor(hours / 24);
    if (days < 30) return `${days}d ago`;
    return new Date(ts).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

// ── Create Key Modal ──────────────────────────────────────────────────────

function openCreateKeyModal() {
    document.getElementById('createKeyOverlay').style.display = 'block';
    document.getElementById('createKeyModal').style.display = 'flex';
    document.getElementById('newKeyName').value = '';
    document.getElementById('newKeyName').focus();
}

function closeCreateKeyModal() {
    document.getElementById('createKeyOverlay').style.display = 'none';
    document.getElementById('createKeyModal').style.display = 'none';
}

async function createAPIKey() {
    const name = document.getElementById('newKeyName').value.trim();
    if (!name) {
        alert('Please enter a key name');
        return;
    }

    const scopes = document.getElementById('newKeyScopes').value;
    const expiresInDays = parseInt(document.getElementById('newKeyExpiry').value) || 0;

    try {
        const response = await fetch('/api/keys', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name, scopes, expires_in_days: expiresInDays }),
        });

        if (!response.ok) {
            const err = await response.text();
            throw new Error(err);
        }

        const data = await response.json();
        closeCreateKeyModal();

        // Show the key reveal modal
        document.getElementById('revealedKey').textContent = data.key;
        document.getElementById('keyRevealOverlay').style.display = 'block';
        document.getElementById('keyRevealModal').style.display = 'flex';

        // Reload the keys list
        loadAPIKeys();
    } catch (err) {
        console.error('Failed to create key:', err);
        alert('Failed to create API key: ' + err.message);
    }
}

function copyRevealedKey() {
    const key = document.getElementById('revealedKey').textContent;
    navigator.clipboard.writeText(key).then(() => {
        const btn = document.getElementById('copyKeyBtn');
        btn.textContent = 'Copied!';
        btn.style.color = 'var(--green)';
        setTimeout(() => {
            btn.textContent = 'Copy';
            btn.style.color = '';
        }, 2000);
    });
}

function closeKeyRevealModal() {
    document.getElementById('keyRevealOverlay').style.display = 'none';
    document.getElementById('keyRevealModal').style.display = 'none';
}

// ── Toggle & Delete ───────────────────────────────────────────────────────

async function toggleAPIKey(id, enabled) {
    try {
        await fetch(`/api/keys/${id}/toggle`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled }),
        });
        loadAPIKeys();
    } catch (err) {
        console.error('Failed to toggle key:', err);
    }
}

async function deleteAPIKey(id, name) {
    if (!confirm(`Revoke API key "${name}"? This cannot be undone \u2014 any clients using this key will immediately lose access.`)) {
        return;
    }
    try {
        await fetch(`/api/keys/${id}`, { method: 'DELETE' });
        loadAPIKeys();
    } catch (err) {
        console.error('Failed to delete key:', err);
    }
}

// ── Auth Toggle ───────────────────────────────────────────────────────────

async function toggleAuth(enabled) {
    if (enabled && apiKeys.length === 0) {
        alert('Create at least one API key before enabling authentication, otherwise all proxy requests will be rejected.');
        document.getElementById('authEnabled').checked = false;
        return;
    }
    const badge = document.getElementById('authKeyCount');
    if (badge) {
        badge.textContent = enabled ? 'Requires restart' : `${apiKeys.length} key${apiKeys.length !== 1 ? 's' : ''}`;
        badge.className = enabled ? 'badge badge-hit-count' : 'badge badge-muted';
    }
}

// HTML escaper
function escapeHtml(str) {
    return String(str)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;');
}

// ═══════════════════════════════════════════════════════════════════════════════
// Model Pricing Management
// ═══════════════════════════════════════════════════════════════════════════════

async function loadPricingData() {
    try {
        const [pricingRes, unknownRes, staleRes] = await Promise.all([
            fetch('/api/pricing'),
            fetch('/api/pricing/unknown-models'),
            fetch('/api/pricing/stale-check'),
        ]);

        if (pricingRes.ok) {
            pricingEntries = await pricingRes.json();
            if (!pricingEntries) pricingEntries = [];
        }
        if (unknownRes.ok) {
            unknownModels = await unknownRes.json();
            if (!unknownModels) unknownModels = [];
        }
        if (staleRes.ok) {
            const staleData = await staleRes.json();
            renderStaleBadge(staleData);
        }

        renderPricingProviderTabs();
        renderPricingList();
        renderUnknownModels();
        renderPricingLastUpdated();
    } catch (err) {
        console.error('Failed to load pricing data:', err);
    }
}

// Shows the date+time of the most recently updated pricing row.
function renderPricingLastUpdated() {
    const el = document.getElementById('pricingLastUpdated');
    if (!el) return;
    const ts = pricingEntries.length
        ? Math.max(0, ...pricingEntries.map(e => e.updated_at || 0))
        : 0;
    el.textContent = ts ? `Last updated: ${new Date(ts * 1000).toLocaleString()}` : '';
}

function renderStaleBadge(data) {
    const badge = document.getElementById('pricingStaleBadge');
    if (!badge) return;
    if (data.stale) {
        badge.textContent = `${data.days_old}d since update`;
        badge.className = 'badge badge-warning-subtle';
        badge.style.display = '';
    } else if (data.days_old > 0) {
        badge.textContent = `Updated ${data.days_old}d ago`;
        badge.className = 'badge badge-muted';
        badge.style.display = '';
    } else {
        badge.style.display = 'none';
    }
}

function renderPricingProviderTabs() {
    const container = document.getElementById('pricingProviderTabs');
    if (!container) return;

    const providers = { all: 0, openai: 0, anthropic: 0, google: 0, other: 0 };
    pricingEntries.forEach(e => {
        providers.all++;
        const p = e.provider || 'other';
        providers[p] = (providers[p] || 0) + 1;
    });

    const tabs = [
        { key: 'all', label: 'All' },
        { key: 'openai', label: 'OpenAI' },
        { key: 'anthropic', label: 'Anthropic' },
        { key: 'google', label: 'Google' },
    ];
    // Add "other" tab only if there are entries
    if (providers.other > 0) tabs.push({ key: 'other', label: 'Other' });

    container.innerHTML = tabs.map(t =>
        `<button class="pricing-tab${activePricingProvider === t.key ? ' active' : ''}" onclick="filterPricingByProvider('${t.key}')">
            ${t.label} <span class="pricing-tab-count">${providers[t.key] || 0}</span>
        </button>`
    ).join('');
}

function filterPricingByProvider(provider) {
    activePricingProvider = provider;
    renderPricingProviderTabs();
    renderPricingList();
}

function renderPricingList() {
    const container = document.getElementById('pricingTable');
    if (!container) return;

    const filtered = activePricingProvider === 'all'
        ? pricingEntries
        : pricingEntries.filter(e => e.provider === activePricingProvider);

    if (filtered.length === 0) {
        container.innerHTML = `
            <div class="fallback-empty-state">
                <p>No pricing entries${activePricingProvider !== 'all' ? ` for ${activePricingProvider}` : ''}</p>
                <p class="fallback-empty-hint">Click "Add Pricing" to configure model costs</p>
            </div>`;
        return;
    }

    container.innerHTML = `
        <div class="pricing-table-header">
            <span>Model Prefix</span>
            <span class="text-right">Input/1M</span>
            <span class="text-right">Cached/1M</span>
            <span class="text-right">Output/1M</span>
            <span>Source</span>
            <span></span>
        </div>
        ${filtered.map(renderPricingRow).join('')}
    `;
}

function renderPricingRow(entry) {
    const providerDotClass = `pricing-provider-dot pricing-provider-${entry.provider || 'other'}`;
    const sourceBadge = entry.source === 'custom'
        ? '<span class="badge badge-accent-subtle">Custom</span>'
        : '<span class="badge badge-muted">Seed</span>';

    return `
        <div class="pricing-row">
            <div class="pricing-model-prefix">
                <span class="${providerDotClass}"></span>
                <code>${escapeHtml(entry.model_prefix)}</code>
            </div>
            <span class="pricing-value text-right">$${formatPrice(entry.input_per_1m)}</span>
            <span class="pricing-value text-right">${entry.cached_input_per_1m > 0 ? '$' + formatPrice(entry.cached_input_per_1m) : '\u2014'}</span>
            <span class="pricing-value text-right">$${formatPrice(entry.output_per_1m)}</span>
            <span>${sourceBadge}</span>
            <div class="fallback-config-actions">
                <button class="btn-icon" onclick="openPricingEditor(${entry.id})" title="Edit">
                    <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="8" cy="8" r="2.5"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M3.2 12.8l1.4-1.4M11.4 4.6l1.4-1.4"/></svg>
                </button>
                <button class="btn-icon btn-icon-danger" onclick="deletePricing(${entry.id}, '${escapeHtml(entry.model_prefix)}')" title="Delete">
                    <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M3 4h10M5.5 4V3a1 1 0 0 1 1-1h3a1 1 0 0 1 1 1v1M4 4l.8 9a1.5 1.5 0 0 0 1.5 1.4h3.4a1.5 1.5 0 0 0 1.5-1.4L12 4"/></svg>
                </button>
            </div>
        </div>`;
}

function formatPrice(val) {
    if (val === 0) return '0.00';
    if (val >= 1) return val.toFixed(2);
    if (val >= 0.01) return val.toFixed(3);
    return val.toFixed(4);
}

function renderUnknownModels() {
    const section = document.getElementById('pricingUnknownSection');
    const badge = document.getElementById('pricingUnknownBadge');
    if (!section) return;

    if (!unknownModels || unknownModels.length === 0) {
        section.style.display = 'none';
        if (badge) badge.style.display = 'none';
        return;
    }

    if (badge) {
        badge.textContent = `${unknownModels.length} unknown`;
        badge.className = 'badge badge-warning-subtle';
        badge.style.display = '';
    }

    section.style.display = '';
    section.innerHTML = `
        <div class="pricing-unknown-section">
            <div class="pricing-unknown-header">
                <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
                    <path d="M8 1.5l6.5 13H1.5L8 1.5z"/>
                    <line x1="8" y1="6" x2="8" y2="9"/>
                    <circle cx="8" cy="11.5" r="0.5" fill="currentColor"/>
                </svg>
                <span>Unknown Models Detected</span>
                <span class="pricing-unknown-hint">These models were seen in requests but have no pricing configured (using $3/$15 fallback)</span>
            </div>
            ${unknownModels.map(m => `
                <div class="pricing-unknown-row">
                    <code class="pricing-unknown-model">${escapeHtml(m.model)}</code>
                    <span class="pricing-unknown-hits">${m.hit_count} hit${m.hit_count !== 1 ? 's' : ''}</span>
                    <button class="btn btn-ghost btn-sm" onclick="openPricingEditorForModel('${escapeHtml(m.model)}')">Configure</button>
                </div>
            `).join('')}
        </div>`;
}

// ── Pricing Editor Modal ──────────────────────────────────────────────────

function openPricingEditor(id = null) {
    editingPricingId = id;
    const title = document.getElementById('pricingEditorTitle');
    const prefixInput = document.getElementById('pricingModelPrefix');
    const providerSelect = document.getElementById('pricingProvider');

    if (id) {
        const entry = pricingEntries.find(e => e.id === id);
        if (!entry) return;
        title.textContent = `Edit: ${entry.model_prefix}`;
        prefixInput.value = entry.model_prefix;
        providerSelect.value = entry.provider || 'other';
        document.getElementById('pricingInput').value = entry.input_per_1m;
        document.getElementById('pricingCachedInput').value = entry.cached_input_per_1m;
        document.getElementById('pricingOutput').value = entry.output_per_1m;
    } else {
        title.textContent = 'Add Model Pricing';
        prefixInput.value = '';
        providerSelect.value = 'openai';
        document.getElementById('pricingInput').value = '';
        document.getElementById('pricingCachedInput').value = '';
        document.getElementById('pricingOutput').value = '';
    }

    CustomDropdown.refresh(providerSelect);
    document.getElementById('pricingEditorOverlay').style.display = 'block';
    document.getElementById('pricingEditorModal').style.display = 'flex';
    if (!id) prefixInput.focus();
}

function openPricingEditorForModel(model) {
    editingPricingId = null;
    const title = document.getElementById('pricingEditorTitle');
    const prefixInput = document.getElementById('pricingModelPrefix');
    const providerSelect = document.getElementById('pricingProvider');

    title.textContent = 'Configure: ' + model;
    prefixInput.value = model;

    // Auto-detect provider
    if (model.startsWith('gpt-') || model.startsWith('o1') || model.startsWith('o3') || model.startsWith('o4')) {
        providerSelect.value = 'openai';
    } else if (model.startsWith('claude-')) {
        providerSelect.value = 'anthropic';
    } else if (model.startsWith('gemini-')) {
        providerSelect.value = 'google';
    } else {
        providerSelect.value = 'other';
    }

    document.getElementById('pricingInput').value = '';
    document.getElementById('pricingCachedInput').value = '';
    document.getElementById('pricingOutput').value = '';

    CustomDropdown.refresh(providerSelect);
    document.getElementById('pricingEditorOverlay').style.display = 'block';
    document.getElementById('pricingEditorModal').style.display = 'flex';
}

function closePricingEditor() {
    document.getElementById('pricingEditorOverlay').style.display = 'none';
    document.getElementById('pricingEditorModal').style.display = 'none';
    editingPricingId = null;
}

async function savePricing() {
    const prefix = document.getElementById('pricingModelPrefix').value.trim();
    if (!prefix) {
        alert('Please enter a model prefix');
        return;
    }

    const data = {
        model_prefix: prefix,
        provider: document.getElementById('pricingProvider').value,
        input_per_1m: parseFloat(document.getElementById('pricingInput').value) || 0,
        cached_input_per_1m: parseFloat(document.getElementById('pricingCachedInput').value) || 0,
        output_per_1m: parseFloat(document.getElementById('pricingOutput').value) || 0,
    };

    try {
        let response;
        if (editingPricingId) {
            response = await fetch(`/api/pricing/${editingPricingId}`, {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(data),
            });
        } else {
            response = await fetch('/api/pricing', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(data),
            });
        }

        if (!response.ok) {
            const err = await response.text();
            throw new Error(err);
        }

        closePricingEditor();
        await loadPricingData();
    } catch (err) {
        console.error('Failed to save pricing:', err);
        alert('Failed to save pricing: ' + err.message);
    }
}

async function deletePricing(id, prefix) {
    if (!confirm(`Delete pricing for "${prefix}"? The model will use fallback pricing.`)) return;
    try {
        await fetch(`/api/pricing/${id}`, { method: 'DELETE' });
        await loadPricingData();
    } catch (err) {
        console.error('Failed to delete pricing:', err);
        alert('Failed to delete pricing');
    }
}

async function resetPricingDefaults() {
    if (!confirm('Reset all model pricing to built-in defaults? This will delete any custom entries.')) return;
    try {
        const response = await fetch('/api/pricing/reset-defaults', { method: 'POST' });
        if (!response.ok) throw new Error('Failed to reset');
        await loadPricingData();
    } catch (err) {
        console.error('Failed to reset pricing defaults:', err);
        alert('Failed to reset pricing defaults');
    }
}

// Pull live model prices from OpenRouter (server-side, using OPENROUTER_API_KEY)
// and upsert them under the openrouter/<id> prefix.
async function refreshOpenRouterPricing() {
    const btn = document.getElementById('refreshOpenRouterBtn');
    const orig = btn ? btn.textContent : '';
    if (btn) { btn.disabled = true; btn.textContent = 'Refreshing…'; }
    try {
        const response = await fetch('/api/pricing/refresh-openrouter', { method: 'POST' });
        const data = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(data.error || 'refresh failed');
        await loadPricingData();
        if (btn) {
            btn.textContent = `Updated ${data.updated} models`;
            setTimeout(() => { btn.textContent = orig; btn.disabled = false; }, 2500);
        }
    } catch (err) {
        console.error('OpenRouter pricing refresh failed:', err);
        alert('OpenRouter pricing refresh failed: ' + err.message);
        if (btn) { btn.textContent = orig; btn.disabled = false; }
    }
}

// ═══════════════════════════════════════════════════════════════════════════════
// Response Cache Management
// ═══════════════════════════════════════════════════════════════════════════════

async function loadCacheStats() {
    try {
        const response = await fetch('/api/cache');
        if (!response.ok) return;
        const stats = await response.json();

        const entriesEl = document.getElementById('cacheStatsEntries');
        const hitRateEl = document.getElementById('cacheStatsHitRate');
        const savedEl   = document.getElementById('cacheStatsSaved');

        if (entriesEl) entriesEl.textContent = `${stats.entries} / ${stats.max_entries} entries`;
        if (hitRateEl) {
            const rate = stats.hits + stats.misses > 0
                ? ((stats.hits / (stats.hits + stats.misses)) * 100).toFixed(1)
                : '0.0';
            hitRateEl.textContent = `${rate}% hit rate`;
        }
        if (savedEl) savedEl.textContent = `$${(stats.cost_saved || 0).toFixed(4)} saved`;
    } catch (err) {
        console.error('Failed to load cache stats:', err);
    }
}

async function flushCache() {
    if (!confirm('Flush all cached responses? This cannot be undone.')) return;

    const btn = document.querySelector('[onclick="flushCache()"]');
    if (btn) {
        btn.disabled = true;
        btn.textContent = 'Flushing\u2026';
    }

    try {
        const response = await fetch('/api/cache/flush', { method: 'POST' });
        if (!response.ok) throw new Error('Failed to flush');
        const data = await response.json();

        if (btn) {
            btn.textContent = `Flushed ${data.flushed} entries`;
            btn.style.color = 'var(--green)';
            setTimeout(() => {
                btn.disabled = false;
                btn.textContent = 'Flush Cache';
                btn.style.color = '';
            }, 3000);
        }

        await loadCacheStats();
    } catch (err) {
        console.error('Failed to flush cache:', err);
        alert('Failed to flush cache');
        if (btn) {
            btn.disabled = false;
            btn.textContent = 'Flush Cache';
        }
    }
}

// ══════════════════════════════════════════════════════════════════════════════
//  Custom Providers Management
// ══════════════════════════════════════════════════════════════════════════════

let providersList = [];
let editingProviderId = null;

async function loadProviders() {
    try {
        const response = await fetch('/api/providers');
        if (!response.ok) return;
        providersList = await response.json();
        renderProviders();
        updateProviderBadge();
    } catch (e) {
        console.error('Failed to load custom providers:', e);
    }
}

function updateProviderBadge() {
    const badge = document.getElementById('providerCountBadge');
    if (!badge) return;
    const count = providersList.filter(p => p.enabled).length;
    badge.textContent = `${count} provider${count !== 1 ? 's' : ''}`;
    badge.className = count > 0 ? 'badge badge-hit-count' : 'badge badge-muted';
}

function renderProviders() {
    const container = document.getElementById('customProvidersList');
    if (!container) return;

    if (providersList.length === 0) {
        container.innerHTML = `
            <div class="fallback-empty-state">
                <p>No custom providers configured</p>
                <p class="fallback-empty-hint">Click "Add Provider" to connect Ollama, vLLM, DeepSeek, or any OpenAI-compatible endpoint</p>
            </div>`;
        return;
    }

    container.innerHTML = providersList.map(renderProviderRow).join('');
}

function renderProviderRow(provider) {
    const models = (provider.models || []).slice(0, 4);
    const moreCount = (provider.models || []).length - 4;
    const modelsStr = models.map(m => `<code style="font-size:11px;background:var(--bg-secondary);padding:1px 5px;border-radius:3px;">${escapeHtml(m)}</code>`).join(' ');
    const moreStr = moreCount > 0 ? `<span style="font-size:11px;color:var(--text-muted);"> +${moreCount} more</span>` : '';

    return `
        <div class="fallback-config-row" style="opacity:${provider.enabled ? 1 : 0.6}">
            <div class="fallback-config-top">
                <div class="fallback-config-chain" style="align-items:center;">
                    <span class="fallback-chain-source">${escapeHtml(provider.display_name || provider.name)}</span>
                    <code class="provider-url">${escapeHtml(provider.base_url)}</code>
                    <span class="badge badge-muted" style="font-size:10px;">${provider.api_format}</span>
                </div>
                <div class="fallback-config-actions">
                    <button class="btn-icon" onclick="testExistingProvider(${provider.id})" title="Test Connection">
                        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
                            <path d="M13 3l-8 8M13 3h-5M13 3v5"/>
                        </svg>
                    </button>
                    <button class="btn-icon" onclick="editProvider(${provider.id})" title="Edit">
                        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
                            <circle cx="8" cy="8" r="2.5"/>
                            <path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M3.2 12.8l1.4-1.4M11.4 4.6l1.4-1.4"/>
                        </svg>
                    </button>
                    <button class="btn-icon" onclick="toggleProvider(${provider.id}, ${!provider.enabled})" title="${provider.enabled ? 'Disable' : 'Enable'}">
                        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="${provider.enabled ? 'var(--green)' : 'var(--text-muted)'}" stroke-width="1.5">
                            ${provider.enabled
                                ? '<circle cx="8" cy="8" r="6"/><path d="M5 8l2 2 4-4"/>'
                                : '<circle cx="8" cy="8" r="6"/><path d="M6 6l4 4M10 6l-4 4"/>'}
                        </svg>
                    </button>
                    <button class="btn-icon btn-icon-danger" onclick="deleteProvider(${provider.id})" title="Delete">
                        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5">
                            <path d="M3 4h10M5 4V3h6v1M6 7v4M10 7v4M4 4l1 9h6l1-9"/>
                        </svg>
                    </button>
                </div>
            </div>
            <div class="fallback-config-status">
                ${provider.enabled
                    ? '<span class="badge badge-success">Enabled</span>'
                    : '<span class="badge badge-muted">Disabled</span>'}
                <span style="font-size:11px;color:var(--text-muted);margin-left:8px;">
                    Usage: <code style="background:var(--bg-secondary);padding:1px 5px;border-radius:3px;">${escapeHtml(provider.name)}/model-name</code>
                </span>
                ${modelsStr ? `<span style="margin-left:8px;">${modelsStr}${moreStr}</span>` : ''}
                <span id="providerTestResult_${provider.id}" style="font-size:11px;margin-left:8px;"></span>
            </div>
        </div>`;
}

function openProviderEditor(id) {
    editingProviderId = id || null;
    const title = document.getElementById('providerEditorTitle');
    const nameInput = document.getElementById('providerName');

    // Reset form
    document.getElementById('providerName').value = '';
    document.getElementById('providerDisplayName').value = '';
    document.getElementById('providerBaseURL').value = '';
    document.getElementById('providerAPIFormat').value = 'openai';
    document.getElementById('providerAPIPath').value = '/v1/chat/completions';
    document.getElementById('providerAuthKey').value = '';
    document.getElementById('providerAuthEnvVar').value = '';
    document.getElementById('providerModels').value = '';
    document.getElementById('testProviderResult').textContent = '';

    if (id) {
        title.textContent = 'Edit Custom Provider';
        nameInput.disabled = true; // Name is immutable
        const p = providersList.find(p => p.id === id);
        if (p) {
            nameInput.value = p.name;
            document.getElementById('providerDisplayName').value = p.display_name || '';
            document.getElementById('providerBaseURL').value = p.base_url || '';
            document.getElementById('providerAPIFormat').value = p.api_format || 'openai';
            document.getElementById('providerAPIPath').value = p.api_path || '/v1/chat/completions';
            document.getElementById('providerAuthKey').value = p.auth_header ? '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022' : '';
            document.getElementById('providerAuthEnvVar').value = p.auth_env_var || '';
            document.getElementById('providerModels').value = (p.models || []).join('\n');
        }
    } else {
        title.textContent = 'Add Custom Provider';
        nameInput.disabled = false;
    }

    document.getElementById('providerEditorOverlay').style.display = 'block';
    document.getElementById('providerEditorModal').style.display = 'flex';
}

function closeProviderEditor() {
    document.getElementById('providerEditorOverlay').style.display = 'none';
    document.getElementById('providerEditorModal').style.display = 'none';
    editingProviderId = null;
}

async function saveProvider() {
    const name = document.getElementById('providerName').value.trim().toLowerCase();
    const displayName = document.getElementById('providerDisplayName').value.trim();
    const baseURL = document.getElementById('providerBaseURL').value.trim();
    const apiFormat = document.getElementById('providerAPIFormat').value;
    const apiPath = document.getElementById('providerAPIPath').value.trim() || '/v1/chat/completions';
    const authKeyRaw = document.getElementById('providerAuthKey').value.trim();
    const authEnvVar = document.getElementById('providerAuthEnvVar').value.trim();
    const modelsText = document.getElementById('providerModels').value.trim();
    const models = modelsText ? modelsText.split('\n').map(m => m.trim()).filter(m => m) : [];

    // Client-side validation
    if (!name) { alert('Provider name is required'); return; }
    if (!baseURL) { alert('Base URL is required'); return; }

    // Build auth_header — only update if user typed something new (not the mask)
    let authHeader = '';
    if (authKeyRaw && authKeyRaw !== '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022') {
        authHeader = authKeyRaw.startsWith('Bearer ') ? authKeyRaw : 'Bearer ' + authKeyRaw;
    } else if (editingProviderId) {
        // Keep existing auth_header
        const existing = providersList.find(p => p.id === editingProviderId);
        authHeader = existing ? existing.auth_header : '';
    }

    const body = {
        name,
        display_name: displayName,
        base_url: baseURL,
        api_format: apiFormat,
        api_path: apiPath,
        auth_header: authHeader,
        auth_env_var: authEnvVar,
        models,
        enabled: true,
    };

    try {
        let response;
        if (editingProviderId) {
            response = await fetch(`/api/providers/${editingProviderId}`, {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body),
            });
        } else {
            response = await fetch('/api/providers', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body),
            });
        }

        if (!response.ok) {
            const err = await response.json().catch(() => ({ error: 'Unknown error' }));
            alert(err.error || 'Failed to save provider');
            return;
        }

        closeProviderEditor();
        await loadProviders();
        // Refresh model dropdowns
        if (typeof loadCustomProviderModels === 'function') loadCustomProviderModels();
    } catch (e) {
        alert('Failed to save provider: ' + e.message);
    }
}

async function deleteProvider(id) {
    const p = providersList.find(p => p.id === id);
    const name = p ? p.name : 'this provider';
    if (!confirm(`Delete custom provider "${name}"? This cannot be undone.`)) return;

    try {
        const response = await fetch(`/api/providers/${id}`, { method: 'DELETE' });
        if (!response.ok) throw new Error('Delete failed');
        await loadProviders();
    } catch (e) {
        alert('Failed to delete provider: ' + e.message);
    }
}

async function toggleProvider(id, enabled) {
    try {
        const response = await fetch(`/api/providers/${id}/toggle`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled }),
        });
        if (!response.ok) throw new Error('Toggle failed');
        await loadProviders();
    } catch (e) {
        alert('Failed to toggle provider: ' + e.message);
    }
}

function editProvider(id) {
    openProviderEditor(id);
}

async function testProviderConnection() {
    const btn = document.getElementById('testProviderBtn');
    const result = document.getElementById('testProviderResult');
    btn.disabled = true;
    result.innerHTML = '<span style="color:var(--text-muted);">Testing...</span>';

    const baseURL = document.getElementById('providerBaseURL').value.trim();
    const authKeyRaw = document.getElementById('providerAuthKey').value.trim();
    const authEnvVar = document.getElementById('providerAuthEnvVar').value.trim();

    let authHeader = '';
    if (authKeyRaw && authKeyRaw !== '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022') {
        authHeader = authKeyRaw.startsWith('Bearer ') ? authKeyRaw : 'Bearer ' + authKeyRaw;
    }

    try {
        const response = await fetch('/api/providers/test', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                base_url: baseURL,
                auth_header: authHeader,
                auth_env_var: authEnvVar,
            }),
        });
        const data = await response.json();

        if (data.reachable) {
            let msg = `<span style="color:var(--green);">&#10003; Connected (${data.latency_ms}ms)</span>`;
            if (data.note) msg += `<br><span style="color:var(--text-muted);font-size:11px;">${escapeHtml(data.note)}</span>`;
            if (data.models && data.models.length > 0) {
                msg += `<br><span style="color:var(--text-muted);font-size:11px;">Found ${data.models.length} model(s)</span>`;
                // Auto-fill models if textarea is empty
                const textarea = document.getElementById('providerModels');
                if (!textarea.value.trim()) {
                    textarea.value = data.models.join('\n');
                }
            }
            result.innerHTML = msg;
        } else {
            result.innerHTML = `<span style="color:var(--red);">&#10007; ${escapeHtml(data.error || 'Connection failed')}</span>`;
        }
    } catch (e) {
        result.innerHTML = `<span style="color:var(--red);">&#10007; ${escapeHtml(e.message)}</span>`;
    }

    btn.disabled = false;
}

async function testExistingProvider(id) {
    const resultSpan = document.getElementById(`providerTestResult_${id}`);
    if (resultSpan) resultSpan.innerHTML = '<span style="color:var(--text-muted);">Testing...</span>';

    try {
        const response = await fetch(`/api/providers/${id}/test`, { method: 'POST' });
        const data = await response.json();

        if (resultSpan) {
            if (data.reachable) {
                resultSpan.innerHTML = `<span style="color:var(--green);">&#10003; ${data.latency_ms}ms</span>`;
            } else {
                resultSpan.innerHTML = `<span style="color:var(--red);">&#10007; unreachable</span>`;
            }
            setTimeout(() => { resultSpan.textContent = ''; }, 5000);
        }
    } catch (e) {
        if (resultSpan) {
            resultSpan.innerHTML = `<span style="color:var(--red);">&#10007; error</span>`;
            setTimeout(() => { resultSpan.textContent = ''; }, 5000);
        }
    }
}

// ═══════════════════════════════════════════════════════════════════════════════
// Security — PII Redaction
// ═══════════════════════════════════════════════════════════════════════════════

// All PII/secret category definitions — must match Go redactor/patterns.go AllCategories
const PII_CATEGORIES = [
    { key: 'email',             label: 'Email Address',        group: 'pii',    tag: '[REDACTED_EMAIL]' },
    { key: 'phone',             label: 'Phone Number',         group: 'pii',    tag: '[REDACTED_PHONE]' },
    { key: 'ssn',               label: 'Social Security Number', group: 'pii',  tag: '[REDACTED_SSN]' },
    { key: 'credit_card',       label: 'Credit Card Number',   group: 'pii',    tag: '[REDACTED_CC]' },
    { key: 'iban',              label: 'IBAN',                  group: 'pii',    tag: '[REDACTED_IBAN]' },
    { key: 'ip_address',        label: 'IP Address',           group: 'pii',    tag: '[REDACTED_IP]' },
    { key: 'date_of_birth',     label: 'Date of Birth',        group: 'pii',    tag: '[REDACTED_DOB]' },
    { key: 'zip_code',          label: 'US Zip Code',          group: 'pii',    tag: '[REDACTED_ZIP]' },
    { key: 'openai_key',        label: 'OpenAI API Key',       group: 'secret', tag: '[REDACTED_OPENAI_KEY]' },
    { key: 'aws_key',           label: 'AWS Access Key',       group: 'secret', tag: '[REDACTED_AWS_KEY]' },
    { key: 'github_token',      label: 'GitHub Token',         group: 'secret', tag: '[REDACTED_GITHUB_TOKEN]' },
    { key: 'generic_api_key',   label: 'Generic API Key',      group: 'secret', tag: '[REDACTED_API_KEY]' },
    { key: 'jwt',               label: 'JWT Token',            group: 'secret', tag: '[REDACTED_JWT]' },
    { key: 'bearer_token',      label: 'Bearer Token',         group: 'secret', tag: '[REDACTED_BEARER]' },
    { key: 'private_key',       label: 'Private Key (PEM/SSH)', group: 'secret', tag: '[REDACTED_PRIVATE_KEY]' },
    { key: 'connection_string', label: 'Connection String',    group: 'secret', tag: '[REDACTED_CONN_STRING]' },
    { key: 'env_password',      label: 'Env Password/Secret',  group: 'secret', tag: '[REDACTED_ENV_SECRET]' },
];

/**
 * Toggle a pattern card on/off when clicked.
 */
function togglePatternCard(card) {
    const cb = card.querySelector('input[type="checkbox"]');
    if (!cb) return;
    cb.checked = !cb.checked;
    card.classList.toggle('is-active', cb.checked);
    updatePatternCounters();
}

/**
 * Update the "N / M" active counters for PII and Secret groups.
 */
function updatePatternCounters() {
    const piiAll = document.querySelectorAll('#piiPatternsGrid .pattern-card');
    const piiOn  = document.querySelectorAll('#piiPatternsGrid .pattern-card.is-active');
    const piiEl  = document.getElementById('piiActiveCount');
    if (piiEl) piiEl.textContent = `${piiOn.length} / ${piiAll.length}`;

    const secAll = document.querySelectorAll('#secretPatternsGrid .pattern-card');
    const secOn  = document.querySelectorAll('#secretPatternsGrid .pattern-card.is-active');
    const secEl  = document.getElementById('secretActiveCount');
    if (secEl) secEl.textContent = `${secOn.length} / ${secAll.length}`;
}

/**
 * Render the PII and Secret category cards into their respective grids.
 * @param {string} enabledCSV — comma-separated keys of enabled categories (empty = all)
 */
function renderPIICategories(enabledCSV) {
    const enabledSet = new Set();
    if (enabledCSV && enabledCSV.trim()) {
        enabledCSV.split(',').forEach(k => enabledSet.add(k.trim()));
    }
    const allEnabled = enabledSet.size === 0;

    const piiGrid = document.getElementById('piiPatternsGrid');
    const secretGrid = document.getElementById('secretPatternsGrid');
    if (!piiGrid || !secretGrid) return;

    piiGrid.innerHTML = '';
    secretGrid.innerHTML = '';

    PII_CATEGORIES.forEach(cat => {
        const checked = allEnabled || enabledSet.has(cat.key);
        const activeClass = checked ? ' is-active' : '';
        const html = `
            <div class="pattern-card${activeClass}" onclick="togglePatternCard(this)">
                <input type="checkbox" data-pii-key="${cat.key}" data-pii-group="${cat.group}" ${checked ? 'checked' : ''}>
                <div class="pattern-card-text">
                    <div class="pattern-card-label">${cat.label}</div>
                    <div class="pattern-card-tag">${cat.tag}</div>
                </div>
                <div class="pattern-toggle">
                    <div class="pattern-toggle-track"></div>
                    <div class="pattern-toggle-thumb"></div>
                </div>
            </div>`;
        if (cat.group === 'pii') {
            piiGrid.insertAdjacentHTML('beforeend', html);
        } else {
            secretGrid.insertAdjacentHTML('beforeend', html);
        }
    });

    updatePatternCounters();
}

/**
 * Collect checked PII categories into a comma-separated string.
 * If all are checked, returns empty string (meaning "all").
 */
function collectPIICategories() {
    const checkboxes = document.querySelectorAll('input[data-pii-key]');
    const checked = [];
    let total = 0;
    checkboxes.forEach(cb => {
        total++;
        if (cb.checked) checked.push(cb.dataset.piiKey);
    });
    if (checked.length === total) return '';
    return checked.join(',');
}

/**
 * Enable or disable all PII-group pattern cards.
 * @param {boolean} enable
 */
function selectAllPII(enable) {
    document.querySelectorAll('#piiPatternsGrid .pattern-card').forEach(card => {
        const cb = card.querySelector('input[type="checkbox"]');
        if (cb) cb.checked = enable;
        card.classList.toggle('is-active', enable);
    });
    updatePatternCounters();
}

/**
 * Enable or disable all secret-group pattern cards.
 * @param {boolean} enable
 */
function selectAllSecrets(enable) {
    document.querySelectorAll('#secretPatternsGrid .pattern-card').forEach(card => {
        const cb = card.querySelector('input[type="checkbox"]');
        if (cb) cb.checked = enable;
        card.classList.toggle('is-active', enable);
    });
    updatePatternCounters();
}

/**
 * Load memory store stats and populate the memory stats bar.
 */
async function loadMemoryStats() {
    try {
        const response = await fetch('/api/memories');
        if (!response.ok) return;
        const data = await response.json();

        const entriesEl  = document.getElementById('memoryStatsEntries');
        const hitRateEl  = document.getElementById('memoryStatsHitRate');
        const agentsEl   = document.getElementById('memoryStatsAgents');
        const staleEl    = document.getElementById('memoryStatsStale');

        if (entriesEl) entriesEl.textContent = `${data.total || 0} memories`;
        if (hitRateEl) {
            const lookups = (data.lookups || 0);
            const hits    = (data.hits || 0);
            const rate    = lookups > 0 ? ((hits / lookups) * 100).toFixed(1) : '0.0';
            hitRateEl.textContent = `${rate}% hit rate`;
        }
        if (agentsEl) {
            const breakdown = data.agent_breakdown || [];
            agentsEl.textContent = `${breakdown.length} agent${breakdown.length !== 1 ? 's' : ''}`;
        }
        if (staleEl) {
            const stale = data.stale_count || 0;
            staleEl.textContent = `${stale} stale`;
            staleEl.style.color = stale > 0 ? 'var(--yellow)' : 'var(--text-muted)';
        }
    } catch (err) {
        console.error('Failed to load memory stats:', err);
    }
}

/**
 * Flush all stored memories with confirmation.
 */
async function flushMemories() {
    if (!confirm('Flush all stored memories? This cannot be undone.')) return;

    const btn = document.querySelector('[onclick="flushMemories()"]');
    if (btn) {
        btn.disabled = true;
        btn.textContent = 'Flushing\u2026';
    }

    try {
        const response = await fetch('/api/memories/flush', { method: 'POST' });
        if (!response.ok) throw new Error('Failed to flush memories');
        const data = await response.json();

        if (btn) {
            btn.textContent = `Flushed ${data.flushed || 0} memories`;
            btn.style.color = 'var(--green)';
            setTimeout(() => {
                btn.disabled = false;
                btn.textContent = 'Flush Memories';
                btn.style.color = '';
            }, 2000);
        }

        loadMemoryStats();
    } catch (err) {
        console.error('Failed to flush memories:', err);
        if (btn) {
            btn.textContent = 'Flush failed';
            btn.style.color = 'var(--red)';
            setTimeout(() => {
                btn.disabled = false;
                btn.textContent = 'Flush Memories';
                btn.style.color = '';
            }, 2000);
        }
    }
}
