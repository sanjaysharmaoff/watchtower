BINARY := bin/watchtowerd
VERSION ?= dev
LDFLAGS := -s -w -X github.com/sanjaysharmaoff/watchtower/internal/bootstrap.Version=$(VERSION)

.PHONY: build run vet fmt tidy clean up up-k8s logs down demo digest

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/watchtowerd

run: build
	$(BINARY)

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	go clean
	rm -rf bin

# Send a sample Alertmanager payload at a running instance.
demo:
	curl -s -X POST http://localhost:8080/alerts \
		-H "Content-Type: application/json" \
		--data-binary @examples/alerts/alertmanager-oom.json

# Run the daily pulse once and exit.
digest: build
	$(BINARY) --job digest

up:
	docker compose up --build

# Adds real Kubernetes event enrichment. Run deploy/k8s/wire-minikube.ps1 first.
up-k8s:
	docker compose -f docker-compose.yml -f docker-compose.minikube.yml up -d --build

logs:
	docker compose logs -f watchtower

down:
	docker compose down -v
