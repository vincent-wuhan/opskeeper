SHELL := /bin/bash
PYTHON := $(shell if [ -x .venv/bin/python ]; then echo .venv/bin/python; else echo python3; fi)

.PHONY: build build-installer build-teamharness test verify validate-release audit-open-source clean

build: build-installer build-teamharness

build-installer:
	$(MAKE) -C plugins/agentteams-plugin-installer plugin-zip

build-teamharness:
	bash plugins/opskeeper-teamharness/scripts/build-package.sh

test:
	OPSKEEPER_STANDALONE=1 $(MAKE) -C plugins/agentteams-plugin-installer self-check
	$(PYTHON) -m pytest plugins/opskeeper-teamharness

validate-release: build
	$(PYTHON) scripts/verify_release.py

verify: test validate-release audit-open-source

audit-open-source:
	$(PYTHON) scripts/audit_open_source.py

clean:
	$(MAKE) -C plugins/agentteams-plugin-installer clean
	rm -rf plugins/opskeeper-teamharness/dist
