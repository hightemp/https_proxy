PROJECT_NAME = https_proxy

.PHONY: build run clean

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