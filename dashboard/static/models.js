// ══════════════════════════════════════════════════════════════════════════════
//  Centralized model definitions — the single source of truth for all UI pages.
//  Kept in sync with proxy/handler.go (aliases) and tracker/cost_calculator.go.
// ══════════════════════════════════════════════════════════════════════════════

const MODELS = {
    openai: [
        // GPT-5
        { value: 'gpt-5.2-pro', label: 'GPT-5.2 Pro',  tier: 'expensive', cost: '$21 / $168' },
        { value: 'gpt-5.2',     label: 'GPT-5.2',       tier: 'premium',   cost: '$1.75 / $14' },
        { value: 'gpt-5-mini',  label: 'GPT-5 Mini',    tier: 'cheap',     cost: '$0.25 / $2' },
        // GPT-4o
        { value: 'gpt-4o',      label: 'GPT-4o',        tier: 'premium',   cost: '$5 / $15' },
        { value: 'gpt-4o-mini', label: 'GPT-4o Mini',   tier: 'cheap',     cost: '$0.15 / $0.60' },
        // Reasoning
        { value: 'o4-mini',     label: 'o4-mini',        tier: 'mid',       cost: '$1.10 / $4.40' },
        { value: 'o3',          label: 'o3',             tier: 'premium',   cost: '$10 / $40' },
        { value: 'o3-mini',     label: 'o3-mini',        tier: 'mid',       cost: '$1.10 / $4.40' },
        { value: 'o1',          label: 'o1',             tier: 'expensive', cost: '$15 / $60' },
        { value: 'o1-mini',     label: 'o1 Mini',        tier: 'mid',       cost: '$1.10 / $4.40' },
        // Legacy
        { value: 'gpt-4-turbo', label: 'GPT-4 Turbo',   tier: 'premium',   cost: '$10 / $30' },
        { value: 'gpt-4',       label: 'GPT-4',          tier: 'expensive', cost: '$30 / $60' },
        { value: 'gpt-3.5-turbo', label: 'GPT-3.5 Turbo', tier: 'cheap',   cost: '$0.50 / $1.50' },
    ],
    anthropic: [
        // Claude 4.x (latest first)
        { value: 'claude-opus-4-6',   label: 'Claude Opus 4.6',   tier: 'premium',   cost: '$5 / $25' },
        { value: 'claude-opus-4-5',   label: 'Claude Opus 4.5',   tier: 'premium',   cost: '$5 / $25' },
        { value: 'claude-sonnet-4-5', label: 'Claude Sonnet 4.5', tier: 'premium',   cost: '$3 / $15' },
        { value: 'claude-sonnet-4',   label: 'Claude Sonnet 4',   tier: 'premium',   cost: '$3 / $15' },
        { value: 'claude-haiku-4-5',  label: 'Claude Haiku 4.5',  tier: 'cheap',     cost: '$1 / $5' },
        { value: 'claude-opus-4-1',   label: 'Claude Opus 4.1',   tier: 'expensive', cost: '$15 / $75' },
        { value: 'claude-opus-4',     label: 'Claude Opus 4',     tier: 'expensive', cost: '$15 / $75' },
        // Claude 3.x
        { value: 'claude-sonnet-3-7', label: 'Claude Sonnet 3.7', tier: 'premium',   cost: '$3 / $15' },
        { value: 'claude-3-5-sonnet', label: 'Claude 3.5 Sonnet', tier: 'premium',   cost: '$3 / $15' },
        { value: 'claude-3-5-haiku',  label: 'Claude 3.5 Haiku',  tier: 'cheap',     cost: '$0.80 / $4' },
        { value: 'claude-3-opus',     label: 'Claude 3 Opus',     tier: 'expensive', cost: '$15 / $75' },
        { value: 'claude-3-haiku',    label: 'Claude 3 Haiku',    tier: 'cheap',     cost: '$0.25 / $1.25' },
    ],
    google: [
        // Gemini 3
        { value: 'gemini-3-pro-preview',   label: 'Gemini 3 Pro',       tier: 'premium', cost: '$2 / $12' },
        { value: 'gemini-3-flash-preview',  label: 'Gemini 3 Flash',     tier: 'cheap',   cost: '$0.50 / $3' },
        // Gemini 2.5
        { value: 'gemini-2.5-pro',   label: 'Gemini 2.5 Pro',   tier: 'premium', cost: '$1.25 / $10' },
        { value: 'gemini-2.5-flash', label: 'Gemini 2.5 Flash', tier: 'cheap',   cost: '$0.075 / $0.30' },
        // Gemini 2.0
        { value: 'gemini-2.0-flash', label: 'Gemini 2.0 Flash', tier: 'cheap',   cost: '$0.10 / $0.40' },
        // Gemini 1.5
        { value: 'gemini-1.5-pro',   label: 'Gemini 1.5 Pro',   tier: 'premium', cost: '$1.25 / $5' },
        { value: 'gemini-1.5-flash', label: 'Gemini 1.5 Flash', tier: 'cheap',   cost: '$0.075 / $0.30' },
    ]
};

