// ══════════════════════════════════════════════════════════════════════════════
//  Custom Dropdown Component — Single source of truth for all <select> UI
//
//  Usage:
//    Static:   <select data-custom-dropdown> ... </select>
//    Dynamic:  CustomDropdown.upgrade(selectElement)      — wrap a native select
//              CustomDropdown.refresh(selectElement)      — re-read options after change
//              CustomDropdown.refreshAll()                — re-read all managed selects
//
//  Every <select> on the page should use this. No native dropdowns.
// ══════════════════════════════════════════════════════════════════════════════

class CustomDropdown {
    // ── Registry — tracks every managed select ───────────────────────────
    static _instances = new Map();   // nativeSelect → CustomDropdown

    /** Wrap a native <select> if not already wrapped. Returns the instance. */
    static upgrade(select) {
        if (CustomDropdown._instances.has(select)) {
            return CustomDropdown._instances.get(select);
        }
        return new CustomDropdown(select);
    }

    /** Re-read options from a native <select> whose <option>s changed. */
    static refresh(select) {
        const inst = CustomDropdown._instances.get(select);
        if (inst) inst.refresh();
    }

    /** Re-sync every managed dropdown (useful after bulk value changes). */
    static refreshAll() {
        CustomDropdown._instances.forEach(inst => inst.refresh());
    }

    /** Destroy wrapper for a native <select> and restore it. */
    static destroy(select) {
        const inst = CustomDropdown._instances.get(select);
        if (inst) inst.destroy();
    }

    // ── Instance ─────────────────────────────────────────────────────────
    constructor(element) {
        this.nativeSelect = element;
        this.isOpen = false;
        this.selectedIndex = element.selectedIndex;
        this.searchInput = null;

        this._build();
        this._bindEvents();
        CustomDropdown._instances.set(element, this);
    }

    // ── Public: re-read options from native select ───────────────────────
    refresh() {
        this.selectedIndex = this.nativeSelect.selectedIndex;
        // Rebuild search if option count crossed threshold
        this._rebuildSearch();
        this.renderOptions();
        this._updateTrigger();
    }

    // ── Build DOM ────────────────────────────────────────────────────────
    _build() {
        this.container = document.createElement('div');
        this.container.className = 'custom-dropdown';
        if (this.nativeSelect.disabled) {
            this.container.classList.add('disabled');
        }

        // Copy size class from native select for compact variants
        if (this.nativeSelect.classList.contains('form-select-sm') ||
            this.nativeSelect.classList.contains('setting-input-sm')) {
            this.container.classList.add('custom-dropdown--sm');
        }

        // Trigger button
        this.trigger = document.createElement('button');
        this.trigger.type = 'button';
        this.trigger.className = 'custom-dropdown-trigger';
        this.trigger.innerHTML = `
            <span class="custom-dropdown-value">${this._getSelectedText()}</span>
            <svg class="custom-dropdown-chevron" width="12" height="8" viewBox="0 0 12 8" fill="none">
                <path d="M1 1.5L6 6.5L11 1.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
            </svg>
        `;

        // Menu
        this.menu = document.createElement('div');
        this.menu.className = 'custom-dropdown-menu';

        this._rebuildSearch();

        // Options container
        this.optionsList = document.createElement('div');
        this.optionsList.className = 'custom-dropdown-options';
        this.renderOptions();
        this.menu.appendChild(this.optionsList);

        // Assemble
        this.container.appendChild(this.trigger);
        this.container.appendChild(this.menu);

        // Replace native
        this.nativeSelect.style.display = 'none';
        this.nativeSelect.parentNode.insertBefore(this.container, this.nativeSelect);
    }

    _rebuildSearch() {
        const count = this.nativeSelect.options.length;
        if (count > 8 && !this.searchInput) {
            this.searchInput = document.createElement('input');
            this.searchInput.type = 'text';
            this.searchInput.className = 'custom-dropdown-search';
            this.searchInput.placeholder = 'Search…';
            this.menu.insertBefore(this.searchInput, this.menu.firstChild);
            this.searchInput.addEventListener('input', (e) => {
                this.renderOptions(e.target.value);
            });
            this.searchInput.addEventListener('click', (e) => e.stopPropagation());
        } else if (count <= 8 && this.searchInput) {
            this.searchInput.remove();
            this.searchInput = null;
        }
    }

