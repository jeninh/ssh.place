BINARY  := sshplace
IMAGE   := sshplace:latest
DATADIR := ./data

# Local dev ports. In Docker the container listens on 2222/8080 and
# docker-compose maps host 22 onto the SSH port.
SSH_ADDR  := :2222
HTTP_ADDR := :8080

.PHONY: help run test test-race build docker docker-run clean fmt vet tidy check

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

run: ## Run the server locally (ssh -p 2222 localhost)
	go run . -ssh-addr $(SSH_ADDR) -http-addr $(HTTP_ADDR) -data $(DATADIR)

test: ## Run all tests with the race detector
	go test -race ./...

test-race: test ## Alias for test, which already enables -race

build: ## Build the binary
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o $(BINARY) .

docker: ## Build the container image
	docker build -t $(IMAGE) .

docker-run: ## Run the image with a local data volume
	docker run --rm -it \
		-p 2222:2222 -p 8080:8080 \
		-v $(CURDIR)/data:/data \
		$(IMAGE)

fmt: ## Format the source
	gofmt -w .

vet: ## Run go vet
	go vet ./...

tidy: ## Tidy go.mod and go.sum
	go mod tidy

check: fmt vet test ## Format, vet and test

clean: ## Remove build output
	rm -f $(BINARY)