// Flat list of all models for searching
const ALL_MODELS = [
    ...MODELS.openai.map(m => ({ ...m, provider: 'openai' })),
    ...MODELS.anthropic.map(m => ({ ...m, provider: 'anthropic' })),
    ...MODELS.google.map(m => ({ ...m, provider: 'google' })),
];

// Provider display names
const PROVIDERS = {
    openai: 'OpenAI',
    anthropic: 'Anthropic',
    google: 'Google'
};

// ── Provider/model group mapping for rules UI ─────────────────────────────────
// Groups models by provider sub-family for the rules editor dropdown.
const MODEL_GROUPS = [
    { group: 'Anthropic Claude 4.x', models: MODELS.anthropic.filter(m => !m.value.startsWith('claude-3')) },
    { group: 'Anthropic Claude 3.x', models: MODELS.anthropic.filter(m => m.value.startsWith('claude-3') || m.value.startsWith('claude-sonnet-3')) },
    { group: 'OpenAI GPT-5',         models: MODELS.openai.filter(m => m.value.startsWith('gpt-5')) },
    { group: 'OpenAI GPT-4o',        models: MODELS.openai.filter(m => m.value.startsWith('gpt-4o')) },
    { group: 'OpenAI Reasoning',     models: MODELS.openai.filter(m => /^o\d/.test(m.value)) },
    { group: 'Google Gemini',        models: MODELS.google },
];

// Get models for a specific provider
function getModelsForProvider(provider) {
    return MODELS[provider] || [];
}

// Get provider label
function getProviderLabel(provider) {
    return PROVIDERS[provider] || provider;
}

// Find model info
function getModelInfo(provider, modelValue) {
    const models = MODELS[provider] || [];
    return models.find(m => m.value === modelValue) || { value: modelValue, label: modelValue, tier: 'unknown', cost: '' };
}

// Get all providers
function getAllProviders() {
    return Object.keys(MODELS);
}

// ── Custom provider dynamic merge ───────────────────────────────────────────
// Fetches custom providers from the API and merges them into MODELS/PROVIDERS
// so all dropdowns (rules, fallback, pricing) include custom providers.

async function loadCustomProviderModels() {
    try {
        const response = await fetch('/api/providers');
        if (!response.ok) return;
        const customProviders = await response.json();

        customProviders.forEach(cp => {
            if (!cp.enabled) return;

            // Add to PROVIDERS display name map
            PROVIDERS[cp.name] = cp.display_name || cp.name;

            // Add models for this provider
            MODELS[cp.name] = (cp.models || []).map(m => ({
                value: m,
                label: m,
                tier: 'custom',
                cost: 'Custom'
            }));

            // Add to ALL_MODELS flat list
            (cp.models || []).forEach(m => {
                ALL_MODELS.push({
                    value: m,
                    label: m,
                    tier: 'custom',
                    cost: 'Custom',
                    provider: cp.name
                });
            });
        });
    } catch (e) {
        // Silently fail — custom providers are optional
        console.debug('Custom providers not loaded:', e.message);
    }
}
