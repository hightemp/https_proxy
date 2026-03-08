PROJECT_NAME = https_proxy
INSTALL_DIR = /usr/bin
CONFIG_DIR = /etc/$(PROJECT_NAME)
SYSTEMD_DIR = /etc/systemd/system

.PHONY: build run clean install uninstall install-service uninstall-service start stop restart status enable disable

build:
	CGO_ENABLED=0 go build -o $(PROJECT_NAME) main.go
	chmod +x $(PROJECT_NAME)

build-static:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -ldflags '-extldflags "-static"' -o $(PROJECT_NAME)_static main.go
	chmod +x $(PROJECT_NAME)

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
		install -m 644 config.example.yaml $(CONFIG_DIR)/config.yaml; \
		echo "Config installed: $(CONFIG_DIR)/config.yaml"; \
	else \
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