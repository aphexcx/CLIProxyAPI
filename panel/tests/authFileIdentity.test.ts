import { describe, expect, test } from 'bun:test';
import { deriveAuthFileIdentity, stripJsonExtension } from '../src/features/authFiles/identity';
import type { AuthFileItem } from '../src/types';

const authFile = (overrides: Partial<AuthFileItem> = {}): AuthFileItem => ({
  name: 'credential.json',
  type: 'codex',
  ...overrides,
});

describe('stripJsonExtension', () => {
  test('strips the extension from a real codex file name', () => {
    expect(stripJsonExtension('codex-abc12345-user@example.com-team.json')).toBe(
      'codex-abc12345-user@example.com-team'
    );
  });

  test('matches the extension case-insensitively', () => {
    expect(stripJsonExtension('Antigravity-User@Example.com.JSON')).toBe(
      'Antigravity-User@Example.com'
    );
  });

  test('leaves an extension-less runtime-only id untouched', () => {
    expect(stripJsonExtension('aistudio-channel-a')).toBe('aistudio-channel-a');
  });

  test('never strips down to an empty string', () => {
    expect(stripJsonExtension('.json')).toBe('.json');
  });

  test('trims surrounding whitespace', () => {
    expect(stripJsonExtension('  kimi-1712345678901.json  ')).toBe('kimi-1712345678901');
  });
});