    // ── Render options list ──────────────────────────────────────────────
    renderOptions(filter = '') {
        this.optionsList.innerHTML = '';
        const nativeOpts = Array.from(this.nativeSelect.options);

        let visible = 0;
        nativeOpts.forEach((option, index) => {
            const text = option.textContent.trim();
            if (filter && !text.toLowerCase().includes(filter.toLowerCase())) return;

            const el = document.createElement('div');
            el.className = 'custom-dropdown-option';
            el.dataset.index = index;
            if (index === this.selectedIndex) el.classList.add('selected');

            const check = index === this.selectedIndex
                ? '<svg width="14" height="14" viewBox="0 0 14 14" fill="none" class="custom-dropdown-check"><path d="M11.6667 3.5L5.25004 9.91667L2.33337 7" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>'
                : '<span style="width:14px;display:inline-block;"></span>';

            el.innerHTML = `${check}<span class="custom-dropdown-option-text">${text}</span>`;
            el.addEventListener('click', (e) => { e.stopPropagation(); this._selectOption(index); });
            this.optionsList.appendChild(el);
            visible++;
        });

        if (visible === 0) {
            const empty = document.createElement('div');
            empty.className = 'custom-dropdown-no-results';
            empty.textContent = 'No results found';
            this.optionsList.appendChild(empty);
        }
    }

    // ── Events ───────────────────────────────────────────────────────────
    _bindEvents() {
        this.trigger.addEventListener('click', (e) => { e.stopPropagation(); this.toggle(); });

        document.addEventListener('click', (e) => {
            if (!this.container.contains(e.target)) this.close();
        });

        this.trigger.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); this.toggle(); }
            else if (e.key === 'ArrowDown') { e.preventDefault(); this.isOpen ? this._navigate(1) : this.open(); }
            else if (e.key === 'ArrowUp')   { e.preventDefault(); if (this.isOpen) this._navigate(-1); }
            else if (e.key === 'Escape')    { this.close(); }
        });

        // External code sets .value on native select → sync trigger
        this.nativeSelect.addEventListener('change', () => {
            this.selectedIndex = this.nativeSelect.selectedIndex;
            this._updateTrigger();
            this.renderOptions();
        });
    }

    // ── Open / close ─────────────────────────────────────────────────────
    open() {
        if (this.nativeSelect.disabled) return;
        this.isOpen = true;
        this.container.classList.add('open');
        if (this.searchInput) setTimeout(() => this.searchInput.focus(), 50);
        const sel = this.optionsList.querySelector('.selected');
        if (sel) sel.scrollIntoView({ block: 'nearest' });
    }

    close() {
        this.isOpen = false;
        this.container.classList.remove('open');
        if (this.searchInput) { this.searchInput.value = ''; this.renderOptions(); }
    }

    toggle() { this.isOpen ? this.close() : this.open(); }

    // ── Select ───────────────────────────────────────────────────────────
    _selectOption(index) {
        this.selectedIndex = index;
        this.nativeSelect.selectedIndex = index;
        this.nativeSelect.dispatchEvent(new Event('change', { bubbles: true }));
        this._updateTrigger();
        this.renderOptions();
        this.close();
    }

    _updateTrigger() {
        this.trigger.querySelector('.custom-dropdown-value').textContent = this._getSelectedText();
    }

    _getSelectedText() {
        const opt = this.nativeSelect.options[this.selectedIndex];
        return opt ? opt.textContent.trim() : '';
    }

    _navigate(dir) {
        const opts = Array.from(this.optionsList.querySelectorAll('.custom-dropdown-option'));
        const cur = opts.findIndex(o => parseInt(o.dataset.index) === this.selectedIndex);
        const next = cur + dir;
        if (next >= 0 && next < opts.length) {
            this._selectOption(parseInt(opts[next].dataset.index));
        }
    }

    // ── Destroy ──────────────────────────────────────────────────────────
    destroy() {
        this.container.remove();
        this.nativeSelect.style.display = '';
        CustomDropdown._instances.delete(this.nativeSelect);
    }
}

// ── Auto-init: every <select data-custom-dropdown> on page load ──────────
function initCustomDropdowns() {
    document.querySelectorAll('select[data-custom-dropdown]').forEach(sel => {
        CustomDropdown.upgrade(sel);
    });
}

if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initCustomDropdowns);
} else {
    initCustomDropdowns();
}

window.CustomDropdown = CustomDropdown;
window.initCustomDropdowns = initCustomDropdowns;
