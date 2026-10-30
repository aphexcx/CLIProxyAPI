BUN ?= pnpm --package=bun@1.4.3 dlx bun
PANEL_VERSION ?= panel-64b4f5a-hub-cache

.PHONY: panel-build panel-test
panel-build:
	cd panel && $(BUN) install --frozen-lockfile
	cd panel && VERSION=$(PANEL_VERSION) $(BUN) run build
	cp panel/dist/index.html panel/dist/management.html

panel-test:
	cd panel && $(BUN) test tests
