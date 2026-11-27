import { describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';

const readProjectFile = (path: string) =>
  readFileSync(new URL(`../${path}`, import.meta.url), 'utf8');
const releaseWorkflow = readProjectFile('../.github/workflows/release.yaml');

describe('tag release attaches the management panel', () => {
  test('a release job builds the panel from the tag being released', () => {
    expect(releaseWorkflow).toMatch(/^ {2}build-panel:\n {4}needs: prepare-release\n/m);
    expect(releaseWorkflow).toContain(
      'make panel-build BUN=bun PANEL_VERSION=\'"$$GITHUB_REF_NAME"\''
    );
    expect(releaseWorkflow).toContain('test -s panel/dist/management.html');
  });

  test("the built page is uploaded to the tag's release", () => {
    expect(releaseWorkflow).toContain(
      'gh release upload "$GITHUB_REF_NAME" panel/dist/management.html --clobber'
    );
  });

  test('the panel build target writes the page the release uploads', () => {
    expect(readProjectFile('../Makefile')).toContain(
      'cp panel/dist/index.html panel/dist/management.html'
    );
  });
});
