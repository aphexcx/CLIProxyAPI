import { describe, expect, test } from 'bun:test';

const flattenLeaves = (value: unknown, prefix = ''): Record<string, string> => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return prefix ? { [prefix]: String(value ?? '') } : {};
  }

  return Object.entries(value).reduce<Record<string, string>>((leaves, [key, child]) => {
    const path = prefix ? `${prefix}.${key}` : key;
    return { ...leaves, ...flattenLeaves(child, path) };
  }, {});
};

const interpolationTokens = (value: string): string[] => value.match(/\{\{[^}]+\}\}/g) ?? [];

describe('Vietnamese locale', () => {
  test('covers every current English key with matching interpolation tokens', async () => {
    const english = flattenLeaves(await Bun.file('src/i18n/locales/en.json').json());
    const vietnamese = flattenLeaves(await Bun.file('src/i18n/locales/vi.json').json());
    const missing = Object.keys(english).filter((key) => !(key in vietnamese));
    const mismatches = Object.keys(english)
      .filter((key) => key in vietnamese)
      .filter(
        (key) =>
          interpolationTokens(english[key]).sort().join('|') !==
          interpolationTokens(vietnamese[key]).sort().join('|')
      );

    expect(missing).toEqual([]);
    expect(mismatches).toEqual([]);
  });
});

test('banked reset observations have complete translations in all six locales', async () => {
  const english = flattenLeaves(
    (await Bun.file('src/i18n/locales/en.json').json()).claude_limit_resets
  );
  for (const locale of ['en', 'vi', 'zh-CN', 'zh-TW', 'ko', 'ru']) {
    const data = await Bun.file(`src/i18n/locales/${locale}.json`).json();
    expect(data.claude_reset).toBeUndefined();
    const translated = flattenLeaves(data.claude_limit_resets);
    expect(Object.keys(translated).sort()).toEqual(Object.keys(english).sort());
    for (const [key, value] of Object.entries(english)) {
      expect(translated[key].trim().length).toBeGreaterThan(0);
      expect(interpolationTokens(translated[key]).sort()).toEqual(
        interpolationTokens(value).sort()
      );
    }
  }
});
