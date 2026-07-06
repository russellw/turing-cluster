# CMD selects which binary to build: "server" (default) or "coordinator".
#   docker build -t worker:latest .
#   docker build --build-arg CMD=coordinator -t coordinator:latest .
FROM golang:1.24-alpine AS builder
ARG CMD=server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /app ./cmd/${CMD}

FROM scratch
COPY --from=builder /app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
