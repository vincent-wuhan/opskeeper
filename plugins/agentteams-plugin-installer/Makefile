# agentteams-plugin-installer build helpers
PLUGIN_NAME := agentteams-plugin-installer
DIST_DIR := dist

.PHONY: build plugin-zip self-check verify clean

build:
	cd dashboard && npm install --silent && npm run build

plugin-zip: build
	@mkdir -p $(DIST_DIR)
	@cd dashboard && [ -f dist/main.js ] || (echo "ERROR: dashboard/dist/main.js missing — run make build first" && exit 1)
	zip -j $(DIST_DIR)/$(PLUGIN_NAME).zip \
	    dashboard/public/plugin.json \
	    dashboard/dist/main.js \
	    ../../LICENSE \
	    ../../NOTICE.md
	@echo "Built: $(DIST_DIR)/$(PLUGIN_NAME).zip"
	@ls -la $(DIST_DIR)/

self-check:
	python3 scripts/self_check.py

verify: self-check
	cd ../.. && \
	  go test ./internal/manager/server/agentteams/ -count=1 -run TestPlugin -v

clean:
	rm -rf dashboard/dist dashboard/node_modules $(DIST_DIR)
