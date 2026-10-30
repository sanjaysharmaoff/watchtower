# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src

# Dependencies first so the module layer caches independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X github.com/sanjaysharmaoff/watchtower/internal/bootstrap.Version=${VERSION}" \
    -o /out/watchtowerd ./cmd/watchtowerd

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/watchtowerd /app/watchtowerd
COPY --from=build /src/migrations /app/migrations

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/watchtowerd"]