describe('deriveAuthFileIdentity', () => {
  test('leads with the account email without displaying the file name below it', () => {
    expect(
      deriveAuthFileIdentity(
        authFile({
          name: 'codex-abc12345-user@example.com-team.json',
          email: 'user@example.com',
        })
      )
    ).toEqual({
      primary: 'user@example.com',
      kind: 'email',
      secondary: null,
      fullName: 'codex-abc12345-user@example.com-team.json',
    });
  });

  test('treats a whitespace-only email as absent', () => {
    const identity = deriveAuthFileIdentity(
      authFile({ name: 'kimi-1712345678901.json', email: '   ' })
    );
    expect(identity.kind).toBe('fileName');
    expect(identity.primary).toBe('kimi-1712345678901');
  });

  test('treats a non-string email as absent (index signature guard)', () => {
    const identity = deriveAuthFileIdentity(
      authFile({ name: 'kimi-1712345678901.json', email: 123 as unknown as string })
    );
    expect(identity.kind).toBe('fileName');
    expect(identity.primary).toBe('kimi-1712345678901');
  });

  test('falls back to the file name and drops the duplicate secondary row', () => {
    expect(
      deriveAuthFileIdentity(authFile({ name: 'kimi-1712345678901.json', type: 'kimi' }))
    ).toEqual({
      primary: 'kimi-1712345678901',
      kind: 'fileName',
      secondary: null,
      fullName: 'kimi-1712345678901.json',
    });
  });

  test('uses the project id when there is no email', () => {
    expect(
      deriveAuthFileIdentity(
        authFile({ name: 'vertex-my-proj.json', type: 'vertex', projectId: 'my-proj' })
      )
    ).toEqual({
      primary: 'my-proj',
      kind: 'projectId',
      secondary: null,
      fullName: 'vertex-my-proj.json',
    });
  });

  test('prefers the email over the project id', () => {
    const identity = deriveAuthFileIdentity(
      authFile({
        name: 'vertex-my-proj.json',
        email: 'sa@project.iam.gserviceaccount.com',
        projectId: 'my-proj',
      })
    );
    expect(identity.kind).toBe('email');
    expect(identity.primary).toBe('sa@project.iam.gserviceaccount.com');
  });

  test('never surfaces the account field — it can be a raw API key', () => {
    const identity = deriveAuthFileIdentity(
      authFile({
        name: 'gemini-apikey.json',
        type: 'gemini',
        account: 'sk-live-abcd1234',
        account_type: 'api_key',
      })
    );
    expect(identity.kind).toBe('fileName');
    expect(identity.primary).toBe('gemini-apikey');
    expect(JSON.stringify(identity)).not.toContain('sk-live');
  });

  test('ignores an oauth account even when it looks like an email (do not add it back to the chain)', () => {
    const identity = deriveAuthFileIdentity(
      authFile({
        name: 'codex-abc12345-user@example.com-team.json',
        account: 'user@example.com',
        account_type: 'oauth',
      })
    );
    expect(identity.kind).toBe('fileName');
    expect(identity.primary).toBe('codex-abc12345-user@example.com-team');
    expect(identity.secondary).toBeNull();
  });

  test('suppresses the secondary row for runtime-only entries whose name is the account', () => {
    expect(
      deriveAuthFileIdentity(
        authFile({
          name: 'aistudio-channel-a',
          type: 'aistudio',
          email: 'aistudio-channel-a',
          runtimeOnly: true,
        })
      ).secondary
    ).toBeNull();
  });

  test('the duplicate guard is case-insensitive', () => {
    expect(
      deriveAuthFileIdentity(authFile({ name: 'USER@X.COM.json', email: 'user@x.com' })).secondary
    ).toBeNull();
  });

  test('distinguishes plans for two credentials sharing one email', () => {
    const team = deriveAuthFileIdentity(
      authFile({
        name: 'codex-abc12345-user@example.com-team.json',
        email: 'user@example.com',
        plan_type: 'team',
      })
    );
    const plus = deriveAuthFileIdentity(
      authFile({
        name: 'codex-abc12345-user@example.com-plus.json',
        email: 'user@example.com',
        plan_type: 'plus',
      })
    );
    expect(team.primary).toBe(plus.primary);
    expect(team.secondary).toBe('Team');
    expect(plus.secondary).toBe('Plus');
  });

  test('never derives the secondary row from a file name', () => {
    expect(
      deriveAuthFileIdentity(
        authFile({ name: '-abc12345-user@example.com-team.json', email: 'user@example.com' })
      ).secondary
    ).toBeNull();
  });

  test('shows the Claude organization below the email and preserves the filename tooltip', () => {
    expect(
      deriveAuthFileIdentity(
        authFile({
          name: 'claude-seat.json',
          type: 'claude',
          email: ' user@example.com ',
          organization_name: ' Example Organization ',
          plan_type: 'pro',
        })
      )
    ).toEqual({
      primary: 'user@example.com',
      kind: 'email',
      secondary: 'Example Organization',
      fullName: 'claude-seat.json',
    });
  });

  test.each(['pro', 'plus', 'team'])('shows the Codex %s plan in words', (plan) => {
    const identity = deriveAuthFileIdentity(
      authFile({
        email: 'user@example.com',
        plan_type: ` ${plan.toUpperCase()} `,
        organization_name: 'Not a Claude seat',
      })
    );
    expect(identity.primary).toBe('user@example.com');
    expect(identity.secondary).toBe(plan[0].toUpperCase() + plan.slice(1));
  });

  test('uses the provider alias when type is absent', () => {
    expect(
      deriveAuthFileIdentity(
        authFile({
          type: undefined,
          provider: ' CLAUDE ',
          email: 'user@example.com',
          organization_name: 'Org',
        })
      ).secondary
    ).toBe('Org');
  });

  test('does not show missing, malformed or unrelated provider metadata', () => {
    for (const value of [undefined, '', '  ', 123, { name: 'Org' }]) {
      expect(
        deriveAuthFileIdentity(
          authFile({ type: 'claude', email: 'u@x.com', organization_name: value as string })
        ).secondary
      ).toBeNull();
      expect(
        deriveAuthFileIdentity(authFile({ email: 'u@x.com', plan_type: value as string })).secondary
      ).toBeNull();
    }
    expect(
      deriveAuthFileIdentity(
        authFile({ type: 'gemini', email: 'u@x.com', organization_name: 'Org', plan_type: 'pro' })
      ).secondary
    ).toBeNull();
  });

  test('handles an empty file name without inventing a placeholder', () => {
    expect(deriveAuthFileIdentity(authFile({ name: '', email: 'user@x.com' }))).toEqual({
      primary: 'user@x.com',
      kind: 'email',
      secondary: null,
      fullName: '',
    });
    expect(deriveAuthFileIdentity(authFile({ name: '' }))).toEqual({
      primary: '',
      kind: 'fileName',
      secondary: null,
      fullName: '',
    });
  });
});
