'use strict';

(() => {
  const STORAGE_KEY = 'melodyshare-theme';
  const ORDER = ['system', 'light', 'dark'];
  const VALID = new Set(ORDER);
  const LABELS = { system: '跟随系统', light: '浅色', dark: '深色' };
  const media = window.matchMedia('(prefers-color-scheme: dark)');

  function getPreference() {
    try {
      const value = localStorage.getItem(STORAGE_KEY) || 'system';
      return VALID.has(value) ? value : 'system';
    } catch {
      return 'system';
    }
  }

  function resolvedTheme(preference) {
    return preference === 'system' ? (media.matches ? 'dark' : 'light') : preference;
  }

  function apply(preference) {
    const safePreference = VALID.has(preference) ? preference : 'system';
    const theme = resolvedTheme(safePreference);
    document.documentElement.dataset.theme = theme;
    document.documentElement.dataset.themePreference = safePreference;
    document.documentElement.style.colorScheme = theme;
    document.querySelectorAll('[data-theme-control]').forEach((control) => {
      control.setAttribute('aria-label', `外观：${LABELS[safePreference]}，点击切换`);
      control.title = `外观：${LABELS[safePreference]}`;
    });
  }

  function setPreference(preference) {
    const safePreference = VALID.has(preference) ? preference : 'system';
    try {
      localStorage.setItem(STORAGE_KEY, safePreference);
    } catch {
      // The active page still receives the preference when storage is unavailable.
    }
    apply(safePreference);
  }

  apply(getPreference());
  media.addEventListener('change', () => {
    if (getPreference() === 'system') apply('system');
  });
  document.addEventListener('DOMContentLoaded', () => {
    apply(getPreference());
    document.querySelectorAll('[data-theme-control]').forEach((control) => {
      control.addEventListener('click', () => {
        const next = ORDER[(ORDER.indexOf(getPreference()) + 1) % ORDER.length];
        setPreference(next);
      });
    });
  });

  window.MelodyTheme = { getPreference, setPreference };
})();
