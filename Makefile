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
	@echo "Установка $(PROJECT_NAME)..."
	install -d $(CONFIG_DIR)
	install -m 755 $(PROJECT_NAME) $(INSTALL_DIR)/$(PROJECT_NAME)
	@if [ ! -f $(CONFIG_DIR)/config.yaml ]; then \
		install -m 644 config.example.yaml $(CONFIG_DIR)/config.yaml; \
		echo "Конфиг установлен: $(CONFIG_DIR)/config.yaml"; \
	else \
		echo "Конфиг уже существует, пропускаем: $(CONFIG_DIR)/config.yaml"; \
	fi
	install -m 644 packaging/$(PROJECT_NAME).service $(SYSTEMD_DIR)/$(PROJECT_NAME).service
	systemctl daemon-reload
	systemctl enable $(PROJECT_NAME)
	systemctl start $(PROJECT_NAME)
	@echo "Установка завершена. Сервис $(PROJECT_NAME) запущен и добавлен в автозагрузку."

uninstall:
	@echo "Удаление $(PROJECT_NAME)..."
	-systemctl stop $(PROJECT_NAME) 2>/dev/null || true
	-systemctl disable $(PROJECT_NAME) 2>/dev/null || true
	rm -f $(SYSTEMD_DIR)/$(PROJECT_NAME).service
	rm -f $(INSTALL_DIR)/$(PROJECT_NAME)
	systemctl daemon-reload
	@echo "Удаление завершено."
	@echo "Конфигурация сохранена в $(CONFIG_DIR)"

uninstall-full: uninstall
	@echo "Удаление конфигурации..."
	rm -rf $(CONFIG_DIR)
	@echo "Полное удаление завершено."

install-service:
	install -m 644 packaging/$(PROJECT_NAME).service $(SYSTEMD_DIR)/$(PROJECT_NAME).service
	systemctl daemon-reload
	systemctl enable $(PROJECT_NAME)
	systemctl start $(PROJECT_NAME)
	@echo "Сервис $(PROJECT_NAME) установлен и запущен."

uninstall-service:
	-systemctl stop $(PROJECT_NAME) 2>/dev/null || true
	-systemctl disable $(PROJECT_NAME) 2>/dev/null || true
	rm -f $(SYSTEMD_DIR)/$(PROJECT_NAME).service
	systemctl daemon-reload
	@echo "Сервис $(PROJECT_NAME) удалён."

start:
	systemctl start $(PROJECT_NAME)
	@echo "Сервис $(PROJECT_NAME) запущен."

stop:
	systemctl stop $(PROJECT_NAME)
	@echo "Сервис $(PROJECT_NAME) остановлен."

restart:
	systemctl restart $(PROJECT_NAME)
	@echo "Сервис $(PROJECT_NAME) перезапущен."

status:
	systemctl status $(PROJECT_NAME)

enable:
	systemctl enable $(PROJECT_NAME)
	@echo "Сервис $(PROJECT_NAME) добавлен в автозагрузку."

disable:
	systemctl disable $(PROJECT_NAME)
	@echo "Сервис $(PROJECT_NAME) удалён из автозагрузки."