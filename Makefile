PROJECT_NAME = https_proxy
VERSION = $(shell cat VERSION | tr -d '[:space:]')
INSTALL_DIR = /usr/bin
CONFIG_DIR = /etc/$(PROJECT_NAME)
SYSTEMD_DIR = /etc/systemd/system
DOCKER_IMAGE = hightemp/$(PROJECT_NAME)

.PHONY: build build-static run clean install uninstall uninstall-full install-service uninstall-service start stop restart status enable disable release docker-build docker-push docker-release

build:
	CGO_ENABLED=0 go build -o $(PROJECT_NAME) ./cmd/https_proxy
	chmod +x $(PROJECT_NAME)

build-static:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -ldflags '-extldflags "-static"' -o $(PROJECT_NAME)_static ./cmd/https_proxy
	chmod +x $(PROJECT_NAME)_static

run:
	./$(PROJECT_NAME)

clean:
	rm -f $(PROJECT_NAME)
	rm -f $(PROJECT_NAME)_static

install: build
	@echo "Installing $(PROJECT_NAME)..."
	install -d $(CONFIG_DIR)
	install -m 755 $(PROJECT_NAME) $(INSTALL_DIR)/$(PROJECT_NAME)
	@if [ ! -f $(CONFIG_DIR)/config.yaml ]; then \
		install -m 600 config.example.yaml $(CONFIG_DIR)/config.yaml; \
		echo "Config installed: $(CONFIG_DIR)/config.yaml"; \
	else \
		chmod 600 $(CONFIG_DIR)/config.yaml; \
		echo "Config already exists, skipping: $(CONFIG_DIR)/config.yaml"; \
	fi
	install -m 644 packaging/$(PROJECT_NAME).service $(SYSTEMD_DIR)/$(PROJECT_NAME).service
	systemctl daemon-reload
	systemctl enable $(PROJECT_NAME)
	systemctl restart $(PROJECT_NAME)
	@echo "Installation complete. Service $(PROJECT_NAME) is running and enabled on boot."

uninstall:
	@echo "Removing $(PROJECT_NAME)..."
	-systemctl stop $(PROJECT_NAME) 2>/dev/null || true
	-systemctl disable $(PROJECT_NAME) 2>/dev/null || true
	rm -f $(SYSTEMD_DIR)/$(PROJECT_NAME).service
	rm -f $(INSTALL_DIR)/$(PROJECT_NAME)
	systemctl daemon-reload
	@echo "Removal complete."
	@echo "Configuration preserved in $(CONFIG_DIR)"

uninstall-full: uninstall
	@echo "Removing configuration..."
	rm -rf $(CONFIG_DIR)
	@echo "Full removal complete."

install-service:
	install -m 644 packaging/$(PROJECT_NAME).service $(SYSTEMD_DIR)/$(PROJECT_NAME).service
	systemctl daemon-reload
	systemctl enable $(PROJECT_NAME)
	systemctl start $(PROJECT_NAME)
	@echo "Service $(PROJECT_NAME) installed and started."

uninstall-service:
	-systemctl stop $(PROJECT_NAME) 2>/dev/null || true
	-systemctl disable $(PROJECT_NAME) 2>/dev/null || true
	rm -f $(SYSTEMD_DIR)/$(PROJECT_NAME).service
	systemctl daemon-reload
	@echo "Service $(PROJECT_NAME) removed."

start:
	systemctl start $(PROJECT_NAME)
	@echo "Service $(PROJECT_NAME) started."

stop:
	systemctl stop $(PROJECT_NAME)
	@echo "Service $(PROJECT_NAME) stopped."

restart:
	systemctl restart $(PROJECT_NAME)
	@echo "Service $(PROJECT_NAME) restarted."

status:
	systemctl status $(PROJECT_NAME)

enable:
	systemctl enable $(PROJECT_NAME)
	@echo "Service $(PROJECT_NAME) enabled on boot."

disable:
	systemctl disable $(PROJECT_NAME)
	@echo "Service $(PROJECT_NAME) disabled from boot."

release:
	@test -n "$(VERSION)" || (echo "VERSION file is empty"; exit 1)
	@echo "Releasing v$(VERSION)..."
	git add -A
	git commit -m "release v$(VERSION)" || true
	git tag -f "v$(VERSION)"
	git push
	git push -f --tags
	@echo "Released v$(VERSION)"

docker-build:
	docker build -t $(DOCKER_IMAGE):$(VERSION) -t $(DOCKER_IMAGE):latest .

docker-push: docker-build
	docker push $(DOCKER_IMAGE):$(VERSION)
	docker push $(DOCKER_IMAGE):latest

docker-release: docker-push
	@echo "Pushed $(DOCKER_IMAGE):$(VERSION) and :latest"
