FROM golang:1.27.1-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o lighthouse ./cmd/lighthouse

# Lighthouse builds this stage before deploying itself: a failure stops the
# deploy. The database tests skip here (no Postgres); scripts/test-db.sh runs them.
FROM builder AS test
RUN apk add --no-cache gcc musl-dev
RUN unformatted="$(gofmt -l .)" && [ -z "$unformatted" ] || { echo "These files need gofmt:"; echo "$unformatted"; exit 1; }
RUN go vet ./...
RUN CGO_ENABLED=1 go test -race -short -count=1 ./...

# Pinned on purpose; bump it deliberately (e.g. for security fixes).
FROM alpine:3.24
WORKDIR /app

# Runs as root: the Docker socket is root-equivalent anyway.
RUN apk add --no-cache docker-cli docker-cli-compose docker-cli-buildx

COPY --from=builder /app/lighthouse /lighthouse

CMD ["/lighthouse", "serve"]
